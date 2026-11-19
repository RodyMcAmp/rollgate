package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/health"
)

// Health runs the health checks, waiting for them to pass, and returns the
// last results.
type Health func() ([]health.Result, error)

// Boot is what rollgate-health.service runs at every boot (rollgate health
// -boot, ADR 0011). It finishes whatever an update left:
//
//   - on the trial boot (/ is @next, update.json says trial): wait for the
//     checks; promote if they pass, otherwise record why and reboot, which
//     lands on @;
//   - on a boot of @ after a trial that didn't promote: delete @next and record
//     the update as failed;
//   - on a boot of @ after a promotion that was cut short: finish it.
//
// It returns the last check results, so the caller can print them and fail
// the unit if a check fails.
func Boot(s System, check Health) ([]health.Result, error) {
	st, err := LoadState(s.Root)
	if err != nil {
		return nil, err
	}
	m, err := rootMount(s)
	if errors.Is(err, errNotBtrfs) {
		// A snapshot booted for recovery: no update to finish here.
		return check()
	}
	if err != nil {
		return nil, err
	}
	switch {
	case m.subvol == "/@next" && st.State == Trial:
		// The checks can take minutes; rollgate apply isn't locked out
		// meanwhile, only during the change itself.
		results, err := check()
		if err == nil && health.AllOK(results) {
			return results, locked(s, func() error { return promote(s, st, m) })
		}
		reason := failedChecks(results, err)
		return results, locked(s, func() error { return failTrial(s, st, reason) })
	case m.subvol == "/@" && st.State == Promoting:
		if err := locked(s, func() error { return finishPromotion(s, st, m) }); err != nil {
			return nil, err
		}
	case m.subvol == "/@" && (st.State == Trial || st.State == TrialFailed):
		if err := locked(s, func() error { return cleanUp(s, st, m) }); err != nil {
			return nil, err
		}
	}
	return check()
}

// locked runs f holding the lock that rollgate apply and update take.
func locked(s System, f func() error) error {
	unlock, err := apply.Lock(s.Root)
	if err != nil {
		return err
	}
	defer unlock()
	return f()
}

// failedChecks says which checks failed, for update.json and rollgate status.
func failedChecks(results []health.Result, err error) string {
	if err != nil {
		return "the health checks couldn't run: " + err.Error()
	}
	var failed []string
	for _, r := range results {
		if !r.OK {
			failed = append(failed, r.Name+": "+r.Detail)
		}
	}
	if len(results) == 0 {
		failed = append(failed, "no health checks found")
	}
	return "health checks failed within 5 minutes: " + strings.Join(failed, "; ")
}

// failTrial records why the trial failed and reboots. GRUB already used up
// its one-shot choice, so the reboot lands on @, whose boot cleans up.
func failTrial(s System, st State, reason string) error {
	st.State, st.Reason, st.Finished = TrialFailed, reason, s.Now().UTC()
	if err := saveState(s.Root, st); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "The update to %s failed its trial: %s\nRebooting into %s.\n", st.Date, reason, st.From)
	// GRUB clears next_entry as it boots the trial; clearing it again costs
	// nothing and makes sure this reboot can't land on the trial.
	err := s.run("grub-editenv", filepath.Join(s.Root, grubDir, "grubenv"), "unset", "next_entry")
	if rerr := s.run("systemctl", "reboot", "--no-block"); rerr != nil {
		err = errors.Join(err, rerr)
	}
	if err != nil {
		return err
	}
	return errors.New("the trial failed; rebooting into the previous system")
}

