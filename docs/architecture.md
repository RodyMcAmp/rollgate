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

### Changing router.toml from a form (`compiler/edit`, ADR 0016)

The web UI's forms (Port forwarding so far) change `router.toml` through `compiler/edit`, which edits the file's text the way a person would, so comments, order and hand edits stay.
- **Edits:** add, change or remove an entry in a list of sections (`[[firewall.port_forward]]`, `[[dhcp.<name>.static]]`, `[[vlan]]`), and set keys in a section (`[system]`). A change names its entry by position and an identifying value (a port forward's `name`), and is refused if the file changed meanwhile.
- **Only the lines that must change:** a changed key keeps the rest of its line, including its comment; a removed entry takes the comment lines directly above it; new sections go after their family (`[dhcp.guest]` after the other `[dhcp...]` sections) in the format `examples/router.toml` uses.
- **Checked by parsing again:** the result must mean the old config plus exactly the edits, and every other line must be unchanged; otherwise nothing is changed.
- **Refusals:** a section written in a way the writer doesn't edit (a value over several lines, an inline table, a dotted key, a quoted section name) is left alone, and the page points to the Advanced editor.
- The result then goes the normal way: validate, diff, apply, confirm.

### Generations

Every apply *(planned, M3)* stores the compiled file set in `/var/lib/rollgate/generations/<N>/` along with the `router.toml` that produced it (ADR 0010).
These generations are the config history; there is no git repository.
Generation 0 is a copy of whatever was installed before the first apply, so even the first apply can be reverted.
`rollgate status` lists the generations and says what is running; `rollgate rollback-config [N]` puts an earlier generation's `router.toml` back (keeping the replaced one as `router.toml.before-rollback`) and applies it with commit-confirm.

## Safe apply

The commit-confirm flow (ADR 0010; `rollgate apply`, `confirm` and `revert`) guards against changes that cut off the person making them:

1. Validate, compile, and check the result with each program's own checker (`nft -c -f`, `dnsmasq --test`, `unbound-checkconf`, `sshd -t`), plus that every referenced secret file exists. Any failure stops the apply and changes nothing.
2. Store the new generation as pending and start a revert timer (`management.confirm_timeout_seconds`, default 120 s). The timer is a systemd timer, so it fires even if the SSH session that ran `apply` is gone.
3. Install the files and reload only the services whose files changed.
4. The user runs `rollgate confirm` over a **new** connection, which proves access still works. `confirm` refuses to run from the connection that ran `apply`.
5. If no one confirms in time, the previous generation is restored and the affected services are reloaded.
   If the router reboots during the window, `rollgate-boot.service` runs `rollgate revert --boot` before nftables, networkd and the services start, and re-applies port renames if needed. It restores the previous generation's files, so everything then starts from them.

Changes that cannot affect reachability (a DNS blocklist, for example) still use this flow. One code path is easier to test than two.

### The local API (`rollgate api`, ADR 0014)

The web UI (M6) changes nothing itself. It asks `rollgate api`, which runs as root and calls the same code as the CLI:

- **Where:** HTTP on the Unix socket `/run/rollgate/api.sock`, opened by `rollgate-api.socket`. The socket is `root:rollgate-api`, mode 0660, so the kernel lets only root and the `rollgate-api` group (the web server) connect.
- **What it accepts:** a fixed list of requests (`compiler/api`). `GET /v1/status`, `/v1/health`, `/v1/config` and `/v1/pending` (the change waiting for confirmation and when it reverts) read. `POST /v1/validate`, `diff`, `apply`, `confirm`, `revert`, `update` and `rollback` change things, one at a time. There is no request that runs a command.
- **Update and rollback** reboot by themselves when they need to, since the browser has no shell to type `reboot` in. (`rollgate rollback` at the CLI still leaves the reboot to the user.)
- **Commit-confirm:** `apply` and `confirm` carry the client's connection (the browser's address and port). An apply from one connection can't be confirmed from the same one, as with SSH.
- **Applying:** `apply` takes the whole `router.toml`. A config that doesn't validate is refused before anything changes. Otherwise it becomes `/etc/rollgate/router.toml`, like an edit from the CLI, and is applied.
- **Logging:** every request is logged in the journal with the asking process (`journalctl -u rollgate-api`).
- **Off by default:** the socket isn't enabled. The web UI will enable it once a web password is set.

### Web password and certificate (`rollgate web-password`, ADR 0014)

`rollgate web-password` (as root, at the console or over SSH) sets the web UI's one admin password. It asks twice without echo; `--stdin` reads one line instead, for scripts.

