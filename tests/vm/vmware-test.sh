#!/usr/bin/env bash
# L3 VMware test (docs/vmware.md): image/vmware.sh makes a VM that works.
#
#   1. image/vmware.sh turns the image into rollgate.vmx and rollgate.vmdk.
#   2. QEMU boots the .vmdk the way the .vmx describes the VM: UEFI, the disk on NVMe,
#      two vmxnet3 adapters with the .vmx's MAC addresses, and the LAN host at
#      192.168.50.1 as on VMnet2.
#   3. `rollgate apply` with the router.toml vmware.sh installed, confirmed over SSH to
#      192.168.50.2 from a new connection, and every health check passes.
#
# Usage:  sudo tests/vm/vmware-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2234)
#
# It doesn't run VMware itself; QEMU emulates the VM's NVMe disk and vmxnet3 adapters.

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: vmware-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2234}
serial="$log_dir/vmware.serial.log"

die() { echo "vmware-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 qemu-img ssh ssh-keygen; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done

if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  echo "vmware-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid=
cleanup() {
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir"

log "making the VM with image/vmware.sh"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
SSH_KEY=$work/key.pub bash "$here/../../image/vmware.sh" "$image" "$work/vm" >/dev/null
vm_dir=$(echo "$work"/vm/rollgate-*)
vmx="$vm_dir/rollgate.vmx"
[[ -f $vmx && -f $vm_dir/rollgate.vmdk ]] || die "vmware.sh made no .vmx and .vmdk"
vmx_get() { sed -n "s/^$1 = \"\(.*\)\"$/\1/p" "$vmx"; }
[[ $(vmx_get firmware) == efi && $(vmx_get uefi.secureBoot.enabled) == FALSE ]] || die ".vmx: not UEFI without Secure Boot"
[[ $(vmx_get nvme0:0.fileName) == rollgate.vmdk ]] || die ".vmx: the disk isn't rollgate.vmdk on NVMe"
[[ $(vmx_get ethernet0.connectionType) == nat && $(vmx_get ethernet1.vnet) == VMnet2 ]] || die ".vmx: wan isn't on NAT, or lan isn't on VMnet2"
wan_mac=$(vmx_get ethernet0.address)
lan_mac=$(vmx_get ethernet1.address)
for mac in "$wan_mac" "$lan_mac"; do
  grep -q "match_mac = \"$mac\"" "$here/../../image/vmware/router.toml" ||
    die "image/vmware/router.toml doesn't name the .vmx's MAC address $mac"
done
qemu-img info "$vm_dir/rollgate.vmdk" | grep -q "create type: monolithicSparse" || die "rollgate.vmdk isn't a VMware single-file disk"

log "booting rollgate.vmdk on NVMe with two vmxnet3 adapters"
cp "$ovmf_vars" "$work/vars.fd"
: > "$serial"
# lan: a QEMU user network laid out like VMnet2 (the host is 192.168.50.1). SSH reaches
# the router at its DHCP address before the config is applied and at 192.168.50.2 after.
qemu-system-x86_64 -machine q35 -m 2048 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$vm_dir/rollgate.vmdk",format=vmdk,if=none,id=disk -device nvme,drive=disk,serial=rollgate \
  -netdev user,id=wan,net=10.0.3.0/24 -device vmxnet3,netdev=wan,mac="$wan_mac" \
  -netdev user,id=lan,net=192.168.50.0/24,host=192.168.50.1,dhcpstart=192.168.50.15,hostfwd=tcp:127.0.0.1:"$ssh_port"-192.168.50.15:22,hostfwd=tcp:127.0.0.1:$((ssh_port + 1))-192.168.50.2:22 \
  -device vmxnet3,netdev=lan,mac="$lan_mac" \
  -display none -monitor none -serial file:"$serial" &
qemu_pid=$!

vm() {
  local port=$1; shift
  timeout 120 ssh -i "$work/key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
deadline=$((SECONDS + timeout_s))
until vm "$ssh_port" true 2>/dev/null; do
  ((SECONDS < deadline)) || die "no SSH within ${timeout_s}s (serial log: $serial)"
  kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (serial log: $serial)"
  sleep 3
done

log "applying the VMware router.toml"
# The apply moves lan from its DHCP address to 192.168.50.2, which ends this SSH
# session, so it runs detached and leaves its output in a file.
vm "$ssh_port" 'nohup rollgate apply >/root/apply.log 2>&1 </dev/null &' ||
  die "couldn't start rollgate apply"
deadline=$((SECONDS + 60))
until out=$(vm "$((ssh_port + 1))" 'rollgate confirm' 2>&1); do
  if ((SECONDS >= deadline)); then
    log=$(vm "$ssh_port" 'cat /root/apply.log' 2>&1) || true
    die "no confirm over SSH to 192.168.50.2 after the apply: $out
apply's output: $log"
  fi
  sleep 3
done
log "  confirmed from a new connection to 192.168.50.2"
out=$(vm "$((ssh_port + 1))" 'rollgate health -wait 120' 2>&1) || die "health checks failed:
$out"
log "  every health check passes"
echo "vmware-test: PASS"
