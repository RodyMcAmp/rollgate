package blocklist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

func TestParse(t *testing.T) {
	in := `# A hosts list
0.0.0.0 ads.example.com tracker.example.net  # two names
127.0.0.1 localhost
::1 ip6-localhost
0.0.0.0 Mixed.Case.Example.ORG.
0.0.0.0 not_valid!.example.com
! Adblock comment
[Adblock Plus 2.0]
||adblock.example.com^
||with.options.example.com^$third-party
||path.example.com^/x
plain.example.io
justaword
10.0.0.1 not-a-block.example.com
`
	got, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ads.example.com", "tracker.example.net", "mixed.case.example.org", "adblock.example.com", "plain.example.io"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse:\n%q\nwant\n%q", got, want)
	}
}

func TestBuild(t *testing.T) {
	lists := [][]string{
		{"ads.example.com", "x.ads.example.com", "tracker.example.net", "shop.example.org", "gw.home.arpa"},
		{"ads.example.com", "cdn.shop.example.org", "b.example.com", "a.example.com"},
	}
	keep := func(n string) bool { return under(n, "home.arpa") }
	got, capped := Build(lists, []string{"shop.example.org"}, keep, 100)
	want := []string{"a.example.com", "ads.example.com", "b.example.com", "tracker.example.net"}
	if !reflect.DeepEqual(got, want) || capped {
		t.Errorf("Build: %q capped=%v, want %q", got, capped, want)
	}
	// The cap keeps names from earlier lists first.
	got, capped = Build(lists, nil, nil, 2)
	if !reflect.DeepEqual(got, []string{"ads.example.com", "tracker.example.net"}) || !capped {
		t.Errorf("Build with a cap: %q capped=%v", got, capped)
	}
}

func TestRender(t *testing.T) {
	got := string(Render([]string{"ads.example.com"}, []string{"Ok.Ads.Example.com."}, "# h\n"))
	want := "# h\nserver:\n\tlocal-zone: \"ads.example.com.\" always_null\n\tlocal-zone: \"ok.ads.example.com.\" transparent\n"
	if got != want {
		t.Errorf("Render:\n%s\nwant\n%s", got, want)
	}
}

// fakeRun records commands; fail makes the named command fail.
type fakeRun struct {
	cmds []string
	fail map[string]bool
}

func (f *fakeRun) Run(name string, args ...string) ([]byte, error) {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.cmds = append(f.cmds, cmd)
	if f.fail[name] {
		return []byte("it broke"), errors.New("exit status 1")
	}
	return nil, nil
}

// router makes a root with a confirmed generation whose router.toml has dns.
func router(t *testing.T, dns string) (string, *fakeRun) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/unbound"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "schema_version = 1\n\n[system]\nhostname = \"gw\"\ndomain = \"home.arpa\"\n\n" +
		"[management]\nallowed_zones = [\"lan\"]\nssh_authorized_keys = [\"admin.pub\"]\n\n" +
		"[[interface]]\nname = \"wan\"\nmatch_device = \"eth0\"\nipv4 = \"dhcp\"\n\n" +
		"[[interface]]\nname = \"lan\"\nmatch_device = \"eth1\"\nipv4 = \"192.168.10.1/24\"\n\n" + dns
	st := apply.Store{Root: root}
	g := apply.Generation{Meta: apply.Meta{Generation: 1, State: apply.Confirmed, Created: time.Now().UTC()}, Source: []byte(src)}
	if err := st.Save(g); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCurrent(1); err != nil {
		t.Fatal(err)
	}
	return root, &fakeRun{fail: map[string]bool{}}
}

func system(root string, run *fakeRun, lists map[string]string) System {
	return System{Root: root, Run: run, Out: io.Discard, Now: func() time.Time { return time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC) },
		Get: func(_ context.Context, url string) (io.ReadCloser, error) {
			body, ok := lists[url]
			if !ok {
				return nil, fmt.Errorf("%s: no such host", url)
			}
			return io.NopCloser(strings.NewReader(body)), nil
		}}
}

