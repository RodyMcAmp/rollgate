#!/usr/bin/env bash
# Builds the rollgate package (packaging/rollgate/PKGBUILD) with makepkg and prints its
# path. image/build.sh, make package and the release workflow use it.
#
# Usage:  packaging/build.sh DATE BINARY OUTDIR
#         DATE    the release date; the package version is DATE with dots (2026.11.16)
#         BINARY  the static rollgate binary (make rollgate)
#         OUTDIR  where the .pkg.tar.zst goes
# makepkg refuses to run as root, so as root it runs as the user nobody.
set -euo pipefail

date=${1:?usage: build.sh DATE BINARY OUTDIR}
binary=$(realpath "${2:?usage: build.sh DATE BINARY OUTDIR}")
outdir=$(realpath -m "${3:?usage: build.sh DATE BINARY OUTDIR}")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

die() { echo "packaging: $*" >&2; exit 1; }
[[ $date =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "bad date '$date'"
[[ -x $binary ]] || die "no rollgate binary at $binary"
command -v makepkg >/dev/null || die "missing makepkg (pacman)"
command -v fakeroot >/dev/null || die "missing fakeroot (see docs/dev-setup.md)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -r "$here/rollgate/." "$work/"
install -m 0755 "$binary" "$work/rollgate"
mkdir -p "$work/out"

run=(env ROLLGATE_PKGVER="${date//-/.}" PKGDEST="$work/out" BUILDDIR="$work/build"
  SRCDEST="$work" LOGDEST="$work" makepkg --nodeps --noconfirm --force)
# makepkg's output only matters when it fails.
if [[ $EUID -eq 0 ]]; then
  chown -R nobody: "$work"
  (cd "$work" && runuser -u nobody -- "${run[@]}") > "$work/makepkg.log" 2>&1 ||
    { cat "$work/makepkg.log" >&2; die "makepkg failed"; }
else
  (cd "$work" && "${run[@]}") > "$work/makepkg.log" 2>&1 ||
    { cat "$work/makepkg.log" >&2; die "makepkg failed"; }
fi

pkg=$(find "$work/out" -name 'rollgate-*.pkg.tar.zst' -print -quit)
[[ -n $pkg ]] || die "makepkg made no package"
mkdir -p "$outdir"
install -m 0644 "$pkg" "$outdir/"
echo "$outdir/$(basename "$pkg")"
