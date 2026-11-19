package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
)

const (
	testUUID  = "83e5b60d-7152-4cd6-8c78-0f15ddf0cdbe"
	imageList = "Server = https://archive.archlinux.org/repos/2026/11/16/$repo/os/$arch\n"
	imageTab  = "UUID=" + testUUID + "  /                  btrfs  rw,noatime,compress=zstd:3,subvol=/@               0 0\n" +
		"UUID=" + testUUID + "  /var/log           btrfs  rw,noatime,compress=zstd:3,subvol=/@var_log        0 0\n"
	cmdline = "BOOT_IMAGE=/@/boot/vmlinuz-linux root=UUID=" + testUUID + " rw rootflags=subvol=@ console=tty0 console=ttyS0,115200 loglevel=4\n"
)

// fakeSystem is a router below a temporary root. The Btrfs top level is a
// plain directory: "mount" does nothing, "btrfs subvolume snapshot" copies.
type fakeSystem struct {
	t        *testing.T
	root     string
	cmds     []string
	subvol   string
	free     uint64
	pacman   func(next string) error // what pacman -Syu does to @next
	stateAt  string                  // update.json's state when grub-editenv ran
	verified bool
	rootid   string // btrfs inspect-internal rootid /
}

func newFake(t *testing.T) *fakeSystem {
	f := &fakeSystem{t: t, root: t.TempDir(), subvol: "/@", free: 10 << 30}
	f.pacman = func(next string) error { return nil }
	f.write(MirrorlistPath, imageList)
	f.write("proc/cmdline", cmdline)
	f.write(KeysPath, ourKey+"\n")
	f.write("efi/grub/grubenv", "# GRUB Environment Block\n")
	// @ at the top of the filesystem: what / is.
	at := filepath.Join(topMount, "@")
	f.write(filepath.Join(at, MirrorlistPath), imageList)
	f.write(filepath.Join(at, fstabPath), imageTab)
	for _, b := range []string{"vmlinuz-linux", "initramfs-linux.img", "intel-ucode.img", "amd-ucode.img"} {
		f.write(filepath.Join(at, "boot", b), b)
	}
	return f
}

func (f *fakeSystem) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSystem) read(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *fakeSystem) Run(name string, args ...string) ([]byte, error) {
	cmd := strings.ReplaceAll(name+" "+strings.Join(args, " "), f.root, "ROOT")
	f.cmds = append(f.cmds, cmd)
	switch {
	case name == "findmnt":
		return []byte(fmt.Sprintf("/dev/vda2[%s] %s %s btrfs\n", f.subvol, f.subvol, testUUID)), nil
	case name == "btrfs" && args[1] == "snapshot":
		return nil, os.CopyFS(args[3], os.DirFS(args[2]))
	case name == "btrfs" && args[1] == "delete":
		return nil, os.RemoveAll(args[2])
	case name == "grub-editenv":
		st, _ := LoadState(f.root)
		f.stateAt = st.State
	case name == "btrfs" && args[0] == "inspect-internal":
		return []byte(f.rootid + "\n"), nil
	case name == "mv" && args[0] == "--exchange":
		a, b := args[2], args[3]
		if err := os.Rename(a, a+".swap"); err != nil {
			return nil, err
		}
		if err := os.Rename(b, a); err != nil {
			return nil, err
		}
		return nil, os.Rename(a+".swap", b)
	}
	return nil, nil
}

func (f *fakeSystem) system() System {
	return System{
		Root: f.root, Run: f, Out: io.Discard,
		Stream: func(w io.Writer, name string, args ...string) error {
			f.cmds = append(f.cmds, strings.ReplaceAll(name+" "+strings.Join(args, " "), f.root, "ROOT"))
			return f.pacman(args[0])
		},
		Now: func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) },
		Get: func(url string) ([]byte, error) {
			return map[string][]byte{"R": []byte(sample), "R.sig": []byte("SIG")}[url], nil
		},
		URL:  "R",
		Free: func(string) (uint64, error) { return f.free, nil },
		Verify: func(sig, data string) ([]byte, error) {
			f.verified = true
			return []byte("[GNUPG:] GOODSIG x\n" + validsig(ourKey, ourKey)), nil
		},
	}
}

