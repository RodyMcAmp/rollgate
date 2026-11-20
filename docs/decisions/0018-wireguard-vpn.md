# 0018. A WireGuard VPN for reaching home from outside

- Status: Accepted (by the maintainer, 2026-11-19)
- Date: 2026-11-19

## Context

The roadmap's M6 has a "road-warrior" VPN: a phone or laptop away from home connects to the router and reaches the home network, or uses the home connection for everything (on public Wi-Fi, say).
ADR 0014 keeps the web UI off the internet on purpose and says remote management comes later through a VPN. ADR 0006 already names WireGuard for tunnels, "later".

What exists: systemd-networkd sets up every interface and VLAN from files the compiler writes. nftables drops everything from WAN except replies, port forwards, rate-limited ICMP and the IPv6 control messages (hard rule 5). dnsmasq answers DNS and DHCP on networks with a `[dhcp.<name>]` section and forwards to Unbound. The web UI edits `router.toml` with `compiler/edit` (ADR 0016).

## Decision

### WireGuard, in the kernel, set up by systemd-networkd

- WireGuard is part of the Linux kernel (both kernels the image ships), and systemd-networkd sets up WireGuard interfaces natively (`Kind=wireguard` in a `.netdev` file). So there is **no new daemon** and no `wg-quick`.
- The image adds the `wireguard-tools` package (Arch `extra`) for `wg show`, which the status page and diagnostics use to show when each device last connected. Nothing runs from it.

### In router.toml: one section, devices listed by name

```toml
[vpn]
# port = 51820               default
# address = "10.66.0.1/24"   default: the router's address inside the VPN
# reach = "everything"       default; or "internet"
# endpoint = "home.example.net"   optional: a name that always points at your home

[[vpn.peer]]
name = "phone"
public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
ipv4 = "10.66.0.2"
```

- Having a `[vpn]` section turns the VPN on; removing it turns it off. Every setting has a default (rule 9).
- **The VPN is a zone named `vpn`**, like a network: the interface is `vpn`, and the router's address in it is `address`.
- **`reach`** is one high-level choice:
  - `"everything"` (the default): connected devices reach the internet and every home network. This is what people want a home VPN for.
  - `"internet"`: they reach only the internet through home (a private connection on public Wi-Fi), not the home networks.
  The compiler turns it into a forward rule for the `vpn` zone placed before the user's `[[firewall.policy]]` rules. It only accepts; it never blocks, so a user's rules can still let the `vpn` zone reach more.
- **Managing the router over the VPN** stays a separate, explicit choice: it works only if `management.allowed_zones` lists `"vpn"`, the same rule as for any network. The VPN page will offer it as a checkbox.
- **Peers** are the devices allowed to connect: a name, the device's public key, and its fixed address inside the VPN. A device without a peer entry can't connect.

### Keys

- **The router's private key** is a secret (rule 8): `/etc/rollgate/secrets/vpn.key`, made the first time it's needed (`rollgate vpn` at the CLI, or the VPN page through the API). The compiler only refers to it (`PrivateKeyFile=`). `apply` refuses a `[vpn]` section while the key is missing, and says how to make it.
- **A device's key pair** is made when the device is added, and **its private key is never stored on the router**:
  - The web UI makes the key pair in the browser's session on the unprivileged web server (Go's standard library has X25519, WireGuard's key type), puts only the public key into `router.toml` with `compiler/edit`, and shows the device's configuration **once**, to download or copy. Losing it means removing the device and adding it again.
  - A device that makes its own keys (the WireGuard apps can) can be added by pasting its public key instead.
- The API gains one read request for the router's public key, and one request that makes the router's key if there is none. Neither can read or change anything else (ADR 0014's fixed list).

### The firewall

- **One port opens on WAN**: UDP `port` to the router itself, IPv4 and IPv6, and only while `[vpn]` exists. This is an explicit, authenticated service: WireGuard answers nothing to a packet that isn't from a known peer's key, so a port scan can't tell it's there. Hard rule 5's default stays: nothing else gets in, and no setting can open WAN in general.
- Traffic from the `vpn` zone is forwarded by `reach` (above), then by the user's rules. IPv4 from the VPN to the internet is masqueraded, like every network's.
- **DNS** from the `vpn` zone to the router is allowed, and dnsmasq answers on the `vpn` interface, so connected devices use the router's DNS: local names, and later its blocklists (ADR 0019).

### The device's configuration

- Generated for each device when it's added: its private key and address, the router's DNS, and the router's public key and endpoint.
- **What goes through the tunnel**, chosen when adding the device: "everything" (`AllowedIPs = 0.0.0.0/0, ::/0`; the default for `reach = "everything"`) or "only my home networks" (the home subnets). IPv6 inside the tunnel comes later. With `::/0`, IPv6 can't leak around the tunnel; apps fall back to IPv4.
- **The endpoint**: `endpoint` if set. Otherwise it's the router's current public IPv4 address, with a warning that it can change, and a pointer to dynamic DNS (DDNS, a name that follows the address). The page also warns when the router's WAN address is a private or shared one (`10/8`, `172.16/12`, `192.168/16`, `100.64/10`: the ISP's carrier-grade NAT), because then nothing from the internet can reach the router, and a VPN can't work without the ISP's help.
- A QR code for phone apps comes later: it needs a QR encoder, a separate small decision (a package, a module, or our own).

### Pages and commands

- **CLI:** `rollgate vpn` shows the router's public key and each device's last handshake. `rollgate vpn add NAME` makes the router's key if needed, a key pair for the device, adds the peer to `router.toml` (with `compiler/edit`), and prints the device's configuration. Apply and confirm as usual.
- **Web UI:** a VPN page that lists devices with when each last connected, and has **Add a device**, **Remove**, and the management checkbox. Everything goes through the usual review, Apply and Keep.

## Alternatives considered

- **OpenVPN or IPsec (strongSwan):** a new daemon each, more settings, and slower. WireGuard is in the kernel and networkd already speaks it.
- **Tailscale or ZeroTier:** easy, and they work behind carrier-grade NAT, but they depend on a company's coordination servers and add a daemon and a package source (the AUR or their own repo, against hard rule 1). Users who want one can run it on a device at home.
- **wg-quick:** its own scripts and config format, next to networkd's. networkd already manages every interface.
- **Keep devices' private keys on the router** to show the config again later: a stolen router backup would then hold every device's key. Showing it once is how the WireGuard apps and most routers work.
- **Open the web UI to WAN behind a password:** ADR 0014 rejected this, and it stays rejected.

## Consequences

- Config spec: a `[vpn]` section and `[[vpn.peer]]` (written into `docs/config-spec.md` and `examples/router.toml` before the code), and validation: unique names, keys and addresses; `address` mustn't overlap other networks (E002); the port must be free; `"vpn"` is a reserved zone name.
- Compiler: a WireGuard `.netdev` and `.network`, nftables input for the port on WAN, `vpn` zone forwarding and masquerading, dnsmasq on the `vpn` interface. All in golden tests.
- `wireguard-tools` joins `image/packages.txt`.
- Tests:
  - L2: two network namespaces with a real WireGuard tunnel through the generated firewall: a peer connects and reaches LAN or only the internet, per `reach`; an unknown key gets nothing.
  - L3: the VM with a WireGuard client on the host through QEMU's forwarded UDP port: connect, resolve a local name, reach the LAN, and with the VPN removed, the port is closed again.
- The firewall and management access change, so the code PRs need the maintainer's review (AGENTS.md, safety-critical).
