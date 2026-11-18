# Architecture

## Layers

```
┌──────────────────────────────────────────────────────────────┐
│  Interfaces       rollgate CLI  ·  web UI (M6, LAN-only)      │  edit router.toml, call apply
├──────────────────────────────────────────────────────────────┤
│  Config           /etc/rollgate/router.toml  (git history)   │  the only thing users edit
├──────────────────────────────────────────────────────────────┤
│  Compiler         rollgate validate → compile → diff → apply │  Go, static binary
├──────────────────────────────────────────────────────────────┤
│  Generated files  networkd · nftables · dnsmasq · unbound ·  │  never hand-edited
│                   sysctl · sshd                              │
├──────────────────────────────────────────────────────────────┤
│  Arch Linux       linux + linux-lts · systemd · Btrfs +      │  stable repos only,
│                   snapper · GRUB                             │  pinned snapshot date
└──────────────────────────────────────────────────────────────┘
```

Only stock, well-tested programs handle traffic: the Linux kernel, nftables, dnsmasq, Unbound.
The project's own code configures them; it never sits in the packet path.
That keeps mistakes in our code to the config layer, where tests can catch them.

## Components

| Concern | Program | Generated file(s) | Decision |
|---|---|---|---|
| Interfaces, VLANs, bridges, WAN DHCPv4 client | systemd-networkd | `/etc/systemd/network/*.network`, `*.netdev` | ADR 0006 |
| All IPv6: prefix delegation, local ULA prefix, router advertisements | systemd-networkd | same files | ADR 0006, 0008 |
| Firewall, NAT, port forwards (IPv4 + IPv6 in one ruleset) | nftables | `/etc/nftables.conf` | ADR 0006 |
| LAN DHCPv4, local hostnames | dnsmasq | `/etc/dnsmasq.conf` (the main file: Arch's default doesn't read a drop-in directory) | ADR 0006 |
| Recursive DNS, DNSSEC, blocklists | Unbound | `/etc/unbound/unbound.conf` (the main file, for the same reason) | ADR 0006 |
| Kernel networking settings | sysctl | `/etc/sysctl.d/90-rollgate.conf` | — |
| Management access | OpenSSH | `/etc/ssh/sshd_config.d/10-rollgate.conf` (read before any other drop-in, so its settings win) | — |
| Config compiler and CLI | rollgate (Go) | — | ADR 0005 |
| Snapshots and rollback | Btrfs, snapper, GRUB, grub-btrfs | — | ADR 0004 |

dnsmasq answers LAN clients (DHCP and local names) and forwards everything else to Unbound on `127.0.0.1:5335`.

### IPv6 (`system.ipv6 = "auto"`, ADR 0008)

```
ISP ──(maybe) delegated prefix──▶ networkd on WAN
                                      │
           every LAN zone gets:       ▼
             • a local /64 from the router's ULA /48   (always)
             • a public /64 from the ISP's prefix      (only if the ISP delegates one)
           announced by router advertisements (SLAAC), DNS server = router's ULA address
```

Without ISP IPv6, devices use IPv6 locally and IPv4 for the internet. Their operating systems choose that on their own (RFC 6724), so nothing breaks.
When the ISP starts offering IPv6, public addresses appear without any config change.

## Config flow

```
router.toml ──validate──▶ typed config ──compile──▶ staging dir ──diff──▶ apply
                 │                           │                          │
          schema + semantic          text/template per          nft -c, networkctl,
          checks (overlapping        output file, header        commit-confirm timer
          subnets, unknown zones)    with source hash
```

1. **validate**: parse TOML, check it against the schema, then run semantic checks.
   Examples: subnets don't overlap, every zone a rule refers to exists, DHCP ranges sit inside their interface subnet, and management stays reachable from at least one LAN zone.
2. **compile**: a pure function from the typed config to a set of files in a staging directory.
   It has no side effects, so the golden tests can cover it completely.
