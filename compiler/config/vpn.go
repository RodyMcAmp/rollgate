package config

import (
	"encoding/base64"
	"net/netip"
	"regexp"
	"strings"
)

// peerNameRe: a device's name, as people write them ("maya-phone").
var peerNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// validateVPN checks [vpn] (ADR 0018) and fills in its parsed addresses. Its
// subnet joins the overlap and zone checks in validateRules.
func validateVPN(ps *problems, c *Config) {
	v := c.VPN
	if v == nil {
		return
	}
	if v.Port < 1 || v.Port > 65535 {
		ps.add(CodeInvalid, "vpn.port", "must be a port from 1 to 65535, not %d", v.Port)
	}
	for i, pf := range c.Firewall.PortForward {
		if strings.Contains(pf.Protocol, "udp") && pf.WANPortFirst <= v.Port && v.Port <= pf.WANPortLast {
			ps.add(CodeDuplicate, "vpn.port", "UDP port %d is already forwarded by %s; choose another port for the VPN",
				v.Port, itemPath("firewall.port_forward", i, pf.Name))
		}
	}
	switch v.Reach {
	case "everything", "internet":
	default:
		ps.add(CodeInvalid, "vpn.reach", "must be \"everything\" or \"internet\", not %q", v.Reach)
	}
	if v.Endpoint != "" {
		if _, err := netip.ParseAddr(v.Endpoint); err != nil && !validDomain(strings.TrimSuffix(v.Endpoint, ".")) {
			ps.add(CodeInvalid, "vpn.endpoint", "%q isn't a name or an address, such as \"home.example.net\"", v.Endpoint)
		}
	}

	p, err := netip.ParsePrefix(v.Address)
	switch {
	case err != nil || !p.Addr().Is4():
		ps.add(CodeInvalid, "vpn.address", "must be an IPv4 address with its size, such as \"10.66.0.1/24\", not %q", v.Address)
		p = netip.Prefix{}
	case p.Bits() < 16 || p.Bits() > 30:
		ps.add(CodeInvalid, "vpn.address", "the VPN's size must be /16 to /30, not /%d", p.Bits())
		p = netip.Prefix{}
	case p.Addr() == p.Masked().Addr() || p.Addr() == lastAddr(p):
		ps.add(CodeInvalid, "vpn.address", "%s is the network's own or broadcast address; the router needs another, such as %s", p.Addr(), p.Masked().Addr().Next())
		p = netip.Prefix{}
	}
	v.Addr = p

	names, keys, addrs := map[string]bool{}, map[string]bool{}, map[netip.Addr]bool{}
	for i := range v.Peers {
		peer := &v.Peers[i]
		path := itemPath("vpn.peer", i, peer.Name)
		switch {
		case peer.Name == "":
			ps.add(CodeMissing, path+" name", "give the device a name, such as \"phone\"")
		case !peerNameRe.MatchString(peer.Name):
			ps.add(CodeInvalid, path+" name", "a device's name is lowercase letters, digits and dashes, up to 32 characters")
		case names[peer.Name]:
			ps.add(CodeDuplicate, path+" name", "another device is already named %q", peer.Name)
		}
		names[peer.Name] = true

		if b, err := base64.StdEncoding.DecodeString(peer.PublicKey); err != nil || len(b) != 32 {
			if peer.PublicKey == "" {
				ps.add(CodeMissing, path+" public_key", "the device's WireGuard public key is missing")
			} else {
				ps.add(CodeInvalid, path+" public_key", "%q isn't a WireGuard public key (44 characters of base64)", peer.PublicKey)
			}
		} else if keys[peer.PublicKey] {
			ps.add(CodeDuplicate, path+" public_key", "another device already has this key")
		}
		keys[peer.PublicKey] = true

		a, err := netip.ParseAddr(peer.IPv4)
		switch {
		case peer.IPv4 == "":
			ps.add(CodeMissing, path+" ipv4", "the device needs its own address inside the VPN, such as %s", suggestPeer(v.Addr))
		case err != nil || !a.Is4():
			ps.add(CodeInvalid, path+" ipv4", "%q isn't an IPv4 address", peer.IPv4)
		case !v.Addr.IsValid():
			// vpn.address is already reported
		case !v.Addr.Contains(a) || a == v.Addr.Masked().Addr() || a == lastAddr(v.Addr):
			ps.add(CodeInvalid, path+" ipv4", "%s isn't a device address inside the VPN (%s)", a, v.Addr.Masked())
		case a == v.Addr.Addr():
			ps.add(CodeInvalid, path+" ipv4", "%s is the router's own address in the VPN", a)
		case addrs[a]:
			ps.add(CodeDuplicate, path+" ipv4", "another device already has %s", a)
		default:
			peer.Addr = a
		}
		addrs[a] = true
	}
}

// suggestPeer is the address after the router's in the VPN, for messages.
func suggestPeer(p netip.Prefix) string {
	if !p.IsValid() {
		return "10.66.0.2"
	}
	return p.Addr().Next().String()
}
