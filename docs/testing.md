# Testing

Tests are how a project maintained partly by AI agents stays trustworthy.
An agent's claim that something works is not evidence. A passing test in CI is.

## Levels

| Level | Name | What it proves | Runs | Speed |
|---|---|---|---|---|
| L0 | Static | Formatting, lint, `router.toml` examples validate against the schema, docs links resolve | every commit, locally and in CI | seconds |
| L1 | Unit + golden | Validator catches bad configs. The compiler turns each input in `tests/golden/<case>/router.toml` into exactly the files in `tests/golden/<case>/expected/` | every commit | seconds |
| L2 | Namespace | Generated nftables and networkd configs load in Linux network namespaces, and real packets behave as intended (allowed, dropped, NATed, forwarded) | every PR, CI (root) | < 1 min |
| L3 | VM end-to-end | The built image boots in QEMU with a simulated ISP and LAN clients. Clients get DHCP leases, resolve names, reach the "internet", and are blocked where policy says. The web UI and SSH are unreachable from WAN. IPv6 runs twice: once with a fake ISP that delegates a prefix (clients get public and ULA addresses and reach the IPv6 "internet"), once with an ISP that offers no IPv6 (clients get ULA only, IPv4 internet still works, health checks pass). | every PR touching `image/`, `compiler/` or `packaging/`; every release | minutes |
| L3 | Rollback rehearsal (`make test-rollback`, since M1) | Follows the recovery runbook without a human: a simulated bad update (pacman removes `linux-lts`) takes a pre snapshot; the test picks that snapshot and its `linux-lts` kernel from the GRUB snapshot menu with keystrokes, runs `rollgate-snapshot-rollback`, and checks the restored system and kernel boot cleanly | every PR touching `image/` or the rollback script; every release | ~2 min |
| L4 | Update + rollback | Install the previous release, apply a config, update to the candidate, and verify health. Then inject a failing health check and verify automatic rollback to the previous snapshot with networking intact. Also verify the `linux-lts` entry boots. | every release candidate (weekly bump PR) | ~10–20 min |

## Required levels by change area

| Change touches | Minimum levels |
|---|---|
| Docs only | L0 |
| Validator or schema | L0, L1 |
| Any template or compiler output | L0, L1, L2 |
| nftables generation, apply/confirm/revert | L0–L3, plus human review |
| Image, packages, boot, GRUB, updater, health checks | L0–L4, plus human review |
| Snapshot date bump | L0–L4 |

## Golden tests

- Each case is a directory: `router.toml` plus the `expected/` file tree. The `example` case must be an exact copy of `examples/router.toml`; the test enforces it.
- `make golden-update` regenerates the expected files. The diff must be reviewed in the PR; never regenerate golden files just to make a failing test pass.
- Every bug fixed in the compiler gets a new golden case that reproduces it.

## Namespace tests (L2)

Topology built with `ip netns`:

```
[wan-ns] ──veth── [router-ns] ──veth── [lan-ns]
                       └──────veth── [iot-ns]
```

The generated nftables ruleset and addresses are loaded into `router-ns`.
Tests send traffic with standard tools (`ping`, `nc`, `curl`, `nft monitor trace` for debugging) and check the outcomes.
No mocks: the kernel's own packet filter is under test.

## VM tests (L3/L4)

- QEMU with user-mode or `vde`/bridge networking. One router VM plus lightweight client namespaces or VMs.
- A fake ISP serves DHCP on the WAN side and hosts an HTTP endpoint and a DNS root stub that act as "the internet".
- The test harness talks to the router over a serial console, so tests still work when networking is broken. Those are exactly the cases that matter.

## Writing tests as an agent

- Write the test first, watch it fail for the right reason, then implement.
- Never weaken or delete an assertion to make a test pass. If you think a test is wrong, explain why in the PR.
- Report the exact commands you ran and their results in the PR description.
