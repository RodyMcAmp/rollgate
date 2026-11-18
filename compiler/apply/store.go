// Package apply installs compiled files on the router and keeps the
// generations that make every change revertible (ADR 0010).
//
// All paths are below a root directory: "/" on the router, a temporary
// directory in tests.
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// StateDir is where rollgate keeps generations, relative to the root. It is
// its own Btrfs subvolume, so it survives a system rollback (ADR 0004).
const StateDir = "var/lib/rollgate"

// State is where a generation is in the commit-confirm flow.
type State string

const (
	Base      State = "base"      // generation 0: what was installed before the first apply
	Pending   State = "pending"   // applied, waiting for rollgate confirm
	Confirmed State = "confirmed" // confirmed over a new connection
	Reverted  State = "reverted"  // not confirmed in time, or reverted by hand
)

// Meta is a generation's meta.json.
type Meta struct {
	Generation    int       `json:"generation"`
	State         State     `json:"state"`
	Previous      int       `json:"previous"` // the generation a revert restores
	Created       time.Time `json:"created"`
	SSHConnection string    `json:"ssh_connection,omitempty"` // $SSH_CONNECTION of the apply
}

// Generation is one stored set of compiled files and the config that made it.
type Generation struct {
	Meta
	Source []byte // router.toml; empty for generation 0
	Files  compile.Files
}

// Store keeps generations in Root/var/lib/rollgate:
//
//	generations/<N>/meta.json
//	generations/<N>/router.toml
//	generations/<N>/files/...   (the same tree rollgate compile -o writes)
//	current                     (the last confirmed generation)
//	pending                     (only during a confirm window)
type Store struct{ Root string }

func (s Store) dir(parts ...string) string {
	return filepath.Join(append([]string{s.Root, filepath.FromSlash(StateDir)}, parts...)...)
}

func (s Store) genDir(n int) string { return s.dir("generations", strconv.Itoa(n)) }

// List returns the stored generations, oldest first.
func (s Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.dir("generations"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue // a leftover .tmp-N from an interrupted Save
		}
		m, err := s.meta(n)
		if err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	slices.SortFunc(metas, func(a, b Meta) int { return a.Generation - b.Generation })
	return metas, nil
}

// Next returns the number for a new generation: one more than the newest, at least 1.
func (s Store) Next() (int, error) {
	metas, err := s.List()
	if err != nil || len(metas) == 0 {
		return 1, err
	}
	return max(1, metas[len(metas)-1].Generation+1), nil
}

func (s Store) meta(n int) (Meta, error) {
	var m Meta
	data, err := os.ReadFile(filepath.Join(s.genDir(n), "meta.json"))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil && m.Generation != n {
		err = errors.New("generation number doesn't match its directory")
	}
	if err != nil {
		return Meta{}, fmt.Errorf("generation %d: %w", n, err)
	}
	return m, nil
}

// Load reads a stored generation.
func (s Store) Load(n int) (Generation, error) {
	m, err := s.meta(n)
	if err != nil {
		return Generation{}, err
	}
	g := Generation{Meta: m, Files: compile.Files{}}
	g.Source, err = os.ReadFile(filepath.Join(s.genDir(n), "router.toml"))
	if errors.Is(err, fs.ErrNotExist) && n == 0 {
		err = nil
	}
	if err != nil {
		return Generation{}, fmt.Errorf("generation %d: %w", n, err)
	}
	files := filepath.Join(s.genDir(n), "files")
	err = filepath.WalkDir(files, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(files, p)
		if err != nil {
			return err
		}
		g.Files[filepath.ToSlash(rel)], err = os.ReadFile(p)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil // a generation 0 of a system with none of the managed files
	}
	if err != nil {
		return Generation{}, fmt.Errorf("generation %d: %w", n, err)
	}
	return g, nil
}

// Save stores a new generation. It is written to a temporary directory and
// renamed into place, so a power cut never leaves half a generation.
func (s Store) Save(g Generation) error {
	final := s.genDir(g.Generation)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("generation %d already exists", g.Generation)
	}
	tmp := s.dir("generations", fmt.Sprintf(".tmp-%d", g.Generation))
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	for p, data := range g.Files {
		if err := checkPath(p); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(tmp, "files", filepath.FromSlash(p)), data); err != nil {
			return err
		}
	}
	if g.Generation != 0 {
		if err := writeFile(filepath.Join(tmp, "router.toml"), g.Source); err != nil {
			return err
		}
	}
	if err := s.writeMeta(tmp, g.Meta); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return syncDir(filepath.Dir(final))
}

func (s Store) writeMeta(dir string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "meta.json"), append(data, '\n'))
}

// SetState records a generation's new state.
func (s Store) SetState(n int, state State) error {
	m, err := s.meta(n)
	if err != nil {
		return err
	}
	m.State = state
	return s.writeMeta(s.genDir(n), m)
}

// Current returns the last confirmed generation, if any.
func (s Store) Current() (int, bool, error) { return s.marker("current") }

// SetCurrent records n as the last confirmed generation.
func (s Store) SetCurrent(n int) error { return s.setMarker("current", n) }

// Pending returns the generation waiting for confirmation, if any.
func (s Store) Pending() (int, bool, error) { return s.marker("pending") }

// SetPending records that n waits for confirmation.
func (s Store) SetPending(n int) error { return s.setMarker("pending", n) }

// ClearPending ends the confirm window.
func (s Store) ClearPending() error {
	err := os.Remove(s.dir("pending"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s Store) marker(name string) (int, bool, error) {
	data, err := os.ReadFile(s.dir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", s.dir(name), err)
	}
	return n, true, nil
}

func (s Store) setMarker(name string, n int) error {
	return writeFile(s.dir(name), []byte(strconv.Itoa(n)+"\n"))
}

// Prune removes the oldest generations so at most keep remain, never
// removing generation 0, the current one or a pending one.
func (s Store) Prune(keep int) error {
	metas, err := s.List()
	if err != nil {
		return err
	}
	current, _, err := s.Current()
	if err != nil {
		return err
	}
	pending, hasPending, err := s.Pending()
	if err != nil {
		return err
	}
	excess := len(metas) - keep
	for _, m := range metas {
		n := m.Generation
		if excess <= 0 {
			break
		}
		if n == 0 || n == current || (hasPending && n == pending) {
			continue
		}
		if err := os.RemoveAll(s.genDir(n)); err != nil {
			return err
		}
		excess--
	}
	return nil
}
