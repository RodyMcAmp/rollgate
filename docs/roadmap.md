# Roadmap

Each milestone ends with something you can run and tests that prove it works.

## M0 — Design ✅ (this repository)

Architecture, config spec draft, ADRs 0001–0008, the agent and contributor process.

## M1 — Bootable image ✅ (2026-11-18)

- `image/build.sh` (ADR 0009): Arch at a pinned date, Btrfs layout from ADR 0004, GRUB on the ESP, `linux` + `linux-lts`, snapper + snap-pac + grub-btrfs, sshd (keys only), placeholder networking and firewall.
- `make image`, `make test-vm` (boot test: both kernels reach a clean `running` state under UEFI).
- CI pipeline running L0, the image build and the L3 boot test.
- Interim rollback command `rollgate-snapshot-rollback`.
- Done when: anyone can build the image from a clean checkout and boot it in QEMU, and CI is green.
- Verified 2026-11-17 (WSL2, snapshot 2026-11-16): `make check`, `make image` and `make test-vm` pass.
  Both kernels (7.3.4, 6.18.63-lts) reach `running` with no failed units in about 5 s after the kernel starts.
- Rollback rehearsal verified the same day: `make test-rollback` passes (4 boots: fresh → pre-update snapshot with its own linux-lts → rollback → restored linux-lts).
  It found and fixed two bugs: an expired root password blocked SSH key logins, and snapshot boots came up "degraded" (systemd-remount-fs on the overlay).
- CI green on GitHub 2026-11-18 (L0, image build, `make test-vm`, `make test-rollback`; build + tests about 5 minutes).
  The first run found two more issues, both fixed: a gpg-agent left by `pacstrap -K` kept the target busy in the container, and GRUB 2.16 copied the runner's firmware boot entries into the menu.

## M2 — Compiler v0 ✅ (2026-11-18)

- `compiler/`: Go CLI with `validate`, `compile`, `diff`. JSON Schema export.
- Templates for networkd, nftables, dnsmasq, Unbound, sysctl, sshd.
- Golden tests for `examples/router.toml` plus edge cases. L2 namespace tests for firewall policy, NAT, and port forwards.
- `ipv6 = "auto"`: ULA prefix generation, prefix delegation handling, router advertisements (ADR 0008).
- Done when: the example config compiles, and L2 proves lan→wan works, iot→lan is blocked over both IPv4 and IPv6, the port forward works, and WAN can't reach SSH.
- Delivered 2026-11-18 in PRs #2–#4, #7–#12; CI green (L0+L1, L2, L3).
  `rollgate validate`, `compile`, `diff` and `schema` work. Golden tests cover the example and two edge cases (IPv6 off with `match_device`, DNS over TLS forwarding).
  L2 runs the generated nftables ruleset and sysctl settings, dnsmasq and sshd in network namespaces and checks every "done when" item over IPv4 and IPv6, plus DHCP leases, local names and SSH key logins.
- Moved to M3: router advertisements and prefix delegation need networkd running, so they are tested in L3 once `apply` installs the compiled files on the image.

## M3 — Safe apply ✅ (2026-11-19)

- `apply`, `confirm`, `revert`, `status`, `rollback-config` (ADR 0010). Generations in `/var/lib/rollgate`.
- The image ships `rollgate`, dnsmasq and Unbound, and runs the compiled configs once a `router.toml` is applied.
- L3: apply a config that cuts off management, don't confirm, and verify access returns by itself. The same after a reboot during the confirm window. Router advertisements and prefix delegation against a fake ISP.
- Done when: it is impossible to lock yourself out through `rollgate apply`.
- Delivered in PRs #16–#22, merged 2026-11-19; CI green.
  `make test-apply` proves a config that locks out SSH reverts by itself: by the timer, after a reboot and after a power cut. It also proves a confirm counts only from a new connection.
  `make test-ipv6` proves `ipv6 = "auto"` against a fake ISP: local IPv6 without one, public IPv6 by prefix delegation with one, no IPv6 NAT, and no way in from the internet.

## M4 — Updates and rollback ✅ (2026-11-19)

