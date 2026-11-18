# Roadmap

Each milestone ends with something you can run and tests that prove it works.

## M0 — Design ✅ (this repository)

Architecture, config spec draft, ADRs 0001–0008, the agent and contributor process.

## M1 — Bootable image 🚧 (builds, boots and rolls back locally; CI not run yet)

- `image/build.sh` (ADR 0009): Arch at a pinned date, Btrfs layout from ADR 0004, GRUB on the ESP, `linux` + `linux-lts`, snapper + snap-pac + grub-btrfs, sshd (keys only), placeholder networking and firewall.
- `make image`, `make test-vm` (boot test: both kernels reach a clean `running` state under UEFI).
- CI pipeline running L0, the image build and the L3 boot test.
- Interim rollback command `rollgate-snapshot-rollback`.
- Done when: anyone can build the image from a clean checkout and boot it in QEMU, and CI is green.
- Verified 2026-11-17 (WSL2, snapshot 2026-11-16): `make check`, `make image` and `make test-vm` pass.
  Both kernels (7.3.4, 6.18.63-lts) reach `running` with no failed units in about 5 s after the kernel starts.
- Rollback rehearsal verified the same day: `make test-rollback` passes (4 boots: fresh → pre-update snapshot with its own linux-lts → rollback → restored linux-lts).
  It found and fixed two bugs: an expired root password blocked SSH key logins, and snapshot boots came up "degraded" (systemd-remount-fs on the overlay).
- Still to do: first CI run (needs the repo on GitHub).

## M2 — Compiler v0

- `compiler/`: Go CLI with `validate`, `compile`, `diff`. JSON Schema export.
- Templates for networkd, nftables, dnsmasq, Unbound, sysctl, sshd.
- Golden tests for `examples/router.toml` plus edge cases. L2 namespace tests for firewall policy, NAT, and port forwards.
- `ipv6 = "auto"`: ULA prefix generation, prefix delegation handling, router advertisements (ADR 0008).
- Done when: the example config compiles, and L2 proves lan→wan works, iot→lan is blocked over both IPv4 and IPv6, the port forward works, and WAN can't reach SSH.

## M3 — Safe apply

- `apply`, `confirm`, `rollback-config`. Generations in `/var/lib/rollgate`, git history in `/etc/rollgate`.
- L3: apply a config that cuts off management, don't confirm, and verify access returns by itself.
- Done when: it is impossible to lock yourself out through `rollgate apply`.

## M4 — Updates and rollback

- Release train: weekly date-bump PRs, `releases.json`, signed `[rollgate]` repo.
- `rollgate update`: install into a new snapshot, trial boot, health checks, watchdog, automatic promotion or rollback.
- L4 test suite.
- Done when: CI proves that an update which fails its health checks rolls back without anyone touching the router.

## M5 — Real-hardware pilot

- Install on the maintainers' hardware. Run it as the home router for at least four weekly releases.
- Do every procedure in `docs/runbooks/recovery.md` for real at least once, and fix the runbook wherever reality differs.
- Hardware notes: NIC compatibility list, watchdog availability.

## M6 — Usability

- Web UI (Go + htmx, management zones only) that edits `router.toml` through the same apply path.
- WireGuard road-warrior VPN, DNS blocklists, a status dashboard that explains the IPv6 state in plain words.

## Later / ideas

- Multi-WAN failover, traffic shaping (CAKE), a Proxmox VM image, our own archive mirror.
- Opt-in IPv6 tunnel (6in4 tunnel broker) for users whose ISP has no IPv6. Needs a public IPv4 address; won't work behind carrier-grade NAT.
- IPv6 inbound pinholes (the IPv6 equivalent of port forwards).
