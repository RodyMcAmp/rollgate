// Package sysinfo reads facts about the router for the web UI's Overview:
// the kernel, uptime, load, memory, disk, hardware and network interfaces.
// It only reads (/proc, /sys and the root filesystem's size) and changes nothing.
package sysinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Info is what the Overview shows. Fields that can't be read are left empty.
type Info struct {
	Hostname      string      `json:"hostname"`
	Kernel        string      `json:"kernel"`  // uname -r
	Release       string      `json:"release"` // the rollgate release (filled in by the API)
	Hardware      string      `json:"hardware,omitempty"`
	CPU           string      `json:"cpu,omitempty"`
	CPUs          int         `json:"cpus"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	Load          [3]float64  `json:"load"` // 1, 5 and 15 minutes
	MemTotal      uint64      `json:"mem_total"`
	MemAvailable  uint64      `json:"mem_available"`
	DiskTotal     uint64      `json:"disk_total"` // the root filesystem
	DiskFree      uint64      `json:"disk_free"`
	Interfaces    []Interface `json:"interfaces"`
}

// Interface is a network port or VLAN.
type Interface struct {
	Name      string   `json:"name"`
	State     string   `json:"state"` // up, down, ...
	MAC       string   `json:"mac,omitempty"`
	SpeedMbps int      `json:"speed_mbps,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// Addrs returns an interface's addresses (net.InterfaceByName(name).Addrs()
// on a router); a function, so tests can give their own.
type Addrs func(name string) []string

// Read gathers Info from the files below root ("/" on a router).
func Read(root string, addrs Addrs) Info {
	p := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	var in Info
	in.Hostname = firstLine(p("proc/sys/kernel/hostname"))
	in.Kernel = firstLine(p("proc/sys/kernel/osrelease"))
	if f := strings.Fields(firstLine(p("proc/uptime"))); len(f) > 0 {
		if s, err := strconv.ParseFloat(f[0], 64); err == nil {
			in.UptimeSeconds = int64(s)
		}
	}
	if f := strings.Fields(firstLine(p("proc/loadavg"))); len(f) >= 3 {
		for i := range in.Load {
			in.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	mem := keyValues(p("proc/meminfo"), ":")
	in.MemTotal = kB(mem["MemTotal"])
	in.MemAvailable = kB(mem["MemAvailable"])
	in.CPU, in.CPUs = cpu(p("proc/cpuinfo"))
	in.Hardware = hardware(p("sys/class/dmi/id"))
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err == nil {
		in.DiskTotal = st.Blocks * uint64(st.Bsize)
		in.DiskFree = st.Bavail * uint64(st.Bsize)
	}
	in.Interfaces = interfaces(p("sys/class/net"), addrs)
	return in
}

func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// keyValues reads "key<sep> value" lines.
func keyValues(path, sep string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), sep); ok {
			k = strings.TrimSpace(k)
			if _, seen := m[k]; !seen {
				m[k] = strings.TrimSpace(v)
			}
		}
	}
	return m
}

// kB turns meminfo's "2048000 kB" into bytes.
func kB(v string) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(v, "kB")), 10, 64)
	if err != nil {
		return 0
	}
	return n * 1024
}

func cpu(path string) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	model, n := "", 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			n++
		case "model name":
			if model == "" {
				model = strings.Join(strings.Fields(v), " ")
			}
		}
	}
	return model, n
}

// hardware is the machine's maker and model from the firmware, if it says.
func hardware(dir string) string {
	var parts []string
	for _, name := range []string{"sys_vendor", "product_name"} {
		v := firstLine(filepath.Join(dir, name))
		low := strings.ToLower(v)
		if v == "" || strings.Contains(low, "o.e.m") || strings.Contains(low, "default string") {
			continue
		}
		if len(parts) > 0 && strings.HasPrefix(v, parts[0]) {
			v = strings.TrimSpace(strings.TrimPrefix(v, parts[0]))
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, " ")
}

func interfaces(dir string, addrs Addrs) []Interface {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Interface
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		in := Interface{Name: name, State: firstLine(filepath.Join(dir, name, "operstate")),
			MAC: firstLine(filepath.Join(dir, name, "address"))}
		if s, err := strconv.Atoi(firstLine(filepath.Join(dir, name, "speed"))); err == nil && s > 0 {
			in.SpeedMbps = s
		}
		if addrs != nil {
			in.Addresses = addrs(name)
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
