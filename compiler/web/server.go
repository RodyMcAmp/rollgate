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

//go:embed pages/*.html pages/style.css
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
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		http.ServeFileFS(w, r, pages, "pages/style.css")
	})
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

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sess := s.current(r)
	if sess == nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		http.Error(w, "This form expired or didn't come from this router's page. Reload the page and try again.", http.StatusForbidden)
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
	sess := s.current(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data := map[string]any{"CSRF": sess.csrf}
	if h, err := s.API.Get(r.Context(), "/v1/health"); err != nil {
		data["Error"] = err.Error()
	} else {
		data["HealthOK"], data["Health"] = h.OK, h.Output
	}
	if st, err := s.API.Get(r.Context(), "/v1/status"); err != nil {
		data["Error"] = err.Error()
	} else {
		data["Status"] = st.Output
		if !st.OK {
			data["Error"] = st.Error
		}
	}
	s.render(w, http.StatusOK, "status.html", data)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
