#!/usr/bin/env bash
# L3 boot test (docs/testing.md): boots the image in QEMU under UEFI with the default
# kernel (linux) and with the fallback (linux-lts), then once more on a machine with no
# hardware watchdog, and checks the ROLLGATE_BOOT line each boot reports (on a
# virtio-serial channel of its own): a clean boot, the right kernel, a watchdog armed.
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
for tool in qemu-system-x86_64 grub-editenv; do
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

# boot_once <name> <grub-entry, or "" for the default> <expected kernel: default|lts>
#           <QEMU machine> <expected watchdog= value>
boot_once() {
  local name=$1 entry=$2 want=$3 machine=$4 want_watchdog=$5
  local disk="$work/$name.img" vars="$work/$name.vars.fd" serial="$log_dir/$name.serial.log"
  local report="$log_dir/$name.report.log"

  log "boot '$name'${entry:+ (entry: $entry)}"
  cp --sparse=always "$image" "$disk"
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
  rm -f "$disk" "$vars"

  [[ -n $line ]] || die "'$name': no ROLLGATE_BOOT line within ${timeout_s}s (serial log: $serial)"
  echo "    $line"

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
}

# q35 has a hardware watchdog (iTCO); the older pc machine has none, so the image must
# fall back to softdog (docs/architecture.md#watchdog).
boot_once default "" default q35 iTCO_wdt:active
boot_once lts "$lts_entry" lts q35 iTCO_wdt:active
boot_once no-hw-watchdog "" default pc Software_Watchdog:active

log "PASS: both kernels boot cleanly, with a watchdog armed (logs: $log_dir)"
