package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// key describes one router.toml key for the JSON Schema. The text is a short
// version of docs/config-spec.md, which stays the reference.
type key struct {
	doc      string
	enum     []string
	required bool
	min, max int // integer bounds; both 0 means none
}

// keys documents every key, by TOML path: "[]" marks an array of tables and
// "*" a table of named tables ([dhcp.<name>]). TestSchemaCoversEveryKey keeps
// it complete.
var keys = map[string]key{
	"schema_version": {doc: "Format version of this file.", required: true, min: 1, max: 1},

	"system":          {doc: "Router identity and IPv6."},
	"system.hostname": {doc: "The router's name (default \"gateway\")."},
	"system.domain":   {doc: "Local DNS domain (default \"home.arpa\")."},
	"system.timezone": {doc: "IANA timezone name (default \"UTC\")."},
	"system.ipv6":     {doc: "\"auto\": local IPv6 always, IPv6 internet if the ISP offers it. \"off\": no IPv6.", enum: []string{"auto", "off"}},

	"management":                         {doc: "Who can manage the router."},
	"management.allowed_zones":           {doc: "Zones that can reach SSH and the web UI (default [\"lan\"]). Never \"wan\"."},
	"management.ssh_authorized_keys":     {doc: "Public key files in /etc/rollgate/secrets/ allowed to log in over SSH."},
	"management.confirm_timeout_seconds": {doc: "Seconds to confirm a change before it's undone (default 120).", min: 30, max: 900},

	"interface":                  {doc: "Physical network ports."},
	"interface[].name":           {doc: "Name, and zone name unless zone is set.", required: true},
	"interface[].match_mac":      {doc: "MAC address of the port (preferred)."},
	"interface[].match_device":   {doc: "Linux device name of the port, e.g. \"enp1s0\"."},
	"interface[].zone":           {doc: "Zone, if not the same as the name. The zone \"wan\" is the internet side."},
	"interface[].ipv4":           {doc: "\"dhcp\" or the router's address with network size, e.g. \"192.168.10.1/24\".", required: true},
	"interface[].ipv6_subnet_id": {doc: "Advanced: pins this network's IPv6 /64 number.", max: 255},

	"vlan":                  {doc: "VLANs on a physical port."},
	"vlan[].name":           {doc: "Name, and zone name unless zone is set.", required: true},
	"vlan[].parent":         {doc: "The interface this VLAN runs on.", required: true},
	"vlan[].id":             {doc: "VLAN number.", required: true, min: 1, max: 4094},
	"vlan[].zone":           {doc: "Zone, if not the same as the name."},
	"vlan[].ipv4":           {doc: "\"dhcp\" or the router's address with network size.", required: true},
	"vlan[].ipv6_subnet_id": {doc: "Advanced: pins this network's IPv6 /64 number.", max: 255},

	"dhcp":                             {doc: "DHCP per interface or VLAN: [dhcp.<name>]."},
	"dhcp.*":                           {doc: "Hand out addresses on this interface or VLAN."},
	"dhcp.*.range":                     {doc: "First and last address to hand out.", required: true},
	"dhcp.*.lease_time":                {doc: "How long a lease lasts, e.g. \"12h\" (default)."},
	"dhcp.*.static":                    {doc: "Devices that always get the same address."},
	"dhcp.*.static[].hostname":         {doc: "Device name; also its local DNS name.", required: true},
	"dhcp.*.static[].mac":              {doc: "Device's MAC address.", required: true},
	"dhcp.*.static[].ipv4":             {doc: "Address it always gets (outside the range).", required: true},
	"dns":                              {doc: "How the router resolves names."},
	"dns.mode":                         {doc: "\"recursive\" (default) or \"forward\".", enum: []string{"recursive", "forward"}},
	"dns.forwarders":                   {doc: "With mode = \"forward\": \"ip\", \"ip@port\" or \"ip@853#tls-name\"; all TLS or none."},
	"dns.blocklists":                   {doc: "Planned (M6); must be empty for now."},
	"firewall":                         {doc: "Forwarding rules and port forwards. WAN inbound is always blocked."},
	"firewall.policy":                  {doc: "Zone-to-zone rules, in order; the first match wins."},
	"firewall.policy[].from":           {doc: "Zone, or \"*\" for every zone except wan.", required: true},
	"firewall.policy[].to":             {doc: "Zone, or \"*\" for every zone.", required: true},
	"firewall.policy[].action":         {doc: "What to do with matching traffic.", required: true, enum: []string{"accept", "drop", "reject"}},
	"firewall.policy[].comment":        {doc: "Note for humans."},
	"firewall.port_forward":            {doc: "Let one service in from the internet."},
	"firewall.port_forward[].name":     {doc: "Name, e.g. \"game server\".", required: true},
	"firewall.port_forward[].protocol": {doc: "Protocol.", required: true, enum: []string{"tcp", "udp", "tcp+udp"}},
	"firewall.port_forward[].wan_port": {doc: "Port on the internet side, or a range \"start-end\".", required: true},
	"firewall.port_forward[].to":       {doc: "Device and port, \"ip:port\".", required: true},
	"ipv6":                             {doc: "Advanced IPv6 overrides; rarely needed."},
	"ipv6.ula_prefix":                  {doc: "A fixed local IPv6 /48 inside fd00::/8."},
	"ipv6.wan_prefix_hint":             {doc: "Delegated prefix size to ask the ISP for (default 56).", min: 48, max: 64},
}

