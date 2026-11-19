package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/health"
	"github.com/RodyMcAmp/rollgate/compiler/web"
)

// webPasswordCmd is rollgate web-password (ADR 0014): set the web UI's admin
// password and make its HTTPS certificate if there is none yet.
func webPasswordCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("web-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStdin := fs.Bool("stdin", false, "read the password from one line of standard input (for scripts)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate web-password: must run as root")
		return 1
	}
	var password string
	var err error
	if *fromStdin {
		password, err = bufio.NewReader(os.Stdin).ReadString('\n')
		if errors.Is(err, io.EOF) && password != "" {
			err = nil
		}
		password = strings.TrimRight(password, "\r\n")
	} else {
		password, err = askPassword(stdout)
	}
	if err == nil {
		err = web.SetPassword("/", password)
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate web-password: %v\n", err)
		return 1
	}
	names, ips := certNames("/")
	fp, created, err := web.EnsureCert("/", names, ips, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "rollgate web-password: the password is set, but making the HTTPS certificate failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Web password set.")
	if created {
		fmt.Fprintln(stdout, "Made the web UI's HTTPS certificate. Your browser will warn about it once, because the router signed it itself.")
	}
	fmt.Fprintf(stdout, "Before accepting it, check that the browser shows this fingerprint (SHA-256):\n  %s\n", fp)
	fmt.Fprintln(stdout, "rollgate status shows it again.")
	return 0
}

// askPassword reads the password twice from the terminal, without echo.
func askPassword(stdout io.Writer) (string, error) {
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("standard input isn't a terminal; use --stdin to read the password from it")
	}
	stty := func(arg string) { c := exec.Command("stty", arg); c.Stdin = os.Stdin; _ = c.Run() }
	stty("-echo")
	defer stty("echo")
	// Ctrl-C must not leave the terminal without echo.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			stty("echo")
			fmt.Fprintln(stdout)
			os.Exit(130)
		}
	}()
	in := bufio.NewReader(os.Stdin)
	read := func(prompt string) (string, error) {
		fmt.Fprint(stdout, prompt)
		line, err := in.ReadString('\n')
		fmt.Fprintln(stdout)
		return strings.TrimRight(line, "\r\n"), err
	}
	first, err := read(fmt.Sprintf("New web password (at least %d characters): ", web.MinPassword))
	if err != nil {
		return "", err
	}
	second, err := read("Again: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two passwords differ; nothing was changed")
	}
	return first, nil
}

// certNames is what the certificate is for: the router's host names and the
// static addresses of its management zones, from the running config.
func certNames(root string) ([]string, []netip.Addr) {
	names := []string{"rollgate"}
	c, err := health.RunningConfig(root)
	if err != nil || c == nil {
		return names, nil
	}
	if h := c.System.Hostname; h != "" {
		names = []string{h}
		if c.System.Domain != "" {
			names = append(names, h+"."+c.System.Domain)
		}
	}
	zone := func(z, name string) string {
		if z == "" {
			return name
		}
		return z
	}
	var ips []netip.Addr
	add := func(z, name string, p netip.Prefix) {
		if p.IsValid() && slices.Contains(c.Management.AllowedZones, zone(z, name)) {
			ips = append(ips, p.Addr())
		}
	}
	for _, in := range c.Interfaces {
		add(in.Zone, in.Name, in.Addr)
	}
	for _, v := range c.VLANs {
		add(v.Zone, v.Name, v.Addr)
	}
	return names, ips
}
