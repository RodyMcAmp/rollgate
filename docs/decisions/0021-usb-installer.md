# 0021. A USB installer that writes the router image and sets up the ports

- Status: Accepted (by the maintainer, 2026-11-20)
- Date: 2026-11-20

## Context

M5 puts rollgate on real hardware: first the maintainer's Ivy Bridge PC with Realtek network ports.
Today the only way onto a real disk is to write `rollgate-<date>.img` straight onto it: take the disk out, or boot some other Linux from USB and copy it by hand.
`docs/build-and-release.md` promises an "installer ISO", and the recovery runbook a "USB installer", but neither exists.

The image also can't know the new box's ports. `router.toml` names each port by its MAC address (`match_mac`), and the person installing doesn't know them, or which socket is which.
The roadmap's "live router install media" idea already sketches the answer: a first-boot tool picks WAN as the port that gets a DHCP lease, makes the others LAN, and picks a LAN subnet that doesn't clash with the modem's. It writes `router.toml`, never generated files (rule 3).

ADR 0020 already makes the router set itself up once it has a `router.toml`: it applies it and asks for a password.
So the installer only has to get the system onto the disk with a `router.toml` that fits the box, and, optionally, the owner's SSH key.

## Decision

### The media: one USB image file per release

- Each release publishes `rollgate-installer-<date>.img` next to the router image, with its checksum and signature (ADR 0012). It's written to a USB stick with balenaEtcher, Rufus (in "DD image" mode) or `dd`.
- It's a disk image, not an ISO: its first partition is a FAT32 EFI system partition, labelled `ROLLGATE`, that Windows and macOS show after writing. It has a `rollgate/` folder with a `README.txt`, where the owner can put:
  - `ssh-key.pub`: an SSH public key, installed as root's (optional; without one, SSH stays closed until a key is added at the console or on the web UI *(planned)*);
  - `router.toml`: a config to use instead of the detected one (optional, for people who know what they want; it must validate).
  The installer writes `install-log.txt` and `hardware.txt` back to the same folder, so the owner can read what happened, and share the hardware details, from another computer.
  An ISO can't do this: its file system is read-only.
- The second partition holds the installer's own system (read-only) and the router image (zstd-compressed).
- UEFI only, like the router (ADR 0009). Secure Boot must be off.

### The installer's system

- Built by a new `image/installer.sh` with the same tools and the same pinned archive date as the router image (`pacstrap`, ADR 0009; stable repos only, rule 1). It holds `linux-lts`, the same firmware packages as the router (`linux-firmware-intel`, `linux-firmware-realtek`), and the tools it needs (`zstd`, `util-linux`, `iproute2`, `dosfstools`). No new package source, no daemon.
- It boots from the USB stick and runs `rollgate install` on the screen (tty1) and the serial console. It needs no internet.

### `rollgate install`

A new command in the `rollgate` binary (Go, no new language):

1. **Checks the basics:** booted in UEFI mode (if not: "turn on UEFI boot, and turn off Legacy/CSM, in the BIOS setup", and stop); at least 2 GB of RAM.
2. **Picks the disk:** lists the internal disks of at least 16 GB, by size and model, never the USB stick it booted from. With one disk, it suggests that one. The owner confirms by typing `erase`. Nothing is written before that.
3. **Writes the router image** to the disk, reads it back to check the checksum, and lets the first boot grow the root file system to fill the disk (already done at first boot).
4. **Sets up the ports** (unless the stick has a `router.toml`):
   - It lists the Ethernet ports, with their MAC addresses, drivers and link state.
   - It asks each port with a cable for a DHCP address, briefly. The port that gets one is WAN (the modem). Another port becomes `lan`: the one with a cable in, or else the first. `router.toml` can't bridge several ports into one network yet, so any further ports are listed in it as comments, with their MAC addresses, and left unused; joining them to LAN is a config-spec change for later.
   - If no port, or more than one, gets an address, it asks: "Plug the cable from your modem into the port you want for the internet", and watches which port's link comes up.
   - A box with only one Ethernet port can't be a router here: it says so and stops. (VLAN-on-a-stick is a later option.)
   - LAN gets `192.168.10.1/24`, as in `examples/router.toml`, or the first of `192.168.20.0/24`, `10.10.0.0/24`, `172.16.10.0/24` that doesn't overlap the WAN address it saw.
   - It writes `router.toml` from a small template: WAN by DHCP with `ipv6 = "auto"`, LAN with DHCP for its devices, management from LAN only, every other option at its default. It validates it with the same code as `rollgate validate`, and refuses to install a config that doesn't validate.
5. **Installs the SSH key** from `ssh-key.pub`, if there is one.
6. **Writes `hardware.txt` and `install-log.txt`** to the stick: CPU, memory, disks, PCI devices with their drivers (`lspci -nnk`), the ports and what was chosen.
7. Says "Remove the USB stick, then press Enter to restart." The first boot (ADR 0020) does the rest: it applies the config and asks for the password.

### Tests

- A new L3 `make test-install`: QEMU boots the installer image with a blank target disk, three network ports (one with DHCP from QEMU, two without), and a `ssh-key.pub` on the stick. The test drives the console as `test-firstboot` does: it refuses until `erase` is typed, then installs. Then the target disk boots on its own: the first boot applies the detected config (WAN has an address, LAN is `192.168.10.1`), the key logs in over SSH, and `hardware.txt` and `install-log.txt` are on the stick.
- Unit tests for choosing the disk, choosing WAN and the LAN subnet, and the `router.toml` template (it validates).
- The release workflow builds and publishes the installer image; `release/dry-run.sh` checks it's signed with the rest.

## Consequences

- Installing on real hardware becomes: write the stick, boot it, type `erase`, then choose a password. No MAC addresses, no commands.
- `hardware.txt` gives the details of the pilot's hardware (and the Realtek ports' driver) without needing network access to the box.
- The detected `router.toml` is deliberately plain: two zones, defaults everywhere. Anything more is changed afterwards on the web UI, through the normal apply path.
- Not in this ADR: a "live" stick that runs as a router without installing, and the runbook's "Rescue" entry. The installer's system is built so either can be added later (a later ADR).
- `docs/build-and-release.md` changes "installer ISO" to the installer image, and the recovery runbook's reinstall step points at it.
- Writing a disk is destructive, so the installer never writes before the typed `erase`, and never offers the stick it booted from. The image build, boot media and the generated config's management access are safety-critical, and need the maintainer's review.
