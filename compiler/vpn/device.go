package vpn

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NextAddress is the first address in p after the router's that no device
// has, for a new device.
func NextAddress(p netip.Prefix, used []netip.Addr) (netip.Addr, error) {
	taken := map[netip.Addr]bool{p.Addr(): true}
	for _, a := range used {
		taken[a] = true
	}
	last := broadcast(p)
	for a := p.Masked().Addr().Next(); a.IsValid() && a.Less(last); a = a.Next() {
		if !taken[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("the VPN's addresses (%s) are all taken; make vpn.address bigger", p.Masked())
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= 1<<(32-p.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Device is what a device's WireGuard configuration needs.
type Device struct {
	Name       string
	PrivateKey string
	Address    netip.Addr
	DNS        netip.Addr // the router's address in the VPN
	RouterKey  string     // the router's public key
	Endpoint   string     // host:port
	AllowedIPs []string   // what goes through the tunnel
}

// Config is the device's configuration file, as the WireGuard apps import it.
func (d Device) Config() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# rollgate VPN: %s. Import this into the WireGuard app; it holds the\n# device's private key, so keep it private.\n", d.Name)
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nAddress = %s/32\nDNS = %s\n\n", d.PrivateKey, d.Address, d.DNS)
	fmt.Fprintf(&b, "[Peer]\nPublicKey = %s\nEndpoint = %s\nAllowedIPs = %s\n", d.RouterKey, d.Endpoint, strings.Join(d.AllowedIPs, ", "))
	// Keep NAT mappings on the device's network open, so the router can reach it.
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}

// Everything is AllowedIPs for "everything through the tunnel". IPv6 isn't
// carried yet; ::/0 keeps it from leaking around the tunnel.
var Everything = []string{"0.0.0.0/0", "::/0"}

// sharedNets are addresses no one on the internet can reach the router at.
var sharedNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
}

// Endpoint picks the address devices connect to: configured (vpn.endpoint),
// or the WAN address the router has now. The warning, if any, is for the user.
func Endpoint(configured string, wan netip.Addr, port int) (string, string, error) {
	p := strconv.Itoa(port)
	if configured != "" {
		return joinHostPort(configured, p), "", nil
	}
	if !wan.IsValid() {
		return "", "", errors.New("the router has no WAN address right now, and vpn.endpoint isn't set; set vpn.endpoint to a name that points at your home")
	}
	for _, n := range sharedNets {
		if n.Contains(wan) {
			return joinHostPort(wan.String(), p), fmt.Sprintf("The router's WAN address (%s) is a private or shared one: your ISP, or another router in front of this one, sits between it and the internet. "+
				"Devices outside can only connect if that box forwards UDP port %d to this router; with carrier-grade NAT (100.64.x.x) your ISP has to help.", wan, port), nil
		}
	}
	return joinHostPort(wan.String(), p), fmt.Sprintf("Devices will connect to %s, the router's address now. Most home connections get a new address now and then; "+
		"set vpn.endpoint to a dynamic DNS name that follows it, so devices keep working.", wan), nil
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// Handshakes reads `wg show <dev> latest-handshakes`: public key, tab, Unix
// time (0 for never), one per line.
func Handshakes(out string) map[string]time.Time {
	m := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		key, ts, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); err == nil && n > 0 {
			m[key] = time.Unix(n, 0)
		}
	}
	return m
}

// Ago says how long ago t was, as a person reads it: "never", "2 min ago".
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// HomeNets is AllowedIPs for "only my home networks": the given networks
// and the VPN, without duplicates, sorted.
func HomeNets(nets []netip.Prefix) []string {
	set := map[string]bool{}
	for _, n := range nets {
		set[n.Masked().String()] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
