#!/usr/bin/env bash
# Adds a release to the published site (ADR 0012, docs/build-and-release.md): makes DATE
# the current release in SITE/releases.json, signs it, and checks the signature the
# way a router does. The publish workflow (.github/workflows/release.yml) runs it on a
# checkout of the gh-pages branch; release/dry-run.sh runs it with a throwaway key.
#
# Usage:  release/publish.sh SITE DATE
# Env:    RELEASE_KEY      fingerprint of the signing key, whose secret half is in
#                          gpg's keyring ($GNUPGHOME)
#         ROLLGATE_RELEASE the rollgate-release binary (default: build/rollgate-release)
set -euo pipefail

site=${1:?usage: publish.sh SITE DATE}
date=${2:?usage: publish.sh SITE DATE}
key=${RELEASE_KEY:?set RELEASE_KEY to the fingerprint of the signing key}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tool=${ROLLGATE_RELEASE:-$here/../build/rollgate-release}

die() { echo "publish: $*" >&2; exit 1; }
[[ -x $tool ]] || die "no rollgate-release at $tool (run: make rollgate-release)"
[[ -d $site ]] || die "no site directory at $site"

"$tool" add "$site/releases.json" "$date"
gpg --batch --yes --local-user "$key" --detach-sign --output "$site/releases.json.sig" "$site/releases.json"
"$tool" verify "$site/releases.json" "$key"
