#!/usr/bin/env bash
# L3 rollback rehearsal (docs/testing.md): walks through docs/runbooks/recovery.md
# section 2 in a VM, without a human.
#
#   1. Boot the image, log in over SSH, and simulate a bad update by removing linux-lts
#      and nano with pacman. snap-pac takes a "pre" snapshot first.
#   2. Boot that pre snapshot from GRUB's snapshot menu, with the linux-lts kernel that
#      only exists inside the snapshot (proves /boot is snapshotted, ADR 0004).
#   3. Run rollgate-snapshot-rollback from inside the snapshot, then reboot normally.
#   4. Check the system is restored (kernel, package, broken copy kept), and that the
#      restored linux-lts boots from the normal menu.
#
# Usage:  sudo tests/vm/rollback-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2222)
#
# The image is never modified: the test boots a sparse copy with a throwaway SSH key
# added to root's authorized_keys.

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: rollback-test.sh <image>}
image=$(realpath "$image")
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2222}
monitor_port=$((ssh_port + 1))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/rollback.serial.log"

die() { echo "rollback-test: FAIL: $*" >&2; exit 1; }
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
  echo "rollback-test: /dev/kvm not available, using slow software emulation" >&2
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

# wait_for_boot <n>: waits for the n-th boot report on the serial console and for SSH,
# then checks the report says the system is running with nothing failed.
wait_for_boot() {
  local n=$1 deadline=$((SECONDS + timeout_s)) line
  while (($(grep -ac 'ROLLGATE_BOOT state=' "$serial" || true) < n)); do
    ((SECONDS < deadline)) || die "boot $n: no ROLLGATE_BOOT line within ${timeout_s}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "boot $n: QEMU exited (log: $serial)"
    sleep 2
  done
  line=$(grep -a 'ROLLGATE_BOOT state=' "$serial" | sed -n "${n}p" | tr -d '\r')
  echo "    ${line#*boot-report*: }"
  [[ $line == *"state=running "* && $line == *"failed=" ]] || die "boot $n did not come up cleanly: $line"
  until vm true 2>/dev/null; do
    ((SECONDS < deadline)) || die "boot $n: SSH not reachable"
    sleep 2
  done
}

reboot_vm() { vm systemctl reboot 2>/dev/null || true; }

# GRUB boots this entry once on the next boot. It can't reach into the snapshot menu,
# which GRUB loads from a separate file; use press_keys there.
set_next_entry() { vm "grub-editenv /efi/grub/grubenv set $(printf %q "next_entry=$1")"; }

# press_keys <key>...: types on the VM's keyboard through the QEMU monitor, like a person
# at the console (key names as in QEMU's sendkey: down, ret, ...).
press_keys() {
  local fd key
  exec {fd}<>"/dev/tcp/127.0.0.1/$monitor_port"
  for key in "$@"; do
    printf 'sendkey %s\n' "$key" >&"$fd"
    sleep 0.3
  done
  exec {fd}>&-
}

# wait_for_text <text> <count>: waits until <text> has appeared <count> times on the console.
wait_for_text() {
  local text=$1 count=$2 deadline=$((SECONDS + timeout_s))
  until (($(grep -acF "$text" "$serial" || true) >= count)); do
    ((SECONDS < deadline)) || die "'$text' did not appear on the console (log: $serial)"
    sleep 1
  done
  sleep 1
}

log "preparing a copy of the image with a throwaway SSH key"
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
  -display none -monitor tcp:127.0.0.1:"$monitor_port",server=on,wait=off \
  -serial file:"$serial" &
qemu_pid=$!

log "boot 1: fresh image"
wait_for_boot 1

log "simulating a bad update: pacman -R linux-lts nano"
vm 'pacman -R --noconfirm linux-lts nano' >/dev/null
pre=$(vm 'snapper --csvout list --columns number,type' | awk -F, '$2 == "pre" { n = $1 } END { print n }')
[[ -n $pre ]] || die "snap-pac did not create a pre snapshot"
vm '! test -e /boot/vmlinuz-linux-lts' || die "linux-lts kernel still present after removal"
echo "    pre-update snapshot: #$pre; linux-lts and nano are gone"

