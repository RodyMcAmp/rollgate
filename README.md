# rollgate

A router and firewall OS for home labs and small offices, built on Arch Linux.
It always runs a recent stable kernel and recent stable packages, and any update can be rolled back from the boot menu.

> **Status: M1 done.** The image builds, boots both kernels and passes the rollback rehearsal in CI. It isn't a router yet; that starts with M2. See [docs/roadmap.md](docs/roadmap.md).

[![ci](https://github.com/RodyMcAmp/rollgate/actions/workflows/ci.yml/badge.svg)](https://github.com/RodyMcAmp/rollgate/actions/workflows/ci.yml)
> To build it, see [docs/dev-setup.md](docs/dev-setup.md). "rollgate" is a working name.

## The idea

- **Rolling, but only stable and only tested.** Packages come from Arch's stable repos, pinned to a dated snapshot.
  A new snapshot date becomes a release only after it passes the full test suite, including an update-and-rollback test.
- **One config file.** You describe the router in `router.toml`: interfaces, VLANs, DHCP, DNS, firewall zones, port forwards.
  A compiler turns that into native systemd-networkd, nftables, dnsmasq and Unbound configs.
- **Hard to brick.** Config changes revert automatically unless you confirm them.
  Updates install into a new Btrfs snapshot and boot once as a trial; if health checks fail, the router goes back to the previous snapshot.
  The previous kernel (`linux-lts`) is always in the boot menu.
- **Built to be maintained by people and AI agents together.** Decisions are recorded as ADRs, the rules agents must follow are in [AGENTS.md](AGENTS.md), and every behavior is backed by tests that run in CI.

## Read next

| If you want to… | Read |
|---|---|
| Understand the system | [docs/architecture.md](docs/architecture.md) |
| Write a config | [docs/config-spec.md](docs/config-spec.md), [examples/router.toml](examples/router.toml) |
| Understand releases and updates | [docs/build-and-release.md](docs/build-and-release.md) |
| Contribute (human or AI) | [CONTRIBUTING.md](CONTRIBUTING.md), [AGENTS.md](AGENTS.md) |
| Know why something is the way it is | [docs/decisions/](docs/decisions/) |
| Recover a broken router | [docs/runbooks/recovery.md](docs/runbooks/recovery.md) — print it |

## License

[Apache License 2.0](LICENSE). Contributions are accepted under the same license.
