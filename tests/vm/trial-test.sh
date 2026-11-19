#!/usr/bin/env bash
# L3 trial test (docs/testing.md, ADR 0011): `rollgate update` boots the update once
# as a trial, and the trial either becomes the system or goes away, by itself.
#
#   1. Sign a releases.json with a throwaway key that only this VM trusts, and serve
#      it from the host. The release is the image's own date, so pacman -Syu has
#      little to download. rollgate update with no date has nothing to do;
#      rollgate update DATE (the running date) installs it again.
#   2. A trial that fails its checks: a test-only check fails on @next while a flag
#      file exists. The trial boots from @next with panic=10, GRUB has cleared its
#      one-shot choice, and a second update is refused. When the checks fail, it
#      reboots by itself; the boot of @ deletes @next and records why it failed.
#   3. A trial that passes: it becomes @ without a reboot (@ becomes @previous),
#      and the next boot is the updated system.
#   4. rollgate rollback: after a reboot the original system runs again, and the
#      updated one is @previous.
#   5. The "rollgate previous system" boot menu entry, chosen once, boots @previous
#      cleanly, and rollgate rollback there makes it @ for good.
#
# The image's health wait is 5 minutes; the test shortens it to 90 s with a drop-in.
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
qemu_pid='' server_pid=''
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

