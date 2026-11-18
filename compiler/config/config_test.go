package config

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
)

// base is the smallest valid config: everything else comes from defaults.
const base = `
schema_version = 1

[[interface]]
name = "wan"
match_device = "eth0"
ipv4 = "dhcp"

[[interface]]
name = "lan"
match_device = "eth1"
ipv4 = "192.168.10.1/24"
`

func TestExampleConfig(t *testing.T) {
	c, err := Load("../../examples/router.toml")
	if err != nil {
		t.Fatalf("examples/router.toml must be valid:\n%v", err)
	}
	if got := c.Interfaces[1].Addr; got != netip.MustParsePrefix("192.168.10.1/24") {
		t.Errorf("lan address = %v", got)
	}
	if c.Interfaces[0].Addr.IsValid() {
		t.Errorf("wan uses DHCP, so it has no static address; got %v", c.Interfaces[0].Addr)
	}
	if got := c.VLANs[0].Zone; got != "iot" {
		t.Errorf("vlan zone defaults to its name; got %q", got)
	}
	if got := c.DHCP["lan"].RangeEnd; got != netip.MustParseAddr("192.168.10.199") {
		t.Errorf("dhcp.lan range end = %v", got)
	}
	if got := c.DHCP["lan"].Static[0].Addr; got != netip.MustParseAddr("192.168.10.5") {
		t.Errorf("static lease address = %v", got)
	}
	pf := c.Firewall.PortForward[0]
	if pf.WANPortFirst != 27015 || pf.WANPortLast != 27015 || pf.Target != netip.MustParseAddrPort("192.168.10.50:27015") {
		t.Errorf("port forward parsed as %d-%d -> %v", pf.WANPortFirst, pf.WANPortLast, pf.Target)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(base + "\n[dhcp.lan]\nrange = [\"192.168.10.100\", \"192.168.10.199\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"hostname", c.System.Hostname, "gateway"},
		{"domain", c.System.Domain, "home.arpa"},
		{"timezone", c.System.Timezone, "UTC"},
		{"ipv6", c.System.IPv6, "auto"},
		{"allowed_zones", c.Management.AllowedZones, []string{"lan"}},
		{"confirm_timeout_seconds", c.Management.ConfirmTimeoutSeconds, 120},
		{"dns mode", c.DNS.Mode, "recursive"},
		{"wan_prefix_hint", c.IPv6.WANPrefixHint, 56},
		{"zone", c.Interfaces[1].Zone, "lan"},
		{"lease_time", c.DHCP["lan"].LeaseTime, "12h"},
	}
	for _, ch := range checks {
		if !equal(ch.got, ch.want) {
			t.Errorf("default %s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
}

func TestProblems(t *testing.T) {
	const dmz = "\n[[interface]]\nname = \"dmz\"\nmatch_device = \"eth2\"\n"
	cases := []struct {
		name   string
		config string
		want   []string // "CODE path" of every expected problem
	}{
		{"minimal config is valid", base, nil},
		{"unknown key", base + "[system]\nhostnme = \"x\"\n", []string{"E007 system.hostnme"}},
		{"missing schema_version", "[system]\n", []string{"E010 schema_version"}},
		{"unsupported schema_version", "schema_version = 2\n", []string{"E009 schema_version"}},
		{"not TOML", "schema_version = \n", []string{"E012 line 1"}},
		{"wrong type", base + "[[vlan]]\nname = \"iot\"\nparent = \"lan\"\nid = \"20\"\n", []string{"E012 line 16"}},

		{"bad hostname", base + "[system]\nhostname = \"my router\"\n", []string{"E009 system.hostname"}},
		{"empty domain", base + "[system]\ndomain = \"\"\n", []string{"E009 system.domain"}},
		{"unknown timezone", base + "[system]\ntimezone = \"Mars/Base\"\n", []string{"E009 system.timezone"}},
		{"ipv6 choice", base + "[system]\nipv6 = \"on\"\n", []string{"E009 system.ipv6"}},

		{"management from wan", base + "[management]\nallowed_zones = [\"wan\"]\n", []string{"E009 management.allowed_zones"}},
		{"no management zone", base + "[management]\nallowed_zones = []\n", []string{"E009 management.allowed_zones"}},
		{"key path escape", base + "[management]\nssh_authorized_keys = [\"../shadow\"]\n", []string{"E009 management.ssh_authorized_keys"}},
		{"confirm timeout", base + "[management]\nconfirm_timeout_seconds = 10\n", []string{"E009 management.confirm_timeout_seconds"}},

		{"network address", base + dmz + "ipv4 = \"10.0.0.0/24\"\n", []string{`E009 interface "dmz" ipv4`}},
		{"broadcast address", base + dmz + "ipv4 = \"10.0.0.255/24\"\n", []string{`E009 interface "dmz" ipv4`}},
		{"too small network", base + dmz + "ipv4 = \"10.0.0.1/31\"\n", []string{`E009 interface "dmz" ipv4`}},
		{"IPv6 in ipv4", base + dmz + "ipv4 = \"fd00::1/64\"\n", []string{`E009 interface "dmz" ipv4`}},
		{"missing ipv4", base + dmz, []string{`E010 interface "dmz" ipv4`}},
		{"no match", base + "\n[[interface]]\nname = \"dmz\"\nipv4 = \"10.0.0.1/24\"\n", []string{`E010 interface "dmz"`}},
		{"both matches", base + dmz + "match_mac = \"00:00:5e:00:53:01\"\nipv4 = \"10.0.0.1/24\"\n", []string{`E009 interface "dmz"`}},
		{"bad mac", base + "\n[[interface]]\nname = \"dmz\"\nmatch_mac = \"00:00:5e\"\nipv4 = \"10.0.0.1/24\"\n", []string{`E009 interface "dmz" match_mac`}},
		{"duplicate name", base + "\n[[interface]]\nname = \"lan\"\nmatch_device = \"eth2\"\nipv4 = \"10.0.0.1/24\"\n", []string{`E011 interface "lan" name`}},
		{"uppercase name", base + "\n[[interface]]\nname = \"DMZ\"\nmatch_device = \"eth2\"\nipv4 = \"10.0.0.1/24\"\n",
			[]string{`E009 interface "DMZ" name`, `E009 interface "DMZ" zone`}},

		{"vlan id 0", base + "[[vlan]]\nname = \"iot\"\nparent = \"lan\"\nipv4 = \"10.0.0.1/24\"\n", []string{`E009 vlan "iot" id`}},
		{"vlan no parent", base + "[[vlan]]\nname = \"iot\"\nid = 20\nipv4 = \"10.0.0.1/24\"\n", []string{`E010 vlan "iot" parent`}},
		{"vlan id twice", base + "[[vlan]]\nname = \"a\"\nparent = \"lan\"\nid = 20\nipv4 = \"10.0.1.1/24\"\n" +
			"[[vlan]]\nname = \"b\"\nparent = \"lan\"\nid = 20\nipv4 = \"10.0.2.1/24\"\n", []string{`E011 vlan "b" id`}},

		{"dhcp range reversed", base + "[dhcp.lan]\nrange = [\"192.168.10.200\", \"192.168.10.100\"]\n", []string{"E009 dhcp.lan range"}},
		{"dhcp no range", base + "[dhcp.lan]\nlease_time = \"1h\"\n", []string{"E010 dhcp.lan range"}},
		{"dhcp bad lease time", base + "[dhcp.lan]\nrange = [\"192.168.10.100\", \"192.168.10.199\"]\nlease_time = \"12 hours\"\n", []string{"E009 dhcp.lan lease_time"}},
		{"static lease twice", base + "[dhcp.lan]\nrange = [\"192.168.10.100\", \"192.168.10.199\"]\n" +
			"[[dhcp.lan.static]]\nhostname = \"a\"\nmac = \"00:00:5e:00:53:10\"\nipv4 = \"192.168.10.5\"\n" +
			"[[dhcp.lan.static]]\nhostname = \"b\"\nmac = \"00:00:5E:00:53:10\"\nipv4 = \"192.168.10.6\"\n",
			[]string{`E011 dhcp.lan static "b"`}},

		{"forward without forwarders", base + "[dns]\nmode = \"forward\"\n", []string{"E010 dns.forwarders"}},
		{"forwarders when recursive", base + "[dns]\nforwarders = [\"9.9.9.9\"]\n", []string{"E009 dns.forwarders"}},
		{"DNS over TLS forwarder", base + "[dns]\nmode = \"forward\"\nforwarders = [\"9.9.9.9@853#dns.quad9.net\", \"2620:fe::fe\"]\n", nil},
		{"bad forwarder", base + "[dns]\nmode = \"forward\"\nforwarders = [\"dns.quad9.net\"]\n", []string{"E009 dns.forwarders"}},
		{"blocklists not yet", base + "[dns]\nblocklists = [\"https://example.com/list\"]\n", []string{"E009 dns.blocklists"}},

		{"policy action", base + "[[firewall.policy]]\nfrom = \"lan\"\nto = \"*\"\naction = \"allow\"\n", []string{"E009 firewall.policy #1 action"}},
		{"policy without from", base + "[[firewall.policy]]\nto = \"*\"\naction = \"drop\"\n", []string{"E010 firewall.policy #1 from"}},

		{"port out of range", base + pf(`wan_port = 70000`, `to = "192.168.10.50:80"`), []string{`E009 firewall.port_forward "web" wan_port`}},
		{"port range reversed", base + pf(`wan_port = "100-50"`, `to = "192.168.10.50:80"`), []string{`E009 firewall.port_forward "web" wan_port`}},
		{"port range", base + pf(`wan_port = "8000-8010"`, `to = "192.168.10.50:8000"`), nil},
		{"target without port", base + pf(`wan_port = 80`, `to = "192.168.10.50"`), []string{`E009 firewall.port_forward "web" to`}},
		{"no wan_port", base + pf(`to = "192.168.10.50:80"`), []string{`E010 firewall.port_forward "web" wan_port`}},
		{"port forward twice", base + pf(`wan_port = 80`, `to = "192.168.10.50:80"`) + pf(`wan_port = 81`, `to = "192.168.10.50:81"`),
			[]string{`E011 firewall.port_forward "web" name`}},

		{"ula prefix", base + "[ipv6]\nula_prefix = \"fd12:3456:789a::/48\"\n", nil},
		{"ula not /48", base + "[ipv6]\nula_prefix = \"fd12:3456:789a::/64\"\n", []string{"E008 ipv6.ula_prefix"}},
		{"ula not ULA", base + "[ipv6]\nula_prefix = \"2001:db8::/48\"\n", []string{"E008 ipv6.ula_prefix"}},
		{"prefix hint", base + "[ipv6]\nwan_prefix_hint = 40\n", []string{"E008 ipv6.wan_prefix_hint"}},
		{"subnet id twice", base + dmz + "ipv4 = \"10.0.0.1/24\"\nipv6_subnet_id = 7\n" +
			"[[vlan]]\nname = \"iot\"\nparent = \"lan\"\nid = 20\nipv4 = \"10.0.1.1/24\"\nipv6_subnet_id = 7\n",
			[]string{`E008 vlan "iot" ipv6_subnet_id`}},
		{"subnet id range", base + dmz + "ipv4 = \"10.0.0.1/24\"\nipv6_subnet_id = 256\n", []string{`E008 interface "dmz" ipv6_subnet_id`}},

		{"every problem is reported", base + "[system]\nipv6 = \"on\"\nhostname = \"-\"\n[management]\nconfirm_timeout_seconds = 0\n",
			[]string{"E009 system.hostname", "E009 system.ipv6", "E009 management.confirm_timeout_seconds"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.config))
			var got []string
			var ps Problems
			if errors.As(err, &ps) {
				for _, p := range ps {
					got = append(got, p.Code+" "+p.Path)
					if p.Msg == "" {
						t.Errorf("%s has no message", p.Code)
					}
				}
			} else if err != nil {
				t.Fatalf("error is %T, want Problems: %v", err, err)
			}
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) {
				t.Errorf("problems:\n got  %q\n want %q\nfull output:\n%v", got, want, err)
			}
		})
	}
}

func pf(lines ...string) string {
	s := "\n[[firewall.port_forward]]\nname = \"web\"\nprotocol = \"tcp\"\n"
	for _, l := range lines {
		s += l + "\n"
	}
	return s
}

func equal(a, b any) bool {
	as, aok := a.([]string)
	bs, bok := b.([]string)
	if aok && bok {
		return slices.Equal(as, bs)
	}
	return a == b
}
