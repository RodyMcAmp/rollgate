package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

const networksToml = `schema_version = 1

[management]
allowed_zones = ["lan"]

[[interface]]
name = "wan"
match_mac = "00:00:5e:00:53:01"
ipv4 = "dhcp"

[[interface]]
name = "lan"
match_mac = "00:00:5e:00:53:02"
ipv4 = "192.168.10.1/24"

[[vlan]]
name = "iot"
parent = "lan"
id = 20
ipv4 = "192.168.20.1/24"

[dhcp.lan]
range = ["192.168.10.100", "192.168.10.199"]

[dhcp.iot]
range = ["192.168.20.100", "192.168.20.199"]

[[firewall.policy]]
from = "lan"
to = "*"
action = "accept"

[[firewall.policy]]
from = "iot"
to = "wan"
action = "accept"

# Everything else is dropped.
[[firewall.policy]]
from = "*"
to = "*"
action = "drop"
`

func networksSetup(t *testing.T, config string) (*fakeAPI, http.Handler, *http.Cookie, string) {
	t.Helper()
	s, f, h, _ := setup(t)
	f.config = config
	f.leases = []sysinfo.Lease{{MAC: "52:54:00:aa:bb:01", IPv4: "192.168.10.123"}, {MAC: "52:54:00:aa:bb:02", IPv4: "192.168.20.150"}}
	c, csrf := loginForm(t, s, h)
	return f, h, c, csrf
}

func addNetwork(csrf string, set map[string]string) url.Values {
	form := url.Values{"csrf": {csrf}, "action": {"review"}, "name": {"guest"}, "parent": {"lan"}, "id": {"30"},
		"address": {"192.168.30.1/24"}, "range_start": {"192.168.30.100"}, "range_end": {"192.168.30.199"},
		"dhcp": {"on"}, "reach": {"internet"}}
	for k, v := range set {
		form.Set(k, v)
	}
	return form
}

func TestNetworksPage(t *testing.T) {
	_, h, c, _ := networksSetup(t, networksToml)
	body := do(h, "GET", "/networks", nil, c).Body.String()
	for _, want := range []string{
		"VLAN 20 on lan", "192.168.20.100 to 192.168.20.199", "manages the router",
		`aria-current="page">Networks`,
		// lan reaches iot (lan -> * accept); iot reaches only the internet.
		"<td>iot</td>\n</tr>", "<span class=\"muted\">nothing else</span>",
		// the suggestion skips VLAN 10 and 20, whose addresses are taken
		`value="30"`, `value="192.168.30.1/24"`, `value="192.168.30.100"`,
		`value="all"`, // firewall rules exist, so "everything" can be offered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestAddNetworkWithRules(t *testing.T) {
	_, h, c, csrf := networksSetup(t, networksToml)
	body := do(h, "POST", "/networks", addNetwork(csrf, nil), c).Body.String()
	text := reviewed(t, body)
	for _, want := range []string{
		"[[vlan]]\nname = \"guest\"\nparent = \"lan\"\nid = 30\nipv4 = \"192.168.30.1/24\"\n",
		"[dhcp.guest]\nrange = [\"192.168.30.100\", \"192.168.30.199\"]\n",
		// before the catch-all, which keeps its comment
		"[[firewall.policy]]\nfrom = \"guest\"\nto = \"wan\"\naction = \"accept\"\ncomment = \"guest: internet only\"\n\n# Everything else is dropped.\n[[firewall.policy]]\nfrom = \"*\"",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the new router.toml lacks:\n%s\n---\n%s", want, text)
		}
	}
	text = reviewed(t, do(h, "POST", "/networks", addNetwork(csrf, map[string]string{"reach": "all", "dhcp": ""}), c).Body.String())
	if !strings.Contains(text, "to = \"*\"\naction = \"accept\"\ncomment = \"guest: internet and the other networks\"") || strings.Contains(text, "dhcp.guest") {
		t.Errorf("reach everything, no DHCP:\n%s", text)
	}
}

func TestAddNetworkWithoutRules(t *testing.T) {
	cfg := strings.Split(networksToml, "[[firewall.policy]]")[0]
	_, h, c, csrf := networksSetup(t, cfg)
	text := reviewed(t, do(h, "POST", "/networks", addNetwork(csrf, nil), c).Body.String())
	if strings.Contains(text, "firewall.policy") {
		t.Errorf("a rule was added to a file without rules, which would turn off the default for every network:\n%s", text)
	}
	body := do(h, "POST", "/networks", addNetwork(csrf, map[string]string{"reach": "all"}), c).Body.String()
	if !strings.Contains(body, "has no firewall rules") || strings.Contains(body, "config64") {
		t.Errorf("reach everything without rules:\n%s", body)
	}
	if body := do(h, "GET", "/networks", nil, c).Body.String(); strings.Contains(body, `value="all"`) {
		t.Error("\"everything\" is offered though the file has no rules")
	}
}

func TestAddNetworkMistakes(t *testing.T) {
	f, h, c, csrf := networksSetup(t, networksToml)
	for name, tc := range map[string]struct {
		set  map[string]string
		want string
	}{
		"bad name":        {map[string]string{"name": "Guest Wifi"}, "lowercase letters"},
		"name taken":      {map[string]string{"name": "iot"}, "already a network or port named"},
		"wan":             {map[string]string{"name": "wan"}, "lowercase letters"},
		"no such port":    {map[string]string{"parent": "eth9"}, "Choose the port"},
		"VLAN used":       {map[string]string{"id": "20"}, "VLAN 20 is already used"},
		"VLAN too big":    {map[string]string{"id": "5000"}, "1 to 4094"},
		"overlap":         {map[string]string{"address": "192.168.20.5/16"}, "overlaps a network"},
		"network address": {map[string]string{"address": "192.168.30.0/24"}, "own or broadcast"},
		"range outside":   {map[string]string{"range_end": "192.168.31.10"}, "must be on the new network"},
		"range reversed":  {map[string]string{"range_start": "192.168.30.200"}, "comes before the last"},
		"router in range": {map[string]string{"range_start": "192.168.30.1"}, "own address can"},
	} {
		t.Run(name, func(t *testing.T) {
			body := do(h, "POST", "/networks", addNetwork(csrf, tc.set), c).Body.String()
			if !strings.Contains(body, tc.want) || strings.Contains(body, "config64") {
				t.Errorf("want %q and no Apply:\n%s", tc.want, body)
			}
		})
	}
	if len(f.posts) != 0 {
		t.Errorf("mistakes reached the API: %v", f.posts)
	}
}
