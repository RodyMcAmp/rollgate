package apply

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// ReloadPlan returns the commands that make the running system use newly
// installed files, in the order to run them (ADR 0010). changed lists the
// paths the install wrote or removed; prev and next are the file sets before
// and after (the generation replaced, and the one installed).
//
// The order:
//  1. nft -f: the firewall first. It matches interfaces by name, so it can be
//     loaded before the interfaces exist, and it is default-deny.
//  2. sysctl -p: kernel settings such as forwarding, now that the firewall
//     that guards forwarding is in place.
//  3. udev, if .link files changed, so renamed ports get their new names.
//  4. networkctl delete for VLANs whose .netdev was changed or removed
//     (networkd never changes or removes an existing device by itself),
//     then networkctl reload.
//  5. Unbound, then dnsmasq, which forwards to it.
//  6. sshd last, with a reload that keeps open sessions.
//
// dnsmasq and Unbound are enabled and restarted when their new file is one
// rollgate generated, and stopped and disabled when it isn't (a revert to
// generation 0 restores the distribution's default, which would serve DNS on
// every interface).
func ReloadPlan(changed []string, prev, next compile.Files) [][]string {
	// Every generated file's first line carries the hash of router.toml, so
	// any edit changes every file. Only a change below that line needs a
	// reload; otherwise each apply would restart everything.
	changed = slices.DeleteFunc(slices.Clone(changed), func(p string) bool { return sameBody(prev[p], next[p]) })

	var plan [][]string
	add := func(cmd ...string) { plan = append(plan, cmd) }
	has := func(p string) bool { return slices.Contains(changed, p) }

	if has(nftablesConf) && next[nftablesConf] != nil {
		add("nft", "-f", "/"+nftablesConf)
	}
	if has(sysctlConf) && next[sysctlConf] != nil {
		add("sysctl", "-p", "/"+sysctlConf)
	}

	var links, network bool
	var deleteDevices []string
	for _, p := range changed {
		if !strings.HasPrefix(p, networkDir) {
			continue
		}
		network = true
		name := strings.TrimPrefix(p, networkDir)
		switch {
		case strings.HasSuffix(name, ".link"):
			links = true
		case strings.HasSuffix(name, ".netdev") && prev[p] != nil:
			if dev, ok := netdevDevice(name); ok {
				deleteDevices = append(deleteDevices, dev)
			}
		}
	}
	if links {
		add("udevadm", "control", "--reload")
		add("udevadm", "trigger", "--action=add", "--subsystem-match=net")
		add("udevadm", "settle")
	}
	for _, dev := range deleteDevices {
		add("networkctl", "delete", dev)
	}
	if network {
		add("networkctl", "reload")
	}

	for _, s := range []struct{ path, unit string }{
		{unboundConf, "unbound.service"},
		{dnsmasqConf, "dnsmasq.service"},
	} {
		if !has(s.path) {
			continue
		}
		if compile.IsGenerated(next[s.path]) {
			add("systemctl", "enable", s.unit)
			add("systemctl", "restart", s.unit)
		} else {
			add("systemctl", "disable", "--now", s.unit)
		}
	}
	if has(sshdConf) {
		add("systemctl", "reload-or-restart", "sshd.service")
	}
	return plan
}

// sameBody reports whether two versions of a file differ at most in the
// header line naming the source config and its hash. A missing file
// (removed, or new) always differs from an existing one.
func sameBody(a, b []byte) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return bytes.Equal(body(a), body(b))
}

func body(data []byte) []byte {
	if !compile.IsGenerated(data) {
		return data
	}
	if _, rest, ok := bytes.Cut(data, []byte("\n")); ok {
		return rest
	}
	return nil
}

// netdevDevice returns the device a compiled .netdev file creates:
// 20-rollgate-<name>.netdev creates <name> (compile names VLAN devices
// after the VLAN).
func netdevDevice(file string) (string, bool) {
	_, rest, ok := strings.Cut(strings.TrimSuffix(file, ".netdev"), "-rollgate-")
	return rest, ok && rest != ""
}

// Reload runs a plan in order and stops at the first failure. The caller
// then reverts: installing the previous generation and running its plan.
func Reload(r Runner, plan [][]string) error {
	for _, cmd := range plan {
		if out, err := r.Run(cmd[0], cmd[1:]...); err != nil {
			return fmt.Errorf("%s: %v%s", strings.Join(cmd, " "), err, indent(out))
		}
	}
	return nil
}
