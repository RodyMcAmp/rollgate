# 0001. Arch Linux as the base OS

- Status: Accepted
- Date: 2026-11-17

## Context

The project wants to run the latest stable Linux kernel and recent stable versions of every component, upgrading in small, frequent steps rather than in large jumps every few years.
Fixed-release distros hold versions back by design.
The maintainers prefer pacman and the PKGBUILD format over RPM.

## Decision

Build on plain Arch Linux (not a derivative), headless, with a minimal package set.

## Alternatives considered

- **Debian stable**: very solid and familiar, but "always the latest kernel and packages" works against how it is designed. Backports help, but only partly.
- **openSUSE Tumbleweed / MicroOS**: rolling, with openQA-tested snapshots and built-in transactional updates and automatic rollback, which is technically a very strong fit. Rejected because the maintainers prefer pacman over RPM and want Arch's somewhat fresher packages. We copy its ideas instead: snapshot-gated releases (ADR 0002) and transactional updates with health checks (ADR 0004).
- **Omarchy**: an Arch-based *desktop* setup (Hyprland, desktop apps). Wrong job for a headless appliance.
- **Koozali SME Server**: proven all-in-one server whose settings-database-plus-templates design inspired ADR 0005. Rejected as a base: Rocky Linux 8 (end of life in May 2029), no in-place major upgrades, Perl codebase, iptables-era firewall.
- **pfSense / OPNsense (FreeBSD)**: mature, but not Linux and not rolling.

## Consequences

- Arch has no automated release testing, so **we** are the test gate. That is why ADR 0002 exists.
- Arch does not support partial upgrades. Updates move the whole system to a new pinned date.
- The Arch wiki becomes our main reference. Agents should cite it when relevant.
