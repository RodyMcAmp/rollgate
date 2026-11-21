package main

// rollgate install (ADR 0021): the USB installer's program. It runs on the
// installer stick's own system, never on a router.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/install"
)

const (
	// installImages is where the installer's system keeps the router image.
	installImages = "/usr/share/rollgate"
	// installStick is where the installer's system mounts the stick's FAT
	// partition (image/installer.sh writes the fstab line).
	installStick = "/run/rollgate/stick"
)

var installImageName = regexp.MustCompile(`^rollgate-(\d{4}-\d{2}-\d{2})\.img\.zst$`)

func installCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	image := fs.String("image", "", "the router image (default: the one in "+installImages+")")
	stick := fs.String("stick", installStick, "the stick's FAT partition")
	consoles := fs.Bool("console", false, "talk on the kernel's consoles, and restart at the end")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "rollgate install: must run as root")
		return 1
	}
	out, lines := io.Writer(stdout), stdinLines()
	if *consoles {
		out, lines = consoleIO(stdout)
	}

	code := 0
	in := &install.Installer{
		Out: out, Lines: lines, Root: "/", Now: time.Now,
		Probe: install.Probe, WaitLink: install.WaitLink,
		Write: install.WriteImage, Finish: install.InstallFiles,
		Run: func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() },
	}
	if mountPoint(*stick) {
		in.Stick = *stick
	}
	var err error
	if in.Image, in.Sum, in.Release, err = findImage(*image); err != nil {
		fmt.Fprintf(out, "rollgate install: %v\n", err)
		code = 1
	} else if err := in.Install(); err != nil {
		code = 1
	}
	if !*consoles {
		return code
	}
	if code == 0 {
		fmt.Fprint(out, "\nRemove the USB stick, then press Enter to restart.\n")
	} else {
		fmt.Fprint(out, "\nPress Enter to restart.\n")
	}
	<-lines
	if err := exec.Command("systemctl", "reboot").Run(); err != nil {
		fmt.Fprintf(out, "rollgate install: restarting: %v\n", err)
		return 1
	}
	return code
}

// findImage finds the router image and its checksum file next to it.
func findImage(path string) (image, sum, release string, err error) {
	if path == "" {
		matches, _ := filepath.Glob(filepath.Join(installImages, "rollgate-*.img.zst"))
		if len(matches) != 1 {
			return "", "", "", fmt.Errorf("expected one router image in %s, found %d", installImages, len(matches))
		}
		path = matches[0]
	}
	m := installImageName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return "", "", "", fmt.Errorf("%s: expected a name like rollgate-2026-11-16.img.zst", path)
	}
	sum, err = install.ImageSum(strings.TrimSuffix(path, ".zst") + ".sha256")
	return path, sum, m[1], err
}

// consoleIO opens the kernel's consoles (/sys/class/tty/console/active: the
// screen and the serial port): what the installer says goes to all of them,
// and a line typed on any of them is read. With none, it uses stdout and
// standard input.
func consoleIO(stdout io.Writer) (io.Writer, <-chan string) {
	var writers []io.Writer
	lines := make(chan string)
	active, _ := os.ReadFile("/sys/class/tty/console/active")
	for _, name := range strings.Fields(string(active)) {
		if name == "tty0" { // the screen's current terminal
			name = "tty1"
		}
		tty, err := os.OpenFile("/dev/"+name, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			continue
		}
		writers = append(writers, tty)
		go readLines(tty, lines)
	}
	if len(writers) == 0 {
		return stdout, stdinLines()
	}
	return io.MultiWriter(writers...), lines
}

func stdinLines() <-chan string {
	lines := make(chan string)
	go func() {
		readLines(os.Stdin, lines)
		close(lines)
	}()
	return lines
}

func readLines(r io.Reader, lines chan<- string) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		lines <- strings.TrimRight(line, "\r\n")
	}
}

// mountPoint reports whether dir has a file system mounted on it.
func mountPoint(dir string) bool {
	var a, b syscall.Stat_t
	if syscall.Stat(dir, &a) != nil || syscall.Stat(filepath.Join(dir, ".."), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}
