# 0009. Build the image with pacstrap scripts; GRUB's files on the ESP

- Status: Accepted
- Date: 2026-11-17
- Amends: 0004 (where GRUB's own files live)

## Context

M1 needs a reproducible disk image: Arch at a pinned date (ADR 0002), the Btrfs layout and GRUB setup from ADR 0004, buildable on a developer's machine and in CI.
The design docs originally assumed mkosi. While implementing, three problems came up:

1. mkosi's boot integration is built around kernels on the ESP and unified kernel images.
   ADR 0004 needs the opposite: kernels in `/boot` inside the Btrfs snapshot.
2. GRUB cannot write its environment block (`grubenv`) on Btrfs.
   The M4 trial boot depends on `grub-reboot`, which writes `next_entry` to `grubenv`.
   If GRUB's directory sits on Btrfs, trial boots silently can't work.
3. CI runs in a container where partitioned loop devices (`/dev/loop0p1`) often don't appear, because udev isn't running.

## Decision

- **The image is built by `image/build.sh` using the standard Arch tools**: `pacstrap`, `arch-chroot`, `mkinitcpio`, `grub-install` and `grub-mkconfig`.
  These are the same steps as the Arch installation guide, which every contributor and agent can follow and look up.
- **GRUB's own files (modules, `grub.cfg`, `grubenv`) live on the ESP at `/efi/grub`.** GRUB is installed with `--boot-directory=/efi`.
  Kernels, initramfs and modules stay in `/boot` inside the Btrfs root, so each snapshot is still self-contained (ADR 0004's core rule is unchanged).
  `grub.cfg` stays valid across rollbacks because Arch kernel file names don't contain versions (`/boot/vmlinuz-linux`).
- **The ESP and the root filesystem are built as separate filesystem image files** on plain loop devices, then copied into a GPT disk image at fixed offsets with `sgdisk` and `dd`.
  No partitioned loop devices are needed.
- GRUB is installed with `--removable` (`EFI/BOOT/BOOTX64.EFI`), so the image boots on any UEFI machine without NVRAM entries.
  After `grub-install`, the EFI binary is rebuilt with `grub-mkimage` and an embedded early config that finds the ESP **by filesystem UUID**.
  Because the ESP was an unpartitioned image at build time, `grub-install` embedded the location `()/grub`, and the first test boot stopped at `grub rescue>`.
- Overlay files from `image/rootfs/` are installed with explicit modes (0644, or 0755 for scripts), never the checkout's: a WSL checkout of a Windows folder makes everything 0777.
  The build fails if any world-writable path ends up in the image.
- The initramfs is built without `autodetect`, because the build machine is not the target machine.
- M1 placeholders, all replaced by compiler output in M2: DHCP on every wired port, an nftables ruleset that drops everything inbound except SSH, and systemd-resolved for DNS.
- Console access: root has no password until the owner sets one, as on OpenWrt; `/etc/issue` reminds them at the login prompt. SSH accepts keys only (`image/authorized_keys`, never committed), so the empty password can't be used remotely.
  The password is deliberately not marked expired: with an expired password, sshd refuses key logins too ("Password change required but no TTY available"). The rollback rehearsal caught this.
- `systemd-remount-fs` is skipped when `/` is an overlay, i.e. when a snapshot is booted from the GRUB menu. Otherwise it fails applying fstab's Btrfs options and the recovery boot shows as "degraded".

## Alternatives considered

- **mkosi**: declarative and well maintained, but working around its boot model would cost more than the ~200-line script, and it's less familiar to contributors than the install guide.
- **archiso**: built for live ISOs, not installed systems.
- **Keeping `/boot/grub` on Btrfs and storing the env elsewhere with custom GRUB script code**: works, but it's custom and easy to break. Putting GRUB's directory on the ESP is the standard layout.

## Consequences

- `image/build.sh` is safety-critical (it defines how the system boots), so changes need L3 and human review.
- The ESP offset (sector 2048) is shared between `image/build.sh` and `tests/vm/boot-test.sh`. Change both together.
- The image is a fixed size (8 GiB by default). Growing the root partition to fill a real disk belongs to the installer work in M5.
