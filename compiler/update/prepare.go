package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/templates"
)

// Paths relative to the root.
const (
	StatePath = "var/lib/rollgate/update.json"
	grubDir   = "efi/grub" // GRUB's directory on the ESP (ADR 0009)
	topMount  = "run/rollgate/btrfs"
	fstabPath = "etc/fstab"
)

// TrialEntry is the GRUB menu entry id of the trial boot.
const TrialEntry = "rollgate-trial"

// minFree is the free space rollgate update needs on the root filesystem.
const minFree = 2 << 30

// Update states in update.json.
const (
	Trial       = "trial"        // @next is prepared and its one trial boot is armed or running
	TrialFailed = "trial-failed" // the trial's checks failed; the next boot of @ cleans up
	Promoting   = "promoting"    // the checks passed and @next is becoming @
	Promoted    = "promoted"     // the update is the system
	Failed      = "failed"       // the update didn't become the system; @next is gone
)

// State is update.json: the last update and how far it got. It lives in
// /var/lib/rollgate, which every root shares, so a trial boot and the next
// boot of @ both see it.
type State struct {
	State    string    `json:"state"`
	Date     string    `json:"date"` // the release being installed
	From     string    `json:"from"` // the release being left
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Reason   string    `json:"reason,omitempty"`  // why it failed
	NextID   string    `json:"next_id,omitempty"` // @next's subvolume ID while promoting
}

// unfinished says whether st is an update that hasn't promoted or failed yet.
func (st State) unfinished() bool {
	return st.State == Trial || st.State == TrialFailed || st.State == Promoting
}

// LoadState reads update.json below root. A missing file is the zero State.
func LoadState(root string) (State, error) {
	var st State
	data, err := os.ReadFile(filepath.Join(root, StatePath))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("/%s: %w", StatePath, err)
	}
	return st, nil
}

func saveState(root string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(root, StatePath), append(data, '\n'), true)
}

// System is what rollgate update works on. The real one has root "/".
type System struct {
	Root   string
	Run    apply.Runner                                         // short commands; output returned
	Stream func(w io.Writer, name string, args ...string) error // long commands (pacman); output shown as it runs
	Out    io.Writer
	Now    func() time.Time
	Sleep  func(time.Duration)
	Get    Get
	Verify Verify
	URL    string                            // releases.json
	Free   func(path string) (uint64, error) // bytes available to root
}

// StreamExec runs a program with its output going to w.
func StreamExec(w io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// FreeBytes returns the space available to unprivileged users on path's
// filesystem; what is reserved for root isn't counted.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

var tmpl = template.Must(template.New("").Option("missingkey=error").ParseFS(templates.FS, "grub-custom.tmpl", "mirrorlist.tmpl"))

func render(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Prepare is rollgate update [DATE] (ADR 0011): choose the date, snapshot @
// as @next, update @next in a chroot, arm one trial boot of it and record
// the trial. Nothing in @ changes. It returns ErrUpToDate (after saying so)
// when there's nothing to update; the caller reboots on success.
func Prepare(s System, want string) error {
	unlock, err := apply.Lock(s.Root)
	if err != nil {
		return err
	}
	defer unlock()

	// Refuse first, before downloading anything (ADR 0011, step 2).
	running, err := RunningRelease(s.Root)
	if err != nil {
		return err
	}
	if n, ok, err := (apply.Store{Root: s.Root}).Pending(); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("config generation %d is waiting for rollgate confirm; confirm or revert it first", n)
	}
	st, err := LoadState(s.Root)
	if err != nil {
		return err
	}
	if st.unfinished() {
		return fmt.Errorf("the update to %s hasn't finished its trial yet", st.Date)
	}
	m, err := rootMount(s)
	if err != nil {
		return err
	}
	if m.subvol != "/@" {
		return fmt.Errorf("the running system is %s, not @: update only from the normal system (booted from a snapshot or the previous system?)", m.subvol)
	}
	free, err := s.Free(s.Root)
	if err != nil {
		return err
	}
	if free < minFree {
		return fmt.Errorf("only %d MiB free; an update needs 2048 MiB", free>>20)
	}
	options, err := kernelOptions(s.Root)
	if err != nil {
		return err
	}

	keys, err := ReadKeys(s.Root)
	if err != nil {
		return err
	}
	rels, err := FetchReleases(s.Get, s.Verify, keys, s.URL)
	if err != nil {
		return err
	}
	date, err := rels.Choose(running, want)
	if errors.Is(err, ErrUpToDate) {
		fmt.Fprintf(s.Out, "Already on the newest release, %s.\n", running)
		return err
	}
	if err != nil {
		return err
	}

	// Prepare @next at the top of the Btrfs filesystem (ADR 0011, step 3).
	top := filepath.Join(s.Root, topMount)
	if err := os.MkdirAll(top, 0o700); err != nil {
		return err
	}
	if err := s.run("mount", "-o", "subvolid=5", m.device, top); err != nil {
		return err
	}
	defer func() { _ = s.run("umount", top) }()
	next := filepath.Join(top, "@next")
	if _, err := os.Stat(next); err == nil {
		fmt.Fprintln(s.Out, "Deleting @next, left over from an earlier update.")
		if err := s.run("btrfs", "subvolume", "delete", next); err != nil {
			return err
		}
	}
	fmt.Fprintf(s.Out, "Preparing the update from %s to %s in @next. The running system isn't changed.\n", running, date)
	if err := s.run("btrfs", "subvolume", "snapshot", filepath.Join(top, "@"), next); err != nil {
		return err
	}
	if err := updateNext(s, next, date, m, options); err != nil {
		if derr := s.run("btrfs", "subvolume", "delete", next); derr != nil {
			err = errors.Join(err, derr)
		}
		return fmt.Errorf("%w; @next was deleted and nothing was changed", err)
	}

	// Record the trial, then arm it (ADR 0011, steps 5 and 6). In this order,
	// a crash in between leaves a recorded trial that never booted, which the
	// next boot of @ cleans up; never a trial nobody knows about.
	if err := saveState(s.Root, State{State: Trial, Date: date, From: running, Started: s.Now().UTC()}); err != nil {
		return err
	}
	if err := s.run("grub-editenv", filepath.Join(s.Root, grubDir, "grubenv"), "set", "next_entry="+TrialEntry); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "The update to %s is ready. Rebooting into it once, as a trial.\n", date)
	fmt.Fprintf(s.Out, "If its health checks don't pass within 5 minutes, the router goes back to %s by itself.\n", running)
	return nil
}

