package main

import (
	"bytes"
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
	cases := []struct {
		name     string
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		{"example is valid", []string{"validate", "../../../examples/router.toml"}, 0, "valid", ""},
		{"invalid config", []string{"validate", bad}, 1, "", "E009 system.ipv6"},
		{"missing file", []string{"validate", filepath.Join(t.TempDir(), "nope.toml")}, 1, "", "E012"},
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
