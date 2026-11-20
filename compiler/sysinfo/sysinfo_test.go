package sysinfo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func plant(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	plant(t, root, map[string]string{
		"proc/sys/kernel/hostname":  "gateway\n",
		"proc/sys/kernel/osrelease": "6.12.10-arch1-1\n",
		"proc/uptime":               "533412.81 1040123.20\n",
		"proc/loadavg":              "0.12 0.08 0.01 1/123 4567\n",
		"proc/meminfo":              "MemTotal:        2014532 kB\nMemFree:          912345 kB\nMemAvailable:    1523456 kB\n",
		"proc/cpuinfo": "processor\t: 0\nmodel name\t: Intel(R) Celeron(R) N5105 @ 2.00GHz\n\n" +
			"processor\t: 1\nmodel name\t: Intel(R) Celeron(R) N5105 @ 2.00GHz\n",
		"sys/class/dmi/id/sys_vendor":   "QEMU\n",
		"sys/class/dmi/id/product_name": "Standard PC (Q35 + ICH9, 2009)\n",
		"sys/class/net/lo/operstate":    "unknown\n",
		"sys/class/net/wan/operstate":   "up\n",
		"sys/class/net/wan/address":     "52:54:00:00:00:02\n",
		"sys/class/net/wan/speed":       "1000\n",
		"sys/class/net/lan/operstate":   "down\n",
		"sys/class/net/lan/address":     "52:54:00:00:00:01\n",
		"sys/class/net/lan/speed":       "-1\n",
	})
	addrs := func(name string) []string {
		if name == "wan" {
			return []string{"10.0.3.15/24"}
		}
		return nil
	}
	in := Read(root, addrs)
	if in.Hostname != "gateway" || in.Kernel != "6.12.10-arch1-1" || in.UptimeSeconds != 533412 {
		t.Errorf("hostname, kernel, uptime: %q %q %d", in.Hostname, in.Kernel, in.UptimeSeconds)
	}
	if in.Load != [3]float64{0.12, 0.08, 0.01} {
		t.Errorf("load %v", in.Load)
	}
	if in.MemTotal != 2014532*1024 || in.MemAvailable != 1523456*1024 {
		t.Errorf("memory %d %d", in.MemTotal, in.MemAvailable)
	}
	if in.CPU != "Intel(R) Celeron(R) N5105 @ 2.00GHz" || in.CPUs != 2 {
		t.Errorf("cpu %q × %d", in.CPU, in.CPUs)
	}
	if in.Hardware != "QEMU Standard PC (Q35 + ICH9, 2009)" {
		t.Errorf("hardware %q", in.Hardware)
	}
	if in.DiskTotal == 0 || in.DiskFree == 0 || in.DiskFree > in.DiskTotal {
		t.Errorf("disk %d free of %d", in.DiskFree, in.DiskTotal)
	}
	want := []Interface{
		{Name: "lan", State: "down", MAC: "52:54:00:00:00:01"},
		{Name: "wan", State: "up", MAC: "52:54:00:00:00:02", SpeedMbps: 1000, Addresses: []string{"10.0.3.15/24"}},
	}
	if !reflect.DeepEqual(in.Interfaces, want) {
		t.Errorf("interfaces:\n%+v\nwant\n%+v", in.Interfaces, want)
	}
}

func TestReadMissing(t *testing.T) {
	// A system without these files (a container, a test machine) gives
	// empty fields, not a failure.
	in := Read(t.TempDir(), nil)
	if in.Kernel != "" || in.CPUs != 0 || in.Interfaces != nil {
		t.Errorf("%+v", in)
	}
}

func TestHardwarePlaceholders(t *testing.T) {
	root := t.TempDir()
	plant(t, root, map[string]string{
		"sys/class/dmi/id/sys_vendor":   "To Be Filled By O.E.M.\n",
		"sys/class/dmi/id/product_name": "Default string\n",
	})
	if h := Read(root, nil).Hardware; h != "" {
		t.Errorf("placeholders shown as hardware: %q", h)
	}
	plant(t, root, map[string]string{
		"sys/class/dmi/id/sys_vendor":   "VMware, Inc.\n",
		"sys/class/dmi/id/product_name": "VMware Virtual Platform\n",
	})
	if h := Read(root, nil).Hardware; h != "VMware, Inc. VMware Virtual Platform" {
		t.Errorf("hardware %q", h)
	}
}
