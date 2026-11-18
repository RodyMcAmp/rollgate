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
# loads the generated .network files on a real router; the L3 tests cover that,
# including router advertisements and prefix delegation, from M3). The router's
# IPv4 and IPv6 addresses are read from the compiled .network files.
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
for tool in ip nft sysctl go timeout dnsmasq unbound-checkconf sshd ssh ssh-keygen; do
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

log "checking the generated sshd config as sshd reads it"
ssh-keygen -q -t ed25519 -N '' -f "$work/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$work/admin"     # stands in for secrets/admin.pub
ssh-keygen -q -t ed25519 -N '' -f "$work/stranger"  # a key nobody listed
cat > "$work/sshd_config" <<EOF
Include $work/out/etc/ssh/sshd_config.d/*.conf
HostKey $work/hostkey
PidFile $work/sshd.pid
EOF
sshd_bin=$(command -v sshd)
effective=$("$sshd_bin" -T -f "$work/sshd_config")
for want in "permitrootlogin prohibit-password" "authenticationmethods publickey" \
  "passwordauthentication no" "kbdinteractiveauthentication no" \
  "authorizedkeysfile /etc/rollgate/secrets/admin.pub"; do
  # OpenSSH 10 prints setting names capitalized; older versions in lowercase.
  grep -qix "$want" <<<"$effective" || die "sshd's effective config lacks '$want'"
done

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
# The router's addresses come from the compiled networkd files: IPv4 as configured,
# IPv6 from the ULA prefix and each network's subnet number (ipv6 = "auto").
compiled() { sed -n "s/^Address=\($2\)/\1/p" "$work/out/etc/systemd/network/30-rollgate-$1.network"; }
lan_addr=$(compiled lan '[0-9.]*\/[0-9]*') lan6=$(compiled lan 'fd.*')
iot_addr=$(compiled iot '[0-9.]*\/[0-9]*') iot6=$(compiled iot 'fd.*')
[[ $lan_addr == 192.168.10.1/24 && $iot_addr == 192.168.20.1/24 ]] || die "unexpected compiled addresses: $lan_addr $iot_addr"
[[ $lan6 == fd12:3456:789a:*::1/64 && $iot6 == fd12:3456:789a:*::1/64 && $lan6 != "$iot6" ]] ||
  die "unexpected compiled IPv6 addresses: '$lan6' '$iot6'"
lan6_net=${lan6%::1/64} iot6_net=${iot6%::1/64}
echo "    LAN IPv6 ${lan6_net}::/64, IoT IPv6 ${iot6_net}::/64"
dev=wan addr rg-router 203.0.113.2/24 2001:db8:ffff::2/64
dev=lan addr rg-router "$lan_addr" "$lan6"
dev=iot addr rg-router "$iot_addr" "$iot6"
addr rg-wan 203.0.113.1/24 2001:db8:ffff::1/64
addr rg-lan 192.168.10.50/24 "${lan6_net}::50/64"
addr rg-iot 192.168.20.50/24 "${iot6_net}::50/64"
ip -n rg-lan route add default via 192.168.10.1
ip -n rg-lan -6 route add default via "${lan6_net}::1"
ip -n rg-iot route add default via 192.168.20.1
ip -n rg-iot -6 route add default via "${iot6_net}::1"
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
# Port 53 on the router is the real dnsmasq (above); port 22 is the real sshd with
# the generated settings. Only the key file location is overridden, because the
# test's key isn't in /etc/rollgate/secrets/ (and StrictModes refuses key files
# under /tmp, which is world-writable).
cp "$work/admin.pub" "$work/authorized_keys"
ip netns exec rg-router "$sshd_bin" -D -e -f "$work/sshd_config" \
  -o AuthorizedKeysFile="$work/authorized_keys" -o StrictModes=no 2>"$work/sshd.log" &
pids+=($!)
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
expect blocked "IoT can't reach the LAN (IPv6)"                   rg-iot "${lan6_net}::50" 80
expect open    "LAN reaches IoT devices (policy lan -> *)"        rg-lan 192.168.20.50 80
expect open    "LAN reaches SSH on the router"                    rg-lan 192.168.10.1 22
expect blocked "IoT can't reach SSH (not a management zone)"      rg-iot 192.168.20.1 22
expect blocked "IoT can't reach SSH over IPv6"                    rg-iot "${iot6_net}::1" 22
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
if grep -qx "${lan6_net}::50" "$work/rg-wan-80.log"; then
  echo "    ok    IPv6 isn't NATed"
else
  echo "    WRONG IPv6 to the internet didn't keep the LAN address"
  failures=$((failures + 1))
fi

log "checking that WAN can't reach local networks directly"
# An attacker on the ISP's network routes LAN addresses at the router.
ip -n rg-wan route add 192.168.0.0/16 via 203.0.113.2
expect blocked "WAN can't reach a LAN device directly (IPv4)"     rg-wan 192.168.10.50 80
expect blocked "WAN can't reach a LAN device directly (IPv6)"     rg-wan "${lan6_net}::50" 80
expect blocked "WAN can't reach an IoT device directly (IPv6)"    rg-wan "${iot6_net}::50" 80

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

log "checking SSH logins (sshd)"
ssh_opts=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
  -o ConnectTimeout=3 -o IdentitiesOnly=yes)
# login <in|refused> <description> <namespace> <router address> <ssh options...>
login() {
  local want=$1 what=$2 ns=$3 host=$4 got out
  shift 4
  if out=$(ip netns exec "$ns" timeout 10 ssh "${ssh_opts[@]}" "$@" "root@$host" true 2>&1); then
    got=in
  else
    got=refused
  fi
  if [[ $got == "$want" ]]; then
    echo "    ok    $what ($got)"
  else
    echo "    WRONG $what: expected $want, got $got: $out"
    failures=$((failures + 1))
  fi
  last_ssh_output=$out
}
login in      "The listed key logs in from the LAN"           rg-lan 192.168.10.1 -i "$work/admin"
login refused "A key that isn't listed is refused"            rg-lan 192.168.10.1 -i "$work/stranger"
login refused "Password login is refused"                     rg-lan 192.168.10.1 \
  -o PubkeyAuthentication=no -o PreferredAuthentications=password,keyboard-interactive
if [[ $last_ssh_output == *"Permission denied (publickey)"* ]]; then
  echo "    ok    The router offers public-key login only"
else
  echo "    WRONG The router should offer public-key login only; ssh said: $last_ssh_output"
  failures=$((failures + 1))
fi
login refused "The listed key can't log in from IoT"          rg-iot 192.168.20.1 -i "$work/admin"
login refused "The listed key can't log in from the internet" rg-wan 203.0.113.2 -i "$work/admin"

if ((failures > 0)); then
  echo "--- dnsmasq log:"
  cat "$work/dnsmasq.log" || true
  echo "--- sshd log:"
  cat "$work/sshd.log" || true
  die "$failures check(s) failed"
fi
log "PASS: firewall, NAT, port forward, DHCP, local DNS and SSH behave as configured"
