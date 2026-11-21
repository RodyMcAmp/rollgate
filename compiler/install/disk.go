// Package install puts rollgate on a computer's internal disk (ADR 0021): it
// picks the disk and the ports, writes the router image, and leaves a
// router.toml that the first boot applies (ADR 0020).
package install

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MinDiskSize is the smallest disk offered: one sold as 16 GB (they hold a
// little under 16·10⁹ bytes). The router image itself is 8 GiB.
const MinDiskSize = 15_000_000_000

// Disk is a whole disk, as the kernel sees it.
type Disk struct {
	Name      string // kernel name: sda, nvme0n1
	Size      int64  // bytes
	Model     string
	Removable bool // the kernel says its media can be removed
	USB       bool // attached through USB, like the installer's own stick
	ReadOnly  bool
	Mounted   bool // it, or one of its partitions, holds a mounted file system
}

// Why says why the installer won't offer d, or "" if it will.
func (d Disk) Why() string {
	switch {
	case d.Mounted:
		return "in use (the installer's own stick)"
	case d.USB || d.Removable:
		return "removable or USB"
	case d.ReadOnly:
		return "read-only"
	case d.Size < MinDiskSize:
		return "smaller than 16 GB"
	}
	return ""
}

// String is how the installer shows d: "sda  500 GB  Samsung SSD 860".
func (d Disk) String() string {
	s := fmt.Sprintf("%s  %s", d.Name, Size(d.Size))
	if d.Model != "" {
		s += "  " + d.Model
	}
	return s
}

// Size formats bytes the way disks are sold: decimal gigabytes.
func Size(b int64) string {
	switch {
	case b >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(b)/1e12)
	case b >= 10e9:
		return fmt.Sprintf("%d GB", (b+5e8)/1e9)
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/1e9)
	}
}

// Candidates returns the disks the installer offers: internal, writable, big
// enough, and not in use. The stick it booted from is always in use: its
// FAT partition is mounted.
func Candidates(disks []Disk) []Disk {
	var out []Disk
	for _, d := range disks {
		if d.Why() == "" {
			out = append(out, d)
		}
	}
	return out
}

// ReadDisks lists the whole disks below sys (normally /sys), sorted by name.
// mountinfo is /proc/self/mountinfo: a disk is Mounted if it or one of its
// partitions is the source of a mount.
func ReadDisks(sys string, mountinfo []byte) ([]Disk, error) {
	ids, names := mountedDevices(mountinfo)
	entries, err := os.ReadDir(filepath.Join(sys, "block"))
	if err != nil {
		return nil, err
	}
	var disks []Disk
	for _, e := range entries {
		name := e.Name()
		dir := filepath.Join(sys, "block", name)
		// Loop, RAM and device-mapper disks have no device; CD drives do,
		// but aren't disks to install on.
		if _, err := os.Stat(filepath.Join(dir, "device")); err != nil || strings.HasPrefix(name, "sr") {
			continue
		}
		sectors, err := strconv.ParseInt(readTrim(filepath.Join(dir, "size")), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: size: %w", name, err)
		}
		d := Disk{
			Name:      name,
			Size:      sectors * 512, // sysfs counts 512-byte sectors on every disk
			Model:     strings.Join(strings.Fields(readTrim(filepath.Join(dir, "device", "model"))), " "),
			Removable: readTrim(filepath.Join(dir, "removable")) == "1",
			ReadOnly:  readTrim(filepath.Join(dir, "ro")) == "1",
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			d.USB = strings.Contains(real, "/usb")
		}
		// The disk and its partitions (subdirectories with a dev file).
		devs := []string{name}
		if subs, err := os.ReadDir(dir); err == nil {
			for _, s := range subs {
				if _, err := os.Stat(filepath.Join(dir, s.Name(), "partition")); err == nil {
					devs = append(devs, s.Name())
				}
			}
		}
		for _, dev := range devs {
			id := readTrim(filepath.Join(dir, dev, "dev"))
			if dev == name {
				id = readTrim(filepath.Join(dir, "dev"))
			}
			if names[dev] || (id != "" && ids[id]) {
				d.Mounted = true
			}
		}
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

// mountedDevices reads mountinfo: the major:minor numbers of mounted devices,
// and the kernel names of mount sources under /dev (Btrfs reports its own
// device numbers, so both are needed).
func mountedDevices(mountinfo []byte) (ids, names map[string]bool) {
	ids, names = map[string]bool{}, map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		ids[f[2]] = true
		for i, s := range f {
			if s == "-" && i+2 < len(f) && strings.HasPrefix(f[i+2], "/dev/") {
				names[filepath.Base(f[i+2])] = true
			}
		}
	}
	return ids, names
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
