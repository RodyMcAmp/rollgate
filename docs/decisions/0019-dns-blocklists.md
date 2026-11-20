# 0019. DNS blocklists in Unbound, refreshed by a timer

- Status: Proposed
- Date: 2026-11-19

## Context

The roadmap's M6 and the "add-ons" idea both want ad and tracker blocking built in, so people don't need a separate Pi-hole.
`router.toml` already reserves `dns.blocklists`: the spec calls it "fetched on a timer *(planned, M6)*", and validation refuses a non-empty list until then, so it can't silently do nothing.

How DNS works today (ADR 0006): devices ask dnsmasq on the router (port 53 on each network with a `[dhcp.<name>]` section). dnsmasq answers local names and forwards everything else to Unbound on `127.0.0.1:5335`, which resolves it, recursively or through forwarders. Every device's lookups therefore pass through Unbound.

Blocklists are lists of domain names published by third parties and updated often, some daily. So something must download them regularly, which nothing on the router does yet. The compiler only turns `router.toml` into files, and has no network access by design.

## Decision

### Block in Unbound, with names from downloaded lists

- Unbound answers a blocked name, and every name under it, with `0.0.0.0` / `::` (`local-zone: "<name>" always_null`). Apps then fail at once instead of waiting, as with Pi-hole's default. Every device on every network is covered, because every lookup goes through Unbound.
- The names live in a data file, `/var/lib/rollgate/dns-blocklist.conf`. The compiler's `unbound.conf` includes it only when `dns.blocklists` is set. So removing the setting (or reverting to a generation without it) stops blocking at the next apply, whatever the data file holds.

### In router.toml

```toml
[dns]
blocklists = ["recommended"]          # a built-in choice, or https:// URLs
allow = ["example-shop.com"]          # names never blocked (lists get things wrong)
```

- `blocklists` takes the built-in name **`"recommended"`** or `https://` URLs. `"recommended"` is a well-known list with few false positives: the StevenBlack unified hosts list (adware and malware; MIT license). Its URL is in the code, so changing it is a reviewed change. More built-in names can be added the same way.
- `allow` is the one other setting. Every blocklist blocks something it shouldn't sooner or later, and users need a way to fix that without editing lists. A name in `allow` and every name under it is never blocked.
- The default is no blocking, as today. Both settings get validation codes as usual: a URL must be `https://`, names must be valid.

### Downloading: a timer and a one-shot service, not a daemon

- `rollgate-blocklists.timer` runs `rollgate dns update` (`rollgate-blocklists.service`, `Type=oneshot`) once a day at a random time, and 5 minutes after boot. `apply` also starts it when `dns.blocklists` or `dns.allow` changed, without waiting for it. The revert timer (ADR 0010) already works this way, so there's no new daemon.
- `rollgate dns update`:
  1. downloads each list over HTTPS (Go's standard library, the system's certificates), with a timeout and a size limit (32 MB per list);
  2. reads the common formats: hosts files (`0.0.0.0 ads.example.com`), plain lists of names, and simple Adblock rules (`||ads.example.com^`); it skips anything else;
  3. keeps only valid names, in lower case, without duplicates. It drops names in `allow`, the router's own names, `localhost`, and the local domain (`system.domain`), so a list can't break local names;
  4. caps the total at 500,000 names, so Unbound's memory stays well inside the 2 GB minimum, and warns if the lists were bigger;
  5. writes the file next to the old one, checks it with `unbound-checkconf`, swaps it in, and reloads Unbound. If the check or the reload fails, it puts the old file back and reloads again.
- **If a download fails, the last good file stays** and blocking goes on with it. A failure never empties the list, and never stops DNS.
- What it did (when, how many names from which list, what failed) goes to the journal and to a small state file, for `rollgate status` and the web UI.

### Why this isn't a second write path (hard rule 3)

- The compiler alone writes Unbound's configuration, and the include line appears only when `router.toml` asks for blocklists. The blocklist file is data that changes daily from outside sources, like dnsmasq's lease file, not configuration.
- It isn't part of a generation (ADR 0010): rolling back `router.toml` doesn't need old copies of the lists. A rollback that removes `dns.blocklists` turns blocking off.

### Later, not in this decision

- A web UI page: how many names are blocked, when the lists were last updated, and an **Allow** button that adds a name to `dns.allow` with `compiler/edit`.
- Per-network blocking (say, only on the kids' network).
- Catching devices that ignore the router's DNS (hard-coded servers, DNS over HTTPS): possible with a firewall redirect of port 53, but it breaks some devices, so it needs its own decision.

## Alternatives considered

- **Pi-hole or AdGuard Home on the router:** a new daemon with its own web UI, DNS server and package source (neither is in Arch's official repos, against hard rule 1).
- **Block in dnsmasq** (`address=/name/`): dnsmasq is slower with very long lists, and only devices that ask dnsmasq would be covered. Unbound sees everything.
- **Unbound's RPZ support:** efficient, but most lists aren't published as RPZ zones, and a `local-zone` file is simpler to check and read.
- **NXDOMAIN instead of `0.0.0.0`:** some apps retry a "no such name" answer, so blocked lookups take longer. `always_null` fails fast.
- **Ship the lists in the image:** they'd be up to a week old (the release train), and the release would carry third-party data.

## Consequences

- The spec's `dns.blocklists` stops being *(planned)*; `dns.allow` is new (`docs/config-spec.md` and `examples/router.toml` first, then code).
- New code: `rollgate dns update`, the parser, the `.timer` and `.service` units in the `rollgate` package, the include line in `unbound.tmpl`, and the status line.
- The router downloads the lists once a day from where they're published, StevenBlack's list from GitHub (`raw.githubusercontent.com`). This goes in the docs, since it's traffic the router makes by itself.
- Tests:
  - L1: the parser, with each format, bad lines, the cap and `allow`; golden `unbound.conf` with and without the include.
  - L3: the VM with a list served by the test's own HTTP server (a test-only URL override, like `ROLLGATE_RELEASES_URL`): a listed name resolves to `0.0.0.0` through the router and an allowed one doesn't; a failed download keeps the last good list; removing the setting stops blocking.
- Unbound uses more memory with large lists, bounded by the cap.
