package apply

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// tree returns every file below root as path → content.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func plant(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		if err := writeFile(filepath.Join(root, filepath.FromSlash(p)), []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func filesOf(m map[string]string) compile.Files {
	f := compile.Files{}
	for p, c := range m {
		f[p] = []byte(c)
	}
	return f
}

func TestInstallAndRevertToBase(t *testing.T) {
	root := t.TempDir()
	before := map[string]string{
		"etc/nftables.conf": "placeholder firewall\n",
		"etc/dnsmasq.conf":  "arch default\n",
		"etc/systemd/network/99-rollgate-unconfigured.network": "DHCP everywhere\n",
		"etc/systemd/network/50-someone-else.network":          "not ours\n",
	}
	plant(t, root, before)
	want := filesOf(map[string]string{
		"etc/nftables.conf":                           "new firewall\n",
		"etc/dnsmasq.conf":                            "arch default\n", // unchanged: not rewritten
		"etc/systemd/network/30-rollgate-lan.network": "lan\n",
		"etc/rollgate/state/ula-prefix":               "fd12:3456:789a::/48\n",
	})

	base, err := Capture(root, want)
	if err != nil {
		t.Fatal(err)
	}
	if got, w := base.Paths(), []string{
		"etc/dnsmasq.conf", "etc/nftables.conf", "etc/systemd/network/99-rollgate-unconfigured.network",
	}; !slices.Equal(got, w) {
		t.Errorf("generation 0 paths = %v, want %v", got, w)
	}

	changed, err := Install(root, want, base)
	if err != nil {
		t.Fatal(err)
	}
	if w := []string{
		"etc/nftables.conf", "etc/rollgate/state/ula-prefix",
		"etc/systemd/network/30-rollgate-lan.network", "etc/systemd/network/99-rollgate-unconfigured.network",
	}; !slices.Equal(changed, w) {
		t.Errorf("apply changed %v, want %v", changed, w)
	}
	applied := tree(t, root)
	if _, ok := applied["etc/systemd/network/99-rollgate-unconfigured.network"]; ok {
		t.Error("placeholder network file not removed")
	}
	if applied["etc/systemd/network/50-someone-else.network"] != "not ours\n" {
		t.Error("a file rollgate doesn't own was touched")
	}

	if changed, err := Install(root, want, want); err != nil || len(changed) != 0 {
		t.Errorf("installing the same files again changed %v (err %v)", changed, err)
	}

	// Revert: back to exactly the original tree, except that the local IPv6
	// prefix is kept.
	if _, err := Install(root, base, want); err != nil {
		t.Fatal(err)
	}
	wantTree := maps.Clone(before)
	wantTree["etc/rollgate/state/ula-prefix"] = "fd12:3456:789a::/48\n"
	if got := tree(t, root); !maps.Equal(got, wantTree) {
		t.Errorf("after revert:\n got %v\nwant %v", got, wantTree)
	}
}

func TestInstallRefusesUnsafePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for _, p := range []string{"../escape", "/etc/passwd", "etc/../../escape", `etc\x`} {
		if _, err := Install(root, compile.Files{p: []byte("x")}, nil); err == nil {
			t.Errorf("Install accepted %q", p)
		}
		if err := (Store{root}).Save(Generation{Meta: Meta{Generation: 1}, Files: compile.Files{p: nil}}); err == nil {
			t.Errorf("Save accepted %q", p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Error("a file was written outside the root")
	}
}

func TestStore(t *testing.T) {
	s := Store{t.TempDir()}
	if n, err := s.Next(); err != nil || n != 1 {
		t.Fatalf("Next on an empty store = %d, %v", n, err)
	}
	created := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	base := Generation{Meta: Meta{Generation: 0, State: Base, Created: created}, Files: filesOf(map[string]string{"etc/nftables.conf": "old\n"})}
	g1 := Generation{
		Meta:   Meta{Generation: 1, State: Pending, Previous: 0, Created: created, SSHConnection: "10.0.2.2 50000 10.0.2.15 22"},
		Source: []byte("schema_version = 1\n"),
		Files:  filesOf(map[string]string{"etc/nftables.conf": "new\n", "etc/dnsmasq.conf": "dhcp\n"}),
	}
	for _, g := range []Generation{base, g1} {
		if err := s.Save(g); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(g1); err == nil {
		t.Error("saving generation 1 twice should fail")
	}
	// A leftover from an interrupted Save is ignored.
	if err := os.MkdirAll(s.dir("generations", ".tmp-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List()
	if err != nil || len(metas) != 2 || metas[0].Generation != 0 || metas[1].Generation != 1 {
		t.Fatalf("List = %+v, %v", metas, err)
	}
	if n, _ := s.Next(); n != 2 {
		t.Errorf("Next = %d, want 2", n)
	}

	got, err := s.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta != g1.Meta || string(got.Source) != string(g1.Source) || !maps.EqualFunc(got.Files, g1.Files, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Errorf("Load(1) = %+v, want %+v", got, g1)
	}
	if got, err := s.Load(0); err != nil || len(got.Source) != 0 || string(got.Files["etc/nftables.conf"]) != "old\n" {
		t.Errorf("Load(0) = %+v, %v", got, err)
	}

	if err := s.SetState(1, Confirmed); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(1); got.State != Confirmed || got.SSHConnection != g1.SSHConnection {
		t.Errorf("after SetState: %+v", got.Meta)
	}

	if _, ok, err := s.Pending(); ok || err != nil {
		t.Errorf("Pending on a fresh store = %v, %v", ok, err)
	}
	if err := s.SetPending(1); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := s.Pending(); !ok || n != 1 {
		t.Errorf("Pending = %d, %v", n, ok)
	}
	if err := s.ClearPending(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPending(); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
	if _, ok, _ := s.Pending(); ok {
		t.Error("pending not cleared")
	}
	if err := s.SetCurrent(1); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := s.Current(); !ok || n != 1 {
		t.Errorf("Current = %d, %v", n, ok)
	}
	if leftovers := strings.Join(slices.Collect(maps.Keys(tree(t, s.Root))), " "); strings.Contains(leftovers, ".rollgate-") {
		t.Errorf("temporary files left behind: %s", leftovers)
	}
}

func TestPrune(t *testing.T) {
	for _, tc := range []struct {
		keep int
		want []int
	}{
		{keep: 3, want: []int{0, 2, 6}}, // generation 0, current and pending always stay
		{keep: 5, want: []int{0, 2, 4, 5, 6}},
		{keep: 30, want: []int{0, 1, 2, 3, 4, 5, 6}},
	} {
		s := Store{t.TempDir()}
		for n := range 7 {
			if err := s.Save(Generation{Meta: Meta{Generation: n}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetCurrent(2); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPending(6); err != nil {
			t.Fatal(err)
		}
		if err := s.Prune(tc.keep); err != nil {
			t.Fatal(err)
		}
		metas, err := s.List()
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, m := range metas {
			got = append(got, m.Generation)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Prune(%d) kept %v, want %v", tc.keep, got, tc.want)
		}
	}
}
