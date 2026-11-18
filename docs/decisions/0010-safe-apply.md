# 0010. Safe apply: generations, commit-confirm, revert at boot

- Status: Proposed
- Date: 2026-11-18

## Context

`rollgate compile` produces a correct set of files (M2), but nothing installs them yet.
Installing them changes a live router's firewall, addresses and SSH, which is exactly how people lock themselves out (hard rule 2).
`docs/architecture.md#safe-apply` sketches commit-confirm. Before writing it, these questions need one answer each:
where installed generations live, what the revert timer is, what happens if the router reboots or the SSH session drops during the confirm window, what "confirm over a new connection" means in practice, and which services to reload.

The design must also keep the live install media idea possible (roadmap, "Later"): apply must work on a fresh system with no stored state.

## Decision

### Generations

- Every apply stores a new generation in `/var/lib/rollgate/generations/<N>/` (N counts up from 1):
  `router.toml` (the source), `files/` (the compiled tree, the same layout `rollgate compile -o` writes) and `meta.json` (time, state, the previous generation).
- A generation's state is `pending`, `confirmed` or `reverted`. `/var/lib/rollgate/current` names the last confirmed generation; `/var/lib/rollgate/pending` exists only during a confirm window.
- **Generation 0** is captured on the first apply: a copy of whatever is installed at the paths rollgate manages (the image's placeholder files). So even the very first apply has something to revert to.
- The 30 newest generations are kept. Generation 0 and `current` are never pruned.
- `/var/lib/rollgate` is its own Btrfs subvolume (ADR 0004), so the generation history survives a system rollback.
- **No git repository in `/etc/rollgate`** (the architecture sketch planned one). Each generation already holds the `router.toml` that produced it, so git would duplicate the history and add a package for no new capability.

### Apply

`rollgate apply [file]` (default `/etc/rollgate/router.toml`):

1. Refuse if another change is pending ("confirm it, or run `rollgate revert`"). One apply at a time, guarded by a lock in `/run/rollgate`.
2. Validate and compile. Then check the compiled files with each program's own checker before anything is installed: `nft -c -f`, `dnsmasq --test`, `unbound-checkconf`, `sshd -t`. Also check that every secret file `router.toml` names exists and is non-empty, so SSH never switches to a missing key file. Any failure stops the apply with nothing changed.
3. Store generation N as `pending`, then start the revert timer (below), **before** touching any system file.
4. Install the files: each one written to a temporary name and renamed into place. rollgate-owned files that the new config no longer produces are removed (the same `*-rollgate-*` patterns `rollgate diff` reports).
5. Reload only what changed:

   | Changed files | Action |
   |---|---|
   | `/etc/systemd/network/*.link` | `udevadm trigger --action=add` on network devices, so renames take effect |
   | `/etc/systemd/network/*` | `networkctl reload` |
   | `/etc/sysctl.d/90-rollgate.conf` | `sysctl -p` on that file |
   | `/etc/nftables.conf` | `nft -f` (atomic: the whole ruleset is replaced in one transaction) |
   | `/etc/dnsmasq.conf`, `/etc/unbound/unbound.conf` | restart that service (enable it if needed) |
   | `/etc/ssh/sshd_config.d/10-rollgate.conf` | reload sshd (open sessions stay up) |

6. Print what changed and how to confirm, then exit. `apply` doesn't wait, so a future web UI can call it the same way.

### Revert timer

- The timer is a transient systemd timer started with `systemd-run --on-active=<confirm_timeout_seconds>`, which runs `rollgate revert` when it fires.
  It belongs to PID 1, not to the SSH session, so it fires even if the connection that ran `apply` has died, which is the common case for a lockout.
- **A reboot during the confirm window also reverts.** `rollgate-boot.service` runs before networkd and nftables start. If a pending generation exists, it restores the previous generation's files first. An unconfirmed change never survives a reboot, whether the reboot was deliberate, a crash or a power cut.
- `rollgate revert` (by the timer, or by hand to undo at once) installs the previous generation's files with the same install-and-reload steps and marks the pending one `reverted`.
  It does not touch `/etc/rollgate/router.toml`: the user's edit stays there so they can fix it, and `rollgate status` says it differs from what is running.

### Confirm

- `rollgate confirm` stops the timer, marks the generation `confirmed` and makes it `current`.
- **It must come over a new connection.** `apply` records `$SSH_CONNECTION` (client address and port, which identify one TCP connection). `confirm` refuses when run from that same connection, with a message saying to open a new SSH session.
  A new connection proves the change didn't cut off management. On the local console there is no `$SSH_CONNECTION`, and confirming is allowed.

### Other commands

- `rollgate status`: the current generation, any pending one with the seconds left, and whether `router.toml` differs from what is running.
- `rollgate rollback-config [N]`: puts generation N's `router.toml` back in `/etc/rollgate/router.toml` (default: the confirmed generation before `current`) and applies it through the same commit-confirm path. It is a change like any other.

### On the image

- Until a `router.toml` exists, the image keeps its placeholder networking (DHCP on every port, renamed to match `*-rollgate-*` so the first apply removes it).
- The image gains the `rollgate` binary, `rollgate-boot.service`, and the dnsmasq and unbound packages (already chosen by ADR 0006).
  Until our signed package repository exists (M4, ADR 0005), `image/build.sh` builds the static binary and copies it in, like the interim rollback script.
- Writing a first `router.toml` automatically (detecting WAN and LAN ports) is part of the live install media work, not M3. In M3, the user or the test writes it.

## Alternatives considered

- **A long-running rollgate daemon that holds the timer**: one more process to keep alive, and it would need its own crash handling. systemd already provides timers that survive the session. Hard rule 6 also discourages new daemons.
- **A `sleep 120 && revert` child process**: dies with the SSH session when the session's processes are killed, which is precisely the lockout case.
- **Btrfs snapshot per apply**: heavier than storing a few kilobytes of text, and it would roll back unrelated state such as packages. Generations stay small and only cover what rollgate writes.
- **`rollgate apply` waits for confirmation in the foreground (like Junos `commit confirmed`)**: easier to read, but a web UI can't hold a request open for two minutes, and it still needs a timer for the dropped-connection case. The non-blocking form serves both.
- **Accept `confirm` from the same connection**: it proves nothing, because an existing connection can survive a change that blocks new ones (conntrack keeps established flows).
- **git history in `/etc/rollgate`**: see above; the generations already are the history.

## Consequences

- No unconfirmed change outlives its timer, the SSH session that started it, or a reboot. L3 must prove all three: a config that cuts off SSH, an unconfirmed reboot, and a refused same-connection confirm.
- The apply code is safety-critical: changes to it need human review (AGENTS.md).
- Generation 0 makes the very first apply as safe as later ones, which the live install media needs.
- Router advertisement and prefix delegation can finally be tested end to end (L3), because applied configs run under real networkd.
- `docs/architecture.md`, the recovery runbook and the roadmap describe this design; `rollgate apply`, `confirm`, `revert`, `status` and `rollback-config` are *(planned)* until their M3 PRs land.
