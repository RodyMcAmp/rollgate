#!/usr/bin/env bash
# L3 install test (ADR 0021): the USB installer puts rollgate on a blank disk.
#
#   1. QEMU starts the installer image as a USB stick, with a blank 20 GB disk, three
#      network ports (the first with DHCP from QEMU, the others on networks with nothing on them),
#      and ssh-key.pub on the stick. The installer, driven over the serial console,
#      picks the first port as WAN, the second as LAN at 192.168.10.1, lists the
#      third as unused, and offers the disk but not the stick.
#   2. Until erase is typed, it writes nothing to the disk; then it installs.
#   3. The stick has install-log.txt and hardware.txt.
#   4. The disk boots on its own, with LAN now on a network the test can reach: the
#      first boot (ADR 0020) applies the detected config and keeps it (WAN has an
#      address, LAN is 192.168.10.1), the stick's key logs in over SSH, and the
#      console asks for the router's password.
#
# Usage:  sudo tests/vm/install-test.sh build/rollgate-installer-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; PORT (default 2400: SSH)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

installer=${1:?usage: install-test.sh <installer image>}
installer=$(realpath "$installer")
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$installer")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${PORT:-2400}
# The stick's FAT partition starts at sector 2048 (image/installer.sh).
fat_offset=$((2048 * 512))
mac_wan=52:54:00:12:00:01 mac_lan=52:54:00:12:00:02 mac_spare=52:54:00:12:00:03

