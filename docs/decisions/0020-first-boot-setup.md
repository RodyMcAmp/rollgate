# 0020. First-boot setup: the config applies by itself, one password for the console and the web UI

- Status: Accepted (by the maintainer, 2026-11-20)
- Date: 2026-11-20

## Context

Today a freshly installed router needs six manual steps before it is useful: log in at the console as root with no password, `passwd`, `rollgate apply`, `rollgate confirm`, then SSH in and `rollgate web-password`.
The maintainer tried this with the VMware VM (2026-11-20) and found it "way too complicated".
It also leaves root without a password on the console until someone sets one, and the web UI off until someone sets a second password.

Rule 9 (keep it simple, safe defaults) says the router should come up working with the installed `router.toml`, and ask the person at the console only for what it can't choose itself: a password.

ADR 0010 says every apply waits for a confirm from a new connection and reverts by itself otherwise.
On first boot that protection has nothing to protect: no config has ever been applied, so no management access can be lost by applying it.
What can go wrong is the config itself (a typo'd interface, a service that won't start), and the health checks already catch that.

## Decision

### The first apply happens by itself

- A one-shot unit, `rollgate-firstboot.service`, runs at boot when **no generation exists yet** (`/var/lib/rollgate/generations/` is empty).
  It's derived from state, not a marker file, so routers that were set up before this (and get it through an update) never run it.
- It runs `rollgate apply` with the installed `/etc/rollgate/router.toml`, then the health checks (`rollgate health`, ADR 0011's checks).
  If they pass, it confirms. If apply or a check fails, it reverts, as an unconfirmed apply would, and the console shows what failed and how to fix it: edit `/etc/rollgate/router.toml`, then `rollgate apply`.
- This needs nobody at the console, so a router without a screen still comes up with its network.
- This is the one exception to ADR 0010's "confirm from a new connection": the first apply is confirmed by passing the health checks.
  Every later apply works as before.

### One password, asked for once

- While root has no password, the console (tty1, and the serial console when the kernel uses one) shows a short setup screen instead of the login prompt:
  "Choose a password for this router (at least 10 characters)", typed twice.
- That one password becomes **both** root's console password and the web UI's password (it is stored twice, in each one's own hashed form: `/etc/shadow` and `/etc/rollgate/secrets/web-password`).
  Setting it also makes the web UI's HTTPS certificate, as `rollgate web-password` does today, and turns the web UI on.
- The screen then shows where to go next, in plain words: the web UI's address (`https://<the LAN address>`), the certificate's fingerprint, and the result of the first apply. Then the normal login prompt.
- SSH stays key-only, as today: the password never works over SSH.
- A new command, `rollgate password`, changes both later (it asks twice, or reads `--stdin`). The web UI's password change, when it has one, goes through the API and changes both too.
  `rollgate web-password` stays, unchanged, for scripts and tests that set only the web UI's password.

### Tests and VM images

- The existing L3 tests apply their configs themselves and count generations, so their copies of the image turn the first-boot unit off (masked). A new `make test-firstboot` checks it, and `make test-vmware` stops applying by hand:
  on first boot the config is applied and kept without anyone typing; a config that fails a health check is reverted and the console says why; the setup screen sets a password that works at the console and on the web UI; SSH refuses that password; a router that already has a generation doesn't run any of it after an update.
- `docs/vmware.md` and the install docs drop the manual `passwd`, `apply` and `confirm` steps.

## Consequences

- A new install comes up working by itself; the person at the console types one password, twice, and is told where to open the web UI.
- Root no longer sits with an empty password on the console until someone remembers to set one.
- One password protects both the console and the web UI. That's simpler for people who aren't networking experts, and anyone with the web password could already change the router's whole config through it. The cost: a leaked web password is also the console password, which needs physical access to use.
- ADR 0010's commit-confirm gets one exception, the first apply, kept only when the health checks pass. This is safety-critical (apply, management access) and needs the maintainer's review.
- New pieces: one unit (not a daemon; it exits), the setup screen in the `rollgate` binary, `rollgate password`, an API call for the web UI to change root's password (it runs unprivileged), and an L3 test.
