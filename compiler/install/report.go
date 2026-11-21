package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hardware is hardware.txt: what the computer has, for the owner to read or
// share from another computer (ADR 0021).
func (in *Installer) hardware(ports []Port, disks []Disk) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "rollgate installer %s, %s\r\n\r\n", in.Release, in.Now().UTC().Format("2006-01-02 15:04 UTC"))
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\r\n", a...) }

	firmware := "Legacy BIOS"
	if exists(in.path("sys/firmware/efi")) {
		firmware = "UEFI, Secure Boot " + secureBoot(in.path("sys/firmware/efi/efivars"))
	}
	line("Firmware: %s", firmware)
	model, threads := cpu(in.path("proc/cpuinfo"))
	line("CPU:      %s (%d threads)", model, threads)
	line("Memory:   %s", Size(memTotal(in.path("proc/meminfo"))))
	line("Kernel:   %s", readTrim(in.path("proc/sys/kernel/osrelease")))
	for _, l := range [][3]string{{"Board:   ", "board_vendor", "board_name"}, {"BIOS:    ", "bios_version", "bios_date"}} {
		if s := strings.TrimSpace(readTrim(in.path("sys/class/dmi/id/"+l[1])) + " " + readTrim(in.path("sys/class/dmi/id/"+l[2]))); s != "" {
			line("%s %s", l[0], s)
		}
	}

	line("\r\nDisks:")
	for _, d := range disks {
		why := d.Why()
		if why == "" {
			why = "can be installed on"
		}
		line("  %s: %s", d, why)
	}
	line("\r\nNetwork ports:")
	var drivers []string
	for _, p := range ports {
		line("  %s", p)
		if p.Driver != "" {
			drivers = append(drivers, p.Driver)
		}
	}
	if len(ports) == 0 {
		line("  none")
	}

	if out, err := in.Run("lspci", "-nnk"); err == nil {
		line("\r\nPCI devices (lspci -nnk):")
		b.WriteString(crlf(out))
	}
	if out, err := in.Run("dmesg"); err == nil {
		line("\r\nKernel messages about firmware and the network drivers:")
		for _, l := range strings.Split(string(out), "\n") {
			low := strings.ToLower(l)
			match := strings.Contains(low, "firmware")
			for _, d := range drivers {
				match = match || strings.Contains(l, d)
			}
			if match {
				line("  %s", strings.TrimSpace(l))
			}
		}
	}
	return b.Bytes()
}

// crlf gives text Windows line ends, so Notepad shows it right.
func crlf(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n", "\r\n")
}

// cpu reads the processor's name and how many threads it runs.
func cpu(path string) (string, int) {
	model, n := "unknown", 0
	for _, l := range strings.Split(string(readFile(path)), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			n++
		case "model name":
			model = strings.TrimSpace(v)
		}
	}
	return model, n
}

// secureBoot reads the firmware's SecureBoot variable: on, off or unknown.
func secureBoot(efivars string) string {
	matches, _ := filepath.Glob(filepath.Join(efivars, "SecureBoot-*"))
	if len(matches) == 0 {
		return "unknown"
	}
	b, err := os.ReadFile(matches[0])
	if err != nil || len(b) < 5 {
		return "unknown"
	}
	if b[4] == 1 {
		return "on"
	}
	return "off"
}
