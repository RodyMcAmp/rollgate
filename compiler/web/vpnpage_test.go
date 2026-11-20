package web

import (
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/vpn"
)

const routerKey = "HIgo9xNzJMWLKASShiTqIybxZ0U3wGLiUeJ1PKf8ykw="

const vpnToml = networksToml + `
[vpn]
reach = "everything"

# Maya's phone.
[[vpn.peer]]
name = "maya-phone"
public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
ipv4 = "10.66.0.2"
`

func TestVPNOff(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	f.config = networksToml
	body := do(h, "GET", "/vpn", nil, c).Body.String()
	if !strings.Contains(body, "The VPN is off") || !strings.Contains(body, `aria-current="page">VPN`) {
		t.Fatalf("VPN page when off:\n%s", body)
	}
	body = do(h, "POST", "/vpn", url.Values{"csrf": {csrf}, "action": {"on"}}, c).Body.String()
	text := reviewed(t, body)
	if !strings.Contains(text, "\n[vpn]\nreach = \"everything\"\n") {
		t.Errorf("turning on:\n%s", text)
	}
	if f.vpn == nil || f.vpn.PublicKey != routerKey {
		t.Error("turning on didn't make the router's key first")
	}
}

func TestVPNAddDevice(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	f.config = vpnToml
	f.vpn = &api.VPNState{PublicKey: routerKey, WANAddress: "203.0.113.5",
		LastSeen: map[string]time.Time{"xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=": time.Date(2026, 11, 19, 11, 57, 0, 0, time.UTC)}}

	body := do(h, "GET", "/vpn", nil, c).Body.String()
	for _, want := range []string{"The VPN is on", "203.0.113.5:51820", "maya-phone", "10.66.0.2", "3 min ago", "new address now and then"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}

	body = do(h, "POST", "/vpn", url.Values{"csrf": {csrf}, "action": {"add"}, "name": {"Laptop"}, "traffic": {"home"}}, c).Body.String()
	text := reviewed(t, body)
	cfg := html.UnescapeString(body)
	priv := ""
	for _, line := range strings.Split(cfg, "\n") {
		if k, ok := strings.CutPrefix(line, "PrivateKey = "); ok {
			priv = k
		}
	}
	pub, err := vpn.PublicKey(priv)
	if err != nil {
		t.Fatalf("the review shows no usable private key:\n%s", body)
	}
	for _, want := range []string{"Address = 10.66.0.3/32", "DNS = 10.66.0.1", "PublicKey = " + routerKey, "Endpoint = 203.0.113.5:51820",
		"AllowedIPs = 10.66.0.0/24, 192.168.10.0/24, 192.168.20.0/24", "shown only now", "Download laptop.conf"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the review lacks %q", want)
		}
	}
	if !strings.Contains(text, "[[vpn.peer]]\nname = \"laptop\"\npublic_key = \""+pub+"\"\nipv4 = \"10.66.0.3\"\n") || !strings.Contains(text, "# Maya's phone.") {
		t.Errorf("router.toml after adding:\n%s", text)
	}
	if strings.Contains(text, priv) {
		t.Error("the device's private key is in router.toml")
	}

	w := do(h, "POST", "/vpn/download", url.Values{"csrf": {csrf}, "name": {"laptop"}, "config": {"[Interface]\r\nPrivateKey = x\r\n"}}, c)
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="laptop.conf"` || w.Body.String() != "[Interface]\nPrivateKey = x\n" {
		t.Errorf("download: %q %q", got, w.Body.String())
	}
	if w := do(h, "POST", "/vpn/download", url.Values{"csrf": {csrf}, "name": {"../x"}, "config": {"y"}}, c); w.Code != 400 {
		t.Errorf("download with a bad name: %d", w.Code)
	}
}

func TestVPNRemoveAndManage(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	f.config = vpnToml
	f.vpn = &api.VPNState{PublicKey: routerKey, WANAddress: "203.0.113.5"}

	text := reviewed(t, do(h, "POST", "/vpn", url.Values{"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "name": {"maya-phone"}}, c).Body.String())
	if strings.Contains(text, "vpn.peer") || strings.Contains(text, "Maya's phone") || !strings.Contains(text, "[vpn]") {
		t.Errorf("remove:\n%s", text)
	}
	text = reviewed(t, do(h, "POST", "/vpn", url.Values{"csrf": {csrf}, "action": {"manage"}, "managed": {"on"}}, c).Body.String())
	if !strings.Contains(text, "[management]\nallowed_zones = [\"lan\", \"vpn\"]\n") {
		t.Errorf("management over the VPN:\n%s", text)
	}
}

func TestVPNMistakes(t *testing.T) {
	_, f, h, c, csrf := devicesSetup(t)
	f.config = vpnToml
	f.vpn = &api.VPNState{PublicKey: routerKey, WANAddress: "203.0.113.5"}
	for name, form := range map[string]url.Values{
		"bad name":    {"csrf": {csrf}, "action": {"add"}, "name": {"my phone!"}},
		"name taken":  {"csrf": {csrf}, "action": {"add"}, "name": {"maya-phone"}},
		"already on":  {"csrf": {csrf}, "action": {"on"}},
		"stale entry": {"csrf": {csrf}, "action": {"remove"}, "index": {"0"}, "name": {"other"}},
	} {
		if body := do(h, "POST", "/vpn", form, c).Body.String(); !strings.Contains(body, `class="error"`) || strings.Contains(body, "config64") {
			t.Errorf("%s: want an error and no Apply:\n%s", name, body)
		}
	}
}
