package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/edit"
	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/vpn"
)

// vpnCmd is rollgate vpn (ADR 0018): the router's key, the VPN's state, and
// adding and removing devices.
func vpnCmd(args []string, stdout, stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate vpn: must run as root")
		return 1
	}
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	var err error
	switch {
	case sub == "status" && len(args) == 0:
		err = vpnStatus(system(stdout), stdout)
	case sub == "key" && len(args) == 0:
		var pub string
		var created bool
		if pub, created, err = vpn.EnsureKey("/"); err == nil {
			if created {
				fmt.Fprintf(stdout, "Made the router's VPN key (/%s).\n", vpn.KeyFile)
			}
			fmt.Fprintf(stdout, "The router's VPN public key: %s\n", pub)
		}
	case sub == "add":
		fs := flag.NewFlagSet("vpn add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		homeOnly := fs.Bool("home-only", false, "send only traffic for the home networks through the VPN, not everything")
		if fs.Parse(args) != nil || fs.NArg() != 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		err = vpnAdd(system(stdout), fs.Arg(0), *homeOnly, stdout, stderr)
	case sub == "remove" && len(args) == 1:
		err = vpnRemove(system(stdout), args[0], stderr)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate vpn %s: %v\n", sub, err)
		return 1
	}
	return 0
}

// vpnConfig reads router.toml, which must be valid and have a [vpn] section.
func vpnConfig() ([]byte, *config.Config, error) {
	source, err := os.ReadFile(config.DefaultPath)
	if err != nil {
		return nil, nil, err
	}
	c, err := config.Parse(source)
	if err != nil {
		return nil, nil, fmt.Errorf("router.toml has mistakes; fix them first (rollgate validate):\n%w", err)
	}
	if c.VPN == nil {
		return nil, nil, errors.New("router.toml has no [vpn] section; add one first (docs/config-spec.md, [vpn])")
	}
	return source, c, nil
}

// changeVPN writes text as router.toml and applies it with commit-confirm. If
// the apply doesn't go through, router.toml goes back to old.
func changeVPN(s apply.System, old, text []byte, stderr io.Writer) error {
	if err := writeConfig(config.DefaultPath, text); err != nil {
		return err
	}
	if err := applyFile(s, config.DefaultPath, stderr); err != nil {
		if _, pending, _ := (apply.Store{Root: s.Root}).Pending(); !pending {
			_ = writeConfig(config.DefaultPath, old)
		}
		return err
	}
	return nil
}

func vpnAdd(s apply.System, name string, homeOnly bool, stdout, stderr io.Writer) error {
	source, c, err := vpnConfig()
	if err != nil {
		return err
	}
	routerKey, _, err := vpn.EnsureKey("/")
	if err != nil {
		return err
	}
	var used []netip.Addr
	for _, p := range c.VPN.Peers {
		if p.Name == name {
			return fmt.Errorf("a device named %q is already in the VPN", name)
		}
		used = append(used, p.Addr)
	}
	addr, err := vpn.NextAddress(c.VPN.Addr, used)
	if err != nil {
		return err
	}
	endpoint, warning, err := vpn.Endpoint(c.VPN.Endpoint, wanAddress(c), c.VPN.Port)
	if err != nil {
		return err
	}
	priv, pub, err := vpn.GenerateKey()
	if err != nil {
		return err
	}
	text, err := edit.Apply(source, edit.Add("vpn.peer",
		edit.Field{Key: "name", Value: name}, edit.Field{Key: "public_key", Value: pub}, edit.Field{Key: "ipv4", Value: addr.String()}))
	if err != nil {
		return err
	}
	if err := changeVPN(s, source, text, stderr); err != nil {
		return err
	}
	allowed := vpn.Everything
	if homeOnly {
		allowed = vpn.HomeNets(homePrefixes(c))
	}
	d := vpn.Device{Name: name, PrivateKey: priv, Address: addr, DNS: c.VPN.Addr.Addr(), RouterKey: routerKey, Endpoint: endpoint, AllowedIPs: allowed}
	fmt.Fprintf(stdout, "\nAdded %s to the VPN as %s. Its configuration is below, shown only now: import it into the device's WireGuard app (or save it as %s.conf). "+
		"It works once you keep the change (rollgate confirm).\n\n%s", name, addr, name, d.Config())
	if warning != "" {
		fmt.Fprintf(stdout, "\nNote: %s\n", warning)
	}
	return nil
}

func vpnRemove(s apply.System, name string, stderr io.Writer) error {
	source, c, err := vpnConfig()
	if err != nil {
		return err
	}
	for i, p := range c.VPN.Peers {
		if p.Name == name {
			text, err := edit.Apply(source, edit.Remove("vpn.peer", i, "name", name))
			if err != nil {
				return err
			}
			return changeVPN(s, source, text, stderr)
		}
	}
	return fmt.Errorf("no device named %q is in the VPN", name)
}

// vpnStatus is rollgate vpn: the running VPN and when each device last
// connected.
func vpnStatus(s apply.System, w io.Writer) error {
	c, err := health.RunningConfig(s.Root)
	if err != nil {
		return err
	}
	if c == nil || c.VPN == nil {
		fmt.Fprintln(w, "The VPN is off: the running config has no [vpn] section.")
		return nil
	}
	fmt.Fprintf(w, "VPN: on, UDP port %d, devices reach %s.\n", c.VPN.Port, map[string]string{"everything": "everything", "internet": "the internet only"}[c.VPN.Reach])
	if pub, ok, err := vpn.RouterPublicKey(s.Root); err == nil && ok {
		fmt.Fprintf(w, "Router's public key: %s\n", pub)
	}
	if endpoint, warning, err := vpn.Endpoint(c.VPN.Endpoint, wanAddress(c), c.VPN.Port); err == nil {
		fmt.Fprintf(w, "Devices connect to: %s\n", endpoint)
		if warning != "" {
			fmt.Fprintf(w, "  Note: %s\n", warning)
		}
	}
	out, _ := s.Run.Run("wg", "show", config.VPNZone, "latest-handshakes")
	seen := vpn.Handshakes(string(out))
	if len(c.VPN.Peers) == 0 {
		fmt.Fprintln(w, "Devices: none yet (rollgate vpn add NAME).")
		return nil
	}
	fmt.Fprintln(w, "Devices:")
	for _, p := range c.VPN.Peers {
		fmt.Fprintf(w, "  %-20s %-15s last connected %s\n", p.Name, p.IPv4, vpn.Ago(seen[p.PublicKey], time.Now()))
	}
	return nil
}

// wanAddress is the router's IPv4 address on WAN now, if it has one.
func wanAddress(c *config.Config) netip.Addr {
	for _, in := range c.Interfaces {
		if in.Zone != "wan" {
			continue
		}
		for _, a := range interfaceAddrs(in.Device()) {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
				return p.Addr()
			}
		}
	}
	return netip.Addr{}
}

// homePrefixes are the home networks and the VPN, for "only my home networks".
func homePrefixes(c *config.Config) []netip.Prefix {
	var out []netip.Prefix
	for _, in := range c.Interfaces {
		if in.Zone != "wan" && in.Addr.IsValid() {
			out = append(out, in.Addr)
		}
	}
	for _, v := range c.VLANs {
		if v.Zone != "wan" && v.Addr.IsValid() {
			out = append(out, v.Addr)
		}
	}
	if c.VPN != nil && c.VPN.Addr.IsValid() {
		out = append(out, c.VPN.Addr)
	}
	return out
}
