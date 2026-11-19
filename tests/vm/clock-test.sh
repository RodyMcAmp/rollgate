#!/usr/bin/env bash
# L3 clock test (ADR 0013, 0015): the router keeps its clock right, even without DNS.
#
#   1. On a normal boot, systemd-timesyncd synchronizes from arch.pool.ntp.org.
#   2. With the router's own name lookups broken and its clock set 9 hours back (as
#      VMware did, handing over local time as UTC), it still synchronizes, from
#      Cloudflare's time service by IP address, and the clock is right again.
#
# Needs internet (public NTP), like the update trial test.
# Usage:  sudo tests/vm/clock-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2262)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: clock-test.sh <image>}
image=$(realpath "$image")
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2262}
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/clock.serial.log"

die() { echo "clock-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
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
umount "$work/root"

: > "$serial"
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -display none -monitor none -serial file:"$serial" &
qemu_pid=$!

vm() {
  timeout 800 ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
deadline=$((SECONDS + timeout_s))
until vm true 2>/dev/null; do
  ((SECONDS < deadline)) || die "no SSH within ${timeout_s}s (serial log: $serial)"
  kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (serial log: $serial)"
  sleep 3
done
# synced SECONDS: wait until the clock is synchronized, then print timesyncd's server.
vm 'cat > /root/synced <<"EOF"
#!/bin/sh
for _ in $(seq "$1"); do
  [ "$(timedatectl show -p NTPSynchronized --value)" = yes ] && exec timedatectl show-timesync -p ServerName --value
  sleep 1
done
echo "not synchronized after $1 s" >&2
timedatectl timesync-status >&2
exit 1
EOF
chmod +x /root/synced'

log "1. a normal boot synchronizes"
# The pool or Cloudflare: at boot the first name is often looked up before the
# network is up, and timesyncd then goes on to the next server (ADR 0015).
server=$(vm '/root/synced 120') || die "the clock didn't synchronize: $server"
[[ $server == *.arch.pool.ntp.org || $server == 162.159.200.1 || $server == 162.159.200.123 ]] ||
  die "synchronized from '$server', which isn't in the configured list"
log "  synchronized from $server"

log "2. no DNS, clock 9 hours back"
# Every DNS server, global or from DHCP, is out of reach: drop the router's own DNS
# traffic, and forget what it already looked up.
vm 'nft add table inet clocktest
nft add chain inet clocktest out "{ type filter hook output priority 0; }"
nft add rule inet clocktest out meta l4proto "{ tcp, udp }" th dport 53 drop
resolvectl flush-caches
systemctl stop systemd-timesyncd
rm -f /var/lib/systemd/timesync/clock
date -s "@$(($(date +%s) - 9 * 3600))" >/dev/null'
if vm 'getent hosts 0.arch.pool.ntp.org' >/dev/null 2>&1; then
  die "the router can still look up names; the test didn't break DNS"
fi
[[ $(vm 'timedatectl show -p NTPSynchronized --value') == no ]] || die "the clock still counts as synchronized after setting it back"
vm 'systemctl start systemd-timesyncd'
# Without DNS this takes about 8 minutes: timesyncd looks up each entry, even an
# address, through systemd-resolved, which waits about 2 minutes per lookup (ADR 0015).
server=$(vm '/root/synced 720') || die "without DNS, the clock didn't synchronize: $server"
[[ $server == 162.159.200.1 || $server == 162.159.200.123 ]] || die "synchronized from '$server', want a Cloudflare address"
skew=$(($(vm 'date +%s') - $(date +%s)))
((skew > -10 && skew < 10)) || die "the clock is still ${skew}s off after synchronizing"
log "  synchronized from $server; the clock is within ${skew#-}s of the host's"
echo "clock-test: PASS"
