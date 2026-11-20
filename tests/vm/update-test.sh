#!/usr/bin/env bash
# L4 update test (docs/testing.md, ADR 0011): install the previous release, apply a
# config, and update to the candidate release, as a router in the field would.
#
#   1. Boot the previous release's image and apply tests/vm/apply/good.toml (renamed
#      ports, a VLAN, firewall, dnsmasq, Unbound). The host serves what the release
#      train publishes, made by release/publish.sh with a throwaway key that only this
#      VM trusts: releases.json listing the previous release and the candidate, and
#      each release's signed [rollgate] package repository, which the VM uses.
#   2. A trial whose health check fails rolls back by itself; the config still runs.
#   3. A trial whose kernel panics rolls back by itself (panic=10, or the watchdog).
#   4. rollgate update to the candidate: pacman -Syu in @next downloads the week's
#      updates (both kernels among them) and rebuilds both initramfs images. The trial
#      passes and becomes the system. After a reboot the candidate runs with its new
#      kernel, the candidate's rollgate package from the [rollgate] repository, and the
#      same config, and its linux-lts entry boots too.
#
# Steps 2 and 3 install the previous release again (a date listed in releases.json),
# which downloads almost nothing; only step 4 downloads the update. The checks for
# steps 2 and 3 are test-only: a health check that fails, and a unit that crashes the
# kernel, both only on a trial boot while a flag file exists.
#
# Usage:  sudo tests/vm/update-test.sh build/rollgate-<previous>.img <candidate-date>
# Env:    LOG_DIR, OVMF_CODE, OVMF_VARS as in boot-test.sh; SSH_PORT (default 2228)
#         ROLLGATE   a rollgate binary to test instead of the image's (for development); the
#                    published packages carry it too (default: build/rollgate)
#         ROLLGATE_RELEASE  the release tool (default: build/rollgate-release)
# Needs internet access (the Arch Linux Archive), gpg, go, makepkg and fakeroot.

# Commands run inside the VM are single-quoted on purpose, so they expand there.
# shellcheck disable=SC2016
set -euo pipefail

