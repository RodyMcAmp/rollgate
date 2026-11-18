#!/usr/bin/env bash
# L2 namespace test (docs/testing.md): compiles tests/netns/router.toml, loads the
# generated nftables ruleset and sysctl settings into a "router" network namespace,
# runs dnsmasq there with the generated config, and sends real traffic between
# namespaces to check the firewall policy, NAT, DHCP and local DNS:
#
#   [rg-wan] ──veth── [rg-router] ──veth── [rg-lan]
#   "internet"             │
#                          └──────veth── [rg-iot]
#
# Interface addresses are set by this script, standing in for networkd (which
# loads the generated .network files on a real router; the L3 tests cover that
# from M3). IPv6 uses fixed test addresses until ipv6 = "auto" lands (M2 PR 6).
#
# Usage:  sudo tests/netns/run.sh        (needs root, ip, nft, sysctl, go)
# Env:    ROLLGATE  path to the rollgate binary (default: build/rollgate)
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
rollgate=$(realpath -m "${ROLLGATE:-$repo/build/rollgate}")

die() { echo "netns-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
for tool in ip nft sysctl go timeout dnsmasq unbound-checkconf; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
[[ -x $rollgate ]] || die "no rollgate binary at $rollgate (run: make rollgate)"

namespaces=(rg-wan rg-router rg-lan rg-iot)
work=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for ns in "${namespaces[@]}"; do ip netns del "$ns" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

log "compiling tests/netns/router.toml"
"$rollgate" compile -o "$work/out" "$here/router.toml" >/dev/null
go build -o "$work/listen" "$here/listen.go"
go build -o "$work/probe" "$here/probe.go"

log "checking the generated DNS configs with the daemons' own checkers"
dnsmasq --test --conf-file="$work/out/etc/dnsmasq.conf"
unbound-checkconf "$work/out/etc/unbound/unbound.conf"

log "building the namespaces"
for ns in "${namespaces[@]}"; do
  ip netns del "$ns" 2>/dev/null || true
  ip netns add "$ns"
  ip -n "$ns" link set lo up
done
# veth <router device> <peer namespace>: the peer end is called eth0.
veth() {
  ip link add "$1" netns rg-router type veth peer name eth0 netns "$2"
  ip -n rg-router link set "$1" up
  ip -n "$2" link set eth0 up
}
veth wan rg-wan
veth lan rg-lan
veth iot rg-iot

# addr <namespace> <address/prefix>...
addr() {
  local ns=$1 a
  shift
  for a in "$@"; do ip -n "$ns" addr add "$a" dev "${dev:-eth0}" nodad 2>/dev/null || ip -n "$ns" addr add "$a" dev "${dev:-eth0}"; done
}
# The router's LAN addresses come from the compiled networkd files.
lan_addr=$(sed -n 's/^Address=//p' "$work/out/etc/systemd/network/30-rollgate-lan.network")
iot_addr=$(sed -n 's/^Address=//p' "$work/out/etc/systemd/network/30-rollgate-iot.network")
[[ $lan_addr == 192.168.10.1/24 && $iot_addr == 192.168.20.1/24 ]] || die "unexpected compiled addresses: $lan_addr $iot_addr"
dev=wan addr rg-router 203.0.113.2/24 2001:db8:ffff::2/64
dev=lan addr rg-router "$lan_addr" fd00:10::1/64
dev=iot addr rg-router "$iot_addr" fd00:20::1/64
addr rg-wan 203.0.113.1/24 2001:db8:ffff::1/64
addr rg-lan 192.168.10.50/24 fd00:10::50/64
addr rg-iot 192.168.20.50/24 fd00:20::50/64
ip -n rg-lan route add default via 192.168.10.1
ip -n rg-lan -6 route add default via fd00:10::1
ip -n rg-iot route add default via 192.168.20.1
ip -n rg-iot -6 route add default via fd00:20::1
ip -n rg-router route add default via 203.0.113.1
ip -n rg-router -6 route add default via 2001:db8:ffff::1
# IPv6 isn't NATed, so the "internet" routes the local prefixes back to the router,
# as an ISP does for a delegated prefix. (IPv4 gets no such route until the last
# checks: replies must work through NAT alone.)
ip -n rg-wan -6 route add fd00::/8 via 2001:db8:ffff::2

log "loading the generated sysctl settings and firewall into the router namespace"
ip netns exec rg-router sysctl -q -p "$work/out/etc/sysctl.d/90-rollgate.conf"
ip netns exec rg-router nft -f "$work/out/etc/nftables.conf"

log "starting dnsmasq with the generated config"
# Only the lease, pid and log files move into the test directory, and it runs as
# root so it can write them; everything else comes from the generated file.
ip netns exec rg-router dnsmasq --conf-file="$work/out/etc/dnsmasq.conf" --keep-in-foreground \
  --user=root --group=root --pid-file="$work/dnsmasq.pid" --dhcp-leasefile="$work/leases" \
  --log-facility="$work/dnsmasq.log" &
pids+=($!)

# listen <namespace> <port>: starts a TCP listener; peers are logged to $work/<ns>-<port>.log
listen() {
  ip netns exec "$1" "$work/listen" "$2" "$work/$1-$2.log" &
  pids+=($!)
}
listen rg-wan 80     # a web server on the internet
listen rg-lan 80     # a server on the LAN (also the port forward's target)
listen rg-iot 80     # a device in the IoT zone
listen rg-router 22  # stands in for sshd on the router (DNS on port 53 is the real dnsmasq)
sleep 1

failures=0
# expect <open|blocked> <description> <namespace> <address> <port>
expect() {
  local want=$1 what=$2 ns=$3 host=$4 port=$5 got
  if ip netns exec "$ns" timeout 2 bash -c "exec 3<>/dev/tcp/$host/$port" 2>/dev/null; then
    got=open
  else
    got=blocked
  fi
  if [[ $got == "$want" ]]; then
    echo "    ok    $what ($got)"
  else
    echo "    WRONG $what: expected $want, got $got"
    failures=$((failures + 1))
  fi
}

log "checking the firewall policy"
expect open    "LAN reaches the internet (IPv4)"                  rg-lan 203.0.113.1 80
expect open    "LAN reaches the internet (IPv6)"                  rg-lan 2001:db8:ffff::1 80
expect open    "IoT reaches the internet (IPv4)"                  rg-iot 203.0.113.1 80
expect open    "IoT reaches the internet (IPv6)"                  rg-iot 2001:db8:ffff::1 80
expect blocked "IoT can't reach the LAN (IPv4)"                   rg-iot 192.168.10.50 80
expect blocked "IoT can't reach the LAN (IPv6)"                   rg-iot fd00:10::50 80
expect open    "LAN reaches IoT devices (policy lan -> *)"        rg-lan 192.168.20.50 80
expect open    "LAN reaches SSH on the router"                    rg-lan 192.168.10.1 22
expect blocked "IoT can't reach SSH (not a management zone)"      rg-iot 192.168.20.1 22
expect blocked "IoT can't reach SSH over IPv6"                    rg-iot fd00:20::1 22
expect open    "IoT reaches DNS on the router ([dhcp.iot])"       rg-iot 192.168.20.1 53
expect blocked "WAN can't reach SSH on the router"                rg-wan 203.0.113.2 22
expect blocked "WAN can't reach SSH on the router over IPv6"      rg-wan 2001:db8:ffff::2 22
expect blocked "WAN can't reach DNS on the router"                rg-wan 203.0.113.2 53
expect open    "WAN reaches the port forward (8080 -> lan:80)"    rg-wan 203.0.113.2 8080
expect blocked "WAN can't reach an unforwarded port"              rg-wan 203.0.113.2 8081

log "checking NAT"
# The internet host saw LAN and IoT connections from the router's WAN address.
peers=$(sort -u "$work/rg-wan-80.log" | { grep -v : || true; } | tr '\n' ' ')
if [[ $peers == "203.0.113.2 " ]]; then
  echo "    ok    IPv4 to the internet is masqueraded as 203.0.113.2"
else
  echo "    WRONG IPv4 to the internet came from: $peers(expected only 203.0.113.2)"
  failures=$((failures + 1))
fi
if grep -qx 'fd00:10::50' "$work/rg-wan-80.log"; then
  echo "    ok    IPv6 isn't NATed"
else
  echo "    WRONG IPv6 to the internet didn't keep the LAN address"
  failures=$((failures + 1))
fi

log "checking that WAN can't reach local networks directly"
# An attacker on the ISP's network routes LAN addresses at the router.
ip -n rg-wan route add 192.168.0.0/16 via 203.0.113.2
expect blocked "WAN can't reach a LAN device directly (IPv4)"     rg-wan 192.168.10.50 80
expect blocked "WAN can't reach a LAN device directly (IPv6)"     rg-wan fd00:10::50 80
expect blocked "WAN can't reach an IoT device directly (IPv6)"    rg-wan fd00:20::50 80

log "checking DHCP and local DNS (dnsmasq)"
# check <description> <expected output as a glob pattern, or "" to expect no answer>
#       <namespace> <probe args...>
check() {
  local what=$1 want=$2 ns=$3 got
  shift 3
  got=$(ip netns exec "$ns" "$work/probe" "$@" 2>/dev/null | tr '\n' ' ' | sed 's/ $//') || got=""
  # shellcheck disable=SC2053 # $want is a pattern on purpose
  if [[ $got == $want ]]; then
    echo "    ok    $what${got:+ ($got)}"
  else
    echo "    WRONG $what: expected '${want:-no answer}', got '${got:-no answer}'"
    failures=$((failures + 1))
  fi
}
check "LAN device with a static lease gets its fixed address" \
  "address=192.168.10.5 router=192.168.10.1 dns=192.168.10.1" rg-lan dhcp 00:00:5e:00:53:10
check "IoT device gets an address from the IoT range" \
  "address=192.168.20.1[0-9][0-9] router=192.168.20.1 dns=192.168.20.1" rg-iot dhcp 00:00:5e:00:53:20
check "No DHCP on the internet side" "" rg-wan dhcp 00:00:5e:00:53:30
check "The router's name resolves on the LAN"     "192.168.10.1" rg-lan dns 192.168.10.1 gateway.home.arpa
check "The router's name resolves on IoT"         "192.168.20.1" rg-iot dns 192.168.20.1 gateway.home.arpa
check "A DHCP device's name resolves"             "192.168.10.5" rg-lan dns 192.168.10.1 nas.home.arpa
check "No DNS for the internet side"              "" rg-wan dns 203.0.113.2 gateway.home.arpa

if ((failures > 0)); then
  echo "--- dnsmasq log:"
  cat "$work/dnsmasq.log" || true
  die "$failures check(s) failed"
fi
log "PASS: firewall, NAT, port forward, DHCP and local DNS behave as configured"
