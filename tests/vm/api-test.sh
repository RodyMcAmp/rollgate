#!/usr/bin/env bash
# L3 API test (ADR 0014): the local API the web UI will use, on the booted image.
#
#   1. rollgate-api.socket is off by default; started, its socket is root:rollgate-api
#      0660, and a user outside that group can't connect.
#   2. Over the socket: status; an invalid config is refused and changes nothing.
#   3. Apply tests/vm/apply/good.toml: it becomes router.toml and runs. A confirm from
#      the applying connection is refused; from another it works (ADR 0010).
#   4. The health checks pass over the API, and the journal names who asked what.
#
# Usage:  sudo tests/vm/api-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2244)

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: api-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2244}
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/api.serial.log"

die() { echo "api-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420
else
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d)
qemu_pid=
cleanup() {
  [[ -n $qemu_pid ]] && kill "$qemu_pid" 2>/dev/null && wait "$qemu_pid" 2>/dev/null
  mountpoint -q "$work/root" && umount "$work/root"
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$log_dir" "$work/root"

log "preparing a copy of the image with a test SSH key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
install -m 0644 "$here/apply/good.toml" "$work/root/root/good.toml"
umount "$work/root"

: > "$serial"
# lan (10.0.2.15 in good.toml) is QEMU's first user network, which the host reaches.
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=52:54:00:00:00:01,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
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

# api METHOD PATH [JSON]: one request over the socket; prints "HTTP-code body".
vm 'cat > /root/api <<"EOF"
#!/bin/sh
exec curl -s --unix-socket /run/rollgate/api.sock -X "$1" -w " %{http_code}" \
  ${3:+-H "Content-Type: application/json" --data-binary "$3"} "http://rollgate$2"
EOF
chmod +x /root/api'
# json_config FILE CONNECTION: an apply/validate body with FILE as the config.
vm 'cat > /root/body <<"EOF"
#!/bin/sh
config=$(sed -e "s/\\\\/\\\\\\\\/g" -e "s/\"/\\\\\"/g" "$1" | awk "{printf \"%s\\\\n\", \$0}")
printf "{\"config\": \"%s\", \"connection\": \"%s\"}" "$config" "$2"
EOF
chmod +x /root/body'

log "1. the socket"
[[ $(vm 'systemctl is-enabled rollgate-api.socket') == disabled ]] || die "rollgate-api.socket is enabled by default"
vm 'systemctl start rollgate-api.socket'
perms=$(vm 'stat -c "%U:%G %a" /run/rollgate/api.sock')
[[ $perms == "root:rollgate-api 660" ]] || die "socket is $perms, want root:rollgate-api 660"
if vm 'runuser -u nobody -- curl -s --unix-socket /run/rollgate/api.sock http://rollgate/v1/status' >/dev/null 2>&1; then
  die "a user outside rollgate-api could use the API"
fi
log "  off by default; root:rollgate-api 0660; others are refused"

log "2. status, and an invalid config"
out=$(vm '/root/api GET /v1/status')
[[ $out == *'"ok":true'* && $out == *"Release: "* && $out == *" 200" ]] || die "status: $out"
vm 'cp /etc/rollgate/router.toml /root/before.toml 2>/dev/null || : > /root/before.toml
printf "schema_version = 1\n[management]\nallowed_zones = [\"wan\"]\n" > /root/bad.toml'
out=$(vm '/root/api POST /v1/apply "$(/root/body /root/bad.toml "10.0.2.2 50001")"')
[[ $out == *'"ok":false'* && $out == *" 422" ]] || die "an invalid config wasn't refused: $out"
vm 'cmp -s /root/before.toml /etc/rollgate/router.toml || { [ ! -s /root/before.toml ] && [ ! -e /etc/rollgate/router.toml ]; }' ||
  die "a refused config changed router.toml"
log "  status works; an invalid config is refused and router.toml is unchanged"

log "3. apply and confirm over the API"
out=$(vm '/root/api POST /v1/apply "$(/root/body /root/good.toml "10.0.2.2 50001")"')
[[ $out == *'"ok":true'* && $out == *"Applied generation"* ]] || die "apply: $out"
vm 'cmp -s /root/good.toml /etc/rollgate/router.toml' || die "router.toml isn't the applied config"
out=$(vm '/root/api POST /v1/confirm "{\"connection\": \"10.0.2.2 50001\"}"')
[[ $out == *'"ok":false'* && $out == *"connection that ran rollgate apply"* ]] ||
  die "a confirm from the applying connection wasn't refused: $out"
out=$(vm '/root/api POST /v1/confirm "{\"connection\": \"10.0.2.2 50002\"}"')
[[ $out == *'"ok":true'* && $out == *"Confirmed generation"* ]] || die "confirm from another connection: $out"
vm 'ip -br link show iot >/dev/null' || die "the applied config isn't running (no iot VLAN)"
log "  applied and running; confirm refused from the same connection, accepted from another"

log "4. health, and the journal"
out=$(vm '/root/api GET /v1/health')
[[ $out == *'"ok":true'* ]] || die "health over the API: $out"
journal=$(vm 'journalctl -u rollgate-api.service -o cat --no-pager')
[[ $journal == *"POST /v1/confirm from pid "*" uid 0"* ]] || die "the journal doesn't log requests with the asking process:
$journal"
log "  health passes; requests are logged with the asking process"
echo "api-test: PASS"
