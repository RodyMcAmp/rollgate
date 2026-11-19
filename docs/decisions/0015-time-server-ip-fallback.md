# 0015. Time servers by IP address as a fallback

- Status: Accepted (by the maintainer, 2026-11-19)
- Date: 2026-11-19
- Amends: 0013

## Context

ADR 0013 has systemd-timesyncd use Arch's default time servers, `0–3.arch.pool.ntp.org`, which are names.
If the router can't look up names, it can't find a time server either, and a wrong clock stays wrong.
That breaks the things ADR 0013 is for: update signatures, HTTPS and the LAN's DNSSEC.

The router resolves names for itself through public DNS servers (systemd-resolved's built-in list), not the ISP's (`UseDNS=no`).
Some networks block outside DNS servers, so name lookups can fail while the internet otherwise works.
The VMware VM showed how far off a clock can start: VMware handed it the PC's local time (Alaska) as UTC, 9 hours behind, which time sync then corrected.

## Decision

- timesyncd's server list becomes `0.arch.pool.ntp.org`, then **Cloudflare's time service by IP address**, `162.159.200.1` and `162.159.200.123`, then the other three pool names.
  These are anycast addresses Cloudflare publishes for NTP (developers.cloudflare.com/time-services/ntp/usage/), so they don't depend on one machine.
- timesyncd tries the servers in order and stays with the first that answers. At boot the first name is often looked up before the network is up, so in practice many routers use Cloudflare from boot on; that's fine, it's a public service built for this. The other pool names stay in the list for when Cloudflare doesn't answer.
  The addresses come second, not last: every entry timesyncd tries costs a lookup, and with no DNS server reachable a lookup can take about 2 minutes to give up (measured in the L3 test VM). The fewer entries before an address, the fewer such waits.
- **Cloudflare, not Google.** Google's time servers smear leap seconds over a day, and Google advises against mixing them with servers that don't. The pool and Cloudflare both don't smear.
- **IPv4 only.** Every network the router can use has IPv4 (ADR 0008 adds IPv6 alongside, never instead).
- **Shipped in the `rollgate` package** (`/usr/lib/systemd/timesyncd.conf.d/10-rollgate.conf`), like the enablement in ADR 0013, so routers that update get it too.
- **Still no setting in `router.toml`** (rule 9).

## Alternatives considered

- **Google's servers by IP:** they smear leap seconds; mixing them with the pool's would let the clock jump between two kinds of time near a leap second.
- **Only IP addresses:** hardcoded addresses can go stale. The pool's names stay first, so the router normally follows them.
- **The ISP's DNS servers as a fallback for the router's own lookups:** a larger change that affects every lookup, not only time. It's an open question for M5, to check on real networks.

## Consequences

- One more file in the `rollgate` package.
- On a network where name lookups fail, the clock still gets set, and `timedatectl timesync-status` names a Cloudflare address as the server.
- A new L3 test blocks the router's DNS, sets its clock 9 hours back, and checks that it syncs from a Cloudflare address. Like the update trial test, it needs internet.
- **With all DNS traffic dropped, the fallback takes about 8 minutes (481 s in the test VM), whatever the order.**
  timesyncd looks up every entry through systemd-resolved, even an IP address, and while no DNS server answers, those lookups only return after resolved's retries, about 2 minutes each.
  Once a request reached Cloudflare, the clock was stepped at once, even 9 hours off.
  Updates can fail in those minutes after such a boot, and `rollgate update`'s error says the clock isn't synchronized yet. When DNS works, the clock is set within seconds.
