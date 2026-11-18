# 0004. Btrfs snapshots for rollback; `linux-lts` fallback kernel

- Status: Accepted, amended by 0009 (GRUB's own files live on the ESP, because GRUB can't write `grubenv` on Btrfs)
- Date: 2026-11-17

## Context

When a router fails, the house loses internet. That probably includes the ability to look things up or ask an AI for help.
Recovery must work offline, from the boot menu, without remembering commands.
Running the newest kernel also brings occasional regressions, especially in NIC drivers.

## Decision

- Root filesystem: Btrfs, managed with snapper. Subvolume layout is in `docs/architecture.md`.
- **`/boot` is inside the Btrfs root subvolume.** The ESP holds only the GRUB EFI binary.
  On Arch, kernels and initramfs live in `/boot`. If `/boot` were on a shared ESP, an update inside a new snapshot would overwrite the kernel that older snapshots need, and their kernel modules would no longer match. Keeping `/boot` in the snapshot makes each snapshot self-contained.
- Bootloader: GRUB (it can read Btrfs), with grub-btrfs listing snapshots in the menu.
- Kernels: `linux` is the default entry. `linux-lts` is always installed as a second entry.
- Updates (M4): install into a new snapshot, trial-boot it once via `grub-reboot`, and promote it to default only after health checks pass. A hardware watchdog (or `softdog`) covers hangs. Flow details are in `docs/architecture.md#update-flow-rollgate-update-m4`.
- Interim (M1): snap-pac pre/post snapshots around every pacman transaction, with rollback chosen by hand from the grub-btrfs menu.

## Alternatives considered

- **systemd-boot + UKIs + boot counting**: elegant automatic boot assessment, but it can't boot Btrfs snapshots directly and expects kernels on the ESP.
- **Limine + snapper integration**: simpler configuration, used by some Arch-based distros. Less documentation and less agent familiarity than GRUB today. Worth revisiting.
- **A/B root partitions**: robust, but only two slots and full-image updates (see ADR 0002).

## Consequences

- GRUB configuration is safety-critical and needs L4 tests and human review.
- Snapshots use disk space. The snapper cleanup policy must keep at least the current, previous, and pre-update snapshots.
- Logs (`@var_log`) and rollgate state (`@rollgate_state`) are separate subvolumes, so they survive rollbacks.
