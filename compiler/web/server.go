package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
)

//go:embed pages/*.html pages/style.css pages/countdown.js
var pages embed.FS

var tmpl = template.Must(template.ParseFS(pages, "pages/*.html"))

const (
	cookieName  = "rollgate_session"
	idleTimeout = 30 * time.Minute
	// Login attempts: after maxFailures failures from one address within
	// failureWindow, that address waits until the window has passed.
	maxFailures   = 5
	failureWindow = 15 * time.Minute
)

// API is what the web server asks the router; *api.Client in production.
type API interface {
	Get(ctx context.Context, path string) (api.Response, error)
	Post(ctx context.Context, path string, req api.Request) (api.Response, error)
}

// Server is rollgate web (ADR 0014). It runs unprivileged and changes nothing
// itself: every page asks the API.
type Server struct {
	API API
	// PasswordHash is the stored web password (HashPassword's output).
	PasswordHash string
	Now          func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	failures map[string][]time.Time // by client IP
	// applied is when this server last asked for an apply. A confirm must
	// come over a connection opened after it (see confirm).
	applied time.Time
}

type connOpenedKey struct{}

// ConnContext is for http.Server.ConnContext: it records when each
// connection was opened, which confirm checks.
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, connOpenedKey{}, s.now())
}

type session struct {
	csrf string
	last time.Time
}

// Handler serves the web UI over HTTPS.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.status)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /settings", s.settingsPage)
	mux.HandleFunc("POST /settings", s.settings)
	mux.HandleFunc("GET /confirm", s.confirmPage)
	mux.HandleFunc("POST /confirm", s.confirm)
	mux.HandleFunc("POST /revert", s.revert)
	mux.HandleFunc("GET /forwards", s.forwardsPage)
	mux.HandleFunc("POST /forwards", s.forwards)
	mux.HandleFunc("GET /updates", s.updatesPage)
	mux.HandleFunc("POST /updates/update", s.update)
	mux.HandleFunc("POST /updates/rollback", s.rollback)
	for _, f := range []string{"style.css", "countdown.js"} {
		mux.HandleFunc("GET /"+f, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=3600")
			http.ServeFileFS(w, r, pages, "pages/"+f)
		})
	}
	return headers(mux)
}

// Redirect serves plain HTTP: everything moves to HTTPS.
func Redirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.Contains(host, ":") { // an IPv6 address
			host = "[" + host + "]"
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// headers adds what every page needs: no framing, no outside resources, no caching.
func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// current returns the request's session, extending it, or nil.
func (s *Server) current(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[c.Value]
	if sess == nil {
		return nil
	}
	if s.now().Sub(sess.last) > idleTimeout {
		delete(s.sessions, c.Value)
		return nil
	}
	sess.last = s.now()
	return sess
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports whether ip has used up its login attempts, forgetting old failures.
func (s *Server) blocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recent []time.Time
	for _, t := range s.failures[ip] {
		if s.now().Sub(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	if s.failures == nil {
		s.failures = map[string][]time.Time{}
	}
	s.failures[ip] = recent
	return len(recent) >= maxFailures
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, page, data); err != nil {
		log.Printf("rendering %s: %v", page, err)
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.current(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", map[string]string{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.blocked(ip) {
		log.Printf("login from %s refused: too many failed attempts", ip)
		s.render(w, http.StatusTooManyRequests, "login.html", map[string]string{
			"Error": "Too many wrong passwords from this computer. Wait 15 minutes, then try again."})
		return
	}
	ok, err := CheckPassword(s.PasswordHash, r.PostFormValue("password"))
	if err != nil {
		log.Printf("checking a login: %v", err)
	}
	if !ok {
		s.mu.Lock()
		s.failures[ip] = append(s.failures[ip], s.now())
		s.mu.Unlock()
		log.Printf("failed login from %s", ip)
		s.render(w, http.StatusUnauthorized, "login.html", map[string]string{"Error": "Wrong password."})
		return
	}
	token, csrf := randomToken(), randomToken()
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	s.sessions[token] = &session{csrf: csrf, last: s.now()}
	delete(s.failures, ip)
	s.mu.Unlock()
	log.Printf("login from %s", ip)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// posted returns the session of a form that changes something, or writes a
// refusal and returns nil: the form must carry its session's CSRF token.
func (s *Server) posted(w http.ResponseWriter, r *http.Request) *session {
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxBody)
	sess := s.current(r)
	if sess == nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		http.Error(w, "This form expired or didn't come from this router's page. Reload the page and try again.", http.StatusForbidden)
		return nil
	}
	return sess
}

// page is what every page's template gets. Each page fills its own fields.
type page struct {
	Title, CSRF, Error, Notice string
	// Pending is the change waiting for confirmation, if any: every page
	// shows it. Left is the seconds until it reverts by itself.
	Pending *api.Pending
	Left    int

	HealthOK       bool   // status
	Health, Status string // status, updates

	Config  string // settings: the text in the editor
	Base    string // settings: digest of the router's router.toml the text started from
	Checked string // settings: digest of the text whose check passed
	Changes string // settings: router.toml diff
	Files   string // settings: what changes in the system files
	Output  string // what a request printed
	Valid   bool   // settings: the check passed, so Apply is offered
	Done    string // result: what happened

	Nav string // the menu entry this page belongs to

	Forwards  []forwardRow // port forwarding
	Devices   []device     // port forwarding: fixed-address devices to choose from
	Form      forwardForm  // port forwarding: the add or edit form
	Removing  string       // port forwarding: the name of the forward being removed
	Reviewing bool         // a form's change is shown for review
	Config64  string       // the reviewed router.toml, for the Apply form
	Advanced  bool         // the error suggests the Advanced editor
}

// newPage starts a page for a logged-in session, with any pending change.
func (s *Server) newPage(ctx context.Context, sess *session, title string) *page {
	p := &page{Title: title, CSRF: sess.csrf}
	resp, err := s.API.Get(ctx, "/v1/pending")
	switch {
	case err != nil:
		p.Error = err.Error()
	case !resp.OK:
		p.Error = resp.Error
	case resp.Pending != nil:
		p.Pending = resp.Pending
		if !resp.Pending.Deadline.IsZero() {
			p.Left = max(0, int(resp.Pending.Deadline.Sub(s.now()).Round(time.Second)/time.Second))
		}
	}
	return p
}

// loggedIn returns the session, or redirects to the login page and returns nil.
func (s *Server) loggedIn(w http.ResponseWriter, r *http.Request) *session {
	sess := s.current(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
	return sess
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.posted(w, r) == nil {
		return
	}
	c, _ := r.Cookie(cookieName)
	s.mu.Lock()
	delete(s.sessions, c.Value)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// status is the start page: the health checks and rollgate status, from the API.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "overview")
	p.Nav = "status"
	if h, err := s.API.Get(r.Context(), "/v1/health"); err != nil {
		p.Error = err.Error()
	} else {
		p.HealthOK, p.Health = h.OK, h.Output
	}
	s.loadStatus(r.Context(), p)
	s.render(w, http.StatusOK, "status.html", p)
}

// loadStatus puts rollgate status on p.
func (s *Server) loadStatus(ctx context.Context, p *page) {
	if st, err := s.API.Get(ctx, "/v1/status"); err != nil {
		p.Error = err.Error()
	} else {
		p.Status = st.Output
		if !st.OK {
			p.Error = st.Error
		}
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
