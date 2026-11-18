# 0002. Releases are pinned Arch Linux Archive dates

- Status: Accepted
- Date: 2026-11-17

## Context

On a normal Arch install, `pacman -Syu` installs whatever the mirrors have at that moment.
Two routers updated an hour apart can end up with different package sets, and no one has tested the exact combination either one received.
The project wants only tested, compatible combinations on routers, and wants every contributor and agent to reproduce exactly what users run.

## Decision

A release is a date. Routers install packages from `https://archive.archlinux.org/repos/YYYY/MM/DD/`.
A date becomes a release only after CI passes L0–L4 against it, including updating from the previous release and rolling back.
Bump the date about weekly, plus out-of-cycle bumps for urgent security fixes. Those go through the same gate.

## Alternatives considered

- **Live mirrors, with users updating whenever they like**: fresher by hours or days, but untested and impossible to reproduce.
- **Building full immutable images (A/B partitions, systemd-sysupdate)**: very robust, but every update means downloading the whole image, and it moves away from pacman, which the maintainers want to keep.
- **Our own full package mirror from day one**: more infrastructure than we need at a few devices. Revisit if the archive's usage expectations or our scale call for it.

## Consequences

- The newest kernel reaches routers within about a week of reaching Arch, not within hours. That's acceptable.
- `image/snapshot-date` is the single source of truth. The image build and the updater both read it.
- Rolling back to a release means returning to a known date, which is simple to reason about.
- We depend on the Arch Linux Archive staying available. The mitigation is mirroring released dates ourselves.
