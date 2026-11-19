# 0014. The web UI: an unprivileged web server over a small root API

- Status: Proposed
- Date: 2026-11-19

## Context

rollgate is managed over SSH or at the console today. Its users aren't assumed to be networking experts (AGENTS.md rule 9), and most of them expect a router to have a web page.
The roadmap (M6) and ADR 0005 already fix the outline: a Go web UI (`net/http`, `html/template`, htmx) that edits `router.toml` and goes through the same apply path as the CLI, reachable only from the management zones.
This ADR decides the rest: what runs, with which rights, how people log in, how commit-confirm works in a browser, and what the first version does.
A web server is a new daemon, so rule 6 needs this ADR.

## Decision

### Two processes, one binary

- **`rollgate web`** serves the pages. It runs as an unprivileged, systemd-sandboxed user (`DynamicUser=`, no capabilities, read-only filesystem). It can't write any file or change anything itself.
- **`rollgate api`** does the work. It runs as root, is started by a systemd socket on a local Unix socket (`/run/rollgate/api.sock`) that only `rollgate web` may connect to, and accepts a fixed list of requests:
  - status, health, the current `router.toml` and a diff against it;
  - validate, apply, confirm and revert (ADR 0010);
  - update and rollback (ADR 0011);
  - list and end web sessions.
  Each request calls the same Go functions as the CLI command of the same name. There is no "run this command" request.
- So a bug in the network-facing part can show pages, but it can't write files or run programs. Both are the existing `rollgate` binary: no new language or package.

### Reachable only from the management zones

- `rollgate web` listens on the addresses of the zones in `management.allowed_zones`, the same as SSH. The firewall already allows only those zones (config-spec.md); the web UI adds port 443, plus port 80, which only redirects to 443.
- WAN can never reach it (rule 5). Remote access comes later through a VPN (WireGuard, M6), not by opening the UI.

### HTTPS with a certificate made on the router

- On first start, `rollgate api` generates a self-signed certificate and key in `/etc/rollgate/secrets/` (Go's standard library; rule 8: never in git).
- Browsers will warn once. The certificate's fingerprint is shown at the console login prompt and by `rollgate status`, so the user can check they reached their own router.

### Login: one admin password, set from the console or SSH

- `rollgate web-password` (console or SSH) sets the password. It's stored as a salted PBKDF2-SHA256 hash in `/etc/rollgate/secrets/web-password`. Go's standard library has PBKDF2, so no third-party module is needed.
- **The web UI stays off until a password is set.** There is no first-visit setup page, because whoever reached it first on the LAN would own the router.
- Sessions use a random token in a cookie (`Secure`, `HttpOnly`, `SameSite=Strict`), expire after 30 minutes idle, and end when the password changes.
- Every form that changes something carries a CSRF token. Failed logins are rate-limited.
- One account. Users and roles can come later if needed.

### Commit-confirm in a browser

ADR 0010 requires a confirm to come over a new connection, proving the change didn't cut off management. In the web UI:

1. **Apply:** the apply request's TCP connection (client address and port) is recorded, the same way `apply` records `$SSH_CONNECTION`. The response closes that connection (`Connection: close`).
2. **Countdown:** the page shows the countdown and a **Keep these settings** button. Clicking it opens a new connection, the browser's next request after the close.
3. **Confirm:** a confirm arriving on the recorded connection is refused.
4. **If the page can't reach the router any more** (the change cut it off), nothing confirms, and the old settings come back by themselves when the timer runs out.

### What the first version does

1. **Status:** health checks, interfaces and addresses, the WAN state, and the IPv6 state in plain words ("Your ISP doesn't offer IPv6; your devices use local IPv6 only"). Also DHCP leases, the release, the last update, and the clock.
2. **Edit `router.toml`:** in a text editor with validation and a diff before applying, then apply and confirm. Editing the file as text keeps the user's comments.
3. **Update and rollback:** buttons for `rollgate update` and `rollgate rollback`, with what they will do spelled out first.

Forms for common tasks (port forwards, DHCP reservations, Wi-Fi-style "add a network") come next. They need a writer that changes one part of `router.toml` without losing comments; that writer gets its own design when the forms do.

### Pages work without JavaScript

htmx improves pages (live status, no full reloads), but every form also works as a plain HTML form.
htmx is a single file, served by the router itself from the binary (`embed`), never from a CDN, because the router may have no internet when someone needs the UI most.

### Kept open for later

- **Add-on pages:** pages are registered per feature in the code, so a future add-on (see roadmap, "Later / ideas") can add its own page without changing the others.
- **Other clients of the API:** today only `rollgate web` may use the API socket. The same API, with its fixed list of requests, can later serve other local tools. One example is an automation or AI assistant that proposes firewall changes, which still go through validate → apply → confirm.

## Alternatives considered

- **One process running as root**: simpler, but then every bug in HTTP handling, sessions or templates runs as root on the device that guards the network.
- **Cockpit**: a ready-made Linux admin UI, but it manages the system directly (networking, services). That breaks "one write path" (rule 3) and doesn't know `router.toml`.
- **LuCI-style (Lua) or a JavaScript single-page app**: a new language, or a JavaScript build toolchain, for something Go templates can do. The rule 6 cost isn't worth it.
- **Log in with the root password (PAM)**: needs cgo, so no static binary (ADR 0005), and ties web access to the console password.
- **A first-visit setup page instead of a password set at the console**: friendlier, but anyone on the LAN who gets there first takes over the router.
- **Let's Encrypt certificates**: need a public name and validation from the internet. That's no fit for a LAN-only page; self-signed with a shown fingerprint is honest about what it is.

## Consequences

- Two new units, `rollgate-web.service` and `rollgate-api.socket` (with `rollgate-api.service`), shipped in the `rollgate` package. The web UI stays off until `rollgate web-password` is run.
- The firewall generator opens 443 and 80 from the management zones when the web UI is on. That's nftables generation, so the change needs a human review.
- A new L3 test drives the UI with HTTP requests in a VM:
  - log in;
  - apply a change and confirm it from a new connection;
  - check that a confirm from the apply's connection is refused;
  - check that a change that cuts off the UI reverts by itself;
  - check that WAN can't reach it.
- Go unit tests cover the API's request list, sessions, CSRF and password hashing with `httptest`.
- `rollgate status` and the console login prompt show the certificate fingerprint and whether the web UI is on.
