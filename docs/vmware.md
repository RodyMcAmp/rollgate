# rollgate in VMware Workstation

Run rollgate as a virtual machine on your PC to try it, or to rehearse updates and the recovery runbook before using real hardware (M5).
You need VMware Workstation 16.2 or newer and the WSL2 Arch setup from [dev-setup.md](dev-setup.md).

What you get:

- A VM with two network ports, like a small router:
  - **wan** on VMware's NAT network: the router's "internet", through your PC.
  - **lan** on a host-only network of its own, **VMnet2** (192.168.77.0/24). Your PC is a LAN client at 192.168.77.1; the router is 192.168.77.2.
    Your other VMs, on VMnet1 or VMnet8, aren't affected.
- Your SSH key installed, and a ready `router.toml` for those networks ([image/vmware/router.toml](../image/vmware/router.toml)).

## 1. Create the VMnet2 network (once)

1. Open VMware Workstation. Choose **Edit → Virtual Network Editor**, then **Change Settings** (it asks for administrator rights).
2. Click **Add Network…**, choose **VMnet2**, then **OK**.
3. With VMnet2 selected:
   - **Host-only** is selected.
   - **Connect a host virtual network adapter to this network** is ticked.
   - **Use local DHCP service** is **not** ticked. The router hands out addresses on its LAN, as a real one does.
   - **Subnet IP:** `192.168.77.0`, **Subnet mask:** `255.255.255.0`.
4. Click **OK**.

If VMnet2 is already used for something else, pick another free VMnet number and change `ethernet1.vnet` in the VM's `rollgate.vmx` to match.

VMnet2's range must differ from your home network's, or your PC sends the router's traffic to your home network instead and SSH times out ("Destination host unreachable").
192.168.77.0/24 is rarely used by home routers; check yours with `ipconfig` in PowerShell.
If your home network does use 192.168.77.x, pick another range here and change the addresses in `image/vmware/router.toml` to match before `make vmware`.

**Set up VMnet2 as 192.168.50.0/24 before?** That range collides with some home routers (ASUS routers use it out of the box), so it changed.
Change VMnet2's **Subnet IP** to `192.168.77.0` as in step 3, then make the VM again with `make vmware` (an older VM's `router.toml` still uses 192.168.50.2).

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

## 4. Start it and choose a password

1. In VMware: **File → Open…**, choose `rollgate.vmx`, then **Power on this virtual machine**.
   If VMware asks whether you moved or copied the VM, answer **I Copied It**.
2. Wait about a minute. On its first boot the router applies its `router.toml` by itself, and keeps it if its health checks pass (ADR 0020).
3. The VM's window asks you to **choose a password for this router**, at least 10 characters, typed twice (nothing shows as you type). Click into the window first; **Ctrl+Alt** gets the mouse back out.
   That one password is for logging in at the console (as `root`) and on the web UI. SSH doesn't accept it: SSH uses your key.
4. The window then shows the web UI's address, `https://192.168.77.2/`, and its certificate's fingerprint. The login prompt keeps showing both.

If the first boot couldn't use the settings (a typo in `router.toml`, say), the window says so before asking for the password, and the router runs without them. Log in at the console, fix `/etc/rollgate/router.toml`, then run `rollgate apply` and `rollgate confirm`.

*(This needs a release from after 2026-11-20. With an older one, log in as `root` with no password, run `passwd`, then `rollgate apply` and `rollgate confirm`, and `rollgate web-password` for the web UI.)*

## 5. Connect from your PC

Open `https://192.168.77.2/` in your browser and log in with the router's password.
The browser warns about the certificate once, because the router made it itself. Compare the fingerprint the browser shows (click the warning, then the certificate details) with the one the VM's window shows, then accept it.

For a shell, in PowerShell:

```
ssh -i $HOME\.ssh\rollgate_ed25519 root@192.168.77.2
```

(In Command Prompt, write `%USERPROFILE%` instead of `$HOME`.) Then, on the router:

```
rollgate health        every check should say "ok"
rollgate status        the config generation, the release, and the last update
rollgate password      change the router's password, for the console and the web UI
```

## Things to try

- **Lock yourself out on purpose:** in `/etc/rollgate/router.toml`, change lan's `ipv4` to `"192.168.77.3/24"` and run `rollgate apply` over SSH. Your session stops working, because the router left 192.168.77.2. Don't confirm: 2 minutes later the old config comes back by itself, and `ssh root@192.168.77.2` works again.
- **A LAN client:** give another VM a network adapter on VMnet2. It gets an address from the router (192.168.77.100–199) and reaches the internet through it.
- **Updates:** `rollgate update` installs the newest release once there is one newer than the VM's, with a trial boot and automatic rollback (ADR 0011).
- **The recovery runbook:** [runbooks/recovery.md](runbooks/recovery.md) works the same in the VM. The GRUB menu shows for 5 seconds at boot.
- **Snapshots:** VMware's own snapshots (**VM → Snapshot**) are a quick way back if an experiment goes wrong.

## Notes

- **Tested:**
  - **In VMware Workstation 17.6.3 on Windows 11, by hand (2026-11-19).** The 2026-11-16 release booted with no failed units, `rollgate apply` and `confirm` worked at the console, and every health check passed. WAN got its address from VMware's NAT and reached the internet, and SSH from the PC to 192.168.50.2 worked.
  - **In CI, on every change (`make test-vmware`).** QEMU emulates the VM's NVMe disk and vmxnet3 adapters; the test applies this guide's `router.toml`, confirms over SSH and runs the health checks.
  - The initramfs also carries drivers for VMware's other disk controllers (SATA, LSI Logic, LSI SAS, paravirtual SCSI).
- **Watchdog:** VMware's virtual hardware has no watchdog Linux can use, so rollgate falls back to its software watchdog. The boot report (`journalctl -b -u rollgate-boot-report`) shows `watchdog=Software_Watchdog:active`.
- **Clock:** the router keeps its clock right with systemd-timesyncd (ADR 0013); `rollgate status` shows whether it is synchronized.
- **Disk size:** the virtual disk is 32 GB. The `.vmdk` file only stores what's used, about 1.3 GB at first. On first boot the router grows its root filesystem to fill the disk.
- **Secure Boot** must stay off. rollgate's boot loader isn't signed for it.
- **Two such VMs** have the same MAC addresses. Don't run them on the same networks at the same time.
