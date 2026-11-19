# Recovery runbook

**Print this page and keep it near the router.** When the router is broken, you probably have no internet to look this up.

You need physical access: a monitor and keyboard plugged into the router, or a serial console cable (115200 baud).

**Logging in at the console:** user `root`. A fresh install has no root password; the login screen reminds you to set one with `passwd`. Do that on day one, and keep the password with this page.

> Commands marked *(planned)* arrive in later milestones.

---

## 1. Internet stopped working right after I changed a setting

**Wait for the confirm timeout** (default 2 minutes). If you didn't run `rollgate confirm`, the router restores the previous settings by itself.
Rebooting the router (power off, wait 10 seconds, power on) also restores them: an unconfirmed change never survives a reboot.

To undo an unconfirmed change at once, log in at the console and run `rollgate revert`.

Still broken after 5 minutes? Log in at the console and run:

```
rollgate rollback-config          puts the previous working settings back
rollgate confirm                  keeps them (the console may always confirm)
```

## 2. Internet stopped working right after an update

**Wait 10 minutes.** `rollgate update` starts the update once as a trial and keeps it only if every health check passes within 5 minutes. If they don't, or the router hangs, it restarts into the old system by itself.
Then log in at the console and run `rollgate status`. Its last lines say whether the update was kept ("Last update: to DATE, ...") or failed, and why.

If the update was kept but something is still wrong, go back to the system from before it:

```
rollgate rollback                 the system from before the update becomes the system again
reboot
```

`rollgate rollback` keeps the updated system as `@previous`, so nothing is lost; running it again goes forward.

If you can't log in to the updated system at all:

1. Reboot the router (power off, wait 10 seconds, power on).
2. In the GRUB boot menu, choose **"rollgate previous system (DATE)"**. It starts the system from before the update, for this one boot.
3. Log in at the console and run `rollgate rollback`. That system becomes the system for good; no reboot is needed.

Both ways are tested automatically on every release (`make test-trial`).

If that doesn't help, or you updated packages by hand with `pacman`, use the boot menu:

1. Reboot the router (power off, wait 10 seconds, power on).
2. When the GRUB boot menu appears, use the arrow keys.
3. **Try the older kernel first:** choose the entry ending in **`linux-lts`**. If the internet works now, the new kernel was the problem. Stay on `linux-lts` and report it.
4. **If that didn't help:** reboot and choose **"Arch Linux snapshots"**. The list shows the newest snapshots first, one per line, with a date, a number and a type.
   Choose the newest line marked **`pre`**: that's the system as it was just before the update. Note its number (`@snapshots/<number>/snapshot`).
   Then choose a kernel: `vmlinuz-linux`, or `vmlinuz-linux-lts` if the new kernel was the problem.
   This menu doesn't count down; take your time.
5. The router now runs from the snapshot. Changes you make in this mode are temporary and vanish at the next reboot.
   If the internet works, make the rollback permanent:

```
snapper list                              find the snapshot's number
rollgate-snapshot-rollback <number>
reboot
```

`rollgate-snapshot-rollback` keeps the broken system as `@broken-<date>`, so nothing is lost.
This exact sequence is tested automatically on every release (`make test-rollback`).

## 3. The router won't boot at all

1. If the GRUB menu appears, follow section 2, boot-menu steps 3–5.
2. If GRUB doesn't appear, boot from the rollgate USB installer and choose **"Rescue"** *(planned)*. It mounts the router's disk so you can roll back a snapshot or copy off `/etc/rollgate`.

## 4. Start over (last resort)

1. Make sure you have a backup of `/etc/rollgate/` (`router.toml` and `secrets/`).
2. Reinstall from the USB installer.
3. Copy your `/etc/rollgate/` back, then run `rollgate apply` and `rollgate confirm`.

---

## Useful commands at the console

| Command | What it tells you |
|---|---|
| `networkctl` | Are the interfaces up? Does WAN have an address? |
| `ip route` | Is there a default route (a line starting `default via`)? |
| `systemctl --failed` | Which services have failed? |
| `journalctl -b -p err` | Errors since this boot |
| `nft list ruleset \| less` | The firewall rules that are actually loaded |
| `snapper list` | Snapshots and their numbers |
| `rollgate status` | Which settings are running, whether a change is waiting for `rollgate confirm`, the recent history, the release, what the last update did, and whether the clock is right |
| `rollgate web-password` | Sets a new web UI password, if you forgot it |
| `timedatectl` | The time, and whether it is synchronized. Updates fail while the clock is wrong |
| `rollgate health` | Whether the firewall, WAN, DNS, SSH and services are all working, one line each |