log "boot 2: pre-update snapshot #$pre from the GRUB snapshot menu, with linux-lts"
# grub-btrfsd rewrites the snapshot menu a moment after each snapshot.
for _ in $(seq 15); do
  vm "grep -qs '@snapshots/$pre/snapshot' /efi/grub/grub-btrfs.cfg" && break
  sleep 1
done
outer=$(vm 'cat /efi/grub/grub.cfg' | grep -oP "^submenu '\K[^']*" | head -n1)
[[ -n $outer ]] || die "no snapshot menu in grub.cfg"
# Where the person has to move the cursor: the snapshot list starts with a header line,
# then one submenu per snapshot; inside a snapshot, an info line, then one entry per kernel.
read -r snapshot_downs kernel_downs < <(vm 'cat /efi/grub/grub-btrfs.cfg' | awk -v snap="@snapshots/$pre/snapshot" '
  /^submenu / { top++; target = index($0, snap) > 0; if (target) pos = top; item = 0; next }
  target && /^[ \t]+(submenu|menuentry) / {
    if (index($0, "vmlinuz-linux-lts ") && kernel == "") kernel = item
    item++
  }
  END { print pos, kernel }')
[[ -n $snapshot_downs && -n $kernel_downs ]] || die "snapshot #$pre or its linux-lts entry is not in the GRUB snapshot menu"
list_seen=$(grep -acF '| Type |' "$serial" || true)
kernels_seen=$(grep -acF 'initramfs-linux-lts.img' "$serial" || true)
set_next_entry "$outer"
reboot_vm
wait_for_text '| Type |' $((list_seen + 1))
mapfile -t keys < <(yes down | head -n "$snapshot_downs")
press_keys "${keys[@]}" ret
wait_for_text 'initramfs-linux-lts.img' $((kernels_seen + 1))
mapfile -t keys < <(yes down | head -n "$kernel_downs")
press_keys "${keys[@]}" ret
wait_for_boot 2
[[ $(vm 'findmnt -no FSTYPE /') == overlay ]] || die "boot 2 is not a snapshot boot (root is not the overlay)"
[[ $(vm 'uname -r') == *-lts ]] || die "boot 2 did not use the linux-lts kernel from the snapshot"
echo "    running from snapshot #$pre (read-only + overlay) with $(vm 'uname -r')"

log "rolling back with rollgate-snapshot-rollback $pre"
vm "rollgate-snapshot-rollback $pre" | sed 's/^/    /'
reboot_vm

log "boot 3: normal boot of the restored system"
wait_for_boot 3
[[ $(vm 'findmnt -no FSTYPE /') == btrfs ]] || die "boot 3: root is not Btrfs"
vm 'findmnt -no OPTIONS / | grep -q "subvol=/@\(,\|$\)"' || die "boot 3: root is not the @ subvolume"
vm 'test -e /boot/vmlinuz-linux-lts' || die "boot 3: linux-lts kernel was not restored"
vm 'pacman -Q linux-lts nano' >/dev/null || die "boot 3: packages were not restored"
broken=$(vm 'mount -o subvolid=5 "$(findmnt -no SOURCE / | sed "s/\[.*//")" /mnt && ls /mnt | grep "^@broken-"; umount /mnt')
[[ -n $broken ]] || die "boot 3: the broken system was not kept as @broken-*"
echo "    restored: linux-lts and nano are back; broken system kept as $broken"

log "boot 4: restored linux-lts from the normal menu"
lts_entry=$(vm 'cat /efi/grub/grub.cfg' | grep -oP "^menuentry '\K[^']*lts(?=')" | head -n1)
set_next_entry "$lts_entry"
reboot_vm
wait_for_boot 4
[[ $(vm 'uname -r') == *-lts ]] || die "boot 4 did not use linux-lts"

log "PASS: rollback rehearsal (log: $serial)"
