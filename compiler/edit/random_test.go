package edit

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The random tests make many edits on the project's real configs. Each
// result must pass Apply's own checks: a CheckFailed is a bug in the
// writer. Refusals and stale entries are allowed answers.

func configs(t testing.TB) map[string][]byte {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"../../examples/*.toml", "../../tests/vm/apply/*.toml", "../../tests/vm/ipv6/*.toml",
		"../../tests/golden/*/router.toml", "../../tests/netns/*.toml", "../../image/vmware/*.toml", "../../tests/edit/*/router.toml"} {
		m, _ := filepath.Glob(pattern)
		paths = append(paths, m...)
	}
	if len(paths) == 0 {
		t.Fatal("no configs found")
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = b
	}
	return out
}

var (
	lists    = []string{"firewall.port_forward", "firewall.policy", "vlan", "interface", "dhcp.lan.static", "dhcp.iot.static", "dhcp.guest.static"}
	sections = []string{"system", "management", "dns", "dhcp.lan", "dhcp.iot", "dhcp.guest", "firewall", "ipv6", ""}
	keys     = []string{"name", "hostname", "protocol", "wan_port", "to", "mac", "ipv4", "comment", "range", "lease_time", "zone", "id", "enabled"}
	strs     = []string{"", "plex", "a \"quoted\" name", "tab\there", "ünïcode", "# not a comment", "[not a header]", "back\\slash", "192.168.10.20:32400"}
)

func randomValue(r *rand.Rand) any {
	switch r.Intn(4) {
	case 0:
		return strs[r.Intn(len(strs))]
	case 1:
		return r.Intn(70000)
	case 2:
		return r.Intn(2) == 0
	}
	n := r.Intn(3)
	v := make([]string, n)
	for i := range v {
		v[i] = strs[r.Intn(len(strs))]
	}
	return v
}

func randomFields(r *rand.Rand) []Field {
	seen := map[string]bool{}
	var fs []Field
	for i := 0; i < 1+r.Intn(4); i++ {
		k := keys[r.Intn(len(keys))]
		if !seen[k] {
			seen[k] = true
			fs = append(fs, Field{k, randomValue(r)})
		}
	}
	return fs
}

// randomEdit picks an edit; for changes and removals it usually names a
// real entry by a real key, as a form would.
func randomEdit(r *rand.Rand, tree map[string]any) Edit {
	table := lists[r.Intn(len(lists))]
	entries, _, _ := list(tree, table, false)
	index := r.Intn(len(entries) + 1)
	switch r.Intn(5) {
	case 0:
		return Add(table, randomFields(r)...)
	case 3:
		return Set(sections[r.Intn(len(sections))], randomFields(r)...)
	case 4:
		return Insert(table, index, randomFields(r)...)
	}
	idKey, idValue := "name", any("nothing")
	if index < len(entries) && r.Intn(5) > 0 {
		if m, ok := entries[index].(map[string]any); ok && len(m) > 0 {
			ks := make([]string, 0, len(m))
			for k := range m {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			idKey = ks[r.Intn(len(ks))]
			idValue = m[idKey]
			if l, ok := idValue.([]any); ok {
				s := make([]string, len(l))
				for i, x := range l {
					s[i], _ = x.(string)
				}
				idValue = s
			}
		}
	}
	if r.Intn(2) == 0 {
		return Remove(table, index, idKey, idValue)
	}
	var remove []string
	if r.Intn(3) == 0 {
		remove = []string{keys[r.Intn(len(keys))]}
	}
	set := randomFields(r)
	for i := 0; i < len(set); i++ {
		if len(remove) > 0 && set[i].Key == remove[0] {
			set = append(set[:i], set[i+1:]...)
		}
	}
	return Change(table, index, idKey, idValue, set, remove...)
}

// try applies 1-3 random edits and fails on a CheckFailed. It returns
// whether the edits applied.
func try(t testing.TB, r *rand.Rand, name string, source []byte) bool {
	tree, err := decode(source)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var edits []Edit
	for i := 0; i < 1+r.Intn(3); i++ {
		edits = append(edits, randomEdit(r, tree))
	}
	_, err = Apply(source, edits...)
	var cf *CheckFailed
	if errors.As(err, &cf) {
		t.Fatalf("%s: %v\nedits: %+v", name, err, edits)
	}
	return err == nil
}

func TestRandomEdits(t *testing.T) {
	r := rand.New(rand.NewSource(16))
	cfgs := configs(t)
	names := make([]string, 0, len(cfgs))
	for n := range cfgs {
		names = append(names, n)
	}
	sort.Strings(names)
	applied := 0
	const rounds = 3000
	for i := 0; i < rounds; i++ {
		n := names[i%len(names)]
		if try(t, r, n, cfgs[n]) {
			applied++
		}
	}
	// Most random edits should go through; a writer that refuses
	// everything would pass the check above.
	if applied < rounds/3 {
		t.Errorf("only %d of %d random edits applied", applied, rounds)
	}
	t.Logf("%d of %d random edits applied", applied, rounds)
}

// FuzzApply is TestRandomEdits driven by the fuzzer: go test -fuzz=FuzzApply ./edit
func FuzzApply(f *testing.F) {
	for i := int64(0); i < 8; i++ {
		f.Add(i, uint8(i))
	}
	cfgs := configs(f)
	names := make([]string, 0, len(cfgs))
	for n := range cfgs {
		names = append(names, n)
	}
	sort.Strings(names)
	f.Fuzz(func(t *testing.T, seed int64, which uint8) {
		n := names[int(which)%len(names)]
		try(t, rand.New(rand.NewSource(seed)), fmt.Sprintf("%s (seed %d)", n, seed), cfgs[n])
	})
}
