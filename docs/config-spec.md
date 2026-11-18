# `router.toml` specification — schema version 1 (draft)

This file is the contract between users and the compiler.
Change this spec and `examples/router.toml` **before** changing compiler code.
A JSON Schema generated from the Go types will be published as `schema/router.schema.json` *(planned, M2)* so editors and agents can validate configs.

Format: [TOML 1.0](https://toml.io/en/v1.0.0). It was chosen over YAML because it has no implicit type coercion (YAML reads `no` as false, for example) and no significant indentation.

## Design principle: simple by default

A typical home config should fit on one screen and need no networking expertise.
- Every option has a safe default. Anything a user doesn't need to think about is left out of the example config.
- Prefer one high-level setting that does the right thing (e.g. `ipv6 = "auto"`) over many low-level knobs.
- Low-level overrides go in the [Advanced](#advanced-rarely-needed) section, and new ones need a stated real-world need.
- Validation errors must say in plain words what's wrong and how to fix it.

## Top level

| Key | Type | Required | Notes |
|---|---|---|---|
| `schema_version` | integer | yes | `1` |
| `[system]` | table | no | all keys have defaults |
| `[management]` | table | no | all keys have defaults |
| `[[interface]]` | array of tables | yes, at least one `wan` and one other zone | |
| `[[vlan]]` | array of tables | no | |
| `[dhcp.<interface>]` | table per interface | no | |
| `[dns]` | table | no | defaults to recursive resolution |
| `[firewall]` | table | no | defaults below |

## `[system]`

| Key | Type | Default | Notes |
|---|---|---|---|
| `hostname` | string | `"gateway"` | RFC 1123 label |
| `domain` | string | `"home.arpa"` | local DNS domain (`home.arpa` is reserved for this by RFC 8375) |
| `timezone` | string | `"UTC"` | IANA name |
| `ipv6` | string | `"auto"` | `"auto"`: every LAN zone always gets local IPv6, plus IPv6 internet automatically if the ISP provides it. `"off"`: no IPv6 anywhere. Details: ADR 0008. |

## `[management]`

| Key | Type | Default | Notes |
|---|---|---|---|
| `allowed_zones` | array of zone names | `["lan"]` | zones that can reach SSH and the web UI. Must not include `wan`; validation rejects it. Must be non-empty. |
| `ssh_authorized_keys` | array of secret names | — | each name refers to a file in `/etc/rollgate/secrets/` |
| `confirm_timeout_seconds` | integer | `120` | commit-confirm window, 30–900 |

## `[[interface]]`

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | also the zone name unless `zone` is set |
| `match_mac` | string | one of `match_mac` / `match_device` | preferred: survives NIC renaming |
| `match_device` | string | | e.g. `enp1s0` |
| `zone` | string | no | defaults to `name`. The zone named `wan` is special. |
| `ipv4` | string | yes | `"dhcp"` or a CIDR address such as `"192.168.10.1/24"` |

There are no per-interface IPv6 settings. IPv6 is handled automatically by `system.ipv6`.

## `[[vlan]]`

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | zone name unless `zone` is set |
| `parent` | string | yes | name of an `[[interface]]` |
| `id` | integer | yes | 1–4094, unique per parent |
| `zone`, `ipv4` | | | same as `[[interface]]` |

## `[dhcp.<name>]`

`<name>` is an interface or VLAN name.

| Key | Type | Default | Notes |
|---|---|---|---|
| `range` | `[start, end]` | — | both inside the interface subnet, and must not include the router's address |
| `lease_time` | string | `"12h"` | dnsmasq duration syntax |
| `[[dhcp.<name>.static]]` | array of tables | | `hostname`, `mac`, `ipv4` (inside the subnet, outside `range`) |

## `[dns]`

| Key | Type | Default | Notes |
|---|---|---|---|
| `mode` | string | `"recursive"` | `"recursive"` (Unbound resolves from the root servers) or `"forward"` |
| `forwarders` | array of strings | — | required when `mode = "forward"`. Each is `"ip"` or `"ip@port#tls-name"` for DNS over TLS. |
| `blocklists` | array of URLs | `[]` | domain blocklists fetched on a timer *(planned, M6)*. Until then a non-empty list is rejected, so it can't silently do nothing. |

## `[firewall]`

Inbound traffic from `wan` to the router is always dropped, apart from replies to established connections and rate-limited ICMP. **This default cannot be changed.**

| Key | Type | Default | Notes |
|---|---|---|---|
| `[[firewall.policy]]` | array of tables | see below | zone-to-zone forwarding rules, evaluated in order, first match wins |
| `[[firewall.port_forward]]` | array of tables | `[]` | destination NAT from WAN |

Default policy when no `[[firewall.policy]]` is given: every non-WAN zone may reach `wan`. Everything else is dropped.

`[[firewall.policy]]`: `from` (zone or `"*"`), `to` (zone or `"*"`), `action` (`"accept"` / `"drop"` / `"reject"`), optional `comment`.

`[[firewall.port_forward]]`: `name`, `protocol` (`"tcp"` / `"udp"` / `"tcp+udp"`), `wan_port` (integer or `"start-end"`), `to` (`"ip:port"`, must be inside a non-WAN zone's subnet).

NAT: masquerade from every non-WAN zone to `wan` is generated automatically.

Traffic to the router itself (input), as opposed to traffic passing through it:
- DHCP and DNS are allowed from every zone that has a `[dhcp.<name>]` section.
- SSH and the web UI are allowed only from `management.allowed_zones`.
- Everything else to the router is dropped.
`[[firewall.policy]]` rules govern forwarding only. They never open services on the router.

## Advanced (rarely needed)

These overrides exist for unusual setups. They are deliberately left out of the example config.

| Key | Type | Default | Notes |
|---|---|---|---|
| `[ipv6] ula_prefix` | string | generated once, stored in `/etc/rollgate/state/ula-prefix` | a fixed local IPv6 /48, e.g. kept from a previous router |
| `[ipv6] wan_prefix_hint` | integer 48–64 | `56` | delegated prefix size to request from the ISP |
| `ipv6_subnet_id` on `[[interface]]` / `[[vlan]]` | integer 0–255 | derived from the zone name | pins the zone's /64 within the /48 (or within the delegated prefix; 0–255 fits the default /56) |

## Validation rules

`rollgate validate [file]` checks a config (default `/etc/rollgate/router.toml`) and lists every problem it finds, not just the first.
Each problem has a stable error code, so docs, tests and agents can refer to it. Codes are never reused for a different meaning.

| Code | Rule |
|---|---|
| `E001` | Exactly one interface or VLAN is in the zone `wan`. *(planned, M2 PR 2)* |
| `E002` | No two subnets overlap. *(planned, M2 PR 2)* |
| `E003` | Every zone or interface named in `management`, `firewall.policy`, `dhcp` or a VLAN's `parent` exists. *(planned, M2 PR 2)* |
| `E004` | Management is reachable: at least one zone in `allowed_zones` has an interface with a static address. *(planned, M2 PR 2)* |
| `E005` | DHCP ranges and static leases lie inside their subnet and don't collide with each other or with the router's address. *(planned, M2 PR 2)* |
| `E006` | Port forward targets lie inside a non-WAN subnet. *(planned, M2 PR 2)* |
| `E007` | Unknown keys are errors, not warnings, so typos can't silently do nothing. |
| `E008` | `ipv6.ula_prefix`, if set, is a /48 inside `fd00::/8`; `wan_prefix_hint` is 48–64; `ipv6_subnet_id` values are 0–255 and unique. |
| `E009` | A value has the wrong format or is out of range (for example an address, a MAC, a port, or a choice such as `ipv6`). |
| `E010` | A required key is missing. |
| `E011` | A name is used twice (interfaces and VLANs, VLAN IDs on one parent, static leases, port forwards). |
| `E012` | The file can't be read or isn't valid TOML (including a value of the wrong type, such as `id = "20"`). |

Names of interfaces, VLANs and zones are 1–15 characters: lowercase letters, digits and `-`, starting with a letter.
The 15-character limit is Linux's limit on network device names.
