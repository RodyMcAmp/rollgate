package apply

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

const configPath = "etc/rollgate/router.toml"

var compileULA = netip.MustParsePrefix("fd12:3456:789a::/48")

// applyExample applies the example config with hostname set, writing it to
// router.toml first as a user would, and returns its source.
func applyExample(t *testing.T, s System, hostname string) []byte {
	t.Helper()
	source, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	source = []byte(strings.Replace(string(source), `hostname = "gateway"`, `hostname = "`+hostname+`"`, 1))
	c, err := config.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	c.IPv6.ULA = compileULA
	files, err := compile.Compile(c, source, "router.toml")
	if err != nil {
		t.Fatal(err)
	}
	plant(t, s.Root, map[string]string{configPath: string(source)})
	if err := Apply(s, c, source, files); err != nil {
		t.Fatal(err)
	}
	return source
}

func status(t *testing.T, s System) string {
	t.Helper()
	out := &strings.Builder{}
	s.Out = out
	if err := Status(s, configPath); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func wantLines(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("status lacks %q:\n%s", w, got)
		}
	}
}

func TestStatus(t *testing.T) {
	s, _ := newSystem(t, &fakeRunner{})
	console := s
	console.SSHConnection = ""

	wantLines(t, status(t, s), "Running: the image's default settings. Nothing has been applied yet.",
		"/etc/rollgate/router.toml: missing.")

	applyExample(t, s, "gateway")
	start := s.Now()
	s.Now = func() time.Time { return start.Add(25 * time.Second) }
	wantLines(t, status(t, s),
		"Running: the settings from before the first apply (generation 0).",
		"Waiting for confirmation: generation 1 (applied 2026-11-18 12:00 UTC). It reverts in 95 s unless confirmed.",
		"rollgate confirm, from a NEW SSH connection",
		"/etc/rollgate/router.toml: matches generation 1.",
		"   1  pending    2026-11-18 12:00 UTC",
		"   0  base       2026-11-18 12:00 UTC")

	s.Now = func() time.Time { return start.Add(130 * time.Second) }
	wantLines(t, status(t, s), "Its revert is overdue by 10 s")

	if err := Confirm(console); err != nil {
		t.Fatal(err)
	}
	plant(t, s.Root, map[string]string{configPath: "# edited\n"})
	got := status(t, s)
	wantLines(t, got,
		"Running: generation 1, confirmed (applied 2026-11-18 12:00 UTC).",
		"/etc/rollgate/router.toml: has changes that aren't applied",
		" *   1  confirmed")
	if strings.Contains(got, "Waiting") {
		t.Errorf("nothing is pending:\n%s", got)
	}
}

func TestRollbackConfig(t *testing.T) {
	s, _ := newSystem(t, &fakeRunner{})
	console := s
	console.SSHConnection = ""
	if _, err := RollbackConfig(s, configPath, PreviousConfirmed); err == nil {
		t.Error("rollback with no generations should fail")
	}

	v1 := applyExample(t, s, "gateway")
	if _, err := RollbackConfig(s, configPath, PreviousConfirmed); err == nil || !strings.Contains(err.Error(), "waiting for confirmation") {
		t.Errorf("rollback while pending: %v", err)
	}
	if err := Confirm(console); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackConfig(s, configPath, PreviousConfirmed); err == nil || !strings.Contains(err.Error(), "no earlier confirmed config") {
		t.Errorf("rollback with only one confirmed generation: %v", err)
	}
	// Generation 2 is reverted, generation 3 confirmed: the default skips 2.
	v2 := applyExample(t, s, "router2")
	if err := Revert(s, ByHand); err != nil {
		t.Fatal(err)
	}
	applyExample(t, s, "router3")
	if err := Confirm(console); err != nil {
		t.Fatal(err)
	}
	plant(t, s.Root, map[string]string{configPath: "# my unapplied edit\n"})

	got, err := RollbackConfig(s, configPath, PreviousConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		data, _ := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(p)))
		return string(data)
	}
	if string(got) != string(v1) || read(configPath) != string(v1) {
		t.Error("the default should go back to generation 1: the confirmed one before the current, skipping the reverted 2")
	}
	if read(configPath+".before-rollback") != "# my unapplied edit\n" {
		t.Error("the replaced router.toml wasn't kept")
	}

	if got, err := RollbackConfig(s, configPath, 2); err != nil || string(got) != string(v2) || read(configPath) != string(v2) {
		t.Errorf("rollback to generation 2: %v", err)
	}
	if _, err := RollbackConfig(s, configPath, 0); err == nil || !strings.Contains(err.Error(), "no router.toml") {
		t.Errorf("rollback to generation 0: %v", err)
	}
	if _, err := RollbackConfig(s, configPath, 99); err == nil {
		t.Error("rollback to a missing generation should fail")
	}
}
