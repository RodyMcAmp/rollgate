// Package config loads and validates router.toml (docs/config-spec.md).
//
// Load returns a Config with every default filled in, or the full list of
// Problems found. Each Problem carries a stable error code (E001...).
package config

import "net/netip"

// Config is a parsed router.toml. Field docs live in docs/config-spec.md;
// keep the toml tags identical to the key names there.
type Config struct {
	SchemaVersion int             `toml:"schema_version"`
	System        System          `toml:"system"`
	Management    Management      `toml:"management"`
	Interfaces    []Interface     `toml:"interface"`
	VLANs         []VLAN          `toml:"vlan"`
	DHCP          map[string]DHCP `toml:"dhcp"`
	DNS           DNS             `toml:"dns"`
	Firewall      Firewall        `toml:"firewall"`
	IPv6          IPv6            `toml:"ipv6"`
}

type System struct {
	Hostname string `toml:"hostname"`
	Domain   string `toml:"domain"`
	Timezone string `toml:"timezone"`
	IPv6     string `toml:"ipv6"` // "auto" or "off"
}

type Management struct {
	AllowedZones          []string `toml:"allowed_zones"`
	SSHAuthorizedKeys     []string `toml:"ssh_authorized_keys"`
	ConfirmTimeoutSeconds int      `toml:"confirm_timeout_seconds"`
}

// Interface is a physical port. Its zone defaults to its name.
type Interface struct {
	Name         string `toml:"name"`
	MatchMAC     string `toml:"match_mac"`
	MatchDevice  string `toml:"match_device"`
	Zone         string `toml:"zone"`
	IPv4         string `toml:"ipv4"` // "dhcp" or a CIDR address
	IPv6SubnetID *int   `toml:"ipv6_subnet_id"`

	Addr netip.Prefix `toml:"-"` // router address and prefix; invalid when IPv4 is "dhcp"
}

// Device is the Linux network device name for this interface: its match_device,
// or else its name (a .link file renames the port to it). See the spec's
// "How names map to devices".
func (in Interface) Device() string {
	if in.MatchDevice != "" {
		return in.MatchDevice
	}
	return in.Name
}

type VLAN struct {
	Name         string `toml:"name"`
	Parent       string `toml:"parent"`
	ID           int    `toml:"id"`
	Zone         string `toml:"zone"`
	IPv4         string `toml:"ipv4"`
	IPv6SubnetID *int   `toml:"ipv6_subnet_id"`

	Addr netip.Prefix `toml:"-"`
}

type DHCP struct {
	Range     []string      `toml:"range"`
	LeaseTime string        `toml:"lease_time"`
	Static    []StaticLease `toml:"static"`

	RangeStart netip.Addr `toml:"-"`
	RangeEnd   netip.Addr `toml:"-"`
}

type StaticLease struct {
	Hostname string `toml:"hostname"`
	MAC      string `toml:"mac"`
	IPv4     string `toml:"ipv4"`

	Addr netip.Addr `toml:"-"`
}

type DNS struct {
	Mode       string   `toml:"mode"` // "recursive" or "forward"
	Forwarders []string `toml:"forwarders"`
	Blocklists []string `toml:"blocklists"`
}

type Firewall struct {
	Policy      []Policy      `toml:"policy"`
	PortForward []PortForward `toml:"port_forward"`
}

type Policy struct {
	From    string `toml:"from"` // zone or "*"
	To      string `toml:"to"`
	Action  string `toml:"action"` // "accept", "drop" or "reject"
	Comment string `toml:"comment"`
}

type PortForward struct {
	Name     string `toml:"name"`
	Protocol string `toml:"protocol"` // "tcp", "udp" or "tcp+udp"
	WANPort  any    `toml:"wan_port"` // integer or "start-end"
	To       string `toml:"to"`       // "ip:port"

	WANPortFirst int            `toml:"-"`
	WANPortLast  int            `toml:"-"`
	Target       netip.AddrPort `toml:"-"`
}

// IPv6 holds the rarely needed overrides from the spec's Advanced section.
type IPv6 struct {
	ULAPrefix     string `toml:"ula_prefix"`
	WANPrefixHint int    `toml:"wan_prefix_hint"`

	ULA netip.Prefix `toml:"-"` // invalid when not set; generated at apply time
}
