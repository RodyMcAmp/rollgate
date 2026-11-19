#!/usr/bin/env bash
# L3 web UI test (ADR 0014): what a browser on the LAN, and one on the internet, see.
#
#   1. Apply tests/vm/apply/good.toml (lan 10.0.2.15 is the management zone).
#      Before a web password is set, nothing answers on 443.
#   2. rollgate web-password turns the web UI on. The certificate a client gets over
#      TLS has the fingerprint web-password printed.
#   3. Not logged in, / goes to the login page; a wrong password gets no session; the
#      right one gets a Secure, HttpOnly, SameSite=Strict cookie, and the status page
#      shows the health checks and the release. Port 80 redirects to HTTPS.
#   4. From WAN, 443 is unreachable (firewall), and the server runs unprivileged.
#   5. A new password restarts the web UI and ends the old session.
#
# Usage:  sudo tests/vm/web-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; PORT (default 2250: SSH,
#         then HTTPS, HTTP and HTTPS from WAN on the next three)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: web-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
port=${PORT:-2250}
ssh_port=$port https_port=$((port + 1)) http_port=$((port + 2)) wan_port=$((port + 3))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/web.serial.log"
password="correct horse battery staple"

die() { echo "web-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for tool in qemu-system-x86_64 curl openssl ssh-keygen; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid=
cleanup() {
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/root"

log "preparing a copy of the image with a test SSH key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
install -m 0644 "$here/apply/good.toml" "$work/root/etc/rollgate/router.toml"
umount "$work/root"

: > "$serial"
# lan: QEMU's first user network (the router is 10.0.2.15 there). wan: a second one,
# where the host reaches the router's WAN address 10.0.3.15, as the internet would.
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:01,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22,hostfwd=tcp:127.0.0.1:"$https_port"-:443,hostfwd=tcp:127.0.0.1:"$http_port"-:80 \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:02,net=10.0.3.0/24,hostfwd=tcp:127.0.0.1:"$wan_port"-10.0.3.15:443 \
  -display none -monitor none -serial file:"$serial" &
qemu_pid=$!

vm() {
  timeout 120 ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
deadline=$((SECONDS + timeout_s))
until vm true 2>/dev/null; do
  ((SECONDS < deadline)) || die "no SSH within ${timeout_s}s (serial log: $serial)"
  kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (serial log: $serial)"
  sleep 3
done
# Header names arrive in lowercase over HTTP/2 and capitalized over HTTP/1.1.
shopt -s nocasematch
# web PATH [curl options]: one HTTPS request from the LAN; prints the headers and body.
web() { local path=$1; shift; curl -sk -m 15 -D - -b "$work/cookies" -c "$work/cookies" "$@" "https://127.0.0.1:$https_port$path"; }

log "1. apply the config; no web UI before a password is set"
vm 'rollgate apply' >/dev/null 2>&1 || die "rollgate apply failed"
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
if curl -sk -m 5 -o /dev/null "https://127.0.0.1:$https_port/"; then
  die "something answers on 443 before a web password is set"
fi
log "  applied; 443 is closed"

log "2. rollgate web-password turns it on"
vm "echo '$password' | rollgate web-password --stdin" > "$work/password.out" || die "rollgate web-password failed"
grep -q "The web UI is on: open https://10.0.2.15/" "$work/password.out" || die "web-password didn't say where the web UI is: $(cat "$work/password.out")"
want_fp=$(vm 'openssl x509 -in /etc/rollgate/secrets/web.crt -noout -fingerprint -sha256 | cut -d= -f2')
got_fp=$(openssl s_client -connect "127.0.0.1:$https_port" </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2)
[[ -n $got_fp && $got_fp == "$want_fp" ]] || die "TLS presents fingerprint '$got_fp', the certificate's is '$want_fp'"
grep -qF "$want_fp" "$work/password.out" || die "web-password printed another fingerprint than the certificate's"
log "  on; TLS presents the certificate whose fingerprint web-password printed"

log "3. log in from the LAN"
out=$(web /)
[[ $out == *" 303"* && $out == *"Location: /login"* ]] || die "/ without a session: $out"
out=$(web /login -d "password=wrong password")
[[ $out == *" 401"* && $out != *"Set-Cookie"* ]] || die "a wrong password: $out"
out=$(web /login --data-urlencode "password=$password")
[[ $out == *" 303"* ]] || die "the right password: $out"
cookie=$(grep -i '^set-cookie: rollgate_session=' <<<"$out" || true)
for flag in Secure HttpOnly SameSite=Strict; do
  [[ $cookie == *"$flag"* ]] || die "the session cookie lacks $flag: $cookie"
done
out=$(web /)
[[ $out == *" 200"* && $out == *"all checks pass"* && $out == *"Release: "* ]] || die "the status page: $out"
out=$(curl -s -m 10 -D - -o /dev/null "http://127.0.0.1:$http_port/x")
[[ $out == *" 301"* && $out == *"Location: https://127.0.0.1/x"* ]] || die "port 80 doesn't redirect to HTTPS: $out"
log "  login page; wrong password refused; session cookie Secure, HttpOnly, SameSite=Strict; status page; 80 redirects"

log "4. not from WAN; unprivileged"
if curl -sk -m 8 -o /dev/null "https://127.0.0.1:$wan_port/"; then
  die "the web UI answers from WAN"
fi
user=$(vm 'ps -o user= -o args= -C rollgate' | awk '$3 == "web" { print $1 }')
[[ -n $user && $user != root ]] || die "rollgate web runs as '${user:-nothing}', want an unprivileged user"
log "  unreachable from WAN; runs as $user"

log "5. a new password ends the old session"
vm "echo 'another long password' | rollgate web-password --stdin" >/dev/null || die "changing the password failed"
out=$(web /)
[[ $out == *" 303"* && $out == *"Location: /login"* ]] || die "the old session still works after a new password: $out"
log "  the old session ended"
echo "web-test: PASS"
