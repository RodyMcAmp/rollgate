package apply

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// Keep is how many generations Apply keeps (ADR 0010).
const Keep = 30

// TimerUnit is the transient systemd timer that reverts an unconfirmed change.
const TimerUnit = "rollgate-revert"

// System is everything the commands touch, so tests can run them below a
// temporary root with a fake Runner.
type System struct {
	Root string    // "/" on the router
	Run  Runner    // runs nft, systemctl, systemd-run, ...
	Out  io.Writer // progress and instructions for the user
	Now  func() time.Time
	// Self is the rollgate binary the revert timer runs (os.Executable).
	Self string
	// SSHConnection is the caller's $SSH_CONNECTION: client address and port,
	// server address and port. Empty on the local console.
	SSHConnection string
}

func (s System) store() Store { return Store{s.Root} }

// lock serializes apply, confirm and revert, including a revert started by
// the timer while someone runs confirm. It returns the unlock function.
func (s System) lock() (func(), error) {
	dir := filepath.Join(s.Root, "run/rollgate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Apply installs a compiled config with commit-confirm (ADR 0010): check it,
// store it as a pending generation, start the revert timer, install the
// files and reload what changed. If installing or reloading fails, it
// reverts at once. It returns without waiting for confirmation.
func Apply(s System, c *config.Config, source []byte, files compile.Files) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st := s.store()

	if n, ok, err := st.Pending(); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("generation %d is still waiting for confirmation: run rollgate confirm (from a new SSH connection) or rollgate revert first", n)
	}
	if err := Precheck(s.Run, s.Root, c, files); err != nil {
		return fmt.Errorf("the new config didn't pass its checks; nothing was changed:\n%w", err)
	}

	prev, err := s.revertTarget(files)
	if err != nil {
		return err
	}
	n, err := st.Next()
	if err != nil {
		return err
	}
	g := Generation{
		Meta: Meta{Generation: n, State: Pending, Previous: prev.Generation,
			Created: s.Now().UTC(), SSHConnection: s.SSHConnection},
		Source: source,
		Files:  files,
	}
	if err := st.Save(g); err != nil {
		return err
	}
	if err := st.SetPending(n); err != nil {
		return err
	}

	// The timer starts before any system file changes, so from here on an
	// unconfirmed change always comes back out.
	timeout := c.Management.ConfirmTimeoutSeconds
	if err := s.startTimer(timeout); err != nil {
		return errors.Join(fmt.Errorf("couldn't start the revert timer, so nothing was installed: %w", err),
			st.SetState(n, Reverted), st.ClearPending())
	}

	changed, err := Install(s.Root, files, prev.Files)
	if err == nil {
		err = Reload(s.Run, ReloadPlan(changed, prev.Files, files))
	}
	if err != nil {
		rerr := s.revert(n, false)
		if rerr == nil {
			return fmt.Errorf("applying generation %d failed, so the previous settings were restored: %w", n, err)
		}
		return errors.Join(fmt.Errorf("applying generation %d failed: %w", n, err),
			fmt.Errorf("restoring the previous settings also failed; reboot the router to restore them: %w", rerr))
	}
	if err := st.Prune(Keep); err != nil {
		fmt.Fprintf(s.Out, "warning: couldn't remove old generations: %v\n", err)
	}

	if len(changed) == 0 {
		fmt.Fprintf(s.Out, "Applied generation %d (no files changed).\n", n)
	} else {
		fmt.Fprintf(s.Out, "Applied generation %d. Changed:\n", n)
		for _, p := range changed {
			fmt.Fprintf(s.Out, "  /%s\n", p)
		}
	}
	fmt.Fprintf(s.Out, "\nTo keep it, open a NEW SSH connection (or use the console) and run:\n"+
		"  rollgate confirm\n"+
		"within %d seconds. Otherwise the previous settings come back by themselves.\n", timeout)
	return nil
}

// revertTarget returns the generation a revert restores: the current one,
// or generation 0, captured from the installed files if it doesn't exist yet.
func (s System) revertTarget(files compile.Files) (Generation, error) {
	st := s.store()
	if n, ok, err := st.Current(); err != nil {
		return Generation{}, err
	} else if ok {
		return st.Load(n)
	}
	if g, err := st.Load(0); err == nil {
		return g, nil
	}
	base, err := Capture(s.Root, files)
	if err != nil {
		return Generation{}, err
	}
	g := Generation{Meta: Meta{Generation: 0, State: Base, Created: s.Now().UTC()}, Files: base}
	return g, st.Save(g)
}

func (s System) startTimer(seconds int) error {
	// A unit left over from an earlier change would make systemd-run fail.
	// Nothing is pending (Apply checked), so stopping it is harmless.
	_, _ = s.Run.Run("systemctl", "stop", TimerUnit+".timer", TimerUnit+".service")
	_, _ = s.Run.Run("systemctl", "reset-failed", TimerUnit+".timer", TimerUnit+".service")
	out, err := s.Run.Run("systemd-run", "--unit="+TimerUnit,
		"--description=rollgate: revert an unconfirmed change",
		"--on-active="+strconv.Itoa(seconds), "--timer-property=AccuracySec=1s",
		s.Self, "revert", "--timer")
	if err != nil {
		return fmt.Errorf("systemd-run: %v%s", err, indent(out))
	}
	return nil
}

