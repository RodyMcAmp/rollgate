package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/update"
	"github.com/RodyMcAmp/rollgate/compiler/web"
)

// apiCmd is rollgate api (ADR 0014): the local API the web UI uses, on the
// socket rollgate-api.socket passes, or on a new socket for development.
func apiCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "serve on a new Unix socket at this path instead of systemd's")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate api: must run as root")
		return 1
	}
	// A change can cut the connection that asked for it: finish it anyway.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	l, err := api.Listener(*listen)
	if err != nil {
		fmt.Fprintf(stderr, "rollgate api: %v\n", err)
		return 1
	}
	log.SetFlags(0)
	log.SetOutput(stderr)
	log.Printf("rollgate api: serving on %s", l.Addr())
	if err := api.Serve(l, apiBackend()); err != nil {
		fmt.Fprintf(stderr, "rollgate api: %v\n", err)
		return 1
	}
	return 0
}

// apiBackend is the router behind each API request: the CLI commands' code.
func apiBackend() api.Backend {
	sys := func(w io.Writer, connection string) apply.System {
		s := system(w)
		s.SSHConnection = connection
		return s
	}
	invalid := errors.New("invalid config; nothing was changed")
	return api.Backend{
		Status: func(w io.Writer) error { return status(sys(w, ""), w) },
		Health: func(w io.Writer) error {
			results, err := health.RunDir(health.Dir, 15*time.Second)
			health.Print(w, results)
			if err == nil && !health.AllOK(results) {
				err = errors.New("not every health check passes")
			}
			return err
		},
		Config: func() ([]byte, error) { return os.ReadFile(config.DefaultPath) },
		Pending: func() (*api.Pending, error) {
			p, ok, err := apply.PendingChange(sys(io.Discard, ""))
			if err != nil || !ok {
				return nil, err
			}
			return &api.Pending{Generation: p.Generation, Deadline: p.Deadline}, nil
		},
		Validate: func(source []byte, w io.Writer) error {
			if _, ok := parse(source, "router.toml", w); !ok {
				return errors.New("invalid config")
			}
			fmt.Fprintln(w, "router.toml: valid")
			return nil
		},
		Diff: func(source []byte, w io.Writer) error {
			c, ok := parse(source, "router.toml", w)
			if !ok || !localPrefix(c, "/", w) {
				return errors.New("invalid config")
			}
			_, err := diff(c, source, filepath.Base(config.DefaultPath), "/", w)
			return err
		},
		Apply: func(source []byte, connection string, w io.Writer) error {
			if _, ok := parse(source, "router.toml", w); !ok {
				return invalid
			}
			// router.toml is the user's file, like an edit from the CLI: if the
			// apply is refused or reverted, the edit stays, and rollgate status
			// says it isn't applied.
			if err := writeConfig(config.DefaultPath, source); err != nil {
				return err
			}
			return applyFile(sys(w, connection), config.DefaultPath, w)
		},
		Confirm: func(connection string, w io.Writer) error { return apply.Confirm(sys(w, connection)) },
		Revert:  func(w io.Writer) error { return apply.Revert(sys(w, ""), apply.ByHand) },
		Update:  func(date string, w io.Writer) error { return updateRouter(sys(w, ""), date) },
		// Like update, a rollback from the web UI reboots by itself: there is
		// no shell to type reboot in.
		Rollback: func(w io.Writer) error {
			u := updateSystem(sys(w, ""))
			if err := update.Rollback(u); err != nil {
				return err
			}
			if !update.RebootPending(u) {
				return nil
			}
			fmt.Fprintln(w, "Rebooting now.")
			if out, err := u.Run.Run("systemctl", "reboot"); err != nil {
				return fmt.Errorf("the rollback is done, but rebooting failed (%v: %s); reboot to start it", err, out)
			}
			return nil
		},
	}
}

// status is rollgate status: the config generations, the release and last
// update, and the clock.
func status(s apply.System, w io.Writer) error {
	if err := apply.Status(s, strings.TrimPrefix(config.DefaultPath, "/")); err != nil {
		return err
	}
	if err := update.Status(updateSystem(s)); err != nil {
		return err
	}
	_, clock := update.Clock(s.Run)
	fmt.Fprintf(w, "Clock: %s.\n", clock)
	fmt.Fprintln(w, web.Status(s.Root))
	return nil
}

// diff prints what installing c's compiled files below root would change, and
// returns how many files would. name is the config's file name, which the
// generated files' headers mention.
func diff(c *config.Config, source []byte, name, root string, w io.Writer) (int, error) {
	files, err := compile.Compile(c, source, name)
	if err != nil {
		return 0, err
	}
	changes, err := files.Compare(root)
	if err != nil {
		return 0, err
	}
	for _, ch := range changes {
		fmt.Fprintf(w, "%s: %s\n%s\n", ch.Kind, filepath.Join(root, filepath.FromSlash(ch.Path)), ch.Diff)
	}
	if len(changes) == 0 {
		fmt.Fprintln(w, "no changes")
	} else {
		fmt.Fprintf(w, "%d file(s) would change\n", len(changes))
	}
	return len(changes), nil
}

// writeConfig replaces path in one step, so a power cut leaves the old or the new file.
func writeConfig(path string, source []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, source, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
