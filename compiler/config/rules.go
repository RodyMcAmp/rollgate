package config

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// segment is one network the router is attached to: an interface or a VLAN.
type segment struct {
	path string // as used in problems, e.g. `vlan "iot"`
	name string
	zone string
	addr netip.Prefix // invalid for "dhcp" or when the value was already reported
}

// validateRules checks how values relate to each other (E001–E006). It runs
// after validateFields and skips values that were already reported as invalid,
// so one mistake produces one problem.
func validateRules(c *Config, ps *problems) {
	var segs []segment
	byName := map[string]segment{}
	zones := map[string]bool{}
	for i, in := range c.Interfaces {
		s := segment{itemPath("interface", i, in.Name), in.Name, in.Zone, in.Addr}
		segs = append(segs, s)
		byName[in.Name] = s
		zones[in.Zone] = true
	}
	interfaces := maps.Clone(byName)
	for i, v := range c.VLANs {
		s := segment{itemPath("vlan", i, v.Name), v.Name, v.Zone, v.Addr}
		segs = append(segs, s)
		byName[v.Name] = s
		zones[v.Zone] = true
		if _, ok := interfaces[v.Parent]; v.Parent != "" && !ok {
			ps.add(CodeUnknownName, s.path+" parent", "there is no interface named %q", v.Parent)
		}
	}

	// E011: each port is matched once, and every segment gets its own device.
	devices := map[string]string{}
	macs := map[string]string{}
	for i, in := range c.Interfaces {
		path := itemPath("interface", i, in.Name)
		if hw, err := net.ParseMAC(in.MatchMAC); err == nil {
			if other, ok := macs[hw.String()]; ok {
				ps.add(CodeDuplicate, path+" match_mac", "%s already matches this MAC address", other)
			}
			macs[hw.String()] = path
		}
		claimDevice(ps, devices, path, in.Device())
	}
	for i, v := range c.VLANs {
		claimDevice(ps, devices, itemPath("vlan", i, v.Name), v.Name)
	}

	// E001: exactly one WAN, using DHCP until the spec has a gateway setting.
	var wans []segment
	for _, s := range segs {
		if s.zone == "wan" {
			wans = append(wans, s)
			if s.addr.IsValid() {
				ps.add(CodeInvalid, s.path+" ipv4", "a static WAN address isn't supported yet (it also needs a gateway setting); use \"dhcp\"")
			}
		}
	}
	switch {
	case len(wans) == 0:
		ps.add(CodeOneWAN, "interface", "no interface is in the zone \"wan\"; add one named \"wan\" for the port that connects to your ISP's modem")
	case len(wans) > 1:
		for _, s := range wans[1:] {
			ps.add(CodeOneWAN, s.path, "only one WAN is supported; %s is already in the zone \"wan\"", wans[0].path)
		}
	}

	// E002: no overlapping subnets.
	for i, a := range segs {
		for _, b := range segs[:i] {
			if a.addr.IsValid() && b.addr.IsValid() && a.addr.Overlaps(b.addr) {
				ps.add(CodeOverlap, a.path+" ipv4", "%s overlaps %s on %s; give each network its own range, such as %s",
					a.addr.Masked(), b.addr.Masked(), b.path, suggestSubnet(segs))
			}
		}
	}

	// E003: names that must exist.
	zoneList := strings.Join(slices.Sorted(maps.Keys(zones)), ", ")
	for _, z := range c.Management.AllowedZones {
		if z != "wan" && nameRe.MatchString(z) && !zones[z] {
			ps.add(CodeUnknownName, "management.allowed_zones", "there is no zone %q; zones are: %s", z, zoneList)
		}
	}
	for i, p := range c.Firewall.Policy {
		for _, end := range []struct{ key, zone string }{{"from", p.From}, {"to", p.To}} {
			if end.zone != "*" && nameRe.MatchString(end.zone) && !zones[end.zone] {
				ps.add(CodeUnknownName, fmt.Sprintf("firewall.policy #%d %s", i+1, end.key), "there is no zone %q; zones are: %s", end.zone, zoneList)
			}
		}
	}

	// E004: the router stays manageable. Skipped when a zone name was already
	// reported, so a typo produces one clear problem instead of two.
	namesOK := len(c.Management.AllowedZones) > 0
	for _, z := range c.Management.AllowedZones {
		namesOK = namesOK && zones[z] && z != "wan"
	}
	reachable := false
	for _, s := range segs {
		if s.addr.IsValid() && slices.Contains(c.Management.AllowedZones, s.zone) {
			reachable = true
		}
	}
	if namesOK && !reachable {
		ps.add(CodeManagement, "management.allowed_zones", "none of the zones %q has an interface with a static address, so you couldn't reach the router to manage it",
			c.Management.AllowedZones)
	}

	// E005: DHCP.
	for _, name := range slices.Sorted(maps.Keys(c.DHCP)) {
		path := "dhcp." + name
		s, ok := byName[name]
		switch {
		case !nameRe.MatchString(name):
			continue // already reported
		case !ok:
			ps.add(CodeUnknownName, path, "there is no interface or VLAN named %q", name)
		case s.zone == "wan":
			ps.add(CodeDHCP, path, "the router must never hand out addresses on the internet side")
		case !s.addr.IsValid():
			ps.add(CodeDHCP, path, "%s needs a static ipv4 address, such as \"192.168.10.1/24\", to serve DHCP", s.path)
		default:
			checkDHCP(ps, path, s.addr, c.DHCP[name])
		}
	}

	// E006: port forwards.
	claimed := map[string]string{} // "tcp/80" -> port forward name
	for i, pf := range c.Firewall.PortForward {
		path := itemPath("firewall.port_forward", i, pf.Name)
		if t := pf.Target.Addr(); t.IsValid() {
			inside := false
			for _, s := range segs {
				if s.zone != "wan" && s.addr.IsValid() && s.addr.Contains(t) {
					inside = true
					if t == s.addr.Addr() || !usable(s.addr, t) {
						ps.add(CodePortForward, path+" to", "%s is the router's own address or not a device address on %s; forward to a device instead", t, s.path)
					}
				}
			}
			if !inside {
				ps.add(CodePortForward, path+" to", "%s isn't on any of your local networks; forward to a device on one of them", t)
			}
		}
		if pf.WANPortFirst == 0 {
			continue
		}
		protocols := strings.Split(pf.Protocol, "+")
		for _, proto := range protocols {
			for port := pf.WANPortFirst; port <= pf.WANPortLast; port++ {
				key := fmt.Sprintf("%s/%d", proto, port)
				if other, ok := claimed[key]; ok && other != pf.Name {
					ps.add(CodePortForward, path+" wan_port", "%s port %d is already forwarded by %q", proto, port, other)
					break
				}
				claimed[key] = pf.Name
			}
		}
	}
}

