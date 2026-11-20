#!/usr/bin/env bash
# L3 VPN test (ADR 0018): a real WireGuard client connects to the router in a VM.
#
# The client is a WireGuard interface the host makes in its own network
# namespace; its UDP socket stays in the host's namespace and reaches the
# router's WAN port through QEMU. Two small web servers on the host stand in for
# a device on the LAN (QEMU's 10.0.2.2) and a server on the internet (10.0.3.2).
#
#   1. A [vpn] section is refused until rollgate vpn key has made the router's key.
#   2. With reach = "everything": the client reaches the router, resolves the
#      router's name through it, reaches the internet, and the firewall lets it
#      through to the LAN (checked with counters; see lan_verdict). SSH over the
#      VPN is closed (management.allowed_zones doesn't list "vpn"). A client with an
#      unknown key gets nothing.
#   3. With reach = "internet": the internet still; the firewall drops it toward the LAN.
#   4. rollgate vpn add makes a device whose printed configuration connects, and
#      whose private key isn't in router.toml; rollgate vpn shows it connected;
#      rollgate vpn remove disconnects it.
#   5. Without [vpn]: the client gets nothing, and the firewall has no VPN port.
#
# Usage:  sudo tests/vm/vpn-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; PORT (default 2290: SSH,
#         then the VPN, the LAN server and the internet server on the next three)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: vpn-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
port=${PORT:-2290}
ssh_port=$port wg_port=$((port + 1)) lan_port=$((port + 2)) net_port=$((port + 3))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/vpn.serial.log"
ns=rgvpn$$ stranger=rgvpnx$$

