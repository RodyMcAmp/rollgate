# 0003. Stable repos only; no testing repos, no AUR

- Status: Accepted
- Date: 2026-11-17

## Context

The maintainers run beta software on their own machines, but the router should only combine stable components that are known to work together.
The AUR is user-submitted build scripts with no review. Installing from it on a firewall is a supply-chain risk.

## Decision

- The router uses only Arch `core`, `extra`, and our own signed `[rollgate]` repo.
- `core-testing`, `extra-testing`, and other testing or staging repos are never enabled, not even temporarily.
- No AUR helpers are installed on the router. Nothing is built from the AUR on the router.
- If we need software that isn't in `core`/`extra`, an ADR approves it first. Then we keep a PKGBUILD in `packaging/`, review it like code, and CI builds and signs it.
- Pre-release upstream versions (release candidates, betas) are not packaged, even in our own repo.

## Consequences

- Some features wait until upstream ships a stable release and Arch packages it.
- Everything on the router can be traced to Arch's maintainers or to a reviewed PKGBUILD in this repo.