- **The password** must be at least 10 characters. Only a hash is kept: salted PBKDF2-HMAC-SHA256, 600,000 iterations (Go's standard library), in `/etc/rollgate/secrets/web-password`, mode 0600.
- **The certificate:** the first time, it also makes the web UI's HTTPS certificate and key (`web.crt`, `web.key`, ECDSA P-256, self-signed, valid 10 years). The certificate covers the router's host name and the static addresses of its management zones from the running config.
- **The fingerprint:** it prints the certificate's SHA-256 fingerprint, and `rollgate status` shows it again, so the user can check that the browser's warning is about their own router. Changing the password later keeps the certificate, so the fingerprint stays the same.
- **Snapshots:** the secrets are part of `@`, so a system rollback brings back the password that system had.
- **Turning it on:** it enables `rollgate-api.socket` and `rollgate-web.socket`, which turns the web UI on, and restarts the web server if it runs, so every old session ends.

### The web server (`rollgate web`, ADR 0014)

- **Ports:** `rollgate-web.socket` opens 443 (HTTPS) and 80 (HTTP, which only redirects to HTTPS). systemd starts `rollgate-web.service` on the first visit.
- **Who can reach it:** like SSH, it listens on every address, and the firewall decides. The generated ruleset accepts 22, 80 and 443 only from `management.allowed_zones`, so WAN can never reach it (hard rule 5).
- **No privileges:** it runs as a throwaway user (`DynamicUser=`) with no capabilities, a read-only system and systemd's other sandbox settings. The certificate, key and password hash reach it as systemd credentials. The only extra right it has is membership of `rollgate-api`, to ask the API.
- **Logging in:** one password, checked against the stored hash. Five wrong passwords from one address block that address for 15 minutes. A session is a random token in a cookie (`Secure`, `HttpOnly`, `SameSite=Strict`) that ends after 30 idle minutes, at logout, or when the password changes.
- **Forms:** every form that changes something carries the session's CSRF token.
- **Pages** come from the binary itself (`embed`), with a Content Security Policy that allows nothing from outside the router. Every page works without JavaScript; the one script, the confirm page's countdown, is served by the router too.
- **The look:** the "Console" layout the maintainer chose: dark, a side menu (Overview, Port forwarding, Updates, and Advanced for the text editor), system fonts only.
- **Overview:** the health checks and `rollgate status` from the API. While a change waits to be kept, a bar across the top of every page counts down and offers **Keep** and **Undo**.
- **Port forwarding:** the `[[firewall.port_forward]]` entries as a table, and a form to add, change or remove one: a name, TCP/UDP/both, the port or range from the internet, and a fixed-address device (from `[[dhcp.<name>.static]]`) or an address. The form makes the change with `compiler/edit` (ADR 0016), so the rest of the file stays as it was, then shows the `router.toml` diff for review; **Apply** sends the reviewed text through the Advanced editor's apply, with all its checks.
- **Advanced:** `router.toml` in a text editor, so the user's comments stay. **Check** validates it and shows the changes to `router.toml` and to the system files; **Apply** is offered only after a check passes. An apply is refused, and the check shown again, if the text was edited after the check or `router.toml` changed on the router since (over SSH, or from another browser).
- **Commit-confirm in the browser (ADR 0010, ADR 0014):** after an apply, the confirm page counts down and offers **Keep these settings** and **Undo now**. The server speaks HTTP/1.1 only and opens a connection for every request (no keep-alive, no HTTP/2), so each click arrives over a new connection. On top of that, a confirm is refused unless its connection was opened after the apply finished, because an older connection can outlive a change that blocks new ones. The API also refuses a confirm from the apply's own connection. If the change cuts the browser off, nothing can confirm, and the revert timer brings the old settings back.
- **Updates:** buttons for `rollgate update` and `rollgate rollback`, with what each does spelled out first. Both may restart the router.

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

#### Disk growth

The image is 8 GB. Written to a bigger disk, the Btrfs partition grows to fill it at boot:

1. `systemd-repart.service` grows the root partition (`/etc/repart.d/50-rollgate-root.conf`) and moves the backup GPT to the end of the disk. The ESP isn't listed, so it stays as it is.
2. `rollgate-growfs.service` grows the mounted Btrfs filesystem to fill the partition (`btrfs filesystem resize max /`).

Both do nothing once the disk is full, and both skip a snapshot booted from the GRUB menu, whose root is a temporary overlay.
These are image files, so a router gets them from its install image, not from updates; growing matters only once, at the first boot.
`make test-vm` boots the image from a 12 GB disk and checks that the partition and filesystem fill it.

### Update flow (`rollgate update`, M4)

Mechanics in ADR 0011; publishing and signing in ADR 0012.

1. Read the target release date: the newest `current` date in the signed `releases.json`, or one the user gives.
2. Create `@next`, a writable snapshot of the running root `@`.
3. Inside `@next` (`arch-chroot`): point pacman at the target date's archive and run `pacman -Syu`.
   This regenerates the initramfs for both `linux` and `linux-lts`.
4. Write a GRUB entry for `@next` to `/efi/grub/custom.cfg` and arm it once (GRUB's `next_entry`, which is what `grub-reboot` sets). The default entry stays `@`.
5. Reboot into the trial. The hardware watchdog is enabled (`RuntimeWatchdogSec=` in systemd, falling back to `softdog`).
6. `rollgate-health.service` (`rollgate health -boot`) runs the health checks. If they pass within 5 minutes, it promotes the trial: `@` and `@next` swap names in one atomic step (`mv --exchange`), then the old system (now `@next`) is renamed `@previous`. No reboot is needed.
   If they fail, it reboots. If the system hangs or panics, the watchdog or `panic=10` reboots it.
   GRUB consumed the one-shot choice, so every one of those reboots lands on the old `@`.

All six steps work (`compiler/update`, tested by `make test-trial`). The next boot of `@` after a trial that didn't promote deletes `@next` and records why in `/var/lib/rollgate/update.json`. A promotion cut short by a power loss is finished, or treated as a failed trial, on the next boot: `update.json` records `@next`'s subvolume ID before the swap, so that boot can tell whether the swap happened. `rollgate rollback` swaps `@` and `@previous` the same atomic way, so after a reboot the previous system runs and the updated one is kept as `@previous`; `rollgate status` shows the release and what the last update or rollback did.

The running system is never modified during an update, so a failed update leaves nothing half-finished.
M1 ships a simpler interim mechanism: snap-pac pre/post snapshots, with rollback chosen by hand from grub-btrfs.

### Health checks

Each check is a small executable in `/usr/lib/rollgate/health.d/` that exits with status 0 to pass and prints one line saying what it saw.
`rollgate health` runs them all; `rollgate-health.service` runs it at every boot and waits up to 5 minutes for all of them to pass. With `-boot` it also promotes or fails an update's trial boot, as in the update flow above.
Its results go to the journal and the console. When `20-wan` fails at boot, it also prints `networkctl list` and `networkctl status` (each port's state and networkd's recent log lines), so the reason, such as a DHCP lease that never came, is on record.
The checks rollgate ships are one-line scripts calling `rollgate check <name>`, so their logic is in Go and tested (`compiler/health`). Before the first apply, the checks that need a config are skipped or relaxed.

| Script | Passes when |
|---|---|
| `10-firewall` | the nftables ruleset is loaded and has the `rollgate` table |
| `20-wan` | the WAN interface has an IPv4 address and the default route goes through it (before the first apply: any default route) |
| `30-dns` | Unbound answers, and dnsmasq answers on loopback and on every network with a `[dhcp]` section. The query is for a name under `localhost.`, which Unbound answers itself, so it works without internet access |
| `40-ssh` | sshd accepts TCP connections on every management network's address |
| `50-services` | networkd, sshd, Unbound and dnsmasq are running (`nftables.service` loads the ruleset and exits, so `10-firewall` checks the ruleset instead) |

Missing IPv6 from the ISP is never a health-check failure. It's a normal state (ADR 0008).

### Watchdog

A router nobody is watching must not stay hung, so the watchdog is on at every boot, not only during a trial.
systemd (PID 1) feeds the board's watchdog (`image/rootfs/etc/systemd/system.conf.d/10-rollgate-watchdog.conf`):

- `RuntimeWatchdogSec=30s`: if the kernel or systemd stops for 30 seconds, the board resets.
- `RebootWatchdogSec=2min`: a reboot that hangs is cut short too.

Most x86 boards have a hardware watchdog the kernel loads a driver for (Intel `iTCO_wdt`, AMD `sp5100_tco`, and others).
On a board without one, `rollgate-softdog.service` loads the kernel's software watchdog (`softdog`) early in boot, so systemd always has a device.
`softdog` catches a hung systemd, but not a hung kernel; only a hardware watchdog catches that.
`wdctl` on the router shows which one is in use.

### Clock

Updates check signatures and download over HTTPS, and Unbound checks DNSSEC for the LAN; all of that needs the right time.
`systemd-timesyncd` keeps the clock right (ADR 0013), with Arch's default servers (`arch.pool.ntp.org`). The `rollgate` package enables it, so routers that update get it too.
The router resolves names for itself through systemd-resolved, without DNSSEC, so finding the time servers works even when the clock is badly wrong (for example after a dead CMOS battery).
Cloudflare's time service by IP address, `162.159.200.1` and `162.159.200.123`, comes right after the first pool name, so the clock gets set even when names can't be looked up. At boot that's often the server timesyncd ends up with, since its first lookup can come before the network is up (ADR 0015; `/usr/lib/systemd/timesyncd.conf.d/10-rollgate.conf`, from the `rollgate` package).
There's no health check for it: an unreachable time server mustn't roll back an update. `rollgate status` shows whether the clock is synchronized, and a failed `rollgate update` says so when it isn't.

### Kernels

`linux` (latest stable) is the default boot entry. `linux-lts` is always installed as a second entry in every snapshot.
A regression in a new kernel, such as a NIC driver, is fixed by booting `linux-lts` without rolling back anything else.

## Security posture

- WAN inbound: drop by default (hard rule). ICMP is rate-limited. No services listen on WAN.
- Management (SSH, web UI): reachable only from zones listed in `management.allowed_zones`. SSH uses keys only.
- Our own packages are signed; pacman is configured to require valid signatures (the default).
- No AUR, no testing repos (ADR 0003).
- Secrets live in `/etc/rollgate/secrets/` (mode 0700) and are referenced by name from `router.toml`.