// Confirm keeps the pending generation. It refuses to run from the SSH
// connection that ran apply: only a new connection proves that management
// access still works (an established connection can outlive a change that
// blocks new ones).
func Confirm(s System) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st := s.store()

	n, ok, err := st.Pending()
	if err != nil {
		return err
	} else if !ok {
		return errors.New("nothing is waiting for confirmation (it may already have been reverted: see rollgate status)")
	}
	g, err := st.Load(n)
	if err != nil {
		return err
	}
	if s.SSHConnection != "" && s.SSHConnection == g.SSHConnection {
		return errors.New("this is the SSH connection that ran rollgate apply, so it can't prove the change kept SSH working.\n" +
			"Open a new SSH connection to the router and run rollgate confirm there (or use the console)")
	}
	if err := st.SetState(n, Confirmed); err != nil {
		return err
	}
	if err := st.SetCurrent(n); err != nil {
		return err
	}
	if err := st.ClearPending(); err != nil {
		return err
	}
	// If the timer can't be stopped it still fires, finds nothing pending
	// and does nothing.
	if out, err := s.Run.Run("systemctl", "stop", TimerUnit+".timer"); err != nil {
		fmt.Fprintf(s.Out, "note: couldn't stop the revert timer (%v%s); when it fires it will find nothing to revert.\n", err, indent(out))
	}
	fmt.Fprintf(s.Out, "Confirmed generation %d. It stays in place, including after a reboot.\n", n)
	return nil
}

// Mode says who asked for a revert.
type Mode int

const (
	ByHand  Mode = iota // rollgate revert: an error if nothing is pending
	ByTimer             // the revert timer: nothing pending is normal (already confirmed)
	AtBoot              // rollgate-boot.service, before networking and services start
)

// Revert restores the generation before the pending one.
//
// At boot (rollgate-boot.service runs before nftables, networkd and the
// services start) it puts the files back, re-applies port renames if .link
// files changed (udev named the ports before /var/lib/rollgate was mounted),
// and adjusts which services are enabled; everything else then starts from
// the restored files by itself.
func Revert(s System, mode Mode) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	n, ok, err := s.store().Pending()
	if err != nil {
		return err
	}
	if !ok {
		if mode == ByHand {
			return errors.New("nothing is waiting for confirmation, so there's nothing to revert (rollgate rollback-config goes back to an earlier confirmed config)")
		}
		return nil
	}
	if err := s.revert(n, mode == AtBoot); err != nil {
		return err
	}
	if mode == ByHand {
		_, _ = s.Run.Run("systemctl", "stop", TimerUnit+".timer")
	}
	return nil
}

// revert restores generation n's predecessor. The caller holds the lock.
// Every reload command is tried even if one fails, since getting the
// firewall, networking and SSH back matters more than stopping early. If
// anything fails, n stays pending, so the next boot tries again.
func (s System) revert(n int, atBoot bool) error {
	st := s.store()
	g, err := st.Load(n)
	if err != nil {
		return err
	}
	prev, err := st.Load(g.Previous)
	if err != nil {
		return err
	}
	changed, err := Install(s.Root, prev.Files, g.Files)
	if err != nil {
		return fmt.Errorf("restoring generation %d: %w", prev.Generation, err)
	}
	plan := ReloadPlan(changed, g.Files, prev.Files)
	if atBoot {
		plan = bootPlan(plan)
	}
	var errs []error
	for _, cmd := range plan {
		if out, err := s.Run.Run(cmd[0], cmd[1:]...); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v%s", strings.Join(cmd, " "), err, indent(out)))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("restored generation %d's files, but reloading failed:\n%w", prev.Generation, err)
	}
	if err := st.SetState(n, Reverted); err != nil {
		return err
	}
	if err := st.ClearPending(); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "Reverted generation %d: generation %d's settings are back.\n", n, prev.Generation)
	return nil
}

// bootPlan keeps the parts of a reload plan that matter before the services
// start: udev renames (networkd hasn't brought any port up yet, so renaming
// is safe) and enable/disable, without waiting for jobs, since a unit
// ordered before the services can't wait for them to start or stop. A
// disable also cancels the service's start in this boot.
func bootPlan(plan [][]string) [][]string {
	var out [][]string
	for _, cmd := range plan {
		switch {
		case cmd[0] == "udevadm":
			out = append(out, cmd)
		case slices.Equal(cmd[:2], []string{"systemctl", "enable"}):
			out = append(out, cmd)
		case len(cmd) == 4 && slices.Equal(cmd[:3], []string{"systemctl", "disable", "--now"}):
			out = append(out, []string{"systemctl", "disable", "--now", "--no-block", cmd[3]})
		}
	}
	return out
}
