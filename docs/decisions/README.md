# Architecture Decision Records

An ADR records one decision: the context, what was decided, and what follows from it.
They exist so that new contributors, especially AI agents arriving with no memory of past discussions, understand *why* things are the way they are and don't re-propose settled alternatives.

## Rules

- One decision per file: `NNNN-short-title.md`, numbered in order.
- Status is one of `Proposed`, `Accepted`, `Superseded by NNNN`, `Rejected`.
- Accepted ADRs are not edited except to change their status. To change a decision, write a new ADR that supersedes the old one.
- Any change to the base OS, a new daemon, a new language, or a new package source needs an ADR.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-arch-linux-base.md) | Arch Linux as the base OS | Accepted |
| [0002](0002-pinned-archive-snapshots.md) | Releases are pinned Arch Linux Archive dates | Accepted |
| [0003](0003-stable-only-no-aur.md) | Stable repos only; no testing repos, no AUR | Accepted |
| [0004](0004-btrfs-snapshots-and-kernel-fallback.md) | Btrfs snapshots for rollback; `linux-lts` fallback kernel | Accepted, amended by 0009 |
| [0005](0005-config-compiler-in-go.md) | One declarative config compiled by a Go tool | Accepted |
| [0006](0006-networking-stack.md) | networkd + nftables + dnsmasq + Unbound | Accepted |
| [0007](0007-docs-for-humans-and-agents.md) | Documentation methodology for humans and AI agents | Accepted |
| [0008](0008-ipv6-auto.md) | IPv6 is one setting: `auto` or `off` | Accepted |
| [0009](0009-image-build-with-pacstrap.md) | Build the image with pacstrap scripts; GRUB's files on the ESP | Accepted |
| [0010](0010-safe-apply.md) | Safe apply: generations, commit-confirm, revert at boot | Accepted |
| [0011](0011-update-trial-boot.md) | Updates: a next root, one trial boot, promotion by rename | Proposed |
| [0012](0012-release-publishing-and-signing.md) | Publishing releases: GitHub Pages, one signing key, pacman's own checks | Proposed |

## Template

```markdown
# NNNN. Title

- Status: Proposed
- Date: YYYY-MM-DD

## Context
What problem or force makes a decision necessary.

## Decision
What we will do.

## Alternatives considered
What else was on the table and why it lost.

## Consequences
What becomes easier, what becomes harder, and what we must now do.
```
