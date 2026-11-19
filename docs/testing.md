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
| L3 | Boot (`make test-vm`, since M1) | The image boots under UEFI with `linux` and with `linux-lts`, with no failed units (`tests/vm/boot-test.sh`). Each boot reports the watchdog systemd armed: the board's own (QEMU q35's iTCO), and `softdog` on a machine without one (QEMU pc). One more boot, from a 12 GB disk, must leave the root partition and Btrfs filesystem filling it | every PR touching `image/`; every release | ~3 min |
| L3 | Apply (`make test-apply`, since M3) | `rollgate apply` on the booted image (`tests/vm/apply-test.sh`, configs in `tests/vm/apply/`): a missing key file is refused with nothing changed; `confirm` from the applying SSH connection is refused and works from a new one; the applied config runs (renamed ports, VLAN, firewall, dnsmasq, Unbound) and survives a reboot; a config that refuses SSH from the admin's zone reverts by itself when not confirmed, and again after a power cut during the confirm window; a crashed kernel is reset by the watchdog, and the router comes back with its confirmed config | every PR touching `image/` or `compiler/`; every release | ~4 min |
| L3 | IPv6 (`make test-ipv6`, since M3) | `ipv6 = "auto"` on the booted image (`tests/vm/ipv6-test.sh`), with a fake ISP and a client in network namespaces bridged to the VM. First the ISP offers no IPv6: the client gets a DHCP lease and local names, reaches the internet over IPv4 (NAT) and gets local IPv6 (ULA) only, with no failed units. Then the ISP turns IPv6 on (router advertisements, and prefix delegation from `tests/vm/pd-server.go`) with `router.toml` unchanged: the client gets a public address from the delegated prefix, reaches the IPv6 internet from that address (no NAT), and the internet can't connect back in | every PR touching `image/` or `compiler/`; every release | ~2 min |
| L3 | Update trial (`make test-trial`, since M4) | `rollgate update` on the booted image (`tests/vm/trial-test.sh`), with a `releases.json` signed by a throwaway key that only the test VM trusts, running the real `pacman -Syu` in `@next` against the Arch Linux Archive. With nothing newer it changes nothing. A trial that fails a (test-only) check boots from `@next` with `panic=10`, GRUB has cleared its one-shot choice, a second update is refused, and when its checks fail it reboots by itself into the original `@`, which deletes `@next` and records why. A trial that passes becomes `@` without a reboot, the original becomes `@previous`, and the next boot is the updated system. Then `rollgate rollback` and a reboot bring the original system back, with the updated one kept as `@previous`. Finally the "rollgate previous system" boot menu entry, chosen once, boots `@previous` with every unit running, and `rollgate rollback` there makes it the system for good. The test shortens the 5-minute health wait to 90 s | every PR touching `image/` or `compiler/`; every release | ~10 min, needs internet |
| L3 | VMware (`make test-vmware`, since M5) | `image/vmware.sh` makes a VM (`tests/vm/vmware-test.sh`): its `.vmx` is UEFI without Secure Boot, with the disk on NVMe, wan on NAT and lan on VMnet2, and `image/vmware/router.toml` names its adapters' MAC addresses. QEMU boots the `.vmdk` on NVMe with two vmxnet3 adapters and the LAN laid out like VMnet2; `rollgate apply` with the installed config, confirmed over SSH to 192.168.50.2 from a new connection, passes every health check. VMware itself isn't run | every PR touching `image/` or `compiler/`; every release | ~1 min |
| L3 | Rollback rehearsal (`make test-rollback`, since M1) | Follows the recovery runbook without a human: a simulated bad update (pacman removes `linux-lts`) takes a pre snapshot; the test picks that snapshot and its `linux-lts` kernel from the GRUB snapshot menu with keystrokes, runs `rollgate-snapshot-rollback`, and checks the restored system and kernel boot cleanly | every PR touching `image/` or the rollback script; every release | ~2 min |
| L4 | Update + rollback (`make image-previous`, `make test-update`, since M4) | `tests/vm/update-test.sh` installs the previous release's image, applies a config (renamed ports, VLAN, firewall, dnsmasq, Unbound) and updates it from a site made by `release/publish.sh` with a throwaway key that only the test VM trusts: `releases.json` and each release's signed `[rollgate]` package repository. A trial whose health check fails rolls back by itself, and so does one whose kernel panics; each time the config still runs and the health checks pass. Then `rollgate update` installs the candidate: `pacman -Syu` in `@next` downloads the week's changes, the trial passes and becomes the system, and after a reboot the candidate runs its new kernel and the candidate's rollgate package (from the `[rollgate]` repository) with the same config, and its `linux-lts` entry boots too. Until the release train publishes releases (M4 step 7), the "previous release" is an image built a week before `image/snapshot-date`, with this tree's `rollgate` | every PR touching `image/` or `compiler/` (its own CI job); every release candidate (weekly bump PR) | ~15 min, needs internet |

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