// checkDHCP checks a DHCP section against the subnet it serves.
func checkDHCP(ps *problems, path string, subnet netip.Prefix, d DHCP) {
	router := subnet.Addr()
	inRange := func(a netip.Addr) bool {
		return d.RangeStart.IsValid() && d.RangeEnd.IsValid() && !a.Less(d.RangeStart) && !d.RangeEnd.Less(a)
	}
	for _, a := range []netip.Addr{d.RangeStart, d.RangeEnd} {
		if a.IsValid() && !usable(subnet, a) {
			ps.add(CodeDHCP, path+" range", "%s isn't a device address in %s; use addresses from %s to %s",
				a, subnet.Masked(), subnet.Masked().Addr().Next(), lastAddr(subnet).Prev())
		}
	}
	if inRange(router) {
		ps.add(CodeDHCP, path+" range", "the range includes the router's own address %s; start the range after it", router)
	}
	for i, s := range d.Static {
		if !s.Addr.IsValid() {
			continue
		}
		spath := itemPath(path+" static", i, s.Hostname) + " ipv4"
		switch {
		case !usable(subnet, s.Addr):
			ps.add(CodeDHCP, spath, "%s isn't a device address in %s", s.Addr, subnet.Masked())
		case s.Addr == router:
			ps.add(CodeDHCP, spath, "%s is the router's own address", s.Addr)
		case inRange(s.Addr):
			ps.add(CodeDHCP, spath, "%s is inside the range %s–%s; pick an address outside it, so no other device can get it",
				s.Addr, d.RangeStart, d.RangeEnd)
		}
	}
}

// claimDevice reports a device name that two segments would both use, such as
// an interface named "wan" and another one with match_device = "wan".
func claimDevice(ps *problems, devices map[string]string, path, device string) {
	if device == "" {
		return
	}
	if other, ok := devices[device]; ok && other != path {
		ps.add(CodeDuplicate, path, "would use the network device %q, which %s already uses", device, other)
	}
	devices[device] = path
}

// usable reports whether a is a device address in p: inside it, and neither
// the network address nor the broadcast address.
func usable(p netip.Prefix, a netip.Addr) bool {
	return p.Contains(a) && a != p.Masked().Addr() && a != lastAddr(p)
}

// suggestSubnet proposes a free 192.168.N.1/24 for an overlap message.
func suggestSubnet(segs []segment) string {
	for n := 2; n < 255; n++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(n), 1}), 24)
		free := true
		for _, s := range segs {
			if s.addr.IsValid() && s.addr.Overlaps(p) {
				free = false
			}
		}
		if free {
			return p.String()
		}
	}
	return "10.20.30.1/24"
}
