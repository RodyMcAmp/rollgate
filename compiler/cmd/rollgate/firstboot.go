package main

// First-boot setup (ADR 0020): rollgate firstboot applies the installed
// router.toml by itself, rollgate setup asks the person at the console for one
// password, and rollgate password changes it later. The password is root's
// console password and the web UI's.

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/web"
)

const (
	shadowPath = "etc/shadow"
	// issuePath is shown above every console login prompt (agetty reads
	// /run/issue.d next to /etc/issue).
	issuePath = "run/issue.d/50-rollgate.issue"
	// firstbootPath keeps what the first boot did, for the setup screen and
	// the login prompt. It's in /run, so only that boot shows it.
	firstbootPath = "run/rollgate/firstboot"
	// wanCheck is the health check that depends on the ISP, not on the config.
	wanCheck = "20-wan"
)

// firstbootCmd is rollgate firstboot, which rollgate-firstboot.service runs
// while no config has ever been applied.
func firstbootCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// The boot console is the serial port when there is one; a screen shows
	// tty1. Tell both.
	out, errOut := stdout, stderr
	if tty, err := os.OpenFile("/dev/tty1", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		out, errOut = io.MultiWriter(stdout, tty), io.MultiWriter(stderr, tty)
	}
	var summary bytes.Buffer
	code := change("firstboot", func(s apply.System) error {
		s.Out = io.MultiWriter(out, &summary)
		err := firstBoot(s, config.DefaultPath, health.Dir, time.Sleep)
		if err != nil {
			fmt.Fprintf(&summary, "%v\n", err)
		}
		return err
	}, out, errOut)
	if err := writeFile(filepath.Join("/", firstbootPath), summary.Bytes(), 0o644); err != nil {
		fmt.Fprintf(stderr, "rollgate firstboot: %v\n", err)
	}
	return code
}

// firstBoot applies the config at path and keeps it if the health checks in
// checks pass, except WAN's: whether the ISP answers says nothing about the
// config, and undoing it wouldn't bring the internet back. If a check fails,
// it undoes the config. This is the one apply that isn't confirmed from a new
// connection (ADR 0020): no config was ever applied, so no management access
// can be lost by applying it.
func firstBoot(s apply.System, path, checks string, sleep func(time.Duration)) error {
	fmt.Fprintf(s.Out, "rollgate: first boot. Applying the router's settings (%s).\n", path)
	fix := fmt.Sprintf("Fix %s, then run: rollgate apply", path)
	var log bytes.Buffer
	quiet := s
	quiet.Out = &log // apply's "open a new SSH connection and confirm" doesn't apply here
	if err := applyFile(quiet, path, &log); err != nil {
		return fmt.Errorf("%sThe router's settings couldn't be applied, so it runs without them: %v\n%s", log.String(), err, fix)
	}
	c, _, ok := load(path, io.Discard)
	if !ok {
		return errors.Join(fmt.Errorf("%s changed while it was applied", path), apply.Revert(s, apply.ByHand))
	}
	// Decide before the revert timer could fire; each round of checks can
	// take a while.
	start := s.Now()
	deadline := start.Add(time.Duration(c.Management.ConfirmTimeoutSeconds)*time.Second - 10*time.Second)
	var results []health.Result
	for {
		var err error
		if results, err = health.RunDir(checks, 15*time.Second); err != nil {
			return errors.Join(fmt.Errorf("couldn't run the health checks: %w", err), apply.Revert(s, apply.ByHand))
		}
		// WAN often needs a little longer (the modem's DHCP); give it 30 s
		// once everything else works.
		if health.AllOK(results) || s.Now().Add(5*time.Second).After(deadline) ||
			(firstBootOK(results) && s.Now().Sub(start) > 30*time.Second) {
			break
		}
		sleep(5 * time.Second)
	}
	health.Print(s.Out, results)
	if !firstBootOK(results) {
		rerr := apply.Revert(s, apply.ByHand)
		return errors.Join(fmt.Errorf("a health check failed, so the router's settings were undone and it runs without them.\n%s", fix), rerr)
	}
	if err := apply.Confirm(s); err != nil {
		return fmt.Errorf("the health checks passed, but keeping the settings failed: %w\n%s", err, fix)
	}
	if !health.AllOK(results) {
		fmt.Fprintln(s.Out, "WAN has no internet address yet. The router keeps trying; check the cable to your modem.")
	}
	return nil
}

