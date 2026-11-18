package config

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // timezone names validate the same on every machine

	"github.com/BurntSushi/toml"
)

var (
	// Interface, VLAN and zone names; 15 is Linux's network device name limit.
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)
	labelRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`) // RFC 1123
	deviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)
	secretRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	leaseRe  = regexp.MustCompile(`^([0-9]+[smhdw]?|infinite)$`)
)

const nameRule = "use 1–15 lowercase letters, digits or '-', starting with a letter"

// validateFields checks each value on its own: formats, ranges and required
// keys. Rules that relate values to each other (E001–E006) are in rules.go.
func validateFields(c *Config, md toml.MetaData, ps *problems) {
	if !md.IsDefined("schema_version") {
		ps.add(CodeMissing, "schema_version", "add `schema_version = 1` at the top of the file")
	} else if c.SchemaVersion != 1 {
		ps.add(CodeInvalid, "schema_version", "this version of rollgate understands schema_version 1 only")
	}

	s := c.System
	if !labelRe.MatchString(s.Hostname) {
		ps.add(CodeInvalid, "system.hostname", "%q isn't a valid hostname; use letters, digits and '-', such as \"gateway\"", s.Hostname)
	}
	if !validDomain(s.Domain) {
		ps.add(CodeInvalid, "system.domain", "%q isn't a valid domain; the default \"home.arpa\" is right for most homes", s.Domain)
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "" || s.Timezone == "Local" {
		ps.add(CodeInvalid, "system.timezone", "%q isn't a known timezone; use a name such as \"UTC\" or \"Europe/Berlin\"", s.Timezone)
	}
	if s.IPv6 != "auto" && s.IPv6 != "off" {
		ps.add(CodeInvalid, "system.ipv6", "must be \"auto\" or \"off\", not %q", s.IPv6)
	}

	m := c.Management
	if len(m.AllowedZones) == 0 {
		ps.add(CodeInvalid, "management.allowed_zones", "must list at least one zone, or nobody could manage the router")
	}
	for _, z := range m.AllowedZones {
		switch {
		case z == "wan":
			ps.add(CodeInvalid, "management.allowed_zones", "\"wan\" isn't allowed: the router must never be manageable from the internet")
		case !nameRe.MatchString(z):
			ps.add(CodeInvalid, "management.allowed_zones", "%q isn't a valid zone name; %s", z, nameRule)
		}
	}
	for _, k := range m.SSHAuthorizedKeys {
		if !secretRe.MatchString(k) || strings.Contains(k, "..") {
			ps.add(CodeInvalid, "management.ssh_authorized_keys", "%q isn't a valid secret name; give a file name in /etc/rollgate/secrets/, such as \"admin.pub\"", k)
		}
	}
	if t := m.ConfirmTimeoutSeconds; t < 30 || t > 900 {
		ps.add(CodeInvalid, "management.confirm_timeout_seconds", "must be 30–900 seconds, not %d", t)
	}

	names := map[string]bool{}
	checkName := func(path, name string) {
		switch {
		case name == "":
			ps.add(CodeMissing, path, "set a name, such as \"lan\"")
		case !nameRe.MatchString(name):
			ps.add(CodeInvalid, path, "%q isn't a valid name; %s", name, nameRule)
		case names[name]:
			ps.add(CodeDuplicate, path, "the name %q is already used by another interface or VLAN", name)
		}
		names[name] = true
	}
	subnetIDs := map[int]string{}
	checkSubnetID := func(path string, id *int) {
		if id == nil {
			return
		}
		if *id < 0 || *id > 255 {
			ps.add(CodeIPv6, path+" ipv6_subnet_id", "must be 0–255, not %d", *id)
		} else if other, ok := subnetIDs[*id]; ok {
			ps.add(CodeIPv6, path+" ipv6_subnet_id", "%d is already used by %s", *id, other)
		}
		subnetIDs[*id] = path
	}

	for i := range c.Interfaces {
		in := &c.Interfaces[i]
		path := itemPath("interface", i, in.Name)
		checkName(path+" name", in.Name)
		switch {
		case in.MatchMAC != "" && in.MatchDevice != "":
			ps.add(CodeInvalid, path, "set only one of match_mac or match_device")
		case in.MatchMAC != "":
			checkMAC(ps, path+" match_mac", in.MatchMAC)
		case in.MatchDevice != "":
			if !deviceRe.MatchString(in.MatchDevice) {
				ps.add(CodeInvalid, path+" match_device", "%q isn't a valid Linux device name, such as \"enp1s0\"", in.MatchDevice)
			}
		default:
			ps.add(CodeMissing, path, "set match_mac (preferred) or match_device so the router knows which port this is")
		}
		checkZone(ps, path+" zone", in.Zone)
		in.Addr = checkIPv4(ps, path+" ipv4", in.IPv4)
		checkSubnetID(path, in.IPv6SubnetID)
	}

	vlanIDs := map[string]bool{}
	for i := range c.VLANs {
		v := &c.VLANs[i]
		path := itemPath("vlan", i, v.Name)
		checkName(path+" name", v.Name)
		if v.Parent == "" {
			ps.add(CodeMissing, path+" parent", "set the interface this VLAN runs on, such as \"lan\"")
		} else if !nameRe.MatchString(v.Parent) {
			ps.add(CodeInvalid, path+" parent", "%q isn't a valid interface name; %s", v.Parent, nameRule)
		}
		if v.ID < 1 || v.ID > 4094 {
			ps.add(CodeInvalid, path+" id", "set a VLAN number from 1 to 4094")
		} else if key := fmt.Sprintf("%s/%d", v.Parent, v.ID); vlanIDs[key] {
			ps.add(CodeDuplicate, path+" id", "VLAN %d is already defined on %q", v.ID, v.Parent)
		} else {
			vlanIDs[key] = true
		}
		checkZone(ps, path+" zone", v.Zone)
		v.Addr = checkIPv4(ps, path+" ipv4", v.IPv4)
		checkSubnetID(path, v.IPv6SubnetID)
	}

	for _, name := range slices.Sorted(maps.Keys(c.DHCP)) {
		d := c.DHCP[name]
		validateDHCP(ps, "dhcp."+name, name, &d)
		c.DHCP[name] = d
	}

	validateDNS(ps, &c.DNS)

	for i, p := range c.Firewall.Policy {
		path := fmt.Sprintf("firewall.policy #%d", i+1)
		for _, end := range []struct{ key, zone string }{{"from", p.From}, {"to", p.To}} {
			switch {
			case end.zone == "":
				ps.add(CodeMissing, path+" "+end.key, "set a zone name, or \"*\" for any zone")
			case end.zone != "*" && !nameRe.MatchString(end.zone):
				ps.add(CodeInvalid, path+" "+end.key, "%q isn't a valid zone name; %s", end.zone, nameRule)
			}
		}
		if p.Action != "accept" && p.Action != "drop" && p.Action != "reject" {
			ps.add(CodeInvalid, path+" action", "must be \"accept\", \"drop\" or \"reject\", not %q", p.Action)
		}
		// AGENTS.md hard rule 5: WAN inbound is default-deny; only port forwards let traffic in.
		if p.From == "wan" && p.Action == "accept" {
			ps.add(CodeInvalid, path+" from", "accepting traffic from the internet would expose your network; use a [[firewall.port_forward]] to let a specific service in")
		}
	}

	forwards := map[string]bool{}
	for i := range c.Firewall.PortForward {
		validatePortForward(ps, i, &c.Firewall.PortForward[i], forwards)
	}

	if c.IPv6.ULAPrefix != "" {
		p, err := netip.ParsePrefix(c.IPv6.ULAPrefix)
		if err != nil || !p.Addr().Is6() || p.Bits() != 48 || p != p.Masked() || !netip.MustParsePrefix("fd00::/8").Contains(p.Addr()) {
			ps.add(CodeIPv6, "ipv6.ula_prefix", "%q must be a /48 inside fd00::/8, such as \"fd12:3456:789a::/48\"", c.IPv6.ULAPrefix)
		} else {
			c.IPv6.ULA = p
		}
	}
	if h := c.IPv6.WANPrefixHint; h < 48 || h > 64 {
		ps.add(CodeIPv6, "ipv6.wan_prefix_hint", "must be 48–64, not %d", h)
	}
}

func validateDHCP(ps *problems, path, name string, d *DHCP) {
	if !nameRe.MatchString(name) {
		ps.add(CodeInvalid, path, "%q isn't a valid interface or VLAN name; %s", name, nameRule)
	}
	switch len(d.Range) {
	case 0:
		ps.add(CodeMissing, path+" range", "set the addresses to hand out, such as [\"192.168.10.100\", \"192.168.10.199\"]")
	case 2:
		d.RangeStart = checkAddr4(ps, path+" range", d.Range[0])
		d.RangeEnd = checkAddr4(ps, path+" range", d.Range[1])
		if d.RangeStart.IsValid() && d.RangeEnd.IsValid() && d.RangeEnd.Less(d.RangeStart) {
			ps.add(CodeInvalid, path+" range", "the first address %s comes after the last one %s", d.RangeStart, d.RangeEnd)
		}
	default:
		ps.add(CodeInvalid, path+" range", "must be two addresses, [first, last]")
	}
	if !leaseRe.MatchString(d.LeaseTime) {
		ps.add(CodeInvalid, path+" lease_time", "%q isn't a duration; use e.g. \"12h\", \"30m\", \"7d\" or \"infinite\"", d.LeaseTime)
	}
	seen := map[string]bool{}
	for i := range d.Static {
		s := &d.Static[i]
		spath := itemPath(path+" static", i, s.Hostname)
		if s.Hostname == "" {
			ps.add(CodeMissing, spath+" hostname", "set the device's name, such as \"nas\"")
		} else if !labelRe.MatchString(s.Hostname) {
			ps.add(CodeInvalid, spath+" hostname", "%q isn't a valid hostname; use letters, digits and '-'", s.Hostname)
		}
		if s.MAC == "" {
			ps.add(CodeMissing, spath+" mac", "set the device's MAC address, such as \"00:00:5e:00:53:10\"")
		} else {
			checkMAC(ps, spath+" mac", s.MAC)
		}
		if s.IPv4 == "" {
			ps.add(CodeMissing, spath+" ipv4", "set the address this device always gets")
		} else {
			s.Addr = checkAddr4(ps, spath+" ipv4", s.IPv4)
		}
		for _, v := range []string{"hostname " + s.Hostname, "mac " + strings.ToLower(s.MAC), "ipv4 " + s.IPv4} {
			if !strings.HasSuffix(v, " ") && seen[v] {
				ps.add(CodeDuplicate, spath, "another static lease already uses %s", v)
			}
			seen[v] = true
		}
	}
}

func validateDNS(ps *problems, d *DNS) {
	switch d.Mode {
	case "recursive":
		if len(d.Forwarders) > 0 {
			ps.add(CodeInvalid, "dns.forwarders", "forwarders are only used with mode = \"forward\"; remove them or change the mode")
		}
	case "forward":
		if len(d.Forwarders) == 0 {
			ps.add(CodeMissing, "dns.forwarders", "mode = \"forward\" needs at least one forwarder, such as \"9.9.9.9\"")
		}
	default:
		ps.add(CodeInvalid, "dns.mode", "must be \"recursive\" or \"forward\", not %q", d.Mode)
	}
	for _, f := range d.Forwarders {
		if !validForwarder(f) {
			ps.add(CodeInvalid, "dns.forwarders", "%q should be an IP address, optionally with a port and TLS name: \"9.9.9.9@853#dns.quad9.net\"", f)
		}
	}
	if len(d.Blocklists) > 0 {
		ps.add(CodeInvalid, "dns.blocklists", "blocklists aren't supported yet (planned for M6); remove this setting for now")
	}
}

func validatePortForward(ps *problems, i int, pf *PortForward, seen map[string]bool) {
	path := itemPath("firewall.port_forward", i, pf.Name)
	switch {
	case pf.Name == "":
		ps.add(CodeMissing, path+" name", "give the port forward a name, such as \"game server\"")
	case seen[pf.Name]:
		ps.add(CodeDuplicate, path+" name", "another port forward is already called %q", pf.Name)
	}
	seen[pf.Name] = true
	if pf.Protocol != "tcp" && pf.Protocol != "udp" && pf.Protocol != "tcp+udp" {
		ps.add(CodeInvalid, path+" protocol", "must be \"tcp\", \"udp\" or \"tcp+udp\", not %q", pf.Protocol)
	}
	first, last, ok := 0, 0, false
	switch v := pf.WANPort.(type) {
	case nil:
		ps.add(CodeMissing, path+" wan_port", "set the port to open on the internet side, such as 27015")
	case int64:
		first, last, ok = int(v), int(v), v >= 1 && v <= 65535
	case string:
		a, b, found := strings.Cut(v, "-")
		var errA, errB error
		first, errA = strconv.Atoi(a)
		last, errB = strconv.Atoi(b)
		ok = found && errA == nil && errB == nil && first >= 1 && first <= last && last <= 65535
	}
	if pf.WANPort != nil && !ok {
		ps.add(CodeInvalid, path+" wan_port", "must be a port 1–65535 such as 27015, or a range such as \"27015-27030\"")
	}
	if ok {
		// Left at 0 when invalid, so the rules in rules.go skip it.
		pf.WANPortFirst, pf.WANPortLast = first, last
	}
	if pf.To == "" {
		ps.add(CodeMissing, path+" to", "set where to send the traffic, as \"ip:port\", such as \"192.168.10.50:27015\"")
	} else if t, err := netip.ParseAddrPort(pf.To); err != nil || !t.Addr().Is4() || t.Port() == 0 {
		ps.add(CodeInvalid, path+" to", "%q must be an IPv4 address and port, such as \"192.168.10.50:27015\"", pf.To)
	} else {
		pf.Target = t
	}
}

// checkIPv4 parses an interface's ipv4 value. It returns an invalid Prefix for
// "dhcp" and for bad values (which it reports).
func checkIPv4(ps *problems, path, value string) netip.Prefix {
	if value == "" {
		ps.add(CodeMissing, path, "set \"dhcp\" or the router's address on this network, such as \"192.168.10.1/24\"")
		return netip.Prefix{}
	}
	if value == "dhcp" {
		return netip.Prefix{}
	}
	p, err := netip.ParsePrefix(value)
	switch {
	case err != nil || !p.Addr().Is4():
		ps.add(CodeInvalid, path, "%q must be \"dhcp\" or an IPv4 address with its network size, such as \"192.168.10.1/24\"", value)
	case p.Bits() < 8 || p.Bits() > 30:
		ps.add(CodeInvalid, path, "a /%d network won't work here; /24 suits almost every home network", p.Bits())
	case p.Addr() == p.Masked().Addr():
		ps.add(CodeInvalid, path, "%s is the network's own address, which no device can use; try %s", p.Addr(), p.Addr().Next())
	case p.Addr() == lastAddr(p):
		ps.add(CodeInvalid, path, "%s is the network's broadcast address, which no device can use; try %s", p.Addr(), p.Masked().Addr().Next())
	default:
		return p
	}
	return netip.Prefix{}
}

func checkAddr4(ps *problems, path, value string) netip.Addr {
	a, err := netip.ParseAddr(value)
	if err != nil || !a.Is4() {
		ps.add(CodeInvalid, path, "%q isn't an IPv4 address, such as \"192.168.10.100\"", value)
		return netip.Addr{}
	}
	return a
}

func checkMAC(ps *problems, path, value string) {
	if hw, err := net.ParseMAC(value); err != nil || len(hw) != 6 {
		ps.add(CodeInvalid, path, "%q isn't a MAC address, such as \"00:00:5e:00:53:01\"", value)
	}
}

func checkZone(ps *problems, path, zone string) {
	if !nameRe.MatchString(zone) {
		ps.add(CodeInvalid, path, "%q isn't a valid zone name; %s", zone, nameRule)
	}
}

// lastAddr returns the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func validDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if !labelRe.MatchString(label) {
			return false
		}
	}
	return true
}

// validForwarder accepts "ip", "ip@port", "ip#tls-name" and "ip@port#tls-name".
func validForwarder(f string) bool {
	rest, tlsName, hasTLS := strings.Cut(f, "#")
	if hasTLS && !validDomain(tlsName) {
		return false
	}
	ip, port, hasPort := strings.Cut(rest, "@")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	_, err := netip.ParseAddr(ip)
	return err == nil
}

// itemPath names an array entry by its name when it has one, else by position.
func itemPath(prefix string, i int, name string) string {
	if name != "" {
		return fmt.Sprintf("%s %q", prefix, name)
	}
	return fmt.Sprintf("%s #%d", prefix, i+1)
}
