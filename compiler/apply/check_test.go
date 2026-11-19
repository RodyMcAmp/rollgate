package apply

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// fakeRunner records commands and fails the programs listed in fail. For
// each argument that names a file, it records the file's content at the
// time of the call, to show the checker saw the compiled file, and follows
// sshd-style "Include <path>" lines in those files.
type fakeRunner struct {
	fail  map[string]bool
	calls [][]string
	seen  map[string]string // path → content when a command ran
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.seen == nil {
		f.seen = map[string]string{}
	}
	var record func(path string)
	record = func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		f.seen[path] = string(data)
		for _, line := range strings.Split(string(data), "\n") {
			if inc, ok := strings.CutPrefix(line, "Include "); ok {
				record(inc)
			}
		}
	}
	for _, a := range args {
		record(strings.TrimPrefix(a, "--conf-file="))
	}
	if f.fail[name] {
		return []byte(name + ": error on line 3\nsecond line"), errors.New("exit status 1")
	}
	return nil, nil
}

// compileExample compiles examples/router.toml.
func compileExample(t *testing.T) (*config.Config, compile.Files) {
	t.Helper()
	source, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	c.IPv6.ULA = netip.MustParsePrefix("fd12:3456:789a::/48")
	files, err := compile.Compile(c, source, "router.toml")
	if err != nil {
		t.Fatal(err)
	}
	return c, files
}

func writeSecret(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(SecretsDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrecheckRunsEachChecker(t *testing.T) {
	c, files := compileExample(t)
	root := t.TempDir()
	writeSecret(t, root, "admin.pub", "ssh-ed25519 AAAA admin\n")
	r := &fakeRunner{}
	if err := Precheck(r, root, c, files); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	var programs []string
	for _, call := range r.calls {
		programs = append(programs, call[0])
	}
	if want := []string{"nft", "dnsmasq", "unbound-checkconf", "sshd"}; !slices.Equal(programs, want) {
		t.Fatalf("ran %v, want %v", programs, want)
	}
	// Each checker got the compiled file, from a staging directory.
	for _, p := range []string{nftablesConf, dnsmasqConf, unboundConf, sshdConf} {
		found := false
		for path, content := range r.seen {
			if strings.HasSuffix(filepath.ToSlash(path), p) && content == string(files[p]) {
				found = true
			}
		}
		if !found {
			t.Errorf("no checker saw the compiled %s", p)
		}
	}
	// sshd reads the new file first, then the system's main config.
	var main string
	for path, content := range r.seen {
		if filepath.Base(path) == "sshd_config" {
			main = content
		}
	}
	lines := strings.Split(strings.TrimSpace(main), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], filepath.FromSlash(sshdConf)) ||
		lines[1] != "Include "+filepath.Join(root, "etc/ssh/sshd_config") {
		t.Errorf("sshd main config:\n%s", main)
	}
	// The staging directory is gone afterwards.
	for path := range r.seen {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("staged file %s left behind", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc/nftables.conf")); err == nil {
		t.Error("Precheck installed a file")
	}
}

func TestPrecheckReportsEveryProblem(t *testing.T) {
	c, files := compileExample(t)
	c.Management.SSHAuthorizedKeys = []string{"admin.pub", "laptop.pub", "phone.pub"}
	root := t.TempDir()
	writeSecret(t, root, "admin.pub", "ssh-ed25519 AAAA admin\n")
	writeSecret(t, root, "phone.pub", "") // empty: as bad as missing
	r := &fakeRunner{fail: map[string]bool{"nft": true, "sshd": true}}

	err := Precheck(r, root, c, files)
	if err == nil {
		t.Fatal("Precheck passed")
	}
	msg := err.Error()
	for _, want := range []string{
		"firewall: nft rejected", "nft: error on line 3", "    second line",
		"SSH: sshd rejected", `"laptop.pub"`, `"phone.pub"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"dnsmasq rejected", `"admin.pub"`} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("error wrongly contains %q:\n%s", unwanted, msg)
		}
	}
	if len(r.calls) != 4 {
		t.Errorf("a failing checker stopped the others: ran %v", r.calls)
	}
}