# Keepalives end a session whose VM rebooted under it, instead of waiting forever.
vm() {
  ssh -i "$work/key" -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o BatchMode=yes root@127.0.0.1 "$@"
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
# An image built with image/release-key.asc has the [rollgate] repository, published
# by the release workflow, which this test doesn't serve (the L4 test does). Without
# it, pacman -Syu needs only the archive.
sed -i "/^# rollgate's own packages/d; /^\[rollgate\]$/,/^Include = \/etc\/pacman.d\/rollgate-mirrorlist$/d" "$work/root/etc/pacman.conf"
rm -f "$work/root/etc/pacman.d/rollgate-mirrorlist"
# A check that fails on a trial boot while the flag exists (/var/lib/rollgate is
# shared by every root, so the test can set the flag from @).
cat > "$work/root/usr/lib/rollgate/health.d/99-trial-test" <<'EOF'
#!/bin/sh
if [ -e /var/lib/rollgate/fail-trial ] && [ "$(findmnt -no FSROOT /)" = /@next ]; then
  echo "failing on purpose (trial test)"
  exit 1
fi
echo "not failing"
EOF
chmod 0755 "$work/root/usr/lib/rollgate/health.d/99-trial-test"
install -d "$work/root/etc/systemd/system/rollgate-health.service.d"
printf '[Service]\nExecStart=\nExecStart=/usr/bin/rollgate health -boot -wait 90\n' \
  > "$work/root/etc/systemd/system/rollgate-health.service.d/10-trial-test.conf"
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

# wait_until <what> <seconds> <command>: waits until a command succeeds in the VM.
wait_until() {
  local what=$1 limit=$2 deadline=$((SECONDS + $2))
  until vm "$3" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || die "$what: not within ${limit}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "$what: QEMU exited (log: $serial)"
    sleep 3
  done
}

# started_cleanly: the boot finished starting with nothing failed.
started_cleanly() {
  expect "clean start" "^running$" 'systemctl is-system-running --wait'
}

uuid_cmd='btrfs subvolume show / | awk "\$1 == \"UUID:\" { print \$2; exit }"'
state() { vm 'cat /var/lib/rollgate/update.json' 2>/dev/null | sed -n 's/.*"state": "\([^"]*\)".*/\1/p'; }

# update: runs rollgate update for the image's date; it reboots at the end, which
# ends the SSH session.
update() {
  local out
  out=$(vm "ROLLGATE_RELEASES_URL=$url rollgate update $date" 2>&1) || true
  echo "$out" | sed -n '/Preparing the update/p; /is ready/p' | sed 's/^/    /'
  grep -q "The update to $date is ready" <<<"$out" || die "rollgate update didn't arm a trial:
$out"
}

log "boot 1: the image"
wait_for_boot 1
expect "running from @" "^/@$" 'findmnt -no FSROOT /'
uuid_a=$(vm "$uuid_cmd" | tr -d '\r')
[[ -n $uuid_a ]] || die "couldn't read @'s subvolume UUID"

url="http://10.0.2.2:$http_port/releases.json"
log "rollgate update with nothing newer"
expect "nothing newer" "Already on the newest release, $date" "ROLLGATE_RELEASES_URL=$url rollgate update"
expect "nothing prepared" "^none$" 'test -e /var/lib/rollgate/update.json || echo none'

log "2. a trial that fails its checks"
vm 'touch /var/lib/rollgate/fail-trial'
update
wait_until "the trial boot of @next" "$timeout_s" '[ "$(findmnt -no FSROOT /)" = /@next ]'
grep -aq "Booting \`rollgate update trial ($date)'" "$serial" || die "GRUB didn't boot the trial entry (log: $serial)"
expect "trial kernel options" "rootflags=subvol=@next .*panic=10" 'cat /proc/cmdline'
expect "GRUB used up the one-shot choice" "^next_entry=$" 'grub-editenv /efi/grub/grubenv list | grep next_entry || echo next_entry='
expect "second update refused" "hasn't finished its trial" 'rollgate update'
echo "    the trial booted from @next with panic=10; GRUB cleared its one-shot choice"
wait_until "the boot of @ after the failed trial" $((2 * timeout_s)) \
  '[ "$(findmnt -no FSROOT /)" = /@ ] && grep -q "\"state\": \"failed\"" /var/lib/rollgate/update.json'
started_cleanly
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_a" ]] || die "after the failed trial, / isn't the original @"
expect "why it failed" "99-trial-test: failing on purpose" 'cat /var/lib/rollgate/update.json'
expect "@next deleted" "^gone$" 'btrfs subvolume list / | grep -q " path @next$" || echo gone'
expect "trial entry removed" "^gone$" 'grep -q rollgate-trial /efi/grub/custom.cfg || echo gone'
echo "    it rebooted by itself into the original @, which deleted @next and recorded why"

log "3. a trial that passes"
vm 'rm /var/lib/rollgate/fail-trial'
update
wait_until "promotion on the trial boot" "$timeout_s" \
  '[ "$(findmnt -no FSROOT /)" = /@ ] && grep -q "\"state\": \"promoted\"" /var/lib/rollgate/update.json'
grep -ac "Booting \`rollgate update trial ($date)'" "$serial" | grep -qx 2 || die "the second update didn't boot as a trial (log: $serial)"
uuid_b=$(vm "$uuid_cmd" | tr -d '\r')
[[ -n $uuid_b && $uuid_b != "$uuid_a" ]] || die "the promoted system is the original @ ($uuid_b)"
expect "the original @ is @previous" "uuid $uuid_a path @previous$" 'btrfs subvolume list -u /'
expect "no @next left" "^gone$" 'btrfs subvolume list / | grep -q " path @next$" || echo gone'
expect "previous system entry" "menuentry 'rollgate previous system \($date\)' --id rollgate-previous" 'cat /efi/grub/custom.cfg'
expect "fstab points at @" "subvol=/@[[:space:]]" 'grep -E "[[:space:]]/[[:space:]]" /etc/fstab'
echo "    promoted without a reboot: the trial is @, the original is @previous"
old_boot=$(vm 'cat /proc/sys/kernel/random/boot_id' | tr -d '\r')
vm systemctl reboot 2>/dev/null || true
wait_until "the boot after promotion" "$timeout_s" "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != $old_boot ]"
started_cleanly
expect "running from @" "^/@$" 'findmnt -no FSROOT /'
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_b" ]] || die "after a reboot, / isn't the promoted system"
expect "normal kernel options" "rootflags=subvol=@ " 'cat /proc/cmdline'
[[ $(state) == promoted ]] || die "update.json changed on the boot after promotion: $(state)"
expect "status" "Last update: to $date, .*The previous system \($date\) is kept" 'rollgate status'
echo "    the next boot is the updated system"

log "4. rollgate rollback"
expect "rollback" "Reboot to start it" 'rollgate rollback'
expect "status before the reboot" "^Last rollback: .*, from $date back to $date\." 'rollgate status'
expect "status asks for a reboot" "^  Reboot to start $date\.$" 'rollgate status'
expect "no update before the reboot" "not @" "ROLLGATE_RELEASES_URL=$url rollgate update $date"
old_boot=$(vm 'cat /proc/sys/kernel/random/boot_id' | tr -d '\r')
vm systemctl reboot 2>/dev/null || true
wait_until "the boot after rollback" "$timeout_s" "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != $old_boot ]"
started_cleanly
expect "running from @" "^/@$" 'findmnt -no FSROOT /'
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_a" ]] || die "after rollback and a reboot, / isn't the original system"
expect "the updated system is @previous" "uuid $uuid_b path @previous$" 'btrfs subvolume list -u /'
expect "previous system entry" "--id rollgate-previous" 'cat /efi/grub/custom.cfg'
echo "    the original system runs again; the updated one is kept as @previous"

log "5. the previous system's boot menu entry, chosen by hand"
# What docs/runbooks/recovery.md tells someone who can't log in to do: pick
# "rollgate previous system" in GRUB's menu (here: GRUB's one-shot choice).
vm "grub-editenv /efi/grub/grubenv set next_entry=rollgate-previous"
old_boot=$(vm 'cat /proc/sys/kernel/random/boot_id' | tr -d '\r')
vm systemctl reboot 2>/dev/null || true
wait_until "the boot of the previous system's entry" "$timeout_s" "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != $old_boot ]"
started_cleanly
expect "running from @previous" "^/@previous$" 'findmnt -no FSROOT /'
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_b" ]] || die "the previous system's entry didn't boot the updated system kept as @previous"
echo "    the entry boots @previous with every unit running"
# There, rollgate rollback keeps it: the running system becomes @.
expect "rollback from @previous" "No reboot is needed" 'rollgate rollback'
expect "now @" "^/@$" 'findmnt -no FSROOT /'
old_boot=$(vm 'cat /proc/sys/kernel/random/boot_id' | tr -d '\r')
vm systemctl reboot 2>/dev/null || true
wait_until "the boot after rolling back from @previous" "$timeout_s" "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != $old_boot ]"
started_cleanly
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_b" ]] || die "after rollgate rollback from @previous, the next boot isn't that system"
expect "the other one is @previous" "uuid $uuid_a path @previous$" 'btrfs subvolume list -u /'
echo "    rollgate rollback there made it @; it's what boots next"

log "PASS: a failing trial goes away, a passing one becomes the system, and rollback goes back (log: $serial)"
