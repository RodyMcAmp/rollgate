# Recovery runbook

**Print this page and keep it near the router.** When the router is broken, you probably have no internet to look this up.

You need physical access: a monitor and keyboard plugged into the router, or a serial console cable (115200 baud).

**Logging in at the console:** user `root`. A fresh install has no root password; the login screen reminds you to set one with `passwd`. Do that on day one, and keep the password with this page.

> Commands marked *(planned)* arrive in later milestones. Until M4, use the interim steps where given.

---

## 1. Internet stopped working right after I changed a setting

**Wait for the confirm timeout** (default 2 minutes). If you didn't run `rollgate confirm`, the router restores the previous settings by itself.
Rebooting the router (power off, wait 10 seconds, power on) also restores them: an unconfirmed change never survives a reboot *(planned, M3)*.

To undo an unconfirmed change at once, log in at the console and run `rollgate revert` *(planned)*.

Still broken after 5 minutes? Log in at the console and run:

```
rollgate rollback-config          (planned) restores the previous working settings
```

## 2. Internet stopped working right after an update

If the router rebooted on its own after the update, it probably already rolled back automatically (M4+). Wait 5 minutes.

Otherwise:

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
rollgate rollback                 (planned, M4)
interim (M1–M3):  snapper list                              find the snapshot's number
                  rollgate-snapshot-rollback <number>
                  reboot
```

The interim command keeps the broken system as `@broken-<date>`, so nothing is lost.
This exact sequence is tested automatically on every release (`make test-rollback`).

## 3. The router won't boot at all

1. If the GRUB menu appears, follow section 2, steps 3–5.
2. If GRUB doesn't appear, boot from the rollgate USB installer and choose **"Rescue"** *(planned)*. It mounts the router's disk so you can roll back a snapshot or copy off `/etc/rollgate`.

## 4. Start over (last resort)

1. Make sure you have a backup of `/etc/rollgate/` (`router.toml` and `secrets/`).
2. Reinstall from the USB installer.
3. Copy your `/etc/rollgate/` back, then run `rollgate apply` and `rollgate confirm` *(planned)*.

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
| `rollgate status` *(planned)* | All of the above, summarized |
