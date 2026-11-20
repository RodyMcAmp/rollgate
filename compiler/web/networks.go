package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/edit"
)

// The Networks page: the router's networks (its ports and VLANs), what each
// may reach, and a form to add a network: a VLAN with its own addresses and
// DHCP, and the firewall rule that lets it out (ADR 0017). Removing or
// rearranging networks is left to the Advanced editor for now.

type netRow struct {
	Name, Kind, Address, DHCP string
	Devices                   int
	Internet                  bool
	Reaches                   []string // other networks it may reach
	Management                bool
}

// netForm is the add form, as typed.
type netForm struct {
	Name, Parent, ID, Address, RangeStart, RangeEnd string
	DHCP                                            bool
	Reach                                           string // "internet" or "all"
}

// netState is what the page and the form need from router.toml.
type netState struct {
	rows     []netRow
	parents  []string // ports a VLAN can go on: non-WAN, with a static address
	names    map[string]bool
	ids      map[string]map[int]bool // parent -> VLAN ids in use
	prefixes []netip.Prefix
	policies []config.Policy
}

func loadNetworks(source string, leasesByAddr []netip.Addr) netState {
	var c config.Config
	_, _ = toml.Decode(source, &c)
	st := netState{names: map[string]bool{}, ids: map[string]map[int]bool{}, policies: c.Firewall.Policy}
	type entry struct{ name, zone, ipv4, kind string }
	var entries []entry
	for _, in := range c.Interfaces {
		zone := in.Zone
		if zone == "" {
			zone = in.Name
		}
		st.names[in.Name], st.names[zone] = true, true
		if zone == "wan" {
			continue
		}
		entries = append(entries, entry{in.Name, zone, in.IPv4, "port"})
		if _, err := netip.ParsePrefix(in.IPv4); err == nil {
			st.parents = append(st.parents, in.Name)
		}
	}
	for _, v := range c.VLANs {
		zone := v.Zone
		if zone == "" {
			zone = v.Name
		}
		st.names[v.Name], st.names[zone] = true, true
		if st.ids[v.Parent] == nil {
			st.ids[v.Parent] = map[int]bool{}
		}
		st.ids[v.Parent][v.ID] = true
		if zone != "wan" {
			entries = append(entries, entry{v.Name, zone, v.IPv4, fmt.Sprintf("VLAN %d on %s", v.ID, v.Parent)})
		}
	}
	zones := map[string]bool{}
	for _, e := range entries {
		zones[e.zone] = true
	}
	for _, e := range entries {
		r := netRow{Name: e.name, Kind: e.kind, Address: e.ipv4,
			Internet: reaches(st.policies, e.zone, "wan")}
		if p, err := netip.ParsePrefix(e.ipv4); err == nil {
			st.prefixes = append(st.prefixes, p.Masked())
			for _, a := range leasesByAddr {
				if p.Contains(a) {
					r.Devices++
				}
			}
		}
		if d, ok := c.DHCP[e.name]; ok && len(d.Range) == 2 {
			r.DHCP = d.Range[0] + " to " + d.Range[1]
		}
		for z := range zones {
			if z != e.zone && reaches(st.policies, e.zone, z) {
				r.Reaches = append(r.Reaches, z)
			}
		}
		sort.Strings(r.Reaches)
		for _, m := range c.Management.AllowedZones {
			r.Management = r.Management || m == e.zone
		}
		st.rows = append(st.rows, r)
	}
	return st
}

// reaches reports whether the firewall lets zone's traffic through to zone
// to: the first matching [[firewall.policy]] decides, and with no rules at
// all, every network may reach the internet and nothing else (config-spec.md).
func reaches(policies []config.Policy, zone, to string) bool {
	if len(policies) == 0 {
		return to == "wan"
	}
	for _, p := range policies {
		if (p.From == zone || p.From == "*") && (p.To == to || p.To == "*") {
			return p.Action == "accept"
		}
	}
	return false
}

// suggest fills in a new network's form with free choices.
func (st netState) suggest() netForm {
	f := netForm{DHCP: true, Reach: "internet"}
	if len(st.parents) > 0 {
		f.Parent = st.parents[0]
	}
	id := 0
	for _, cand := range []int{10, 20, 30, 40, 50, 60, 70, 80, 90} {
		if !st.ids[f.Parent][cand] && st.free(netip.MustParsePrefix(fmt.Sprintf("192.168.%d.0/24", cand))) {
			id = cand
			break
		}
	}
	if id == 0 {
		return f
	}
	f.ID = strconv.Itoa(id)
	f.Address = fmt.Sprintf("192.168.%d.1/24", id)
	f.RangeStart, f.RangeEnd = fmt.Sprintf("192.168.%d.100", id), fmt.Sprintf("192.168.%d.199", id)
	return f
}

// free reports whether p overlaps no network the router has.
func (st netState) free(p netip.Prefix) bool {
	for _, q := range st.prefixes {
		if q.Overlaps(p) {
			return false
		}
	}
	return true
}

func (s *Server) leaseAddrs(ctx context.Context) []netip.Addr {
	var out []netip.Addr
	for _, l := range s.leases(ctx) {
		if a, err := netip.ParseAddr(l.IPv4); err == nil {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) networksPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "networks")
	p.Nav = "networks"
	cur, ok := s.currentConfig(r.Context(), p)
	if ok {
		st := loadNetworks(cur, s.leaseAddrs(r.Context()))
		p.Nets, p.Parents, p.HasPolicies, p.NetForm = st.rows, st.parents, len(st.policies) > 0, st.suggest()
	}
	s.render(w, http.StatusOK, "networks.html", p)
}

