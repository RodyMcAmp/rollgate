# 0012. Publishing releases: GitHub Pages, one signing key, pacman's own checks

- Status: Proposed
- Date: 2026-11-18

## Context

`docs/build-and-release.md` describes the release train: a weekly date bump, CI gates it, merging publishes `releases.json` and the image, and our own `rollgate` package comes from a signed `[rollgate]` pacman repository.
It doesn't say where those files live, who signs them, or how a router checks them.
`rollgate update` (ADR 0011) needs all three: a router must not install a release or package that someone else could have tampered with.

## Decision

### Where things are published

Everything a router downloads is static files on **GitHub Pages** for this repository, `https://rodymcamp.github.io/rollgate/`:

| Path | What |
|---|---|
| `releases.json`, `releases.json.sig` | the list of released dates and their status (`current`, `previous`, `withdrawn`), and its signature |
| `repo/x86_64/rollgate.db` (+ `.sig`) | the `[rollgate]` pacman repository database |
| `repo/x86_64/*.pkg.tar.zst` (+ `.sig`) | our packages, starting with `rollgate` |

Disk images and installer ISOs are too large for Pages; they are attached to the GitHub Release for each date (`release/2026.11.16`), with checksums.
Arch packages keep coming from the Arch Linux Archive at the pinned date (ADR 0002). Nothing about that changes.

### Signing

- **One OpenPGP signing key for the project.** The maintainer generates it offline. Its private half is stored only as a GitHub Actions secret, plus an offline backup the maintainer keeps.
- **CI signs** each package, the repository database and `releases.json` with detached signatures, in the job that runs on merge to `main`. Pull requests never get the key.
- **The image ships the public key** in pacman's keyring (`pacman-key --add` and `--lsign-key` at build time).
  - pacman checks `[rollgate]` with `SigLevel = Required` (build-and-release.md).
  - `rollgate update` checks `releases.json.sig` against the same keyring with `pacman-key --verify`. That uses gnupg, which pacman already depends on, so there's no new dependency and no crypto code of our own.
- **Rotating the key**: a new key ships in a release signed by the old one, and the old one is then retired. Details are left to a later ADR, before the first rotation.

### What a router checks before updating

1. `releases.json` and its signature verify against the keyring. Otherwise nothing happens.
2. The chosen date is in it and not withdrawn.
3. pacman's own signature checks cover every package: Arch's keyring for Arch packages (the archive serves the same signed files), ours for `[rollgate]`.

## Alternatives considered

- **GitHub Releases as the pacman repository** (a fixed "repo" release whose files are replaced): works, but needs URL redirects that pacman follows less predictably, and the files are less easy to inspect. Pages serves plain static files.
- **Our own server or mirror**: more control, but something to run, pay for and secure. Revisit if the archive's usage expectations or the number of routers outgrow Pages (build-and-release.md, open item).
- **No signature on `releases.json`, trusting HTTPS**: HTTPS proves the file came from GitHub, not that we wrote it. A compromised account or a mistake in the Pages branch could point routers at any date. The signature also covers a mirror later.
- **Signing with a key in a hardware token held by the maintainer**: stronger, but CI couldn't publish without a person present. That's possible later, once the project has more than one maintainer.

## Consequences

- **The maintainer must, before the first release:** generate the key, store the private key as the repository secret `ROLLGATE_SIGNING_KEY`, keep an offline backup, and enable GitHub Pages for the `gh-pages` branch. Agents can't do these steps.
- CI gains a publish job on `main` that builds and signs packages and `releases.json`. Until the key exists, the job skips signing and publishes nothing, so an unsigned release can't happen by accident.
- The image build adds the public key to pacman's keyring and the `[rollgate]` repository to `pacman.conf`. Until then, the image keeps copying the `rollgate` binary in (ADR 0010).
- Testing doesn't need the real key: L4 serves a local repository and `releases.json`, signed with a throwaway test key that only the test image trusts.
