package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/web"
)

// okRunner runs nothing and reports success.
type okRunner struct{ calls []string }

func (r *okRunner) Run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// firstBootRouter is a router below a temporary root, as the image leaves it:
// the example config installed, nothing applied yet. failing names the health
// checks that fail. The clock moves only when firstBoot sleeps.
func firstBootRouter(t *testing.T, config string, failing ...string) (apply.System, string, string, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	for p, content := range map[string]string{
		"etc/nftables.conf":                      "# placeholder firewall\n",
		"etc/dnsmasq.conf":                       "# Configuration file for dnsmasq.\n",
		"etc/unbound/unbound.conf":               "server:\n",
		"etc/ssh/sshd_config.d/10-rollgate.conf": "PasswordAuthentication no\n",
		"etc/rollgate/secrets/admin.pub":         "ssh-ed25519 AAAA admin\n",
		"etc/rollgate/router.toml":               config,
	} {
		writeTestFile(t, filepath.Join(root, p), content, 0o644)
	}
	checks := t.TempDir()
	for _, name := range []string{"10-firewall", wanCheck, "30-dns", "40-ssh"} {
		code := "0"
		for _, f := range failing {
			if f == name {
				code = "1"
			}
		}
		writeTestFile(t, filepath.Join(checks, name), "#!/bin/sh\necho "+name+"\nexit "+code+"\n", 0o755)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	out := &bytes.Buffer{}
	s := apply.System{Root: root, Run: &okRunner{}, Out: out, Self: "/usr/bin/rollgate",
		Now: func() time.Time { return now }}
	return s, filepath.Join(root, "etc/rollgate/router.toml"), checks, out
}

func exampleConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFirstBootKeepsAWorkingConfig(t *testing.T) {
	s, path, checks, out := firstBootRouter(t, exampleConfig(t))
	if err := firstBoot(s, path, checks, func(time.Duration) { t.Fatal("slept, though every check passed") }); err != nil {
		t.Fatalf("firstBoot: %v\n%s", err, out)
	}
	st := apply.Store{Root: s.Root}
	if n, ok, _ := st.Current(); !ok || n != 1 {
		t.Errorf("current generation: %d %v, want 1 (kept)", n, ok)
	}
	if _, ok, _ := st.Pending(); ok {
		t.Error("still waiting for confirmation")
	}
	if strings.Contains(out.String(), "NEW SSH connection") {
		t.Errorf("tells the reader to confirm, which the first boot does itself:\n%s", out)
	}
	if !strings.Contains(out.String(), "Confirmed generation 1") {
		t.Errorf("output:\n%s", out)
	}
}

func TestFirstBootWaitsForWANButKeepsTheConfigWithout(t *testing.T) {
	s, path, checks, out := firstBootRouter(t, exampleConfig(t), wanCheck)
	now := s.Now()
	s.Now = func() time.Time { return now }
	slept := time.Duration(0)
	err := firstBoot(s, path, checks, func(d time.Duration) { slept += d; now = now.Add(d + 15*time.Second) })
	if err != nil {
		t.Fatalf("firstBoot: %v\n%s", err, out)
	}
	if slept == 0 {
		t.Error("didn't give WAN any time")
	}
	if n, ok, _ := (apply.Store{Root: s.Root}).Current(); !ok || n != 1 {
		t.Errorf("current generation: %d %v, want 1: a missing internet connection isn't the config's fault", n, ok)
	}
	if !strings.Contains(out.String(), "WAN has no internet address yet") {
		t.Errorf("output doesn't explain WAN:\n%s", out)
	}
}

func TestFirstBootUndoesAConfigThatFailsACheck(t *testing.T) {
	s, path, checks, out := firstBootRouter(t, exampleConfig(t), "40-ssh")
	now := s.Now()
	s.Now = func() time.Time { return now }
	err := firstBoot(s, path, checks, func(d time.Duration) { now = now.Add(d + 15*time.Second) })
	if err == nil || !strings.Contains(err.Error(), "were undone") || !strings.Contains(err.Error(), "rollgate apply") {
		t.Fatalf("firstBoot: %v, want the settings undone and what to do\n%s", err, out)
	}
	st := apply.Store{Root: s.Root}
	if _, ok, _ := st.Current(); ok {
		t.Error("a config that failed a check was kept")
	}
	if _, ok, _ := st.Pending(); ok {
		t.Error("still pending after the undo")
	}
	if !strings.Contains(out.String(), "40-ssh") {
		t.Errorf("output doesn't show the failed check:\n%s", out)
	}
}

func TestFirstBootRefusesAnInvalidConfig(t *testing.T) {
	s, path, checks, _ := firstBootRouter(t, "[system]\nhostname = \"bad name!\"\n")
	err := firstBoot(s, path, checks, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "couldn't be applied") || !strings.Contains(err.Error(), "runs without them") {
		t.Fatalf("firstBoot: %v", err)
	}
	if list, _ := (apply.Store{Root: s.Root}).List(); len(list) != 0 {
		t.Errorf("stored generations for an invalid config: %v", list)
	}
}

func TestFirstBootOK(t *testing.T) {
	r := func(name string, ok bool) health.Result { return health.Result{Name: name, OK: ok} }
	for _, c := range []struct {
		results []health.Result
		want    bool
	}{
		{nil, false},
		{[]health.Result{r("10-firewall", true), r(wanCheck, true)}, true},
		{[]health.Result{r("10-firewall", true), r(wanCheck, false)}, true},
		{[]health.Result{r("10-firewall", false), r(wanCheck, true)}, false},
	} {
		if got := firstBootOK(c.results); got != c.want {
			t.Errorf("firstBootOK(%v) = %v, want %v", c.results, got, c.want)
		}
	}
}

func TestRootHasPassword(t *testing.T) {
	for shadow, want := range map[string]bool{
		"root::20000::::::\nbin:!*:20000::::::\n":     false,
		"root:$y$j9T$abc:20000::::::\n":               true,
		"root:!:20000::::::\n":                        true, // locked on purpose
		"bin::1::::::\nroot:$6$x:20000::::::\n":       true,
		"rooted::1::::::\nroot:$6$x:20000::::::\n":    true,
		"rooted:$6$x:1::::::\nroot::20000::::::\n":    false,
		"daemon:$6$x:1::::::\nroot::20000::::::\nx\n": false,
	} {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, shadowPath), shadow, 0o600)
		if got, err := rootHasPassword(root); err != nil || got != want {
			t.Errorf("rootHasPassword(%q) = %v, %v; want %v", shadow, got, err, want)
		}
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, shadowPath), "bin::1::::::\n", 0o600)
	if _, err := rootHasPassword(root); err == nil {
		t.Error("no error for a shadow file without root")
	}
}

func TestWriteIssue(t *testing.T) {
	root := t.TempDir()
	issue := filepath.Join(root, issuePath)
	if err := writeIssue(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(issue); !os.IsNotExist(err) {
		t.Error("wrote a login prompt text with nothing to say")
	}
	if err := web.SetPassword(root, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	fp, _, err := web.EnsureCert(root, []string{"gateway"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, firstbootPath), "a health check failed, so the router's settings were undone and it runs without them.\n", 0o644)
	if err := writeIssue(root); err != nil {
		t.Fatal(err)
	}
	got := read(t, issue)
	for _, want := range []string{"Web UI: https://", fp, "journalctl -u rollgate-firstboot"} {
		if !strings.Contains(got, want) {
			t.Errorf("login prompt text lacks %q:\n%s", want, got)
		}
	}
}