// firstBootOK reports whether every check but WAN's passed.
func firstBootOK(results []health.Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !r.OK && r.Name != wanCheck {
			return false
		}
	}
	return true
}

// setupCmd is rollgate setup [-- LOGIN-PROGRAM ARGS...]: the console's getty
// units run it in place of agetty (drop-ins for getty@ and serial-getty@), with
// the terminal as standard input and output. While root has no password, it
// asks for one. Then it writes what the login prompt shows above itself, and
// becomes the login program. It runs as the unit's main process, not before
// it, so the unit counts as started at once and the boot finishes while the
// setup screen waits for someone to type.
func setupCmd(args []string, stdout, stderr io.Writer) int {
	var login []string
	if len(args) > 0 {
		if args[0] != "--" || len(args) == 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		login = args[1:]
	}
	code := 0
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate setup: must run as root")
		code = 1
	} else if has, err := rootHasPassword("/"); err != nil {
		fmt.Fprintf(stderr, "rollgate setup: %v\n", err)
		code = 1
	} else if !has {
		// The other consoles' prompts show the first boot's result meanwhile.
		_ = writeIssue("/")
		if err := setupScreen(os.Stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "rollgate setup: %v\n", err)
			code = 1
		}
	}
	if code == 0 {
		if err := writeIssue("/"); err != nil {
			fmt.Fprintf(stderr, "rollgate setup: %v\n", err)
			code = 1
		}
	}
	if len(login) > 0 {
		// Whatever happened, the login prompt must come.
		err := syscall.Exec(login[0], login, os.Environ())
		fmt.Fprintf(stderr, "rollgate setup: starting %s: %v\n", login[0], err)
		return 1
	}
	return code
}

// setupScreen asks for the router's password on the terminal in, and sets it.
// If another console sets it meanwhile, it stops asking.
func setupScreen(in *os.File, out io.Writer) error {
	fmt.Fprintln(out)
	if summary, err := os.ReadFile(filepath.Join("/", firstbootPath)); err == nil {
		fmt.Fprintf(out, "%s\n", summary)
	}
	fmt.Fprintf(out, "Welcome to rollgate.\n\nChoose a password for this router. You'll use it to log in here (as root)\n"+
		"and on the web UI. At least %d characters; it isn't shown as you type.\n\n", web.MinPassword)

	stty := func(arg string) { c := exec.Command("stty", arg); c.Stdin = in; _ = c.Run() }
	stty("-echo")
	defer stty("echo")
	lines := make(chan string)
	go func() {
		r := bufio.NewReader(in)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()
	errElsewhere := errors.New("set elsewhere")
	read := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case line, ok := <-lines:
				fmt.Fprintln(out)
				if !ok {
					return "", io.EOF
				}
				return line, nil
			case <-tick.C:
				if has, _ := rootHasPassword("/"); has {
					fmt.Fprintln(out)
					return "", errElsewhere
				}
			}
		}
	}
	var password string
	for {
		first, err := read("Password: ")
		if err == nil && len([]rune(first)) < web.MinPassword {
			fmt.Fprintf(out, "That's shorter than %d characters. Try again.\n\n", web.MinPassword)
			continue
		}
		var second string
		if err == nil {
			second, err = read("Again: ")
		}
		if errors.Is(err, errElsewhere) {
			fmt.Fprintln(out, "The password was set on another console.")
			return nil
		}
		if err != nil {
			return err
		}
		if first != second {
			fmt.Fprintln(out, "The two didn't match. Try again.")
			fmt.Fprintln(out)
			continue
		}
		password = first
		break
	}
	if err := setRootPassword(password); err != nil {
		return err
	}
	fp, _, ips, err := setWebPassword(password)
	if err != nil {
		return fmt.Errorf("the console password is set, but the web UI's isn't (run rollgate password to try again): %w", err)
	}
	fmt.Fprintf(out, "\nPassword set. Log in here as root with it, and on the web UI:\n  %s\n"+
		"Your browser warns about its certificate once, because the router made it itself.\n"+
		"Accept it if the browser shows this fingerprint (SHA-256):\n  %s\n\n", webAddress(ips), fp)
	return nil
}

