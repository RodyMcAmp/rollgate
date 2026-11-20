package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
)

// RolledBack is the update.json state after rollgate rollback.
const RolledBack = "rolled-back"

// Rollback is rollgate rollback (ADR 0011): @ and @previous swap places, in
// one atomic step, so the next boot is the previous system and this one is
// kept as @previous (rollgate rollback again goes forward). The running
// system keeps running until the reboot.
//
// Run from @previous (booted with GRUB's "rollgate previous system" entry, or
// after a rollback before its reboot), the same swap makes the running system
// @: it stays the system, and no reboot is needed.
func Rollback(s System) error {
	unlock, err := apply.Lock(s.Root)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := LoadState(s.Root)
	if err != nil {
		return err
	}
	if st.unfinished() {
		return fmt.Errorf("the update to %s hasn't finished its trial yet", st.Date)
	}
	if n, ok, err := (apply.Store{Root: s.Root}).Pending(); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("config generation %d is waiting for rollgate confirm; confirm or revert it first", n)
	}
	m, err := rootMount(s)
	if err != nil {
		return err
	}
	fromPrevious := m.subvol == "/@previous"
	if m.subvol != "/@" && !fromPrevious {
		return fmt.Errorf("the running system is %s, not @ or @previous: rollback works only between those two", m.subvol)
	}
	running, err := RunningRelease(s.Root)
	if err != nil {
		return err
	}
	options, err := kernelOptions(s.Root)
	if err != nil {
		return err
	}
	// other is the system that isn't running: the one that becomes @, or,
	// from @previous, the one that stops being @.
	other, otherDate := "@previous", ""
	if fromPrevious {
		other = "@"
	}
	err = withTop(s, m, func(top string) error {
		if _, err := os.Stat(filepath.Join(top, "@previous")); errors.Is(err, os.ErrNotExist) {
			return errors.New("there is no previous system to go back to")
		}
		if otherDate, err = RunningRelease(filepath.Join(top, other)); err != nil {
			return fmt.Errorf("%s: %w", other, err)
		}
		if !fromPrevious {
			if _, err := bootEntry(top, "@previous", "", "", m.uuid, nil, false); err != nil {
				return fmt.Errorf("not rolling back: %w", err)
			}
		}
		if err := s.run("mv", "--exchange", "--no-target-directory", filepath.Join(top, "@previous"), filepath.Join(top, "@")); err != nil {
			return err
		}
		// The previous system's entry now boots the one that stopped being @.
		return writeCustom(s.Root, top, nil, m.uuid, options)
	})
	if err != nil {
		return err
	}
	now := s.Now().UTC()
	st = State{State: RolledBack, Date: otherDate, From: running, Started: now, Finished: now}
	if fromPrevious {
		st.Date, st.From = running, otherDate
	}
	if err := saveState(s.Root, st); err != nil {
		return err
	}
	if fromPrevious {
		fmt.Fprintf(s.Out, "This system (%s) is now @ and stays the system; the one it replaces (%s) is kept as @previous. No reboot is needed.\n", running, otherDate)
		return nil
	}
	fmt.Fprintf(s.Out, "The previous system (%s) is now @, and this one (%s) is kept as @previous.\n", otherDate, running)
	fmt.Fprintln(s.Out, "Reboot to start it: reboot")
	return nil
}

// Status prints the release the router runs and what the last update or
// rollback did, for rollgate status.
func Status(s System) error {
	st, err := LoadState(s.Root)
	if err != nil {
		return err
	}
	// Status is for reading: a system that isn't laid out like a router (no
	// archive date, / not on Btrfs) still gets the rest.
	if running, err := RunningRelease(s.Root); err == nil {
		fmt.Fprintf(s.Out, "\nRelease: %s.\n", running)
	} else {
		fmt.Fprintf(s.Out, "\nRelease: unknown (%v).\n", err)
	}
	m, _ := rootMount(s)
	switch st.State {
	case "":
		fmt.Fprintln(s.Out, "Updates: none yet (rollgate update installs the newest release).")
	case Trial:
		fmt.Fprintf(s.Out, "Update to %s: on trial since %s. It becomes the system once its health checks pass.\n", st.Date, at(st.Started))
	case TrialFailed:
		fmt.Fprintf(s.Out, "Update to %s: failed its trial (%s). The next boot cleans up.\n", st.Date, st.Reason)
	case Promoting:
		fmt.Fprintf(s.Out, "Update to %s: passed its trial and is being made the system.\n", st.Date)
	case Promoted:
		fmt.Fprintf(s.Out, "Last update: to %s, %s. The previous system (%s) is kept: rollgate rollback goes back to it.\n", st.Date, at(st.Finished), st.From)
	case Failed:
		fmt.Fprintf(s.Out, "Last update: to %s failed %s: %s.\n", st.Date, at(st.Finished), st.Reason)
	case RolledBack:
		fmt.Fprintf(s.Out, "Last rollback: %s, from %s back to %s. rollgate rollback again goes forward.\n", at(st.Finished), st.From, st.Date)
		if m.subvol == "/@previous" {
			fmt.Fprintf(s.Out, "  Reboot to start %s.\n", st.Date)
		}
	default:
		fmt.Fprintf(s.Out, "Last update: in an unknown state %q (/%s).\n", st.State, StatePath)
	}
	return nil
}

// RebootPending reports whether a rollback waits for a reboot: the running
// system is no longer @ (the test Status uses for "Reboot to start").
func RebootPending(s System) bool {
	m, err := rootMount(s)
	return err == nil && m.subvol == "/@previous"
}

func at(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
