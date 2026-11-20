package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/edit"
	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

// The Devices page: what has an address from the router now (its DHCP
// leases), and the devices with a fixed address ([[dhcp.<network>.static]]),
// which can be added, changed and removed. Changes go the same way as on
// the Port forwarding page: compiler/edit, a review, then Apply, then Keep.

// network is a network the router serves DHCP on.
type network struct {
	Name                 string
	Prefix               netip.Prefix
	Router               netip.Addr
	RangeStart, RangeEnd netip.Addr
}

func (n network) inRange(a netip.Addr) bool {
	return n.RangeStart.IsValid() && n.RangeStart.Compare(a) <= 0 && a.Compare(n.RangeEnd) <= 0
}

type fixedRow struct {
	Network                string
	Index                  int // in the network's list
	Hostname, MAC, Address string
	Online                 bool
}

type leaseRow struct {
	Name, Address, MAC, Network, Expires string
	Fixed                                bool
}

// deviceForm is the add or edit form, as typed.
type deviceForm struct {
	Index       int // in OrigNetwork's list; -1 for a new one
	OrigName    string
	OrigNetwork string
	Network     string
	Hostname    string
	MAC         string
	Address     string
}

// loadDevices reads the networks and fixed addresses from router.toml, and
// matches the leases to them.
func loadDevices(source string, leases []sysinfo.Lease, now time.Time) ([]network, []fixedRow, []leaseRow) {
	var c config.Config
	_, _ = toml.Decode(source, &c)
	prefixes := map[string]string{}
	for _, in := range c.Interfaces {
		prefixes[in.Name] = in.IPv4
	}
	for _, v := range c.VLANs {
		prefixes[v.Name] = v.IPv4
	}
	var nets []network
	for name, d := range c.DHCP {
		n := network{Name: name}
		if p, err := netip.ParsePrefix(prefixes[name]); err == nil {
			n.Prefix, n.Router = p.Masked(), p.Addr()
		}
		if len(d.Range) == 2 {
			n.RangeStart, _ = netip.ParseAddr(d.Range[0])
			n.RangeEnd, _ = netip.ParseAddr(d.Range[1])
		}
		nets = append(nets, n)
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })

	online := map[string]bool{}
	for _, l := range leases {
		online[l.MAC] = true
	}
	var fixed []fixedRow
	fixedMAC := map[string]bool{}
	for _, n := range nets {
		for i, s := range c.DHCP[n.Name].Static {
			mac := strings.ToLower(s.MAC)
			fixed = append(fixed, fixedRow{Network: n.Name, Index: i, Hostname: s.Hostname, MAC: mac, Address: s.IPv4, Online: online[mac]})
			fixedMAC[mac] = true
		}
	}
	var rows []leaseRow
	for _, l := range leases {
		r := leaseRow{Name: l.Hostname, Address: l.IPv4, MAC: l.MAC, Expires: until(l.Expires, now), Fixed: fixedMAC[l.MAC]}
		if a, err := netip.ParseAddr(l.IPv4); err == nil {
			for _, n := range nets {
				if n.Prefix.IsValid() && n.Prefix.Contains(a) {
					r.Network = n.Name
				}
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := netip.ParseAddr(rows[i].Address)
		b, _ := netip.ParseAddr(rows[j].Address)
		return a.Less(b)
	})
	return nets, fixed, rows
}

// until says when a lease ends, as a person reads it.
func until(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "ended"
	case d < time.Hour:
		return fmt.Sprintf("in %d min", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("in %d h", int(d.Hours()))
	}
	return fmt.Sprintf("in %d days", int(d.Hours()/24))
}

// freeAddress suggests an address on n for a fixed address: outside the DHCP
// range, not the router's, and not taken by a fixed address or a lease.
func freeAddress(n network, fixed []fixedRow, leases []leaseRow) string {
	if !n.Prefix.IsValid() || !n.Prefix.Addr().Is4() {
		return ""
	}
	taken := map[netip.Addr]bool{n.Router: true}
	for _, f := range fixed {
		if a, err := netip.ParseAddr(f.Address); err == nil {
			taken[a] = true
		}
	}
	for _, l := range leases {
		if a, err := netip.ParseAddr(l.Address); err == nil {
			taken[a] = true
		}
	}
	last := lastAddr(n.Prefix)
	for a := n.Prefix.Addr().Next(); a.IsValid() && a.Less(last); a = a.Next() {
		if !taken[a] && !n.inRange(a) {
			return a.String()
		}
	}
	return ""
}

// lastAddr is a prefix's broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= (1<<host - 1)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func (s *Server) leases(ctx context.Context) []sysinfo.Lease {
	resp, err := s.API.Get(ctx, "/v1/leases")
	if err != nil || !resp.OK {
		return nil // the page still shows the fixed addresses
	}
	return resp.Leases
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "devices")
	p.Nav = "devices"
	cur, ok := s.currentConfig(r.Context(), p)
	if !ok {
		s.render(w, http.StatusOK, "devices.html", p)
		return
	}
	p.Networks, p.Fixed, p.Online = loadDevices(cur, s.leases(r.Context()), s.now())
	p.DevForm = deviceForm{Index: -1}
	for i, n := range p.Networks { // lan if there is one, else the first
		if i == 0 || n.Name == "lan" {
			p.DevForm.Network = n.Name
		}
	}
	q := r.URL.Query()
	if mac := strings.ToLower(q.Get("fix")); mac != "" {
		for _, l := range p.Online {
			if l.MAC == mac {
				p.DevForm.MAC, p.DevForm.Hostname = l.MAC, hostLabel(l.Name)
				if l.Network != "" {
					p.DevForm.Network = l.Network
				}
			}
		}
	}
	if netName, idx, ok := strings.Cut(q.Get("edit"), ":"); ok {
		i, _ := strconv.Atoi(idx)
		for _, f := range p.Fixed {
			if f.Network == netName && f.Index == i {
				p.DevForm = deviceForm{Index: i, OrigName: f.Hostname, OrigNetwork: netName, Network: netName,
					Hostname: f.Hostname, MAC: f.MAC, Address: f.Address}
			}
		}
	}
	if p.DevForm.Address == "" {
		for _, n := range p.Networks {
			if n.Name == p.DevForm.Network {
				p.DevForm.Address = freeAddress(n, p.Fixed, p.Online)
			}
		}
	}
	s.render(w, http.StatusOK, "devices.html", p)
}

