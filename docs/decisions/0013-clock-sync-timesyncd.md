# 0013. The router keeps its clock right with systemd-timesyncd

- Status: Accepted, amended by 0015 (time servers by IP address as a fallback)
- Date: 2026-11-19

## Context

The router never sets its clock: no time sync service runs.
In the test VMs, and in VMware, that doesn't show, because the virtual machine starts from the host's clock.
On real hardware the clock comes from the board's battery-backed clock (RTC).
On old boards, which M5 means to support (Sandy Bridge, 2011, and newer), that battery is often dead, so the clock starts in the past at every power-up.

A wrong clock breaks things the router depends on:

- `rollgate update` checks the signature on `releases.json` and downloads over HTTPS. A clock in the past makes signatures look "made in the future" and certificates "not yet valid", so updates fail.
- Unbound validates DNSSEC for the LAN (ADR 0006). Signatures carry validity dates, so with a clock far off, LAN name lookups fail.
- Logs, `rollgate status` and the update history show wrong times.

AGENTS.md rule 6 needs an ADR for a new daemon, and a time sync service is one.

## Decision

- **systemd-timesyncd** keeps the router's clock right. It is part of systemd, already on the image. It is an SNTP client only: it sets this router's clock and serves no one else.
- **Enabled everywhere.** The image enables it. The `rollgate` package also ships the enablement, a symlink in `/usr/lib/systemd/system/sysinit.target.wants/`, so routers installed before this change get it with their next update.
- **Default servers, no option.** It uses the time servers systemd is built with on Arch, `0–3.arch.pool.ntp.org`. `router.toml` gets no setting for it (rule 9: one fewer thing to know). A real need for other servers, such as a network that blocks public NTP, would go in the spec's Advanced section with its own change.
- **Lookups don't depend on the clock.** The router resolves names for itself through systemd-resolved, which doesn't validate DNSSEC. So finding the time servers works even when the clock is badly wrong. Unbound's DNSSEC for the LAN starts working as soon as the clock is right, seconds after WAN comes up.
- **The clock never goes backwards across boots.** When timesyncd starts at boot, it moves the clock forward to no earlier than the time it last saved (`/var/lib/systemd/timesync/clock`). So until the first sync, a dead RTC costs at most the time the router was switched off.
- **No health check for it.** An unreachable time server must not roll back a good update (ADR 0011). Instead:
  - `rollgate status` shows whether the clock is synchronized.
  - When `rollgate update` fails and the clock isn't synchronized, its error says so.
- The firewall needs no change. The router's own outgoing traffic is allowed, and replies match the connection it opened.

## Alternatives considered

- **chrony**: a full NTP implementation that can also serve time to the LAN. It's a new package and a bigger daemon, for a feature nobody has asked for. If serving time to the LAN becomes a need, a later ADR can replace timesyncd with chrony.
- **ntpd or OpenNTPD**: same objection as chrony, and less maintained.
- **Set the clock once at boot (`ntpdate`-style)**: simpler, but the clock drifts over months of uptime, and a router that boots without internet would never correct itself.
- **Make `rollgate update` refuse to run until the clock is synchronized**: safer in theory, but it would block updates for anyone whose network blocks NTP, even when their clock is fine. The error message is enough.

## Consequences

- One more service runs on every router. It is small, and it only talks to the time servers.
- The router sends NTP queries to the public pool (`arch.pool.ntp.org`) about every 34 minutes once in sync, as every Arch system using timesyncd does.
- `rollgate status` gains a clock line, and `rollgate update` a hint in its error message.
- The L3 boot test checks that timesyncd runs on every boot.
