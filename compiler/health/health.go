// Package health runs the router's health checks (docs/architecture.md,
// "Health checks"; ADR 0011).
//
// A check is an executable in /usr/lib/rollgate/health.d/ that exits 0 to
// pass and prints one line saying what it saw. The checks rollgate ships are
// one-line scripts that call `rollgate check <name>`, implemented in this
// package, so their logic is tested here; anyone can add a script of their own.
package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Dir is where the checks live.
const Dir = "/usr/lib/rollgate/health.d"

// Result is one check's outcome.
type Result struct {
	Name   string // the executable's file name, e.g. "20-wan"
	OK     bool
	Detail string // the first line it printed, or why it failed to run
}

// RunDir runs every executable in dir, in name order, each with a time limit.
// A check that can't start or runs too long fails; RunDir itself only fails if
// dir can't be read.
func RunDir(dir string, limit time.Duration) ([]Result, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || strings.HasPrefix(e.Name(), ".") {
			continue // not an executable file (README, editor backups, ...)
		}
		results = append(results, runOne(filepath.Join(dir, e.Name()), limit))
	}
	slices.SortFunc(results, func(a, b Result) int { return strings.Compare(a.Name, b.Name) })
	return results, nil
}

func runOne(path string, limit time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdout, cmd.Stderr = &out, &out
	// On timeout, kill the check and anything it started: a child left running
	// would keep the output pipe open and hang the whole health run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	r := Result{Name: filepath.Base(path), OK: err == nil, Detail: firstLine(out.String())}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.OK, r.Detail = false, fmt.Sprintf("didn't finish within %s", limit)
	case err != nil && r.Detail == "":
		r.Detail = err.Error()
	}
	return r
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// AllOK reports whether every check passed. No checks at all is not OK: an
// empty health.d means the checks are missing, not that the router is healthy.
func AllOK(results []Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

// Print writes one line per check: "ok" or "FAIL", its name, what it saw.
func Print(w io.Writer, results []Result) {
	for _, r := range results {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s  %-14s %s\n", mark, r.Name, r.Detail)
	}
	if len(results) == 0 {
		fmt.Fprintf(w, "FAIL  no health checks found in %s\n", Dir)
	}
}

// Wait runs the checks every interval until all pass or the time is up, and
// returns the last results.
func Wait(dir string, limit, interval, total time.Duration, now func() time.Time, sleep func(time.Duration)) ([]Result, error) {
	deadline := now().Add(total)
	for {
		results, err := RunDir(dir, limit)
		if err != nil || AllOK(results) || !now().Add(interval).Before(deadline) {
			return results, err
		}
		sleep(interval)
	}
}
