package update

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
)

func TestRollback(t *testing.T) {
	f := newTrialFake(t, "/@", Promoted)
	if err := os.RemoveAll(filepath.Join(f.root, topMount, "@next")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := f.system()
	s.Out = &out
	if err := Rollback(s); err != nil {
		t.Fatal(err)
	}
	if f.which("@") != "@previous" || f.which("@previous") != "@" {
		t.Errorf("@ is %s, @previous is %s", f.which("@"), f.which("@previous"))
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "mv --exchange --no-target-directory ROOT/run/rollgate/btrfs/@previous ROOT/run/rollgate/btrfs/@\n") {
		t.Errorf("commands:\n%s", strings.Join(f.cmds, "\n"))
	}
	st, _ := LoadState(f.root)
	if st.State != RolledBack || st.Date != "2026-11-09" || st.From != "2026-11-16" {
		t.Errorf("update.json: %+v", st)
	}
	// The previous system's entry now boots the system that was left.
	if custom := f.read("efi/grub/custom.cfg"); !strings.Contains(custom, "rollgate previous system (2026-11-16)") {
		t.Errorf("custom.cfg:\n%s", custom)
	}
	if !strings.Contains(out.String(), "Reboot to start it") {
		t.Errorf("output:\n%s", out.String())
	}

	// Rolling back again before the reboot undoes it: the running system, now
	// @previous, becomes @ again.
	f.subvol = "/@previous"
	out.Reset()
	s.Out = &out
	if err := Rollback(s); err != nil {
		t.Fatalf("second rollback before the reboot: %v", err)
	}
	if f.which("@") != "@" || f.which("@previous") != "@previous" {
		t.Errorf("after undoing: @ is %s, @previous is %s", f.which("@"), f.which("@previous"))
	}
	if st, _ := LoadState(f.root); st.State != RolledBack || st.Date != "2026-11-16" || st.From != "2026-11-09" {
		t.Errorf("update.json after undoing: %+v", st)
	}
	if !strings.Contains(out.String(), "No reboot is needed") {
		t.Errorf("output:\n%s", out.String())
	}
}

// TestRollbackFromPreviousEntry: booted with GRUB's "rollgate previous system"
// entry, rollgate rollback makes the running system @ for good.
func TestRollbackFromPreviousEntry(t *testing.T) {
	f := newTrialFake(t, "/@previous", Promoted)
	f.write(MirrorlistPath, strings.ReplaceAll(imageList, "11/16", "11/09")) // what runs is @previous
	if err := Rollback(f.system()); err != nil {
		t.Fatal(err)
	}
	if f.which("@") != "@previous" || f.which("@previous") != "@" {
		t.Errorf("@ is %s, @previous is %s", f.which("@"), f.which("@previous"))
	}
	st, _ := LoadState(f.root)
	if st.State != RolledBack || st.Date != "2026-11-09" || st.From != "2026-11-16" {
		t.Errorf("update.json: %+v", st)
	}
	if custom := f.read("efi/grub/custom.cfg"); !strings.Contains(custom, "rollgate previous system (2026-11-16)") {
		t.Errorf("custom.cfg:\n%s", custom)
	}
	// Anything else, such as a snapshot booted for recovery, is refused.
	f = newTrialFake(t, "/@snapshots/3/snapshot", Promoted)
	if err := Rollback(f.system()); err == nil || !strings.Contains(err.Error(), "not @ or @previous") {
		t.Errorf("from a snapshot: %v", err)
	}
}

func TestRollbackRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, err string
		setup     func(f *fakeSystem)
	}{
		{"no previous system", "no previous system", func(f *fakeSystem) {
			if err := os.RemoveAll(filepath.Join(f.root, topMount, "@previous")); err != nil {
				t.Fatal(err)
			}
		}},
		{"previous system can't boot", "has no /boot/vmlinuz-linux", func(f *fakeSystem) {
			if err := os.Remove(filepath.Join(f.root, topMount, "@previous", "boot", "vmlinuz-linux")); err != nil {
				t.Fatal(err)
			}
		}},
		{"trial in progress", "hasn't finished its trial", func(f *fakeSystem) {
			f.write(StatePath, `{"state": "trial", "date": "2026-11-23"}`)
		}},
		{"config waiting", "waiting for rollgate confirm", func(f *fakeSystem) {
			if err := (apply.Store{Root: f.root}).SetPending(2); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		f := newTrialFake(t, "/@", Promoted)
		tc.setup(f)
		err := Rollback(f.system())
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
		}
		if f.which("@") != "@" {
			t.Errorf("%s: @ changed", tc.name)
		}
		if st, _ := LoadState(f.root); st.State == RolledBack {
			t.Errorf("%s: recorded a rollback", tc.name)
		}
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct{ subvol, state, want string }{
		{"/@", "", "Updates: none yet"},
		{"/@next", Trial, "Update to 2026-11-23: on trial since"},
		{"/@", Promoted, "Last update: to 2026-11-23, 2026-11-23 13:00 UTC. The previous system (2026-11-16) is kept"},
		{"/@", Failed, "Last update: to 2026-11-23 failed 2026-11-23 13:00 UTC: the checks failed."},
		{"/@previous", RolledBack, "Reboot to start 2026-11-23."},
	} {
		f := newTrialFake(t, tc.subvol, tc.state)
		if tc.state == "" {
			if err := os.Remove(filepath.Join(f.root, StatePath)); err != nil {
				t.Fatal(err)
			}
		} else {
			f.write(StatePath, `{"state": "`+tc.state+`", "date": "2026-11-23", "from": "2026-11-16", "started": "2026-11-23T12:00:00Z", "finished": "2026-11-23T13:00:00Z", "reason": "the checks failed"}`)
		}
		var out bytes.Buffer
		s := f.system()
		s.Out = &out
		if err := Status(s); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Release: 2026-11-16.") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s %q:\n%s", tc.subvol, tc.state, out.String())
		}
	}
}
