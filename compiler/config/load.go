package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where the router keeps its config.
const DefaultPath = "/etc/rollgate/router.toml"

// Load reads and validates a config file. On failure the error is a Problems
// listing everything found, unless the file can't be read at all.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Problems{{Code: CodeUnreadable, Msg: err.Error()}}
	}
	return Parse(data)
}

// Parse validates a config held in memory. See Load.
func Parse(data []byte) (*Config, error) {
	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, Problems{{
				Code: CodeUnreadable,
				Path: fmt.Sprintf("line %d", pe.Position.Line),
				Msg:  pe.Message,
			}}
		}
		// Type mismatches read: `toml: line 16 (last key "vlan.id"): incompatible types: ...`
		msg := strings.TrimPrefix(err.Error(), "toml: ")
		if where, rest, ok := strings.Cut(msg, "): "); ok && strings.HasPrefix(where, "line ") {
			line, _, _ := strings.Cut(where, " (")
			return nil, Problems{{Code: CodeUnreadable, Path: line, Msg: "wrong type of value: " + rest}}
		}
		return nil, Problems{{Code: CodeUnreadable, Msg: msg}}
	}

	var ps problems
	for _, key := range md.Undecoded() {
		ps.add(CodeUnknownKey, key.String(), "unknown key; check its spelling against docs/config-spec.md")
	}
	applyDefaults(&c, md)
	validateFields(&c, md, &ps)
	if len(ps.list) > 0 {
		return nil, ps.list
	}
	return &c, nil
}

// applyDefaults fills in every default from docs/config-spec.md. Keys the user
// set explicitly are left alone, even when empty, so validation can reject them.
func applyDefaults(c *Config, md toml.MetaData) {
	set := func(dst *string, value string, key ...string) {
		if !md.IsDefined(key...) {
			*dst = value
		}
	}
	set(&c.System.Hostname, "gateway", "system", "hostname")
	set(&c.System.Domain, "home.arpa", "system", "domain")
	set(&c.System.Timezone, "UTC", "system", "timezone")
	set(&c.System.IPv6, "auto", "system", "ipv6")
	set(&c.DNS.Mode, "recursive", "dns", "mode")

	if !md.IsDefined("management", "allowed_zones") {
		c.Management.AllowedZones = []string{"lan"}
	}
	if !md.IsDefined("management", "confirm_timeout_seconds") {
		c.Management.ConfirmTimeoutSeconds = 120
	}
	if !md.IsDefined("ipv6", "wan_prefix_hint") {
		c.IPv6.WANPrefixHint = 56
	}
	for i := range c.Interfaces {
		if c.Interfaces[i].Zone == "" {
			c.Interfaces[i].Zone = c.Interfaces[i].Name
		}
	}
	for i := range c.VLANs {
		if c.VLANs[i].Zone == "" {
			c.VLANs[i].Zone = c.VLANs[i].Name
		}
	}
	for name, d := range c.DHCP {
		if !md.IsDefined("dhcp", name, "lease_time") {
			d.LeaseTime = "12h"
			c.DHCP[name] = d
		}
	}
}
