# Developer setup

`make check` runs anywhere with `shellcheck` and Python 3.11+.
`make image` and `make test-vm` need an **Arch Linux** host (for `pacstrap`), root, loop devices and Btrfs support.
CI does the same thing in an `archlinux` container (`.github/workflows/ci.yml`).

## Windows: WSL2 with Arch Linux

1. In PowerShell **as Administrator** (if WSL isn't installed yet, this installs it too and may ask you to reboot):

   ```
   wsl --install archlinux
   ```

2. Open the Arch shell (`wsl -d archlinux`). It starts as root. Install the tools:

   ```
   pacman -Syu --needed make git rsync openssh arch-install-scripts btrfs-progs dosfstools gptfdisk grub qemu-system-x86 edk2-ovmf shellcheck go staticcheck
   ```

3. Put the repository on the **Linux** filesystem, not under `/mnt/c`. Loop devices and Btrfs images on Windows drives are slow or don't work.

   ```
   git clone <repo-url> ~/rollgate     # or: cp -r /mnt/c/path/to/rollgate ~/rollgate
   cd ~/rollgate
   ```

4. Build and test:

   ```
   make check
   make image        # first run downloads ~630 MB of packages into build/pkgcache
   make test-vm      # both kernels boot (~1 min with KVM)
   make test-rollback   # rollback rehearsal, 4 boots (~2 min with KVM)
   ```

### If something fails in WSL

- **`unknown filesystem type 'btrfs'`**: run `modprobe btrfs` and try again.
- **The boot test says `/dev/kvm` is not available**: it still works using software emulation, just slowly (up to 15 minutes per boot).
  To enable KVM, add this to `%UserProfile%\.wslconfig` on Windows, then run `wsl --shutdown`:

  ```
  [wsl2]
  nestedVirtualization=true
  ```

## Native Arch Linux

Install the same packages as step 2 (add `sudo` if you aren't root). `make` calls `sudo` by itself when needed.

## Useful environment variables

| Variable | Used by | Default |
|---|---|---|
| `BUILD_DIR` | `make image`, `make test-vm` | `build` |
| `DISK_SIZE_MIB` | `image/build.sh` | `8192` |
| `OVMF_CODE`, `OVMF_VARS` | `tests/vm/boot-test.sh` | Arch `edk2-ovmf` paths |

## SSH into a test image

Put your public key in `image/authorized_keys` (it's in `.gitignore`) before `make image`.
To boot the image yourself with SSH forwarded to port 2222:

```
cp --sparse=always build/rollgate-*.img /tmp/try.img
cp /usr/share/edk2/x64/OVMF_VARS.4m.fd /tmp/try.vars
qemu-system-x86_64 -machine q35 -m 1024 -accel kvm -cpu host \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/x64/OVMF_CODE.4m.fd \
  -drive if=pflash,format=raw,file=/tmp/try.vars \
  -drive file=/tmp/try.img,format=raw,if=virtio \
  -nic user,model=virtio-net-pci,hostfwd=tcp::2222-:22 -nographic
ssh -p 2222 root@localhost
```

`-nographic` puts the serial console in your terminal: you'll see GRUB, then a login prompt. Quit QEMU with `Ctrl-a x`.
