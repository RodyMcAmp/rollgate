# 0008. IPv6 is one setting: `auto` or `off`

- Status: Accepted
- Date: 2026-11-17

## Context

IPv6 is where router configuration usually gets hard: prefix delegation sizes, SLAAC vs. DHCPv6, RA flags, prefix IDs per interface, what happens when the ISP changes the prefix.
Many users, including the project's own maintainer, either don't have IPv6 from their ISP or don't know whether they do.
They still want IPv6 to work inside their own network, and IPv6 internet to start working on its own if the ISP enables it.

## Decision

A single user-facing setting: `system.ipv6 = "auto"` (the default) or `"off"`.

With `auto`, the router:

1. **Always gives every LAN zone local IPv6**, from a unique local address (ULA) prefix.
   The router generates a random `fdXX:XXXX:XXXX::/48` (RFC 4193) on first boot and stores it in `/etc/rollgate/state/ula-prefix`, so it never changes and is included in config backups.
   Each zone gets a /64 from it, with a subnet ID derived from the zone name. That means adding or reordering zones never renumbers the others.
2. **Asks the ISP for IPv6 on WAN** (router advertisements plus DHCPv6 prefix delegation). This is harmless when the ISP offers none.
3. **If the ISP delegates a prefix, also gives every LAN zone a public /64 from it**, with the same subnet-ID scheme, and announces it. IPv6 internet then works with no further configuration. When the ISP changes the prefix, networkd re-announces the new one automatically.
4. **Hands out addresses with SLAAC** and announces the router's **ULA address** as the DNS server (RDNSS).
   SLAAC is the one method every OS supports; Android does not support stateful DHCPv6.
   The ULA address never changes, even when the ISP's prefix does, so DNS settings on clients never go stale.
5. **Applies the same firewall policy to IPv6 as to IPv4** (one `inet` ruleset, ADR 0006).
   WAN inbound is default-deny for IPv6 too. There is no NAT for IPv6, which isn't needed.

With `off`: no IPv6 on WAN or LAN, and IPv6 forwarding is disabled.

### Why local-only IPv6 doesn't break internet access

When a device has only a ULA IPv6 address, the operating system's default address selection (RFC 6724) prefers IPv4 for internet destinations.
So devices use IPv4 for the internet and IPv6 for local traffic, with no timeouts or broken sites.

## Advanced overrides (not shown in the example config)

- `[ipv6] ula_prefix`: use a specific ULA /48, e.g. when migrating from another router.
- `ipv6_subnet_id` on an interface or VLAN: pin a zone's /64 ID.
- `[ipv6] wan_prefix_hint`: request a particular delegated prefix size from the ISP (default 56).

These exist for unusual setups. The default experience never needs them.

## Alternatives considered

- **Per-interface IPv6 modes** (pfSense-style: "track interface", "static", "DHCPv6"): flexible, but this is exactly the complexity users struggle with.
- **NAT66/NPTv6** to give LAN devices "public-ish" IPv6 without prefix delegation: works against how IPv6 is designed and adds failure modes. Not offered.
- **No IPv6 unless the ISP provides it**: simpler, but users would lose IPv6 inside their own network.
- **A tunnel broker (6in4) for users whose ISP has no IPv6**: useful, but it needs an account and a public IPv4 address (it doesn't work behind carrier-grade NAT). Possible later as an opt-in feature.

## Consequences

- Health checks must not fail when the ISP offers no IPv6. Missing global IPv6 is a normal state, not an error.
- `rollgate status` should explain the IPv6 state in plain words, e.g. "IPv6: local only (your ISP isn't offering IPv6)" or "IPv6: local + internet (prefix 2001:db8:1234::/56 from ISP)".
- L3 tests must cover both cases: a fake ISP with prefix delegation and one without.
