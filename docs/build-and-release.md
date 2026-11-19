# Build and release

## Principles

- **Reproducible.** Anyone, human or AI, can rebuild exactly what a release contains from this repository alone.
- **Pinned.** A release is defined by one date. Packages come from the [Arch Linux Archive](https://wiki.archlinux.org/title/Arch_Linux_Archive) as they were on that date.
- **Gated.** A date becomes a release only after the full test suite passes against it, including L4 (update and rollback).
- **Small steps.** Releases come out about weekly, so each update moves the system forward by about a week of Arch changes.

## The pinned snapshot date

`image/snapshot-date` holds a single line, such as `2026-11-16`.
That file is the single source of truth. The image build and the on-device updater both read it.

On the device, `/etc/pacman.d/mirrorlist` is generated as:

```
Server = https://archive.archlinux.org/repos/2026/11/15/$repo/os/$arch
```

Pinning the date gives you:

- Every router and every developer's VM runs exactly the same package set.
- "Only compatible configurations": the combination being installed is the combination CI tested.
- Rolling back to a release means going back to a date that was already known to be good.

> **Open item:** read the Arch Linux Archive's usage expectations before deploying beyond a handful of devices.
> If we outgrow that, CI mirrors each released date into our own repository and devices point there instead.

## Release train

1. **Weekly bump** (automated PR, opened by a bot or an agent): change `image/snapshot-date` to the latest available date.
   The PR description lists notable package changes (kernel, systemd, nftables, dnsmasq, Unbound, openssh) from the archive's package lists.
2. **CI** runs L0–L4 ([testing.md](testing.md)). L4 installs the **previous** release, updates to the new date, and verifies a forced rollback.
3. **Merge = release.** CI tags `release/2026.11.15` and publishes:
   - the disk image and installer ISO, with checksums,
   - `releases.json`, which lists released dates and their status (`current`, `previous`, `withdrawn`).
4. **Withdrawing**: if a release causes problems in the field, mark it `withdrawn` in `releases.json`.
   `rollgate update` skips withdrawn dates.

### `releases.json`

Published with a detached signature, `releases.json.sig` (ADR 0012):

```json
{"releases": [
  {"date": "2026-11-09", "status": "previous"},
  {"date": "2026-11-16", "status": "current"}
]}
```

- Each date is listed once. `status` is `current`, `previous` or `withdrawn`; no other fields are allowed.
- `rollgate update` with no date picks the newest `current` date. A date given by hand must be listed and not `withdrawn`. Dates older than the running one are refused (going back is `rollgate rollback`).
- The router's running release is the Arch Linux Archive date in `/etc/pacman.d/mirrorlist`.
- Before reading the file, `rollgate update` checks the signature with gpg against pacman's keyring, `/etc/pacman.d/gnupg`. The signature must be good, from a key that hasn't expired or been revoked, and from a key listed in `/usr/lib/rollgate/release-keys` (full fingerprints, one per line). pacman's keyring also trusts every Arch packager, so a good signature alone isn't enough. With no key listed, `rollgate update` refuses to run.

**Security fixes** that can't wait for the weekly bump get an out-of-cycle date bump through the same pipeline. The gate is never skipped.

### Publishing (`.github/workflows/release.yml`)

After CI passes on `main`, the release workflow checks whether `image/snapshot-date` is already listed in the published `releases.json`. If it isn't:

1. It builds that date's image and attaches it, compressed with zstd and with checksums, to the GitHub Release `release/YYYY.MM.DD`.
2. It adds the date to `releases.json` on the `gh-pages` branch (`release/publish.sh`): the new date becomes `current`, older ones `previous`. It signs the file and checks the signature the way a router does, then pushes. Routers see the release only after this step.

Without the `ROLLGATE_SIGNING_KEY` secret it publishes nothing and says so. It also refuses to sign with a key other than `image/release-key.asc`, because routers installed from that release wouldn't trust it.
`make release-dry-run` (part of `make check`) rehearses step 2 with a throwaway key: two releases, a withdrawal, and a changed file and another key's signature being refused.

**Withdrawing a release** is done by hand, on a checkout of `gh-pages`:

```
make rollgate-release
build/rollgate-release withdraw releases.json 2026-11-16
gpg --local-user <fingerprint> --detach-sign --output releases.json.sig releases.json
build/rollgate-release verify releases.json <fingerprint>
```

The newest release that isn't withdrawn becomes `current` again. Commit both files and push.

### Setting up the signing key (maintainer, once)

ADR 0012 lists these steps. An agent can't do them, because the private key must never pass through anything but your own machine and the repository secret.

1. Create the key on your own computer (Linux, WSL or macOS with gnupg):

   ```
   gpg --quick-generate-key "rollgate releases" ed25519 sign never
   gpg --list-secret-keys --with-colons | grep '^fpr' | head -1 | cut -d: -f10
   ```

   The second command prints the fingerprint (40 characters). Without an expiry date the key can't stop updates by expiring; replacing it later needs the rotation ADR that ADR 0012 mentions.
2. Commit the **public** key: `gpg --armor --export <fingerprint> > image/release-key.asc`, in a pull request. From then on every image trusts it.
3. Store the **private** key as the repository secret, then delete the file:

   ```
   gpg --armor --export-secret-keys <fingerprint> > key.asc
   gh secret set ROLLGATE_SIGNING_KEY < key.asc
   rm key.asc
   ```

4. Keep an offline backup of the private key, for example on a USB stick kept somewhere safe (`gpg --armor --export-secret-keys <fingerprint>`). If the key is lost, routers can only be moved to a new key by reinstalling them.
5. Turn on GitHub Pages: repository **Settings → Pages → Deploy from a branch → `gh-pages`, `/ (root)`**. The branch appears with the first release; until then, come back to this step after step 6.
6. Start the first release: **Actions → release → Run workflow**, or merge the next date bump.

## Image build

The image is built by `image/build.sh` with the standard Arch tools (`pacstrap`, `arch-chroot`, `grub-install`), following the Arch installation guide. See ADR 0009 for why it doesn't use mkosi.

```
image/
  build.sh          the build: filesystems, pacstrap, chroot configuration, disk assembly
  packages.txt      every package in the image (stable repos only)
  rootfs/           files copied verbatim into the image (GRUB defaults, initramfs hooks, the
                    sshd, nftables and networkd defaults until the first `rollgate apply`, units, scripts)
  snapshot-date     the pinned date
  authorized_keys   (optional, not committed) your SSH public key for test images
```

Steps in `build.sh`:
1. Write a build-time `pacman.conf` whose only server is the archive URL for `snapshot-date`.
2. Create the ESP (FAT32) and root (Btrfs) as separate image files. Create the subvolumes and mount them.
3. `pacstrap` the packages from `packages.txt`, then copy in `rootfs/`.
4. Generate `fstab`, the pinned mirrorlist, hostname and locale.
5. In a chroot: configure snapper, build the initramfs, install GRUB to the ESP (with an EFI binary that finds the ESP by UUID), generate `grub.cfg`, enable services.
6. Check the result (both kernels, root UUID, how GRUB finds the ESP, no world-writable files), then assemble the GPT disk image.

`make image` writes `build/rollgate-<date>.img` and a `.sha256` file. Set `BUILD_DIR` to build elsewhere.
Developer setup (WSL2 or native Arch) is in [dev-setup.md](dev-setup.md).

## Our own packages

The only non-Arch package on the router is `rollgate` itself (the Go binary, templates, systemd units and health checks), plus anything an ADR approves later.

- PKGBUILDs live in `packaging/<name>/PKGBUILD`.
- CI builds them in a clean chroot, signs them with the project key, and publishes them to the `rollgate` pacman repository.
- `pacman.conf` on the router lists `[rollgate]` after `core` and `extra`, with `SigLevel = Required`.

## Package holds

Arch does not support partial upgrades, and neither do we.
The supported way to avoid a bad package is to stay on, or return to, an earlier release date.
For kernel regressions, boot `linux-lts`.
A `hold` mechanism for leaf packages (ones nothing else depends on) may be added later through an ADR.

## Versioning

- Releases are named after their date: `2026.11.15`.
- The `router.toml` format has its own `schema_version` integer. The compiler accepts the current version and the one before it, and `rollgate migrate` *(planned)* upgrades a config file.