// devices is the form: review a new, changed or removed fixed address.
func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	p := s.newPage(ctx, sess, "devices")
	p.Nav = "devices"
	f := deviceForm{Index: -1, OrigName: r.PostFormValue("orig_name"), OrigNetwork: r.PostFormValue("orig_network"),
		Network: r.PostFormValue("network"), Hostname: strings.TrimSpace(r.PostFormValue("hostname")),
		MAC: strings.TrimSpace(r.PostFormValue("mac")), Address: strings.TrimSpace(r.PostFormValue("address"))}
	if i, err := strconv.Atoi(r.PostFormValue("index")); err == nil && i >= 0 {
		f.Index = i
	}
	p.DevForm = f
	cur, ok := s.currentConfig(ctx, p)
	if !ok {
		s.render(w, http.StatusOK, "devices.html", p)
		return
	}
	p.Networks, p.Fixed, p.Online = loadDevices(cur, s.leases(ctx), s.now())

	var edits []edit.Edit
	if r.PostFormValue("action") == "remove" {
		if f.Index < 0 || f.OrigNetwork == "" {
			p.Error = "Choose a device to remove."
			s.render(w, http.StatusOK, "devices.html", p)
			return
		}
		edits = append(edits, edit.Remove("dhcp."+f.OrigNetwork+".static", f.Index, "hostname", f.OrigName))
		p.Removing = f.OrigName
		p.DevForm = deviceForm{Index: -1, Network: f.OrigNetwork}
	} else {
		fields, err := f.fields(p.Networks)
		if err != nil {
			p.Error = err.Error()
			s.render(w, http.StatusOK, "devices.html", p)
			return
		}
		table := "dhcp." + f.Network + ".static"
		switch {
		case f.Index < 0:
			edits = append(edits, edit.Add(table, fields...))
		case f.OrigNetwork == f.Network:
			edits = append(edits, edit.Change(table, f.Index, "hostname", f.OrigName, fields))
		default: // moved to another network
			edits = append(edits, edit.Remove("dhcp."+f.OrigNetwork+".static", f.Index, "hostname", f.OrigName), edit.Add(table, fields...))
		}
	}
	text, err := edit.Apply([]byte(cur), edits...)
	if err != nil {
		var refusal *edit.Refusal
		switch {
		case errors.As(err, &refusal):
			p.Error, p.Advanced = refusal.Reason, true
		case errors.Is(err, edit.ErrStale):
			p.Error = "router.toml changed on the router since this page loaded it (over SSH, or from another browser). Here it is as it is now: make your change again."
			p.DevForm = deviceForm{Index: -1, Network: f.Network}
		default:
			p.Error = err.Error()
		}
		s.render(w, http.StatusOK, "devices.html", p)
		return
	}
	p.Config, p.Base = string(text), digest(cur)
	s.check(ctx, p)
	p.Config64 = base64.StdEncoding.EncodeToString(text)
	p.Reviewing = true
	s.render(w, http.StatusOK, "devices.html", p)
}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// hostLabel turns what a device calls itself ("Maya's iPhone") into a name
// the network can use ("mayas-iphone").
func hostLabel(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == ' ' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// fields checks the form and turns it into router.toml's keys. The router's
// own validation (inside the network, outside the range, no repeats) runs
// after this, on the whole file.
func (f deviceForm) fields(nets []network) ([]edit.Field, error) {
	var n *network
	for i := range nets {
		if nets[i].Name == f.Network {
			n = &nets[i]
		}
	}
	if n == nil {
		return nil, mistake("Choose the network the device is on.")
	}
	host := strings.ToLower(f.Hostname)
	if !hostPattern.MatchString(host) {
		return nil, mistake("A name is letters, digits and dashes, such as \"living-room-tv\", up to 63 characters, starting and ending with a letter or digit.")
	}
	mac, err := net.ParseMAC(f.MAC)
	if err != nil || len(mac) != 6 {
		return nil, mistake("The hardware (MAC) address looks like 52:54:00:12:34:56. The device's network settings show it, or pick the device from the list above.")
	}
	addr, err := netip.ParseAddr(f.Address)
	if err != nil || !addr.Is4() {
		return nil, mistake("Type an IPv4 address, such as 192.168.10.20.")
	}
	if n.Prefix.IsValid() && !n.Prefix.Contains(addr) {
		return nil, mistake(fmt.Sprintf("%s isn't on the %s network (%s).", addr, n.Name, n.Prefix))
	}
	if n.inRange(addr) {
		return nil, mistake(fmt.Sprintf("%s is in the range the router hands out by itself (%s to %s). Choose one outside it.", addr, n.RangeStart, n.RangeEnd))
	}
	return []edit.Field{
		{Key: "hostname", Value: host},
		{Key: "mac", Value: mac.String()},
		{Key: "ipv4", Value: addr.String()},
	}, nil
}
