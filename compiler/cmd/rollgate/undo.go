package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
)

// Undo in the web UI puts router.toml back as it was before the change, so
// the pages show what runs again. (rollgate revert at the CLI leaves the
// file alone: there the file is what the user is editing.) An apply from the
// web UI keeps the old file as /var/lib/rollgate/router.toml.before-<N>, N
// being the generation the apply made; Undo restores it, Keep deletes it.

// webUndo finds the files below root.
type webUndo struct {
	root   string
	config string // router.toml's path below root
}

func (u webUndo) configPath() string { return filepath.Join(u.root, u.config) }

func (u webUndo) saved(n int) string {
	return filepath.Join(u.root, filepath.FromSlash(apply.StateDir), "router.toml.before-"+strconv.Itoa(n))
}

// clear deletes copies left by earlier applies (a change the timer undid
// leaves one behind).
func (u webUndo) clear() error {
	old, err := filepath.Glob(filepath.Join(u.root, filepath.FromSlash(apply.StateDir), "router.toml.before-*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range old {
		errs = append(errs, os.Remove(p))
	}
	return errors.Join(errs...)
}

// keep saves old (router.toml before the apply) for generation n's Undo.
func (u webUndo) keep(n int, old []byte) error {
	return os.WriteFile(u.saved(n), old, 0o644)
}

// restore puts back router.toml as it was before generation n, if the file
// still holds what n applied; then it deletes the copy.
func (u webUndo) restore(n int, applied []byte, w io.Writer) error {
	old, err := os.ReadFile(u.saved(n))
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(w, "router.toml still has the undone change: it wasn't applied from the web UI. Advanced shows it.")
		return nil
	}
	if err != nil {
		return err
	}
	defer os.Remove(u.saved(n))
	cur, err := os.ReadFile(u.configPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !bytes.Equal(cur, applied) {
		fmt.Fprintln(w, "router.toml was edited after the change, so it's left as it is. Advanced shows it.")
		return nil
	}
	if err := writeConfig(u.configPath(), old); err != nil {
		return fmt.Errorf("the previous settings are back, but router.toml couldn't be put back: %w", err)
	}
	fmt.Fprintln(w, "router.toml is back as it was before the change.")
	return nil
}

// forget deletes generation n's copy once the change is kept.
func (u webUndo) forget(n int) {
	_ = os.Remove(u.saved(n))
}

// pendingSource returns the generation waiting for confirmation and the
// router.toml it applied, if one waits.
func pendingSource(s apply.System) (int, []byte, bool) {
	p, ok, err := apply.PendingChange(s)
	if err != nil || !ok {
		return 0, nil, false
	}
	g, err := (apply.Store{Root: s.Root}).Load(p.Generation)
	if err != nil {
		return 0, nil, false
	}
	return p.Generation, g.Source, true
}

// webApply writes source as router.toml and applies it with commit-confirm,
// keeping the old file for Undo. If the apply doesn't go through, the old
// file is put back at once.
func webApply(s apply.System, u webUndo, source []byte, w io.Writer) error {
	old, err := os.ReadFile(u.configPath())
	hadOld := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := u.clear(); err != nil {
		fmt.Fprintf(w, "note: couldn't delete an old copy of router.toml: %v\n", err)
	}
	if err := writeConfig(u.configPath(), source); err != nil {
		return err
	}
	applyErr := applyFile(s, u.configPath(), w)
	n, _, pending := pendingSource(s)
	switch {
	case pending && hadOld:
		if err := u.keep(n, old); err != nil {
			fmt.Fprintf(w, "note: Undo won't be able to put router.toml back: %v\n", err)
		}
	case applyErr != nil && !pending && hadOld:
		if err := writeConfig(u.configPath(), old); err != nil {
			return errors.Join(applyErr, fmt.Errorf("putting router.toml back: %w", err))
		}
		fmt.Fprintln(w, "router.toml is as it was before.")
	}
	return applyErr
}
