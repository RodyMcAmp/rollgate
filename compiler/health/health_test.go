package health

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

func script(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunDir(t *testing.T) {
	dir := t.TempDir()
	script(t, dir, "20-fail", "echo 'wan has no address'; echo more; exit 1", 0o755)
	script(t, dir, "10-pass", "echo 'firewall loaded'", 0o755)
	script(t, dir, "30-slow", "sleep 5", 0o755)
	script(t, dir, "40-silent-fail", "exit 3", 0o755)
	script(t, dir, "README", "not a check", 0o644)
	script(t, dir, ".50-editor-backup", "exit 1", 0o755)

	start := time.Now()
	results, err := RunDir(dir, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// The slow check and its sleep child are killed at the limit, not waited for.
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("RunDir took %s; a timed-out check must not hold it up", took)
	}
	want := []Result{
		{"10-pass", true, "firewall loaded"},
		{"20-fail", false, "wan has no address"},
		{"30-slow", false, "didn't finish within 500ms"},
		{"40-silent-fail", false, "exit status 3"},
	}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Errorf("results:\n got %v\nwant %v", results, want)
	}
	if AllOK(results) || !AllOK(results[:1]) || AllOK(nil) {
		t.Error("AllOK: wrong answer (no checks at all must not count as healthy)")
	}
	var out strings.Builder
	Print(&out, results)
	for _, line := range []string{"ok    10-pass        firewall loaded", "FAIL  20-fail        wan has no address"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("Print lacks %q:\n%s", line, out.String())
		}
	}
	if _, err := RunDir(filepath.Join(dir, "missing"), time.Second); err == nil {
		t.Error("a missing directory should be an error")
	}
}

func TestWait(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".up")
	script(t, dir, "10-comes-up", "test -e "+marker, 0o755)
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	runs := 0

	// The check passes on the third run.
	sleep := func(d time.Duration) {
		clock = clock.Add(d)
		if runs++; runs == 2 {
			os.WriteFile(marker, nil, 0o644)
		}
	}
	results, err := Wait(dir, time.Second, 10*time.Second, 5*time.Minute, now, sleep)
	if err != nil || !AllOK(results) || runs != 2 {
		t.Errorf("Wait = %v, %v after %d sleeps; want success after 2", results, err, runs)
	}

	// Never passes: gives up at the deadline.
	os.Remove(marker)
	runs, clock = -100, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	start := clock
	results, _ = Wait(dir, time.Second, 10*time.Second, time.Minute, now, sleep)
	if AllOK(results) || clock.Sub(start) > time.Minute {
		t.Errorf("Wait didn't give up within a minute: %v after %s", results, clock.Sub(start))
	}
}

// fakeEnv answers commands from a table and lets tests break DNS and TCP.
type fakeEnv struct {
	out     map[string]string // "nft list ..." → output
	fail    map[string]bool   // commands that exit non-zero
	dnsDown map[string]bool   // servers that don't answer
	tcpDown map[string]bool   // addresses that refuse connections
}

