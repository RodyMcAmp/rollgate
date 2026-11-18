# 0011. Updates: a next root, one trial boot, promotion by rename

- Status: Proposed
- Date: 2026-11-18

## Context

ADR 0004 settles the shape of updates: install into a new snapshot, boot it once as a trial, promote it only after health checks pass, and let a watchdog cover hangs.
`docs/architecture.md#update-flow-rollgate-update-m4` sketches the steps.
Before writing `rollgate update` (M4), a few mechanics need one answer each:
how the new root is named and booted, how "only once" is enforced, how promotion and rollback work, what the trial must prove, and what a failed trial leaves behind.

The M1 rollback (`rollgate-snapshot-rollback`, tested by `make test-rollback`) already works by renaming Btrfs subvolumes: the running system is always the subvolume `@`, and rolling back means putting a different subvolume at that name.
The update should work the same way, so there is one model to understand and test.

## Decision

### Subvolumes

| Name | What it is |
|---|---|
| `@` | the system. The default GRUB entry and `fstab` always mean `@`. |
| `@next` | an update being prepared or on trial. Exists only between `rollgate update` and the end of the trial. |
| `@previous` | the system before the last promoted update. `rollgate rollback` goes back to it. One is kept. |

`/var/lib/rollgate` (`@rollgate_state`) and `/var/log` stay separate, so the update state and logs survive whichever root boots (ADR 0004).

### `rollgate update [DATE]`

1. **Choose the date**: the newest release marked `current` in `releases.json` (ADR 0012), or the date given. Withdrawn dates and dates older than the running one are refused; going back is `rollgate rollback`.
2. **Refuse if** a config change is waiting for confirmation (ADR 0010), a trial is already in progress, the system was booted from a snapshot or from `@previous`, or the disk has less than 2 GiB free.
3. **Prepare `@next`**: delete any leftover `@next`, then take a writable snapshot of `@` as `@next`.
4. **Update inside it**: with `@next` mounted and `arch-chroot`ed, point its mirrorlist at the new date and run `pacman -Syu` (snap-pac skipped: the whole root is the snapshot). This rebuilds both kernels' initramfs inside `@next/boot`. Then set `@next`'s `fstab` root line to `subvol=/@next`.
5. **Arm one trial boot**: write a GRUB menu entry for `@next` to `/efi/grub/custom.cfg`, which `grub.cfg` already includes. It has the same kernel options as the running system, with `rootflags=subvol=@next`. Then run `grub-reboot` with that entry.
   GRUB clears the one-shot choice from `grubenv` (on the ESP, ADR 0009) as it boots the entry. So the trial boot happens once, and **any later boot, including a watchdog reset, goes back to `@`**.
6. **Record the trial** in `/var/lib/rollgate/update.json` (date, time, the release being left) and reboot.

Nothing in `@` changes during these steps. An update that fails halfway leaves the running system exactly as it was.

### The trial

- `rollgate-health.service` runs on every boot after networking has started. It runs the health checks (below) every 10 seconds until all pass or 5 minutes have gone by.
- On a trial boot (root is `@next`):
  - **All checks pass**: promote (below).
  - **Any check still failing after 5 minutes**: record the failure and the checks' output in `update.json`, then reboot. GRUB boots `@` again.
- **Hangs** are covered by systemd's watchdog (`RuntimeWatchdogSec=30s`), using the board's hardware watchdog or `softdog` when there is none. A watchdog reset also lands on `@`.
- A trial that never reaches the health service (kernel panic, broken initramfs) reboots through the kernel's `panic=10` option, which is part of the trial entry only. That boot lands on `@` too.
- On the next normal boot of `@`, `rollgate` finds a trial in `update.json` that never promoted. It marks the update failed, deletes `@next` and removes the trial entry. `rollgate status` then shows which date failed and why.

### Promotion

Running from `@next`, once the checks pass:

1. Set `@next`'s `fstab` root line back to `subvol=/@`.
2. At the top of the Btrfs filesystem: delete the old `@previous`, rename `@` to `@previous`, rename `@next` to `@`. The running system keeps working, because a mounted subvolume is tied to its ID, not its name.
3. Point the GRUB entry in `custom.cfg` at `@previous` and label it "previous system (DATE)", for booting it by hand from the menu.
4. Record success in `update.json`.

From the next boot on, the default entry boots the updated system as `@`. No reboot is needed to finish.

### `rollgate rollback`

Renames `@` to `@next`, `@previous` to `@`, and `@next` to `@previous`, so the two swap places, then asks for a reboot. It replaces the interim `rollgate-snapshot-rollback`, which stays available for going back to an arbitrary snapper snapshot.

### Health checks

Each check is an executable in `/usr/lib/rollgate/health.d/` that exits 0 to pass and prints one line saying what it saw (architecture.md, "Health checks").
`rollgate health` runs them all and prints the results; it is also what `rollgate-health.service` runs.
A check that can't apply to this router (for example, a DHCP check on a router with no `[dhcp]` sections) passes and says it was skipped.
**Missing IPv6 from the ISP is never a failure** (ADR 0008).

### GRUB itself is not updated

Updates don't re-run `grub-install`, so the ESP keeps the GRUB that was installed. A broken GRUB couldn't be rolled back, because it lives outside the snapshots.
The GRUB package inside the root is still updated, and so are its tools, which only write files GRUB reads.
Updating the GRUB binary on the ESP is a separate, deliberate step *(planned)*, run only after an update has been promoted.

## Alternatives considered

- **Booting the trial through grub-btrfs's snapshot menu** (as the M1 rehearsal does): those entries boot a read-only snapshot with an overlay, which is right for recovery but not for a system that will become permanent.
- **Changing GRUB's default entry to the new root and changing it back on failure**: if the new root hangs before anything can change the default back, the router boots the broken root forever. The one-shot `grub-reboot` choice can't get stuck, because GRUB consumes it.
- **`btrfs subvolume set-default` instead of renames**: makes the default invisible in `fstab` and `grub.cfg`, and the M1 rollback already uses names. One model is easier to test.
- **A/B partitions**: twice the disk, and no snapper history. Btrfs snapshots give the same atomic switch without it (ADR 0004).
- **Promoting after a fixed delay instead of health checks**: a router that boots but has no internet would be promoted. The checks are the point.

## Consequences

- An update can't brick the router. It either passes its checks and becomes `@`, or the next boot is the old `@`, whether the trial failed its checks, hung or panicked. L4 must prove each path.
- One previous system is kept. Older states are still in snapper's history (`rollgate-snapshot-rollback`).
- GRUB on the ESP changes only by an explicit, separate step.
- `rollgate status` gains update information (the running release, any failed trial).
- The update, rollback and health-check code is safety-critical (AGENTS.md).
