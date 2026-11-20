package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/sysinfo"
)

// fakeAPI is the router: it keeps router.toml and the pending change, and
// records the changes it was asked for.
type fakeAPI struct {
	down     bool
	config   string       // router.toml
	pending  *api.Pending // the change waiting for confirmation
	deadline time.Time    // an apply's revert deadline
	invalid  bool         // diff refuses every config
	posts    []string     // "path connection" for each change asked
	applied  string       // the last config applied
	system   *sysinfo.Info
	leases   []sysinfo.Lease
	vpn      *api.VPNState
}

func (f *fakeAPI) Get(_ context.Context, path string) (api.Response, error) {
	if f.down {
		return api.Response{}, errors.New("the router's API didn't answer")
	}
	switch path {
	case "/v1/health":
		return api.Response{OK: true, Output: "ok    20-wan         wan (wan) has 10.0.3.15/24\n"}, nil
	case "/v1/status":
		return api.Response{OK: true, Output: "Release: 2026-11-16.\n<script>alert(1)</script>\n"}, nil
	case "/v1/config":
		return api.Response{OK: true, Config: f.config}, nil
	case "/v1/pending":
		return api.Response{OK: true, Pending: f.pending}, nil
	case "/v1/vpn":
		if f.vpn == nil {
			return api.Response{OK: true, VPN: &api.VPNState{}}, nil
		}
		return api.Response{OK: true, VPN: f.vpn}, nil
	case "/v1/leases":
		return api.Response{OK: true, Leases: f.leases}, nil
	case "/v1/system":
		if f.system == nil {
			return api.Response{Error: "not in this test"}, nil
		}
		return api.Response{OK: true, System: f.system}, nil
	}
	return api.Response{}, errors.New("unexpected " + path)
}

func (f *fakeAPI) Post(_ context.Context, path string, req api.Request) (api.Response, error) {
	if f.down {
		return api.Response{}, errors.New("the router's API didn't answer")
	}
	f.posts = append(f.posts, strings.TrimSpace(path+" "+req.Connection))
	switch path {
	case "/v1/diff":
		if f.invalid {
			return api.Response{Error: "invalid config", Output: "router.toml:2: unknown key \"wan_typo\"\n"}, nil
		}
		return api.Response{OK: true, Output: "changed: /etc/nftables.conf\n1 file(s) would change\n"}, nil
	case "/v1/apply":
		f.config, f.applied = req.Config, req.Config
		f.pending = &api.Pending{Generation: 3, Deadline: f.deadline}
		return api.Response{OK: true, Output: "Applied generation 3.\n"}, nil
	case "/v1/confirm", "/v1/revert":
		if f.pending == nil {
			return api.Response{Error: "nothing is waiting for confirmation"}, nil
		}
		f.pending = nil
		return api.Response{OK: true}, nil
	case "/v1/vpn/key":
		if f.vpn == nil {
			f.vpn = &api.VPNState{}
		}
		f.vpn.PublicKey = "HIgo9xNzJMWLKASShiTqIybxZ0U3wGLiUeJ1PKf8ykw="
		return api.Response{OK: true}, nil
	case "/v1/update":
		return api.Response{OK: true, Output: "Already on the newest release, 2026-11-16.\n"}, nil
	case "/v1/rollback":
		return api.Response{Error: "there is no previous system to go back to"}, nil
	}
	return api.Response{}, errors.New("unexpected " + path)
}

const testPassword = "correct horse battery"

func newServer(t *testing.T, now *time.Time) (*Server, http.Handler) {
	t.Helper()
	h, err := hash(testPassword, []byte("0123456789abcdef"), 1000) // fast for tests
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{API: &fakeAPI{}, PasswordHash: h, Now: func() time.Time { return *now }}
	return s, s.Handler()
}

