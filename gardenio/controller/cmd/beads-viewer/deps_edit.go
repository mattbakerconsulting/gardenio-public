package main

import (
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The blocking-edge writes behind the in-garden bead panel's "Waits on" and
// "Holds up" lists (Lands workbench-glass spec WB-4, task gd-PXHQecYeDU): add
// or remove one `blocks` dependency through the daemon client the /b/{id}/deps
// fragment reads with. Form fields:
//
//	other  the other bead's id
//	dir    "waits" (this bead waits on other: id depends on other) or
//	       "holds" (this bead holds other up: other depends on id)
//
// Both routes are operator-gated where they are registered, like
// POST /b/{id}/parent, and answer JSON through jsonAction.

// depEdge resolves a deps form into the edge it names: the bead that waits and
// the bead it waits on. ok is false, with the message to show, for a form that
// names no edge.
func depEdge(id string, r *http.Request) (from, to, msg string, ok bool) {
	other := strings.TrimSpace(r.PostForm.Get("other"))
	switch {
	case other == "":
		return "", "", "Say which bead.", false
	case other == id:
		return "", "", "A bead cannot wait on itself.", false
	case len(other) > 200:
		return "", "", "That is not a bead id.", false
	}
	switch r.PostForm.Get("dir") {
	case "waits":
		return id, other, "", true
	case "holds":
		return other, id, "", true
	}
	return "", "", "Say whether this bead waits on it or holds it up.", false
}

// depRequest is the shared front of both handlers: the signed-in operator, the
// parsed form and the edge. It answers the request itself and reports false
// when there is nothing to do.
func (s *Server) depRequest(w http.ResponseWriter, r *http.Request) (email, from, to string, ok bool) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return "", "", "", false
	}
	email = oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return "", "", "", false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return "", "", "", false
	}
	from, to, msg, ok := depEdge(id, r)
	if !ok {
		http.Error(w, msg, http.StatusBadRequest)
		return "", "", "", false
	}
	return email, from, to, true
}

// handleAddDep serves POST /b/{id}/deps: both beads must be ones the viewer can
// read (a bead it cannot see is not there, as on the bead page).
func (s *Server) handleAddDep(w http.ResponseWriter, r *http.Request) {
	email, from, to, ok := s.depRequest(w, r)
	if !ok {
		return
	}
	for _, id := range []string{from, to} {
		if b, err := s.daemon.GetBead(r.Context(), id); err != nil || b == nil {
			var ae *beadsapi.APIError
			if err == nil || (errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden)) {
				http.Error(w, "There is no bead "+id+", or you cannot see it.", http.StatusNotFound)
				return
			}
			s.logger.Error("deps: loading bead", "id", id, "error", err)
			http.Error(w, "Failed to load bead", http.StatusBadGateway)
			return
		}
	}
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))
	if err := s.daemon.AddDependency(ctx, from, to, "blocks", email); err != nil {
		s.logger.Error("deps: add", "from", from, "to", to, "actor", email, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to add the link.")
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveDep serves POST /b/{id}/deps/remove.
func (s *Server) handleRemoveDep(w http.ResponseWriter, r *http.Request) {
	email, from, to, ok := s.depRequest(w, r)
	if !ok {
		return
	}
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))
	if err := s.daemon.RemoveDependency(ctx, from, to, "blocks"); err != nil {
		s.logger.Error("deps: remove", "from", from, "to", to, "actor", email, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to remove the link.")
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
