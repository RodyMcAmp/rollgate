# `router.toml` specification — schema version 1 (draft)

This file is the contract between users and the compiler.
Change this spec and `examples/router.toml` **before** changing compiler code.
The JSON Schema `schema/router.schema.json` is generated from the Go types (`rollgate schema`), so editors and agents can check a config as it's written. Editors with TOML schema support (for example VS Code with the Even Better TOML extension) can use it with a first line `#:schema https://raw.githubusercontent.com/RodyMcAmp/rollgate/main/schema/router.schema.json`. `rollgate validate` remains the full check.

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
| `ssh_authorized_keys` | array of secret names | `[]` | public keys allowed to log in as root over SSH. Each name refers to a file in `/etc/rollgate/secrets/`, which sshd reads in place. When empty, `/root/.ssh/authorized_keys` is used (where the installer puts your key). SSH never accepts passwords. |
| `confirm_timeout_seconds` | integer | `120` | commit-confirm window, 30–900 |

## `[[interface]]`

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | also the zone name unless `zone` is set |
| `match_mac` | string | one of `match_mac` / `match_device` | preferred: survives NIC renaming |
| `match_device` | string | | e.g. `enp1s0` |
| `zone` | string | no | defaults to `name`. The zone named `wan` is special. |
| `ipv4` | string | yes | `"dhcp"` or a CIDR address such as `"192.168.10.1/24"`. The `wan` interface must use `"dhcp"` for now; a static WAN address also needs a gateway setting *(planned)*. |

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
| `forwarders` | array of strings | — | required when `mode = "forward"`. Each is `"ip"`, `"ip@port"`, or `"ip@port#tls-name"` for DNS over TLS. Either all forwarders use TLS or none do. |
| `blocklists` | array of strings | `[]` | lists of names to block on every network (ADR 0019): `"recommended"` (a built-in, well-known list with few false positives) or `https://` URLs of lists in hosts, plain or simple Adblock format. Blocked names, and every name under them, answer `0.0.0.0` / `::`. The router downloads the lists once a day and keeps the last good copy if a download fails. |
| `allow` | array of names | `[]` | names never blocked, with every name under them, for when a list gets one wrong. Only used with `blocklists`. |

Blocklists: the router downloads each list over HTTPS once a day at a random time, and soon after boot and after an apply that changes these settings (`rollgate-blocklists.timer`). `"recommended"` is the StevenBlack unified hosts list (adware and malware), from `raw.githubusercontent.com`. At most 500,000 names are used. The router's own names and the local domain are never blocked. `rollgate status` says how many names are blocked and when the lists were last updated.

Local names: devices that get an address from the router's DHCP are reachable as `<their hostname>.<domain>` (for example `nas.home.arpa`), and the router itself as `<hostname>.<domain>` (`gateway.home.arpa`), from every zone with a `[dhcp.<name>]` section.
Names in `<domain>` are never sent to the internet.

## `[vpn]`

A WireGuard VPN for reaching home from outside (ADR 0018): a phone or laptop away from home connects to the router, and uses the home networks, or the home internet connection, as if it were at home. Having a `[vpn]` section turns it on.

