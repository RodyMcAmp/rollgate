package compile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// ULAStateFile is where the router keeps its generated local IPv6 prefix,
// relative to the filesystem root (ADR 0008).
const ULAStateFile = "etc/rollgate/state/ula-prefix"

// LocalPrefix returns the ULA /48 to compile with when the config doesn't set
// ipv6.ula_prefix: the stored one below root, or a new random one (generated
// reports which). The caller puts it in c.IPv6.ULA; Compile then includes the
// state file in its output, so apply stores a generated prefix for good.
func LocalPrefix(root string) (p netip.Prefix, generated bool, err error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ULAStateFile)))
	switch {
	case err == nil:
		// The file is generated: comment lines, then the prefix.
		var value string
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				value = line
			}
		}
		p, err = netip.ParsePrefix(value)
		if err != nil || !isULA48(p) {
			return netip.Prefix{}, false, fmt.Errorf("%s doesn't hold a ULA /48: %q", ULAStateFile, value)
		}
		return p, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return netip.Prefix{}, false, err
	}
	// RFC 4193: fd followed by a random 40-bit global ID.
	var b [16]byte
	b[0] = 0xfd
	if _, err := rand.Read(b[1:6]); err != nil {
		return netip.Prefix{}, false, err
	}
	return netip.PrefixFrom(netip.AddrFrom16(b), 48), true, nil
}

func isULA48(p netip.Prefix) bool {
	return p.Addr().Is6() && p.Bits() == 48 && p == p.Masked() && netip.MustParsePrefix("fd00::/8").Contains(p.Addr())
}

// subnetIDs gives every non-WAN interface and VLAN with a static IPv4 address
// its IPv6 subnet number (docs/config-spec.md, "How ipv6 = auto is compiled"):
// ipv6_subnet_id if set, else an FNV-1a hash of the name; on a clash the
// alphabetically later name takes the next free number.
func subnetIDs(c *config.Config) map[string]int {
	ids := map[string]int{}
	used := map[int]bool{}
	var derived []string
	add := func(name, zone string, addr netip.Prefix, pinned *int) {
		switch {
		case zone == "wan" || !addr.IsValid():
		case pinned != nil:
			ids[name], used[*pinned] = *pinned, true
		default:
			derived = append(derived, name)
		}
	}
	for _, in := range c.Interfaces {
		add(in.Name, in.Zone, in.Addr, in.IPv6SubnetID)
	}
	for _, v := range c.VLANs {
		add(v.Name, v.Zone, v.Addr, v.IPv6SubnetID)
	}
	slices.Sort(derived)
	for _, name := range derived {
		h := fnv.New32a()
		h.Write([]byte(name))
		id := int(h.Sum32() % 256)
		for used[id] {
			id = (id + 1) % 256
		}
		ids[name], used[id] = id, true
	}
	return ids
}

// subnet64 returns the /64 with the given subnet number inside a /48.
func subnet64(ula netip.Prefix, id int) netip.Prefix {
	b := ula.Addr().As16()
	b[6], b[7] = 0, byte(id)
	return netip.PrefixFrom(netip.AddrFrom16(b), 64)
}

// routerAddr returns the router's address (::1) in a /64, with the prefix length.
func routerAddr(p netip.Prefix) netip.Prefix {
	b := p.Addr().As16()
	b[15] = 1
	return netip.PrefixFrom(netip.AddrFrom16(b), 64)
}
