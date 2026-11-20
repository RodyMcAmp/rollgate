// Package blocklist is rollgate dns update (ADR 0019): it downloads the
// lists in dns.blocklists, turns them into names for Unbound to answer with
// 0.0.0.0, and swaps them in safely, keeping the last good lists when a
// download fails.
package blocklist

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strings"
)

// label is one part of a name. Lists contain underscores now and then
// (_dmarc, service records), so those are accepted too.
var label = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// validName reports whether s is a domain name with at least two labels.
func validName(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if !label.MatchString(l) {
			return false
		}
	}
	return true
}

// hostsAddrs are the addresses hosts-format lists point blocked names at.
var hostsAddrs = map[string]bool{"0.0.0.0": true, "127.0.0.1": true, "::": true, "::1": true, "0": true}

// notNames are entries hosts files carry for the machine itself.
var notNames = map[string]bool{"localhost": true, "localhost.localdomain": true, "local": true,
	"broadcasthost": true, "ip6-localhost": true, "ip6-loopback": true, "0.0.0.0": true}

// Parse reads one list in any of the common formats, line by line, and
// returns the names in it, lower case, in order, with duplicates. It skips
// what it doesn't understand:
//
//	0.0.0.0 ads.example.com tracker.example.net   (hosts)
//	ads.example.com                               (one name per line)
//	||ads.example.com^                            (simple Adblock rules)
//
// Comments start with # (or ! in Adblock lists).
func Parse(r io.Reader) ([]string, error) {
	var names []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '!' || line[0] == '[' {
			continue
		}
		var candidates []string
		switch fields := strings.Fields(line); {
		case strings.HasPrefix(line, "||"):
			rule := strings.TrimPrefix(line, "||")
			name, rest, ok := strings.Cut(rule, "^")
			if !ok || rest != "" {
				continue // options or paths: not a plain name rule
			}
			candidates = []string{name}
		case len(fields) >= 2 && hostsAddrs[fields[0]]:
			candidates = fields[1:]
		case len(fields) == 1:
			candidates = fields
		}
		for _, c := range candidates {
			c = strings.TrimSuffix(strings.ToLower(c), ".")
			if !notNames[c] && validName(c) {
				names = append(names, c)
			}
		}
	}
	return names, sc.Err()
}

// under reports whether name is zone or a name below it.
func under(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// Build combines lists into the names to block: without duplicates, without
// names that a blocked parent already covers (Unbound blocks every name
// under a blocked one), without allowed names and the names keep protects,
// and at most max of them, earlier lists first. It reports whether it had
// to leave names out.
func Build(lists [][]string, allow []string, keep func(name string) bool, max int) ([]string, bool) {
	set := map[string]bool{}
	var order []string
	for _, list := range lists {
		for _, n := range list {
			if !set[n] {
				set[n] = true
				order = append(order, n)
			}
		}
	}
	// covered: a parent of n is blocked too, which already blocks n.
	covered := func(n string) bool {
		for p := n; ; {
			i := strings.IndexByte(p, '.')
			if i < 0 {
				return false
			}
			p = p[i+1:]
			if set[p] {
				return true
			}
		}
	}
	var out []string
	capped := false
	for _, n := range order {
		if covered(n) || (keep != nil && keep(n)) {
			continue
		}
		allowed := false
		for _, a := range allow {
			allowed = allowed || under(n, a)
		}
		if allowed {
			continue
		}
		if len(out) == max {
			capped = true
			break
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, capped
}

// Render writes Unbound's configuration for the names: each blocked name
// answers 0.0.0.0 / :: for itself and every name under it, and each
// allowed name goes back to normal resolution even under a blocked one.
func Render(names, allow []string, header string) []byte {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("server:\n")
	for _, n := range names {
		b.WriteString("\tlocal-zone: \"")
		b.WriteString(n)
		b.WriteString(".\" always_null\n")
	}
	for _, a := range allow {
		b.WriteString("\tlocal-zone: \"")
		b.WriteString(strings.TrimSuffix(strings.ToLower(a), "."))
		b.WriteString(".\" transparent\n")
	}
	return []byte(b.String())
}
