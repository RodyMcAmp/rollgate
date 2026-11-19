package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/health"
)

var (
	pass = []health.Result{{Name: "10-firewall", OK: true, Detail: "loaded"}, {Name: "20-wan", OK: true, Detail: "up"}}
	fail = []health.Result{{Name: "10-firewall", OK: true, Detail: "loaded"}, {Name: "20-wan", OK: false, Detail: "no default route"}}
)

// newTrialFake is a router during or after an update: @ is the old system
// (2026-11-16), @next the update (2026-11-23), @previous an older one, each
// with a "which" file naming it.
func newTrialFake(t *testing.T, subvol, state string) *fakeSystem {
	f := newFake(t)
	f.subvol, f.rootid = subvol, "257"
	for name, date := range map[string]string{"@": "11/16", "@next": "11/23", "@previous": "11/09"} {
		dir := filepath.Join(topMount, name)
		f.write(filepath.Join(dir, "which"), name)
		f.write(filepath.Join(dir, MirrorlistPath), strings.ReplaceAll(imageList, "11/16", date))
		f.write(filepath.Join(dir, fstabPath), imageTab)
		for _, b := range []string{"vmlinuz-linux", "initramfs-linux.img"} {
			f.write(filepath.Join(dir, "boot", b), b)
		}
	}
	// The running system's fstab, as rollgate update left it in @next.
	f.write(fstabPath, strings.Replace(imageTab, "subvol=/@ ", "subvol=/@next ", 1))
	f.write(StatePath, `{"state": "`+state+`", "date": "2026-11-23", "from": "2026-11-16", "next_id": "257"}`)
	return f
}

func (f *fakeSystem) which(subvol string) string {
	data, err := os.ReadFile(filepath.Join(f.root, topMount, subvol, "which"))
	if err != nil {
		return "missing"
	}
	return string(data)
}

func checks(results []health.Result, called *bool) Health {
	return func() ([]health.Result, error) {
		*called = true
		return results, nil
	}
}

func TestBootTrialPasses(t *testing.T) {
	f := newTrialFake(t, "/@next", Trial)
	var called bool
	results, err := Boot(f.system(), checks(pass, &called))
	if err != nil || !called || !health.AllOK(results) {
		t.Fatalf("Boot: %v, checks called %v", err, called)
	}
	if f.which("@") != "@next" || f.which("@previous") != "@" || f.which("@next") != "missing" {
		t.Errorf("after promotion: @ is %s, @previous is %s, @next is %s", f.which("@"), f.which("@previous"), f.which("@next"))
	}
	if got := f.read(fstabPath); !strings.Contains(got, "subvol=/@ ") {
		t.Errorf("fstab still points at @next:\n%s", got)
	}
	cmds := strings.Join(f.cmds, "\n")
	if !strings.Contains(cmds, "btrfs subvolume delete ROOT/run/rollgate/btrfs/@previous\nmv --exchange --no-target-directory ROOT/run/rollgate/btrfs/@next ROOT/run/rollgate/btrfs/@") {
		t.Errorf("commands:\n%s", cmds)
	}
	st, _ := LoadState(f.root)
	if st.State != Promoted || st.NextID != "" || st.Finished.IsZero() {
		t.Errorf("update.json: %+v", st)
	}
	custom := f.read("efi/grub/custom.cfg")
	if strings.Contains(custom, TrialEntry) || !strings.Contains(custom, "menuentry 'rollgate previous system (2026-11-16)' --id rollgate-previous") ||
		!strings.Contains(custom, "linux /@previous/boot/vmlinuz-linux root=UUID="+testUUID+" rw rootflags=subvol=@previous console=tty0") ||
		strings.Contains(custom, "panic=10") {
		t.Errorf("custom.cfg:\n%s", custom)
	}
}

func TestBootTrialFails(t *testing.T) {
	f := newTrialFake(t, "/@next", Trial)
	var called bool
	_, err := Boot(f.system(), checks(fail, &called))
	if err == nil {
		t.Fatal("a failed trial returned no error")
	}
	st, _ := LoadState(f.root)
	if st.State != TrialFailed || !strings.Contains(st.Reason, "20-wan: no default route") {
		t.Errorf("update.json: %+v", st)
	}
	want := "grub-editenv ROOT/efi/grub/grubenv unset next_entry\nsystemctl reboot --no-block"
	if cmds := strings.Join(f.cmds, "\n"); !strings.HasSuffix(cmds, want) || strings.Contains(cmds, "mv ") {
		t.Errorf("commands:\n%s", cmds)
	}
	if f.which("@") != "@" || f.which("@next") != "@next" {
		t.Error("the subvolumes changed")
	}
	// The checks couldn't run at all: also a failure.
	f = newTrialFake(t, "/@next", Trial)
	broken := func() ([]health.Result, error) { return nil, errors.New("no health.d") }
	if _, err := Boot(f.system(), broken); err == nil {
		t.Error("checks that couldn't run: no error")
	}
	if st, _ := LoadState(f.root); st.State != TrialFailed || !strings.Contains(st.Reason, "couldn't run") {
		t.Errorf("update.json: %+v", st)
	}
}

