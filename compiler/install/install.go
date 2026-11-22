package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Files on the stick, in its FAT partition's rollgate/ folder.
const (
	StickDir     = "rollgate"
	KeyFile      = "ssh-key.pub"
	ConfigFile   = "router.toml"
	LogFile      = "install-log.txt"
	HardwareFile = "hardware.txt"
	// MinMemory is what the kernel reports on a computer with 2 GB, give or
	// take what the firmware and graphics keep for themselves.
	MinMemory = 1536 << 20
)

// File is a file the installer puts on the installed system.
type File struct {
	Path    string // absolute, on the installed system
	Data    []byte
	Mode    os.FileMode
	DirMode os.FileMode // for directories it has to make
}

// Installer is one run of rollgate install. The function fields touch the
// hardware; tests replace them.
type Installer struct {
	Out     io.Writer     // every console
	Lines   <-chan string // what is typed on any console, a line at a time
	Release string        // the release date of the image, YYYY-MM-DD
	Root    string        // where /sys, /proc and /dev are ("/", or a test's directory)
	Stick   string        // the stick's FAT partition, mounted; "" if it isn't
	Image   string        // the zstd-compressed router image
	Sum     string        // the uncompressed image's SHA-256
	Now     func() time.Time

	// Probe asks each port for a DHCP address, briefly, and returns the
	// ports with Carrier and Addr filled in.
	Probe func(ports []Port, out io.Writer) ([]Port, error)
	// WaitLink returns the port whose link comes up next.
	WaitLink func(ports []Port) (string, error)
	// Write writes the image onto the disk dev (WriteImage).
	Write func(dev, src, sum string, progress func(step string, done, total int64)) error
	// Finish puts files on the installed system on dev (InstallFiles).
	Finish func(dev string, files []File) error
	// Run runs a command for hardware.txt and returns its output.
	Run func(name string, args ...string) ([]byte, error)

	log bytes.Buffer
}

func (in *Installer) say(format string, a ...any) {
	fmt.Fprintf(io.MultiWriter(in.Out, &in.log), format+"\n", a...)
}

func (in *Installer) path(p string) string { return filepath.Join(in.Root, p) }

// ask shows prompt and returns the next line typed.
func (in *Installer) ask(prompt string) (string, error) {
	fmt.Fprint(io.MultiWriter(in.Out, &in.log), prompt)
	line, ok := <-in.Lines
	if !ok {
		return "", errors.New("no keyboard: nothing more can be typed")
	}
	fmt.Fprintln(&in.log, line)
	return strings.TrimSpace(line), nil
}

