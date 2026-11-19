package web

import (
	"context"
	"net/http"

	"github.com/RodyMcAmp/rollgate/compiler/api"
)

// The updates page (ADR 0014): rollgate update and rollgate rollback (ADR
// 0011), with what each does spelled out first. Both end in a restart.

func (s *Server) updatesPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "updates")
	s.loadStatus(r.Context(), p)
	s.render(w, http.StatusOK, "updates.html", p)
}

const restartNote = "If the text above says the router is restarting, this page stops answering for a few minutes. Then reload it."

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	// An update takes minutes and must not stop if the browser gives up waiting.
	ctx := context.WithoutCancel(r.Context())
	resp, err := s.API.Post(ctx, "/v1/update", api.Request{})
	s.result(ctx, w, sess, "update", resp, err, restartNote)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	resp, err := s.API.Post(ctx, "/v1/rollback", api.Request{})
	s.result(ctx, w, sess, "rollback", resp, err, restartNote)
}
