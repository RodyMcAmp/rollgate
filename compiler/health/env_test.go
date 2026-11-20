package health

import (
	"os"
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// TestInstalledConfig: the checks test the pending generation during a confirm
// window (ADR 0020's first boot keeps its apply only if they pass), and the
// current one otherwise. RunningConfig stays the confirmed one.
func TestInstalledConfig(t *testing.T) {
	root := t.TempDir()
	st := apply.Store{Root: root}
	example, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), `hostname = "gateway"`) {
		t.Fatal(`examples/router.toml has no hostname = "gateway" to change`)
	}
	save := func(n int, hostname string) {
		t.Helper()
		src := strings.Replace(string(example), `hostname = "gateway"`, `hostname = "`+hostname+`"`, 1)
		if err := st.Save(apply.Generation{Meta: apply.Meta{Generation: n}, Source: []byte(src)}); err != nil {
			t.Fatal(err)
		}
	}
	hostname := func(f func(string) (*config.Config, error)) string {
		t.Helper()
		c, err := f(root)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			return ""
		}
		return c.System.Hostname
	}

	if got := hostname(InstalledConfig); got != "" {
		t.Errorf("before any apply: %q, want none", got)
	}
	save(1, "first")
	if err := st.SetPending(1); err != nil {
		t.Fatal(err)
	}
	if got := hostname(InstalledConfig); got != "first" {
		t.Errorf("first apply pending: installed %q, want first", got)
	}
	if got := hostname(RunningConfig); got != "" {
		t.Errorf("first apply pending: running %q, want none until it's confirmed", got)
	}
	if err := st.SetCurrent(1); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearPending(); err != nil {
		t.Fatal(err)
	}
	save(2, "second")
	if err := st.SetPending(2); err != nil {
		t.Fatal(err)
	}
	if got := hostname(InstalledConfig); got != "second" {
		t.Errorf("second apply pending: installed %q, want second", got)
	}
	if got := hostname(RunningConfig); got != "first" {
		t.Errorf("second apply pending: running %q, want first", got)
	}
	if err := st.ClearPending(); err != nil {
		t.Fatal(err)
	}
	if got := hostname(InstalledConfig); got != "first" {
		t.Errorf("after the revert: installed %q, want first", got)
	}
}
