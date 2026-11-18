package compile

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// testULA is the local prefix used by tests whose config doesn't set one.
var testULA = netip.MustParsePrefix("fd12:3456:789a::/48")

func TestSubnetIDs(t *testing.T) {
	seven := 7
	c := &config.Config{
		Interfaces: []config.Interface{
			{Name: "wan", Zone: "wan"},
			{Name: "lan", Zone: "lan", Addr: netip.MustParsePrefix("192.168.10.1/24")},
			{Name: "dmz", Zone: "dmz", Addr: netip.MustParsePrefix("192.168.30.1/24"), IPv6SubnetID: &seven},
			{Name: "client", Zone: "client"}, // DHCP client: no IPv6 role
		},
		VLANs: []config.VLAN{{Name: "iot", Zone: "iot", Addr: netip.MustParsePrefix("192.168.20.1/24")}},
	}
	ids := subnetIDs(c)
	if _, ok := ids["wan"]; ok {
		t.Error("WAN must not get a subnet")
	}
	if _, ok := ids["client"]; ok {
		t.Error("a network without a static address must not get a subnet")
	}
	if ids["dmz"] != 7 {
		t.Errorf("pinned ipv6_subnet_id ignored: %d", ids["dmz"])
	}
	// Stable: the same names give the same numbers, whatever else is configured.
	alone := subnetIDs(&config.Config{Interfaces: []config.Interface{c.Interfaces[1]}})
	if alone["lan"] != ids["lan"] {
		t.Errorf("adding networks renumbered lan: %d vs %d", alone["lan"], ids["lan"])
	}
	if ids["lan"] == ids["iot"] || ids["lan"] == 7 || ids["iot"] == 7 {
		t.Errorf("subnet numbers must be unique: %v", ids)
	}
}

func TestSubnetIDClash(t *testing.T) {
	// Pin "b" to whatever "a" hashes to: "a" must move to the next number.
	h := subnetIDs(&config.Config{Interfaces: []config.Interface{{Name: "a", Zone: "a", Addr: netip.MustParsePrefix("10.0.0.1/24")}}})["a"]
	c := &config.Config{Interfaces: []config.Interface{
		{Name: "a", Zone: "a", Addr: netip.MustParsePrefix("10.0.0.1/24")},
		{Name: "b", Zone: "b", Addr: netip.MustParsePrefix("10.0.1.1/24"), IPv6SubnetID: &h},
	}}
	if ids := subnetIDs(c); ids["b"] != h || ids["a"] != (h+1)%256 {
		t.Errorf("clash not resolved: %v (hash of a = %d)", ids, h)
	}
}

func TestSubnetAddresses(t *testing.T) {
	p := subnet64(testULA, 0x4f)
	if p.String() != "fd12:3456:789a:4f::/64" || routerAddr(p).String() != "fd12:3456:789a:4f::1/64" {
		t.Errorf("got %s and %s", p, routerAddr(p))
	}
}

func TestLocalPrefix(t *testing.T) {
	root := t.TempDir()
	p, generated, err := LocalPrefix(root)
	if err != nil || !generated || !isULA48(p) {
		t.Fatalf("no state yet: want a generated ULA /48, got %v %v %v", p, generated, err)
	}
	state := filepath.Join(root, filepath.FromSlash(ULAStateFile))
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("# comment\nfd12:3456:789a::/48\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, generated, err := LocalPrefix(root); err != nil || generated || p != testULA {
		t.Errorf("stored prefix: got %v %v %v", p, generated, err)
	}
	if err := os.WriteFile(state, []byte("2001:db8::/48\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LocalPrefix(root); err == nil {
		t.Error("a non-ULA stored prefix must be an error")
	}
}
