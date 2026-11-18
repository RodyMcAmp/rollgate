package compile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

func TestUnifiedDiff(t *testing.T) {
	// Two changes eight unchanged lines apart: more than 2×3 lines of context,
	// so they become two hunks.
	old := []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n")
	new := []byte("a\nb\nc\nD\ne\nf\ng\nh\ni\nj\nk\nl\nm\n")
	want := "--- installed /x\n+++ compiled /x\n" +
		"@@ -1,7 +1,7 @@\n a\n b\n c\n-d\n+D\n e\n f\n g\n" +
		"@@ -10,3 +10,4 @@\n j\n k\n l\n+m\n"
	if got := unifiedDiff("x", old, new); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := unifiedDiff("x", nil, []byte("one\n")); got != "--- installed /x\n+++ compiled /x\n@@ -1,0 +1,1 @@\n+one\n" {
		t.Errorf("new file diff:\n%s", got)
	}
}

func TestCompare(t *testing.T) {
	source, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Compile(c, source, "router.toml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := files.WriteTree(root); err != nil {
		t.Fatal(err)
	}
	if changes, err := files.Compare(root); err != nil || len(changes) != 0 {
		t.Fatalf("freshly installed files should match: %v %+v", err, changes)
	}

	// Change one file, delete another, and leave a stale file from an old config.
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/sysctl.d/90-rollgate.conf", "net.ipv4.ip_forward = 0\n")
	if err := os.Remove(filepath.Join(root, "etc/dnsmasq.conf")); err != nil {
		t.Fatal(err)
	}
	write("etc/systemd/network/30-rollgate-guest.network", "[Match]\nName=guest\n")
	write("etc/systemd/network/50-someone-else.network", "not ours\n")

	changes, err := files.Compare(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ch := range changes {
		got[ch.Path] = ch.Kind
		if ch.Diff == "" {
			t.Errorf("%s has no diff", ch.Path)
		}
	}
	want := map[string]string{
		"etc/sysctl.d/90-rollgate.conf":                 "changed",
		"etc/dnsmasq.conf":                              "new",
		"etc/systemd/network/30-rollgate-guest.network": "removed",
	}
	if len(got) != len(want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
	for p, kind := range want {
		if got[p] != kind {
			t.Errorf("%s: got %q, want %q", p, got[p], kind)
		}
	}
}