func TestPrepare(t *testing.T) {
	f := newFake(t)
	if err := Prepare(f.system(), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"findmnt -n -r -o SOURCE,FSROOT,UUID,FSTYPE /",
		"mount -o subvolid=5 /dev/vda2 ROOT/run/rollgate/btrfs",
		"btrfs subvolume snapshot ROOT/run/rollgate/btrfs/@ ROOT/run/rollgate/btrfs/@next",
		"arch-chroot ROOT/run/rollgate/btrfs/@next env SNAP_PAC_SKIP=y pacman -Syu --noconfirm",
		"grub-editenv ROOT/efi/grub/grubenv set next_entry=rollgate-trial",
		"umount ROOT/run/rollgate/btrfs",
	}
	if strings.Join(f.cmds, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	next := filepath.Join(topMount, "@next")
	if got := f.read(filepath.Join(next, MirrorlistPath)); !strings.Contains(got, "\nServer = https://archive.archlinux.org/repos/2026/11/23/$repo/os/$arch\n") {
		t.Errorf("@next mirrorlist:\n%s", got)
	}
	if got := f.read(filepath.Join(next, fstabPath)); !strings.Contains(got, "compress=zstd:3,subvol=/@next ") || !strings.Contains(got, "subvol=/@var_log") {
		t.Errorf("@next fstab:\n%s", got)
	}
	// @ itself is untouched.
	if f.read(filepath.Join(topMount, "@", MirrorlistPath)) != imageList || f.read(filepath.Join(topMount, "@", fstabPath)) != imageTab {
		t.Error("the running system's files were changed")
	}
	entry := f.read("efi/grub/custom.cfg")
	for _, s := range []string{
		"menuentry 'rollgate update trial (2026-11-23)' --id rollgate-trial {",
		"search --no-floppy --fs-uuid --set=root " + testUUID,
		"\tlinux /@next/boot/vmlinuz-linux root=UUID=" + testUUID + " rw rootflags=subvol=@next console=tty0 console=ttyS0,115200 loglevel=4 panic=10\n",
		"\tinitrd /@next/boot/intel-ucode.img /@next/boot/amd-ucode.img /@next/boot/initramfs-linux.img\n",
	} {
		if !strings.Contains(entry, s) {
			t.Errorf("custom.cfg lacks %q:\n%s", s, entry)
		}
	}
	st, err := LoadState(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if st != (State{State: Trial, Date: "2026-11-23", From: "2026-11-16", Started: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}) {
		t.Errorf("update.json: %+v", st)
	}
	if f.stateAt != Trial {
		t.Errorf("the trial was armed before it was recorded (state %q)", f.stateAt)
	}
}

func TestPrepareLeftoverNext(t *testing.T) {
	f := newFake(t)
	f.write(filepath.Join(topMount, "@next", "stale"), "x")
	if err := Prepare(f.system(), "2026-11-16"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "btrfs subvolume delete ROOT/run/rollgate/btrfs/@next\nbtrfs subvolume snapshot") {
		t.Errorf("leftover @next not deleted first:\n%s", strings.Join(f.cmds, "\n"))
	}
	if _, err := os.Stat(filepath.Join(f.root, topMount, "@next", "stale")); err == nil {
		t.Error("leftover @next content survived")
	}
}

func TestPrepareUpToDate(t *testing.T) {
	f := newFake(t)
	f.write(MirrorlistPath, strings.ReplaceAll(imageList, "11/16", "11/23"))
	if err := Prepare(f.system(), ""); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("got %v, want ErrUpToDate", err)
	}
	if len(f.cmds) != 1 { // findmnt only
		t.Errorf("commands run: %q", f.cmds)
	}
}

// TestPrepareRefuses: each refusal happens before anything is downloaded,
// mounted or written.
func TestPrepareRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, err string
		setup     func(f *fakeSystem)
	}{
		{"config waiting for confirmation", "waiting for rollgate confirm", func(f *fakeSystem) {
			if err := (apply.Store{Root: f.root}).SetPending(3); err != nil {
				t.Fatal(err)
			}
		}},
		{"trial in progress", "hasn't finished its trial", func(f *fakeSystem) {
			f.write(StatePath, `{"state": "trial", "date": "2026-11-23"}`)
		}},
		{"booted from a snapshot", "not @", func(f *fakeSystem) { f.subvol = "/@snapshots/12/snapshot" }},
		{"booted from @previous", "not @", func(f *fakeSystem) { f.subvol = "/@previous" }},
		{"disk full", "2048 MiB", func(f *fakeSystem) { f.free = 1 << 30 }},
		{"odd kernel option", "kernel option", func(f *fakeSystem) { f.write("proc/cmdline", cmdline+` foo="a b"`) }},
		{"no release key", "no release signing key", func(f *fakeSystem) { f.write(KeysPath, "# none yet\n") }},
	} {
		f := newFake(t)
		tc.setup(f)
		err := Prepare(f.system(), "")
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
			continue
		}
		for _, c := range f.cmds {
			if !strings.HasPrefix(c, "findmnt ") {
				t.Errorf("%s: ran %q", tc.name, c)
			}
		}
		if f.verified && tc.name != "no release key" {
			t.Errorf("%s: downloaded releases.json first", tc.name)
		}
		if _, err := os.Stat(filepath.Join(f.root, topMount, "@next")); err == nil {
			t.Errorf("%s: @next created", tc.name)
		}
	}
}

func TestPrepareFailedUpdate(t *testing.T) {
	for name, pacman := range map[string]func(next string) error{
		"pacman fails": func(string) error { return errors.New("exit status 1") },
		"no kernel":    func(next string) error { return os.Remove(filepath.Join(next, "boot", "vmlinuz-linux")) },
	} {
		f := newFake(t)
		f.pacman = pacman
		err := Prepare(f.system(), "")
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("%s: err %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(f.root, topMount, "@next")); err == nil {
			t.Errorf("%s: @next not deleted", name)
		}
		if _, err := os.Stat(filepath.Join(f.root, StatePath)); err == nil {
			t.Errorf("%s: update.json written", name)
		}
		cmds := strings.Join(f.cmds, "\n")
		if strings.Contains(cmds, "grub-editenv") || !strings.HasSuffix(cmds, "umount ROOT/run/rollgate/btrfs") {
			t.Errorf("%s: commands:\n%s", name, cmds)
		}
	}
}

func TestSetRootSubvol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(path, []byte("# comment / btrfs subvol=/@\n"+imageTab), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setRootSubvol(path, "/@next"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if want := "# comment / btrfs subvol=/@\n" + strings.Replace(imageTab, "subvol=/@ ", "subvol=/@next ", 1); string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
	if err := os.WriteFile(path, []byte("UUID=x / btrfs rw,noatime 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setRootSubvol(path, "/@next"); err == nil {
		t.Error("fstab without subvol= accepted")
	}
}
