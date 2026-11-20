package web

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

const devicesToml = `schema_version = 1

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

# The NAS in the closet.
[[dhcp.lan.static]]
hostname = "nas"
mac = "00:00:5e:00:53:10"
ipv4 = "192.168.10.2"

[dhcp.iot]
range = ["192.168.20.100", "192.168.20.199"]
`

func devicesSetup(t *testing.T) (*Server, *fakeAPI, http.Handler, *http.Cookie, string) {
	t.Helper()
	s, f, h, now := setup(t)
	f.config = devicesToml
	f.leases = []sysinfo.Lease{
		{Expires: now.Add(3*time.Hour + time.Minute), MAC: "52:54:00:aa:bb:01", IPv4: "192.168.10.123", Hostname: "Mayas-iPhone"},
		{MAC: "00:00:5e:00:53:10", IPv4: "192.168.10.2", Hostname: "nas"},
		{Expires: now.Add(20 * time.Minute), MAC: "52:54:00:aa:bb:02", IPv4: "192.168.20.150"},
	}
	c, csrf := loginForm(t, s, h)
	return s, f, h, c, csrf
}

func TestDevicesPage(t *testing.T) {
	_, _, h, c, _ := devicesSetup(t)
	body := do(h, "GET", "/devices", nil, c).Body.String()
	for _, want := range []string{
		"Mayas-iPhone", "192.168.10.123", "in 3 h", `href="/devices?fix=52%3a54%3a00%3aaa%3abb%3a01#form"`, // a lease (the link escapes the colons)
		"in 21 min", "no name", // the iot lease without a name
		"nas", "fixed address", `<span class="ok">connected</span>`, `href="/devices?edit=lan:0#form"`, // the fixed one
		`aria-current="page">Devices`,
		`value="192.168.10.3"`, // the suggested free address: .1 is the router, .2 the NAS
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestDeviceFixFromLease(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	body := do(h, "GET", "/devices?fix=52:54:00:aa:bb:01", nil, c).Body.String()
	for _, want := range []string{`value="mayas-iphone"`, `value="52:54:00:aa:bb:01"`, `value="192.168.10.3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the prefilled form lacks %q", want)
		}
	}
	w := do(h, "POST", "/devices", url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"-1"}, "network": {"lan"},
		"hostname": {"mayas-iphone"}, "mac": {"52:54:00:AA:BB:01"}, "address": {"192.168.10.3"}}, c)
	body = w.Body.String()
	text := reviewed(t, body)
	add := "[[dhcp.lan.static]]\nhostname = \"mayas-iphone\"\nmac = \"52:54:00:aa:bb:01\"\nipv4 = \"192.168.10.3\"\n"
	if !strings.Contains(text, "ipv4 = \"192.168.10.2\"\n\n"+add+"\n[dhcp.iot]") {
		t.Fatalf("the new entry isn't after the NAS:\n%s", text)
	}
	fs := fields(body)
	w = do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "action": {"apply"}, "base": {fs["base"]},
		"checked": {fs["checked"]}, "config64": {html.UnescapeString(fs["config64"])}}, c)
	if w.Code != http.StatusSeeOther || f.applied != text {
		t.Fatalf("apply: %d, applied:\n%s", w.Code, f.applied)
	}
}

func TestDeviceChangeMoveRemove(t *testing.T) {
	_, _, h, c, csrf := devicesSetup(t)
	body := do(h, "GET", "/devices?edit=lan:0", nil, c).Body.String()
	if fs := fields(body); fs["orig_name"] != "nas" || fs["orig_network"] != "lan" || fs["index"] != "0" {
		t.Fatalf("edit form: %v", fs)
	}
	// Same network: a change in place keeps the comment.
	w := do(h, "POST", "/devices", url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"0"}, "orig_name": {"nas"},
		"orig_network": {"lan"}, "network": {"lan"}, "hostname": {"nas"}, "mac": {"00:00:5e:00:53:10"}, "address": {"192.168.10.4"}}, c)
	if text := reviewed(t, w.Body.String()); !strings.Contains(text, "# The NAS in the closet.\n[[dhcp.lan.static]]\nhostname = \"nas\"\nmac = \"00:00:5e:00:53:10\"\nipv4 = \"192.168.10.4\"") {
		t.Errorf("change:\n%s", text)
	}
	// Another network: removed from lan, added to iot.
	w = do(h, "POST", "/devices", url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"0"}, "orig_name": {"nas"},
		"orig_network": {"lan"}, "network": {"iot"}, "hostname": {"nas"}, "mac": {"00:00:5e:00:53:10"}, "address": {"192.168.20.5"}}, c)
	text := reviewed(t, w.Body.String())
	if strings.Contains(text, "dhcp.lan.static") || !strings.Contains(text, "[[dhcp.iot.static]]\nhostname = \"nas\"") {
		t.Errorf("move:\n%s", text)
	}
	// Remove.
	w = do(h, "POST", "/devices", url.Values{"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "orig_name": {"nas"}, "orig_network": {"lan"}}, c)
	body = w.Body.String()
	if !strings.Contains(body, `Remove the fixed address of "nas"?`) {
		t.Errorf("remove review:\n%s", body)
	}
	if text := reviewed(t, body); strings.Contains(text, "static") || strings.Contains(text, "closet") {
		t.Errorf("remove kept the entry or its comment:\n%s", text)
	}
}

func TestDeviceFormMistakes(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	base := url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"-1"}, "network": {"lan"},
		"hostname": {"tv"}, "mac": {"52:54:00:aa:bb:09"}, "address": {"192.168.10.9"}}
	for name, tc := range map[string]struct{ key, value, want string }{
		"no network":      {"network", "guest", "Choose the network"},
		"bad name":        {"hostname", "living room tv!", "letters, digits and dashes"},
		"bad MAC":         {"mac", "52:54:00", "52:54:00:12:34:56"},
		"bad address":     {"address", "fd00::9", "IPv4 address"},
		"wrong network":   {"address", "192.168.20.9", "isn&#39;t on the lan network"},
		"in the range":    {"address", "192.168.10.150", "in the range the router hands out"},
		"dash at the end": {"hostname", "tv-", "letters, digits and dashes"},
	} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{}
			for k, v := range base {
				form[k] = v
			}
			form.Set(tc.key, tc.value)
			body := do(h, "POST", "/devices", form, c).Body.String()
			if !strings.Contains(body, tc.want) || strings.Contains(body, "config64") {
				t.Errorf("want %q and no Apply:\n%s", tc.want, body)
			}
		})
	}
	if len(f.posts) != 0 {
		t.Errorf("mistakes reached the API: %v", f.posts)
	}
}

func TestDeviceHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"Maya's iPhone": "mayas-iphone", "LIVING_ROOM.tv": "living-room-tv", "--x--": "x", "": "", "a  b": "a-b",
	} {
		if got := hostLabel(in); got != want {
			t.Errorf("hostLabel(%q) = %q, want %q", in, got, want)
		}
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{-time.Minute: "ended", 30 * time.Second: "in 1 min", 5 * time.Hour: "in 5 h", 72 * time.Hour: "in 3 days"} {
		if got := until(now.Add(d), now); got != want {
			t.Errorf("until(%v) = %q, want %q", d, got, want)
		}
	}
	if got := until(time.Time{}, now); got != "never" {
		t.Errorf("until(zero) = %q", got)
	}
}
