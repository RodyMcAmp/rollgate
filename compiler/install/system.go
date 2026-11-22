package install

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// probeNetwork is the systemd-networkd file Probe writes while it asks for
// addresses. systemd-networkd is part of systemd, which the installer's
// system already runs; nothing else is needed.
const probeNetwork = "/run/systemd/network/10-rollgate-install.network"

// Probe brings the ports up, asks each for an address by DHCP for up to 30
// seconds, and returns them with their link state and the address each got.
// It gives up early when no port has a cable in, and waits 6 seconds after
// the first address in case another port gets one too. It leaves the ports
// up, without addresses, so WaitLink can see cables being plugged in.
func Probe(ports []Port, out io.Writer) ([]Port, error) {
	var names []string
	for _, p := range ports {
		names = append(names, p.Name)
		if err := run("ip", "link", "set", "dev", p.Name, "up"); err != nil {
			return ports, err
		}
	}
	network := "# Written by rollgate install while it looks for the modem (ADR 0021).\n" +
		"[Match]\nName=" + strings.Join(names, " ") + "\n\n" +
		"[Link]\nRequiredForOnline=no\n\n" +
		"[Network]\nDHCP=ipv4\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n" +
		"[DHCPv4]\nUseDNS=no\nUseNTP=no\nUseHostname=no\nSendHostname=no\n"
	if err := os.MkdirAll(filepath.Dir(probeNetwork), 0o755); err != nil {
		return ports, err
	}
	if err := os.WriteFile(probeNetwork, []byte(network), 0o644); err != nil {
		return ports, err
	}
	defer func() {
		_ = os.Remove(probeNetwork)
		// The sockets too, or the next network event starts it again.
		for _, u := range []string{"systemd-networkd.socket", "systemd-networkd-varlink.socket", "systemd-networkd.service"} {
			_ = run("systemctl", "stop", u) // one at a time: a unit this systemd lacks fails alone
		}
		for _, n := range names {
			_ = run("ip", "-4", "addr", "flush", "dev", n)
		}
	}()
	if err := run("systemctl", "restart", "systemd-networkd.service"); err != nil {
		return ports, err
	}

	start := time.Now()
	var first time.Time
	result := append([]Port{}, ports...)
	for {
		time.Sleep(500 * time.Millisecond)
		carrier := false
		for i := range result {
			result[i].Carrier = readTrim("/sys/class/net/"+result[i].Name+"/carrier") == "1"
			carrier = carrier || result[i].Carrier
			if result[i].Addr = ipv4(result[i].Name); result[i].Addr.IsValid() && first.IsZero() {
				first = time.Now()
			}
		}
		waited := time.Since(start)
		switch {
		case waited > 30*time.Second,
			!first.IsZero() && time.Since(first) > 6*time.Second,
			waited > 6*time.Second && !carrier:
			return result, nil
		}
	}
}

// ipv4 returns the IPv4 address a port has, with its network's size, if any
// (not a link-local one).
func ipv4(name string) netip.Prefix {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Prefix{}
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		ones, _ := n.Mask.Size()
		if ok && ip.Unmap().Is4() && !ip.IsLinkLocalUnicast() {
			return netip.PrefixFrom(ip.Unmap(), ones)
		}
	}
	return netip.Prefix{}
}

// WaitLink returns the first port whose cable is plugged in (its link goes
// from down to up). The ports must be up (Probe leaves them so).
func WaitLink(ports []Port) (string, error) {
	carrier := func(name string) bool { return readTrim("/sys/class/net/"+name+"/carrier") == "1" }
	last := map[string]bool{}
	for _, p := range ports {
		last[p.Name] = carrier(p.Name)
	}
	for {
		time.Sleep(500 * time.Millisecond)
		for _, p := range ports {
			now := carrier(p.Name)
			if now && !last[p.Name] {
				return p.Name, nil
			}
			last[p.Name] = now
		}
	}
}

// InstallFiles mounts the root file system WriteImage put on dev, at a
// temporary directory, and writes files into it.
func InstallFiles(dev string, files []File) error {
	offset, err := RootOffset(dev)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "rollgate-target-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	// subvol=/@ is the running system (docs/architecture.md#disk-layout). The
	// partition itself, which the kernel knows since WriteImage had it re-read
	// the table: a loop device over the disk would be a second device with the
	// same Btrfs file system on it. The loop device only if the kernel lacks it.
	args := []string{"-t", "btrfs", "-o", "subvol=/@"}
	if part := partition(dev, offset); part != "" {
		args = append(args, part, dir)
	} else {
		args = append(args, "-o", "loop,offset="+strconv.FormatInt(offset, 10), dev, dir)
	}
	if err := run("mount", args...); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(path), f.DirMode)
		}
		if err == nil {
			err = os.WriteFile(path, f.Data, f.Mode)
		}
		if err == nil {
			err = os.Chmod(path, f.Mode) // WriteFile keeps an existing file's mode
		}
	}
	if uerr := run("umount", dir); err == nil {
		err = uerr
	}
	_ = run("sync")
	return err
}

// partition returns the device of dev's partition that starts at offset,
// waiting a few seconds for the kernel to make it; "" if it doesn't.
func partition(dev string, offset int64) string {
	disk := filepath.Join("/sys/class/block", filepath.Base(dev))
	start := strconv.FormatInt(offset/512, 10)
	for range 10 {
		entries, _ := os.ReadDir(disk)
		for _, e := range entries {
			if readTrim(filepath.Join(disk, e.Name(), "start")) == start && exists("/dev/"+e.Name()) {
				return "/dev/" + e.Name()
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
