#!/usr/bin/env bash
# L3 apply test (docs/testing.md, ADR 0010): proves in a VM that `rollgate apply`
# can't lock you out.
#
#   1. A config naming a missing SSH key file is refused; nothing changes.
#   2. Apply a working config. `rollgate confirm` from the same SSH connection is
#      refused; from a new connection it works. The config is running (addresses,
#      VLAN, firewall, dnsmasq, Unbound) and survives a reboot.
#   3. Apply a config that refuses SSH from lan and don't confirm: new SSH
#      connections fail, then the revert timer brings SSH back by itself.
#   4. Apply the same config with a long timeout and cut the power: the boot
#      reverts it before SSH starts.
#
# Usage:  sudo tests/vm/apply-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2224)
#         ROLLGATE   a rollgate binary to test instead of the image's (for development)
#
# The image is never modified: the test boots a sparse copy with a throwaway SSH key
# in root's authorized_keys and in /etc/rollgate/secrets/admin.pub.

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: apply-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2224}
monitor_port=$((ssh_port + 1))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/apply.serial.log"
report="$log_dir/apply.report.log"
# The MACs in tests/vm/apply/*.toml.
lan_mac=52:54:00:00:00:01
wan_mac=52:54:00:00:00:02

die() { echo "apply-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 ssh ssh-keygen; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done

if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=180
else
  echo "apply-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=900
fi

work=$(mktemp -d)
qemu_pid=
cleanup() {
  if [[ -n $qemu_pid ]]; then kill "$qemu_pid" 2>/dev/null || true; fi
  umount "$work/root" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/root" "$log_dir"

vm() {
  ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes root@127.0.0.1 "$@"
}

# wait_for_boot <n>: waits for the n-th boot report and for SSH, then checks the report
# says the system is running with nothing failed.
wait_for_boot() {
  local n=$1 deadline=$((SECONDS + timeout_s)) line
  while (($(grep -ac '^ROLLGATE_BOOT state=' "$report" || true) < n)); do
    ((SECONDS < deadline)) || die "boot $n: no ROLLGATE_BOOT line within ${timeout_s}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "boot $n: QEMU exited (log: $serial)"
    sleep 2
  done
  line=$(grep -a '^ROLLGATE_BOOT state=' "$report" | sed -n "${n}p" | tr -d '\r')
  echo "    $line"
  [[ $line == *"state=running "* && $line == *"failed=" ]] || die "boot $n did not come up cleanly: $line"
  until vm true 2>/dev/null; do
    ((SECONDS < deadline)) || die "boot $n: SSH not reachable"
    sleep 2
  done
}

# expect <what> <pattern> <command>: runs a command in the VM and checks its output.
expect() {
  local what=$1 pattern=$2 out
  out=$(vm "$3" 2>&1) || true
  grep -qE -- "$pattern" <<<"$out" || die "$what: expected /$pattern/ in the output of '$3':
$out"
}

# The running system matches tests/vm/apply/good.toml.
check_good_config() {
  expect "lan address" "10\.0\.2\.15/24" 'ip -br addr show lan'
  expect "wan renamed" "^wan " 'ip -br link show wan'
  expect "iot VLAN" "192\.168\.20\.1/24" 'ip -br addr show iot'
  expect "firewall" 'iifname ([{] )?"lan"( [}])? tcp dport 22' 'nft list table inet rollgate'
  expect "services" "^active
active$" 'systemctl is-active dnsmasq unbound'
  expect "placeholder networking removed" "^gone$" 'test -e /etc/systemd/network/99-rollgate-unconfigured.network || echo gone'
}

log "preparing a copy of the image with a throwaway SSH key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
for f in "$here"/apply/*.toml; do install -m 0644 "$f" "$work/root/root/"; done
if [[ -n ${ROLLGATE:-} ]]; then
  echo "    testing $ROLLGATE instead of the image's rollgate"
  install -m 0755 "$ROLLGATE" "$work/root/usr/bin/rollgate"
fi
umount "$work/root"

: > "$serial"
: > "$report"
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=$lan_mac,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -nic user,model=virtio-net-pci,mac=$wan_mac,net=10.0.3.0/24 \
  -display none -monitor tcp:127.0.0.1:"$monitor_port",server=on,wait=off \
  -chardev file,id=report,path="$report" \
  -device virtio-serial-pci -device virtserialport,chardev=report,name=org.rollgate.report \
  -serial file:"$serial" &
qemu_pid=$!

log "boot 1: fresh image"
wait_for_boot 1
expect "fresh status" "Nothing has been applied yet" 'rollgate status'

log "1. a config naming a missing key file is refused"
expect "missing key refused" 'laptop\.pub.*missing or empty' 'rollgate apply /root/missing-key.toml'
expect "nothing changed" "Nothing has been applied yet" 'rollgate status'

log "2. apply a working config; confirm only from a new connection"
out=$(vm 'cp /root/good.toml /etc/rollgate/router.toml && rollgate apply && rollgate confirm' 2>&1) &&
  die "rollgate confirm was accepted from the connection that ran apply:
$out"
grep -q "Applied generation 1" <<<"$out" || die "apply didn't apply:
$out"
grep -q "new SSH connection" <<<"$out" || die "confirm from the same connection failed for the wrong reason:
$out"
echo "    confirm from the applying connection: refused"
expect "confirm" "Confirmed generation 1" 'rollgate confirm'
check_good_config
echo "    running: lan 10.0.2.15/24, wan, iot VLAN, firewall, dnsmasq and Unbound"

log "boot 2: the confirmed config survives a reboot"
vm systemctl reboot 2>/dev/null || true
wait_for_boot 2
expect "status after reboot" "Running: generation 1, confirmed" 'rollgate status'
expect "boot revert skipped" "^ConditionResult=no$" 'systemctl show -p ConditionResult rollgate-boot.service'
check_good_config

log "3. lockout: SSH from lan refused, not confirmed"
expect "lockout applied" "Applied generation 2" 'cp /root/lockout.toml /etc/rollgate/router.toml && rollgate apply'
start=$SECONDS
vm true 2>/dev/null && die "a new SSH connection still worked after the lockout config was applied"
echo "    new SSH connections are refused"
until vm true 2>/dev/null; do
  ((SECONDS - start < 90)) || die "SSH didn't come back within 90 s of a 30 s confirm timeout"
  sleep 2
done
echo "    SSH came back by itself after $((SECONDS - start)) s"
expect "reverted by the timer" "Running: generation 1, confirmed" 'rollgate status'
expect "generation 2 reverted" "^ +2  reverted" 'rollgate status'
check_good_config

log "4. lockout again with a 900 s timeout, then a power cut"
expect "lockout applied" "Applied generation 3" \
  'sed "s/^confirm_timeout_seconds = 30/confirm_timeout_seconds = 900/" /root/lockout.toml > /etc/rollgate/router.toml && rollgate apply'
vm true 2>/dev/null && die "a new SSH connection still worked after the lockout config was applied"
printf 'system_reset\n' > "/dev/tcp/127.0.0.1/$monitor_port"
echo "    power cut (QEMU system_reset) while generation 3 waits for confirmation"

log "boot 3: the boot reverts the unconfirmed change before SSH starts"
wait_for_boot 3
expect "boot revert ran" "Reverted generation 3" 'journalctl -b -u rollgate-boot.service'
expect "status after boot revert" "Running: generation 1, confirmed" 'rollgate status'
expect "generation 3 reverted" "^ +3  reverted" 'rollgate status'
check_good_config

log "PASS: rollgate apply can't lock you out (log: $serial)"
