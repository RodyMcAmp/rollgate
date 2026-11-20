package apply

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// historyLines is how many generations Status lists.
const historyLines = 10

// Status prints what the router is running, any change waiting for
// confirmation and the time left, whether router.toml (configPath, below
// the root) has changes that aren't applied, and the recent generations.
// It changes nothing and needs no lock.
func Status(s System, configPath string) error {
	st := s.store()
	metas, err := st.List()
	if err != nil {
		return err
	}
	current, hasCurrent, err := st.Current()
	if err != nil {
		return err
	}
	pending, hasPending, err := st.Pending()
	if err != nil {
		return err
	}

	// The generation whose router.toml is in effect: the pending one if any.
	var running *Generation
	switch {
	case hasCurrent:
		g, err := st.Load(current)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "Running: generation %d, confirmed (applied %s).\n", current, when(g.Created))
		running = &g
	case len(metas) > 0:
		fmt.Fprintln(s.Out, "Running: the settings from before the first apply (generation 0). No change has been confirmed yet.")
	default:
		fmt.Fprintln(s.Out, "Running: the image's default settings. Nothing has been applied yet.")
	}

	if hasPending {
		g, err := st.Load(pending)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "Waiting for confirmation: generation %d (applied %s). ", pending, when(g.Created))
		if deadline, ok := revertDeadline(g); ok {
			left := deadline.Sub(s.Now())
			if left > 0 {
				fmt.Fprintf(s.Out, "It reverts in %d s unless confirmed.\n", int(left.Round(time.Second)/time.Second))
			} else {
				fmt.Fprintf(s.Out, "Its revert is overdue by %d s: run rollgate revert, or reboot the router.\n", int(-left.Round(time.Second)/time.Second))
			}
		} else {
			fmt.Fprintln(s.Out, "It reverts unless confirmed.")
		}
		fmt.Fprintln(s.Out, "  Keep it: rollgate confirm, from a NEW SSH connection or the console. Undo it now: rollgate revert.")
		running = &g
	}

	shown := "/" + filepath.ToSlash(configPath)
	source, err := os.ReadFile(filepath.Join(s.Root, configPath))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(s.Out, "%s: missing.\n", shown)
	case err != nil:
		return err
	case running == nil:
		fmt.Fprintf(s.Out, "%s: not applied yet (rollgate apply applies it).\n", shown)
	case bytes.Equal(source, running.Source):
		fmt.Fprintf(s.Out, "%s: matches generation %d.\n", shown, running.Generation)
	default:
		fmt.Fprintf(s.Out, "%s: has changes that aren't applied (rollgate diff shows them, rollgate apply applies them).\n", shown)
	}

	if len(metas) > 0 {
		fmt.Fprintln(s.Out, "\nGenerations (newest first):")
		for i := len(metas) - 1; i >= 0 && i >= len(metas)-historyLines; i-- {
			m := metas[i]
			mark := " "
			if hasCurrent && m.Generation == current {
				mark = "*"
			}
			fmt.Fprintf(s.Out, " %s %3d  %-9s  %s\n", mark, m.Generation, m.State, when(m.Created))
		}
		fmt.Fprintln(s.Out, "(* = running. rollgate rollback-config N goes back to generation N's config.)")
	}
	return nil
}

// Waiting is a change waiting for confirmation.
type Waiting struct {
	Generation int
	// Deadline is when the revert timer undoes it; zero if its stored
	// router.toml can't be read.
	Deadline time.Time
}

// PendingChange returns the change waiting for confirmation, if there is one.
// It changes nothing and needs no lock.
func PendingChange(s System) (Waiting, bool, error) {
	st := s.store()
	n, ok, err := st.Pending()
	if err != nil || !ok {
		return Waiting{}, false, err
	}
	g, err := st.Load(n)
	if err != nil {
		return Waiting{}, false, err
	}
	w := Waiting{Generation: n}
	w.Deadline, _ = revertDeadline(g)
	return w, true, nil
}

// revertDeadline is when the revert timer undoes pending generation g: its
// apply time plus its own confirm timeout.
func revertDeadline(g Generation) (time.Time, bool) {
	c, err := config.Parse(g.Source)
	if err != nil {
		return time.Time{}, false
	}
	return g.Created.Add(time.Duration(c.Management.ConfirmTimeoutSeconds) * time.Second), true
}

func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// PreviousConfirmed asks RollbackConfig for the confirmed generation before
// the current one.
const PreviousConfirmed = -1

// RollbackConfig puts generation n's router.toml back at configPath (below
// the root) and returns it, for the caller to apply through commit-confirm
// like any other change (ADR 0010). n = PreviousConfirmed means the newest
// confirmed generation before the current one. The replaced router.toml is
// kept as router.toml.before-rollback.
func RollbackConfig(s System, configPath string, n int) ([]byte, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	st := s.store()

	if p, ok, err := st.Pending(); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("generation %d is still waiting for confirmation: confirm it or run rollgate revert first", p)
	}
	if n == PreviousConfirmed {
		n = 0
		current, ok, err := st.Current()
		if err != nil {
			return nil, err
		}
		metas, err := st.List()
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			if m.Generation > 0 && m.State == Confirmed && (!ok || m.Generation < current) {
				n = m.Generation // the newest such, since metas is oldest first
			}
		}
		if n == 0 {
			return nil, errors.New("there is no earlier confirmed config to go back to (rollgate status lists the generations)")
		}
	}
	if n == 0 {
		return nil, errors.New("generation 0 is what was installed before the first apply; it has no router.toml to go back to")
	}
	g, err := st.Load(n)
	if err != nil {
		return nil, fmt.Errorf("no generation %d to go back to (rollgate status lists them): %w", n, err)
	}

	target := filepath.Join(s.Root, configPath)
	if old, err := os.ReadFile(target); err == nil && !bytes.Equal(old, g.Source) {
		if err := writeFile(target+".before-rollback", old); err != nil {
			return nil, err
		}
		fmt.Fprintf(s.Out, "Saved the replaced config as /%s.before-rollback.\n", filepath.ToSlash(configPath))
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := writeFile(target, g.Source); err != nil {
		return nil, err
	}
	fmt.Fprintf(s.Out, "Put generation %d's config back in /%s. Applying it:\n", n, filepath.ToSlash(configPath))
	return g.Source, nil
}
