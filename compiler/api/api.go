// Package api is rollgate's local API (ADR 0014): what the web UI asks the
// router to do. It serves HTTP on a Unix socket that only root and the
// rollgate-api group can open (rollgate-api.socket), and offers a fixed list of
// requests, each calling the same code as the CLI command of the same name.
// There is no request that runs an arbitrary command.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

// Backend is the router: the functions behind each request. Each writes what
// the CLI would print to w. An error is a refusal or failure, explained by the
// error and by what was written.
type Backend struct {
	Status func(w io.Writer) error
	Health func(w io.Writer) error // an error if any check fails
	Config func() ([]byte, error)  // the current router.toml
	// Pending is the change waiting for confirmation, or nil.
	Pending func() (*Pending, error)
	// System is the kernel, uptime, memory, disk and interfaces.
	System func() (*sysinfo.Info, error)
	// Leases is the DHCP server's current leases.
	Leases func() ([]sysinfo.Lease, error)
	// VPN is the router's VPN key, WAN address and devices' last handshakes;
	// VPNKey makes the router's VPN key if there is none (ADR 0018).
	VPN      func() (*VPNState, error)
	VPNKey   func(w io.Writer) error
	Validate func(source []byte, w io.Writer) error
	Diff     func(source []byte, w io.Writer) error
	// Apply validates source, makes it router.toml and applies it with
	// commit-confirm (ADR 0010). connection identifies the client's
	// connection, which can't confirm the change.
	Apply    func(source []byte, connection string, w io.Writer) error
	Confirm  func(connection string, w io.Writer) error
	Revert   func(w io.Writer) error
	Update   func(date string, w io.Writer) error // date "" for the newest
	Rollback func(w io.Writer) error
}

// Request is the body of a POST request. Fields a request doesn't use are ignored.
type Request struct {
	Config     string `json:"config,omitempty"`     // validate, diff, apply
	Connection string `json:"connection,omitempty"` // apply, confirm: the browser's TCP connection
	Date       string `json:"date,omitempty"`       // update
}

// Response is every reply. OK false comes with Error, and Output says more.
type Response struct {
	OK      bool            `json:"ok"`
	Output  string          `json:"output,omitempty"`
	Error   string          `json:"error,omitempty"`
	Config  string          `json:"config,omitempty"`  // GET /v1/config
	Pending *Pending        `json:"pending,omitempty"` // GET /v1/pending, if a change waits
	System  *sysinfo.Info   `json:"system,omitempty"`  // GET /v1/system
	Leases  []sysinfo.Lease `json:"leases,omitempty"`  // GET /v1/leases
	VPN     *VPNState       `json:"vpn,omitempty"`     // GET /v1/vpn
}

// VPNState is what the web UI's VPN page needs from the router (ADR 0018).
type VPNState struct {
	PublicKey  string               `json:"public_key,omitempty"`  // the router's; "" until it has a key
	WANAddress string               `json:"wan_address,omitempty"` // the router's IPv4 address on WAN now
	LastSeen   map[string]time.Time `json:"last_seen,omitempty"`   // devices' last handshakes, by public key
}

// Pending is a change waiting for confirmation (ADR 0010).
type Pending struct {
	Generation int `json:"generation"`
	// Deadline is when it reverts by itself; zero if unknown.
	Deadline time.Time `json:"deadline,omitzero"`
}