// updateNext updates the snapshot at next to date and writes its trial boot
// entry (ADR 0011, steps 4 and 5, before arming).
func updateNext(s System, next, date string, m mount, options []string) error {
	mirrorlist, err := render("mirrorlist.tmpl", map[string]string{"Date": date, "Path": strings.ReplaceAll(date, "-", "/")})
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(next, MirrorlistPath), mirrorlist, true); err != nil {
		return err
	}
	if err := pacmanUpgrade(s, m); err != nil {
		return err
	}
	trial, err := bootEntry(filepath.Dir(next), "@next", "rollgate update trial ("+date+")", TrialEntry, m.uuid, options, true)
	if err != nil {
		return fmt.Errorf("after the update: %w", err)
	}
	if err := setRootSubvol(filepath.Join(next, fstabPath), "/@next"); err != nil {
		return err
	}
	return writeCustom(s.Root, filepath.Dir(next), trial, m.uuid, options)
}

// nextMount is where @next is mounted on its own for the chroot.
const nextMount = "run/rollgate/next"

// pacmanUpgrade runs pacman -Syu inside @next. @next is mounted on its own,
// not used as a directory below the top-level mount: pacman checks free space
// per mount point, and inside the chroot / must be one.
func pacmanUpgrade(s System, m mount) error {
	dir := filepath.Join(s.Root, nextMount)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := s.run("mount", "-o", "subvol=/@next", m.device, dir); err != nil {
		return err
	}
	err := downloadAndInstall(s, dir)
	if uerr := s.run("umount", dir); err == nil {
		err = uerr
	}
	return err
}

// downloadAttempts bounds the retries of a stalled download.
const downloadAttempts = 3

// downloadAndInstall downloads every package first, retrying: the Arch Linux
// Archive sometimes stalls (image/build.sh retries for the same reason). A
// failed download installs nothing, so a retry is safe. Then it installs from
// the cache.
func downloadAndInstall(s System, dir string) error {
	// The whole root is the snapshot, so snap-pac's own snapshots are skipped.
	pacman := func(args ...string) error {
		return s.Stream(s.Out, "arch-chroot", append([]string{dir, "env", "SNAP_PAC_SKIP=y", "pacman", "--noconfirm"}, args...)...)
	}
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err = pacman("-Syuw"); err == nil {
			break
		}
		if attempt < downloadAttempts {
			fmt.Fprintf(s.Out, "Downloading the update failed (attempt %d of %d); retrying in 30 seconds.\n", attempt, downloadAttempts)
			s.Sleep(30 * time.Second)
		}
	}
	if err != nil {
		return fmt.Errorf("downloading the update failed %d times: %w", downloadAttempts, err)
	}
	if err := pacman("-Su"); err != nil {
		return fmt.Errorf("pacman -Su in @next failed: %w", err)
	}
	return nil
}

// entry is one GRUB menu entry in custom.cfg.
type entry struct {
	Title, ID, Subvol, UUID string
	Options, Initrds        []string
	Panic                   bool // reboot 10 s after a kernel panic (trial boots only)
}

// PreviousEntry is the GRUB menu entry id of the previous system.
const PreviousEntry = "rollgate-previous"

