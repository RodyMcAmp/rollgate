# 0006. networkd + nftables + dnsmasq + Unbound

- Status: Accepted
- Date: 2026-11-17

## Context

We need interface and VLAN management, a WAN DHCP client, IPv6 (see ADR 0008), a stateful firewall with NAT, LAN DHCP with local hostname resolution, and a recursive DNS resolver.
Every piece must be in Arch `extra` or `core`, be well documented, and have a config format the compiler can generate.

## Decision

| Need | Choice |
|---|---|
| Links, VLANs, bridges, WAN DHCPv4 client, WireGuard tunnels (later) | **systemd-networkd** |
| All IPv6: WAN DHCPv6 prefix delegation, assigning /64s to LAN zones, router advertisements (SLAAC + DNS server), the local-only ULA prefix | **systemd-networkd** |
| Firewall and NAT, IPv4 and IPv6 together | **nftables**, one `inet`-family ruleset, loaded atomically |
| LAN DHCPv4, local hostnames for leases | **dnsmasq** |
| Recursive resolution, DNSSEC, blocklists | **Unbound**, listening on `127.0.0.1:5335`; dnsmasq forwards to it |

All IPv6 lives in networkd, because networkd tracks the ISP's delegated prefix and re-announces it to the LAN by itself whenever it changes.
Splitting IPv6 across daemons would need glue code to pass prefix changes along. dnsmasq does not do router advertisements or DHCPv6.

## Alternatives considered

- **Kea (DHCP) + Unbound only**: the direction pfSense and OPNsense have taken. Kea is more capable, but registering dynamic lease hostnames in DNS needs extra glue, which dnsmasq does natively. Reconsider if we need DHCP high availability or Kea's API.
- **dnsmasq for router advertisements as well**: it can construct RAs from interface addresses, but then two programs share responsibility for IPv6.
- **networkd's built-in DHCPv4 server**: fewer daemons, but static leases and local DNS integration are weaker.
- **iptables**: legacy; nftables is its replacement in the kernel.
- **Unbound alone, without dnsmasq**: Unbound has no DHCP.

## Consequences

- Two DNS daemons. Each does what it is best at, at the cost of one extra hop for external lookups.
- The firewall uses one ruleset for both IPv4 and IPv6, so zone policy (e.g. "IoT can't reach LAN") applies to both automatically.
- Local hostnames are registered for IPv4 leases. IPv6 hostnames for SLAAC addresses are out of scope for v1.