// Schema returns the JSON Schema for router.toml, generated from the Config
// types and the descriptions above (published as schema/router.schema.json).
func Schema() ([]byte, error) {
	s, err := schemaFor(reflect.TypeFor[Config](), "")
	if err != nil {
		return nil, err
	}
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["title"] = "rollgate router.toml"
	s["description"] = "Generated by `rollgate schema` from the Go types. Reference: docs/config-spec.md"
	out, err := json.MarshalIndent(s, "", "  ")
	return append(out, '\n'), err
}

func schemaFor(t reflect.Type, path string) (map[string]any, error) {
	s := map[string]any{}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaFor(t.Elem(), path)
	case reflect.String:
		s["type"] = "string"
	case reflect.Int:
		s["type"] = "integer"
	case reflect.Interface: // wan_port: a port number or "start-end"
		s["type"] = []string{"integer", "string"}
	case reflect.Slice:
		items, err := schemaFor(t.Elem(), path+"[]")
		if err != nil {
			return nil, err
		}
		s["type"], s["items"] = "array", items
	case reflect.Map:
		values, err := schemaFor(t.Elem(), join(path, "*"))
		if err != nil {
			return nil, err
		}
		s["type"], s["additionalProperties"] = "object", values
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := range t.NumField() {
			name := t.Field(i).Tag.Get("toml")
			if name == "" || name == "-" {
				continue
			}
			p := join(path, name)
			prop, err := schemaFor(t.Field(i).Type, p)
			if err != nil {
				return nil, err
			}
			props[name] = prop
			if keys[p].required {
				required = append(required, name)
			}
		}
		// Unknown keys are errors (E007).
		s["type"], s["properties"], s["additionalProperties"] = "object", props, false
		if required != nil {
			s["required"] = required
		}
	default:
		return nil, fmt.Errorf("schema: unsupported type %s at %q", t, path)
	}
	k, ok := keys[path]
	if path != "" && !ok && !strings.HasSuffix(path, "[]") {
		return nil, fmt.Errorf("schema: key %q has no description in keys", path)
	}
	if k.doc != "" {
		s["description"] = k.doc
	}
	if k.enum != nil {
		s["enum"] = k.enum
	}
	if k.min != 0 || k.max != 0 {
		s["minimum"], s["maximum"] = k.min, k.max
	}
	return s, nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
