#!/usr/bin/env bash
# The weekly date bump (docs/build-and-release.md, "Release train", step 1): finds the
# newest Arch Linux Archive date, writes it to image/snapshot-date, and writes a pull
# request description listing what changed in the image's packages.
# .github/workflows/bump.yml runs it every week and opens the pull request.
#
# Usage:  release/bump.sh [DATE] [BODY_FILE]
#         DATE defaults to yesterday (UTC): the archive's newest complete day.
#         BODY_FILE (default: build/bump-body.md) gets the pull request description.
# Exit:   0 bumped; 3 nothing newer than image/snapshot-date; 1 error.
set -euo pipefail
export LC_ALL=C # sort and join must agree on order

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/.." && pwd)
new=${1:-$(date -u -d yesterday +%F)}
body=${2:-$root/build/bump-body.md}
old=$(tr -d '[:space:]' < "$root/image/snapshot-date")
archive=https://archive.archlinux.org/repos

die() { echo "bump: $*" >&2; exit 1; }
[[ $new =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "bad date '$new'"
if [[ ! $new > $old ]]; then
  echo "bump: $old is already the newest date (asked for $new)"
  exit 3
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# versions DATE: "name version" for every package in core and extra on DATE.
versions() {
  local repo dir
  for repo in core extra; do
    dir="$work/$1/$repo"
    mkdir -p "$dir"
    # The archive sometimes drops connections; download whole, with retries.
    if ! curl -fsSL --retry 5 --retry-all-errors --retry-delay 10 -o "$dir.db" \
      "$archive/${1//-//}/$repo/os/x86_64/$repo.db" || ! bsdtar -xf "$dir.db" -C "$dir"; then
      die "can't read $repo.db for $1 from the archive"
    fi
  done
  for desc in "$work/$1"/*/*/desc; do
    awk '/^%NAME%$/ { getline; n = $0 } /^%VERSION%$/ { getline; v = $0 } END { print n, v }' "$desc"
  done | sort
}
versions "$old" > "$work/old"
versions "$new" > "$work/new"

# The image's own packages (image/packages.txt), with the ones people ask about first.
notable="linux linux-lts systemd glibc nftables dnsmasq unbound openssh"
listed=$(grep -v '^\s*#' "$root/image/packages.txt" | awk 'NF { print $1 }')
changed=$(join "$work/old" "$work/new" | awk '$2 != $3')
{
  echo "Weekly date bump: \`image/snapshot-date\` $old → $new (docs/build-and-release.md)."
  echo
  echo "CI runs L0–L4 against the new date; merging makes it a release."
  echo
  echo "| Package | $old | $new |"
  echo "|---|---|---|"
  n=0
  for p in $notable $listed; do
    line=$(awk -v p="$p" '$1 == p' <<<"$changed")
    [[ -n $line ]] || continue
    grep -qx "$p" <<<"${shown:-}" && continue
    shown+=$'\n'"$p"
    read -r _ a b <<<"$line"
    echo "| $p | $a | $b |"
    n=$((n + 1))
  done
  ((n > 0)) || echo "| (none of the image's listed packages changed) | | |"
  echo
  echo "$(grep -c . <<<"$changed" || true) packages changed in core and extra in all; the image installs those it needs."
} > "$body"

echo "$new" > "$root/image/snapshot-date"
echo "bump: $old → $new; description in $body"