die() { echo "install-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $installer ]] || die "no installer image at $installer (run: make installer)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 qemu-img ssh-keygen mkfifo; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1800
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
  mountpoint -q "$work/fat" && umount "$work/fat"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/fat"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"

log "preparing the stick and a blank disk"
cp --sparse=always "$installer" "$work/stick.img"
mount -o loop,offset="$fat_offset" "$work/stick.img" "$work/fat"
[[ -f $work/fat/rollgate/README.txt ]] || die "the stick has no rollgate/README.txt"
# As a Windows editor saves it: CRLF line ends.
sed 's/$/\r/' "$work/key.pub" > "$work/fat/rollgate/ssh-key.pub"
umount "$work/fat"
qemu-img create -q -f qcow2 "$work/disk.qcow2" 20G
cp "$ovmf_vars" "$work/vars.fd"

# start NAME QEMU-ARGS...: start a VM whose serial console is a pair of FIFOs: what
# it prints goes to $serial, and send() types into it.
start() {
  serial="$log_dir/install-$1.serial.log"
  shift
  : > "$serial"
  mark=0
  rm -f "$work/con.in" "$work/con.out"
  mkfifo "$work/con.in" "$work/con.out"
  qemu-system-x86_64 -machine q35 -m 2048 -smp 2 "${accel[@]}" \
    -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
    -drive if=pflash,format=raw,file="$work/vars.fd" \
    "$@" \
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
# The disk holds no data at all.
disk_blank() { ! qemu-img map -U --output=json "$work/disk.qcow2" | grep -q '"data": true'; }
# No network boot ROMs: the firmware boots the first disk without trying PXE.
nic() { echo "virtio-net-pci,netdev=$1,mac=$2,romfile="; }
# A network with nothing on it: the port's link is up, and what it sends goes to a
# UDP port on the host where nobody listens.
nowhere() { echo "socket,id=$1,udp=127.0.0.1:$2,localaddr=127.0.0.1:$(($2 + 1))"; }

log "1. the installer finds the ports, the disk and the key"
start installer \
  -device qemu-xhci -drive if=none,id=stick,format=raw,file="$work/stick.img" \
  -device usb-storage,drive=stick,bootindex=0 \
  -drive file="$work/disk.qcow2",format=qcow2,if=virtio,discard=unmap,detect-zeroes=unmap \
  -netdev user,id=wan -device "$(nic wan "$mac_wan")" \
  -netdev "$(nowhere lan $((ssh_port + 10)))" -device "$(nic lan "$mac_lan")" \
  -netdev "$(nowhere spare $((ssh_port + 20)))" -device "$(nic spare "$mac_spare")"
expect "rollgate installer, release" "$timeout_s"
expect "Looking for your modem"
expect "Internet (WAN):" 90
expect "$mac_wan  virtio_net, got 10.0.2.15 from the modem"
expect "Home network (LAN): "
expect "$mac_lan  virtio_net, the router will be 192.168.10.1"
expect "Not used for now:"
expect "$mac_spare  virtio_net"
expect "SSH: the key from the stick's ssh-key.pub will log in as root."
expect "The disk: vda  21 GB"
log "  WAN, LAN and the unused port as expected; the disk offered, the stick not"

log "2. nothing is written until erase is typed"
expect "Type erase and press Enter"
send "yes"
expect "Nothing was written."
expect "Type erase and press Enter"
send ""
expect "Nothing was written."
expect "Type erase and press Enter"
disk_blank || die "the disk was written to before erase was typed"
send "erase"
expect "Installing rollgate"
expect "Writing: 100%" "$timeout_s"
expect "Checking: 100%" "$timeout_s"
expect "rollgate is installed on vda."
expect "Remove the USB stick, then press Enter to restart."
stop_vm
disk_blank && die "the disk is still blank"
log "  refused twice, then installed"

log "3. the stick has the reports"
mount -o loop,ro,offset="$fat_offset" "$work/stick.img" "$work/fat"
grep -q "rollgate on it: yes" "$work/fat/rollgate/install-log.txt" || die "install-log.txt doesn't show the refusal"
grep -q "rollgate is installed on vda." "$work/fat/rollgate/install-log.txt" || die "install-log.txt doesn't say it's installed"
for want in "Firmware: UEFI" "vda  21 GB" "$mac_spare  virtio_net" "PCI devices (lspci -nnk):" "Kernel driver in use: virtio-pci"; do
  grep -qF "$want" "$work/fat/rollgate/hardware.txt" || die "hardware.txt lacks '$want': $(cat "$work/fat/rollgate/hardware.txt")"
done
cp "$work/fat/rollgate/install-log.txt" "$work/fat/rollgate/hardware.txt" "$log_dir/"
umount "$work/fat"
log "  install-log.txt and hardware.txt are there"

log "4. the installed router sets itself up"
# LAN now on a QEMU network with the router's address in it, so the test can reach
# SSH there (management is from LAN only).
start router \
  -drive file="$work/disk.qcow2",format=qcow2,if=none,id=disk -device virtio-blk-pci,drive=disk,bootindex=0 \
  -netdev user,id=wan -device "$(nic wan "$mac_wan")" \
  -netdev user,id=lan,net=192.168.10.0/24,host=192.168.10.2,hostfwd=tcp:127.0.0.1:"$ssh_port"-192.168.10.1:22 \
  -device "$(nic lan "$mac_lan")" \
  -netdev "$(nowhere spare $((ssh_port + 20)))" -device "$(nic spare "$mac_spare")"
vm() {
  timeout 120 ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
deadline=$((SECONDS + timeout_s))
until vm 'systemctl is-system-running --wait >/dev/null 2>&1; systemctl show -p ActiveState --value rollgate-firstboot' 2>/dev/null |
  grep -qxE 'active|failed|inactive'; do
  ((SECONDS < deadline)) || die "no SSH at 192.168.10.1 with the stick's key, or no first boot, within ${timeout_s}s (log: $serial)"
  kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (log: $serial)"
  sleep 3
done
[[ $(vm 'systemctl show -p ActiveState --value rollgate-firstboot') == active ]] ||
  die "the first boot didn't succeed: $(vm 'journalctl -b -u rollgate-firstboot --no-pager | tail -30')"
[[ $(vm 'cat /var/lib/rollgate/current; test -e /var/lib/rollgate/pending && echo pending') == 1 ]] ||
  die "generation 1 isn't kept: $(vm 'rollgate status' 2>&1)"
[[ $(vm 'ip -4 -br addr show wan') == *10.0.2.15/24* ]] || die "WAN has no address: $(vm 'ip -br addr')"
[[ $(vm 'ip -4 -br addr show lan') == *192.168.10.1/24* ]] || die "LAN isn't 192.168.10.1: $(vm 'ip -br addr')"
vm "grep -q '^#   .*  $mac_spare  virtio_net' /etc/rollgate/router.toml" || die "router.toml doesn't list the unused port: $(vm 'cat /etc/rollgate/router.toml')"
vm 'rollgate validate' >/dev/null || die "router.toml isn't valid"
expect "Choose a password for this router" 60
log "  the detected config is kept, the key logs in at 192.168.10.1, and the console asks for a password"
echo "install-test: PASS"