func (f fakeEnv) env(c *config.Config) Env {
	return Env{
		Config: c,
		Run: func(name string, args ...string) ([]byte, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			if f.fail[cmd] {
				return []byte(f.out[cmd]), errors.New("exit status 1")
			}
			return []byte(f.out[cmd]), nil
		},
		DNS: func(server, name string) ([]netip.Addr, error) {
			if f.dnsDown[server] {
				return nil, errors.New("i/o timeout")
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Dial: func(address string) error {
			if f.tcpDown[address] {
				return errors.New("connection refused")
			}
			return nil
		},
	}
}

func exampleConfig(t *testing.T) *config.Config {
	t.Helper()
	source, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const (
	routeCmd    = "ip -4 -o route show default"
	wanAddrCmd  = "ip -4 -o addr show dev wan"
	unitsCmd    = "systemctl is-active systemd-networkd.service sshd.service unbound.service dnsmasq.service"
	defaultWAN  = "default via 203.0.113.1 dev wan proto dhcp src 203.0.113.2 metric 1024\n"
	defaultLAN  = "default via 10.0.2.2 dev enp0s2 proto dhcp src 10.0.2.15 metric 1024\n"
	wanAddrLine = "3: wan    inet 203.0.113.2/24 metric 1024 brd 203.0.113.255 scope global dynamic wan\n"
)

func TestChecks(t *testing.T) {
	c := exampleConfig(t) // wan (DHCP), lan 192.168.10.1 with DHCP, iot VLAN 192.168.20.1 with DHCP
	healthy := fakeEnv{out: map[string]string{
		routeCmd: defaultWAN, wanAddrCmd: wanAddrLine,
		unitsCmd: "active\nactive\nactive\nactive\n",
		"systemctl is-active systemd-networkd.service sshd.service": "active\nactive\n",
	}}
	for _, tc := range []struct {
		check  string
		env    fakeEnv
		config *config.Config
		ok     bool
		detail string
	}{
		{"firewall", healthy, c, true, "the rollgate firewall is loaded"},
		{"firewall", fakeEnv{out: map[string]string{"nft list table inet rollgate": "Error: No such file or directory"},
			fail: map[string]bool{"nft list table inet rollgate": true}}, c, false, "isn't loaded: Error: No such file"},

		{"wan", healthy, c, true, "wan (wan) has 203.0.113.2/24; default route via 203.0.113.1"},
		{"wan", fakeEnv{out: map[string]string{routeCmd: defaultWAN}}, c, false, "wan (wan) has no IPv4 address"},
		{"wan", fakeEnv{out: map[string]string{routeCmd: defaultLAN, wanAddrCmd: wanAddrLine}}, c, false, "no default route through it"},
		{"wan", fakeEnv{out: map[string]string{routeCmd: defaultLAN}}, nil, true, "default route via 10.0.2.2 on enp0s2 (no config applied yet)"},
		{"wan", fakeEnv{}, nil, false, "no IPv4 default route"},

		{"dns", healthy, c, true, "Unbound and dnsmasq answer, on lan, iot"},
		{"dns", healthy, nil, true, "skipped"},
		{"dns", fakeEnv{dnsDown: map[string]bool{"127.0.0.1:5335": true}}, c, false, "Unbound (127.0.0.1:5335) doesn't answer"},
		{"dns", fakeEnv{dnsDown: map[string]bool{"192.168.20.1:53": true}}, c, false, "dnsmasq on iot (192.168.20.1:53) doesn't answer"},

		{"ssh", healthy, c, true, "sshd accepts connections on lan (192.168.10.1)"},
		{"ssh", fakeEnv{tcpDown: map[string]bool{"192.168.10.1:22": true}}, c, false, "on lan (192.168.10.1:22): connection refused"},
		{"ssh", healthy, nil, true, "(no config applied yet)"},

		{"services", healthy, c, true, "all 4 services running"},
		{"services", healthy, nil, true, "all 2 services running"},
		{"services", fakeEnv{out: map[string]string{unitsCmd: "active\nactive\nactive\nfailed\n"},
			fail: map[string]bool{unitsCmd: true}}, c, false, "not running: dnsmasq.service (failed)"},
	} {
		ok, detail := Checks[tc.check](tc.env.env(tc.config))
		if ok != tc.ok || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s (config %v): got %v %q, want %v %q", tc.check, tc.config != nil, ok, detail, tc.ok, tc.detail)
		}
	}
}

// reply builds a DNS response to query id with the given A records.
func reply(id [2]byte, rcode byte, addrs ...[4]byte) []byte {
	m := []byte{id[0], id[1], 0x81, 0x80 | rcode, 0, 1, 0, byte(len(addrs)), 0, 0, 0, 0}
	m = append(m, question...)
	for _, a := range addrs {
		// Name: a pointer to the question's name (offset 12); type A, class IN,
		// TTL 60, 4 bytes of data.
		m = append(m, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		m = append(m, a[:]...)
	}
	return m
}

// question is rollgate-health.localhost, type A, class IN, in wire format.
const question = "\x0frollgate-health\x09localhost\x00\x00\x01\x00\x01"

func TestLookupA(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			// The question asks for an A record of rollgate-health.localhost.
			if !strings.Contains(string(q), question) {
				continue
			}
			conn.WriteTo(reply([2]byte(q[:2]), 0, [4]byte{127, 0, 0, 1}, [4]byte{127, 0, 0, 2}), from)
		}
	}()
	addrs, err := lookupA(conn.LocalAddr().String(), dnsProbe)
	if err != nil || fmt.Sprint(addrs) != "[127.0.0.1 127.0.0.2]" {
		t.Errorf("lookupA = %v, %v", addrs, err)
	}

	id := [2]byte{1, 2}
	if _, err := parseA(reply(id, 3), id); err == nil || !strings.Contains(err.Error(), "error code 3") {
		t.Errorf("NXDOMAIN: %v", err)
	}
	if _, err := parseA(reply([2]byte{9, 9}, 0), id); err == nil {
		t.Error("a reply to another query should be rejected")
	}
	if _, err := parseA(reply(id, 0, [4]byte{1, 2, 3, 4})[:40], id); err == nil {
		t.Error("a truncated reply should be rejected")
	}
}

func TestDiagnose(t *testing.T) {
	var ran []string
	run := func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if args[len(args)-1] == "status" {
			return nil, errors.New("exit status 1")
		}
		return []byte("IDX LINK TYPE  OPERATIONAL SETUP\n  3 wan  ether routable    configuring\n"), nil
	}
	var out strings.Builder
	Diagnose(&out, []Result{{Name: "10-firewall", OK: true}, {Name: "30-dns", Detail: "no answer"}}, run)
	if out.Len() != 0 || len(ran) != 0 {
		t.Errorf("no failed WAN check, but it ran %q and wrote:\n%s", ran, out.String())
	}
	Diagnose(&out, []Result{{Name: "20-wan", Detail: "wan (wan) has no IPv4 address"}}, run)
	for _, want := range []string{"20-wan: networkctl list\n", "  |   3 wan  ether routable    configuring\n", "20-wan: networkctl --lines=30 status\n", "  | (exit status 1)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
