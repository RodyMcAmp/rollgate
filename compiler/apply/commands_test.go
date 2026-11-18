package apply

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// placeholders is what the image has installed before the first apply.
var placeholders = map[string]string{
	"etc/nftables.conf": "# placeholder firewall\n",
	"etc/systemd/network/99-rollgate-unconfigured.network": "[Match]\nName=en*\n[Network]\nDHCP=yes\n",
	"etc/dnsmasq.conf":                       "# Configuration file for dnsmasq.\n",
	"etc/unbound/unbound.conf":               "server:\n",
	"etc/ssh/sshd_config.d/10-rollgate.conf": "PasswordAuthentication no\n",
}

const applySession = "10.0.2.2 50000 10.0.2.15 22"

// flakyRunner fails the first call of one program, then behaves like fake.
type flakyRunner struct {
	*fakeRunner
	failFirst string
	failed    bool
}

func (f *flakyRunner) Run(name string, args ...string) ([]byte, error) {
	if name == f.failFirst && !f.failed {
		f.failed = true
		f.calls = append(f.calls, append([]string{name}, args...))
		return []byte("boom"), errors.New("exit status 1")
	}
	return f.fakeRunner.Run(name, args...)
}

func newSystem(t *testing.T, r Runner) (System, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	plant(t, root, placeholders)
	writeSecret(t, root, "admin.pub", "ssh-ed25519 AAAA admin\n")
	out := &bytes.Buffer{}
	return System{
		Root: root, Run: r, Out: out, Self: "/usr/bin/rollgate", SSHConnection: applySession,
		Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	}, out
}

// systemFiles is the installed system config: the tree without rollgate's
// own state, secrets and lock.
func systemFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := tree(t, root)
	maps.DeleteFunc(files, func(p, _ string) bool {
		return strings.HasPrefix(p, "var/") || strings.HasPrefix(p, "run/") || strings.HasPrefix(p, "etc/rollgate/")
	})
	return files
}

func stringsOf(f compile.Files) map[string]string {
	m := map[string]string{}
	for p, data := range f {
		m[p] = string(data)
	}
	return m
}

func callIndex(calls [][]string, cmd ...string) int {
	return slices.IndexFunc(calls, func(c []string) bool { return slices.Equal(c, cmd) })
}

func TestApplyThenConfirm(t *testing.T) {
	r := &fakeRunner{}
	s, out := newSystem(t, r)
	c, files := compileExample(t)
	if err := Apply(s, c, []byte("source v1"), files); err != nil {
		t.Fatal(err)
	}

	st := s.store()
	base, err := st.Load(0)
	if err != nil || base.State != Base || !maps.Equal(stringsOf(base.Files), placeholders) {
		t.Errorf("generation 0 = %+v, %v; want the placeholders", base.Files.Paths(), err)
	}
	g, err := st.Load(1)
	if err != nil || g.State != Pending || g.Previous != 0 || g.SSHConnection != applySession || string(g.Source) != "source v1" {
		t.Errorf("generation 1 = %+v, %v", g.Meta, err)
	}
	if n, ok, _ := st.Pending(); !ok || n != 1 {
		t.Errorf("pending = %d, %v", n, ok)
	}
	installed := systemFiles(t, s.Root)
	for p, data := range files {
		if !strings.HasPrefix(p, "etc/rollgate/") && installed[p] != string(data) {
			t.Errorf("%s not installed", p)
		}
	}
	if _, ok := installed["etc/systemd/network/99-rollgate-unconfigured.network"]; ok {
		t.Error("placeholder networking not removed")
	}

	timer := callIndex(r.calls, "systemd-run", "--unit=rollgate-revert", "--description=rollgate: revert an unconfirmed change",
		"--on-active=120", "--timer-property=AccuracySec=1s", "/usr/bin/rollgate", "revert", "--timer")
	firewall := callIndex(r.calls, "nft", "-f", "/etc/nftables.conf")
	if timer < 0 || firewall < 0 || timer > firewall {
		t.Errorf("the revert timer must start before anything is loaded (timer at %d, nft -f at %d): %q", timer, firewall, r.calls)
	}
	if !strings.Contains(out.String(), "rollgate confirm") || !strings.Contains(out.String(), "within 120 seconds") {
		t.Errorf("apply didn't say how to confirm:\n%s", out)
	}

	if err := Apply(s, c, []byte("source v2"), files); err == nil || !strings.Contains(err.Error(), "still waiting") {
		t.Errorf("a second apply while one is pending: %v", err)
	}

	if err := Confirm(s); err == nil || !strings.Contains(err.Error(), "new SSH connection") {
		t.Errorf("confirm from the applying connection: %v", err)
	}
	if _, ok, _ := st.Pending(); !ok {
		t.Fatal("a refused confirm cleared the pending generation")
	}

	other := s
	other.SSHConnection = "10.0.2.2 50001 10.0.2.15 22" // same client, new connection
	if err := Confirm(other); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := st.Current(); !ok || n != 1 {
		t.Errorf("current = %d, %v", n, ok)
	}
	if _, ok, _ := st.Pending(); ok {
		t.Error("still pending after confirm")
	}
	if g, _ := st.Load(1); g.State != Confirmed {
		t.Errorf("state = %s", g.State)
	}
	if last := r.calls[len(r.calls)-1]; !slices.Equal(last, []string{"systemctl", "stop", "rollgate-revert.timer"}) {
		t.Errorf("confirm didn't stop the timer; last command %q", last)
	}
	if err := Confirm(other); err == nil {
		t.Error("confirming twice should fail")
	}
}

