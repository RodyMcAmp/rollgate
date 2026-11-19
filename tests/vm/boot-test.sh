#!/usr/bin/env bash
# L3 boot test (docs/testing.md): boots the image in QEMU under UEFI with the default
# kernel (linux) and with the fallback (linux-lts), then on a machine with no hardware
# watchdog, then from a disk bigger than the image, and checks the ROLLGATE_BOOT line
# each boot reports (on a virtio-serial channel of its own): a clean boot, the right
# kernel, a watchdog armed. After the last boot the root partition and filesystem must
# fill the bigger disk.
#
# Usage:  sudo tests/vm/boot-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR    where serial logs are kept (default: next to the image, test-logs/)
#         OVMF_CODE, OVMF_VARS   UEFI firmware (default: Arch edk2-ovmf paths)
#
# Root is needed to loop-mount the ESP (to read grub.cfg and set GRUB's next_entry).
# The image itself is never modified; each boot uses a sparse copy.
set -euo pipefail

image=${1:?usage: boot-test.sh <image>}
image=$(realpath "$image")
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
# Must match esp_start_sector in image/build.sh.
esp_offset=$((2048 * 512))

die() { echo "boot-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 grub-editenv sfdisk losetup btrfs; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done

if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420 # boot waits for the health checks, up to 300 s
else
  echo "boot-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid=
cleanup() {
  if [[ -n $qemu_pid ]]; then kill "$qemu_pid" 2>/dev/null || true; fi
  umount "$work/esp" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/esp" "$log_dir"

# Title of the linux-lts entry, e.g. "rollgate Linux, with Linux linux-lts". Matching on
# a trailing "lts" skips the "(fallback initramfs)" variant.
mount -o loop,ro,offset="$esp_offset" "$image" "$work/esp"
lts_entry=$(grep -oP "^menuentry '\K[^']*lts(?=')" "$work/esp/grub/grub.cfg" | head -n1 || true)
umount "$work/esp"
[[ -n $lts_entry ]] || die "no linux-lts menu entry in grub.cfg"

# check_grown <disk> <name>: after a boot on a disk bigger than the image, the root
# partition reaches the end of the disk and its Btrfs filesystem fills the partition
# (docs/architecture.md#disk-growth).
check_grown() {
  local disk=$1 name=$2 start size last loop fs_bytes
  read -r start size < <(sfdisk -d "$disk" | sed -n "s|^$disk""2 : start= *\([0-9]*\), size= *\([0-9]*\),.*|\1 \2|p")
  last=$(sfdisk -d "$disk" | sed -n 's/^last-lba: //p')
  [[ -n $start && -n $last ]] || die "'$name': can't read the partition table"
  # Partitions end on a 1 MiB boundary, so up to 2047 sectors may stay free.
  ((start + size - 1 > last - 2048)) ||
    die "'$name': the root partition ends at sector $((start + size - 1)), not near the disk's end ($last)"
  loop=$(losetup -f --show -o $((start * 512)) --sizelimit $((size * 512)) "$disk")
  fs_bytes=$(btrfs inspect-internal dump-super "$loop" | awk '$1 == "total_bytes" { print $2; exit }')
  losetup -d "$loop"
  ((fs_bytes > size * 512 - 1048576)) ||
    die "'$name': the filesystem is $fs_bytes bytes, the partition $((size * 512))"
  echo "    grown: root partition and filesystem $((fs_bytes / 1048576)) MiB"
}

# boot_once <name> <grub-entry, or "" for the default> <expected kernel: default|lts>
#           <QEMU machine> <expected watchdog= value> [disk size in MiB: check it grows]
boot_once() {
  local name=$1 entry=$2 want=$3 machine=$4 want_watchdog=$5 disk_mib=${6:-}
  local disk="$work/$name.img" vars="$work/$name.vars.fd" serial="$log_dir/$name.serial.log"
  local report="$log_dir/$name.report.log"

  log "boot '$name'${entry:+ (entry: $entry)}${disk_mib:+ (disk: $disk_mib MiB)}"
  cp --sparse=always "$image" "$disk"
  [[ -z $disk_mib ]] || truncate -s "${disk_mib}M" "$disk"
  cp "$ovmf_vars" "$vars"
  : > "$serial"
  : > "$report"
  if [[ -n $entry ]]; then
    mount -o loop,offset="$esp_offset" "$disk" "$work/esp"
    grub-editenv "$work/esp/grub/grubenv" set "next_entry=$entry"
    umount "$work/esp"
  fi

  qemu-system-x86_64 -machine "$machine" -m 1024 -smp 2 "${accel[@]}" \
    -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
    -drive if=pflash,format=raw,file="$vars" \
    -drive file="$disk",format=raw,if=virtio \
    -nic user,model=virtio-net-pci \
    -chardev file,id=report,path="$report" \
    -device virtio-serial-pci -device virtserialport,chardev=report,name=org.rollgate.report \
    -display none -monitor none -serial file:"$serial" &
  qemu_pid=$!

  # The boot report arrives on its own virtio-serial channel (see
  # image/rootfs/usr/local/lib/rollgate/boot-report), never mixed with console output.
  local line="" deadline=$((SECONDS + timeout_s))
  while ((SECONDS < deadline)); do
    line=$(grep -a -m1 '^ROLLGATE_BOOT state=' "$report" | tr -d '\r' || true)
    [[ -n $line ]] && break
    kill -0 "$qemu_pid" 2>/dev/null || break
    sleep 2
  done
  kill "$qemu_pid" 2>/dev/null || true
  wait "$qemu_pid" 2>/dev/null || true
  qemu_pid=

  [[ -n $line ]] || die "'$name': no ROLLGATE_BOOT line within ${timeout_s}s (serial log: $serial)"
  echo "    $line"
  [[ -z $disk_mib ]] || check_grown "$disk" "$name"
  rm -f "$disk" "$vars"

  local state kernel watchdog failed
  state=$(sed -n 's/.*state=\([^ ]*\).*/\1/p' <<<"$line")
  kernel=$(sed -n 's/.*kernel=\([^ ]*\).*/\1/p' <<<"$line")
  watchdog=$(sed -n 's/.*watchdog=\([^ ]*\).*/\1/p' <<<"$line")
  failed=$(sed -n 's/.*failed=\([^ ]*\).*/\1/p' <<<"$line")

  [[ $state == running ]] || die "'$name': system state is '$state', failed units: '${failed:-none}' (log: $serial)"
  case $want in
    default) [[ $kernel != *-lts ]] || die "'$name': expected the default kernel, booted $kernel" ;;
    lts)     [[ $kernel == *-lts ]] || die "'$name': expected linux-lts, booted $kernel" ;;
  esac
  [[ $watchdog == "$want_watchdog" ]] || die "'$name': expected watchdog=$want_watchdog, got '$watchdog' (log: $serial)"
  # ADR 0013: systemd-timesyncd keeps the clock right.
  [[ $line == *" timesync=active "* ]] || die "'$name': systemd-timesyncd isn't running (log: $serial)"
}

# q35 has a hardware watchdog (iTCO); the older pc machine has none, so the image must
# fall back to softdog (docs/architecture.md#watchdog).
boot_once default "" default q35 iTCO_wdt:active
boot_once lts "$lts_entry" lts q35 iTCO_wdt:active
boot_once no-hw-watchdog "" default pc Software_Watchdog:active
# Written to a disk bigger than the image, the root partition and filesystem fill it.
boot_once bigger-disk "" default q35 iTCO_wdt:active 12288

log "PASS: both kernels boot cleanly, with a watchdog armed, and the root grows to fill the disk (logs: $log_dir)"