// networks is the add form: review the new network.
func (s *Server) networks(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	p := s.newPage(ctx, sess, "networks")
	p.Nav = "networks"
	f := netForm{Name: strings.TrimSpace(r.PostFormValue("name")), Parent: r.PostFormValue("parent"),
		ID: strings.TrimSpace(r.PostFormValue("id")), Address: strings.TrimSpace(r.PostFormValue("address")),
		RangeStart: strings.TrimSpace(r.PostFormValue("range_start")), RangeEnd: strings.TrimSpace(r.PostFormValue("range_end")),
		DHCP: r.PostFormValue("dhcp") == "on", Reach: r.PostFormValue("reach")}
	p.NetForm = f
	cur, ok := s.currentConfig(ctx, p)
	if !ok {
		s.render(w, http.StatusOK, "networks.html", p)
		return
	}
	st := loadNetworks(cur, s.leaseAddrs(ctx))
	p.Nets, p.Parents, p.HasPolicies = st.rows, st.parents, len(st.policies) > 0

	edits, err := f.edits(st)
	if err != nil {
		p.Error = err.Error()
		s.render(w, http.StatusOK, "networks.html", p)
		return
	}
	text, err := edit.Apply([]byte(cur), edits...)
	if err != nil {
		var refusal *edit.Refusal
		switch {
		case errors.As(err, &refusal):
			p.Error, p.Advanced = refusal.Reason, true
		case errors.Is(err, edit.ErrStale):
			p.Error = "router.toml changed on the router since this page loaded it. Reload the page and try again."
		default:
			p.Error = err.Error()
		}
		s.render(w, http.StatusOK, "networks.html", p)
		return
	}
	p.Config, p.Base = string(text), digest(cur)
	s.check(ctx, p)
	p.Config64 = base64.StdEncoding.EncodeToString(text)
	p.Reviewing = true
	s.render(w, http.StatusOK, "networks.html", p)
}

var zonePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)

// edits checks the form and turns it into the changes to router.toml: the
// VLAN, its DHCP section, and its firewall rule if the file has rules.
func (f netForm) edits(st netState) ([]edit.Edit, error) {
	if !zonePattern.MatchString(f.Name) || f.Name == "wan" {
		return nil, mistake("A network's name is 1 to 15 lowercase letters, digits and dashes, starting with a letter, such as \"guest\".")
	}
	if st.names[f.Name] {
		return nil, mistake(fmt.Sprintf("There's already a network or port named %q. Choose another name.", f.Name))
	}
	parentOK := false
	for _, p := range st.parents {
		parentOK = parentOK || p == f.Parent
	}
	if !parentOK {
		return nil, mistake("Choose the port the new network runs on.")
	}
	id, err := strconv.Atoi(f.ID)
	if err != nil || id < 1 || id > 4094 {
		return nil, mistake("A VLAN number is from 1 to 4094.")
	}
	if st.ids[f.Parent][id] {
		return nil, mistake(fmt.Sprintf("VLAN %d is already used on %s. Choose another number.", id, f.Parent))
	}
	addr, err := netip.ParsePrefix(f.Address)
	if err != nil || !addr.Addr().Is4() || addr.Bits() < 8 || addr.Bits() > 30 {
		return nil, mistake("Type the router's address on the new network with its size, such as 192.168.30.1/24.")
	}
	if addr.Addr() == addr.Masked().Addr() || addr.Addr() == lastAddr(addr) {
		return nil, mistake(fmt.Sprintf("%s is the network's own or broadcast address; the router needs another one, such as %s.", addr.Addr(), addr.Masked().Addr().Next()))
	}
	if !st.free(addr.Masked()) {
		return nil, mistake(fmt.Sprintf("%s overlaps a network the router already has. Choose other addresses, such as 192.168.%d.1/24.", addr.Masked(), min(id, 254)))
	}
	vlan := []edit.Field{{Key: "name", Value: f.Name}, {Key: "parent", Value: f.Parent}, {Key: "id", Value: id}, {Key: "ipv4", Value: addr.String()}}
	edits := []edit.Edit{edit.Add("vlan", vlan...)}
	if f.DHCP {
		a, err1 := netip.ParseAddr(f.RangeStart)
		b, err2 := netip.ParseAddr(f.RangeEnd)
		switch {
		case err1 != nil || err2 != nil || !a.Is4() || !b.Is4():
			return nil, mistake("Type the first and last address the router hands out, such as 192.168.30.100 and 192.168.30.199.")
		case !addr.Contains(a) || !addr.Contains(b):
			return nil, mistake(fmt.Sprintf("The addresses handed out must be on the new network (%s).", addr.Masked()))
		case b.Less(a):
			return nil, mistake("The first address handed out comes before the last.")
		case !addr.Addr().Less(a) && !b.Less(addr.Addr()):
			return nil, mistake("The router's own address can't be one it hands out.")
		}
		edits = append(edits, edit.Set("dhcp."+f.Name, edit.Field{Key: "range", Value: []string{a.String(), b.String()}}))
	}
	if len(st.policies) == 0 {
		if f.Reach == "all" {
			return nil, mistake("This router has no firewall rules, so every network reaches the internet and nothing else. Letting the new one reach your other networks needs rules for all of them: use Advanced.")
		}
		return edits, nil // the default lets it reach the internet only
	}
	rule := []edit.Field{{Key: "from", Value: f.Name}, {Key: "to", Value: "wan"}, {Key: "action", Value: "accept"},
		{Key: "comment", Value: f.Name + ": internet only"}}
	if f.Reach == "all" {
		rule[1].Value, rule[3].Value = "*", f.Name+": internet and the other networks"
	}
	before := len(st.policies)
	for i, p := range st.policies {
		if p.From == "*" {
			before = i
			break
		}
	}
	return append(edits, edit.Insert("firewall.policy", before, rule...)), nil
}