// MaxBody bounds a request body: router.toml is a few kilobytes.
const MaxBody = 1 << 20

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Handler routes the requests. Changes run one at a time.
func Handler(b Backend) http.Handler {
	var changing sync.Mutex
	mux := http.NewServeMux()
	read := func(f func(w io.Writer) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { reply(w, run(f)) }
	}
	change := func(f func(req Request, w io.Writer) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req Request
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				reply(w, Response{Error: "bad request: " + err.Error()})
				return
			}
			changing.Lock()
			defer changing.Unlock()
			reply(w, run(func(out io.Writer) error { return f(req, out) }))
		}
	}
	mux.Handle("GET /v1/status", read(b.Status))
	mux.Handle("GET /v1/health", read(b.Health))
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		source, err := b.Config()
		if err != nil {
			reply(w, Response{Error: err.Error()})
			return
		}
		reply(w, Response{OK: true, Config: string(source)})
	})
	mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, r *http.Request) {
		p, err := b.Pending()
		if err != nil {
			reply(w, Response{Error: err.Error()})
			return
		}
		reply(w, Response{OK: true, Pending: p})
	})
	mux.HandleFunc("GET /v1/system", func(w http.ResponseWriter, r *http.Request) {
		info, err := b.System()
		if err != nil {
			reply(w, Response{Error: err.Error()})
			return
		}
		reply(w, Response{OK: true, System: info})
	})
	mux.HandleFunc("GET /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		leases, err := b.Leases()
		if err != nil {
			reply(w, Response{Error: err.Error()})
			return
		}
		reply(w, Response{OK: true, Leases: leases})
	})
	mux.HandleFunc("GET /v1/vpn", func(w http.ResponseWriter, r *http.Request) {
		st, err := b.VPN()
		if err != nil {
			reply(w, Response{Error: err.Error()})
			return
		}
		reply(w, Response{OK: true, VPN: st})
	})
	mux.Handle("POST /v1/vpn/key", change(func(_ Request, w io.Writer) error { return b.VPNKey(w) }))
	mux.Handle("POST /v1/validate", change(func(req Request, w io.Writer) error { return b.Validate([]byte(req.Config), w) }))
	mux.Handle("POST /v1/diff", change(func(req Request, w io.Writer) error { return b.Diff([]byte(req.Config), w) }))
	mux.Handle("POST /v1/apply", change(func(req Request, w io.Writer) error {
		if req.Connection == "" {
			return errors.New("apply needs the client's connection, so that a confirm can't come from it")
		}
		return b.Apply([]byte(req.Config), "web "+req.Connection, w)
	}))
	mux.Handle("POST /v1/confirm", change(func(req Request, w io.Writer) error {
		if req.Connection == "" {
			return errors.New("confirm needs the client's connection, to check it isn't the one that applied")
		}
		return b.Confirm("web "+req.Connection, w)
	}))
	mux.Handle("POST /v1/revert", change(func(_ Request, w io.Writer) error { return b.Revert(w) }))
	mux.Handle("POST /v1/update", change(func(req Request, w io.Writer) error {
		if req.Date != "" && !datePattern.MatchString(req.Date) {
			return fmt.Errorf("%q isn't a release date (YYYY-MM-DD)", req.Date)
		}
		return b.Update(req.Date, w)
	}))
	mux.Handle("POST /v1/rollback", change(func(_ Request, w io.Writer) error { return b.Rollback(w) }))
	return logged(mux)
}

// run calls f, collecting what it writes.
func run(f func(w io.Writer) error) Response {
	var out bytes.Buffer
	if err := f(&out); err != nil {
		return Response{Output: out.String(), Error: err.Error()}
	}
	return Response{OK: true, Output: out.String()}
}

func reply(w http.ResponseWriter, resp Response) {
	w.Header().Set("Content-Type", "application/json")
	if !resp.OK {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

type peerKey struct{}

// logged writes one journal line per request: what was asked, by which process.
func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _ := r.Context().Value(peerKey{}).(string)
		log.Printf("%s %s from %s", r.Method, r.URL.Path, peer)
		h.ServeHTTP(w, r)
	})
}

// Serve answers requests on l until it fails.
func Serve(l net.Listener, b Backend) error {
	srv := &http.Server{
		Handler:  Handler(b),
		ErrorLog: log.Default(),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerOf(c))
		},
	}
	return srv.Serve(l)
}

// peerOf describes the process at the other end of a Unix socket connection.
func peerOf(c net.Conn) string {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return c.RemoteAddr().String()
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "unknown"
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cerr != nil {
		return "unknown"
	}
	return fmt.Sprintf("pid %d uid %d", cred.Pid, cred.Uid)
}

// Listener is the socket systemd passes (rollgate-api.socket), or, for tests
// and development, a new socket at path.
func Listener(path string) (net.Listener, error) {
	if path != "" {
		return net.Listen("unix", path)
	}
	ls, err := SystemdListeners()
	if err != nil || len(ls) != 1 {
		return nil, errors.New("no socket from systemd (start rollgate-api.socket, or pass -listen PATH)")
	}
	return ls[0], nil
}
