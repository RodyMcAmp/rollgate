package health

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// Env is what the built-in checks look at, so tests can fake each part.
type Env struct {
	Run    func(name string, args ...string) ([]byte, error)
	Config *config.Config // the running config; nil before the first apply
	// DNS looks up name's IPv4 addresses at server ("addr:port").
	DNS func(server, name string) ([]netip.Addr, error)
	// Dial opens and closes a TCP connection to address ("addr:port").
	Dial func(address string) error
}

// Check is one built-in check: whether it passed, and one line saying what it saw.
type Check func(Env) (bool, string)

// Checks are the built-in checks, run as `rollgate check <name>`. The image's
// health.d has one script per check.
var Checks = map[string]Check{
	"firewall": firewallCheck,
	"wan":      wanCheck,
	"dns":      dnsCheck,
	"ssh":      sshCheck,
	"services": servicesCheck,
}

// network is an interface or VLAN, with what the checks need to know.
type network struct {
	name, zone, device string
	addr               netip.Prefix // invalid for DHCP
}

func networks(c *config.Config) []network {
	var nets []network
	zone := func(z, name string) string {
		if z == "" {
			return name
		}
		return z
	}
	for _, in := range c.Interfaces {
		nets = append(nets, network{in.Name, zone(in.Zone, in.Name), in.Device(), in.Addr})
	}
	for _, v := range c.VLANs {
		nets = append(nets, network{v.Name, zone(v.Zone, v.Name), v.Name, v.Addr})
	}
	return nets
}

// firewallCheck: the nftables ruleset is loaded and contains rollgate's table.
func firewallCheck(e Env) (bool, string) {
	out, err := e.Run("nft", "list", "table", "inet", "rollgate")
	if err != nil {
		return false, "the rollgate firewall isn't loaded: " + firstLine(string(out))
	}
	return true, "the rollgate firewall is loaded"
}

// defaultRoutes parses `ip -4 -o route show default` into device → gateway.
func defaultRoutes(out string) map[string]string {
	routes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "default" {
			continue
		}
		var via, dev string
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				via = f[i+1]
			case "dev":
				dev = f[i+1]
			}
		}
		if dev != "" {
			routes[dev] = via
		}
	}
	return routes
}

// wanCheck: the WAN interface has an address and the default route goes
// through it. Before the first apply, any IPv4 default route will do.
func wanCheck(e Env) (bool, string) {
	out, err := e.Run("ip", "-4", "-o", "route", "show", "default")
	if err != nil {
		return false, "couldn't read the routing table: " + firstLine(string(out))
	}
	routes := defaultRoutes(string(out))
	if e.Config == nil {
		for dev, via := range routes {
			return true, fmt.Sprintf("default route via %s on %s (no config applied yet)", via, dev)
		}
		return false, "no IPv4 default route (no config applied yet)"
	}
	var wan *network
	for _, n := range networks(e.Config) {
		if n.zone == "wan" {
			wan = &n
		}
	}
	if wan == nil {
		return false, "the running config has no wan interface"
	}
	out, err = e.Run("ip", "-4", "-o", "addr", "show", "dev", wan.device)
	var addr string
	if f := strings.Fields(string(out)); err == nil {
		if i := slices.Index(f, "inet"); i >= 0 && i+1 < len(f) {
			addr = f[i+1]
		}
	}
	if addr == "" {
		return false, fmt.Sprintf("wan (%s) has no IPv4 address", wan.device)
	}
	via, ok := routes[wan.device]
	if !ok {
		return false, fmt.Sprintf("wan (%s) has %s, but there's no default route through it", wan.device, addr)
	}
	return true, fmt.Sprintf("wan (%s) has %s; default route via %s", wan.device, addr, via)
}

// dnsProbe is answered by Unbound's built-in localhost zone (every name under
// localhost. is 127.0.0.1), so it works without internet access. Asking
// dnsmasq for it proves dnsmasq forwards to Unbound.
const dnsProbe = "rollgate-health.localhost"

// dnsCheck: Unbound answers, and dnsmasq answers on loopback and on every
// network it serves.
func dnsCheck(e Env) (bool, string) {
	if e.Config == nil {
		return true, "skipped: no config applied yet"
	}
	type server struct{ what, addr string }
	servers := []server{{"Unbound", "127.0.0.1:5335"}, {"dnsmasq", "127.0.0.1:53"}}
	var served []string
	for _, n := range networks(e.Config) {
		if _, ok := e.Config.DHCP[n.name]; ok && n.addr.IsValid() {
			servers = append(servers, server{"dnsmasq on " + n.name, netip.AddrPortFrom(n.addr.Addr(), 53).String()})
			served = append(served, n.name)
		}
	}
	for _, s := range servers {
		addrs, err := e.DNS(s.addr, dnsProbe)
		if err != nil {
			return false, fmt.Sprintf("%s (%s) doesn't answer: %v", s.what, s.addr, err)
		}
		if !slices.Contains(addrs, netip.MustParseAddr("127.0.0.1")) {
			return false, fmt.Sprintf("%s (%s) answered %v for %s, expected 127.0.0.1", s.what, s.addr, addrs, dnsProbe)
		}
	}
	if len(served) == 0 {
		return true, "Unbound and dnsmasq answer"
	}
	return true, "Unbound and dnsmasq answer, on " + strings.Join(served, ", ")
}

// sshCheck: sshd accepts connections on each management network.
func sshCheck(e Env) (bool, string) {
	if e.Config == nil {
		if err := e.Dial("127.0.0.1:22"); err != nil {
			return false, fmt.Sprintf("sshd doesn't accept connections: %v", err)
		}
		return true, "sshd accepts connections (no config applied yet)"
	}
	var where []string
	for _, n := range networks(e.Config) {
		if !slices.Contains(e.Config.Management.AllowedZones, n.zone) || !n.addr.IsValid() {
			continue
		}
		addr := netip.AddrPortFrom(n.addr.Addr(), 22).String()
		if err := e.Dial(addr); err != nil {
			return false, fmt.Sprintf("sshd doesn't accept connections on %s (%s): %v", n.name, addr, err)
		}
		where = append(where, fmt.Sprintf("%s (%s)", n.name, n.addr.Addr()))
	}
	if len(where) == 0 {
		return false, "no management network has a static address (management.allowed_zones)"
	}
	return true, "sshd accepts connections on " + strings.Join(where, ", ")
}

// servicesCheck: every long-running service rollgate relies on is running.
// nftables.service isn't one: it loads the ruleset and exits, so it shows as
// inactive by design; the firewall check looks at the loaded ruleset instead.
func servicesCheck(e Env) (bool, string) {
	units := []string{"systemd-networkd.service", "sshd.service"}
	if e.Config != nil {
		units = append(units, "unbound.service", "dnsmasq.service")
	}
	// is-active prints one state per unit and fails if any isn't active.
	out, _ := e.Run("systemctl", append([]string{"is-active"}, units...)...)
	states := strings.Fields(string(out))
	var bad []string
	for i, u := range units {
		state := "unknown"
		if i < len(states) {
			state = states[i]
		}
		if state != "active" {
			bad = append(bad, fmt.Sprintf("%s (%s)", u, state))
		}
	}
	if len(bad) > 0 {
		return false, "not running: " + strings.Join(bad, ", ")
	}
	return true, fmt.Sprintf("all %d services running", len(units))
}