3. **diff**: show what would change compared with the live generation (`rollgate diff`). Until M3 stores generations, it compares with the files installed on the system, and also lists rollgate's per-interface networkd files that the new config no longer produces.
4. **apply**: see [Safe apply](#safe-apply).

### Generations

Every apply *(planned, M3)* stores the compiled file set in `/var/lib/rollgate/generations/<N>/` along with the `router.toml` that produced it (ADR 0010).
These generations are the config history; there is no git repository.
Generation 0 is a copy of whatever was installed before the first apply, so even the first apply can be reverted.
`rollgate rollback-config [N]` *(planned)* puts an earlier generation's `router.toml` back and applies it.

## Safe apply

The commit-confirm flow *(planned, M3; ADR 0010)* guards against changes that cut off the person making them:

1. Validate, compile, and check the result with each program's own checker (`nft -c -f`, `dnsmasq --test`, `unbound-checkconf`, `sshd -t`), plus that every referenced secret file exists. Any failure stops the apply and changes nothing.
2. Store the new generation as pending and start a revert timer (`management.confirm_timeout_seconds`, default 120 s). The timer is a systemd timer, so it fires even if the SSH session that ran `apply` is gone.
3. Install the files and reload only the services whose files changed.
4. The user runs `rollgate confirm` over a **new** connection, which proves access still works. `confirm` refuses to run from the connection that ran `apply`.
5. If no one confirms in time, the previous generation is restored and the affected services are reloaded.
   If the router reboots during the window, `rollgate-boot.service` restores the previous generation before networking starts.

Changes that cannot affect reachability (a DNS blocklist, for example) still use this flow. One code path is easier to test than two.

## Updates and rollback

See [build-and-release.md](build-and-release.md) for the release train. On the device:

### Disk layout

- ESP (`/efi`, FAT32): GRUB only. That's the EFI binary plus `/efi/grub` (modules, `grub.cfg`, `grubenv`). GRUB can't write `grubenv` on Btrfs, and trial boots need it (ADR 0009). No kernels here.
- One Btrfs partition with these subvolumes:
  - `@` for `/`, including **`/boot`**. Each snapshot carries its own kernels, initramfs and modules.
  - `@snapshots` for `/.snapshots`.
  - `@var_log` for `/var/log`, kept out of root snapshots so logs survive a rollback.
  - `@rollgate_state` for `/var/lib/rollgate`. Config generations survive a rollback.

`/etc/rollgate` (the user config) sits inside `@`, so a system rollback also rolls back the config to match.
After a rollback, `rollgate` offers to re-apply the newest config if it validates against the older system.

### Update flow (`rollgate update`, M4)

Mechanics in ADR 0011 *(proposed)*; publishing and signing in ADR 0012 *(proposed)*.

1. Read the target release date: the newest `current` date in the signed `releases.json`, or one the user gives.
2. Create `@next`, a writable snapshot of the running root `@`.
3. Inside `@next` (`arch-chroot`): point pacman at the target date's archive and run `pacman -Syu`.
   This regenerates the initramfs for both `linux` and `linux-lts`.
4. Write a GRUB entry for `@next` and arm it once with `grub-reboot`. The default entry stays `@`.
5. Reboot into the trial. The hardware watchdog is enabled (`RuntimeWatchdogSec=` in systemd, falling back to `softdog`).
6. `rollgate-health.service` runs the health checks. If they pass within 5 minutes, it promotes the trial by renaming: `@` becomes `@previous`, `@next` becomes `@`.
   If they fail, it reboots. If the system hangs or panics, the watchdog or `panic=10` reboots it.
   GRUB consumed the one-shot choice, so every one of those reboots lands on the old `@`.

The running system is never modified during an update, so a failed update leaves nothing half-finished.
M1 ships a simpler interim mechanism: snap-pac pre/post snapshots, with rollback chosen by hand from grub-btrfs.

### Health checks

Each check is a small executable in `/usr/lib/rollgate/health.d/` that exits with status 0 to pass.
The initial set:

- The WAN interface has an address and a default route exists.
- The local resolver answers a known name, through both dnsmasq and Unbound.
- The nftables ruleset is loaded and contains the `rollgate` tables.
- dnsmasq is running and listening on every LAN interface.
- sshd is listening on the management addresses.
- No failed units among those rollgate manages.

Missing IPv6 from the ISP is never a health-check failure. It's a normal state (ADR 0008).

### Kernels

`linux` (latest stable) is the default boot entry. `linux-lts` is always installed as a second entry in every snapshot.
A regression in a new kernel, such as a NIC driver, is fixed by booting `linux-lts` without rolling back anything else.

## Security posture

- WAN inbound: drop by default (hard rule). ICMP is rate-limited. No services listen on WAN.
- Management (SSH, web UI): reachable only from zones listed in `management.allowed_zones`. SSH uses keys only.
- Our own packages are signed; pacman is configured to require valid signatures (the default).
- No AUR, no testing repos (ADR 0003).
- Secrets live in `/etc/rollgate/secrets/` (mode 0700) and are referenced by name from `router.toml`.
