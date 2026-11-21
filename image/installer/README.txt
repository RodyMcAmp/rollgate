rollgate installer
==================

This USB stick puts rollgate, a router system, on a computer's internal disk.
Everything on that disk is erased.

Before you start the computer from this stick, you can put files in this folder:

  ssh-key.pub    Your SSH public key (the .pub file, for example id_ed25519.pub,
                 renamed). It lets you log in to the router over SSH as root.
                 Optional: without it, SSH stays closed.

  router.toml    Your own router settings, if you know what you want
                 (docs/config-spec.md). Optional: without it, the installer
                 finds the port to your modem and sets up the rest by itself.

After the install, this folder also has:

  install-log.txt   what the installer did
  hardware.txt      the computer's hardware: CPU, memory, disks, network ports

How to install:

  1. Plug the cable from your modem into one of the computer's network ports.
  2. Start the computer from this stick, in UEFI mode, with Secure Boot off.
  3. Check what the installer found, then type erase and press Enter.
  4. Remove the stick and press Enter. The router starts, sets itself up, and
     asks you to choose a password.

If Windows offers to format a drive when you plug the stick in, choose Cancel:
that's the installer's own system, which Windows can't read.

More: https://github.com/RodyMcAmp/rollgate/blob/main/docs/install.md