// bootEntry describes booting subvol at the top of the filesystem (top). The
// subvolume must have a kernel and an initramfs.
func bootEntry(top, subvol, title, id, uuid string, options []string, panic bool) (*entry, error) {
	boot := filepath.Join(top, subvol, "boot")
	for _, f := range []string{"vmlinuz-linux", "initramfs-linux.img"} {
		if _, err := os.Stat(filepath.Join(boot, f)); err != nil {
			return nil, fmt.Errorf("%s has no /boot/%s", subvol, f)
		}
	}
	e := &entry{Title: title, ID: id, Subvol: subvol, UUID: uuid, Options: options, Panic: panic}
	for _, f := range []string{"intel-ucode.img", "amd-ucode.img", "initramfs-linux.img"} {
		if _, err := os.Stat(filepath.Join(boot, f)); err == nil {
			e.Initrds = append(e.Initrds, f)
		}
	}
	return e, nil
}

// writeCustom writes GRUB's custom.cfg: the trial entry if there is one,
// and an entry for @previous when it exists. grub.cfg sources custom.cfg from
// GRUB's directory (grub-mkconfig's 41_custom).
func writeCustom(root, top string, trial *entry, uuid string, options []string) error {
	var previous *entry
	if _, err := os.Stat(filepath.Join(top, "@previous")); err == nil {
		date, err := RunningRelease(filepath.Join(top, "@previous"))
		if err != nil {
			date = "unknown release"
		}
		// A previous system that can't boot just gets no entry.
		previous, _ = bootEntry(top, "@previous", "rollgate previous system ("+date+")", PreviousEntry, uuid, options, false)
	}
	data, err := render("grub-custom.tmpl", map[string]*entry{"Trial": trial, "Previous": previous})
	if err != nil {
		return err
	}
	// The ESP is FAT, which has no file modes to set.
	return writeAtomic(filepath.Join(root, grubDir, "custom.cfg"), data, false)
}

func (s System) run(name string, args ...string) error {
	if out, err := s.Run.Run(name, args...); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

type mount struct{ device, subvol, uuid string }

// errNotBtrfs means / isn't a Btrfs subvolume: for example a read-only
// snapshot booted from GRUB's snapshot menu, which runs on an overlay.
var errNotBtrfs = errors.New("/ is not a Btrfs subvolume")

// rootMount finds the Btrfs device, subvolume and filesystem UUID of /.
func rootMount(s System) (mount, error) {
	out, err := s.Run.Run("findmnt", "-n", "-r", "-o", "SOURCE,FSROOT,UUID,FSTYPE", "/")
	if err != nil {
		return mount{}, fmt.Errorf("findmnt /: %v: %s", err, out)
	}
	f := strings.Fields(string(out))
	if len(f) != 4 || f[3] != "btrfs" {
		return mount{}, fmt.Errorf("%w (findmnt: %q)", errNotBtrfs, strings.TrimSpace(string(out)))
	}
	device, _, _ := strings.Cut(f[0], "[") // "/dev/sda2[/@]"
	return mount{device: device, subvol: f[1], uuid: f[2]}, nil
}

// kernelOptions returns the running kernel's options, minus those the trial
// entry sets itself, so the trial boots like the running system.
func kernelOptions(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc/cmdline"))
	if err != nil {
		return nil, err
	}
	var options []string
	for _, o := range strings.Fields(string(data)) {
		key, _, _ := strings.Cut(o, "=")
		switch key {
		case "BOOT_IMAGE", "initrd", "root", "rootflags", "rw", "ro", "panic":
			continue
		}
		// The options go into a GRUB script, where quotes, $ and ; mean
		// something. Real options don't need them.
		if !safeOption.MatchString(o) {
			return nil, fmt.Errorf("can't copy kernel option %q into the trial boot entry", o)
		}
		options = append(options, o)
	}
	return options, nil
}

var safeOption = regexp.MustCompile(`^[A-Za-z0-9_.,:/+=-]+$`)

// setRootSubvol points fstab's / line at subvol.
func setRootSubvol(path, subvol string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	found := 0
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 || strings.HasPrefix(f[0], "#") || f[1] != "/" {
			continue
		}
		opts := strings.Split(f[3], ",")
		for j, o := range opts {
			if strings.HasPrefix(o, "subvol=") {
				opts[j] = "subvol=" + subvol
				found++
			}
		}
		lines[i] = strings.Replace(line, f[3], strings.Join(opts, ","), 1)
	}
	if found != 1 {
		return fmt.Errorf("%s: expected one subvol= option on the / line, found %d", path, found)
	}
	return writeAtomic(path, []byte(strings.Join(lines, "\n")), true)
}

// writeAtomic writes data to path through a synced temporary file renamed
// over it, then syncs the directory. With chmod, the file is 0644.
func writeAtomic(path string, data []byte, chmod bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rollgate-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	_, err = f.Write(data)
	if err == nil && chmod {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		var d *os.File
		if d, err = os.Open(dir); err == nil {
			err = d.Sync()
			if cerr := d.Close(); err == nil {
				err = cerr
			}
		}
	}
	return err
}
