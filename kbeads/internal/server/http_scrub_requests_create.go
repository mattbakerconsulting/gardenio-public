// POST /v1/scrub-requests (docs/adr/taints.md §6.2/§7): the dedicated
// grant-holder-facing surface for filing a scrub-request, backing `kd scrub
// request`. The generic POST /v1/beads (type=scrub-request) already runs the
// full born-inert guard (applyScrubRequestGuard, scrub_request.go) — this
// handler is a thin wrapper around the same createBead choke point that adds
// exactly one thing the generic path cannot: surfacing the named taint's
// CURRENT scrub_policy.detaint_mode on the create response for a kind=detaint
// request (task description, decision gd-Swd7mnGf3L). That value is
// deliberately NOT written into the bead's own Fields — it is the taint's
// field, read fresh and echoed, never a new writable field on the request.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"gardenio/kbeads/internal/model"
)

// createScrubRequestBody is the JSON body for POST /v1/scrub-requests
// (docs/adr/taints.md §6.2). No mode/detaint_mode field exists here by
// design — see scrub_request.go's package doc.
type createScrubRequestBody struct {
	Taint string          `json:"taint"`
	Kind  string          `json:"kind"`
	Scope json.RawMessage `json:"scope"`
}

// scrubRequestCreateResponse is the response body for POST
// /v1/scrub-requests: the created bead (carrying resolved_bead_ids in its
// Fields, per applyScrubRequestGuard) plus, for a kind=detaint request only,
// the named taint's current detaint_mode.
type scrubRequestCreateResponse struct {
	*model.Bead
	DetaintMode string `json:"detaint_mode,omitempty"`
}

// scrubRequestTitle generates the title a scrub-request bead is created
// with. The CLI/API surface (ADR §7) takes no --title — there is nothing
// requester-authored to put here beyond what taint/kind already say.
func scrubRequestTitle(taint, kind string) string {
	return fmt.Sprintf("scrub-request: %s %s", kind, taint)
}

// handleCreateScrubRequest handles POST /v1/scrub-requests. It builds an
// ordinary type=scrub-request createBeadInput and routes it through the
// shared s.createBead choke point — every born-inert invariant (forbidden
// lifecycle fields, grant-holder check, scope resolution/freeze, purge
// readability check, requested_by stamping) is enforced there by
// applyScrubRequestGuard exactly as it is for a generic POST /v1/beads. The
// only behavior this handler adds is the detaint_mode echo below.
func (s *BeadsServer) handleCreateScrubRequest(w http.ResponseWriter, r *http.Request) {
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var body createScrubRequestBody
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Taint == "" {
		writeError(w, http.StatusBadRequest, "taint is required")
		return
	}
	if body.Kind == "" {
		writeError(w, http.StatusBadRequest, "kind is required")
		return
	}

	fields, err := json.Marshal(struct {
		Taint string          `json:"taint"`
		Kind  string          `json:"kind"`
		Scope json.RawMessage `json:"scope"`
	}{Taint: body.Taint, Kind: body.Kind, Scope: body.Scope})
	if err != nil {
		writeServerError(r.Context(), w, "handleCreateScrubRequest", err, "failed to encode fields")
		return
	}

	in := createBeadInput{
		Title:  scrubRequestTitle(body.Taint, body.Kind),
		Type:   string(model.TypeScrubRequest),
		Fields: fields,
	}

	ctx := r.Context()
	bead, err := s.createBead(ctx, in)
	if err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			if s.handleEscalation(w, r, denial, bodyBytes, "") {
				return
			}
			s.auditDenial(r, denial, "")
			writeAuthzDenial(w, denial)
			return
		}
		var refusal *operatorRefusalError
		if errors.As(err, &refusal) {
			writeOperatorRefusal(w, refusal.Source)
			return
		}
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
		} else {
			writeServerError(ctx, w, "handleCreateScrubRequest", err, "internal server error")
		}
		return
	}

	resp := scrubRequestCreateResponse{Bead: bead}
	if body.Kind == "detaint" {
		mode, err := s.currentTaintDetaintMode(ctx, body.Taint)
		if err != nil {
			writeServerError(ctx, w, "handleCreateScrubRequest", err, "failed to resolve taint detaint_mode")
			return
		}
		resp.DetaintMode = mode
	}

	writeJSON(w, http.StatusCreated, resp)
}

// currentTaintDetaintMode reads the named taint's CURRENT
// scrub_policy.detaint_mode, straight off the taint row — no hardcoded
// two/three-value switch, so a new mode value landing later on the taint
// (as "lift" did, ADR §15.4) surfaces here with no code change. An unset
// detaint_mode resolves to the ADR §6.1/§3 documented default, "fork".
func (s *BeadsServer) currentTaintDetaintMode(ctx context.Context, taintName string) (string, error) {
	taint, err := s.store.GetTaint(ctx, taintName)
	if err != nil {
		return "", err
	}
	if len(taint.ScrubPolicy) == 0 {
		return string(model.DetaintModeFork), nil
	}
	var policy model.ScrubPolicy
	if err := json.Unmarshal(taint.ScrubPolicy, &policy); err != nil {
		return "", fmt.Errorf("parsing scrub_policy for taint %q: %w", taintName, err)
	}
	if policy.DetaintMode == "" {
		return string(model.DetaintModeFork), nil
	}
	return string(policy.DetaintMode), nil
}
