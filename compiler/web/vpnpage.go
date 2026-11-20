package web

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/edit"
	"github.com/RodyMcAmp/rollgate/compiler/vpn"
)

// The VPN page (ADR 0018): turn the VPN on, add and remove devices, and let
// the router be managed over it. A new device's key pair is made here, in the
// unprivileged web server; only its public key goes into router.toml, and its
// configuration is shown once, on the review page, never stored.

type vpnView struct {
	On               bool
	Port             int
	Reach            string
	Address          string
	RouterKey        string
	Endpoint, Notice string
	Devices          []vpnDevice
	Managed          bool // management.allowed_zones lists "vpn"
}

type vpnDevice struct {
	Index                   int
	Name, Address, LastSeen string
}

// vpnNew is a just-made device's configuration, shown once.
type vpnNew struct{ Name, Config string }

var deviceNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// vpnState reads router.toml and the router's VPN state from the API.
func (s *Server) vpnState(ctx context.Context, p *page) (string, *config.Config, *api.VPNState, bool) {
	cur, ok := s.currentConfig(ctx, p)
	if !ok {
		return "", nil, nil, false
	}
	c, err := config.Parse([]byte(cur))
	if err != nil {
		p.Error, p.Advanced = "router.toml has mistakes, so this page can't change it. Fix them first.", true
		return "", nil, nil, false
	}
	st := &api.VPNState{}
	if resp, err := s.API.Get(ctx, "/v1/vpn"); err == nil && resp.OK && resp.VPN != nil {
		st = resp.VPN
	}
	return cur, c, st, true
}

func (s *Server) buildVPNView(c *config.Config, st *api.VPNState) *vpnView {
	v := &vpnView{RouterKey: st.PublicKey, Managed: slices.Contains(c.Management.AllowedZones, config.VPNZone)}
	if c.VPN == nil {
		return v
	}
	v.On, v.Port, v.Reach, v.Address = true, c.VPN.Port, c.VPN.Reach, c.VPN.Address
	wan, _ := netip.ParseAddr(st.WANAddress)
	if ep, note, err := vpn.Endpoint(c.VPN.Endpoint, wan, c.VPN.Port); err == nil {
		v.Endpoint, v.Notice = ep, note
	} else {
		v.Notice = err.Error()
	}
	for i, peer := range c.VPN.Peers {
		v.Devices = append(v.Devices, vpnDevice{Index: i, Name: peer.Name, Address: peer.IPv4, LastSeen: vpn.Ago(st.LastSeen[peer.PublicKey], s.now())})
	}
	return v
}

func (s *Server) vpnPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "vpn")
	p.Nav = "vpn"
	if _, c, st, ok := s.vpnState(r.Context(), p); ok {
		p.VPN = s.buildVPNView(c, st)
	}
	s.render(w, http.StatusOK, "vpn.html", p)
}

