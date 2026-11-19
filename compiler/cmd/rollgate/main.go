// Command rollgate validates, compiles and applies router.toml.
// See docs/architecture.md#config-flow and #safe-apply (ADR 0010).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/update"
)

const usage = `usage: rollgate <command> [arguments]

commands:
  validate [file]          check a config (default ` + config.DefaultPath + `)
  compile -o DIR [-root R] [file]
                           validate, then write the generated system files below
                           DIR (a new or empty directory); changes nothing else.
                           Reads the stored local IPv6 prefix below R (default /)
  diff [-root DIR] [file]  show what installing the compiled files would change
                           below DIR (default /); exit 0 no changes, 1 changes,
                           2 invalid config or error; changes nothing
  schema                   print the JSON Schema for router.toml

changing the router (as root; docs/architecture.md#safe-apply):
  apply [file]             check, install and load a config. It reverts by itself
                           unless confirmed within management.confirm_timeout_seconds
  confirm                  keep the applied config; run it from a NEW SSH connection
                           (or the console), which proves SSH still works
  revert                   undo an unconfirmed apply now
  status                   what is running, what waits for confirmation, whether
                           router.toml has changes that aren't applied, and the
                           release and last update
  rollback-config [N]      put generation N's router.toml back (default: the
                           confirmed one before the current) and apply it
  update [DATE]            update to the newest release (or DATE) in a copy of the
                           system, then reboot into it once as a trial (ADR 0011)
  rollback                 make the system before the last update the system
                           again (it and this one swap places); then reboot

health (docs/architecture.md#health-checks):
  health [-wait SECONDS] [-boot]
                           run the checks in ` + health.Dir + `; with -wait,
                           retry every 10 s until all pass or the time is up.
                           Exit 0 if all pass. -boot (rollgate-health.service):
                           also promote or fail an update's trial boot, or
                           clean up after one (ADR 0011)
  check NAME               run one built-in check (` + "firewall, wan, dns, ssh, services" + `);
                           the scripts in health.d call this
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 success, 1 invalid config or failure, 2 bad usage.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "validate":
		if len(args) > 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		path := config.DefaultPath
		if len(args) == 2 {
			path = args[1]
		}
		if _, _, ok := load(path, stderr); !ok {
			return 1
		}
		fmt.Fprintf(stdout, "%s: valid\n", path)
		return 0
	case "compile":
		return compileCmd(args[1:], stdout, stderr)
	case "diff":
		return diffCmd(args[1:], stdout, stderr)
	case "schema":
		s, err := config.Schema()
		if err != nil || len(args) > 1 {
			fmt.Fprintf(stderr, "rollgate schema: %v\n%s", err, usage)
			return 2
		}
		stdout.Write(s)
		return 0
	case "apply":
		return applyCmd(args[1:], stdout, stderr)
	case "confirm":
		if len(args) > 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return change("confirm", func(s apply.System) error { return apply.Confirm(s) }, stdout, stderr)
	case "revert":
		// --timer and --boot are for the revert timer and rollgate-boot.service.
		modes := map[string]apply.Mode{"": apply.ByHand, "--timer": apply.ByTimer, "--boot": apply.AtBoot}
		opt := ""
		if len(args) == 2 {
			opt = args[1]
		}
		mode, ok := modes[opt]
		if !ok || len(args) > 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return change("revert", func(s apply.System) error { return apply.Revert(s, mode) }, stdout, stderr)
	case "status":
		if len(args) > 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		s := system(stdout)
		if err := apply.Status(s, strings.TrimPrefix(config.DefaultPath, "/")); err != nil {
			fmt.Fprintf(stderr, "rollgate status: %v\n", err)
			return 1
		}
		if err := update.Status(updateSystem(s)); err != nil {
			fmt.Fprintf(stderr, "rollgate status: %v\n", err)
			return 1
		}
		return 0
	case "rollback":
		if len(args) > 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return change("rollback", func(a apply.System) error { return update.Rollback(updateSystem(a)) }, stdout, stderr)
	case "rollback-config":
		n := apply.PreviousConfirmed
		if len(args) == 2 {
			var err error
			if n, err = strconv.Atoi(args[1]); err != nil || n < 0 {
				fmt.Fprint(stderr, usage)
				return 2
			}
		}
		if len(args) > 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return change("rollback-config", func(s apply.System) error {
			if _, err := apply.RollbackConfig(s, strings.TrimPrefix(config.DefaultPath, "/"), n); err != nil {
				return err
			}
			return applyFile(s, config.DefaultPath, stderr)
		}, stdout, stderr)
	case "update":
		if len(args) > 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		want := ""
		if len(args) == 2 {
			want = args[1]
		}
		return updateCmd(want, stdout, stderr)
	case "health":
		return healthCmd(args[1:], stdout, stderr)
	case "check":
		if len(args) != 2 || health.Checks[args[1]] == nil {
			fmt.Fprint(stderr, usage)
			return 2
		}
		env, err := health.SystemEnv("/")
		if err != nil {
			fmt.Fprintf(stderr, "%s: couldn't read the running config: %v\n", args[1], err)
			return 1
		}
		ok, detail := health.Checks[args[1]](env)
		fmt.Fprintln(stdout, detail)
		if !ok {
			return 1
		}
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "rollgate: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// updateCmd is rollgate update [DATE] (ADR 0011): prepare @next, arm its
// trial boot, reboot into it.
func updateCmd(want string, stdout, stderr io.Writer) int {
	return change("update", func(a apply.System) error {
		err := update.Prepare(updateSystem(a), want)
		if errors.Is(err, update.ErrUpToDate) {
			return nil
		}
		if err != nil {
			return err
		}
		if out, err := a.Run.Run("systemctl", "reboot"); err != nil {
			return fmt.Errorf("the trial is armed, but rebooting failed (%v: %s); reboot to start it", err, out)
		}
		return nil
	}, stdout, stderr)
}

// updateSystem is the real router for rollgate update.
func updateSystem(a apply.System) update.System {
	url := os.Getenv("ROLLGATE_RELEASES_URL") // for tests; routers use the default
	if url == "" {
		url = update.DefaultReleasesURL
	}
	return update.System{Root: a.Root, Run: a.Run, Stream: update.StreamExec, Out: a.Out, Now: a.Now, Sleep: time.Sleep,
		Get: update.HTTPGet, Verify: update.GPGVerify(filepath.Join(a.Root, update.PacmanGnupg)),
		URL: url, Free: update.FreeBytes}
}

// healthCmd runs the health checks: exit 0 if all pass, 1 if any fails.
func healthCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Int("wait", 0, "retry for up to this many seconds until all checks pass")
	boot := fs.Bool("boot", false, "at boot: also finish what an update left (ADR 0011)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *wait < 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	const perCheck, interval = 15 * time.Second, 10 * time.Second
	check := func() ([]health.Result, error) {
		return health.Wait(health.Dir, perCheck, interval, time.Duration(*wait)*time.Second, time.Now, time.Sleep)
	}
	var results []health.Result
	var err error
	if *boot {
		code := change("health", func(a apply.System) error {
			results, err = update.Boot(updateSystem(a), check)
			return nil
		}, stdout, stderr)
		if code != 0 {
			return code
		}
	} else {
		results, err = check()
	}
	if err != nil {
		health.Print(stdout, results)
		fmt.Fprintf(stderr, "rollgate health: %v\n", err)
		return 1
	}
	health.Print(stdout, results)
	if !health.AllOK(results) {
		return 1
	}
	return 0
}

// system is the real router: root /, real programs.
func system(stdout io.Writer) apply.System {
	self, err := os.Executable()
	if err != nil {
		self = "/usr/bin/rollgate"
	}
	return apply.System{Root: "/", Run: apply.Exec{}, Out: stdout, Now: time.Now,
		Self: self, SSHConnection: os.Getenv("SSH_CONNECTION")}
}

// change runs a command that changes the router.
func change(name string, f func(apply.System) error, stdout, stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintf(stderr, "rollgate %s: must run as root\n", name)
		return 1
	}
	// A change often cuts the SSH connection it was started from. Finish it
	// anyway: ignore the hangup, and let writes to the dead terminal fail
	// instead of killing the process mid-install.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	if err := f(system(stdout)); err != nil {
		fmt.Fprintf(stderr, "rollgate %s: %v\n", name, err)
		return 1
	}
	return 0
}

func applyCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := config.DefaultPath
	if len(args) == 1 {
		path = args[0]
	}
	return change("apply", func(s apply.System) error { return applyFile(s, path, stderr) }, stdout, stderr)
}

// applyFile validates, compiles and applies the config at path.
func applyFile(s apply.System, path string, stderr io.Writer) error {
	c, source, ok := load(path, stderr)
	if !ok || !localPrefix(c, s.Root, stderr) {
		return errors.New("invalid config; nothing was changed")
	}
	files, err := compile.Compile(c, source, filepath.Base(path))
	if err != nil {
		return err
	}
	return apply.Apply(s, c, source, files)
}

// diffCmd follows diff(1)'s exit codes: 0 no changes, 1 changes, 2 trouble.
func diffCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "/", "directory the files are installed below")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := config.DefaultPath
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	c, source, ok := load(path, stderr)
	if !ok || !localPrefix(c, *root, stderr) {
		return 2
	}
	files, err := compile.Compile(c, source, filepath.Base(path))
	var changes []compile.Change
	if err == nil {
		changes, err = files.Compare(*root)
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate diff: %v\n", err)
		return 2
	}
	for _, ch := range changes {
		fmt.Fprintf(stdout, "%s: %s\n%s\n", ch.Kind, filepath.Join(*root, filepath.FromSlash(ch.Path)), ch.Diff)
	}
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "no changes")
		return 0
	}
	fmt.Fprintf(stdout, "%d file(s) would change\n", len(changes))
	return 1
}

func compileCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "output directory (new or empty)")
	root := fs.String("root", "/", "where to find the stored local IPv6 prefix")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() > 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := config.DefaultPath
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	c, source, ok := load(path, stderr)
	if !ok || !localPrefix(c, *root, stderr) {
		return 1
	}
	files, err := compile.Compile(c, source, filepath.Base(path))
	if err == nil {
		err = files.WriteTree(*out)
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate compile: %v\n", err)
		return 1
	}
	for _, p := range files.Paths() {
		fmt.Fprintln(stdout, filepath.Join(*out, filepath.FromSlash(p)))
	}
	return 0
}

// localPrefix supplies the local IPv6 prefix for ipv6 = "auto" when the config
// doesn't set one: the stored prefix below root, or a new one, which the
// compiled files then store (docs/config-spec.md, "How ipv6 = auto is compiled").
func localPrefix(c *config.Config, root string, stderr io.Writer) bool {
	if c.System.IPv6 != "auto" || c.IPv6.ULA.IsValid() {
		return true
	}
	p, generated, err := compile.LocalPrefix(root)
	if err != nil {
		fmt.Fprintf(stderr, "rollgate: %v\n", err)
		return false
	}
	if generated {
		fmt.Fprintf(stderr, "rollgate: no local IPv6 prefix stored yet; using new prefix %s (applying stores it)\n", p)
	}
	c.IPv6.ULA = p
	return true
}

// load reads and validates a config, printing every problem to stderr.
func load(path string, stderr io.Writer) (*config.Config, []byte, bool) {
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s %v\n", path, config.CodeUnreadable, err)
		return nil, nil, false
	}
	c, err := config.Parse(source)
	var problems config.Problems
	switch {
	case err == nil:
		return c, source, true
	case errors.As(err, &problems):
		for _, p := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", path, p)
		}
		fmt.Fprintf(stderr, "%s: %d problem(s); see docs/config-spec.md#validation-rules\n", path, len(problems))
	default:
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
	}
	return nil, nil, false
}
