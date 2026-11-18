#!/usr/bin/env bash
# L3 IPv6 test (docs/testing.md, ADR 0008): ipv6 = "auto" on the booted image, with
# a fake ISP and a LAN client.
#
#   wan  ── bridge ── rg6-isp namespace: dnsmasq (DHCPv4, later router
#                     advertisements) and tests/vm/pd-server.go (DHCPv6 prefix
#                     delegation); 2001:db8:ffff::1 and 10.99.0.1 are "the internet"
#   home ── bridge ── rg6-client namespace: a client, configured by DHCP and SLAAC
#   lan  ── QEMU user networking, only to manage the router over SSH
#
#   1. ISP without IPv6: the client gets an IPv4 lease and local names, reaches the
#      internet over IPv4 (NAT), and gets local IPv6 (ULA) only. Nothing fails.
#   2. The ISP turns IPv6 on, with no change to router.toml: the router gets a
#      delegated prefix, the client gets a public address from it and reaches the
#      IPv6 internet without NAT, and the internet can't connect back in.
#
# Usage:  sudo tests/vm/ipv6-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2226)
#         ROLLGATE   a rollgate binary to test instead of the image's (for development)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: ipv6-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2226}
root_offset=$(((2048 + 512 * 2048) * 512)) # root_start_sector in image/build.sh
serial="$log_dir/ipv6.serial.log"
report="$log_dir/ipv6.report.log"
# The MACs in tests/vm/ipv6/router.toml.
lan_mac=52:54:00:00:00:01
wan_mac=52:54:00:00:00:02
home_mac=52:54:00:00:00:03
delegated=2001:db8:1000::/56