// vpnForm is every change on the page: turn on, add or remove a device, and
// management over the VPN. Each ends in the usual review.
func (s *Server) vpnForm(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	p := s.newPage(ctx, sess, "vpn")
	p.Nav = "vpn"
	cur, c, st, ok := s.vpnState(ctx, p)
	if !ok {
		s.render(w, http.StatusOK, "vpn.html", p)
		return
	}
	p.VPN = s.buildVPNView(c, st)
	fail := func(msg string) {
		p.Error = msg
		s.render(w, http.StatusOK, "vpn.html", p)
	}

	var edits []edit.Edit
	switch r.PostFormValue("action") {
	case "on":
		if c.VPN != nil {
			fail("The VPN is already on.")
			return
		}
		// apply refuses [vpn] without the router's key, so make it now.
		if resp, err := s.API.Post(ctx, "/v1/vpn/key", api.Request{}); err != nil || !resp.OK {
			if err == nil {
				err = apiError(resp)
			}
			fail("Couldn't make the router's VPN key: " + err.Error())
			return
		}
		edits = append(edits, edit.Set("vpn", edit.Field{Key: "reach", Value: "everything"}))

	case "add":
		if c.VPN == nil {
			fail("Turn the VPN on first.")
			return
		}
		name := strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
		if !deviceNameRe.MatchString(name) {
			fail("A device's name is lowercase letters, digits and dashes, such as \"maya-phone\", up to 32 characters.")
			return
		}
		var used []netip.Addr
		for _, peer := range c.VPN.Peers {
			if peer.Name == name {
				fail("A device with that name is already in the VPN.")
				return
			}
			used = append(used, peer.Addr)
		}
		if st.PublicKey == "" {
			fail("The router has no VPN key yet: turn the VPN off and on again, or run rollgate vpn key.")
			return
		}
		addr, err := vpn.NextAddress(c.VPN.Addr, used)
		if err != nil {
			fail(err.Error())
			return
		}
		wan, _ := netip.ParseAddr(st.WANAddress)
		endpoint, _, err := vpn.Endpoint(c.VPN.Endpoint, wan, c.VPN.Port)
		if err != nil {
			fail(err.Error())
			return
		}
		priv, pub, err := vpn.GenerateKey()
		if err != nil {
			fail(err.Error())
			return
		}
		allowed := vpn.Everything
		if r.PostFormValue("traffic") == "home" {
			allowed = vpn.HomeNets(webHomePrefixes(c))
		}
		d := vpn.Device{Name: name, PrivateKey: priv, Address: addr, DNS: c.VPN.Addr.Addr(), RouterKey: st.PublicKey, Endpoint: endpoint, AllowedIPs: allowed}
		p.VPNNew = &vpnNew{Name: name, Config: d.Config()}
		edits = append(edits, edit.Add("vpn.peer", edit.Field{Key: "name", Value: name},
			edit.Field{Key: "public_key", Value: pub}, edit.Field{Key: "ipv4", Value: addr.String()}))

	case "remove":
		i, err := strconv.Atoi(r.PostFormValue("index"))
		name := r.PostFormValue("name")
		if err != nil || c.VPN == nil {
			fail("Choose a device to remove.")
			return
		}
		p.Removing = name
		edits = append(edits, edit.Remove("vpn.peer", i, "name", name))

	case "manage":
		zones := slices.DeleteFunc(slices.Clone(c.Management.AllowedZones), func(z string) bool { return z == config.VPNZone })
		if r.PostFormValue("managed") == "on" {
			zones = append(zones, config.VPNZone)
		}
		edits = append(edits, edit.Set("management", edit.Field{Key: "allowed_zones", Value: zones}))

	default:
		fail("Unknown request.")
		return
	}

	text, err := edit.Apply([]byte(cur), edits...)
	if err != nil {
		p.VPNNew = nil // its key will never be in router.toml
		var refusal *edit.Refusal
		switch {
		case errors.As(err, &refusal):
			p.Error, p.Advanced = refusal.Reason, true
		case errors.Is(err, edit.ErrStale):
			p.Error = "router.toml changed on the router since this page loaded it. Reload the page and try again."
		default:
			p.Error = err.Error()
		}
		s.render(w, http.StatusOK, "vpn.html", p)
		return
	}
	p.Config, p.Base = string(text), digest(cur)
	s.check(ctx, p)
	if !p.Valid {
		p.VPNNew = nil
	}
	p.Config64 = base64.StdEncoding.EncodeToString(text)
	p.Reviewing = true
	s.render(w, http.StatusOK, "vpn.html", p)
}

// vpnDownload hands back a device's configuration from the review page as a
// file. Nothing is stored: the browser sends the text, the server returns it.
func (s *Server) vpnDownload(w http.ResponseWriter, r *http.Request) {
	if s.posted(w, r) == nil {
		return
	}
	name := r.PostFormValue("name")
	if !deviceNameRe.MatchString(name) {
		http.Error(w, "bad device name", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.conf"`)
	_, _ = w.Write([]byte(strings.ReplaceAll(r.PostFormValue("config"), "\r\n", "\n")))
}

// webHomePrefixes are the home networks and the VPN, for "only my home networks".
func webHomePrefixes(c *config.Config) []netip.Prefix {
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