- Release train: weekly date-bump PRs, `releases.json`, signed `[rollgate]` repo.
- `rollgate update`: install into a new snapshot, trial boot, health checks, watchdog, automatic promotion or rollback.
- L4 test suite.
- Done when: CI proves that an update which fails its health checks rolls back without anyone touching the router.
- Delivered in PRs #25–#34 (ADRs 0011 and 0012 in #23), merged 2026-11-19 with the release signing key (#36); CI green.
  `make test-update` (L4) installs the previous week's release, updates it to this one and proves the "done when": a trial that fails a health check, and one whose kernel panics, both roll back by themselves. A good update becomes the system, with the config and both kernels intact and `rollgate` upgraded from the signed `[rollgate]` repository.
  `make test-trial` covers promotion, `rollgate rollback` and the "rollgate previous system" boot entry. The hardware watchdog (softdog when there's none) resets a hung kernel.
- Open: an occasional missed WAN DHCP lease in CI VMs (#35). Since #37 the next occurrence logs networkd's view.

## M5 — Real-hardware pilot

- First in VMware Workstation ([vmware.md](vmware.md)): `make vmware` builds a ready VM. The 2026-11-16 release ran in VMware Workstation 17.6.3 on 2026-11-19 with every health check passing; CI tests the VM in QEMU emulating VMware's NVMe disk and vmxnet3 adapters.
- Install on the maintainers' hardware. Run it as the home router for at least four weekly releases.
- Minimum hardware *(planned, to be confirmed by the pilot)*: 64-bit Intel or AMD from Sandy Bridge (2011) on, UEFI boot with Secure Boot off, 2 GB RAM, a 32 GB disk, and 2 Ethernet ports (Intel preferred).
- Before real hardware: the root filesystem grows to fill the disk (docs/architecture.md#disk-growth), and systemd-timesyncd keeps the clock right, since a dead CMOS battery would otherwise break signature checks (ADR 0013).
- Do every procedure in `docs/runbooks/recovery.md` for real at least once, and fix the runbook wherever reality differs.
- Hardware notes: NIC compatibility list, watchdog availability.

## M6 — Usability

- Web UI (Go + htmx, management zones only) that edits `router.toml` through the same apply path (ADR 0014).
  Steps: 1. the root API (`rollgate api`); 2. `rollgate web-password` and the HTTPS certificate; 3. the web server with login and a status page; 4. editing `router.toml` with apply and confirm, and update and rollback buttons *(planned)*.
- WireGuard road-warrior VPN, DNS blocklists, a status dashboard that explains the IPv6 state in plain words.

## Later / ideas

- **Live router install media** (proposed 2026-11-18). The USB installer boots into a working router with "good enough" defaults, so you can try rollgate, or rescue a network, without touching the disk.
  "Install" then copies the system to disk and keeps the settings you made. OPNsense's installer works this way. Design notes:
  - The defaults are a generated `router.toml`, never hand-made system files (hard rule 3). A first-boot tool picks WAN as the port that gets a DHCP lease, makes the other ports LAN, and chooses a LAN subnet that doesn't clash with the WAN subnet (the ISP modem may already use `192.168.1.0/24`).
  - It runs from RAM with a writable overlay, the same mechanism as booting a snapshot from the GRUB menu (grub-btrfs overlayfs, ADR 0009).
  - Safe while live: WAN stays default-deny; management only from LAN; console login; no SSH until keys are added.
  - The same media provides the recovery runbook's planned "Rescue" entry.
  - **Keep it possible from M2 on:** the compiler and apply must work on a read-only root with a RAM overlay and must not assume state already exists (the ULA prefix is simply generated fresh on a live boot). Auto-detection writes `router.toml`; it never writes generated files.
- **Add-ons** (proposed 2026-11-19). Optional features installed on demand, so the base router stays small. Candidates:
  - ad blocking: Pi-hole's job, done natively with DNS blocklists (M6) instead of a second DNS server;
  - intrusion detection/prevention: Suricata is in Arch's repositories, Snort 3 isn't;
  - the UniFi Network controller (proprietary, Java and MongoDB: only practical in a container);
  - UPnP/NAT-PMP (miniupnpd), dynamic DNS, an mDNS reflector across VLANs, ntopng traffic statistics, CrowdSec, Tailscale.

  Design notes, to keep possible from now on:
  - An add-on is a package from the signed `[rollgate]` repository (hard rule 1), or a container image once an ADR allows a container runtime.
  - It's switched on and configured in `router.toml`, in a section of its own (`[addon.<name>]`). It never writes generated system files (rule 3): it declares what it needs (ports, DNS hooks, a firewall zone), and the compiler turns that into config.
  - It's part of the snapshot, so updates and rollback cover it. A failing add-on mustn't make a good core update roll back.
  - Its settings page plugs into the web UI (ADR 0014 keeps pages per feature).
  - Needs an ADR for the add-on model before the first add-on.
- **AI-assisted firewall management** (proposed 2026-11-19). An assistant that explains and proposes, while a person decides:
  - It answers "why can't my console reach the game server?" from the config, the firewall counters and the logs.
  - It proposes changes as a diff to `router.toml`.
  - Nothing reaches the firewall except through validate → apply → commit-confirm (rule 2), and WAN stays default-deny whatever it suggests (rule 5).
  - The model runs off the router (the hardware is too small), so sending logs out is opt-in.
  - The hook is the local API from ADR 0014, with read-only access by default.
  - Needs its own ADR.
- Multi-WAN failover, traffic shaping (CAKE), a Proxmox VM image, our own archive mirror.
- Opt-in IPv6 tunnel (6in4 tunnel broker) for users whose ISP has no IPv6. Needs a public IPv4 address; won't work behind carrier-grade NAT.
- IPv6 inbound pinholes (the IPv6 equivalent of port forwards).
