#!/usr/bin/env bash
# Rehearses the release train's publishing with a throwaway key (make release-dry-run,
# part of make check): two releases in a row, a withdrawal, and the checks a router
# makes, including that a changed file or another key is refused. Changes nothing
# outside a temporary directory.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tool=$(realpath "${ROLLGATE_RELEASE:-$here/../build/rollgate-release}")
date=$(tr -d '[:space:]' < "$here/../image/snapshot-date")
previous=$(date -u -d "$date -7 days" +%F)

die() { echo "release-dry-run: FAIL: $*" >&2; exit 1; }
[[ -x $tool ]] || die "no rollgate-release at $tool (run: make rollgate-release)"

work=$(mktemp -d /tmp/rgrelease.XXXX) # short: gpg-agent's socket path is limited
cleanup() {
  gpgconf --homedir "$work/gnupg" --kill all 2>/dev/null || true
  gpgconf --homedir "$work/other" --kill all 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/site"

newkey() { # homedir: generates a throwaway signing key and prints its fingerprint
  install -d -m 0700 "$1"
  gpg --homedir "$1" --batch --pinentry-mode loopback --passphrase '' \
    --quick-generate-key "rollgate release dry run <test@invalid>" ed25519 sign never 2>/dev/null
  gpg --homedir "$1" --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }'
}
export GNUPGHOME=$work/gnupg ROLLGATE_RELEASE=$tool
RELEASE_KEY=$(newkey "$GNUPGHOME")
export RELEASE_KEY

"$here/publish.sh" "$work/site" "$previous" >/dev/null
"$here/publish.sh" "$work/site" "$date" > "$work/out"
grep -q "\"date\": \"$previous\", \"status\": \"previous\"" "$work/out" || die "$previous isn't previous:
$(cat "$work/out")"
grep -q "\"date\": \"$date\", \"status\": \"current\"" "$work/out" || die "$date isn't current:
$(cat "$work/out")"
echo "release-dry-run: published $previous, then $date; signed and verified"

"$here/publish.sh" "$work/site" "$previous" >/dev/null 2>&1 && die "publishing an older date was accepted"

"$tool" withdraw "$work/site/releases.json" "$date"
[[ ! -e $work/site/releases.json.sig ]] || die "withdraw left a stale signature"
gpg --batch --local-user "$RELEASE_KEY" --detach-sign --output "$work/site/releases.json.sig" "$work/site/releases.json"
"$tool" verify "$work/site/releases.json" "$RELEASE_KEY" | grep -q "\"date\": \"$previous\", \"status\": \"current\"" ||
  die "after withdrawing $date, $previous isn't current"
echo "release-dry-run: withdrew $date; $previous is current again"

cp "$work/site/releases.json" "$work/good.json"
printf ' ' >> "$work/site/releases.json"
"$tool" verify "$work/site/releases.json" "$RELEASE_KEY" >/dev/null 2>&1 && die "a changed releases.json was accepted"
cp "$work/good.json" "$work/site/releases.json"
other=$(newkey "$work/other")
gpg --homedir "$work/other" --batch --yes --detach-sign --output "$work/site/releases.json.sig" "$work/site/releases.json"
gpg --homedir "$work/other" --armor --export "$other" | gpg --batch --quiet --import
"$tool" verify "$work/site/releases.json" "$RELEASE_KEY" >/dev/null 2>&1 && die "a signature by another key was accepted"
echo "release-dry-run: a changed file and another key's signature are refused"
echo "release-dry-run: PASS"