// promote makes the running trial @ (ADR 0011, "Promotion"). @ and @next
// swap names in one atomic step (renameat2 RENAME_EXCHANGE, via mv
// --exchange), so a power cut never leaves the filesystem without an @ to
// boot. update.json says "promoting" from before the swap until the end, with
// @next's subvolume ID, so a boot after a cut can tell whether the swap
// happened (finishPromotion).
func promote(s System, st State, m mount) error {
	id, err := s.output("btrfs", "inspect-internal", "rootid", "/")
	if err != nil {
		return err
	}
	st.State, st.NextID = Promoting, id
	if err := saveState(s.Root, st); err != nil {
		return err
	}
	// Until the swap, a boot lands on the old @, which treats this as a
	// failed trial; @next's fstab may already say @ by then, which is harmless.
	if err := setRootSubvol(filepath.Join(s.Root, fstabPath), "/@"); err != nil {
		return err
	}
	return withTop(s, m, func(top string) error {
		if err := deleteIfExists(s, filepath.Join(top, "@previous")); err != nil {
			return err
		}
		if err := s.run("mv", "--exchange", "--no-target-directory", filepath.Join(top, "@next"), filepath.Join(top, "@")); err != nil {
			return err
		}
		return finish(s, st, m, top)
	})
}

// finishPromotion runs on a boot of @ when a promotion was cut short. If @ is
// the trial (the swap happened), it finishes the promotion. Otherwise this is
// the old system: the update failed.
func finishPromotion(s System, st State, m mount) error {
	id, err := s.output("btrfs", "inspect-internal", "rootid", "/")
	if err != nil {
		return err
	}
	if id != st.NextID {
		st.Reason = "the promotion was cut short before the systems were switched"
		return cleanUp(s, st, m)
	}
	return withTop(s, m, func(top string) error { return finish(s, st, m, top) })
}

// finish completes a promotion once @ is the new system and @next the old
// one: @next becomes @previous, the GRUB entries are rewritten, and the
// update is recorded as promoted.
func finish(s System, st State, m mount, top string) error {
	if _, err := os.Stat(filepath.Join(top, "@next")); err == nil {
		if err := deleteIfExists(s, filepath.Join(top, "@previous")); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(top, "@next"), filepath.Join(top, "@previous")); err != nil {
			return err
		}
	}
	options, err := kernelOptions(s.Root)
	if err != nil {
		return err
	}
	if err := writeCustom(s.Root, top, nil, m.uuid, options); err != nil {
		return err
	}
	st.State, st.NextID, st.Finished = Promoted, "", s.Now().UTC()
	if err := saveState(s.Root, st); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "The update to %s passed its checks and is now the system. The previous system is kept as @previous.\n", st.Date)
	return nil
}

// cleanUp runs on a boot of @ after a trial that didn't promote: it deletes
// @next, removes the trial's GRUB entry and records the update as failed.
func cleanUp(s System, st State, m mount) error {
	if st.Reason == "" {
		st.Reason = "the trial boot never finished its checks (it hung, panicked or lost power)"
	}
	err := withTop(s, m, func(top string) error {
		if err := deleteIfExists(s, filepath.Join(top, "@next")); err != nil {
			return err
		}
		options, err := kernelOptions(s.Root)
		if err != nil {
			return err
		}
		return writeCustom(s.Root, top, nil, m.uuid, options)
	})
	if err != nil {
		return err
	}
	// In case the trial was armed but never booted.
	if err := s.run("grub-editenv", filepath.Join(s.Root, grubDir, "grubenv"), "unset", "next_entry"); err != nil {
		return err
	}
	if st.Finished.IsZero() {
		st.Finished = s.Now().UTC()
	}
	st.State, st.NextID = Failed, ""
	if err := saveState(s.Root, st); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "The update to %s failed: %s. This is still %s.\n", st.Date, st.Reason, st.From)
	return nil
}

// withTop mounts the top of the Btrfs filesystem for f.
func withTop(s System, m mount, f func(top string) error) error {
	top := filepath.Join(s.Root, topMount)
	if err := os.MkdirAll(top, 0o700); err != nil {
		return err
	}
	if err := s.run("mount", "-o", "subvolid=5", m.device, top); err != nil {
		return err
	}
	err := f(top)
	if uerr := s.run("umount", top); err == nil {
		err = uerr
	}
	return err
}

func deleteIfExists(s System, subvol string) error {
	if _, err := os.Stat(subvol); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return s.run("btrfs", "subvolume", "delete", subvol)
}

// output runs a command and returns its trimmed output.
func (s System) output(name string, args ...string) (string, error) {
	out, err := s.Run.Run(name, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
