#!/usr/bin/env bash
# Turns a rollgate disk image into a VMware Workstation virtual machine
# (docs/vmware.md): a .vmdk disk and a .vmx file with the settings rollgate needs.
#
# Usage:  sudo SSH_KEY=~/.ssh/id_ed25519.pub image/vmware.sh IMAGE [OUTDIR]
#         IMAGE   build/rollgate-<date>.img (make image), or a release's image, unpacked
#         OUTDIR  where the VM's folder goes (default: build/vmware). Under WSL, a Windows
#                 folder such as "/mnt/c/Users/<you>/Documents/Virtual Machines" works.
# Env:    SSH_KEY  your SSH public key. It is installed for root and as
#                  /etc/rollgate/secrets/admin.pub, and image/vmware/router.toml becomes
#                  /etc/rollgate/router.toml, so `rollgate apply` at the console sets the
#                  router up for VMware's networks.
#
# Output: OUTDIR/rollgate-<date>/ with rollgate.vmx and rollgate.vmdk.
#
# The VM: UEFI (Secure Boot off), 2 CPUs, 2 GB, a 32 GB disk on NVMe, and two vmxnet3
# network adapters with fixed MAC addresses, so the config can name them:
#   wan  00:50:56:3a:00:01  VMware's NAT network, VMnet8 (the VM's "internet")
#   lan  00:50:56:3a:00:02  VMnet2, a host-only network of its own (192.168.50.0/24,
#                           VMware's DHCP off), so your PC is a LAN client and other
#                           VMs on VMnet1 aren't affected. docs/vmware.md sets it up.
# Two VMs made by this script share those addresses; don't run both on one network.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
die() { echo "vmware: error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $# -ge 1 && $# -le 2 ]] || die "usage: SSH_KEY=<public key file> $0 IMAGE [OUTDIR]"
image=$1
outdir=${2:-$repo_root/build/vmware}
[[ -f $image ]] || die "no image at $image"
[[ $(basename "$image") =~ ^rollgate-([0-9]{4}-[0-9]{2}-[0-9]{2})\.img$ ]] ||
  die "$image: expected a name like rollgate-2026-11-16.img"
date=${BASH_REMATCH[1]}
[[ -n ${SSH_KEY:-} && -f $SSH_KEY ]] || die "set SSH_KEY to your SSH public key file (see docs/vmware.md)"
grep -qE '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-[a-z0-9-]+) ' "$SSH_KEY" ||
  die "$SSH_KEY doesn't look like an SSH public key (the .pub file)"
[[ $EUID -eq 0 ]] || die "must run as root (it mounts the image to install your key)"
for tool in qemu-img losetup mount; do
  command -v "$tool" >/dev/null || die "missing '$tool' (pacman -S qemu-img)"
done

vm="$outdir/rollgate-$date"
[[ ! -e $vm ]] || die "$vm already exists; move it away first"
work=$(mktemp -d)
cleanup() {
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT

log "copying $image"
cp --sparse=always "$image" "$work/disk.img"
# A 32 GB virtual disk: the router grows its root filesystem to fill it on first boot
# (docs/architecture.md#disk-growth). The .vmdk only stores what's used.
if (($(stat -c %s "$work/disk.img") < 32 * 1024 ** 3)); then
  truncate -s 32G "$work/disk.img"
fi

log "installing your SSH key and the VMware router.toml"
# The root partition starts after 1 MiB and the 512 MiB ESP (image/build.sh).
mkdir "$work/root"
mount -o loop,offset=$(((2048 + 512 * 2048) * 512)),subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$SSH_KEY" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$SSH_KEY" "$work/root/etc/rollgate/secrets/admin.pub"
install -m 0644 "$repo_root/image/vmware/router.toml" "$work/root/etc/rollgate/router.toml"
umount "$work/root"

log "converting to $vm/rollgate.vmdk"
mkdir -p "$vm"
# monolithicSparse: one growable file, VMware's own "single file" disk type.
qemu-img convert -O vmdk -o subformat=monolithicSparse,adapter_type=lsilogic \
  "$work/disk.img" "$vm/rollgate.vmdk"

cat > "$vm/rollgate.vmx" <<EOF
.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "19"
displayName = "rollgate $date"
guestOS = "other5xlinux-64"
firmware = "efi"
uefi.secureBoot.enabled = "FALSE"
numvcpus = "2"
memsize = "2048"
nvme0.present = "TRUE"
nvme0:0.present = "TRUE"
nvme0:0.fileName = "rollgate.vmdk"
floppy0.present = "FALSE"
pciBridge0.present = "TRUE"
pciBridge4.present = "TRUE"
pciBridge4.virtualDev = "pcieRootPort"
pciBridge4.functions = "8"
pciBridge5.present = "TRUE"
pciBridge5.virtualDev = "pcieRootPort"
pciBridge5.functions = "8"
pciBridge6.present = "TRUE"
pciBridge6.virtualDev = "pcieRootPort"
pciBridge6.functions = "8"
pciBridge7.present = "TRUE"
pciBridge7.virtualDev = "pcieRootPort"
pciBridge7.functions = "8"
ethernet0.present = "TRUE"
ethernet0.virtualDev = "vmxnet3"
ethernet0.connectionType = "nat"
ethernet0.addressType = "static"
ethernet0.address = "00:50:56:3a:00:01"
ethernet1.present = "TRUE"
ethernet1.virtualDev = "vmxnet3"
ethernet1.connectionType = "custom"
ethernet1.vnet = "VMnet2"
ethernet1.addressType = "static"
ethernet1.address = "00:50:56:3a:00:02"
EOF

log "done: open $vm/rollgate.vmx in VMware Workstation (docs/vmware.md)"