image=${1:?usage: update-test.sh <previous-image> <candidate-date>}
candidate=${2:?usage: update-test.sh <previous-image> <candidate-date>}
image=$(realpath "$image")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# The rollgate the published packages carry, and the release tool (make test-update
# builds both).
binary=$(realpath -m "${ROLLGATE:-$here/../../build/rollgate}")
release_tool=$(realpath -m "${ROLLGATE_RELEASE:-$here/../../build/rollgate-release}")
log_dir=$(realpath -m "${LOG_DIR:-$(dirname "$image")/test-logs}")
ovmf_code=${OVMF_CODE:-/usr/share/edk2/x64/OVMF_CODE.4m.fd}
ovmf_vars=${OVMF_VARS:-/usr/share/edk2/x64/OVMF_VARS.4m.fd}
ssh_port=${SSH_PORT:-2228}
http_port=$((ssh_port + 1))
# Must match root_start_sector in image/build.sh (ESP at 2048, 512 MiB long).
root_offset=$(((2048 + 512 * 2048) * 512))
serial="$log_dir/update.serial.log"
report="$log_dir/update.report.log"
# The MACs in tests/vm/apply/good.toml.
lan_mac=52:54:00:00:00:01
wan_mac=52:54:00:00:00:02
# The previous release's date is in the image's name (build/rollgate-<date>.img).
previous=$(basename "$image" .img)
previous=${previous#rollgate-}

die() { echo "update-test: FAIL: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -f $image ]] || die "no image at $image (run: make image-previous)"
[[ $previous =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "can't read the release date from the image name $image"
[[ $candidate > $previous ]] || die "the candidate ($candidate) must be newer than the previous release ($previous)"
for f in "$ovmf_code" "$ovmf_vars"; do [[ -f $f ]] || die "missing UEFI firmware $f (install edk2-ovmf)"; done
for tool in qemu-system-x86_64 ssh ssh-keygen gpg go makepkg fakeroot repo-add; do
  command -v "$tool" >/dev/null || die "missing '$tool' (see docs/dev-setup.md)"
done
[[ -x $binary ]] || die "no rollgate at $binary (run: make rollgate)"
[[ -x $release_tool ]] || die "no rollgate-release at $release_tool (run: make rollgate-release)"

if [[ -w /dev/kvm ]]; then
  accel=(-accel kvm -cpu host); timeout_s=420 # boot waits for the health checks, up to 300 s
else
  echo "update-test: /dev/kvm not available, using slow software emulation" >&2
  accel=(-accel tcg); timeout_s=1200
fi

work=$(mktemp -d /tmp/rgupdate.XXXX) # short: gpg-agent's socket path is limited
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

# expect <what> <pattern> <command>: runs a command in the VM and checks its output.
expect() {
  local what=$1 pattern=$2 out
  out=$(vm "$3" 2>&1) || true
  grep -qE -- "$pattern" <<<"$out" || die "$what: expected /$pattern/ in the output of '$3':
$out"
}

# wait_until <what> <seconds> <command>: waits until a command succeeds in the VM.
wait_until() {
  local what=$1 limit=$2 deadline=$((SECONDS + $2))
  until vm "$3" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || die "$what: not within ${limit}s (log: $serial)"
    kill -0 "$qemu_pid" 2>/dev/null || die "$what: QEMU exited (log: $serial)"
    sleep 3
  done
}

boot_id() { vm 'cat /proc/sys/kernel/random/boot_id' | tr -d '\r'; }

# reboot_and_wait <what>: reboots the VM and waits for the next boot to start cleanly.
reboot_and_wait() {
  local old
  old=$(boot_id)
  vm systemctl reboot 2>/dev/null || true
  wait_until "$1" "$timeout_s" "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != $old ]"
  started_cleanly
}

started_cleanly() {
  expect "clean start" "^running$" 'systemctl is-system-running --wait'
}

# The config from tests/vm/apply/good.toml runs, and every health check passes.
check_config() {
  expect "lan address" "10\.0\.2\.15/24" 'ip -br addr show lan'
  expect "wan renamed" "^wan " 'ip -br link show wan'
  expect "iot VLAN" "192\.168\.20\.1/24" 'ip -br addr show iot'
  expect "services" "^active
active$" 'systemctl is-active dnsmasq unbound'
  expect "health" "^ok +50-services" 'rollgate health -wait 60'
}

release() { vm 'rollgate status' 2>/dev/null | sed -n 's/^Release: \(.*\)\.$/\1/p' | tr -d '\r'; }

# update <date>: runs rollgate update; it reboots at the end, ending the SSH session.
update() {
  local out
  out=$(vm "ROLLGATE_RELEASES_URL=$url ROLLGATE_REPO_URL=$repo_url rollgate update $1" 2>&1) || true
  echo "$out" | sed -n '/Preparing the update/p; /is ready/p' | sed 's/^/    /'
  grep -q "is ready" <<<"$out" || die "rollgate update $1 didn't arm a trial:
$out"
}

# failed_trial <what>: waits for the boot of @ that cleans up after a failed trial.
failed_trial() {
  wait_until "$1: the boot of @ after the failed trial" $((2 * timeout_s)) \
    '[ "$(findmnt -no FSROOT /)" = /@ ] && grep -q "\"state\": \"failed\"" /var/lib/rollgate/update.json'
  started_cleanly
  expect "$1: @next deleted" "^gone$" 'btrfs subvolume list / | grep -q " path @next$" || echo gone'
  [[ $(release) == "$previous" ]] || die "$1: the release changed to $(release)"
  check_config
}

log "publishing $previous and $candidate with a throwaway key (release/publish.sh)"
gpg_test() { gpg --homedir "$work/gnupg" --batch --pinentry-mode loopback --passphrase '' "$@"; }
install -d -m 0700 "$work/gnupg"
gpg_test --quick-generate-key "rollgate update test <test@invalid>" ed25519 sign never 2>/dev/null
fpr=$(gpg_test --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')
gpg_test --armor --export "$fpr" > "$work/release-key.asc"
for d in "$previous" "$candidate"; do
  pkg=$(bash "$here/../../packaging/build.sh" "$d" "$binary" "$work/pkg")
  GNUPGHOME=$work/gnupg RELEASE_KEY=$fpr ROLLGATE_RELEASE=$release_tool \
    bash "$here/../../release/publish.sh" "$work/www" "$d" "$pkg" >/dev/null
done

log "preparing a copy of the $previous image"
cp --sparse=always "$image" "$work/disk.img"
cp "$ovmf_vars" "$work/vars.fd"
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
mount -o loop,offset="$root_offset",subvol=/@ "$work/disk.img" "$work/root"
install -d -m 0700 "$work/root/root/.ssh"
install -m 0600 "$work/key.pub" "$work/root/root/.ssh/authorized_keys"
install -m 0600 "$work/key.pub" "$work/root/etc/rollgate/secrets/admin.pub"
install -m 0644 "$here/apply/good.toml" "$work/root/etc/rollgate/router.toml"
# This test applies its config itself and counts generations: no first-boot apply
# (ADR 0020), in case the previous release has it (CI builds it from this tree).
ln -sf /dev/null "$work/root/etc/systemd/system/rollgate-firstboot.service"
# Import into a copy of pacman's keyring and copy it back: gpg may start helpers
# (gpg-agent, keyboxd) that would keep the mounted image busy.
cp -a "$work/root/etc/pacman.d/gnupg" "$work/keyring"
gpg --homedir "$work/keyring" --batch --quiet --import "$work/release-key.asc"
# Signed locally, as image/build.sh does with the real key, so pacman trusts it for
# the [rollgate] repository.
pacman-key --gpgdir "$work/keyring" --lsign-key "$fpr" >/dev/null 2>&1
gpgconf --homedir "$work/keyring" --kill all 2>/dev/null || true
cp -a "$work/keyring/." "$work/root/etc/pacman.d/gnupg/"
find "$work/root/etc/pacman.d/gnupg" -type s -delete
echo "$fpr" >> "$work/root/usr/lib/rollgate/release-keys"
# The [rollgate] repository as image/build.sh writes it with a release key, but served
# by the host (rollgate update moves it with ROLLGATE_REPO_URL). An image built with
# image/release-key.asc already has the section.
grep -qx '\[rollgate\]' "$work/root/etc/pacman.conf" || cat >> "$work/root/etc/pacman.conf" <<'EOF'

[rollgate]
SigLevel = Required
Include = /etc/pacman.d/rollgate-mirrorlist
EOF
echo "Server = http://10.0.2.2:$http_port/repo/$previous/\$arch" > "$work/root/etc/pacman.d/rollgate-mirrorlist"
# Test-only failures, on a trial boot only (/var/lib/rollgate is shared by every root,
# so the test sets the flags from @): a health check that fails, and a unit that
# crashes the kernel.
cat > "$work/root/usr/lib/rollgate/health.d/99-update-test" <<'EOF'
#!/bin/sh
if [ -e /var/lib/rollgate/fail-trial ] && [ "$(findmnt -no FSROOT /)" = /@next ]; then
  echo "failing on purpose (update test)"
  exit 1
fi
echo "not failing"
EOF
chmod 0755 "$work/root/usr/lib/rollgate/health.d/99-update-test"
cat > "$work/root/etc/systemd/system/update-test-panic.service" <<'EOF'
[Unit]
Description=update test: crash the kernel on a trial boot while the flag exists
RequiresMountsFor=/var/lib/rollgate
ConditionPathExists=/var/lib/rollgate/panic-trial
After=sshd.service

[Service]
Type=oneshot
ExecStart=/bin/sh -c '[ "$(findmnt -no FSROOT /)" != /@next ] || echo c > /proc/sysrq-trigger'

[Install]
WantedBy=multi-user.target
EOF
ln -s ../update-test-panic.service "$work/root/etc/systemd/system/multi-user.target.wants/update-test-panic.service"
install -d "$work/root/etc/systemd/system/rollgate-health.service.d"
printf '[Service]\nExecStart=\nExecStart=/usr/bin/rollgate health -boot -wait 90\n' \
  > "$work/root/etc/systemd/system/rollgate-health.service.d/10-update-test.conf"
if [[ -n ${ROLLGATE:-} ]]; then
  echo "    testing $ROLLGATE instead of the image's rollgate"
  install -m 0755 "$ROLLGATE" "$work/root/usr/bin/rollgate"
fi
umount "$work/root"

go build -o "$work/serve" "$here/serve.go"
"$work/serve" -addr "127.0.0.1:$http_port" -dir "$work/www" &
server_pid=$!
url="http://10.0.2.2:$http_port/releases.json"
repo_url="http://10.0.2.2:$http_port"

: > "$serial"
: > "$report"
# lan: QEMU's user network, where the host's 127.0.0.1 is 10.0.2.2. wan: a second one,
# with QEMU's DHCP server and the way to the internet.
qemu-system-x86_64 -machine q35 -m 1024 -smp 2 "${accel[@]}" \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf_code" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/disk.img",format=raw,if=virtio \
  -nic user,model=virtio-net-pci,mac=$lan_mac,hostfwd=tcp:127.0.0.1:"$ssh_port"-:22 \
  -nic user,model=virtio-net-pci,mac=$wan_mac,net=10.0.3.0/24 \
  -display none -monitor none \
  -chardev file,id=report,path="$report" \
  -device virtio-serial-pci -device virtserialport,chardev=report,name=org.rollgate.report \
  -serial file:"$serial" &
qemu_pid=$!

log "1. the previous release ($previous), with a config applied"
wait_until "the first boot" "$timeout_s" true
started_cleanly
[[ $(release) == "$previous" ]] || die "the image runs release $(release), not $previous"
out=$(vm 'rollgate apply' 2>&1) || die "rollgate apply failed:
$out"
expect "confirm" "Confirmed generation 1" 'rollgate confirm'
check_config
uuid_cmd='btrfs subvolume show / | awk "\$1 == \"UUID:\" { print \$2; exit }"'
uuid_a=$(vm "$uuid_cmd" | tr -d '\r')
kernel_a=$(vm 'uname -r' | tr -d '\r')
packages_a=$(vm 'pacman -Q | md5sum' | tr -d '\r')
expect "the image's rollgate package" "^rollgate ${previous//-/.}-1$" 'pacman -Q rollgate'
echo "    release $previous (kernel $kernel_a, rollgate ${previous//-/.}-1), config applied and confirmed, health checks pass"

log "2. a trial that fails a health check rolls back by itself"
vm 'touch /var/lib/rollgate/fail-trial'
update "$previous"
failed_trial "failing check"
expect "why it failed" "99-update-test: failing on purpose" 'cat /var/lib/rollgate/update.json'
vm 'rm /var/lib/rollgate/fail-trial'
echo "    back on $previous with the config running; update.json says which check failed"

log "3. a trial whose kernel panics rolls back by itself"
vm 'touch /var/lib/rollgate/panic-trial'
update "$previous"
failed_trial "kernel panic"
# Console output from systemd can split the kernel's lines, so match on two short ones.
grep -aqE "sysrq triggered crash|Rebooting in 10 seconds" "$serial" || die "the trial didn't panic (log: $serial)"
expect "why it failed" "never finished its checks" 'cat /var/lib/rollgate/update.json'
vm 'rm /var/lib/rollgate/panic-trial'
[[ $(vm "$uuid_cmd" | tr -d '\r') == "$uuid_a" ]] || die "after two failed trials, / isn't the original system"
echo "    back on $previous with the config running; update.json says the trial never finished"

log "4. rollgate update to $candidate"
expect "status before" "^Release: $previous\.$" 'rollgate status'
update ""
wait_until "promotion on the trial boot" "$timeout_s" \
  '[ "$(findmnt -no FSROOT /)" = /@ ] && grep -q "\"state\": \"promoted\"" /var/lib/rollgate/update.json'
started_cleanly
echo "    the trial passed its checks and became the system"
reboot_and_wait "the first boot of $candidate"
[[ $(release) == "$candidate" ]] || die "after the update, the release is $(release), not $candidate"
expect "the new default kernel" "^ok$" '[ "$(uname -r | tr - .)" = "$(pacman -Q linux | cut -d" " -f2 | tr - .)" ] && echo ok'
[[ $(vm 'pacman -Q | md5sum' | tr -d '\r') != "$packages_a" ]] || die "no package changed between $previous and $candidate"
expect "the original system is @previous" "uuid $uuid_a path @previous$" 'btrfs subvolume list -u /'
expect "status after" "Last update: to $candidate, .*The previous system \($previous\) is kept" 'rollgate status'
check_config
expect "rollgate from the [rollgate] repository" "^rollgate ${candidate//-/.}-1$" 'pacman -Q rollgate'
echo "    $candidate runs with its new kernel ($(vm 'uname -r' | tr -d '')), rollgate ${candidate//-/.}-1 and the same config"
vm "grub-editenv /efi/grub/grubenv set 'next_entry=rollgate Linux, with Linux linux-lts'"
reboot_and_wait "the linux-lts boot of $candidate"
expect "the new linux-lts" "^ok$" '[ "$(uname -r | sed "s/-lts$//" | tr - .)" = "$(pacman -Q linux-lts | cut -d" " -f2 | tr - .)" ] && echo ok'
check_config
echo "    its linux-lts entry boots too ($(vm 'uname -r' | tr -d '\r'))"

log "PASS: $previous updated to $candidate; failed and panicking trials rolled back by themselves (log: $serial)"
