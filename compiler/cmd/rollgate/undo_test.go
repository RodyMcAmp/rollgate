package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newUndo(t *testing.T, content string) webUndo {
	t.Helper()
	root := t.TempDir()
	u := webUndo{root: root, config: "etc/rollgate/router.toml"}
	for _, d := range []string{"etc/rollgate", "var/lib/rollgate"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(u.configPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return u
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUndoRestores(t *testing.T) {
	u := newUndo(t, "# applied\n")
	if err := u.keep(7, []byte("# before\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := u.restore(7, []byte("# applied\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := read(t, u.configPath()); got != "# before\n" {
		t.Errorf("router.toml is %q, want the file from before the change", got)
	}
	if !strings.Contains(out.String(), "back as it was") {
		t.Errorf("output: %q", out.String())
	}
	if _, err := os.Stat(u.saved(7)); !os.IsNotExist(err) {
		t.Error("the copy is still there after Undo")
	}
}

func TestUndoKeepsLaterEdits(t *testing.T) {
	u := newUndo(t, "# edited over SSH after the apply\n")
	if err := u.keep(7, []byte("# before\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := u.restore(7, []byte("# applied\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := read(t, u.configPath()); got != "# edited over SSH after the apply\n" {
		t.Errorf("Undo overwrote a later edit: %q", got)
	}
	if !strings.Contains(out.String(), "edited after the change") {
		t.Errorf("output: %q", out.String())
	}
}

func TestUndoWithoutCopy(t *testing.T) {
	// A change applied at the CLI, undone in the web UI: the file stays.
	u := newUndo(t, "# applied\n")
	var out bytes.Buffer
	if err := u.restore(7, []byte("# applied\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := read(t, u.configPath()); got != "# applied\n" || !strings.Contains(out.String(), "wasn't applied from the web UI") {
		t.Errorf("router.toml %q, output %q", got, out.String())
	}
}

func TestUndoClearAndForget(t *testing.T) {
	u := newUndo(t, "x = 1\n")
	for _, n := range []int{3, 4} {
		if err := u.keep(n, []byte("old\n")); err != nil {
			t.Fatal(err)
		}
	}
	u.forget(4)
	if _, err := os.Stat(u.saved(4)); !os.IsNotExist(err) {
		t.Error("forget left the copy")
	}
	if err := u.clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.saved(3)); !os.IsNotExist(err) {
		t.Error("clear left an old copy")
	}
}