// passwordCmd is rollgate password: change the router's password, for the
// console and the web UI.
func passwordCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStdin := fs.Bool("stdin", false, "read the password from one line of standard input (for scripts)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate password: must run as root")
		return 1
	}
	password, err := readPassword(*fromStdin, "New password", stdout)
	if err == nil && len([]rune(password)) < web.MinPassword {
		err = fmt.Errorf("the password must be at least %d characters; nothing was changed", web.MinPassword)
	}
	if err == nil {
		err = setRootPassword(password)
	}
	var fp string
	var ips []netip.Addr
	if err == nil {
		if fp, _, ips, err = setWebPassword(password); err != nil {
			err = fmt.Errorf("the console password is changed, but the web UI's isn't: %w", err)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate password: %v\n", err)
		return 1
	}
	if err := writeIssue("/"); err != nil {
		fmt.Fprintf(stderr, "note: couldn't update the login prompt's text: %v\n", err)
	}
	fmt.Fprintf(stdout, "Password changed, for the console and the web UI (%s); web sessions were ended.\n", webAddress(ips))
	fmt.Fprintf(stdout, "The web UI's certificate fingerprint (SHA-256):\n  %s\n", fp)
	return 0
}

// readPassword reads one line from standard input, or asks twice on the
// terminal.
func readPassword(fromStdin bool, prompt string, stdout io.Writer) (string, error) {
	if !fromStdin {
		return askPassword(stdout, prompt)
	}
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if errors.Is(err, io.EOF) && password != "" {
		err = nil
	}
	return strings.TrimRight(password, "\r\n"), err
}

// rootHasPassword reports whether root's password in root/etc/shadow is set.
// A locked one ("!", "*") counts as set: someone chose that.
func rootHasPassword(root string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(root, shadowPath))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, rest, ok := strings.Cut(line, ":"); ok && name == "root" {
			hash, _, _ := strings.Cut(rest, ":")
			return hash != "", nil
		}
	}
	return false, errors.New("/etc/shadow has no root entry")
}

// setRootPassword sets root's password with chpasswd, which hashes it as
// /etc/login.defs says.
func setRootPassword(password string) error {
	cmd := exec.Command("chpasswd")
	cmd.Stdin = strings.NewReader("root:" + password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setting root's password failed: chpasswd: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setWebPassword stores the web UI's password, makes its HTTPS certificate if
// there is none, and turns the web UI on, restarting it if it runs so it uses
// the new password and every old session ends. It returns the certificate's
// fingerprint, whether the certificate was made now, and the addresses the
// web UI is for.
func setWebPassword(password string) (string, bool, []netip.Addr, error) {
	if err := web.SetPassword("/", password); err != nil {
		return "", false, nil, err
	}
	names, ips := certNames("/")
	fp, created, err := web.EnsureCert("/", names, ips, time.Now())
	if err != nil {
		return "", false, ips, fmt.Errorf("the password is set, but making the HTTPS certificate failed: %w", err)
	}
	run := apply.Exec{}
	for _, cmd := range [][]string{
		{"systemctl", "enable", "--now", "rollgate-api.socket", "rollgate-web.socket"},
		{"systemctl", "try-restart", "rollgate-web.service"},
	} {
		if out, err := run.Run(cmd[0], cmd[1:]...); err != nil {
			return fp, created, ips, fmt.Errorf("the password is set, but %s failed: %v\n%s", strings.Join(cmd, " "), err, out)
		}
	}
	return fp, created, ips, nil
}

// webAddress is where to open the web UI.
func webAddress(ips []netip.Addr) string {
	if len(ips) == 0 {
		return "https://<the router's LAN address>/"
	}
	return fmt.Sprintf("https://%s/", ips[0])
}

// writeIssue writes what the console shows above its login prompt: whether
// the first boot had to undo the settings, and where the web UI is.
func writeIssue(root string) error {
	var b strings.Builder
	if summary, err := os.ReadFile(filepath.Join(root, firstbootPath)); err == nil && strings.Contains(string(summary), "runs without them") {
		b.WriteString("The router's settings couldn't be used at first boot: journalctl -u rollgate-firstboot says why.\n")
	}
	if web.PasswordSet(root) {
		if certPEM, err := os.ReadFile(filepath.Join(root, web.CertPath)); err == nil {
			_, ips := certNames(root)
			fmt.Fprintf(&b, "Web UI: %s\n", webAddress(ips))
			if fp, err := web.Fingerprint(certPEM); err == nil {
				fmt.Fprintf(&b, "Its certificate's fingerprint (SHA-256):\n  %s\n", fp)
			}
		}
	}
	path := filepath.Join(root, issuePath)
	if b.Len() == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b.WriteString("\n")
	return writeFile(path, []byte(b.String()), 0o644)
}

// writeFile replaces path in one step, creating its directory if needed.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