// Install runs the installer: checks, ports, disk, the typed erase, writing,
// and the reports on the stick. Nothing is written to a disk before erase is
// typed. Whatever happens, it leaves install-log.txt on the stick.
func (in *Installer) Install() (err error) {
	defer func() {
		if err != nil {
			in.say("\nThe installer stopped: %v", err)
		}
		in.saveStick(LogFile, []byte(crlf(in.log.Bytes())))
	}()
	in.say("rollgate installer, release %s (ADR 0021)\n", in.Release)
	in.say("It puts rollgate on this computer's internal disk, and erases everything on it.")
	in.say("Nothing is written until you type erase.\n")

	if !exists(in.path("sys/firmware/efi")) {
		return errors.New("this computer started in Legacy (BIOS) mode. Turn on UEFI boot, and turn off Legacy/CSM, in the BIOS setup, then start from the stick again")
	}
	mem := memTotal(in.path("proc/meminfo"))
	if mem > 0 && mem < MinMemory {
		return fmt.Errorf("rollgate needs at least 2 GB of memory; this computer has %s", Size(mem))
	}

	key, custom, err := in.stickFiles()
	if err != nil {
		return err
	}
	ports, err := ReadPorts(in.path("sys"))
	if err != nil {
		return fmt.Errorf("listing the network ports: %w", err)
	}
	var toml []byte
	var files []File
	if custom != nil {
		in.say("Using router.toml from the USB stick instead of setting up the ports.")
		toml = custom.data
		files = custom.secrets
	} else {
		var choice Choice
		if choice, ports, err = in.choosePorts(ports); err != nil {
			in.saveStick(HardwareFile, in.hardware(ports, nil))
			return err
		}
		if toml, err = RouterTOML(choice, in.Now().UTC().Format("2006-01-02")); err != nil {
			return err
		}
		in.say("\nThe router's ports:")
		wan := fmt.Sprintf("  Internet (WAN):     %s  %s  %s", choice.WAN.Name, choice.WAN.MAC, choice.WAN.Driver)
		if choice.WAN.Addr.IsValid() {
			wan += fmt.Sprintf(", got %s from the modem", choice.WAN.Addr.Addr())
		}
		in.say("%s", wan)
		in.say("  Home network (LAN): %s  %s  %s, the router will be %s", choice.LAN.Name, choice.LAN.MAC, choice.LAN.Driver, choice.LANAddr.Addr())
		for _, p := range choice.Extra {
			in.say("  Not used for now:   %s  %s  %s", p.Name, p.MAC, p.Driver)
		}
	}
	files = append(files, File{Path: "/etc/rollgate/router.toml", Data: toml, Mode: 0o644, DirMode: 0o755})
	if key != nil {
		files = append(files, File{Path: "/root/.ssh/authorized_keys", Data: key, Mode: 0o600, DirMode: 0o700})
		in.say("SSH: the key from the stick's %s will log in as root.", KeyFile)
	} else {
		in.say("SSH: no %s on the stick, so SSH stays closed until a key is added.", KeyFile)
	}

	disks, err := ReadDisks(in.path("sys"), readFile(in.path("proc/self/mountinfo")))
	if err != nil {
		return fmt.Errorf("listing the disks: %w", err)
	}
	in.saveStick(HardwareFile, in.hardware(ports, disks))
	disk, err := in.chooseDisk(disks)
	if err != nil {
		return err
	}
	in.say("\nEverything on %s will be erased.", disk)
	for {
		answer, err := in.ask("Type erase and press Enter to install rollgate on it: ")
		if err != nil {
			return err
		}
		if answer == "erase" {
			break
		}
		in.say("Nothing was written. To stop, turn the computer off and remove the stick.")
	}

	dev := in.path("dev/" + disk.Name)
	in.say("\nInstalling rollgate %s on %s.", in.Release, disk.Name)
	last := map[string]int64{}
	err = in.Write(dev, in.Image, in.Sum, func(step string, done, total int64) {
		if total <= 0 {
			if done/(1<<30) > last[step] {
				last[step] = done / (1 << 30)
				in.say("  %s: %s", step, Size(done))
			}
			return
		}
		if pct := done * 10 / total; pct > last[step] || done == total && last[step] < 10 {
			last[step] = pct
			in.say("  %s: %d%%", step, pct*10)
		}
	})
	if err != nil {
		return err
	}
	if err := in.Finish(dev, files); err != nil {
		return fmt.Errorf("setting up the installed system: %w", err)
	}
	in.say("\nrollgate is installed on %s.", disk.Name)
	in.say("When it starts, it sets itself up and asks you to choose a password.")
	if in.Stick != "" {
		in.say("The stick's rollgate folder now has %s and %s.", LogFile, HardwareFile)
	}
	return nil
}

// custom is a router.toml from the stick, with the key files it names.
type custom struct {
	data    []byte
	secrets []File
}

