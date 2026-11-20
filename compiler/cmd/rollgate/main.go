// Command rollgate validates, compiles and applies router.toml.
// See docs/architecture.md#config-flow and #safe-apply (ADR 0010).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/blocklist"
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
                           router.toml has changes that aren't applied, the
                           release and last update, and whether the clock is
                           synchronized
  rollback-config [N]      put generation N's router.toml back (default: the
                           confirmed one before the current) and apply it
  update [DATE]            update to the newest release (or DATE) in a copy of the
                           system, then reboot into it once as a trial (ADR 0011)
  rollback                 make the system before the last update the system
                           again (it and this one swap places); then reboot.
                           Booted from the "rollgate previous system" menu entry,
                           it keeps the running system instead

  vpn [status]             the VPN (ADR 0018): its port, the router's public key,
                           and when each device last connected
  vpn key                  make the router's WireGuard key if there is none, and
                           print its public key
  vpn add [--home-only] NAME
                           add a device: make its keys, add it to router.toml,
                           apply, and print its configuration (shown only once).
                           --home-only: only home traffic goes through the VPN
  vpn remove NAME          remove a device from router.toml and apply
  dns update               download dns.blocklists and give them to Unbound
                           (ADR 0019; rollgate-blocklists.timer runs it daily)

health (docs/architecture.md#health-checks):
  health [-wait SECONDS] [-boot]
                           run the checks in ` + health.Dir + `; with -wait,
                           retry every 10 s until all pass or the time is up.
                           Exit 0 if all pass. -boot (rollgate-health.service):
                           also promote or fail an update's trial boot, or
                           clean up after one (ADR 0011)
  check NAME               run one built-in check (` + "firewall, wan, dns, ssh, services" + `);
                           the scripts in health.d call this

the router's password (ADR 0020):
  password [--stdin]       change the router's password, for the console (root)
                           and the web UI (asked twice, or one line from stdin)
  firstboot                apply router.toml at the first boot and keep it if the
                           health checks pass (rollgate-firstboot.service runs it)
  setup [-- PROGRAM ARGS]  on a console: ask for the router's password if root has
                           none yet, then run PROGRAM (the getty units run it, with
                           agetty as PROGRAM)

web UI (ADR 0014):
  web-password [--stdin]   set only the web UI's admin password (asked twice, or one
                           line from stdin), make its HTTPS certificate if there
                           is none (prints its fingerprint), and turn the web UI on
  web                      the web UI's server (rollgate-web.service runs it)
  api [-listen PATH]       serve the local API the web UI uses, on the socket
                           rollgate-api.socket passes (or a new one at PATH)
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
		if err := status(system(stdout), stdout); err != nil {
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
	case "vpn":
		return vpnCmd(args[1:], stdout, stderr)
	case "dns":
		if len(args) != 2 || args[1] != "update" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		if os.Geteuid() != 0 {
			fmt.Fprintln(stderr, "rollgate dns update: must run as root")
			return 1
		}
		s := system(stdout)
		if err := blocklist.Update(blocklist.System{Root: s.Root, Run: s.Run, Get: blocklist.HTTPGet, Now: time.Now, Out: stdout}); err != nil {
			fmt.Fprintf(stderr, "rollgate dns update: %v\n", err)
			return 1
		}
		return 0
	case "health":
		return healthCmd(args[1:], stdout, stderr)
	case "api":
		return apiCmd(args[1:], stderr)
	case "password":
		return passwordCmd(args[1:], stdout, stderr)
	case "firstboot":
		return firstbootCmd(args[1:], stdout, stderr)
	case "setup":
		return setupCmd(args[1:], stdout, stderr)
	case "web-password":
		return webPasswordCmd(args[1:], stdout, stderr)
	case "web":
		return webCmd(args[1:], stderr)
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
	return change("update", func(a apply.System) error { return updateRouter(a, want) }, stdout, stderr)
}

// updateRouter prepares @next for want ("" for the newest release), arms its
// trial boot and reboots into it. Already up to date isn't an error.
func updateRouter(a apply.System, want string) error {
	err := update.Prepare(updateSystem(a), want)
	if errors.Is(err, update.ErrUpToDate) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w%s", err, update.ClockHint(a.Run))
	}
	if out, err := a.Run.Run("systemctl", "reboot"); err != nil {
		return fmt.Errorf("the trial is armed, but rebooting failed (%v: %s); reboot to start it", err, out)
	}
	return nil
}

// updateSystem is the real router for rollgate update.
func updateSystem(a apply.System) update.System {
	url := os.Getenv("ROLLGATE_RELEASES_URL") // for tests; routers use the defaults
	repo := os.Getenv("ROLLGATE_REPO_URL")
	if url == "" {
		url = update.DefaultReleasesURL
	}
	if repo == "" {
		repo = update.DefaultRepoURL
	}
	return update.System{Root: a.Root, Run: a.Run, Stream: update.StreamExec, Out: a.Out, Now: a.Now, Sleep: time.Sleep,
		Get: update.HTTPGet, Verify: update.GPGVerify(filepath.Join(a.Root, update.PacmanGnupg)),
		URL: url, Repo: repo, Free: update.FreeBytes}
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
		if *boot {
			health.Diagnose(stdout, results, func(name string, args ...string) ([]byte, error) {
				return exec.Command(name, args...).CombinedOutput()
			})
		}
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
	prev, _ := health.RunningConfig(s.Root)
	if err := apply.Apply(s, c, source, files); err != nil {
		return err
	}
	// Blocklist settings changed (added, changed or removed): update now, in
	// the background, instead of waiting for the daily timer (ADR 0019). With
	// none set, the update tidies up and clears dnsmasq's cached answers.
	var was config.DNS
	if prev != nil {
		was = prev.DNS
	}
	if !slices.Equal(was.Blocklists, c.DNS.Blocklists) || !slices.Equal(was.Allow, c.DNS.Allow) {
		if out, err := s.Run.Run("systemctl", "start", "--no-block", "rollgate-blocklists.service"); err != nil {
			fmt.Fprintf(s.Out, "note: couldn't start updating the DNS blocklists (%v: %s); the daily timer will.\n", err, strings.TrimSpace(string(out)))
		} else if len(c.DNS.Blocklists) > 0 {
			fmt.Fprintln(s.Out, "Downloading the DNS blocklists in the background; rollgate status shows when they're in use.")
		}
	}
	return nil
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
	n, err := diff(c, source, filepath.Base(path), *root, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "rollgate diff: %v\n", err)
		return 2
	}
	if n == 0 {
		return 0
	}
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
	c, ok := parse(source, path, stderr)
	return c, source, ok
}

// parse validates a config, printing every problem to w, each starting with name.
func parse(source []byte, name string, w io.Writer) (*config.Config, bool) {
	c, err := config.Parse(source)
	var problems config.Problems
	switch {
	case err == nil:
		return c, true
	case errors.As(err, &problems):
		for _, p := range problems {
			fmt.Fprintf(w, "%s: %s\n", name, p)
		}
		fmt.Fprintf(w, "%s: %d problem(s); see docs/config-spec.md#validation-rules\n", name, len(problems))
	default:
		fmt.Fprintf(w, "%s: %v\n", name, err)
	}
	return nil, false
}
