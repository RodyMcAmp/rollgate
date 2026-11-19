# rollgate in VMware Workstation

Run rollgate as a virtual machine on your PC to try it, or to rehearse updates and the recovery runbook before using real hardware (M5).
You need VMware Workstation 16.2 or newer and the WSL2 Arch setup from [dev-setup.md](dev-setup.md).

What you get:

- A VM with two network ports, like a small router:
  - **wan** on VMware's NAT network: the router's "internet", through your PC.
  - **lan** on a host-only network of its own, **VMnet2** (192.168.50.0/24). Your PC is a LAN client at 192.168.50.1; the router is 192.168.50.2.
    Your other VMs, on VMnet1 or VMnet8, aren't affected.
- Your SSH key installed, and a ready `router.toml` for those networks ([image/vmware/router.toml](../image/vmware/router.toml)).

## 1. Create the VMnet2 network (once)

1. Open VMware Workstation. Choose **Edit → Virtual Network Editor**, then **Change Settings** (it asks for administrator rights).
2. Click **Add Network…**, choose **VMnet2**, then **OK**.
3. With VMnet2 selected:
   - **Host-only** is selected.
   - **Connect a host virtual network adapter to this network** is ticked.
   - **Use local DHCP service** is **not** ticked. The router hands out addresses on its LAN, as a real one does.
   - **Subnet IP:** `192.168.50.0`, **Subnet mask:** `255.255.255.0`.
4. Click **OK**.

If VMnet2 is already used for something else, pick another free VMnet number and change `ethernet1.vnet` in the VM's `rollgate.vmx` to match.

## 2. Create an SSH key (once)

In PowerShell:

```
ssh-keygen -t ed25519 -f $HOME\.ssh\rollgate_ed25519
```

A passphrase is optional. It protects the key if someone copies it off your PC.

## 3. Build the VM

In WSL (`wsl -d archlinux`), in the repository. You need `qemu-img` once: `pacman -S --needed qemu-img`.

Use a published release image:

```
gh release download release/2026.11.16 -R RodyMcAmp/rollgate -p 'rollgate-2026-11-16.img*'
sha256sum -c rollgate-2026-11-16.img.zst.sha256
zstd -d --sparse rollgate-2026-11-16.img.zst
sudo SSH_KEY=/mnt/c/Users/<you>/.ssh/rollgate_ed25519.pub bash image/vmware.sh rollgate-2026-11-16.img "/mnt/c/Users/<you>/Documents/Virtual Machines"
```

Or one you built with `make image`:

```
make vmware SSH_KEY=/mnt/c/Users/<you>/.ssh/rollgate_ed25519.pub VMWARE_DIR="/mnt/c/Users/<you>/Documents/Virtual Machines"
```

Either way you get a folder `rollgate-<date>` with `rollgate.vmx` and `rollgate.vmdk` (about 1.3 GB).

## 4. Start it and apply the config

1. In VMware: **File → Open…**, choose `rollgate.vmx`, then **Power on this virtual machine**.
   If VMware asks whether you moved or copied the VM, answer **I Copied It**.
2. Click into the VM's window. At `rollgate login:` type `root` (no password yet).
   To get the mouse back out of the window, press **Ctrl+Alt**.
3. Set a root password, for the console only (SSH uses your key):

   ```
   passwd
   ```

4. Apply the router config and keep it:

   ```
   rollgate apply
   rollgate confirm
   ```

   At the console, `rollgate confirm` is always allowed. Over SSH it must come from a new connection, so a change that cut you off can't confirm itself (ADR 0010).

## 5. Connect from your PC

In PowerShell:

```
ssh -i $HOME\.ssh\rollgate_ed25519 root@192.168.50.2
```

Then, on the router:

```
rollgate health        every check should say "ok"
rollgate status        the config generation, the release, and the last update
```

## Things to try

- **Lock yourself out on purpose:** in `/etc/rollgate/router.toml`, change lan's `ipv4` to `"192.168.50.3/24"` and run `rollgate apply` over SSH. Your session stops working, because the router left 192.168.50.2. Don't confirm: 2 minutes later the old config comes back by itself, and `ssh root@192.168.50.2` works again.
- **A LAN client:** give another VM a network adapter on VMnet2. It gets an address from the router (192.168.50.100–199) and reaches the internet through it.
- **Updates:** `rollgate update` installs the newest release once there is one newer than the VM's, with a trial boot and automatic rollback (ADR 0011).
- **The recovery runbook:** [runbooks/recovery.md](runbooks/recovery.md) works the same in the VM. The GRUB menu shows for 5 seconds at boot.
- **Snapshots:** VMware's own snapshots (**VM → Snapshot**) are a quick way back if an experiment goes wrong.

## Notes

- **Tested:**
  - **In VMware Workstation 17.6.3 on Windows 11, by hand (2026-11-19).** The 2026-11-16 release booted with no failed units, `rollgate apply` and `confirm` worked at the console, and every health check passed. WAN got its address from VMware's NAT and reached the internet, and SSH from the PC to 192.168.50.2 worked.
  - **In CI, on every change (`make test-vmware`).** QEMU emulates the VM's NVMe disk and vmxnet3 adapters; the test applies this guide's `router.toml`, confirms over SSH and runs the health checks.
  - The initramfs also carries drivers for VMware's other disk controllers (SATA, LSI Logic, LSI SAS, paravirtual SCSI).
- **Watchdog:** VMware's virtual hardware has no watchdog Linux can use, so rollgate falls back to its software watchdog. The boot report (`journalctl -b -u rollgate-boot-report`) shows `watchdog=Software_Watchdog:active`.
- **Clock:** the router doesn't sync its clock yet *(planned, M5)*. In VMware it starts from your PC's clock, so it's close enough for now.
- **Disk size:** the virtual disk is 32 GB. The `.vmdk` file only stores what's used, about 1.3 GB at first. On first boot the router grows its root filesystem to fill the disk.
- **Secure Boot** must stay off. rollgate's boot loader isn't signed for it.
- **Two such VMs** have the same MAC addresses. Don't run them on the same networks at the same time.