die() {
  echo "ipv6-test: FAIL: $*" >&2
  # What the router and the client saw, for debugging a CI failure.
  if [[ -n ${qemu_pid:-} ]] && vm true 2>/dev/null; then
    echo "--- router:" >&2
    vm 'ip -br link; ip -br addr; ip -6 route; networkctl --no-pager; systemctl --failed --no-pager;
        journalctl -b --no-pager -n 40 -u systemd-networkd -u dnsmasq' >&2 2>&1 || true
  fi
  if [[ -n ${work:-} ]]; then
    echo "--- client:" >&2
    ip netns exec rg6-client ip -br addr >&2 2>&1 || true
    for f in "$work"/*.log; do [[ -f $f ]] && { echo "--- $f:" >&2; tail -n 20 "$f" >&2; }; done
  fi
  exit 1
}
log() { echo "==> $*"; }
# Any unexpected failing command reports where, with the same diagnostics.
set -E
trap 'die "unexpected error on line $LINENO: $BASH_COMMAND"' ERR

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 ssh ssh-keygen ip dnsmasq go ping; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=180
else
  echo "ipv6-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=900
fi

work=$(mktemp -d)
qemu_pid=
pids=()
cleanup() {
  local pid
  if [[ -n $qemu_pid ]]; then kill "$qemu_pid" 2>/dev/null || true; fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  # Everything still running in the namespaces (dnsmasq, helpers), or a process
  # would keep a deleted namespace and its links alive.
  for ns in rg6-isp rg6-client; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null || true
    ip netns del "$ns" 2>/dev/null || true
  done
  for side in wan home; do
    ip link del "rg6-tap-$side" 2>/dev/null || true
    ip link del "rg6-br-$side" 2>/dev/null || true
  done
  umount "$work/root" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/root" "$log_dir"

vm() {
  ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes root@127.0.0.1 "$@"
}
isp() { ip netns exec rg6-isp "$@"; }
client() { ip netns exec rg6-client "$@"; }

wait_for_boot() {
  local deadline=$((SECONDS + timeout_s)) line
  until grep -aq '^ROLLGATE_BOOT state=' "$report"; do
    ((SECONDS < deadline)) || die "no ROLLGATE_BOOT line within ${timeout_s}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (log: $serial)"
    sleep 2
  done
  line=$(grep -a -m1 '^ROLLGATE_BOOT state=' "$report" | tr -d '\r')
  echo "    $line"
  [[ $line == *"state=running "* && $line == *"failed=" ]] || die "boot did not come up cleanly: $line"
  until vm true 2>/dev/null; do
    ((SECONDS < deadline)) || die "SSH not reachable"
    sleep 2
  done
}

# eventually <seconds> <what> <command...>: retries a command until it succeeds.
# Every 15 s it bounces the client's link, so the client asks for router
# advertisements again (as a device does when it reconnects).
eventually() {
  local limit=$1 what=$2 start=$SECONDS next=15
  shift 2
  until "$@" >/dev/null 2>&1; do
    ((SECONDS - start < limit)) || die "$what (gave up after $limit s)"
    if ((SECONDS - start >= next)); then
      client ip link set eth0 down
      client ip link set eth0 up
      next=$((next + 15))
    fi
    sleep 2
  done
}

# client_has <regex>: the client has a global IPv6 address matching regex.
client_has() { client ip -6 -o addr show dev eth0 scope global | grep -qE "inet6 $1"; }

# The first four groups of an IPv6 /64 written as "a:b:c:d::1/64".
net64() { sed -E 's/::1\/64$//' <<<"$1"; }

log "building the fake ISP's helpers"
go build -o "$work/pd-server" "$here/pd-server.go"
go build -o "$work/probe" "$here/../netns/probe.go"
go build -o "$work/listen" "$here/../netns/listen.go"

log "building the fake ISP and the client network"
ip netns add rg6-isp
ip netns add rg6-client
for side in wan home; do
  ns=rg6-isp
  [[ $side == home ]] && ns=rg6-client
  ip link add "rg6-br-$side" type bridge mcast_snooping 0
  sysctl -qw "net.ipv6.conf.rg6-br-$side.disable_ipv6=1"
  ip tuntap add dev "rg6-tap-$side" mode tap
  ip link add "rg6-v-$side" type veth peer name eth0 netns "$ns"
  for port in "rg6-tap-$side" "rg6-v-$side"; do
    ip link set "$port" master "rg6-br-$side" up
  done
  ip link set "rg6-br-$side" up
  ip netns exec "$ns" ip link set lo up
  ip netns exec "$ns" ip link set eth0 up
done
isp ip addr add 10.99.0.1/24 dev eth0
isp ip -6 addr add 2001:db8:ffff::1/64 dev eth0 nodad
ip netns exec rg6-isp "$work/listen" 80 "$work/isp-80.log" &
pids+=($!)

# start_isp_dnsmasq [extra options...]: (re)starts the ISP's dnsmasq: DHCPv4 always,
# router advertisements when asked to.
isp_dnsmasq=
start_isp_dnsmasq() {
  if [[ -n $isp_dnsmasq ]]; then kill "$isp_dnsmasq"; wait "$isp_dnsmasq" 2>/dev/null || true; fi
  ip netns exec rg6-isp dnsmasq --conf-file=/dev/null --keep-in-foreground --port=0 --interface=eth0 --bind-interfaces \
    --user=root --group=root --pid-file="$work/isp-dnsmasq.pid" --dhcp-leasefile="$work/isp.leases" \
    --log-facility="$work/isp-dnsmasq.log" --dhcp-range=10.99.0.100,10.99.0.150,1h "$@" &
  isp_dnsmasq=$!
  pids+=("$isp_dnsmasq")
}
start_isp_dnsmasq

log "preparing a copy of the image with a throwaway SSH key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
install -m 0644 "$here/ipv6/router.toml" "$work/root/etc/rollgate/router.toml"
if [[ -n ${ROLLGATE:-} ]]; then
  echo "    testing $ROLLGATE instead of the image's rollgate"
  install -m 0755 "$ROLLGATE" "$work/root/usr/bin/rollgate"
fi
umount "$work/root"

: > "$serial"
: > "$report"
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=$lan_mac,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -netdev tap,id=wan,ifname=rg6-tap-wan,script=no,downscript=no \
  -device virtio-net-pci,netdev=wan,mac=$wan_mac \
  -netdev tap,id=home,ifname=rg6-tap-home,script=no,downscript=no \
  -device virtio-net-pci,netdev=home,mac=$home_mac \
  -display none -monitor none \
  -chardev file,id=report,path="$report" \
  -device virtio-serial-pci -device virtserialport,chardev=report,name=org.rollgate.report \
  -serial file:"$serial" &
qemu_pid=$!

log "boot: applying tests/vm/ipv6/router.toml"
wait_for_boot
vm 'rollgate apply' | sed 's/^/    /'
vm 'rollgate confirm' | sed 's/^/    /'

log "1. ISP without IPv6"
eventually 60 "the router got no IPv4 address from the ISP" vm 'ip -4 -br addr show dev wan | grep -q 10\.99\.0\.'
# The probe uses a plain UDP socket, not a raw one like a real DHCP client, so a
# client with no IPv4 address yet needs a route for its 255.255.255.255 request
# and no reverse-path filter (which would drop the reply from an unroutable source).
client ip route add 255.255.255.255/32 dev eth0
for conf in all default eth0; do client sysctl -qw "net.ipv4.conf.$conf.rp_filter=0"; done
lease=$(client "$work/probe" dhcp "$(client cat /sys/class/net/eth0/address)") || die "the client got no DHCP lease"
echo "    client lease: $lease"
addr=$(sed -n 's/.*address=\([^ ]*\).*/\1/p' <<<"$lease")
[[ $addr == 192.168.30.1[0-9][0-9] && $lease == *"router=192.168.30.1 "* ]] || die "unexpected lease: $lease"
client ip addr add "$addr/24" dev eth0
client ip route add default via 192.168.30.1
names=$(client "$work/probe" dns 192.168.30.1 gateway.home.arpa) || true
[[ $names == 192.168.30.1 ]] || die "gateway.home.arpa resolved to '$names' at the router, expected 192.168.30.1"
echo "    local name gateway.home.arpa -> $names"
eventually 30 "the client can't reach the IPv4 internet" client ping -c1 -W2 10.99.0.1
echo "    IPv4 internet works"

out=$(vm 'ip -6 -br addr show dev home' 2>&1) || die "couldn't read the router's addresses (exit $?): $out"
ula=$(grep -oE 'fd[0-9a-f:]+::1/64' <<<"$out" | head -n1 || true)
[[ -n $ula ]] || die "the router has no local IPv6 (ULA) address on home: $out"
eventually 60 "the client got no local IPv6 address in $(net64 "$ula")::/64" client_has "$(net64 "$ula"):"
eventually 30 "the client can't reach the router's local IPv6 address" client ping -6 -c1 -W2 "${ula%/64}"
client_has "2001:db8:" && die "the client has a public IPv6 address though the ISP offers none"
echo "    client: local IPv6 in $(net64 "$ula")::/64 only, router reachable at ${ula%/64}"
failed=$(vm 'systemctl --failed --no-legend --plain' | tr -d '\r')
[[ -z $failed ]] || die "failed units without ISP IPv6: $failed"

log "2. the ISP turns IPv6 on (router.toml unchanged)"
start_isp_dnsmasq --enable-ra --dhcp-range=2001:db8:ffff::,ra-only,64,1h --ra-param=eth0,10
ip netns exec rg6-isp "$work/pd-server" eth0 "$delegated" > "$work/pd-server.log" 2>&1 &
pids+=($!)
eventually 90 "the router got no delegated prefix on home (see pd-server.log below)" \
  vm 'ip -6 -br addr show dev home | grep -q 2001:db8:10'
public=$(vm 'ip -6 -br addr show dev home' | grep -oE '2001:db8:10[0-9a-f:]+::1/64' | head -n1)
grep -q "reply $delegated" "$work/pd-server.log" || die "the router never requested the prefix"
echo "    router: delegated $delegated; home announces $(net64 "$public")::/64"
vm 'ip -6 route show default | grep -q "via fe80"' || die "the router has no IPv6 default route via the ISP"

# The ISP routes the delegated prefix to the router, as a real ISP does.
wan_ll=$(vm 'ip -6 -br addr show dev wan' | grep -oE 'fe80::[0-9a-f:]+' | head -n1)
isp ip -6 route add "$delegated" via "$wan_ll" dev eth0

eventually 90 "the client got no public IPv6 address in $(net64 "$public")::/64" client_has "$(net64 "$public"):"
client_addr=$(client ip -6 -o addr show dev eth0 scope global | grep -oE "$(net64 "$public"):[0-9a-f:]+" | head -n1)
echo "    client: public $client_addr"
eventually 30 "the client can't reach the IPv6 internet" client ping -6 -c1 -W2 2001:db8:ffff::1
client timeout 5 bash -c 'exec 3<>/dev/tcp/2001:db8:ffff::1/80' || die "TCP to the IPv6 internet failed"
grep -qx "$client_addr" "$work/isp-80.log" || die "IPv6 to the internet didn't come from the client's own address (no NAT for IPv6): $(tr '\n' ' ' < "$work/isp-80.log")"
echo "    IPv6 internet works, from the client's own address (no NAT)"

isp ping -6 -c2 -W2 "$client_addr" >/dev/null 2>&1 && die "the internet can ping the client (WAN inbound must be default-deny)"
ip netns exec rg6-client "$work/listen" 8080 "$work/client-8080.log" &
pids+=($!)
sleep 1
isp timeout 5 bash -c "exec 3<>/dev/tcp/$client_addr/8080" 2>/dev/null && die "the internet can connect to the client"
echo "    the internet can't reach the client (inbound default-deny)"

log "PASS: ipv6 = auto works with and without ISP IPv6 (log: $serial)"