func TestRevertByTimerAndByHand(t *testing.T) {
	r := &fakeRunner{}
	s, _ := newSystem(t, r)
	console := s
	console.SSHConnection = ""
	c, files := compileExample(t)
	if err := Apply(s, c, []byte("v1"), files); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(console); err != nil { // the console may always confirm
		t.Fatal(err)
	}
	confirmed := systemFiles(t, s.Root)

	c.System.Hostname = "router2"
	files2, err := compile.Compile(c, []byte("v2"), "router.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(s, c, []byte("v2"), files2); err != nil {
		t.Fatal(err)
	}
	if maps.Equal(systemFiles(t, s.Root), confirmed) {
		t.Fatal("the second apply changed nothing; the test needs a real change")
	}
	r.calls = nil
	if err := Revert(s, ByTimer); err != nil {
		t.Fatal(err)
	}
	if got := systemFiles(t, s.Root); !maps.Equal(got, confirmed) {
		t.Error("the timer's revert didn't restore generation 1's files")
	}
	if len(r.calls) == 0 {
		t.Error("revert reloaded nothing")
	}
	st := s.store()
	if g, _ := st.Load(2); g.State != Reverted {
		t.Errorf("generation 2 state = %s", g.State)
	}
	if n, _, _ := st.Current(); n != 1 {
		t.Errorf("current = %d, want 1", n)
	}
	if _, ok, _ := st.Pending(); ok {
		t.Error("still pending after revert")
	}

	// The timer firing after a confirm or revert is normal and does nothing;
	// a revert by hand with nothing pending says so.
	if err := Revert(s, ByTimer); err != nil {
		t.Errorf("timer with nothing pending: %v", err)
	}
	if err := Revert(s, ByHand); err == nil {
		t.Error("revert by hand with nothing pending should fail")
	}
}

func TestRevertAtBoot(t *testing.T) {
	s, _ := newSystem(t, &fakeRunner{})
	c, files := compileExample(t)
	if err := Apply(s, c, []byte("v1"), files); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	s.Run = r
	if err := Revert(s, AtBoot); err != nil {
		t.Fatal(err)
	}
	if got := systemFiles(t, s.Root); !maps.Equal(got, placeholders) {
		t.Errorf("boot revert left:\n%v", got)
	}
	// The example renames ports by MAC (.link files), so udev re-applies names
	// from the restored files. Then only enable/disable, without waiting:
	// nothing else runs this early (no nft, sysctl or networkctl).
	want := []string{
		"udevadm control --reload", "udevadm trigger --action=add --subsystem-match=net", "udevadm settle",
		"systemctl disable --now --no-block unbound.service", "systemctl disable --now --no-block dnsmasq.service",
	}
	if got := planStrings(r.calls); !slices.Equal(got, want) {
		t.Errorf("boot revert ran %q, want %q", got, want)
	}
}

func TestApplyFailures(t *testing.T) {
	t.Run("checks fail: nothing changes, nothing is stored", func(t *testing.T) {
		s, _ := newSystem(t, &fakeRunner{fail: map[string]bool{"dnsmasq": true}})
		c, files := compileExample(t)
		if err := Apply(s, c, []byte("v1"), files); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("err = %v", err)
		}
		if metas, _ := s.store().List(); len(metas) != 0 {
			t.Errorf("stored %v", metas)
		}
		if got := systemFiles(t, s.Root); !maps.Equal(got, placeholders) {
			t.Error("files changed")
		}
	})

	t.Run("timer can't start: nothing is installed", func(t *testing.T) {
		s, _ := newSystem(t, &fakeRunner{fail: map[string]bool{"systemd-run": true}})
		c, files := compileExample(t)
		if err := Apply(s, c, []byte("v1"), files); err == nil || !strings.Contains(err.Error(), "nothing was installed") {
			t.Errorf("err = %v", err)
		}
		if got := systemFiles(t, s.Root); !maps.Equal(got, placeholders) {
			t.Error("files changed")
		}
		if g, _ := s.store().Load(1); g.State != Reverted {
			t.Errorf("generation 1 state = %s", g.State)
		}
		if _, ok, _ := s.store().Pending(); ok {
			t.Error("left pending")
		}
	})

	t.Run("a reload fails: reverted at once", func(t *testing.T) {
		s, _ := newSystem(t, &flakyRunner{fakeRunner: &fakeRunner{}, failFirst: "networkctl"})
		c, files := compileExample(t)
		err := Apply(s, c, []byte("v1"), files)
		if err == nil || !strings.Contains(err.Error(), "previous settings were restored") || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v", err)
		}
		if got := systemFiles(t, s.Root); !maps.Equal(got, placeholders) {
			t.Errorf("not reverted:\n%v", got)
		}
		if _, ok, _ := s.store().Pending(); ok {
			t.Error("left pending")
		}
	})

	t.Run("the revert fails too: stays pending for the next boot", func(t *testing.T) {
		s, _ := newSystem(t, &fakeRunner{fail: map[string]bool{"networkctl": true}})
		c, files := compileExample(t)
		err := Apply(s, c, []byte("v1"), files)
		if err == nil || !strings.Contains(err.Error(), "reboot the router") {
			t.Errorf("err = %v", err)
		}
		if n, ok, _ := s.store().Pending(); !ok || n != 1 {
			t.Error("generation 1 should stay pending, so rollgate-boot.service reverts it")
		}
	})
}
