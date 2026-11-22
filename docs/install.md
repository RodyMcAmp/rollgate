# Installing rollgate on a computer

The USB installer (ADR 0021) puts rollgate on a computer's internal disk.
You don't need to know the computer's network ports or type any commands: the installer finds the port that goes to your modem and sets up the rest.

**Everything on the computer's internal disk is erased.**

## What you need

- A computer with at least two Ethernet ports, 2 GB of memory and an internal disk of at least 16 GB. It must be able to start in UEFI mode (most computers from 2011 on can).
- A USB stick of at least 4 GB. Everything on it is erased too.
- Another computer (Windows, macOS or Linux) to prepare the stick.
- Optional: an SSH public key, if you want to log in to the router over SSH.

## 1. Write the stick

1. Download `rollgate-installer-<date>.img` *(planned: published with each release; until then, build it with `make installer`, docs/dev-setup.md)*.
2. Write it to the stick with [balenaEtcher](https://etcher.balena.io/) (choose the file, the stick, then Flash), or with [Rufus](https://rufus.ie/): choose the file, and when Rufus asks, choose **"Write in DD Image mode"**.
   On Linux or macOS, `dd` works too: `sudo dd if=rollgate-installer-<date>.img of=/dev/<stick> bs=4M conv=fsync`.
3. Take the stick out and plug it back in. A drive called **ROLLGATE** appears, with a `rollgate` folder in it.
   If Windows offers to format a drive, choose **Cancel**: that's the installer's own system, which Windows can't read.

## 2. Add your SSH key (optional)

Copy your SSH **public** key into the stick's `rollgate` folder and name it `ssh-key.pub`.
It's usually `id_ed25519.pub` in the `.ssh` folder of your home folder; make one with `ssh-keygen -t ed25519` if you have none.
Never copy the file without `.pub`: that's your private key, and the installer refuses it.

Without a key, the router's SSH stays closed. You can still use the web UI and the console.

People who know what they want can also put their own `router.toml` in the folder (docs/config-spec.md); the installer uses it instead of setting up the ports, and refuses it if it isn't valid.

## 3. Start the computer from the stick

1. Plug the cable from your modem into one of the computer's network ports.
2. Plug in the stick, switch the computer on, and open its boot menu (often F12, F11, F8 or Esc right after switching on).
   Choose the entry for the stick that starts with **"UEFI:"**.
   If there's none: open the BIOS setup (often Del or F2), turn on UEFI boot, turn off Legacy/CSM and Secure Boot, and try again.
3. The installer starts by itself and shows what it found:
   - **Internet (WAN):** the port that got an address from your modem.
   - **Home network (LAN):** the port for your home network; the router will be `192.168.10.1` (or another address if your modem already uses that one).
   - **Not used for now:** any other ports. They can't join the home network yet.
   - **The disk** it will install on. It never offers the stick.

   If it can't tell which port goes to the modem, it asks you to plug the modem's cable in (or unplug it and plug it in again), and watches which port comes alive.
4. Check the disk, then type **`erase`** and press Enter. Anything else writes nothing.
5. Writing and checking take a few minutes. When it says so, remove the stick and press Enter.

## 4. The first start

The router sets itself up (ADR 0020): it applies the settings the installer wrote and checks that everything works.
Then the screen asks you to choose a password. It's the password for the console (log in as `root`) and for the web UI.

Connect a computer, a switch or a Wi-Fi access point to the home network port. Devices there get an address from the router, and the web UI is at `https://192.168.10.1` (the screen shows the address).

## If something goes wrong

The stick's `rollgate` folder now has two files you can read on another computer:

- `install-log.txt`: everything the installer showed, and why it stopped if it did.
- `hardware.txt`: the computer's processor, memory, disks and network ports with their drivers. Include it when you ask for help.

The installer writes nothing to the disk before you type `erase`, so you can always switch the computer off and start again.
To reinstall a router later, see docs/runbooks/recovery.md, section 4.