// do sends one request from 192.168.50.10, with the session cookie if one is given.
func do(h http.Handler, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "https://192.168.50.2"+path, body)
	r.RemoteAddr = "192.168.50.10:50000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(h, "POST", "/login", url.Values{"password": {testPassword}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func TestLoginAndStatus(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	_, h := newServer(t, &now)

	if w := do(h, "GET", "/", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("status without logging in: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := do(h, "GET", "/login", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `type="password"`) {
		t.Errorf("login page: %d", w.Code)
	}
	if w := do(h, "POST", "/login", url.Values{"password": {"wrong password!"}}, nil); w.Code != http.StatusUnauthorized ||
		len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "Wrong password") {
		t.Errorf("wrong password: %d, cookies %v", w.Code, w.Result().Cookies())
	}

	c := login(t, h)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || len(c.Value) < 40 {
		t.Errorf("session cookie %+v: want Secure, HttpOnly, SameSite=Strict and a long token", c)
	}
	w := do(h, "GET", "/", nil, c)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "all checks pass") || !strings.Contains(body, "Release: 2026-11-16.") {
		t.Errorf("status page: %d\n%s", w.Code, body)
	}
	if strings.Contains(body, "<script>") {
		t.Error("the status page doesn't escape the API's output")
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "no-store",
	} {
		if !strings.Contains(w.Header().Get(k), want) {
			t.Errorf("header %s: %q, want %q", k, w.Header().Get(k), want)
		}
	}

	// 30 minutes without a request end the session.
	now = now.Add(31 * time.Minute)
	if w := do(h, "GET", "/", nil, c); w.Code != http.StatusSeeOther {
		t.Errorf("after 31 idle minutes: %d, want a redirect to the login page", w.Code)
	}
}

func TestLogout(t *testing.T) {
	now := time.Now()
	s, h := newServer(t, &now)
	c := login(t, h)
	// A form without this session's CSRF token, as another site could post, is refused.
	if w := do(h, "POST", "/logout", url.Values{"csrf": {"forged"}}, c); w.Code != http.StatusForbidden {
		t.Errorf("logout with a forged token: %d", w.Code)
	}
	var csrf string
	s.mu.Lock()
	for _, sess := range s.sessions {
		csrf = sess.csrf
	}
	s.mu.Unlock()
	if w := do(h, "POST", "/logout", url.Values{"csrf": {csrf}}, c); w.Code != http.StatusSeeOther {
		t.Errorf("logout: %d", w.Code)
	}
	if w := do(h, "GET", "/", nil, c); w.Code != http.StatusSeeOther {
		t.Errorf("the session still works after logging out: %d", w.Code)
	}
}

func TestLoginLimit(t *testing.T) {
	now := time.Now()
	_, h := newServer(t, &now)
	for i := 0; i < maxFailures; i++ {
		do(h, "POST", "/login", url.Values{"password": {"guess"}}, nil)
	}
	// Even the right password is refused until the window has passed.
	if w := do(h, "POST", "/login", url.Values{"password": {testPassword}}, nil); w.Code != http.StatusTooManyRequests {
		t.Errorf("after %d failures: %d, want 429", maxFailures, w.Code)
	}
	now = now.Add(failureWindow + time.Second)
	login(t, h)
}

func TestAPIDown(t *testing.T) {
	now := time.Now()
	s, h := newServer(t, &now)
	c := login(t, h)
	s.API = &fakeAPI{down: true}
	if w := do(h, "GET", "/", nil, c); w.Code != 200 || !strings.Contains(w.Body.String(), "API didn&#39;t answer") {
		t.Errorf("status page with the API down: %d\n%s", w.Code, w.Body)
	}
}

func TestRedirect(t *testing.T) {
	for host, want := range map[string]string{
		"192.168.50.2":    "https://192.168.50.2/x?y=1",
		"192.168.50.2:80": "https://192.168.50.2/x?y=1",
		"[fd00::1]:80":    "https://[fd00::1]/x?y=1",
		"gateway":         "https://gateway/x?y=1",
	} {
		r := httptest.NewRequest("GET", "http://"+host+"/x?y=1", nil)
		r.Host = host
		w := httptest.NewRecorder()
		Redirect().ServeHTTP(w, r)
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want %q", host, w.Code, w.Header().Get("Location"), want)
		}
	}
}
