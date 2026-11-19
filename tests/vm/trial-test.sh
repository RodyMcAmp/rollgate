#!/usr/bin/env bash
# L3 trial test (docs/testing.md, ADR 0011): `rollgate update` prepares @next and
# arms one trial boot of it.
#
#   1. Sign a releases.json with a throwaway key that only this VM trusts, and serve
#      it from the host. The release is the image's own date, so pacman -Syu has
#      little to download.
#   2. rollgate update with no date has nothing to do. rollgate update DATE (the
#      running date) updates @next in a chroot, writes the trial entry, records
#      the trial and reboots.
#   3. The next boot is the trial: / is @next, with the trial's panic=10. GRUB has
#      cleared its one-shot choice, and a second update is refused while the trial
#      is recorded.
#   4. The boot after that is @ again, without anyone choosing it.
#
# Promotion and the clean-up of a failed trial are M4 step 5; this test grows then.
#
# Usage:  sudo tests/vm/trial-test.sh build/rollgate-<date>.img
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2226)
#         ROLLGATE   a rollgate binary to test instead of the image's (for development)
# Needs internet access (the Arch Linux Archive), gpg and go.

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: trial-test.sh <image>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2226}
http_port=$((ssh_port + 1))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/trial.serial.log"
report="$log_dir/trial.report.log"
date=$(tr -d '[:space:]' < "$here/../../image/snapshot-date")

die() { echo "trial-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 ssh ssh-keygen gpg go; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done

if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420 # boot waits for the health checks, up to 300 s
else
  echo "trial-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d /tmp/rgtrial.XXXX) # short: gpg-agent's socket path is limited
qemu_pid= server_pid=
cleanup() {
  if [[ -n $qemu_pid ]]; then kill "$qemu_pid" 2>/dev/null || true; fi
  if [[ -n $server_pid ]]; then kill "$server_pid" 2>/dev/null || true; fi
  gpgconf --homedir "$work/gnupg" --kill all 2>/dev/null || true
  gpgconf --homedir "$work/keyring" --kill all 2>/dev/null || true
  umount "$work/root" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/root" "$work/www" "$log_dir"

vm() {
  ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes root@127.0.0.1 "$@"
}

# wait_for_boot <n>: waits for the n-th boot report and for SSH, then checks the report
# says the system is running with nothing failed.
wait_for_boot() {
  local n=$1 deadline=$((SECONDS + timeout_s)) line
  while (($(grep -ac '^ROLLGATE_BOOT state=' "$report" || true) < n)); do
    ((SECONDS < deadline)) || die "boot $n: no ROLLGATE_BOOT line within ${timeout_s}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "boot $n: QEMU exited (log: $serial)"
    sleep 2
  done
  line=$(grep -a '^ROLLGATE_BOOT state=' "$report" | sed -n "${n}p" | tr -d '\r')
  echo "    $line"
  [[ $line == *"state=running "* && $line == *"failed=" ]] || die "boot $n did not come up cleanly: $line"
  until vm true 2>/dev/null; do
    ((SECONDS < deadline)) || die "boot $n: SSH not reachable"
    sleep 2
  done
}

# expect <what> <pattern> <command>: runs a command in the VM and checks its output.
expect() {
  local what=$1 pattern=$2 out
  out=$(vm "$3" 2>&1) || true
  grep -qE -- "$pattern" <<<"$out" || die "$what: expected /$pattern/ in the output of '$3':
$out"
}

log "signing releases.json ($date) with a throwaway key"
gpg_test() { gpg --homedir "$work/gnupg" --batch --pinentry-mode loopback --passphrase '' "$@"; }
install -d -m 0700 "$work/gnupg"
gpg_test --quick-generate-key "rollgate trial test <test@invalid>" ed25519 sign never 2>/dev/null
fpr=$(gpg_test --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')
printf '{"releases": [{"date": "%s", "status": "current"}]}\n' "$date" > "$work/www/releases.json"
gpg_test --detach-sign --output "$work/www/releases.json.sig" "$work/www/releases.json"
gpg_test --armor --export "$fpr" > "$work/release-key.asc"

log "preparing a copy of the image that trusts the test key"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
# Import into a copy of pacman's keyring and copy it back: gpg may start helpers
# (gpg-agent, keyboxd) that would keep the mounted image busy.
cp -a "$work/root/etc/pacman.d/gnupg" "$work/keyring"
gpg --homedir "$work/keyring" --batch --quiet --import "$work/release-key.asc"
gpgconf --homedir "$work/keyring" --kill all 2>/dev/null || true
cp -a "$work/keyring/." "$work/root/etc/pacman.d/gnupg/"
find "$work/root/etc/pacman.d/gnupg" -type s -delete
echo "$fpr" >> "$work/root/usr/lib/rollgate/release-keys"
if [[ -n ${ROLLGATE:-} ]]; then
  echo "    testing $ROLLGATE instead of the image's rollgate"
  install -m 0755 "$ROLLGATE" "$work/root/usr/bin/rollgate"
fi
umount "$work/root"

go build -o "$work/serve" "$here/serve.go"
"$work/serve" -addr "127.0.0.1:$http_port" -dir "$work/www" &
server_pid=$!

: > "$serial"
: > "$report"
# QEMU's user network reaches the host's 127.0.0.1 as 10.0.2.2.
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -display none -monitor none \
  -chardev file,id=report,path="$report" \
  -device virtio-serial-pci -device virtserialport,chardev=report,name=org.rollgate.report \
  -serial file:"$serial" &
qemu_pid=$!

log "boot 1: the image"
wait_for_boot 1
expect "running from @" "^/@$" 'findmnt -no FSROOT /'

url="http://10.0.2.2:$http_port/releases.json"
log "rollgate update"
# The only release is the running one, so with no date there's nothing to do.
expect "nothing newer" "Already on the newest release, $date" "ROLLGATE_RELEASES_URL=$url rollgate update"
expect "nothing prepared" "^none$" 'test -e /var/lib/rollgate/update.json || echo none'
# Naming the running date installs it again. It reboots at the end, which ends the
# SSH session.
out=$(vm "ROLLGATE_RELEASES_URL=$url rollgate update $date" 2>&1) || true
echo "$out" | sed -n '/Preparing the update/p; /is ready/p' | sed 's/^/    /'
grep -q "The update to $date is ready" <<<"$out" || die "rollgate update didn't arm a trial:
$out"

log "boot 2: the trial boot of @next"
wait_for_boot 2
expect "running from @next" "^/@next$" 'findmnt -no FSROOT /'
expect "trial kernel options" "rootflags=subvol=@next .*panic=10" 'cat /proc/cmdline'
expect "GRUB used up the one-shot choice" "^next_entry=$" 'grub-editenv /efi/grub/grubenv list | grep next_entry || echo next_entry='
expect "trial recorded" "\"state\": \"trial\"" 'cat /var/lib/rollgate/update.json'
expect "second update refused" "hasn't finished its trial" 'rollgate update'
echo "    / is @next with panic=10; the one-shot choice is gone"

log "boot 3: any later boot is @ again"
vm systemctl reboot 2>/dev/null || true
wait_for_boot 3
expect "back on @" "^/@$" 'findmnt -no FSROOT /'
expect "normal kernel options" "rootflags=subvol=@ " 'cat /proc/cmdline'

log "PASS: rollgate update arms exactly one trial boot of @next (log: $serial)"
