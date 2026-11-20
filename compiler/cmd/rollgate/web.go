package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/config"
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
	// Turn the web UI on, and restart it if it runs, so it uses the new
	// password and every old session ends.
	run := apply.Exec{}
	for _, cmd := range [][]string{
		{"systemctl", "enable", "--now", "rollgate-api.socket", "rollgate-web.socket"},
		{"systemctl", "try-restart", "rollgate-web.service"},
	} {
		if out, err := run.Run(cmd[0], cmd[1:]...); err != nil {
			fmt.Fprintf(stderr, "rollgate web-password: the password is set, but %s failed: %v\n%s", strings.Join(cmd, " "), err, out)
			return 1
		}
	}
	if len(ips) == 0 {
		fmt.Fprintln(stdout, "The web UI is on: open https://<the router's LAN address>/ from a computer in a management zone.")
	} else {
		fmt.Fprintf(stdout, "The web UI is on: open https://%s/ from a computer in a management zone.\n", ips[0])
	}
	return 0
}

// webCmd is rollgate web (ADR 0014): the web UI's server. rollgate-web.service
// runs it unprivileged, with the sockets rollgate-web.socket opened (443 and
// 80) and the secrets passed as systemd credentials. It changes nothing
// itself: it asks rollgate api.
func webCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	secret := func(name, fallback string) string {
		if dir != "" {
			return filepath.Join(dir, name)
		}
		return filepath.Join("/", fallback)
	}
	certFile := fs.String("cert", secret("web.crt", web.CertPath), "certificate")
	keyFile := fs.String("key", secret("web.key", web.KeyPath), "key")
	passwordFile := fs.String("password", secret("web-password", web.PasswordPath), "stored password hash")
	apiSocket := fs.String("api", api.Socket, "rollgate api's socket")
	httpsAddr := fs.String("https", "", "listen for HTTPS here instead of on systemd's sockets (development)")
	httpAddr := fs.String("http", "", "with -https: listen for HTTP (redirects) here")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	log.SetFlags(0)
	log.SetOutput(stderr)
	fail := func(err error) int { fmt.Fprintf(stderr, "rollgate web: %v\n", err); return 1 }
	hash, err := os.ReadFile(*passwordFile)
	if err != nil {
		return fail(fmt.Errorf("no web password (rollgate web-password sets one): %w", err))
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fail(err)
	}
	var httpsLs, httpLs []net.Listener
	if *httpsAddr != "" {
		l, err := net.Listen("tcp", *httpsAddr)
		if err != nil {
			return fail(err)
		}
		httpsLs = append(httpsLs, l)
		if *httpAddr != "" {
			if l, err = net.Listen("tcp", *httpAddr); err != nil {
				return fail(err)
			}
			httpLs = append(httpLs, l)
		}
	} else {
		ls, err := api.SystemdListeners()
		if err != nil {
			return fail(fmt.Errorf("%w (start rollgate-web.socket, or pass -https ADDR)", err))
		}
		for _, l := range ls {
			if a, ok := l.Addr().(*net.TCPAddr); ok && a.Port == 80 {
				httpLs = append(httpLs, l)
			} else {
				httpsLs = append(httpsLs, l)
			}
		}
	}
	srv := &web.Server{API: api.NewClient(*apiSocket), PasswordHash: strings.TrimSpace(string(hash))}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	errs := make(chan error, len(httpsLs)+len(httpLs))
	// HTTP/1.1 only, and one request per connection: every request comes
	// over a connection opened for it, so a confirm after an apply is always
	// a new connection (ADR 0014). HTTP/2 would carry every request over one.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	for _, l := range httpsLs {
		s := &http.Server{Handler: srv.Handler(), TLSConfig: tlsConfig, ErrorLog: log.Default(),
			Protocols: &protocols, ConnContext: srv.ConnContext, ReadHeaderTimeout: 10 * time.Second}
		s.SetKeepAlivesEnabled(false)
		go func() { errs <- s.ServeTLS(l, "", "") }()
		log.Printf("rollgate web: HTTPS on %s", l.Addr())
	}
	for _, l := range httpLs {
		s := &http.Server{Handler: web.Redirect(), ReadHeaderTimeout: 10 * time.Second}
		go func() { errs <- s.Serve(l) }()
		log.Printf("rollgate web: HTTP (redirects to HTTPS) on %s", l.Addr())
	}
	return fail(<-errs)
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
	if c.VPN != nil { // when the VPN may manage the router (ADR 0018)
		add(config.VPNZone, config.VPNZone, c.VPN.Addr)
	}
	return names, ips
}
