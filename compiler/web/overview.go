package web

import (
	"context"
	"fmt"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

// sysView is the Overview's system facts, ready to show.
type sysView struct {
	Kernel, Uptime, Load       string
	Memory, Disk               string // "312 MB of 1.9 GB"
	MemoryPct, DiskPct         int    // used, 0-100
	Hostname, Release          string
	Hardware, CPU              string
	Interfaces                 []ifaceView
	MemoryHigh, DiskHigh, Busy bool // worth a second look
}

type ifaceView struct {
	Name, State, MAC, Speed string
	Up                      bool
	Addresses               []string
}

// loadSystem asks the API for the system facts and puts them on p.
func (s *Server) loadSystem(ctx context.Context, p *page) {
	resp, err := s.API.Get(ctx, "/v1/system")
	if err != nil || !resp.OK || resp.System == nil {
		return // the rest of the page still works; Status says more
	}
	p.System = systemView(*resp.System)
}

func systemView(in sysinfo.Info) *sysView {
	v := &sysView{Kernel: in.Kernel, Hostname: in.Hostname, Release: in.Release, Hardware: in.Hardware,
		Uptime: duration(in.UptimeSeconds),
		Load:   fmt.Sprintf("%.2f  %.2f  %.2f", in.Load[0], in.Load[1], in.Load[2])}
	if in.CPU != "" {
		cpu := strings.NewReplacer("(R)", "", "(TM)", "", "(tm)", "", " CPU", "").Replace(in.CPU)
		v.CPU = strings.Join(strings.Fields(cpu), " ")
		if in.CPUs > 1 {
			v.CPU += fmt.Sprintf(" × %d", in.CPUs)
		}
	}
	if in.MemTotal > 0 {
		used := in.MemTotal - min(in.MemAvailable, in.MemTotal)
		v.Memory = bytes(used) + " of " + bytes(in.MemTotal)
		v.MemoryPct = int(used * 100 / in.MemTotal)
		v.MemoryHigh = v.MemoryPct >= 90
	}
	if in.DiskTotal > 0 {
		used := in.DiskTotal - min(in.DiskFree, in.DiskTotal)
		v.Disk = bytes(used) + " of " + bytes(in.DiskTotal)
		v.DiskPct = int(used * 100 / in.DiskTotal)
		v.DiskHigh = v.DiskPct >= 90
	}
	v.Busy = in.CPUs > 0 && in.Load[1] > float64(in.CPUs)
	for _, i := range in.Interfaces {
		iv := ifaceView{Name: i.Name, State: i.State, MAC: i.MAC, Up: i.State == "up", Addresses: i.Addresses}
		switch {
		case i.SpeedMbps >= 1000 && i.SpeedMbps%1000 == 0:
			iv.Speed = fmt.Sprintf("%d Gb/s", i.SpeedMbps/1000)
		case i.SpeedMbps > 0:
			iv.Speed = fmt.Sprintf("%d Mb/s", i.SpeedMbps)
		}
		v.Interfaces = append(v.Interfaces, iv)
	}
	return v
}

// bytes is a size as a person reads it: "312 MB", "7.4 GB".
func bytes(n uint64) string {
	const kib, mib, gib = 1 << 10, 1 << 20, 1 << 30
	switch {
	case n >= 10*gib:
		return fmt.Sprintf("%d GB", n/gib)
	case n >= gib:
		return fmt.Sprintf("%.1f GB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%d MB", n/mib)
	case n >= kib:
		return fmt.Sprintf("%d kB", n/kib)
	}
	return fmt.Sprintf("%d bytes", n)
}

// duration is an uptime as a person reads it: "6 days 4 h", "3 h 12 min".
func duration(s int64) string {
	d, h, m := s/86400, s%86400/3600, s%3600/60
	switch {
	case d == 1:
		return fmt.Sprintf("1 day %d h", h)
	case d > 1:
		return fmt.Sprintf("%d days %d h", d, h)
	case h > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d min", m)
}
