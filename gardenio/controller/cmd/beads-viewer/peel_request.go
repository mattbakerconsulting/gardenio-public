package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The "request peel" write half of the R4 peel catalog (design doc
// ka-0000006592 §4.2). Browsing the menu opens nothing upstream; clicking
// "request peel" on a slice files a peel-request bead that IS the demand
// signal. A bead_event trigger on the `peel-request` label wakes the
// demand-driven peel gardener, which takes the pre-computed diff, opens ONE
// standalone upstream MR with provenance, and stops — the merge stays
// human-gated (§4.4). This handler never opens an upstream MR and never
// touches the real monorepo.
//
// Bead shape (design §9 open-question #3, resolved here): the peel-request is
// a type=task bead carrying the `peel-request` label — NOT a decision. A
// decision must present a genuine operator fork (server guard, ka-0000002237),
// and a peel request has none: the operator's judgment call is the LATER merge
// of the upstream MR the gardener opens, not this request. Modeling it as a
// task keeps it a first-class unit of work the peel gardener claims, and the
// `peel-request` label is exactly the bead_event wake key the gardener's
// trigger matches.

// peelRequestFieldMRBead / peelRequestFieldMRURL / peelRequestFieldRequestedBy
// are the fields stamped on the peel-request bead so the peel gardener (and the
// provenance chain, design §4.3) can resolve the slice without re-reading the
// catalog.
const (
	peelRequestFieldMRBead      = "peel_mr_bead"
	peelRequestFieldMRURL       = "peel_mr_url"
	peelRequestFieldRequestedBy = "peel_requested_by"
)

// peelRequestResultView is the fragment returned after a request POST. On
// success it carries the new bead id; on error, Error is set and the affordance
// re-renders so the operator can retry.
type peelRequestResultView struct {
	MRBeadID     string
	RequestID    string
	AlreadyThere bool
	Error        string
}

// handleRequestPeel handles POST /peel-catalog/{id}/request. {id} is the dark
// MR bead id. It is operator-gated at the route (operatorOnly). The response is
// an htmx fragment that swaps the row's affordance in place.
func (s *Server) handleRequestPeel(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	mrBeadID := strings.TrimSpace(r.PathValue("id"))
	if mrBeadID == "" {
		s.renderPeelRequestResult(w, r, peelRequestResultView{Error: "Missing MR bead id."}, http.StatusBadRequest)
		return
	}

	// Stamp the operator on the daemon ctx so the write calls carry
	// X-Operated-By — the kbeads audit resolver fail-closes without it (R2
	// enforcement, ka-0000005772), exactly as quick-spawn does.
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	mr, err := s.daemon.GetBead(ctx, mrBeadID)
	if err != nil || mr == nil {
		s.logger.Error("request-peel: fetching MR bead", "mr_bead", mrBeadID, "error", err)
		s.renderPeelRequestResult(w, r, peelRequestResultView{MRBeadID: mrBeadID, Error: "Could not load that MR."}, http.StatusBadGateway)
		return
	}
	if mr.Type != "merge-request" {
		s.renderPeelRequestResult(w, r, peelRequestResultView{MRBeadID: mrBeadID, Error: "Not a merge-request bead."}, http.StatusBadRequest)
		return
	}

	// Idempotency: if a peel was already requested for this MR, don't file a
	// duplicate. The `peel:requested` label on the MR bead is the marker.
	if peelHasLabel(mr.Labels, peelRequestedMRLabel) {
		s.renderPeelRequestResult(w, r, peelRequestResultView{MRBeadID: mrBeadID, AlreadyThere: true}, http.StatusOK)
		return
	}

	requestID, err := s.filePeelRequest(ctx, email, mr)
	if err != nil {
		s.logger.Error("request-peel: filing peel-request bead", "mr_bead", mrBeadID, "error", err)
		s.renderPeelRequestResult(w, r, peelRequestResultView{MRBeadID: mrBeadID, Error: "Failed to file the peel request."}, http.StatusBadGateway)
		return
	}

	s.renderPeelRequestResult(w, r, peelRequestResultView{MRBeadID: mrBeadID, RequestID: requestID}, http.StatusOK)
}

// filePeelRequest creates the peel-request bead, wires the provenance edge,
// and stamps the MR bead so a second visitor sees "requested". It returns the
// new bead id.
//
// Edge choice (design §4.3): the request bead is linked to the dark MR bead
// with a `related` edge, NOT `produces`. A `produces` edge means "this bead's
// work created that MR", which is false — the dark MR already exists; the
// request merely points at it. (The `produces` edge in the peel provenance
// chain is written LATER by `gb mr link --upstream`: peel-request --produces-->
// the UPSTREAM-mirror bead the peel gardener opens, not this dark MR.) The MR
// bead id also rides in the peel_mr_bead field so the peel gardener can resolve
// the slice by field even if the edge write fails.
func (s *Server) filePeelRequest(ctx context.Context, email string, mr *beadsapi.BeadDetail) (string, error) {
	mrURL := mrFieldString(mr.Fields, "mr_url", "url")

	fields := map[string]string{
		peelRequestFieldMRBead:      mr.ID,
		peelRequestFieldRequestedBy: email,
	}
	if mrURL != "" {
		fields[peelRequestFieldMRURL] = mrURL
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshalling peel-request fields: %w", err)
	}

	title := "peel request: " + mr.Title
	description := fmt.Sprintf(
		"Pull-on-demand peel request for dark MR %s (%s).\n\nRequested by %s via the peel catalog. The peel gardener wakes on the `peel-request` label to open ONE standalone upstream MR with provenance; the merge stays a human call.",
		mr.ID, peelFallback(mrURL, "no dark MR URL"), email,
	)

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "task",
		Description: description,
		CreatedBy:   email,
		Labels:      []string{peelRequestLabel, "project:gardenio"},
		Fields:      fieldsJSON,
		// One peel-request per MR — a double-click, a racing second tab, or a
		// second operator asking for the same slice all converge on the single
		// bead instead of filing duplicates (the peel gardener needs one wake per
		// slice, not one per requester).
		IdempotencyKey: "peel-request:" + mr.ID,
	}
	id, err := s.daemon.CreateBead(ctx, req)
	if err != nil {
		return "", fmt.Errorf("creating peel-request bead: %w", err)
	}

	// Best-effort provenance edge + MR marker — the request bead itself is the
	// load-bearing wake signal, so a failed edge/label is logged, not fatal.
	if err := s.daemon.AddDependency(ctx, id, mr.ID, "related", email); err != nil {
		s.logger.Warn("request-peel: adding related edge", "request", id, "mr_bead", mr.ID, "error", err)
	}
	if err := s.daemon.AddLabel(ctx, mr.ID, peelRequestedMRLabel); err != nil {
		s.logger.Warn("request-peel: stamping requested label on MR bead", "request", id, "mr_bead", mr.ID, "error", err)
	}
	return id, nil
}

// renderPeelRequestResult writes the request-result fragment with the given
// status. htmx swaps it into the row's affordance slot.
func (s *Server) renderPeelRequestResult(w http.ResponseWriter, r *http.Request, view peelRequestResultView, status int) {
	w.WriteHeader(status)
	s.renderPartial(w, r, "templates/partials/peel_request.html", "peel_request_result", map[string]any{
		"Result": view,
	})
}

// peelFallback returns v when non-empty (after trimming), else def.
func peelFallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
