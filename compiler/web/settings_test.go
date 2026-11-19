package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
)

const routerToml = "schema_version = 1\n\n# my note\n[system]\nhostname = \"gw\"\n"

// loginForm logs in and returns the cookie and the session's CSRF token.
func loginForm(t *testing.T, s *Server, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	c := login(t, h)
	s.mu.Lock()
	defer s.mu.Unlock()
	return c, s.sessions[c.Value].csrf
}

// doOpened is do over a connection opened at opened, as http.Server's
// ConnContext records it.
func doOpened(h http.Handler, path string, form url.Values, cookie *http.Cookie, opened time.Time) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://192.168.50.2"+path, strings.NewReader(form.Encode()))
	r = r.WithContext(context.WithValue(r.Context(), connOpenedKey{}, opened))
	r.RemoteAddr = "192.168.50.10:50001"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var hidden = regexp.MustCompile(`name="(\w+)" value="([^"]*)"`)

// fields returns the hidden form fields on a page.
func fields(body string) map[string]string {
	m := map[string]string{}
	for _, f := range hidden.FindAllStringSubmatch(body, -1) {
		m[f[1]] = f[2]
	}
	return m
}

func setup(t *testing.T) (*Server, *fakeAPI, http.Handler, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s, h := newServer(t, &now)
	f := &fakeAPI{config: routerToml, deadline: now.Add(120 * time.Second)}
	s.API = f
	return s, f, h, &now
}

// TestEditApplyConfirm walks the whole change: edit, check, apply, a refused
// confirm over an old connection, and a confirm over a new one.
func TestEditApplyConfirm(t *testing.T) {
	s, f, h, now := setup(t)
	c, csrf := loginForm(t, s, h)

	w := do(h, "GET", "/settings", nil, c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "# my note") {
		t.Fatalf("settings page: %d\n%s", w.Code, w.Body)
	}
	base := fields(w.Body.String())["base"]

	// The browser sends the textarea with CRLF line ends.
	edited := strings.ReplaceAll(strings.Replace(routerToml, `"gw"`, `"home"`, 1), "\n", "\r\n")
	w = do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "base": {base}, "config": {edited}, "action": {"check"}}, c)
	body := w.Body.String()
	for _, want := range []string{"&#43;hostname = &#34;home&#34;", "-hostname = &#34;gw&#34;", "/etc/nftables.conf", `value="apply"`} {
		if !strings.Contains(body, want) {
			t.Errorf("check page lacks %q:\n%s", want, body)
		}
	}
	if len(f.posts) != 1 || f.posts[0] != "/v1/diff" {
		t.Errorf("check asked %v, want only a diff", f.posts)
	}
	checked := fields(body)["checked"]

	*now = now.Add(time.Second)
	old := *now // a connection the browser opened before the apply
	*now = now.Add(time.Second)
	w = do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "base": {base}, "checked": {checked}, "config": {edited}, "action": {"apply"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/confirm" || w.Header().Get("Connection") != "close" {
		t.Fatalf("apply: %d to %q, Connection %q\n%s", w.Code, w.Header().Get("Location"), w.Header().Get("Connection"), w.Body)
	}
	if want := strings.ReplaceAll(edited, "\r\n", "\n"); f.applied != want {
		t.Errorf("applied %q, want %q (LF line ends)", f.applied, want)
	}
	if !slices.Contains(f.posts, "/v1/apply 192.168.50.10:50000") {
		t.Errorf("apply didn't pass the browser's connection: %v", f.posts)
	}

	*now = now.Add(20 * time.Second)
	w = do(h, "GET", "/confirm", nil, c)
	if body := w.Body.String(); !strings.Contains(body, "Keep these settings") || !strings.Contains(body, `<strong id="left">98</strong>`) {
		t.Errorf("confirm page: want the button and 98 seconds left:\n%s", body)
	}
	if body := do(h, "GET", "/", nil, c).Body.String(); !strings.Contains(body, "waiting for you to keep it") {
		t.Errorf("the status page doesn't say a change is waiting:\n%s", body)
	}

	// A connection opened before the apply can't confirm.
	posts := len(f.posts)
	if w := doOpened(h, "/confirm", url.Values{"csrf": {csrf}}, c, old); w.Code != http.StatusConflict || len(f.posts) != posts {
		t.Errorf("confirm over an old connection: %d, API asked %v", w.Code, f.posts[posts:])
	}
	// Nor can a request that doesn't say when its connection opened.
	if w := do(h, "POST", "/confirm", url.Values{"csrf": {csrf}}, c); w.Code != http.StatusConflict || len(f.posts) != posts {
		t.Errorf("confirm without a connection time: %d", w.Code)
	}
	w = doOpened(h, "/confirm", url.Values{"csrf": {csrf}}, c, *now)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "The new settings are kept") || f.posts[len(f.posts)-1] != "/v1/confirm 192.168.50.10:50001" {
		t.Errorf("confirm over a new connection: %d, API asked %v\n%s", w.Code, f.posts[posts:], w.Body)
	}
	if body := do(h, "GET", "/confirm", nil, c).Body.String(); !strings.Contains(body, "Nothing is waiting") {
		t.Errorf("confirm page after confirming:\n%s", body)
	}
}

