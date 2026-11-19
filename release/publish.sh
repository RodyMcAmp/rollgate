#!/usr/bin/env bash
# Adds a release to the published site (ADR 0012, docs/build-and-release.md):
#
#   1. with PACKAGE, the release's [rollgate] repository: SITE/repo/DATE/x86_64, with
#      the package and the repository database, both signed;
#   2. DATE as the current release in SITE/releases.json, signed, and checked the way
#      a router checks it.
#
# Routers look at releases.json first, so it changes last. The publish workflow
# (.github/workflows/release.yml) runs this on a checkout of the gh-pages branch;
# release/dry-run.sh and the L4 test (tests/vm/update-test.sh) run it with a
# throwaway key.
#
# Usage:  release/publish.sh SITE DATE [PACKAGE]
# Env:    RELEASE_KEY      fingerprint of the signing key, whose secret half is in
#                          gpg's keyring ($GNUPGHOME)
#         ROLLGATE_RELEASE the rollgate-release binary (default: build/rollgate-release)
set -euo pipefail

site=${1:?usage: publish.sh SITE DATE [PACKAGE]}
date=${2:?usage: publish.sh SITE DATE [PACKAGE]}
package=${3:-}
key=${RELEASE_KEY:?set RELEASE_KEY to the fingerprint of the signing key}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tool=${ROLLGATE_RELEASE:-$here/../build/rollgate-release}

die() { echo "publish: $*" >&2; exit 1; }
[[ -x $tool ]] || die "no rollgate-release at $tool (run: make rollgate-release)"
[[ -d $site ]] || die "no site directory at $site"
sign() { gpg --batch --yes --local-user "$key" --detach-sign --output "$1.sig" "$1"; }

if [[ -n $package ]]; then
  [[ -f $package ]] || die "no package at $package"
  repo=$site/repo/$date/x86_64
  [[ ! -e $repo ]] || die "$repo already exists: a release's repository never changes"
  mkdir -p "$repo"
  install -m 0644 "$package" "$repo/"
  pkg=$repo/$(basename "$package")
  sign "$pkg"
  repo-add --quiet --sign --key "$key" "$repo/rollgate.db.tar.gz" "$pkg" >/dev/null
  # GitHub Pages doesn't serve symbolic links; repo-add makes rollgate.db and
  # rollgate.files links to the .tar.gz files.
  for link in "$repo"/*; do
    if [[ -L $link ]]; then cp --remove-destination "$(readlink -f "$link")" "$link"; fi
  done
  for f in "$pkg" "$repo/rollgate.db"; do
    gpg --batch --verify "$f.sig" "$f" 2>/dev/null || die "the signature of $f doesn't check out"
  done
  echo "publish: [rollgate] repository for $date: $(basename "$pkg")"
fi

"$tool" add "$site/releases.json" "$date"
sign "$site/releases.json"
"$tool" verify "$site/releases.json" "$key"