func TestUpdate(t *testing.T) {
	root, run := router(t, "[dns]\nblocklists = [\"https://lists.example/a.txt\", \"https://lists.example/b.txt\"]\nallow = [\"good.example.com\"]\n")
	lists := map[string]string{
		"https://lists.example/a.txt": "0.0.0.0 ads.example.com\n0.0.0.0 good.example.com\n0.0.0.0 nas.home.arpa\n",
		"https://lists.example/b.txt": "||tracker.example.net^\n",
	}
	if err := Update(system(root, run, lists)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, strings.TrimPrefix(compile.BlocklistFile, "/"))
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ads.example.com." always_null`, `"tracker.example.net." always_null`, `"good.example.com." transparent`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the list lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "home.arpa") || strings.Contains(string(got), `"good.example.com." always_null`) {
		t.Errorf("a local or allowed name is blocked:\n%s", got)
	}
	if len(run.cmds) != 3 || !strings.HasPrefix(run.cmds[0], "unbound-checkconf ") || run.cmds[1] != "systemctl reload-or-restart unbound.service" ||
		run.cmds[2] != "systemctl kill --signal=HUP dnsmasq.service" {
		t.Errorf("commands: %q, want a check, Unbound's reload, and dnsmasq's cache cleared", run.cmds)
	}
	st, ok := ReadState(root)
	if !ok || st.Names != 2 || len(st.Lists) != 2 || st.Lists[0].Error != "" {
		t.Errorf("state: %+v", st)
	}

	// The next day list b is gone: its last good copy is used.
	delete(lists, "https://lists.example/b.txt")
	run.cmds = nil
	if err := Update(system(root, run, lists)); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(file)
	st, _ = ReadState(root)
	if !strings.Contains(string(got), "tracker.example.net") || !strings.Contains(st.Lists[1].Error, "last good download") {
		t.Errorf("a failed download dropped its list: %+v\n%s", st, got)
	}
	if !strings.Contains(st.Summary(), "1 of 2 list(s) failed") {
		t.Errorf("summary: %s", st.Summary())
	}
}

func TestUpdateKeepsOldListOnFailure(t *testing.T) {
	root, run := router(t, "[dns]\nblocklists = [\"https://lists.example/a.txt\"]\n")
	file := filepath.Join(root, strings.TrimPrefix(compile.BlocklistFile, "/"))
	if err := os.WriteFile(file, []byte("server:\n\tlocal-zone: \"old.example.com.\" always_null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lists := map[string]string{"https://lists.example/a.txt": "new.example.com\n"}

	// unbound-checkconf refuses the new file: the old one stays.
	run.fail["unbound-checkconf"] = true
	if err := Update(system(root, run, lists)); err == nil || !strings.Contains(err.Error(), "old one stays") {
		t.Errorf("Update with a refused file: %v", err)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "old.example.com") {
		t.Errorf("the old list was replaced:\n%s", got)
	}

	// Unbound doesn't take it: the old one comes back.
	run.fail = map[string]bool{"systemctl": true}
	if err := Update(system(root, run, lists)); err == nil || !strings.Contains(err.Error(), "old one is back") {
		t.Errorf("Update with a failed reload: %v", err)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "old.example.com") {
		t.Errorf("the old list didn't come back:\n%s", got)
	}

	// Nothing downloads and no copy was saved: the file stays as it is.
	run.fail = map[string]bool{}
	if err := os.RemoveAll(filepath.Join(root, cacheDir)); err != nil {
		t.Fatal(err)
	}
	if err := Update(system(root, run, nil)); err == nil {
		t.Error("Update with no list at all: no error")
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "old.example.com") {
		t.Errorf("the old list was replaced by nothing:\n%s", got)
	}
}

func TestUpdateWithoutBlocklists(t *testing.T) {
	root, run := router(t, "")
	file := filepath.Join(root, strings.TrimPrefix(compile.BlocklistFile, "/"))
	if err := os.WriteFile(file, []byte("server:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Update(system(root, run, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the list file stays after dns.blocklists was removed")
	}
	// dnsmasq may still have blocked answers cached.
	if len(run.cmds) != 1 || run.cmds[0] != "systemctl kill --signal=HUP dnsmasq.service" {
		t.Errorf("commands without blocklists: %q, want dnsmasq's cache cleared", run.cmds)
	}
	// With no file left, a second run has nothing to clear.
	run.cmds = nil
	if err := Update(system(root, run, nil)); err != nil || len(run.cmds) != 0 {
		t.Errorf("a second run without blocklists: %v %q", err, run.cmds)
	}
}

func TestDownloadLimits(t *testing.T) {
	root, run := router(t, "[dns]\nblocklists = [\"https://lists.example/big\"]\n")
	s := system(root, run, map[string]string{"https://lists.example/big": strings.Repeat("a.example.com\n", MaxBytes/14+2)})
	if _, err := s.download("https://lists.example/big"); err == nil || !strings.Contains(err.Error(), "bigger than") {
		t.Errorf("an oversized list: %v", err)
	}
	s = system(root, run, map[string]string{"https://lists.example/empty": "<html>not a list</html>\n"})
	if _, err := s.download("https://lists.example/empty"); err == nil || !strings.Contains(err.Error(), "no names") {
		t.Errorf("a page that isn't a list: %v", err)
	}
}
