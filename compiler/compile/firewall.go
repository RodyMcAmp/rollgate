package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// firewall is the template data for nftables.tmpl.
type firewall struct {
	Header        string
	Zones         []zone
	WAN           []string // devices in the zone "wan"
	Services      []string // devices with a [dhcp.<name>] section: DHCP and DNS allowed
	Management    []string // devices in management.allowed_zones: SSH allowed
	IPv6Off       bool
	DefaultPolicy bool // no [[firewall.policy]] given
	Policies      []rule
	PortForwards  []forward
}

type zone struct {
	Name    string
	Devices []string
}

type rule struct {
	Comment string
	From    []string // devices; never contains a WAN device when Verdict is accept
	To      []string // devices; nil means any
	Verdict string
}

type forward struct {
	Name      string
	Protocols []string // "tcp", "udp"
	WANPorts  string   // "27015" or "27015-27030"
	Target    string   // "ip:port", or "ip" for a range (ports are kept)
}

var verdicts = map[string]string{
	"accept": "accept",
	"drop":   "drop",
	"reject": "reject with icmpx admin-prohibited",
}

func buildFirewall(c *config.Config, header string) (firewall, error) {
	fw := firewall{Header: header, IPv6Off: c.System.IPv6 == "off"}

	devicesOf := map[string][]string{}
	deviceByName := map[string]string{}
	addDevice := func(name, zoneName, device string) {
		if _, ok := devicesOf[zoneName]; !ok {
			fw.Zones = append(fw.Zones, zone{Name: zoneName})
		}
		devicesOf[zoneName] = append(devicesOf[zoneName], device)
		deviceByName[name] = device
	}
	for _, in := range c.Interfaces {
		addDevice(in.Name, in.Zone, in.Device())
	}
	for _, v := range c.VLANs {
		addDevice(v.Name, v.Zone, v.Name)
	}
	var local []string
	for i, z := range fw.Zones {
		fw.Zones[i].Devices = devicesOf[z.Name]
		if z.Name != "wan" {
			local = append(local, devicesOf[z.Name]...)
		}
	}
	fw.WAN = devicesOf["wan"]

	for _, name := range append(namesOf(c.Interfaces), vlanNames(c.VLANs)...) {
		if _, ok := c.DHCP[name]; ok {
			fw.Services = append(fw.Services, deviceByName[name])
		}
	}
	for _, z := range fw.Zones {
		if slices.Contains(c.Management.AllowedZones, z.Name) {
			fw.Management = append(fw.Management, z.Devices...)
		}
	}

	if len(c.Firewall.Policy) == 0 {
		fw.DefaultPolicy = true
		fw.Policies = []rule{{From: local, To: fw.WAN, Verdict: "accept"}}
	}
	for i, p := range c.Firewall.Policy {
		r := rule{Comment: p.Comment, From: devicesOf[p.From], To: devicesOf[p.To], Verdict: verdicts[p.Action]}
		if p.From == "*" {
			r.From = local // "*" never includes WAN (docs/config-spec.md#firewall)
		}
		if p.To == "*" {
			r.To = nil
		}
		// Defense in depth for AGENTS.md hard rule 5; validation already rejects this.
		if p.Action == "accept" && slices.ContainsFunc(r.From, func(d string) bool { return slices.Contains(fw.WAN, d) }) {
			return fw, fmt.Errorf("firewall.policy #%d would accept traffic from WAN; refusing to compile it", i+1)
		}
		if r.Verdict == "" || len(r.From) == 0 {
			return fw, fmt.Errorf("firewall.policy #%d: unexpected action %q or zone %q (config not validated?)", i+1, p.Action, p.From)
		}
		fw.Policies = append(fw.Policies, r)
	}

	for _, pf := range c.Firewall.PortForward {
		f := forward{
			Name:      pf.Name,
			Protocols: strings.Split(pf.Protocol, "+"),
			WANPorts:  fmt.Sprint(pf.WANPortFirst),
			Target:    pf.Target.String(),
		}
		if pf.WANPortLast != pf.WANPortFirst {
			f.WANPorts = fmt.Sprintf("%d-%d", pf.WANPortFirst, pf.WANPortLast)
			f.Target = pf.Target.Addr().String()
		}
		fw.PortForwards = append(fw.PortForwards, f)
	}
	return fw, nil
}

func namesOf(ins []config.Interface) []string {
	names := make([]string, len(ins))
	for i, in := range ins {
		names[i] = in.Name
	}
	return names
}

func vlanNames(vs []config.VLAN) []string {
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = v.Name
	}
	return names
}

// Template helpers for nftables syntax.
var funcs = map[string]any{
	// set renders device names as an nftables set: { "lan", "iot" }
	"set": func(names []string) string {
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		return "{ " + strings.Join(quoted, ", ") + " }"
	},
	// list renders keywords: tcp, or { tcp, udp }
	"list": func(words []string) string {
		if len(words) == 1 {
			return words[0]
		}
		return "{ " + strings.Join(words, ", ") + " }"
	},
	"join": strings.Join,
}