| Key | Type | Default | Notes |
|---|---|---|---|
| `port` | integer | `51820` | the UDP port the router listens on, on WAN. The only port the VPN opens. |
| `address` | string | `"10.66.0.1/24"` | the router's address inside the VPN, with its size. Mustn't overlap another network. |
| `reach` | string | `"everything"` | `"everything"`: connected devices reach the internet and every home network. `"internet"`: only the internet, through home. |
| `endpoint` | string | — | a name (or address) that always points at your home, put into devices' configurations. Without it, the router's current public address is used. |
| `[[vpn.peer]]` | array of tables | | the devices allowed to connect: `name`, `public_key` (the device's WireGuard public key), `ipv4` (its fixed address inside the VPN). |

- The VPN is a zone named `vpn`: `firewall.policy` rules can name it, and `management.allowed_zones` must list `"vpn"` for the router to be managed over the VPN. It never counts as the network that keeps the router manageable (E004): a network at home must.
- The router's private key is a secret, `/etc/rollgate/secrets/vpn.key`; `rollgate vpn key` makes it and prints the router's public key. `rollgate apply` refuses a `[vpn]` section until it exists.
- Devices use the router's DNS (`address`), so local names work over the VPN.
- `reach` is a forward rule for the `vpn` zone that comes before your `[[firewall.policy]]` rules. It only allows, so your rules can still let the `vpn` zone reach more.

## `[firewall]`

Inbound traffic from `wan` is always dropped, both to the router and through it. The only ways in are replies to connections started from inside, port forwards, rate-limited ICMP, and the IPv6 control messages an internet connection needs (see below). **This default cannot be changed** (AGENTS.md hard rule 5).

| Key | Type | Default | Notes |
|---|---|---|---|
| `[[firewall.policy]]` | array of tables | see below | zone-to-zone forwarding rules, evaluated in order, first match wins |
| `[[firewall.port_forward]]` | array of tables | `[]` | destination NAT from WAN |

Default policy when no `[[firewall.policy]]` is given: every non-WAN zone may reach `wan`. Everything else is dropped.

`[[firewall.policy]]`: `from` (zone or `"*"`), `to` (zone or `"*"`), `action` (`"accept"` / `"drop"` / `"reject"`), optional `comment`.
- `from = "*"` means every zone **except `wan`**. `to = "*"` means every zone, including `wan`.
- A rule that accepts traffic `from = "wan"` is rejected (`E009`): use a port forward to let something in. `drop` and `reject` rules from `wan` are allowed but change nothing, since that traffic is dropped anyway.
- Traffic that matches no rule is dropped.

`[[firewall.port_forward]]`: `name`, `protocol` (`"tcp"` / `"udp"` / `"tcp+udp"`), `wan_port` (integer or `"start-end"`), `to` (`"ip:port"`, must be inside a non-WAN zone's subnet).
For a port range, `to` must use the range's first port; every port in the range goes to the same port on the device (`"27015-27030"` to `"192.168.10.50:27015"` forwards 27015→27015, 27016→27016, and so on).
A port forward lets the traffic through the firewall by itself; no `[[firewall.policy]]` rule is needed.

NAT: IPv4 traffic leaving through `wan` is masqueraded (gets the router's WAN address). IPv6 is never NATed.

Traffic to the router itself (input), as opposed to traffic passing through it:
- DHCP and DNS are allowed from every interface or VLAN that has a `[dhcp.<name>]` section.
- SSH and the web UI are allowed only from `management.allowed_zones`.
- ICMP (ping and error messages) is allowed from every zone, rate-limited.
- IPv6 neighbor discovery and router solicitations are allowed from every zone. From `wan`, router advertisements and DHCPv6 replies from the ISP's link-local address are allowed, so IPv6 internet can work (`ipv6 = "auto"`).
- Everything else to the router is dropped.
`[[firewall.policy]]` rules govern forwarding only. They never open services on the router.

The compiler writes the whole ruleset to `/etc/nftables.conf` as one `inet` table, so IPv4 and IPv6 get the same policy (ADR 0006).

## Advanced (rarely needed)

These overrides exist for unusual setups. They are deliberately left out of the example config.

| Key | Type | Default | Notes |
|---|---|---|---|
| `[ipv6] ula_prefix` | string | generated once, stored in `/etc/rollgate/state/ula-prefix` | a fixed local IPv6 /48, e.g. kept from a previous router |
| `[ipv6] wan_prefix_hint` | integer 48–64 | `56` | delegated prefix size to request from the ISP |
| `ipv6_subnet_id` on `[[interface]]` / `[[vlan]]` | integer 0–255 | derived from the name | pins this network's /64 within the /48 (or within the delegated prefix; 0–255 fits the default /56) |

### How `ipv6 = "auto"` is compiled (ADR 0008)

- **Local prefix (ULA /48):** `ipv6.ula_prefix` if set; otherwise the one stored in `/etc/rollgate/state/ula-prefix`.
  If neither exists (a new router, or a live boot), `rollgate compile` generates a random `fdXX:XXXX:XXXX::/48` and includes the state file in its output, so the first apply stores it and it never changes afterwards.
- **One /64 per network:** every interface and VLAN with a static IPv4 address (not WAN) gets subnet number `ipv6_subnet_id`, or else one derived from its name (FNV-1a hash, 0–255), so adding or reordering networks doesn't renumber the others.
  If two names hash to the same number, the alphabetically later one takes the next free number.
  The router is `::1` in each /64, e.g. `fd12:3456:789a:4f::1/64`.
- **LAN side:** the router announces each /64 with router advertisements (SLAAC), its own ULA address as the DNS server, and `system.domain` as the search domain.
  When the ISP delegates a prefix, each network also gets a public /64 from it with the same subnet number, announced the same way.
  A delegated /60 has room for subnet numbers 0–15 only; networks with a higher number then get local IPv6 only.
- **WAN side:** the router accepts the ISP's router advertisements and always asks for a delegated prefix of size `wan_prefix_hint` with DHCPv6, ignoring the ISP's DNS settings (the router resolves names itself).

## Validation rules

`rollgate validate [file]` checks a config (default `/etc/rollgate/router.toml`) and lists every problem it finds, not just the first.
Each problem has a stable error code, so docs, tests and agents can refer to it. Codes are never reused for a different meaning.

| Code | Rule |
|---|---|
| `E001` | Exactly one interface or VLAN is in the zone `wan`. |
| `E002` | No two subnets overlap. |
| `E003` | Every zone or interface named in `management`, `firewall.policy`, `dhcp` or a VLAN's `parent` exists. |
| `E004` | Management is reachable: at least one zone in `allowed_zones` has an interface with a static address. |
| `E005` | DHCP is served only on non-WAN interfaces and VLANs with a static address. Ranges and static leases lie inside that subnet and don't collide with each other or with the router's address. |
| `E006` | Port forward targets lie inside a non-WAN subnet and aren't the router itself. Two port forwards can't claim the same WAN port and protocol. For a port range, `to` uses the range's first port. |
| `E007` | Unknown keys are errors, not warnings, so typos can't silently do nothing. |
| `E008` | `ipv6.ula_prefix`, if set, is a /48 inside `fd00::/8`; `wan_prefix_hint` is 48–64; `ipv6_subnet_id` values are 0–255 and unique. |
| `E009` | A value has the wrong format or is out of range (for example an address, a MAC, a port, or a choice such as `ipv6`), or a firewall policy would accept traffic from `wan`. |
| `E010` | A required key is missing. |
| `E011` | A name or port is used twice (interfaces and VLANs, the same MAC or device matched by two interfaces, VLAN IDs on one parent, static leases, port forwards). |
| `E012` | The file can't be read or isn't valid TOML (including a value of the wrong type, such as `id = "20"`). |

Names of interfaces, VLANs and zones are 1–15 characters: lowercase letters, digits and `-`, starting with a letter.
The 15-character limit is Linux's limit on network device names.

## How names map to devices

The compiler gives every interface and VLAN a Linux network device named after it, so firewall rules and status output use the names from `router.toml`:
- An interface with `match_mac` is renamed to its `name` (a systemd `.link` file matching the port's permanent MAC address).
- An interface with `match_device` keeps that device name.
- A VLAN is created as a device named after the VLAN.
