package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fake records what the backend was asked to do.
type fake struct{ calls []string }

func (f *fake) backend() Backend {
	note := func(format string, args ...any) { f.calls = append(f.calls, fmt.Sprintf(format, args...)) }
	return Backend{
		Status: func(w io.Writer) error { fmt.Fprintln(w, "Running: generation 2"); return nil },
		Health: func(w io.Writer) error {
			fmt.Fprintln(w, "FAIL  20-wan")
			return errors.New("not every health check passes")
		},
		Config: func() ([]byte, error) { return []byte("schema_version = 1\n"), nil },
		Validate: func(source []byte, w io.Writer) error {
			note("validate %q", source)
			return nil
		},
		Diff: func(source []byte, w io.Writer) error { note("diff %q", source); return nil },
		Apply: func(source []byte, connection string, w io.Writer) error {
			note("apply %q from %q", source, connection)
			fmt.Fprintln(w, "Applied generation 3.")
			return nil
		},
		Confirm:  func(connection string, w io.Writer) error { note("confirm from %q", connection); return nil },
		Revert:   func(w io.Writer) error { note("revert"); return nil },
		Update:   func(date string, w io.Writer) error { note("update %q", date); return nil },
		Rollback: func(w io.Writer) error { note("rollback"); return nil },
	}
}

// serve starts the API on a Unix socket and returns a client for it.
func serve(t *testing.T, b Backend) *http.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := Listener(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() { _ = Serve(l, b) }()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func call(t *testing.T, c *http.Client, method, path, body string) (int, Response) {
	t.Helper()
	req, err := http.NewRequest(method, "http://rollgate"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r Response
	if resp.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode, r
}

func TestRequests(t *testing.T) {
	f := &fake{}
	c := serve(t, f.backend())

	if code, r := call(t, c, "GET", "/v1/status", ""); code != 200 || !r.OK || r.Output != "Running: generation 2\n" {
		t.Errorf("status: %d %+v", code, r)
	}
	if code, r := call(t, c, "GET", "/v1/health", ""); code != 422 || r.OK || r.Output != "FAIL  20-wan\n" || r.Error == "" {
		t.Errorf("health with a failing check: %d %+v", code, r)
	}
	if code, r := call(t, c, "GET", "/v1/config", ""); code != 200 || r.Config != "schema_version = 1\n" {
		t.Errorf("config: %d %+v", code, r)
	}
	if code, r := call(t, c, "POST", "/v1/apply", `{"config": "x = 1", "connection": "192.168.50.1 51234"}`); code != 200 || r.Output != "Applied generation 3.\n" {
		t.Errorf("apply: %d %+v", code, r)
	}
	for _, body := range []string{`{"connection": "192.168.50.1 51240"}`, ""} {
		call(t, c, "POST", "/v1/confirm", body)
	}
	call(t, c, "POST", "/v1/validate", `{"config": "y = 2"}`)
	call(t, c, "POST", "/v1/update", `{"date": "2026-11-23"}`)
	call(t, c, "POST", "/v1/update", "")
	call(t, c, "POST", "/v1/rollback", "")
	call(t, c, "POST", "/v1/revert", "")

	want := []string{
		`apply "x = 1" from "web 192.168.50.1 51234"`,
		`confirm from "web 192.168.50.1 51240"`,
		`validate "y = 2"`,
		`update "2026-11-23"`,
		`update ""`,
		"rollback",
		"revert",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("backend calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestRefusals(t *testing.T) {
	f := &fake{}
	c := serve(t, f.backend())
	for _, tc := range []struct {
		method, path, body string
		code               int
		err                string
	}{
		// Without the client's connection, apply couldn't keep that connection
		// from confirming (ADR 0010).
		{"POST", "/v1/apply", `{"config": "x = 1"}`, 422, "needs the client's connection"},
		{"POST", "/v1/confirm", `{}`, 422, "needs the client's connection"},
		{"POST", "/v1/update", `{"date": "latest; reboot"}`, 422, "isn't a release date"},
		{"POST", "/v1/apply", `{"config": "x", "connection": "c", "command": "rm -rf /"}`, 422, "unknown field"},
		{"POST", "/v1/apply", `not json`, 422, "bad request"},
		{"POST", "/v1/run", `{}`, 404, ""},
		{"GET", "/v1/apply", ``, 405, ""},
		{"POST", "/v1/status", ``, 405, ""},
	} {
		code, r := call(t, c, tc.method, tc.path, tc.body)
		if code != tc.code || !strings.Contains(r.Error, tc.err) {
			t.Errorf("%s %s %s: %d %+v, want %d and %q", tc.method, tc.path, tc.body, code, r, tc.code, tc.err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("refused requests reached the backend: %q", f.calls)
	}
}

func TestBodyLimit(t *testing.T) {
	f := &fake{}
	c := serve(t, f.backend())
	big := `{"config": "` + strings.Repeat("x", MaxBody) + `", "connection": "c"}`
	if code, r := call(t, c, "POST", "/v1/apply", big); code != 422 || len(f.calls) != 0 {
		t.Errorf("a body over %d bytes: %d %+v, calls %q", MaxBody, code, r, f.calls)
	}
}

func TestPeer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan string)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err.Error()
			return
		}
		defer c.Close()
		done <- peerOf(c)
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := <-done; !strings.HasPrefix(got, "pid ") || !strings.Contains(got, " uid ") {
		t.Errorf("peer: %q", got)
	}
}

func TestListenerWithoutSystemd(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	if _, err := Listener(""); err == nil || !strings.Contains(err.Error(), "rollgate-api.socket") {
		t.Errorf("no socket from systemd: %v", err)
	}
}