func TestBootCleansUp(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{
		{TrialFailed, "health checks failed"}, // recorded by the trial
		{Trial, "never finished its checks"},  // the trial hung or panicked
	} {
		f := newTrialFake(t, "/@", tc.state)
		if tc.state == TrialFailed {
			f.write(StatePath, `{"state": "trial-failed", "date": "2026-11-23", "from": "2026-11-16", "reason": "health checks failed within 5 minutes: 20-wan: no default route"}`)
		}
		var called bool
		if _, err := Boot(f.system(), checks(pass, &called)); err != nil || !called {
			t.Fatalf("%s: %v, checks called %v", tc.state, err, called)
		}
		if f.which("@next") != "missing" || f.which("@") != "@" || f.which("@previous") != "@previous" {
			t.Errorf("%s: @next %s, @ %s, @previous %s", tc.state, f.which("@next"), f.which("@"), f.which("@previous"))
		}
		st, _ := LoadState(f.root)
		if st.State != Failed || !strings.Contains(st.Reason, tc.reason) || st.Finished.IsZero() {
			t.Errorf("%s: update.json %+v", tc.state, st)
		}
		custom := f.read("efi/grub/custom.cfg")
		if strings.Contains(custom, TrialEntry) || !strings.Contains(custom, "rollgate previous system (2026-11-09)") {
			t.Errorf("%s: custom.cfg:\n%s", tc.state, custom)
		}
		if !strings.Contains(strings.Join(f.cmds, "\n"), "grub-editenv ROOT/efi/grub/grubenv unset next_entry") {
			t.Errorf("%s: next_entry not cleared", tc.state)
		}
	}
}

func TestBootFinishesPromotion(t *testing.T) {
	// Cut after the swap: @ is the update (ID 257, as recorded), @next the old system.
	f := newTrialFake(t, "/@", Promoting)
	top := filepath.Join(f.root, topMount)
	if err := os.RemoveAll(filepath.Join(top, "@previous")); err != nil { // deleted before the swap
		t.Fatal(err)
	}
	if _, err := f.Run("mv", "--exchange", "--no-target-directory", filepath.Join(top, "@next"), filepath.Join(top, "@")); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	var called bool
	if _, err := Boot(f.system(), checks(pass, &called)); err != nil || !called {
		t.Fatalf("%v, checks called %v", err, called)
	}
	if f.which("@") != "@next" || f.which("@previous") != "@" || f.which("@next") != "missing" {
		t.Errorf("@ %s, @previous %s, @next %s", f.which("@"), f.which("@previous"), f.which("@next"))
	}
	if st, _ := LoadState(f.root); st.State != Promoted {
		t.Errorf("update.json: %+v", st)
	}

	// Cut before the swap: @ is still the old system (another ID).
	f = newTrialFake(t, "/@", Promoting)
	f.rootid = "256"
	if _, err := Boot(f.system(), checks(pass, &called)); err != nil {
		t.Fatal(err)
	}
	if f.which("@") != "@" || f.which("@next") != "missing" {
		t.Errorf("@ %s, @next %s", f.which("@"), f.which("@next"))
	}
	if st, _ := LoadState(f.root); st.State != Failed || !strings.Contains(st.Reason, "cut short") {
		t.Errorf("update.json: %+v", st)
	}
}

// TestBootNothingToDo: a normal boot, and a boot of @next that isn't a trial,
// only run the checks.
func TestBootNothingToDo(t *testing.T) {
	for _, tc := range []struct{ subvol, state string }{
		{"/@", Promoted}, {"/@", Failed}, {"/@", ""}, {"/@next", Promoted}, {"/@previous", Trial},
		{"overlay", Trial}, // a read-only snapshot booted for recovery (make test-rollback)
	} {
		f := newTrialFake(t, tc.subvol, tc.state)
		var called bool
		results, err := Boot(f.system(), checks(fail, &called))
		if err != nil || !called || health.AllOK(results) {
			t.Errorf("%s %q: %v, called %v", tc.subvol, tc.state, err, called)
		}
		if len(f.cmds) != 1 {
			t.Errorf("%s %q: ran %q", tc.subvol, tc.state, f.cmds)
		}
		if f.which("@") != "@" || f.which("@next") != "@next" {
			t.Errorf("%s %q: subvolumes changed", tc.subvol, tc.state)
		}
	}
}

// TestPrepareKeepsPreviousEntry: a trial's custom.cfg still offers @previous.
func TestPrepareKeepsPreviousEntry(t *testing.T) {
	f := newFake(t)
	at := filepath.Join(topMount, "@previous")
	f.write(filepath.Join(at, MirrorlistPath), strings.ReplaceAll(imageList, "11/16", "11/09"))
	for _, b := range []string{"vmlinuz-linux", "initramfs-linux.img"} {
		f.write(filepath.Join(at, "boot", b), b)
	}
	if err := Prepare(f.system(), ""); err != nil {
		t.Fatal(err)
	}
	custom := f.read("efi/grub/custom.cfg")
	trial, previous := strings.Index(custom, "--id "+TrialEntry), strings.Index(custom, "--id "+PreviousEntry)
	if trial < 0 || previous < trial {
		t.Errorf("custom.cfg:\n%s", custom)
	}
}
