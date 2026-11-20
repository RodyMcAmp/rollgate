#!/usr/bin/env bash
# L3 DNS blocklist test (ADR 0019): blocklists on a router in a VM.
#
#   1. The config sets dns.blocklists to a list the host serves over HTTPS (its
#      certificate is trusted by rollgate-blocklists.service only) and allows one
#      name. rollgate-blocklists.timer is active. The apply starts a download by
#      itself, and rollgate status says how many names are blocked.
#   2. Through the router's DNS: listed names and names under them answer 0.0.0.0,
#      in hosts and Adblock format; the allowed name doesn't.
#   3. With the list server gone, an update keeps the last good list, and status
#      says the download failed.
#   4. Without dns.blocklists, names resolve normally again, and the next update
#      deletes the list file.
#
# Usage:  sudo tests/vm/blocklist-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; PORT (default 2270: SSH,
#         then DNS and the list server on the next two)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: blocklist-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
port=${PORT:-2270}
ssh_port=$port dns_port=$((port + 1)) list_port=$((port + 2))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/blocklist.serial.log"
# From the VM, QEMU's user network reaches the host's 127.0.0.1 at 10.0.2.2.
list_url="https://10.0.2.2:$list_port/list.txt"

die() { echo "blocklist-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for tool in qemu-system-x86_64 openssl ssh-keygen go; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid='' serve_pid=''
cleanup() {
  [[ -n $serve_pid ]] && kill "$serve_pid" 2>/dev/null
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/root" "$work/lists"

log "building the helpers; a certificate and a list for the host's HTTPS server"
go build -o "$work/serve" "$here/serve.go"
go build -o "$work/dnsq" "$here/dnsq.go"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=rollgate-test \
  -addext "subjectAltName=IP:10.0.2.2" -keyout "$work/list.key" -out "$work/list.crt" 2>/dev/null
cat > "$work/lists/list.txt" <<'EOF'
# test list: hosts and Adblock lines
0.0.0.0 ads.test-block.example
0.0.0.0 good.test-block.example
||tracker.test-block.example^
EOF
"$work/serve" -addr "127.0.0.1:$list_port" -dir "$work/lists" -cert "$work/list.crt" -key "$work/list.key" &
serve_pid=$!

# good.toml, with DHCP (and so DNS) on lan, where the host reaches the router,
# and the blocklist.
blocklist_config() {
  cat "$here/apply/good.toml"
  printf '\n[dhcp.lan]\nrange = ["10.0.2.100", "10.0.2.199"]\n'
  if [[ $1 == on ]]; then
    printf '\n[dns]\nblocklists = ["%s"]\nallow = ["good.test-block.example"]\n' "$list_url"
  fi
}

log "preparing a copy of the image with a test SSH key, the config and the test CA"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
# This test applies its config itself and counts generations: no first-boot apply (ADR 0020).
ln -sf /dev/null "$work/root/etc/systemd/system/rollgate-firstboot.service"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
blocklist_config on > "$work/root/etc/rollgate/router.toml"
install -m 0644 "$work/list.crt" "$work/root/etc/rollgate-test-ca.pem"
install -d "$work/root/etc/systemd/system/rollgate-blocklists.service.d"
printf '[Service]\nEnvironment=SSL_CERT_FILE=/etc/rollgate-test-ca.pem\n' \
  > "$work/root/etc/systemd/system/rollgate-blocklists.service.d/test-ca.conf"
umount "$work/root"

: > "$serial"
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:01,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22,hostfwd=udp:127.0.0.1:"$dns_port"-:53 \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:02,net=10.0.3.0/24 \
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
# ask NAME: the router's answer, through its DNS on lan.
ask() { "$work/dnsq" -server "127.0.0.1:$dns_port" "$1"; }
# until_status TEXT: wait up to 60 s for rollgate status to say TEXT.
until_status() {
  local start=$SECONDS
  until grep -qF "$1" <<<"$(vm 'rollgate status')"; do
    ((SECONDS - start < 60)) || die "rollgate status never said '$1': $(vm 'rollgate status'; vm 'journalctl -u rollgate-blocklists --no-pager | tail -20')"
    sleep 2
  done
}

log "1. apply: the blocklist downloads by itself"
grep -qx "active" <<<"$(vm 'systemctl is-active rollgate-blocklists.timer')" || die "rollgate-blocklists.timer isn't active: $(vm 'systemctl status rollgate-blocklists.timer --no-pager' || true)"
out=$(vm 'rollgate apply' 2>&1) || die "rollgate apply failed: $out"
[[ $out == *"Downloading the DNS blocklists in the background"* ]] || die "apply didn't start the download: $out"
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
until_status "DNS blocklists: 2 names blocked"
log "  apply started the download; status: $(vm 'rollgate status' | grep 'DNS blocklists')"

log "2. blocked names answer 0.0.0.0; the allowed one doesn't"
for name in ads.test-block.example. sub.ads.test-block.example. tracker.test-block.example.; do
  got=$(ask "$name")
  [[ $got == "0.0.0.0" ]] || die "$name: got '$got', want 0.0.0.0"
done
got=$(ask good.test-block.example.)
[[ $got != *"0.0.0.0"* ]] || die "the allowed name is blocked: $got"
log "  ads, a name under it and the Adblock entry answer 0.0.0.0; the allowed name gets a normal answer ($got)"

log "3. the list server is gone: the last good list stays"
kill "$serve_pid"; wait "$serve_pid" 2>/dev/null || true; serve_pid=
vm 'systemctl start rollgate-blocklists.service' || die "the update failed instead of keeping the last good list: $(vm 'journalctl -u rollgate-blocklists --no-pager | tail -10')"
[[ $(ask ads.test-block.example.) == "0.0.0.0" ]] || die "blocking stopped after a failed download"
until_status "1 of 1 list(s) failed to download"
log "  still blocked; status says the download failed"

log "4. without dns.blocklists: names resolve normally, the file goes away"
blocklist_config off | vm 'cat > /etc/rollgate/router.toml'
vm 'rollgate apply' >/dev/null 2>&1 || die "applying without blocklists failed"
vm 'rollgate confirm' >/dev/null || die "rollgate confirm failed"
# The apply starts the update in the background, which clears dnsmasq's cache.
start=$SECONDS
until got=$(ask ads.test-block.example.); [[ $got != *"0.0.0.0"* ]]; do
  ((SECONDS - start < 60)) || die "still blocked a minute after removing dns.blocklists: $got"
  sleep 2
done
vm 'systemctl start rollgate-blocklists.service' || die "the update without blocklists failed"
vm 'test ! -e /etc/unbound/rollgate-blocklist.conf' || die "the list file stays without dns.blocklists"
grep -q "DNS blocklists" <<<"$(vm 'rollgate status')" && die "status still talks about blocklists"
log "  not blocked any more; the list file is gone"
echo "blocklist-test: PASS"
