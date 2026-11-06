package server

import (
	"context"
	"net/http"
	"strings"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

// Whoami returns the authenticated identity derived from the request context.
// Plan-of-record: beads3d/docs/my-work-operator-tab.md §Phase 3. The viewer
// uses this to default the operator id from the auth claim instead of from
// a URL param / localStorage.
//
// Source values:
//   - "auth"     — OIDC claim (forwardauth or bearer-with-IdentityVerifier)
//   - "token"    — static or fleet bearer token
//   - "runner"   — a runner bearer, resolved to its owner (local-agents D6);
//     an oidc: owner is given as the bare email an OIDC login would carry
//   - "anonymous" — no auth context (server running without auth, or the
//     middleware did not attach an identity)
//
// The code path stays a no-op until forwardauth lands in front of the viewer:
// without an OIDC claim and without a static token the response is
// {operator_id:"", source:"anonymous"}.

const (
	whoamiSourceAuth      = "auth"
	whoamiSourceToken     = "token"
	whoamiSourceAnonymous = "anonymous"
	whoamiSourceRunner    = "runner"
)

// computeWhoami resolves the (operator_id, source) pair from the auth
// identity attached to ctx by the auth middleware.
func computeWhoami(ctx context.Context) (string, string) {
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "oidc":
		return id.Subject, whoamiSourceAuth
	case "fleet", "machine", "land":
		return id.Subject, whoamiSourceToken
	case runnerIdentityType:
		return strings.TrimPrefix(id.Subject, "oidc:"), whoamiSourceRunner
	default:
		return "", whoamiSourceAnonymous
	}
}

// Whoami is the gRPC handler.
func (s *BeadsServer) Whoami(ctx context.Context, _ *beadsv1.WhoamiRequest) (*beadsv1.WhoamiResponse, error) {
	op, src := computeWhoami(ctx)
	return &beadsv1.WhoamiResponse{OperatorId: op, Source: src}, nil
}

// whoamiJSON mirrors the proto field names so the HTTP / Connect-RPC client
// can consume the same payload from either transport.
type whoamiJSON struct {
	OperatorID string `json:"operator_id"`
	Source     string `json:"source"`
}

// handleWhoami handles GET /v1/whoami.
func (s *BeadsServer) handleWhoami(w http.ResponseWriter, r *http.Request) {
	op, src := computeWhoami(r.Context())
	writeJSON(w, http.StatusOK, whoamiJSON{OperatorID: op, Source: src})
}
