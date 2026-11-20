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
#   5. router.toml edited in the browser: the check shows the change, Apply makes it
#      pending, a confirm over a connection opened before the apply is refused, and
#      one over a new connection keeps it.
#   6. A port forward added on the Port forwarding page (compiler/edit, ADR 0016):
#      reviewed, applied, kept; nftables has it, and router.toml keeps step 5's comment.
#      A second one, undone with Undo, leaves router.toml as it was (matching generation 3).
#   7. A change applied in the browser that cuts the browser off (lockout.toml)
#      reverts by itself, and the web UI comes back.
#   8. A new password restarts the web UI and ends the old session.
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
# Header names are matched without case, as HTTP defines them.
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

# field NAME: the value of the hidden form field NAME on the page in $out.
field() { sed -n "s/.*name=\"$1\" value=\"\([^\"]*\)\".*/\1/p" <<<"$out" | head -n 1; }
# check_config FILE: check FILE in the settings editor; sets csrf, base and checked.
check_config() {
  out=$(web /settings)
  csrf=$(field csrf) base=$(field base)
  [[ -n $csrf && -n $base ]] || die "the settings page has no form: $out"
  out=$(web /settings --data-urlencode "csrf=$csrf" --data-urlencode "base=$base" \
    --data-urlencode "config@$1" -d action=check)
  checked=$(field checked)
  [[ -n $checked && $out == *'value="apply"'* ]] || die "checking $1 offers no Apply: $out"
}
apply_config() {
  web /settings --data-urlencode "csrf=$csrf" --data-urlencode "base=$(field base)" \
    --data-urlencode "checked=$checked" --data-urlencode "config@$1" -d action=apply
}

log "5. edit router.toml in the browser: check, apply, confirm from a new connection"
vm 'cat /etc/rollgate/router.toml' > "$work/edited.toml"
echo "# changed in the browser" >> "$work/edited.toml"
check_config "$work/edited.toml"
[[ $out == *"+# changed in the browser"* || $out == *"&#43;# changed in the browser"* ]] || die "the check doesn't show the change: $out"
[[ $out == HTTP/1.1* ]] || die "want HTTP/1.1 (each request on its own connection): $out"
# A connection opened before the apply sends its headers now and its body after
# the apply: its confirm must be refused, because it doesn't prove that the
# router still accepts new connections.
session=$(awk '$6 == "rollgate_session" { print $7 }' "$work/cookies")
body="csrf=$(printf %s "$csrf" | sed 's/=/%3D/g')"
mkfifo "$work/old.in"
openssl s_client -quiet -connect "127.0.0.1:$https_port" <"$work/old.in" >"$work/old.out" 2>/dev/null &
exec 3>"$work/old.in"
printf 'POST /confirm HTTP/1.1\r\nHost: 127.0.0.1\r\nCookie: rollgate_session=%s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n' \
  "$session" "${#body}" >&3
sleep 2
out=$(apply_config "$work/edited.toml")
[[ $out == *" 303"* && $out == *"Location: /confirm"* && $out == *"Connection: close"* ]] || die "apply: $out"
grep -q "Waiting for confirmation: generation 2" <<<"$(vm 'rollgate status')" || die "no generation 2 waiting after the apply: $(vm 'rollgate status')"
printf '%s' "$body" >&3
for _ in $(seq 20); do grep -q "^HTTP/1.1" "$work/old.out" && break; sleep 0.5; done
exec 3>&-
grep -q "^HTTP/1.1 409" "$work/old.out" || die "a confirm over a connection opened before the apply wasn't refused: $(head -c 2000 "$work/old.out")"
grep -q "Waiting for confirmation: generation 2" <<<"$(vm 'rollgate status')" || die "the refused confirm confirmed something"
log "  apply went through; a confirm over a connection opened before it was refused"
out=$(web /confirm)
[[ $out == *"Keep these settings"* ]] || die "the confirm page: $out"
out=$(web /confirm --data-urlencode "csrf=$csrf")
[[ $out == *" 200"* && $out == *"The new settings are kept"* ]] || die "confirm: $out"
grep -q "Running: generation 2, confirmed" <<<"$(vm 'rollgate status')" || die "generation 2 isn't confirmed: $(vm 'rollgate status')"
vm 'grep -q "^# changed in the browser" /etc/rollgate/router.toml' || die "router.toml doesn't have the browser's edit"
log "  confirmed from a new connection; router.toml has the edit"

log "6. add a port forward on the Port forwarding page"
out=$(web /forwards)
[[ $out == *"No forwards"* ]] || die "the port forwarding page: $out"
out=$(web /forwards --data-urlencode "csrf=$csrf" -d action=review -d index=-1 --data-urlencode "name=web server" \
  -d protocol=tcp -d wan_port=8080 -d address=10.0.2.50 -d device_port=80)
