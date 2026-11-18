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
| L3 | Apply (`make test-apply`, since M3) | `rollgate apply` on the booted image (`tests/vm/apply-test.sh`, configs in `tests/vm/apply/`): a missing key file is refused with nothing changed; `confirm` from the applying SSH connection is refused and works from a new one; the applied config runs (renamed ports, VLAN, firewall, dnsmasq, Unbound) and survives a reboot; a config that refuses SSH from the admin's zone reverts by itself when not confirmed, and again after a power cut during the confirm window | every PR touching `image/` or `compiler/`; every release | ~3 min |
| L3 | IPv6 (`make test-ipv6`, since M3) | `ipv6 = "auto"` on the booted image (`tests/vm/ipv6-test.sh`), with a fake ISP and a client in network namespaces bridged to the VM. First the ISP offers no IPv6: the client gets a DHCP lease and local names, reaches the internet over IPv4 (NAT) and gets local IPv6 (ULA) only, with no failed units. Then the ISP turns IPv6 on (router advertisements, and prefix delegation from `tests/vm/pd-server.go`) with `router.toml` unchanged: the client gets a public address from the delegated prefix, reaches the IPv6 internet from that address (no NAT), and the internet can't connect back in | every PR touching `image/` or `compiler/`; every release | ~2 min |
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

`make test-netns` first runs `TestPrecheckReal` (`compiler/apply`): the checks `rollgate apply` will run before installing anything (`nft -c`, `dnsmasq --test`, `unbound-checkconf`, `sshd -t`), with the real programs, on the compiled example. It also breaks each file and confirms each checker rejects it.

Then it runs `tests/netns/run.sh`. Topology built with `ip netns`:

```
[rg-wan] ──veth── [rg-router] ──veth── [rg-lan]
"internet"             └──────veth── [rg-iot]
```

`tests/netns/router.toml` is compiled, the generated nftables ruleset and sysctl settings are loaded into `rg-router`, and dnsmasq and sshd run there with their generated configs (after `dnsmasq --test`, `unbound-checkconf` and `sshd -T` accept the generated files). SSH checks log in with a throwaway key; only its file location is overridden.
Interface addresses are set by the script, standing in for networkd; the router's IPv4 and IPv6 (ULA) addresses are read from the compiled `.network` files. Router advertisements and prefix delegation need networkd itself running, so the L3 IPv6 test (`make test-ipv6`) covers them.
Each check is a real TCP connection (bash's `/dev/tcp` as the client, `tests/netns/listen.go` as the server) with an expected outcome, open or blocked, over IPv4 and IPv6.
The NAT check reads which address connections arrived from. DHCP and DNS checks use `tests/netns/probe.go`, which gets a real lease (discover, offer, request, ack) and looks up local names.
No mocks: the kernel's own packet filter is under test.

Before trusting a new check, make it fail: for example, replace the `nft -f` line with `nft flush ruleset` and confirm the blocked checks report `WRONG`.
Debug with `ip netns exec rg-router nft monitor trace` after adding `meta nftrace set 1` to a rule.

## VM tests (L3/L4)

- QEMU with user-mode or `vde`/bridge networking. One router VM plus lightweight client namespaces or VMs.
- A fake ISP serves DHCP on the WAN side and hosts an HTTP endpoint and a DNS root stub that act as "the internet".
- The test harness talks to the router over a serial console, so tests still work when networking is broken. Those are exactly the cases that matter.

## Writing tests as an agent

- Write the test first, watch it fail for the right reason, then implement.
- Never weaken or delete an assertion to make a test pass. If you think a test is wrong, explain why in the PR.
- Report the exact commands you ran and their results in the PR description.
