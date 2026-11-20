#!/usr/bin/env bash
# L3 first-boot test (ADR 0020): a new router sets itself up.
#
#   1. With a working router.toml installed, the first boot applies it and keeps it
#      by itself: generation 1 is confirmed, without anyone typing.
#   2. The serial console asks for the router's password: too short and two that
#      differ are refused; then it's set. It logs in as root at the console, it logs
#      in to the web UI, and SSH doesn't accept passwords. The login prompt shows
#      where the web UI is.
#   3. After a reboot nothing runs again: no first-boot apply, no password question.
#   4. A second new router, whose router.toml validates but fails a health check
#      (lan on a port that doesn't exist): the first boot undoes it and says why on
#      the console, and the router stays reachable with the image's defaults.
#
# Usage:  sudo tests/vm/firstboot-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; PORT (default 2300: SSH,
#         then HTTPS)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: firstboot-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
port=${PORT:-2300}
ssh_port=$port https_port=$((port + 1))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
password="correct horse battery staple"

die() { echo "firstboot-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 ssh-keygen curl mkfifo; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid='' cat_pid=''
stop_vm() {
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  [[ -n $cat_pid ]] && kill "$cat_pid" 2>/dev/null && wait "$cat_pid" 2>/dev/null
  qemu_pid='' cat_pid=''
  exec 3>&-
}
cleanup() {
  stop_vm
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/root"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"

# new_router NAME CONFIG: a copy of the image as it leaves the factory, with the
# test's SSH key and CONFIG as its router.toml. Nothing else.
new_router() {
  cp --sparse=always "$image" "$work/$1.img"
  mount -o loop,offset="$root_offset",subvol=/@ "$work/$1.img" "$work/root"
  install -d -m 0700 "$work/root/root/.ssh"
  install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
  install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
  install -m 0644 "$2" "$work/root/etc/rollgate/router.toml"
  umount "$work/root"
}

# boot NAME: start the router. Its serial console is a pair of FIFOs: what it prints
# goes to $serial, and send() types into it.
boot() {
  serial="$log_dir/firstboot-$1.serial.log"
  : > "$serial"
  mark=0
  rm -f "$work/con.in" "$work/con.out"
  mkfifo "$work/con.in" "$work/con.out"
  cp "$ovmf_vars" "$work/$1.vars.fd"
  qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
    -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
    -drive if=pflash,format=raw,file="$work/$1.vars.fd" \
    -drive file="$work/$1.img",format=raw,if=virtio \
    -nic user,model=virtio-net-pci,mac=52:54:00:00:00:01,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22,hostfwd=tcp:127.0.0.1:"$https_port"-:443 \
    -nic user,model=virtio-net-pci,mac=52:54:00:00:00:02,net=10.0.3.0/24 \
    -chardev pipe,id=con,path="$work/con" -serial chardev:con \
    -display none -monitor none &
  qemu_pid=$!
  cat "$work/con.out" >> "$serial" &
  cat_pid=$!
  exec 3> "$work/con.in"
}
send() { printf '%s\r' "$1" >&3; }
# expect TEXT [SECONDS]: wait until the console shows TEXT after what the last
# expect matched.
expect() {
  local deadline=$((SECONDS + ${2:-120})) at
  until at=$(tail -c +$((mark + 1)) "$serial" | grep -boF -- "$1" | head -n 1 | cut -d: -f1) && [[ -n $at ]]; do
    ((SECONDS < deadline)) || die "the console didn't show '$1' (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (log: $serial)"
    sleep 1
  done
  mark=$((mark + at + ${#1}))
}
vm() {
  timeout 120 ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
# wait_firstboot: wait for SSH and for rollgate-firstboot to have finished.
wait_firstboot() {
  local deadline=$((SECONDS + timeout_s))
  until vm 'systemctl is-system-running --wait >/dev/null 2>&1; systemctl show -p ActiveState --value rollgate-firstboot' 2>/dev/null |
    grep -qxE 'active|failed|inactive'; do
    ((SECONDS < deadline)) || die "no SSH, or the first boot didn't finish, within ${timeout_s}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (log: $serial)"
    sleep 3
  done
}

log "1. a new router applies its router.toml by itself and keeps it"
new_router good "$here/apply/good.toml"
boot good
wait_firstboot
[[ $(vm 'systemctl show -p ActiveState --value rollgate-firstboot') == active ]] ||
  die "rollgate-firstboot didn't succeed: $(vm 'journalctl -b -u rollgate-firstboot --no-pager | tail -30')"
[[ $(vm 'cat /var/lib/rollgate/current; test -e /var/lib/rollgate/pending && echo pending') == 1 ]] ||
  die "generation 1 isn't kept: $(vm 'rollgate status' 2>&1)"
[[ $(vm 'ip -4 -br addr show lan') == *10.0.2.15/24* ]] || die "lan doesn't have its configured address: $(vm 'ip -br addr')"
log "  generation 1 is confirmed, lan is 10.0.2.15/24"

log "2. the console asks for the router's password"
expect "Choose a password for this router"
expect "Password: "
send "too short"
expect "shorter than 10 characters"
expect "Password: "
send "$password"
expect "Again: "
send "something else entirely"
expect "didn't match"
expect "Password: "
send "$password"
expect "Again: "
send "$password"
expect "Password set."
expect "Web UI: https://10.0.2.15/"
expect "login: "
send root
expect "Password: "
send "$password"
expect "]#"
send 'echo "logged in as $(id -un) $((6 * 7))"'
expect "logged in as root 42"
log "  too short and differing passwords refused; it logs in at the console"
vm "awk -F: '\$1 == \"root\" && substr(\$2, 1, 1) == \"\$\" {found = 1} END {exit !found}' /etc/shadow" ||
  die "root's password isn't hashed in /etc/shadow"
vm 'systemctl is-active -q rollgate-web.socket' || die "the web UI isn't on"
web() { curl -sk -m 15 -D - -o /dev/null "$@" "https://127.0.0.1:$https_port/login"; }
[[ $(web -d "password=wrong password") != *" 303"* ]] || die "the web UI accepted a wrong password"
[[ $(web --data-urlencode "password=$password") == *" 303"* ]] || die "the web UI refused the router's password"
vm 'sshd -T | grep -qix "passwordauthentication no" && sshd -T | grep -qix "kbdinteractiveauthentication no"' ||
  die "sshd accepts passwords"
vm 'grep -q "Web UI: https://10.0.2.15/" /run/issue.d/50-rollgate.issue' || die "the login prompt doesn't show the web UI's address"
log "  the same password logs in to the web UI; SSH takes keys only; the login prompt shows the web UI"

log "3. after a reboot, nothing runs again"
vm 'systemctl reboot' || true
sleep 10
wait_firstboot
[[ $(vm 'systemctl show -p ConditionResult --value rollgate-firstboot') == no ]] || die "rollgate-firstboot ran again"
[[ $(vm 'ls /var/lib/rollgate/generations | sort -n | tail -n 1') == 1 ]] || die "a new generation appeared"
expect "login: "
reboot_log=$(tail -c +$((mark + 1)) "$serial")
[[ $reboot_log != *"Choose a password"* ]] || die "the console asked for the password again"
log "  no apply and no password question"
stop_vm

log "4. a new router whose router.toml fails a health check undoes it"
# good.toml without its VLAN and DHCP (so every service starts), and lan on a MAC
# address no port has: it applies, but nothing listens on lan.
sed -e '/^\[\[vlan\]\]/,$d' -e 's/^match_mac = "52:54:00:00:00:01"/match_mac = "52:54:00:00:00:99"/' \
  "$here/apply/good.toml" > "$work/broken.toml"
grep -q '52:54:00:00:00:99' "$work/broken.toml" || die "couldn't make the broken config"
new_router broken "$work/broken.toml"
boot broken
expect "were undone" "$timeout_s"
expect "Choose a password for this router"
wait_firstboot
[[ $(vm 'systemctl show -p ActiveState --value rollgate-firstboot') == failed ]] || die "rollgate-firstboot didn't report the failure"
vm 'test ! -e /var/lib/rollgate/current && test ! -e /var/lib/rollgate/pending' ||
  die "the broken config wasn't undone: $(vm 'rollgate status' 2>&1)"
vm 'grep -q "\"state\": *\"reverted\"" /var/lib/rollgate/generations/1/meta.json' ||
  die "generation 1 isn't marked reverted: $(vm 'cat /var/lib/rollgate/generations/1/meta.json')"
vm 'journalctl -b -u rollgate-firstboot --no-pager | grep -q "40-ssh"' || die "the journal doesn't say which check failed"
vm 'grep -q "journalctl -u rollgate-firstboot" /run/issue.d/50-rollgate.issue' || die "the login prompt doesn't mention the failure"
log "  undone, reachable with the image's defaults, and the console and the journal say why"
echo "firstboot-test: PASS"