[[ $out == *"Review the change"* ]] || die "the forward's review: $out"
# html/template writes + in attributes as &#43;.
c64=$(field config64 | sed 's/&#43;/+/g')
[[ -n $c64 ]] || die "the review offers no Apply: $out"
out=$(web /settings --data-urlencode "csrf=$csrf" --data-urlencode "base=$(field base)" \
  --data-urlencode "checked=$(field checked)" --data-urlencode "config64=$c64" -d action=apply)
[[ $out == *" 303"* && $out == *"Location: /confirm"* ]] || die "applying the forward: $out"
out=$(web /confirm --data-urlencode "csrf=$csrf")
[[ $out == *"The new settings are kept"* ]] || die "keeping the forward: $out"
grep -q "Running: generation 3, confirmed" <<<"$(vm 'rollgate status')" || die "generation 3 isn't confirmed: $(vm 'rollgate status')"
grep -q "10.0.2.50:80" <<<"$(vm 'nft list ruleset')" || die "the firewall has no forward to 10.0.2.50:80"
vm 'grep -q "^# changed in the browser" /etc/rollgate/router.toml' || die "the form's edit lost the comment from step 5"
out=$(web /forwards)
[[ $out == *"web server"* && $out == *"10.0.2.50:80"* ]] || die "the page doesn't list the new forward: $out"
log "  added on the page, kept, and in the firewall; router.toml kept its comment"
# Another one, undone: Undo puts router.toml back as it was.
out=$(web /forwards --data-urlencode "csrf=$csrf" -d action=review -d index=-1 --data-urlencode "name=undo me" \
  -d protocol=udp -d wan_port=9999 -d address=10.0.2.51)
c64=$(field config64 | sed 's/&#43;/+/g')
out=$(web /settings --data-urlencode "csrf=$csrf" --data-urlencode "base=$(field base)" \
  --data-urlencode "checked=$(field checked)" --data-urlencode "config64=$c64" -d action=apply)
[[ $out == *" 303"* ]] || die "applying the second forward: $out"
out=$(web /revert --data-urlencode "csrf=$csrf")
[[ $out == *"The previous settings are back"* && $out == *"router.toml is back as it was"* ]] || die "Undo: $out"
vm 'grep -q "undo me" /etc/rollgate/router.toml' && die "Undo left the undone forward in router.toml"
vm 'grep -q "web server" /etc/rollgate/router.toml' || die "Undo lost the kept forward from router.toml"
status=$(vm 'rollgate status')
if ! grep -q "Running: generation 3, confirmed" <<<"$status" || ! grep -Eq "^ +4  reverted" <<<"$status" ||
  ! grep -q "router.toml: matches generation 3" <<<"$status"; then
  die "after Undo: $status"
fi
log "  Undo of a second forward: generation 4 reverted, and router.toml matches generation 3 again"

log "7. a change that cuts off the web UI reverts by itself"
check_config "$here/apply/lockout.toml"
out=$(apply_config "$here/apply/lockout.toml")
[[ $out == *" 303"* ]] || die "applying the lockout: $out"
start=$SECONDS
if curl -sk -m 5 -o /dev/null "https://127.0.0.1:$https_port/login"; then
  die "a new connection to the web UI still worked after the lockout was applied"
fi
log "  new connections to the web UI are refused"
until curl -sk -m 5 -o /dev/null "https://127.0.0.1:$https_port/login"; do
  ((SECONDS - start < 120)) || die "the web UI didn't come back within 120 s of a 30 s confirm timeout"
  sleep 2
done
log "  the web UI came back by itself after $((SECONDS - start)) s"
until vm true 2>/dev/null; do ((SECONDS - start < 180)) || die "no SSH after the revert"; sleep 2; done
status=$(vm 'rollgate status')
if ! grep -q "Running: generation 3, confirmed" <<<"$status" || ! grep -Eq "^ +5  reverted" <<<"$status"; then
  die "after the revert: $status"
fi
out=$(web /login --data-urlencode "password=$password")
out=$(web /)
[[ $out == *" 200"* && $out == *"all checks pass"* ]] || die "the status page after the revert: $out"
log "  generation 5 reverted, generation 3 runs; logged in again"

log "8. a new password ends the old session"
vm "echo 'another long password' | rollgate web-password --stdin" >/dev/null || die "changing the password failed"
out=$(web /)
[[ $out == *" 303"* && $out == *"Location: /login"* ]] || die "the old session still works after a new password: $out"
log "  the old session ended"
echo "web-test: PASS"