// stickFiles reads ssh-key.pub and router.toml from the stick, if there.
func (in *Installer) stickFiles() (key []byte, c *custom, err error) {
	if in.Stick == "" {
		in.say("The USB stick's files can't be read, so there are no reports and no SSH key.")
		return nil, nil, nil
	}
	dir := filepath.Join(in.Stick, StickDir)
	if data, err := os.ReadFile(filepath.Join(dir, KeyFile)); err == nil {
		if key, err = AuthorizedKeys(data); err != nil {
			return nil, nil, err
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return key, nil, nil
	}
	cfg, err := Validate(data)
	if err != nil {
		return nil, nil, fmt.Errorf("the stick's router.toml isn't valid (docs/config-spec.md):\n%v", err)
	}
	c = &custom{data: data}
	// Key files it names must come along, or SSH can't start (apply refuses).
	for _, name := range cfg.Management.SSHAuthorizedKeys {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, fmt.Errorf("the stick's router.toml names the SSH key file %s: put it in the stick's rollgate folder too", name)
		}
		if b, err = AuthorizedKeys(b); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		c.secrets = append(c.secrets, File{Path: "/etc/rollgate/secrets/" + name, Data: b, Mode: 0o600, DirMode: 0o700})
	}
	return key, c, nil
}

// choosePorts finds out which port goes to the modem, asking if it must.
func (in *Installer) choosePorts(ports []Port) (Choice, []Port, error) {
	if len(ports) < 2 {
		return Choice{}, ports, ErrOnePort
	}
	in.say("Looking for your modem on each network port (up to 30 seconds)...")
	ports, err := in.Probe(ports, in.Out)
	if err != nil {
		return Choice{}, ports, err
	}
	for _, p := range ports {
		in.say("  %s", p)
	}
	c, err := ChoosePorts(ports)
	if !errors.Is(err, ErrWhichWAN) {
		return c, ports, err
	}
	if n := countAddrs(ports); n > 1 {
		in.say("\n%d ports got an address, so the installer can't tell which one goes to the modem.", n)
	} else {
		in.say("\nNo port got an address from a modem.")
	}
	in.say("Plug the cable from your modem into the port you want for the internet.")
	in.say("If it's plugged in already, unplug it, wait a few seconds, and plug it in again.")
	wan, err := in.WaitLink(ports)
	if err != nil {
		return Choice{}, ports, err
	}
	in.say("The modem is on %s.", wan)
	// Ask the modem again, to learn its network. Addresses other ports got
	// are networks too, which the home network must keep clear of.
	for i, p := range ports {
		if p.Name == wan {
			if again, err := in.Probe([]Port{p}, in.Out); err == nil && len(again) == 1 {
				ports[i].Carrier, ports[i].Addr = again[0].Carrier, again[0].Addr
			}
		}
	}
	c, err = ChooseWithWAN(ports, wan)
	return c, ports, err
}

func countAddrs(ports []Port) int {
	n := 0
	for _, p := range ports {
		if p.Addr.IsValid() {
			n++
		}
	}
	return n
}

// chooseDisk offers the disks the installer may use.
func (in *Installer) chooseDisk(disks []Disk) (Disk, error) {
	offered := Candidates(disks)
	if len(offered) == 0 {
		in.say("\nNo disk to install on. The disks this computer has:")
		for _, d := range disks {
			in.say("  %s: %s", d, d.Why())
		}
		if len(disks) == 0 {
			in.say("  none")
		}
		return Disk{}, errors.New("no internal disk of at least 16 GB was found")
	}
	if len(offered) == 1 {
		in.say("\nThe disk: %s", offered[0])
		return offered[0], nil
	}
	in.say("\nThe disks:")
	for i, d := range offered {
		in.say("  %d) %s", i+1, d)
	}
	for {
		answer, err := in.ask(fmt.Sprintf("Type the number of the disk to install on (1-%d): ", len(offered)))
		if err != nil {
			return Disk{}, err
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(offered) {
			return offered[n-1], nil
		}
	}
}

// saveStick writes name into the stick's rollgate folder, if it can.
func (in *Installer) saveStick(name string, data []byte) {
	if in.Stick == "" {
		return
	}
	path := filepath.Join(in.Stick, StickDir, name)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = writeSync(path, data)
	}
	if err != nil {
		fmt.Fprintf(in.Out, "Couldn't write %s to the stick: %v\n", name, err)
	}
}

// writeSync writes a file and makes sure it is on the disk.
func writeSync(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readFile(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

// memTotal reads MemTotal from /proc/meminfo, in bytes (0 if unknown).
func memTotal(path string) int64 {
	for _, line := range strings.Split(string(readFile(path)), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}