die() { echo "vpn-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for tool in qemu-system-x86_64 ssh-keygen go wg ip curl; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
modprobe wireguard 2>/dev/null || true
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid='' pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  ip netns del "$ns" 2>/dev/null || true
  ip netns del "$stranger" 2>/dev/null || true
  ip netns del "rgvpnl$$" 2>/dev/null || true
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/root" "$work/lan" "$work/net"

log "building the helpers; the host's stand-in servers"
go build -o "$work/serve" "$here/serve.go"
go build -o "$work/dnsq" "$here/dnsq.go"
echo lan-ok > "$work/lan/x"
echo internet-ok > "$work/net/x"
"$work/serve" -addr "127.0.0.1:$lan_port" -dir "$work/lan" & pids+=($!)
"$work/serve" -addr "127.0.0.1:$net_port" -dir "$work/net" & pids+=($!)
wg genkey > "$work/client.key"
client_pub=$(wg pubkey < "$work/client.key")

# config REACH: good.toml with a [vpn] section (REACH: everything, internet) and
# the client as its device; "off" for no VPN.
config() {
  cat "$here/apply/good.toml"
  [[ $1 == off ]] && return
  printf '\n[vpn]\nreach = "%s"\n\n[[vpn.peer]]\nname = "test-client"\npublic_key = "%s"\nipv4 = "10.66.0.2"\n' "$1" "$client_pub"
}

log "preparing a copy of the image with a test SSH key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
config off > "$work/root/etc/rollgate/router.toml"
umount "$work/root"

: > "$serial"
# The router's WAN gets 10.0.3.15 from QEMU's DHCP; the host's UDP port goes to
# the VPN port there, as from the internet.
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:01,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:02,net=10.0.3.0/24,hostfwd=udp:127.0.0.1:"$wg_port"-10.0.3.15:51820 \
  -display none -monitor none -serial file:"$serial" &
qemu_pid=$!

vm() {
  timeout 120 ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
}
deadline=$((SECONDS + timeout_s))
until vm true 2>/dev/null; do
  ((SECONDS < deadline)) || die "no SSH within ${timeout_s}s (serial log: $serial)"
  kill -0 "$qemu_pid" 2>/dev/null || die "QEMU exited (serial log: $serial)"
  sleep 3
done
vm 'rollgate apply' >/dev/null 2>&1 || die "applying the base config failed"
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"

# apply_config REACH: write and apply a config, and keep it.
apply_config() {
  config "$1" | vm 'cat > /etc/rollgate/router.toml'
  out=$(vm 'rollgate apply' 2>&1) || die "applying the $1 config failed: $out"
  vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
}
# client NS KEYFILE [ADDRESS]: a WireGuard client in namespace NS, as ADDRESS
# (default 10.66.0.2).
client() {
  local dev=wg$1
  ip netns add "$1"
  ip link add "$dev" type wireguard
  wg set "$dev" private-key "$2" peer "$router_pub" endpoint "127.0.0.1:$wg_port" allowed-ips 0.0.0.0/0 persistent-keepalive 5
  ip link set "$dev" netns "$1"
  ip -n "$1" addr add "${3:-10.66.0.2}/32" dev "$dev"
  ip -n "$1" link set lo up
  ip -n "$1" link set "$dev" up
  ip -n "$1" route add default dev "$dev"
}
in_ns() { ip netns exec "$ns" "$@"; }
reaches() { in_ns curl -s -m 5 "http://$1/x"; }
# lan_verdict: what the firewall does with the client's traffic to the LAN,
# "accepted" or "dropped". QEMU's user network can't answer an address outside
# its own (10.66.0.2), as a real LAN device would through the router, so this
# counts packets instead: one rule first in the forward chain sees them all, one
# last sees only those no rule accepted. Both only count; the next apply's
# ruleset replaces them.
lan_verdict() {
  vm 'nft insert rule inet rollgate forward iifname "vpn" oifname "lan" counter comment "\"rgtest-seen\""
      nft add rule inet rollgate forward iifname "vpn" oifname "lan" counter comment "\"rgtest-fell\""' ||
    die "couldn't add the counting rules"
  in_ns curl -s -m 3 "http://10.0.2.2:$lan_port/x" >/dev/null || true
  local rules seen fell
  rules=$(vm 'nft list chain inet rollgate forward')
  seen=$(sed -n 's/.*counter packets \([0-9]*\) .*rgtest-seen.*/\1/p' <<<"$rules")
  fell=$(sed -n 's/.*counter packets \([0-9]*\) .*rgtest-fell.*/\1/p' <<<"$rules")
  [[ ${seen:-0} -gt 0 ]] || die "no packet from the VPN toward the LAN reached the firewall: $rules"
  if [[ ${fell:-0} -eq 0 ]]; then echo accepted; else echo dropped; fi
}

log "1. no VPN without the router's key"
config everything | vm 'cat > /etc/rollgate/router.toml'
if out=$(vm 'rollgate apply' 2>&1); then die "apply took [vpn] before the router's key existed: $out"; fi
[[ $out == *"run rollgate vpn key first"* ]] || die "apply's refusal doesn't say what to do: $out"
out=$(vm 'rollgate vpn key') || die "rollgate vpn key failed"
router_pub=$(sed -n 's/^The router.s VPN public key: //p' <<<"$out")
[[ -n $router_pub ]] || die "rollgate vpn key printed no public key: $out"
[[ $(vm 'stat -c "%a %U:%G" /etc/rollgate/secrets/vpn.key') == "640 root:systemd-network" ]] ||
  die "the key file's permissions: $(vm 'stat -c "%a %U:%G" /etc/rollgate/secrets/vpn.key')"
[[ $(vm 'rollgate vpn key' | sed -n 's/^The router.s VPN public key: //p') == "$router_pub" ]] || die "a second rollgate vpn key changed the key"
log "  refused, then made the key ($router_pub), root:systemd-network 0640; a second run keeps it"

log "2. reach = everything: the router, its DNS, the LAN and the internet"
apply_config everything
client "$ns" "$work/client.key"
start=$SECONDS
until in_ns ping -c 1 -W 2 10.66.0.1 >/dev/null 2>&1; do
  ((SECONDS - start < 60)) || die "the client can't reach the router over the VPN: $(vm 'networkctl status vpn --no-pager; journalctl -u systemd-networkd --no-pager | tail -15' || true)"
  sleep 1
done
got=$(in_ns "$work/dnsq" -server 10.66.0.1:53 gateway.home.arpa.)
[[ $got == "10.66.0.1" ]] || die "the router's name over the VPN: got '$got', want 10.66.0.1"
[[ $(lan_verdict) == accepted ]] || die "the firewall doesn't let the client through to the LAN"
[[ $(reaches "10.0.3.2:$net_port") == internet-ok ]] || die "the client doesn't reach the internet"
if in_ns timeout 5 bash -c 'exec 3<>/dev/tcp/10.66.0.1/22' 2>/dev/null; then die "SSH answers over the VPN, though allowed_zones doesn't list vpn"; fi
wg genkey > "$work/stranger.key"
client "$stranger" "$work/stranger.key"
if ip netns exec "$stranger" ping -c 2 -W 2 10.66.0.1 >/dev/null 2>&1; then die "a client with an unknown key reached the router"; fi
ip netns del "$stranger"
log "  ping, DNS (gateway.home.arpa = 10.66.0.1), LAN and internet work; SSH is closed; an unknown key gets nothing"

log "3. reach = internet: the internet, not the LAN"
apply_config internet
[[ $(reaches "10.0.3.2:$net_port") == internet-ok ]] || die "the client lost the internet with reach = internet"
[[ $(lan_verdict) == dropped ]] || die "the firewall still lets the client through to the LAN with reach = internet"
log "  internet yes, LAN no"

log "4. rollgate vpn add, a client with the configuration it prints, rollgate vpn, rollgate vpn remove"
out=$(vm 'rollgate vpn add laptop' 2>&1) || die "rollgate vpn add failed: $out"
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
sed -n 's/^PrivateKey = //p' <<<"$out" > "$work/laptop.key"
laptop_addr=$(sed -n 's/^Address = \(.*\)\/32$/\1/p' <<<"$out")
[[ -s $work/laptop.key && $laptop_addr == 10.66.0.3 ]] || die "rollgate vpn add printed no usable configuration: $out"
grep -q "^Endpoint = 10.0.3.15:51820$" <<<"$out" || die "the configuration's endpoint isn't the router's WAN address: $out"
grep -q "carrier-grade NAT\|forwards UDP port" <<<"$out" || die "no warning about the private WAN address: $out"
vm 'grep -q "name = \"laptop\"" /etc/rollgate/router.toml' || die "router.toml has no laptop"
vm 'grep -q "^PrivateKey" /etc/rollgate/router.toml' && die "the device's private key is in router.toml"
laptop=rgvpnl$$
client "$laptop" "$work/laptop.key" "$laptop_addr"
start=$SECONDS
until ip netns exec "$laptop" ping -c 1 -W 2 10.66.0.1 >/dev/null 2>&1; do
  ((SECONDS - start < 60)) || { ip netns del "$laptop"; die "the added device can't reach the router"; }
  sleep 1
done
status=$(vm 'rollgate vpn')
grep -Eq "laptop +10.66.0.3 +last connected (just now|[0-9]+ min ago)" <<<"$status" || { ip netns del "$laptop"; die "rollgate vpn doesn't show the laptop connected: $status"; }
vm 'rollgate vpn remove laptop' >/dev/null 2>&1 || { ip netns del "$laptop"; die "rollgate vpn remove failed"; }
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
if ip netns exec "$laptop" ping -c 2 -W 2 10.66.0.1 >/dev/null 2>&1; then ip netns del "$laptop"; die "the removed device still reaches the router"; fi
ip netns del "$laptop"
log "  added (10.66.0.3, with a warning about the private WAN address), connected, shown by rollgate vpn, removed"

log "5. without [vpn]: nothing, and no port open"
apply_config off
if in_ns ping -c 2 -W 2 10.66.0.1 >/dev/null 2>&1; then die "the client still reaches the router without [vpn]"; fi
grep -q "51820" <<<"$(vm 'nft list ruleset')" && die "the firewall still opens the VPN port"
log "  the client gets nothing; the port is closed"
echo "vpn-test: PASS"
