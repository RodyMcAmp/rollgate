package vpn

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNextAddress(t *testing.T) {
	p := netip.MustParsePrefix("10.66.0.1/24")
	a, err := NextAddress(p, []netip.Addr{netip.MustParseAddr("10.66.0.2"), netip.MustParseAddr("10.66.0.4")})
	if err != nil || a.String() != "10.66.0.3" {
		t.Errorf("NextAddress: %v %v, want 10.66.0.3", a, err)
	}
	full := netip.MustParsePrefix("10.66.0.1/30") // .1 router, .2 the only device address
	if _, err := NextAddress(full, []netip.Addr{netip.MustParseAddr("10.66.0.2")}); err == nil {
		t.Error("NextAddress in a full VPN: no error")
	}
}

func TestDeviceConfig(t *testing.T) {
	d := Device{Name: "phone", PrivateKey: knownPrivate, Address: netip.MustParseAddr("10.66.0.2"), DNS: netip.MustParseAddr("10.66.0.1"),
		RouterKey: knownPublic, Endpoint: "home.example.net:51820", AllowedIPs: Everything}
	got := d.Config()
	for _, want := range []string{"[Interface]\nPrivateKey = " + knownPrivate + "\nAddress = 10.66.0.2/32\nDNS = 10.66.0.1\n",
		"[Peer]\nPublicKey = " + knownPublic + "\nEndpoint = home.example.net:51820\nAllowedIPs = 0.0.0.0/0, ::/0\nPersistentKeepalive = 25\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks:\n%s\n---\n%s", want, got)
		}
	}
}

func TestEndpoint(t *testing.T) {
	for _, tc := range []struct {
		configured, wan string
		want, warn      string
	}{
		{"home.example.net", "203.0.113.5", "home.example.net:51820", ""},
		{"", "203.0.113.5", "203.0.113.5:51820", "new address now and then"},
		{"", "100.72.1.2", "100.72.1.2:51820", "carrier-grade NAT"},
		{"", "192.168.1.20", "192.168.1.20:51820", "forwards UDP port 51820"},
		{"2001:db8::1", "", "[2001:db8::1]:51820", ""},
	} {
		var wan netip.Addr
		if tc.wan != "" {
			wan = netip.MustParseAddr(tc.wan)
		}
		got, warn, err := Endpoint(tc.configured, wan, 51820)
		if err != nil || got != tc.want || (tc.warn == "") != (warn == "") || !strings.Contains(warn, tc.warn) {
			t.Errorf("Endpoint(%q, %q) = %q, %q, %v; want %q and a warning with %q", tc.configured, tc.wan, got, warn, err, tc.want, tc.warn)
		}
	}
	if _, _, err := Endpoint("", netip.Addr{}, 51820); err == nil {
		t.Error("Endpoint with no WAN address and no name: no error")
	}
}

func TestHandshakesAndAgo(t *testing.T) {
	m := Handshakes(knownPublic + "\t1795130000\n" + knownPrivate + "\t0\n\n")
	if len(m) != 1 || m[knownPublic].Unix() != 1795130000 {
		t.Errorf("Handshakes: %v", m)
	}
	now := time.Unix(1795130000, 0)
	for d, want := range map[time.Duration]string{10 * time.Second: "just now", 5 * time.Minute: "5 min ago", 3 * time.Hour: "3 h ago", 72 * time.Hour: "3 days ago"} {
		if got := Ago(now.Add(-d), now); got != want {
			t.Errorf("Ago(%v) = %q, want %q", d, got, want)
		}
	}
	if Ago(time.Time{}, now) != "never" {
		t.Error("Ago(zero) isn't never")
	}
}

func TestHomeNets(t *testing.T) {
	got := HomeNets([]netip.Prefix{netip.MustParsePrefix("192.168.10.1/24"), netip.MustParsePrefix("10.66.0.1/24"), netip.MustParsePrefix("192.168.10.0/24")})
	if !reflect.DeepEqual(got, []string{"10.66.0.0/24", "192.168.10.0/24"}) {
		t.Errorf("HomeNets: %v", got)
	}
}
