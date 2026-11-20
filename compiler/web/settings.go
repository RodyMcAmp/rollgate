package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/api"
	"github.com/RodyMcAmp/rollgate/compiler/compile"
)

// Editing router.toml (ADR 0014): the text is checked first (validation, what
// changes in router.toml and in the system files), then applied with
// commit-confirm (ADR 0010), then kept or undone on the confirm page.

// digest identifies a version of router.toml in a form, so the server can
// tell whether the text, or the router's file, changed since the check.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// editorText is the editor's text as router.toml: browsers send a
// textarea's lines ending in CRLF.
func editorText(r *http.Request) string {
	return strings.ReplaceAll(r.PostFormValue("config"), "\r\n", "\n")
}

// settingsPage shows router.toml in the editor.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "advanced")
	p.Nav = "settings"
	if cur, err := s.API.Get(r.Context(), "/v1/config"); err != nil {
		p.Error = err.Error()
	} else if !cur.OK {
		p.Error = cur.Error
	} else {
		p.Config, p.Base = cur.Config, digest(cur.Config)
	}
	s.render(w, http.StatusOK, "settings.html", p)
}

// settings is the editor's form: Check, or Apply after a check.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	// A change runs to the end even if the browser goes away.
	ctx := context.WithoutCancel(r.Context())
	p := s.newPage(ctx, sess, "advanced")
	p.Nav = "settings"
	text, err := submittedConfig(r)
	if err != nil {
		p.Error = "This form was damaged on the way; reload the page and try again."
		s.render(w, http.StatusBadRequest, "settings.html", p)
		return
	}
	p.Config, p.Base = text, r.PostFormValue("base")
	if r.PostFormValue("action") == "apply" && s.apply(ctx, w, r, p) {
		return
	}
	s.check(ctx, p)
	s.render(w, http.StatusOK, "settings.html", p)
}

// check validates p.Config and shows what applying it would change. Its
// changes to router.toml are against the router's file as it is now, which
// becomes the base an apply checks against.
func (s *Server) check(ctx context.Context, p *page) {
	cur, err := s.API.Get(ctx, "/v1/config")
	if err != nil || !cur.OK {
		if err == nil {
			err = apiError(cur)
		}
		p.Error = "Couldn't read the router's router.toml: " + err.Error()
		return
	}
	if p.Base != "" && digest(cur.Config) != p.Base && p.Notice == "" {
		p.Notice = "router.toml was changed on the router after you opened this page (over SSH, or from another browser). " +
			"The changes below compare your text with that newer version; applying replaces it with your text."
	}
	p.Base = digest(cur.Config)
	if cur.Config != p.Config {
		p.Changes = compile.TextDiff("router.toml on the router", "your version", []byte(cur.Config), []byte(p.Config))
	}
	d, err := s.API.Post(ctx, "/v1/diff", api.Request{Config: p.Config})
	switch {
	case err != nil:
		p.Error = err.Error()
	case !d.OK:
		p.Error = "This config has mistakes, so it can't be applied. Fix them and check again."
		p.Output = d.Output
	default:
		p.Files, p.Valid, p.Checked = d.Output, true, digest(p.Config)
	}
}

// apply applies p.Config if it is the text that was checked and the
// router's router.toml is still the one the check compared against; it
// reports whether it answered the request.
func (s *Server) apply(ctx context.Context, w http.ResponseWriter, r *http.Request, p *page) bool {
	if digest(p.Config) != r.PostFormValue("checked") {
		p.Notice = "You changed the text after checking it. Look over the changes again below, then apply."
		return false
	}
	cur, err := s.API.Get(ctx, "/v1/config")
	if err == nil && cur.OK && digest(cur.Config) != p.Base {
		p.Notice = "router.toml was changed on the router after your check (over SSH, or from another browser). " +
			"Look over the changes again below: they now compare your text with that newer version."
		return false
	}
	// Recorded before and after: a confirm must come over a connection
	// opened after the change was made.
	s.markApplied()
	resp, err := s.API.Post(ctx, "/v1/apply", api.Request{Config: p.Config, Connection: r.RemoteAddr})
	s.markApplied()
	if err != nil || !resp.OK {
		if err == nil {
			err = apiError(resp)
		}
		log.Printf("apply from %s failed: %v", r.RemoteAddr, err)
		p.Error, p.Output = "The change wasn't applied: "+err.Error(), resp.Output
		s.render(w, http.StatusOK, "settings.html", p)
		return true
	}
	log.Printf("apply from %s", r.RemoteAddr)
	// The browser fetches the confirm page over a new connection.
	w.Header().Set("Connection", "close")
	http.Redirect(w, r, "/confirm", http.StatusSeeOther)
	return true
}

func (s *Server) markApplied() {
	s.mu.Lock()
	s.applied = s.now()
	s.mu.Unlock()
}

// confirmPage shows the change waiting for confirmation, with its countdown.
func (s *Server) confirmPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "keep the new settings?")
	p.Nav = "confirm"
	s.render(w, http.StatusOK, "confirm.html", p)
}

// confirm keeps the pending change. Only a request over a connection opened
// after the apply proves that the change still lets this computer reach the
// router (ADR 0010, ADR 0014): an older connection can outlive a change that
// blocks new ones. rollgate web opens a connection for every request, so
// this holds for every click on the page; the API also refuses the apply's
// own connection.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	opened, ok := r.Context().Value(connOpenedKey{}).(time.Time)
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	if !ok || !opened.After(applied) {
		log.Printf("confirm from %s refused: its connection is older than the apply", r.RemoteAddr)
		p := s.newPage(ctx, sess, "keep the new settings?")
		p.Nav = "confirm"
		p.Error = "This click came over a connection that was open before the change, so it doesn't prove the router still accepts new connections. " +
			"Click Keep these settings again."
		w.Header().Set("Connection", "close")
		s.render(w, http.StatusConflict, "confirm.html", p)
		return
	}
	resp, err := s.API.Post(ctx, "/v1/confirm", api.Request{Connection: r.RemoteAddr})
	s.result(ctx, w, sess, "settings kept", resp, err,
		"The new settings are kept. They stay in place, also after the router restarts.")
}

// revert undoes the pending change now.
func (s *Server) revert(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	resp, err := s.API.Post(ctx, "/v1/revert", api.Request{})
	s.result(ctx, w, sess, "settings undone", resp, err,
		"The previous settings are back. The edited router.toml is still saved on the router, not applied: Advanced shows it, to change it and try again.")
}

// result shows how a change went: done, or why not, with what it printed.
func (s *Server) result(ctx context.Context, w http.ResponseWriter, sess *session, title string, resp api.Response, err error, done string) {
	p := s.newPage(ctx, sess, title)
	p.Output = resp.Output
	if err == nil && !resp.OK {
		err = apiError(resp)
	}
	if err != nil {
		p.Error = err.Error()
	} else {
		p.Done = done
	}
	s.render(w, http.StatusOK, "result.html", p)
}

type apiRefusal string

func (e apiRefusal) Error() string { return string(e) }

// apiError is the API's refusal as an error.
func apiError(resp api.Response) error {
	if resp.Error == "" {
		return apiRefusal("the router refused without saying why")
	}
	return apiRefusal(resp.Error)
}
