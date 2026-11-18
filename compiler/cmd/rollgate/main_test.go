package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCommand(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(bad, []byte("schema_version = 1\n[system]\nipv6 = \"on\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A root with the example's compiled files installed, for diff.
	installed := filepath.Join(t.TempDir(), "root")
	if code := run([]string{"compile", "-o", installed, "../../../examples/router.toml"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("compile exit code %d", code)
	}
	cases := []struct {
		name     string
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		{"diff with nothing installed", []string{"diff", "-root", t.TempDir(), "../../../examples/router.toml"}, 1, "new: ", ""},
		{"diff with the same files installed", []string{"diff", "-root", installed, "../../../examples/router.toml"}, 0, "no changes", ""},
		{"diff refuses invalid config", []string{"diff", "-root", installed, bad}, 2, "", "E009"},
		{"example is valid", []string{"validate", "../../../examples/router.toml"}, 0, "valid", ""},
		{"invalid config", []string{"validate", bad}, 1, "", "E009 system.ipv6"},
		{"missing file", []string{"validate", filepath.Join(t.TempDir(), "nope.toml")}, 1, "", "E012"},
		{"compile example", []string{"compile", "-o", filepath.Join(t.TempDir(), "out"), "../../../examples/router.toml"}, 0, "30-rollgate-lan.network", ""},
		{"compile needs -o", []string{"compile", "../../../examples/router.toml"}, 2, "", "usage"},
		{"compile refuses invalid config", []string{"compile", "-o", filepath.Join(t.TempDir(), "out"), bad}, 1, "", "E009"},
		{"compile refuses non-empty dir", []string{"compile", "-o", filepath.Dir(bad), "../../../examples/router.toml"}, 1, "", "not empty"},
		{"schema", []string{"schema"}, 0, "\"$schema\"", ""},
		{"no command", nil, 2, "", "usage"},
		{"unknown command", []string{"frobnicate"}, 2, "", "unknown command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String(), tc.inStdout) || !strings.Contains(stderr.String(), tc.inStderr) {
				t.Errorf("stdout %q should contain %q; stderr %q should contain %q", &stdout, tc.inStdout, &stderr, tc.inStderr)
			}
		})
	}
}
