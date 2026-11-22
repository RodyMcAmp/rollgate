package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// put writes files below dir: path -> content, "->target" makes a symlink.
func put(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		if target, ok := strings.CutPrefix(content, "->"); ok {
			err = os.Symlink(target, p)
		} else if strings.HasSuffix(path, "/") {
			err = os.MkdirAll(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte(content), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// fakeDisk adds a disk to a fake /sys at sys: dev is "pci" or "usb".
func fakeDisk(t *testing.T, sys, name, bus string, gb int64, removable string, parts ...string) {
	t.Helper()
	real := "devices/pci0000:00/0000:00:1f.2/" + bus + "/host0/block/" + name
	files := map[string]string{
		"block/" + name:         "->../" + real,
		real + "/size":          itoa(gb * 1e9 / 512),
		real + "/removable":     removable,
		real + "/ro":            "0",
		real + "/dev":           "8:" + itoa(int64(len(name))*16),
		real + "/device/model":  "Disk  " + strings.ToUpper(name) + " ",
		real + "/device/vendor": "x",
	}
	for i, p := range parts {
		files[real+"/"+p+"/partition"] = itoa(int64(i + 1))
		files[real+"/"+p+"/dev"] = "8:" + itoa(int64(len(name))*16+int64(i)+1)
	}
	put(t, sys, files)
}

func TestReadDisksAndCandidates(t *testing.T) {
	sys := t.TempDir()
	fakeDisk(t, sys, "sda", "ata1", 500, "0", "sda1")
	fakeDisk(t, sys, "sdbb", "usb1", 32, "1", "sdbb1", "sdbb2") // the stick: USB, mounted
	fakeDisk(t, sys, "sdc", "ata2", 8, "0")                     // too small
	fakeDisk(t, sys, "nvme0n1", "nvme", 256, "0", "nvme0n1p1")  // mounted through Btrfs
	put(t, sys, map[string]string{"block/loop0/size": "1000000000"})
	// sdbb1's numbers (8:65), and a Btrfs mount whose numbers are its own.
	mountinfo := []byte("22 1 0:20 / / rw - overlay overlay rw\n" +
		"30 22 8:65 / /run/rollgate/stick rw,noatime - vfat /dev/sdbb1 rw\n" +
		"31 22 0:44 / /mnt rw - btrfs /dev/nvme0n1p1 rw\n")
	disks, err := ReadDisks(sys, mountinfo)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range disks {
		got = append(got, d.Name+":"+d.Why())
	}
	want := "nvme0n1:in use (the installer's own stick)|sda:|sdbb:in use (the installer's own stick)|sdc:smaller than 16 GB"
	if strings.Join(got, "|") != want {
		t.Errorf("disks:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
	c := Candidates(disks)
	if len(c) != 1 || c[0].Name != "sda" || c[0].Size != 500e9 || c[0].Model != "Disk SDA" {
		t.Errorf("candidates: %+v", c)
	}
	if s := c[0].String(); s != "sda  500 GB  Disk SDA" {
		t.Errorf("String() = %q", s)
	}
	// A removable disk is never offered, even when nothing on it is mounted.
	disks, _ = ReadDisks(sys, nil)
	for _, d := range Candidates(disks) {
		if d.Name == "sdbb" {
			t.Error("a USB disk was offered")
		}
	}
}

func TestReadPorts(t *testing.T) {
	sys := t.TempDir()
	port := func(name, index, mac, driver, carrier string) map[string]string {
		d := "class/net/" + name + "/"
		return map[string]string{d + "type": "1", d + "ifindex": index, d + "address": mac,
			d + "carrier": carrier, d + "device/driver": "->../../../bus/pci/drivers/" + driver}
	}
	for _, m := range []map[string]string{
		port("enp3s0", "3", "aa:00:00:00:00:03", "r8169", "1"),
		port("enp2s0", "2", "aa:00:00:00:00:02", "r8169", "0"),
		port("wlan0", "4", "aa:00:00:00:00:04", "iwlwifi", "0"),
		{"class/net/wlan0/phy80211": "x", "class/net/lo/type": "772", "class/net/br0/type": "1"},
	} {
		put(t, sys, m)
	}
	ports, err := ReadPorts(sys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports[0].Name != "enp2s0" || ports[1].Name != "enp3s0" ||
		ports[0].Driver != "r8169" || ports[0].Carrier || !ports[1].Carrier || ports[1].MAC != "aa:00:00:00:00:03" {
		t.Errorf("ports: %+v", ports)
	}
}

func p(name string, carrier bool, addr string) Port {
	port := Port{Name: name, MAC: "52:54:00:00:00:0" + name[len(name)-1:], Driver: "r8169", Carrier: carrier}
	if addr != "" {
		port.Addr = netip.MustParsePrefix(addr)
	}
	return port
}

func TestChoosePorts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ports   []Port
		wan     string // "" with err
		lan     string
		extra   string
		lanAddr string
		err     error
	}{
		{"one port", []Port{p("e1", true, "192.168.1.5/24")}, "", "", "", "", ErrOnePort},
		{"no address", []Port{p("e1", true, ""), p("e2", true, "")}, "", "", "", "", ErrWhichWAN},
		{"two addresses", []Port{p("e1", true, "192.168.1.5/24"), p("e2", true, "10.0.0.5/24")}, "", "", "", "", ErrWhichWAN},
		{"modem on the second", []Port{p("e1", false, ""), p("e2", true, "192.168.1.5/24")}, "e2", "e1", "", "192.168.10.1/24", nil},
		{"LAN is the one with a cable", []Port{p("e1", true, "192.168.1.5/24"), p("e2", false, ""), p("e3", true, ""), p("e4", false, "")},
			"e1", "e3", "e2,e4", "192.168.10.1/24", nil},
		{"LAN is the first without cables", []Port{p("e1", true, "192.168.1.5/24"), p("e2", false, ""), p("e3", false, "")},
			"e1", "e2", "e3", "192.168.10.1/24", nil},
		{"modem uses 192.168.10.x", []Port{p("e1", true, "192.168.10.23/24"), p("e2", true, "")}, "e1", "e2", "", "192.168.20.1/24", nil},
		{"modem uses a big network", []Port{p("e1", true, "192.168.0.77/16"), p("e2", true, "")}, "e1", "e2", "", "10.10.0.1/24", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ChoosePorts(tc.ports)
			if err != tc.err {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err != nil {
				return
			}
			var extra []string
			for _, e := range c.Extra {
				extra = append(extra, e.Name)
			}
			if c.WAN.Name != tc.wan || c.LAN.Name != tc.lan || strings.Join(extra, ",") != tc.extra || c.LANAddr.String() != tc.lanAddr {
				t.Errorf("got wan %s lan %s extra %v at %s", c.WAN.Name, c.LAN.Name, extra, c.LANAddr)
			}
		})
	}
	// After asking: the modem's port named, the other address still kept clear of.
	c, err := ChooseWithWAN([]Port{p("e1", true, "192.168.10.5/24"), p("e2", true, "192.168.20.5/24"), p("e3", true, "")}, "e2")
	if err != nil || c.WAN.Name != "e2" || c.LAN.Name != "e1" || c.LANAddr.String() != "10.10.0.1/24" {
		t.Errorf("ChooseWithWAN: %+v %v", c, err)
	}
}

func TestChooseLAN(t *testing.T) {
	all := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if _, err := ChooseLAN(all); err == nil {
		t.Error("ChooseLAN found a network that doesn't overlap everything")
	}
	if got, _ := ChooseLAN(nil); got.String() != "192.168.10.1/24" {
		t.Errorf("ChooseLAN(nil) = %s", got)
	}
	if got, _ := ChooseLAN([]netip.Prefix{netip.MustParsePrefix("192.168.20.9/24"), netip.MustParsePrefix("192.168.10.9/24"),
		netip.MustParsePrefix("10.0.0.0/8")}); got.String() != "172.16.10.1/24" {
		t.Errorf("ChooseLAN = %s", got)
	}
	start, end := DHCPRange(netip.MustParsePrefix("10.10.0.1/24"))
	if start.String() != "10.10.0.100" || end.String() != "10.10.0.199" {
		t.Errorf("DHCPRange = %s %s", start, end)
	}
}

func TestRouterTOML(t *testing.T) {
	ports := []Port{p("e1", true, "192.168.10.23/24"), p("e2", false, ""), p("e3", false, "")}
	c, err := ChoosePorts(ports)
	if err != nil {
		t.Fatal(err)
	}
	data, err := RouterTOML(c, "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Validate(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	text := string(data)
	for _, want := range []string{
		"installer on 2026-11-20", `match_mac = "52:54:00:00:00:01"   # e1, r8169; got 192.168.10.23/24 from the modem`,
		`ipv4 = "192.168.20.1/24"`, `range = ["192.168.20.100", "192.168.20.199"]`, "#   e3  52:54:00:00:00:03  r8169\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("router.toml lacks %q:\n%s", want, text)
		}
	}
	if len(cfg.Interfaces) != 2 || len(cfg.Management.SSHAuthorizedKeys) != 0 || cfg.Management.AllowedZones[0] != "lan" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	// No other ports, no address seen (the person plugged the modem in later).
	c, _ = ChooseWithWAN([]Port{p("e1", true, ""), p("e2", true, "")}, "e1")
	data, err = RouterTOML(c, "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "other ports") || strings.Contains(string(data), "got ") {
		t.Errorf("unexpected text:\n%s", data)
	}
}

func TestAuthorizedKeys(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample me@laptop"
	got, err := AuthorizedKeys([]byte("\xef\xbb\xbf" + key + "\r\n\r\n# comment\r\necdsa-sha2-nistp256 AAAAE2 x\r\n"))
	if err != nil || string(got) != key+"\necdsa-sha2-nistp256 AAAAE2 x\n" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "\r\n", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n", "hello world", "ssh-ed25519"} {
		if _, err := AuthorizedKeys([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestZstdContentSize(t *testing.T) {
	magic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	h8 := append(append([]byte{}, magic...), 0xc0, 0x58) // 8-byte size, a window byte
	h8 = binary.LittleEndian.AppendUint64(h8, 8<<30)
	for _, tc := range []struct {
		h    []byte
		want int64
	}{
		{h8, 8 << 30},
		{append(append([]byte{}, magic...), 0x20, 200), 200},             // single segment, 1 byte
		{append(append([]byte{}, magic...), 0x60, 0x10, 0x00), 256 + 16}, // 2 bytes, +256
		{append(append([]byte{}, magic...), 0x00, 0x58), 0},              // not recorded
		{[]byte("not zstd at all"), 0},
	} {
		if got := zstdContentSize(tc.h); got != tc.want {
			t.Errorf("zstdContentSize(% x) = %d, want %d", tc.h, got, tc.want)
		}
	}
}

// fakeImage is a small disk image with a GPT whose second entry is the
// router's root partition, at sector 40.
func fakeImage(size int) []byte {
	img := make([]byte, size)
	copy(img[512:], "EFI PART")
	binary.LittleEndian.PutUint64(img[512+72:], 2) // entries at LBA 2
	binary.LittleEndian.PutUint32(img[512+80:], 128)
	binary.LittleEndian.PutUint32(img[512+84:], 128)
	copy(img[1024+128:], linuxRootX86_64)
	binary.LittleEndian.PutUint64(img[1024+128+32:], 40)
	for i := 40 * 512; i < size; i += 7 {
		img[i] = byte(i)
	}
	return img
}

func TestWriteImage(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("no zstd")
	}
	dir := t.TempDir()
	img := fakeImage(9 << 20)
	sum := sha256.Sum256(img)
	if err := os.WriteFile(filepath.Join(dir, "img"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("zstd", "-q", filepath.Join(dir, "img"), "-o", filepath.Join(dir, "img.zst")).CombinedOutput(); err != nil {
		t.Fatalf("zstd: %v %s", err, out)
	}
	disk := filepath.Join(dir, "disk")
	old := bytes.Repeat([]byte{0xee}, 16<<20) // an earlier system's leftovers
	if err := os.WriteFile(disk, old, 0o644); err != nil {
		t.Fatal(err)
	}
	var steps []string
	err := WriteImage(disk, filepath.Join(dir, "img.zst"), hex.EncodeToString(sum[:]), func(step string, done, total int64) {
		if done == total {
			steps = append(steps, step+":"+itoa(total))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(disk)
	if !bytes.Equal(got[:len(img)], img) {
		t.Error("the disk doesn't hold the image")
	}
	if !bytes.Equal(got[len(got)-1<<20:], make([]byte, 1<<20)) || got[len(img)] != 0xee {
		t.Error("only the disk's last MiB should be cleared")
	}
	if strings.Join(steps, " ") != "Writing:9437184 Checking:9437184" {
		t.Errorf("progress: %v", steps)
	}
	if off, err := RootOffset(disk); err != nil || off != 40*512 {
		t.Errorf("RootOffset = %d, %v", off, err)
	}
	// A damaged stick: the checksum doesn't match.
	if err := WriteImage(disk, filepath.Join(dir, "img.zst"), strings.Repeat("0", 64), nil); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("a wrong checksum: %v", err)
	}
	// Too small a disk.
	if err := os.Truncate(disk, 4<<20); err != nil {
		t.Fatal(err)
	}
	if err := WriteImage(disk, filepath.Join(dir, "img.zst"), hex.EncodeToString(sum[:]), nil); err == nil {
		t.Error("an image bigger than the disk was written")
	}
}

// fakeComputer is a fake root (/sys, /proc) with UEFI, 4 GB, two ports and
// one disk, and a stick with an SSH key.
func fakeComputer(t *testing.T) (root, stick string) {
	root, stick = t.TempDir(), t.TempDir()
	put(t, root, map[string]string{
		"sys/firmware/efi/":     "",
		"proc/meminfo":          "MemTotal:        4000000 kB\n",
		"proc/cpuinfo":          "processor\t: 0\nmodel name\t: Intel(R) Core(TM) i5-3470\nprocessor\t: 1\n",
		"proc/self/mountinfo":   "30 22 8:17 / /run/rollgate/stick rw - vfat /dev/sdb1 rw\n",
		"sys/class/net/e1/type": "1", "sys/class/net/e1/ifindex": "2", "sys/class/net/e1/address": "52:54:00:00:00:01",
		"sys/class/net/e1/device/driver": "->r8169",
		"sys/class/net/e2/type":          "1", "sys/class/net/e2/ifindex": "3", "sys/class/net/e2/address": "52:54:00:00:00:02",
		"sys/class/net/e2/device/driver": "->r8169",
	})
	fakeDisk(t, filepath.Join(root, "sys"), "sda", "ata1", 120, "0")
	fakeDisk(t, filepath.Join(root, "sys"), "sdb", "usb1", 8, "1", "sdb1")
	put(t, stick, map[string]string{"rollgate/ssh-key.pub": "ssh-ed25519 AAAAC3Nza me@laptop\r\n"})
	return root, stick
}

type fakeRun struct {
	writes   []string
	finished []File
}

func newInstaller(root, stick string, f *fakeRun, typed ...string) (*Installer, *bytes.Buffer) {
	lines := make(chan string, len(typed))
	for _, l := range typed {
		lines <- l
	}
	close(lines)
	var out bytes.Buffer
	return &Installer{
		Out: &out, Lines: lines, Release: "2026-11-16", Root: root, Stick: stick, Image: "/img.zst", Sum: "abc",
		Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
		Probe: func(ports []Port, _ io.Writer) ([]Port, error) {
			out := append([]Port{}, ports...)
			for i := range out {
				out[i].Carrier = true
				if out[i].Name == "e1" {
					out[i].Addr = netip.MustParsePrefix("10.0.2.15/24")
				}
			}
			return out, nil
		},
		WaitLink: func([]Port) (string, error) { panic("not asked") },
		Write: func(dev, src, sum string, progress func(string, int64, int64)) error {
			f.writes = append(f.writes, dev+" "+src+" "+sum)
			progress("Writing", 5, 10)
			progress("Writing", 10, 10)
			return nil
		},
		Finish: func(dev string, files []File) error { f.finished = files; return nil },
		Run:    func(string, ...string) ([]byte, error) { return []byte("00:02.0 Ethernet controller: Realtek\n"), nil },
	}, &out
}

func TestInstallWritesOnlyAfterErase(t *testing.T) {
	root, stick := fakeComputer(t)
	var f fakeRun
	in, out := newInstaller(root, stick, &f, "yes", "", "ERASE", "erase")
	if err := in.Install(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(out.String(), "Nothing was written."); n != 3 {
		t.Errorf("refused %d times, want 3:\n%s", n, out)
	}
	if len(f.writes) != 1 || f.writes[0] != filepath.Join(root, "dev/sda")+" /img.zst abc" {
		t.Errorf("writes: %v", f.writes)
	}
	for _, want := range []string{"The disk: sda  120 GB  Disk SDA", "Internet (WAN):     e1  52:54:00:00:00:01  r8169, got 10.0.2.15",
		"Home network (LAN): e2  52:54:00:00:00:02  r8169, the router will be 192.168.10.1", "Writing: 50%", "Writing: 100%", "rollgate is installed on sda."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(f.finished) != 2 || f.finished[0].Path != "/etc/rollgate/router.toml" || f.finished[1].Path != "/root/.ssh/authorized_keys" ||
		string(f.finished[1].Data) != "ssh-ed25519 AAAAC3Nza me@laptop\n" || f.finished[1].Mode != 0o600 || f.finished[1].DirMode != 0o700 {
		t.Errorf("files: %+v", f.finished)
	}
	if _, err := Validate(f.finished[0].Data); err != nil {
		t.Error(err)
	}
	logText, _ := os.ReadFile(filepath.Join(stick, "rollgate", LogFile))
	hw, _ := os.ReadFile(filepath.Join(stick, "rollgate", HardwareFile))
	if !strings.Contains(string(logText), "Type erase and press Enter to install rollgate on it: erase\r\n") {
		t.Errorf("install-log.txt:\n%s", logText)
	}
	for _, want := range []string{"Intel(R) Core(TM) i5-3470 (2 threads)", "Memory:   4.1 GB", "sda  120 GB  Disk SDA: can be installed on",
		"sdb  8.0 GB  Disk SDB: in use", "e2  52:54:00:00:00:02  r8169  cable in", "Realtek\r\n"} {
		if !strings.Contains(string(hw), want) {
			t.Errorf("hardware.txt lacks %q:\n%s", want, hw)
		}
	}
}

func TestInstallStops(t *testing.T) {
	root, stick := fakeComputer(t)
	var f fakeRun
	// Nobody types erase.
	in, out := newInstaller(root, stick, &f, "no")
	if err := in.Install(); err == nil || len(f.writes) != 0 {
		t.Errorf("err %v, writes %v\n%s", err, f.writes, out)
	}
	// Started in Legacy mode.
	if err := os.Remove(filepath.Join(root, "sys/firmware/efi")); err != nil {
		t.Fatal(err)
	}
	in, _ = newInstaller(root, stick, &f, "erase")
	if err := in.Install(); err == nil || !strings.Contains(err.Error(), "turn off Legacy/CSM") || len(f.writes) != 0 {
		t.Errorf("Legacy boot: %v", err)
	}
	logText, _ := os.ReadFile(filepath.Join(stick, "rollgate", LogFile))
	if !strings.Contains(string(logText), "The installer stopped: this computer started in Legacy") {
		t.Errorf("install-log.txt:\n%s", logText)
	}
	put(t, root, map[string]string{"sys/firmware/efi/": ""})
	// A router.toml on the stick that doesn't validate.
	put(t, stick, map[string]string{"rollgate/router.toml": "schema_version = 1\n"})
	in, out = newInstaller(root, stick, &f, "erase")
	if err := in.Install(); err == nil || !strings.Contains(err.Error(), "isn't valid") || len(f.writes) != 0 {
		t.Errorf("invalid router.toml: %v\n%s", err, out)
	}
	// One that names a key file the stick lacks; then with it.
	example, err := os.ReadFile("../../examples/router.toml")
	if err != nil {
		t.Fatal(err)
	}
	put(t, stick, map[string]string{"rollgate/router.toml": string(example)})
	in, _ = newInstaller(root, stick, &f, "erase")
	if err := in.Install(); err == nil || !strings.Contains(err.Error(), "admin.pub") {
		t.Errorf("missing key file: %v", err)
	}
	put(t, stick, map[string]string{"rollgate/admin.pub": "ssh-ed25519 AAAAadmin"})
	in, out = newInstaller(root, stick, &f, "erase")
	if err := in.Install(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.finished) != 3 || f.finished[0].Path != "/etc/rollgate/secrets/admin.pub" || !bytes.Equal(f.finished[1].Data, example) {
		t.Errorf("files: %+v", f.finished)
	}
	// One port only.
	if err := os.RemoveAll(filepath.Join(root, "sys/class/net/e2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stick, "rollgate/router.toml")); err != nil {
		t.Fatal(err)
	}
	f = fakeRun{}
	in, _ = newInstaller(root, stick, &f, "erase")
	if err := in.Install(); err != ErrOnePort || len(f.writes) != 0 {
		t.Errorf("one port: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestInstallAsksForTheModem(t *testing.T) {
	root, stick := fakeComputer(t)
	var f fakeRun
	in, out := newInstaller(root, stick, &f, "erase")
	probes := 0
	// The first round: no port gets an address (the modem's cable is out).
	// After the cable goes into e2, asking e2 again gets one.
	in.Probe = func(ports []Port, _ io.Writer) ([]Port, error) {
		probes++
		res := append([]Port{}, ports...)
		for i := range res {
			res[i].Carrier = true
			if probes > 1 && res[i].Name == "e2" {
				res[i].Addr = netip.MustParsePrefix("192.168.10.40/24")
			}
		}
		return res, nil
	}
	in.WaitLink = func([]Port) (string, error) { return "e2", nil }
	if err := in.Install(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"No port got an address from a modem.", "Plug the cable from your modem", "The modem is on e2.",
		"Internet (WAN):     e2", "Home network (LAN): e1  52:54:00:00:00:01  r8169, the router will be 192.168.20.1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if probes != 2 || len(f.writes) != 1 {
		t.Errorf("probes %d, writes %v", probes, f.writes)
	}
}
