package web

import (
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

var sampleSystem = sysinfo.Info{
	Hostname: "gateway", Kernel: "6.12.10-arch1-1", Release: "2026-11-16",
	Hardware: "Protectli VP2420", CPU: "Intel(R) Celeron(R) CPU J6412 @ 2.00GHz", CPUs: 4,
	UptimeSeconds: 6*86400 + 4*3600 + 59, Load: [3]float64{0.12, 0.08, 0.01},
	MemTotal: 8 << 30, MemAvailable: 6 << 30, DiskTotal: 60 << 30, DiskFree: 57 << 30,
	Interfaces: []sysinfo.Interface{
		{Name: "lan", State: "up", MAC: "00:00:5e:00:53:02", SpeedMbps: 2500, Addresses: []string{"192.168.10.1/24", "fd12:3456::1/64"}},
		{Name: "wan", State: "down", MAC: "00:00:5e:00:53:01"},
	},
}

func TestOverviewSystem(t *testing.T) {
	s, f, h, _ := setup(t)
	f.system = &sampleSystem
	c, _ := loginForm(t, s, h)
	body := do(h, "GET", "/", nil, c).Body.String()
	for _, want := range []string{
		"6.12.10-arch1-1", "6 days 4 h", "0.12  0.08  0.01",
		"25%", "2.0 GB of 8.0 GB used", `value="25"`, // memory
		"5%", "3.0 GB of 60 GB used", // disk
		"Protectli VP2420", "Intel Celeron J6412 @ 2.00GHz × 4", "release 2026-11-16",
		"192.168.10.1/24<br>fd12:3456::1/64", "2500 Mb/s", `<span class="ok">up</span>`, `<span class="muted">down</span>`,
		"all checks pass",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestOverviewWithoutSystem(t *testing.T) {
	// An API without /v1/system (an older router): the page still works.
	s, _, h, _ := setup(t)
	c, _ := loginForm(t, s, h)
	w := do(h, "GET", "/", nil, c)
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "all checks pass") || strings.Contains(body, "Kernel") {
		t.Errorf("overview without system facts: %d\n%s", w.Code, body)
	}
}

func TestSystemViewFlags(t *testing.T) {
	in := sampleSystem
	in.MemAvailable, in.DiskFree, in.Load = 100<<20, 1<<30, [3]float64{5, 6, 7}
	v := systemView(in)
	if !v.MemoryHigh || !v.DiskHigh || !v.Busy {
		t.Errorf("high memory, disk and load not flagged: %+v", v)
	}
	if got := duration(59 * 60); got != "59 min" {
		t.Errorf("duration: %q", got)
	}
	if got := duration(86400 + 3600); got != "1 day 1 h" {
		t.Errorf("duration: %q", got)
	}
	if got := bytes(312 << 20); got != "312 MB" {
		t.Errorf("bytes: %q", got)
	}
	if got := systemView(sysinfo.Info{Interfaces: []sysinfo.Interface{{Name: "wan", SpeedMbps: 1000}}}).Interfaces[0].Speed; got != "1 Gb/s" {
		t.Errorf("speed: %q", got)
	}
}
