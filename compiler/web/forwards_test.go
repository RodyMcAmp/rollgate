package web

import (
	"encoding/base64"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const forwardsToml = `schema_version = 1

[dhcp.lan]
range = ["192.168.10.100", "192.168.10.199"]

[[dhcp.lan.static]]
hostname = "nas"
mac = "00:00:5e:00:53:10"
ipv4 = "192.168.10.5"

# For the kids' game nights.
[[firewall.port_forward]]
name = "game server"
protocol = "udp"
wan_port = 27015   # the game's default
to = "192.168.10.50:27015"
`

// reviewed returns the router.toml a review page would apply.
func reviewed(t *testing.T, body string) string {
	t.Helper()
	b64 := fields(body)["config64"]
	if b64 == "" {
		t.Fatalf("the review has no Apply form:\n%s", body)
	}
	raw, err := base64.StdEncoding.DecodeString(html.UnescapeString(b64))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestForwardsList(t *testing.T) {
	s, f, h, _ := setup(t)
	f.config = forwardsToml
	c, _ := loginForm(t, s, h)
	body := do(h, "GET", "/forwards", nil, c).Body.String()
	for _, want := range []string{"game server", "27015", "192.168.10.50:27015", `<option value="192.168.10.5">nas · 192.168.10.5</option>`, `aria-current="page">Port forwarding`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
}

func TestForwardAddApply(t *testing.T) {
	s, f, h, _ := setup(t)
	f.config = forwardsToml
	c, csrf := loginForm(t, s, h)
	w := do(h, "POST", "/forwards", url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"-1"},
		"name": {"plex"}, "protocol": {"tcp"}, "wan_port": {"32400"}, "device": {"192.168.10.5"}}, c)
	body := w.Body.String()
	if !strings.Contains(body, "Review the change") || !strings.Contains(body, "&#43;name = &#34;plex&#34;") {
		t.Fatalf("review page:\n%s", body)
	}
	text := reviewed(t, body)
	want := forwardsToml + "\n[[firewall.port_forward]]\nname = \"plex\"\nprotocol = \"tcp\"\nwan_port = 32400\nto = \"192.168.10.5:32400\"\n"
	if text != want {
		t.Fatalf("reviewed router.toml:\n%s\nwant:\n%s", text, want)
	}
	if f.applied != "" {
		t.Fatal("a review applied something")
	}

	// The review's Apply goes through the editor's apply, with its checks.
	fs := fields(body)
	w = do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "action": {"apply"}, "base": {fs["base"]},
		"checked": {fs["checked"]}, "config64": {html.UnescapeString(fs["config64"])}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/confirm" {
		t.Fatalf("apply: %d %s\n%s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if f.applied != want {
		t.Errorf("applied:\n%s\nwant:\n%s", f.applied, want)
	}
	// The bar at the top offers Keep and Undo on every page now.
	if body := do(h, "GET", "/forwards", nil, c).Body.String(); !strings.Contains(body, `action="/confirm"`) || !strings.Contains(body, "Generation 3 is live") {
		t.Errorf("no Keep/Undo bar after the apply:\n%s", body)
	}
}

func TestForwardChangeAndRemove(t *testing.T) {
	s, f, h, _ := setup(t)
	f.config = forwardsToml
	c, csrf := loginForm(t, s, h)

	body := do(h, "GET", "/forwards?edit=0", nil, c).Body.String()
	if fs := fields(body); fs["orig_name"] != "game server" || fs["index"] != "0" {
		t.Fatalf("edit form: %v\n%s", fs, body)
	}
	if !strings.Contains(body, `value="27015"`) || !strings.Contains(body, `value="udp" checked`) {
		t.Errorf("the edit form isn't filled in:\n%s", body)
	}

	w := do(h, "POST", "/forwards", url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"0"}, "orig_name": {"game server"},
		"name": {"game server"}, "protocol": {"tcp+udp"}, "wan_port": {"27015-27030"}, "address": {"192.168.10.50"}}, c)
	text := reviewed(t, w.Body.String())
	if !strings.Contains(text, "wan_port = \"27015-27030\"   # the game's default") || !strings.Contains(text, "# For the kids' game nights.") {
		t.Errorf("change lost the comments or the value:\n%s", text)
	}

	w = do(h, "POST", "/forwards", url.Values{"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "orig_name": {"game server"}}, c)
	body = w.Body.String()
	if !strings.Contains(body, `Remove "game server"?`) {
		t.Errorf("remove review:\n%s", body)
	}
	if text := reviewed(t, body); strings.Contains(text, "port_forward") || strings.Contains(text, "game nights") {
		t.Errorf("remove kept the forward or its comment:\n%s", text)
	}
}

func TestForwardFormMistakes(t *testing.T) {
	s, f, h, _ := setup(t)
	f.config = forwardsToml
	c, csrf := loginForm(t, s, h)
	base := url.Values{"csrf": {csrf}, "action": {"review"}, "index": {"-1"}, "name": {"web"}, "protocol": {"tcp"}, "wan_port": {"443"}, "address": {"192.168.10.20"}}
	for name, tc := range map[string]struct{ key, value, want string }{
		"no name":            {"name", "", "Give the forward a name"},
		"bad protocol":       {"protocol", "icmp", "Choose TCP, UDP or both"},
		"port too big":       {"wan_port", "70000", "1 to 65535"},
		"backwards range":    {"wan_port", "30-20", "smaller first"},
		"words for a port":   {"wan_port", "http", "Type the port"},
		"no address":         {"address", "", "IPv4 address"},
		"IPv6 address":       {"address", "fd00::5", "IPv4 address"},
		"range to a port":    {"wan_port", "1000-1010", ""}, // with device_port below
		"device port number": {"device_port", "0", "1 to 65535"},
	} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{}
			for k, v := range base {
				form[k] = v
			}
			form.Set(tc.key, tc.value)
			want := tc.want
			if name == "range to a port" {
				form.Set("device_port", "2000")
				want = "leave the device&#39;s port empty"
			}
			body := do(h, "POST", "/forwards", form, c).Body.String()
			if !strings.Contains(body, want) || strings.Contains(body, "config64") {
				t.Errorf("want the message %q and no Apply:\n%s", want, body)
			}
		})
	}
	if len(f.posts) != 0 {
		t.Errorf("mistakes reached the API: %v", f.posts)
	}
}

func TestForwardStaleAndRefused(t *testing.T) {
	s, f, h, _ := setup(t)
	f.config = forwardsToml
	c, csrf := loginForm(t, s, h)
	// Someone renamed the forward over SSH meanwhile.
	body := do(h, "POST", "/forwards", url.Values{"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "orig_name": {"old name"}}, c).Body.String()
	if !strings.Contains(body, "changed on the router since this page loaded it") || strings.Contains(body, "config64") {
		t.Errorf("stale remove:\n%s", body)
	}
	// An entry written in a way the writer doesn't edit.
	f.config = "[[firewall.port_forward]]\nname = \"x\"\nprotocol = \"tcp\"\nwan_port = 1\nto = \"\"\"\n192.168.10.5:1\"\"\"\n"
	body = do(h, "POST", "/forwards", url.Values{"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "orig_name": {"x"}}, c).Body.String()
	if !strings.Contains(body, "can&#39;t change safely") || !strings.Contains(body, `<a href="/settings">Open Advanced</a>`) {
		t.Errorf("refused remove:\n%s", body)
	}
}
