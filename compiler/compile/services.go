package compile

import (
	"net"
	"net/netip"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// dnsmasq is the template data for dnsmasq.tmpl.
type dnsmasq struct {
	Header   string
	Hostname string
	Domain   string
	Services []dhcpService
	DNSOnly  []string // devices that get DNS but no DHCP: the VPN's
}

// dhcpService is one interface or VLAN with a [dhcp.<name>] section.
type dhcpService struct {
	Kind      string // "interface" or "VLAN"
	Name      string
	Device    string
	Router    string // the router's address on this network
	Netmask   string
	Start     string
	End       string
	LeaseTime string
	Static    []config.StaticLease
}

// unbound is the template data for unbound.tmpl.
type unbound struct {
	Header     string
	Domain     string
	IPv6Off    bool
	Forwarders []string // in Unbound's forward-addr syntax, which the spec uses too
	TLS        bool

	Blocklist     bool // dns.blocklists is set: include the downloaded names
	BlocklistGlob string
}

// BlocklistFile is where rollgate dns update keeps the blocked names (ADR
// 0019): inside Unbound's chroot (/etc/unbound), so a reload can read it.
// unbound.conf includes it with a pattern, which matches nothing until the
// first download instead of failing.
const (
	BlocklistFile = "/etc/unbound/rollgate-blocklist.conf"
	blocklistGlob = "/etc/unbound/rollgate-blocklist*.conf"
)

func buildDNSMasq(c *config.Config, header string) dnsmasq {
	d := dnsmasq{Header: header, Hostname: c.System.Hostname, Domain: c.System.Domain}
	if c.VPN != nil {
		d.DNSOnly = []string{config.VPNZone}
	}
	add := func(kind, name, device string, addr netip.Prefix) {
		dh, ok := c.DHCP[name]
		if !ok {
			return
		}
		d.Services = append(d.Services, dhcpService{
			Kind: kind, Name: name, Device: device,
			Router:    addr.Addr().String(),
			Netmask:   net.IP(net.CIDRMask(addr.Bits(), 32)).String(),
			Start:     dh.RangeStart.String(),
			End:       dh.RangeEnd.String(),
			LeaseTime: dh.LeaseTime,
			Static:    dh.Static,
		})
	}
	for _, in := range c.Interfaces {
		add("interface", in.Name, in.Device(), in.Addr)
	}
	for _, v := range c.VLANs {
		add("VLAN", v.Name, v.Name, v.Addr)
	}
	return d
}

func buildUnbound(c *config.Config, header string) unbound {
	u := unbound{Header: header, Domain: c.System.Domain, IPv6Off: c.System.IPv6 == "off",
		Blocklist: len(c.DNS.Blocklists) > 0, BlocklistGlob: blocklistGlob}
	if c.DNS.Mode == "forward" {
		u.Forwarders = c.DNS.Forwarders
		// Validation guarantees all or none use TLS.
		u.TLS = len(u.Forwarders) > 0 && strings.Contains(u.Forwarders[0], "#")
	}
	return u
}