func TestApplyOnlyWhatWasChecked(t *testing.T) {
	s, f, h, _ := setup(t)
	c, csrf := loginForm(t, s, h)
	base := digest(routerToml)
	edited := routerToml + "# more\n"
	form := url.Values{"csrf": {csrf}, "base": {base}, "config": {edited}, "action": {"apply"}}

	// Edited after the check: shown again, not applied.
	form.Set("checked", digest(routerToml))
	if w := do(h, "POST", "/settings", form, c); !strings.Contains(w.Body.String(), "You changed the text after checking it") || f.applied != "" {
		t.Errorf("apply of unchecked text: applied %q\n%s", f.applied, w.Body)
	}
	// router.toml changed on the router after the check: shown again, not applied.
	form.Set("checked", digest(edited))
	f.config = routerToml + "# edited over SSH\n"
	w := do(h, "POST", "/settings", form, c)
	if !strings.Contains(w.Body.String(), "changed on the router after your check") || f.applied != "" {
		t.Errorf("apply over a changed router.toml: applied %q\n%s", f.applied, w.Body)
	}
	// That page's check compared against the new file, so applying it now works.
	form.Set("base", fields(w.Body.String())["base"])
	if w := do(h, "POST", "/settings", form, c); w.Code != http.StatusSeeOther || f.applied != edited {
		t.Errorf("apply after checking again: %d, applied %q", w.Code, f.applied)
	}
}

func TestCheckInvalid(t *testing.T) {
	s, f, h, _ := setup(t)
	c, csrf := loginForm(t, s, h)
	f.invalid = true
	w := do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "config": {"wan_typo = 1\n"}, "action": {"check"}}, c)
	body := w.Body.String()
	if !strings.Contains(body, "has mistakes") || !strings.Contains(body, "unknown key") || strings.Contains(body, `value="apply"`) {
		t.Errorf("check of an invalid config: want the mistakes and no Apply button\n%s", body)
	}
}

func TestNoApplyWhilePending(t *testing.T) {
	s, f, h, _ := setup(t)
	c, csrf := loginForm(t, s, h)
	f.pending = &api.Pending{Generation: 2}
	w := do(h, "POST", "/settings", url.Values{"csrf": {csrf}, "config": {routerToml + "# x\n"}, "action": {"check"}}, c)
	if body := w.Body.String(); strings.Contains(body, `value="apply"`) || !strings.Contains(body, "Do that first") {
		t.Errorf("check while a change is pending: want no Apply button\n%s", body)
	}
}

func TestRevert(t *testing.T) {
	s, f, h, _ := setup(t)
	c, csrf := loginForm(t, s, h)
	f.pending = &api.Pending{Generation: 3}
	if w := do(h, "POST", "/revert", url.Values{"csrf": {csrf}}, c); !strings.Contains(w.Body.String(), "previous settings are back") || f.pending != nil {
		t.Errorf("revert: %d\n%s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/revert", url.Values{"csrf": {csrf}}, c); !strings.Contains(w.Body.String(), "didn't work") {
		t.Errorf("revert with nothing pending: want the refusal\n%s", w.Body)
	}
}

func TestUpdatesPage(t *testing.T) {
	s, f, h, _ := setup(t)
	c, csrf := loginForm(t, s, h)
	if w := do(h, "GET", "/updates", nil, c); !strings.Contains(w.Body.String(), "Update now") || !strings.Contains(w.Body.String(), "Release: 2026-11-16.") {
		t.Errorf("updates page:\n%s", w.Body)
	}
	if w := do(h, "POST", "/updates/update", url.Values{"csrf": {csrf}}, c); !strings.Contains(w.Body.String(), "Already on the newest release") {
		t.Errorf("update:\n%s", w.Body)
	}
	if w := do(h, "POST", "/updates/rollback", url.Values{"csrf": {csrf}}, c); !strings.Contains(w.Body.String(), "no previous system") {
		t.Errorf("rollback:\n%s", w.Body)
	}
	if !slices.Equal(f.posts, []string{"/v1/update", "/v1/rollback"}) {
		t.Errorf("API asked %v", f.posts)
	}
}

// Every form that changes something needs the session's CSRF token, and a session.
func TestChangesNeedCSRF(t *testing.T) {
	s, f, h, now := setup(t)
	c, csrf := loginForm(t, s, h)
	for _, path := range []string{"/settings", "/confirm", "/revert", "/updates/update", "/updates/rollback"} {
		form := url.Values{"csrf": {"forged"}, "config": {routerToml}, "action": {"apply"}, "checked": {digest(routerToml)}}
		if w := doOpened(h, path, form, c, now.Add(time.Hour)); w.Code != http.StatusForbidden {
			t.Errorf("%s with a forged token: %d", path, w.Code)
		}
		form.Set("csrf", csrf)
		if w := do(h, "POST", path, form, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s without a session: %d", path, w.Code)
		}
	}
	if len(f.posts) != 0 {
		t.Errorf("refused forms reached the API: %v", f.posts)
	}
}
