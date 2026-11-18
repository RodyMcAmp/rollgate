package compile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Change is one file that installing the compiled files would change.
type Change struct {
	Path string // relative to the root, e.g. "etc/nftables.conf"
	Kind string // "new", "changed" or "removed"
	Diff string // unified diff, installed version first
}

// removable lists the generated files whose names depend on the config
// (one per interface or VLAN). A file matching one of these that the new
// config no longer produces would be removed by apply.
var removable = []string{
	"etc/systemd/network/*-rollgate-*.network",
	"etc/systemd/network/*-rollgate-*.netdev",
	"etc/systemd/network/*-rollgate-*.link",
}

// Owned lists the installed files below root that match the removable
// patterns, relative to root and sorted. Apply removes those the new
// config doesn't produce.
func Owned(root string) ([]string, error) {
	var owned []string
	for _, pattern := range removable {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			rel, err := filepath.Rel(root, m)
			if err != nil {
				return nil, err
			}
			owned = append(owned, filepath.ToSlash(rel))
		}
	}
	slices.Sort(owned)
	return owned, nil
}

// Compare reports how the files installed below root differ from f: what
// the router is running against what installing f would give.
func (f Files) Compare(root string) ([]Change, error) {
	var changes []Change
	for _, p := range f.Paths() {
		installed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			changes = append(changes, Change{p, "new", unifiedDiff(p, nil, f[p])})
		case err != nil:
			return nil, err
		case !bytes.Equal(installed, f[p]):
			changes = append(changes, Change{p, "changed", unifiedDiff(p, installed, f[p])})
		}
	}
	owned, err := Owned(root)
	if err != nil {
		return nil, err
	}
	for _, p := range owned {
		if f[p] != nil {
			continue
		}
		installed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		changes = append(changes, Change{p, "removed", unifiedDiff(p, installed, nil)})
	}
	return changes, nil
}

// unifiedDiff renders a unified diff with three lines of context. Generated
// files are small, so a simple longest-common-subsequence table is enough.
func unifiedDiff(path string, old, new []byte) string {
	a, b := lines(old), lines(new)
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	type op struct {
		kind         byte // ' ', '-' or '+'
		text         string
		aLine, bLine int // lines consumed before this op
	}
	var ops []op
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i, j = i+1, j+1
		// Removed lines come before added ones, as in diff(1).
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', b[j], i, j})
			j++
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- installed /%s\n+++ compiled /%s\n", path, path)
	const context = 3
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start, end := max(0, k-context), k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			next := end
			for next < len(ops) && ops[next].kind == ' ' {
				next++
			}
			if next == len(ops) || next-end > 2*context {
				end = min(len(ops), end+context)
				break
			}
			end = next
		}
		aCount, bCount := 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aCount++
			}
			if o.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ops[start].aLine+1, aCount, ops[start].bLine+1, bCount)
		for _, o := range ops[start:end] {
			fmt.Fprintf(&out, "%c%s\n", o.kind, o.text)
		}
		k = end
	}
	return out.String()
}

func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
