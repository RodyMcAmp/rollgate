package apply

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// keptDir holds state the compiler reads back, such as the local IPv6
// prefix. Install writes files there but never removes them, so a revert
// can't make the router pick a new prefix (docs/config-spec.md, ipv6 auto).
const keptDir = "etc/rollgate/state/"

// Capture copies the files currently installed below root at the paths a
// compiled set would write or remove: generation 0, the state before the
// first apply (ADR 0010). Paths with no file are left out, so restoring
// generation 0 removes whatever the first apply added there.
func Capture(root string, want compile.Files) (compile.Files, error) {
	owned, err := compile.Owned(root)
	if err != nil {
		return nil, err
	}
	paths := append(want.Paths(), owned...)
	base := compile.Files{}
	for _, p := range paths {
		if err := checkPath(p); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		base[p] = data
	}
	return base, nil
}

// Install makes the files below root match want and returns the paths it
// changed, sorted, for deciding what to reload. prev is the generation being
// replaced: its files that want doesn't have are removed, as are installed
// rollgate files (compile.Owned) that want doesn't have. Files whose content
// already matches are left alone. Each file is written to a temporary name,
// synced and renamed into place, so every file is either old or new, never
// half-written, even after a power cut.
//
// Install stops at the first error. The caller then reverts by installing
// the previous generation, which works from any mix of old and new files.
func Install(root string, want, prev compile.Files) ([]string, error) {
	var changed []string
	for _, p := range want.Paths() {
		if err := checkPath(p); err != nil {
			return changed, err
		}
		target := filepath.Join(root, filepath.FromSlash(p))
		if installed, err := os.ReadFile(target); err == nil && bytes.Equal(installed, want[p]) {
			continue
		}
		if err := writeFile(target, want[p]); err != nil {
			return changed, err
		}
		changed = append(changed, p)
	}

	owned, err := compile.Owned(root)
	if err != nil {
		return changed, err
	}
	remove := append(prev.Paths(), owned...)
	slices.Sort(remove)
	for _, p := range slices.Compact(remove) {
		if want[p] != nil || strings.HasPrefix(p, keptDir) {
			continue
		}
		if err := checkPath(p); err != nil {
			return changed, err
		}
		target := filepath.Join(root, filepath.FromSlash(p))
		err := os.Remove(target)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return changed, err
		}
		if err := syncDir(filepath.Dir(target)); err != nil {
			return changed, err
		}
		changed = append(changed, p)
	}
	slices.Sort(changed)
	return changed, nil
}

// checkPath refuses paths that would leave the root, so a damaged
// generation can't write outside the directories rollgate manages.
func checkPath(p string) error {
	if !filepath.IsLocal(filepath.FromSlash(p)) || strings.Contains(p, `\`) {
		return fmt.Errorf("refusing unsafe path %q", p)
	}
	return nil
}

// writeFile writes data to path atomically and durably: a temporary file in
// the same directory, synced, renamed over path, then the directory synced.
func writeFile(path string, data []byte) error {
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
	if err == nil {
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
		err = syncDir(dir)
	}
	return err
}

// syncDir makes a rename or removal in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
