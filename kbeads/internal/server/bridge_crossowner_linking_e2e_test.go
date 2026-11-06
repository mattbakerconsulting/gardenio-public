package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// E2E defense for the MR→beads graph-linking regression (gd-XOcerNXFV2,
// epic gd-v9gFo1rRU1). The gitlab-bridge authenticates with a FLEET token
// (identity Type="fleet") and performs SYSTEM-MANAGED graph writes against
// task beads OWNED BY OTHER AGENTS:
//
//   - produces / triggered-by dependency edges (task→MR, MR→task), and
//   - mr_url back-fill (reverse MR→bead reconciler).
//
// The ownership middleware (authorizeOwnership, write_authz.go) bypasses only
// admin-OIDC and the primary static token; ordinary fleet tokens are gated.
// Once the bridge moved to a fleet token, every cross-owner graph write it
// attempted began returning HTTP 403 "ownership denied" — the regression
// diagnosed in gd-QV4p0vqRhF. The pre-existing MR-link e2e coverage never
// caught it because those tests drive the writes with the admin/static token,
// which owns nothing yet bypasses the gate, so the cross-owner path was never
// exercised.
//
// T1 (ka-0000001946) introduces a narrow PRIVILEGED service-fleet capability:
// a designated bridge fleet token may bypass the ownership gate for these
// system-managed writes, while ORDINARY fleet tokens stay gated.
//
// This test is intentionally DECOUPLED from T1's not-yet-landed Go symbols.
// It requests the privileged capability over the wire by sending
// `"privileged": true` in the POST /v1/fleet-tokens body. decodeJSONBody does
// not DisallowUnknownFields, so on `main` (pre-T1) the flag is silently
// ignored: the token stays an ordinary fleet token, the cross-owner writes
// 403, and TestBridgeCrossOwnerMRLinkingE2E FAILS — demonstrating the
// regression. After T1 honors the flag and bypasses authorizeOwnership for the
// privileged identity, the same writes succeed and the test PASSES.
//
// The negative-control test (TestBridgeOrdinaryFleetCrossOwnerStillDenied)
// passes both before AND after T1: it locks in the security boundary so the
// privileged bypass cannot accidentally widen to ordinary fleet tokens.
//
// Everything runs against the full httptest stack (AuthMiddleware →
// requireRole → ownership gate) and is fully deterministic — no reliance on
// the 15-minute reverse-reconciler cadence.

// crossOwnerFixture sets up a server plus a task bead owned by identity A and
// a merge-request bead, both created by the admin/static token but stamped
// with a distinct OIDC owner so the bridge fleet token (identity B) is a
// genuine cross-owner caller.
type crossOwnerFixture struct {
	ts         *httptest.Server
	srv        *BeadsServer
	taskBeadID string
	mrBeadID   string
	ownerA     string
	mrURL      string
}

func newCrossOwnerFixture(t *testing.T, fc *FleetConfig, adminToken string) *crossOwnerFixture {
	t.Helper()
	ts, srv, _ := newE2EServer(t, adminToken, fc)

	const ownerA = "alice@agent.example.com"
	mrURL := "https://gitlab.com/acme/platform/gardenio/-/merge_requests/4242"

	// Task bead owned by identity A. Created with the admin token (which
	// bypasses ownership) but with owner=ownerA so the bridge fleet token is
	// a cross-owner caller against it.
	taskBeadID := createTaskBeadOwnedBy(t, ts, adminToken, "E2E xowner: producing task", ownerA)

	// The type=merge-request bead the bridge dual-writes. Owned by ownerA too
	// (it carries its own mr_url and is linked to the producing task).
	mrBeadID := createMRBead(t, ts, adminToken, "E2E xowner: MR bead", ownerA, mrURL)

	return &crossOwnerFixture{
		ts:         ts,
		srv:        srv,
		taskBeadID: taskBeadID,
		mrBeadID:   mrBeadID,
		ownerA:     ownerA,
		mrURL:      mrURL,
	}
}

// createTaskBeadOwnedBy creates a task bead with owner set to ownerA via the
// admin token and returns its ID.
func createTaskBeadOwnedBy(t *testing.T, ts *httptest.Server, token, title, ownerA string) string {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", token, createBeadInput{
		Title:  title,
		Type:   "task",
		Owner:  ownerA,
		Labels: []string{"project:gardenio"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create task bead: expected 201, got %d; body: %s", resp.StatusCode, body)
	}
	var bead model.Bead
	decodeBody(t, resp, &bead)
	if bead.ID == "" {
		t.Fatal("create task bead: empty ID")
	}
	return bead.ID
}

// createMRBead creates a type=merge-request bead carrying mr_url, owned by
// ownerA, via the admin token.
func createMRBead(t *testing.T, ts *httptest.Server, token, title, ownerA, mrURL string) string {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", token, createBeadInput{
		Title:  title,
		Type:   "merge-request",
		Owner:  ownerA,
		Labels: []string{"project:gardenio"},
		Fields: mustJSON(map[string]any{"mr_url": mrURL}),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create MR bead: expected 201, got %d; body: %s", resp.StatusCode, body)
	}
	var bead model.Bead
	decodeBody(t, resp, &bead)
	if bead.ID == "" {
		t.Fatal("create MR bead: empty ID")
	}
	return bead.ID
}

// registerFleetToken registers a fleet token directly on the FleetConfig,
// optionally with the privileged capability (T1). It does not go through
// POST /v1/fleet-tokens: minting a privileged token over HTTP needs the
// token-mint machine capability (gd-ua4DGNtZdG), which is not what these tests
// exercise.
func registerFleetToken(fc *FleetConfig, token, fleet, subject string, privileged bool) {
	fc.RegisterTokenFull(token, fleet, "", nil, nil, time.Time{}, subject, privileged)
}

// addDep POSTs a dependency edge as the given token and returns the HTTP status.
func addDep(t *testing.T, ts *httptest.Server, token, beadID, dependsOnID, depType string) int {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads/"+beadID+"/dependencies", token,
		map[string]string{"depends_on_id": dependsOnID, "type": depType})
	defer resp.Body.Close()
	return resp.StatusCode
}

// setMRURL PATCHes the bead's mr_url field as the given token and returns the
// HTTP status.
func setMRURL(t *testing.T, ts *httptest.Server, token, beadID, mrURL string) int {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodPatch, "/v1/beads/"+beadID, token,
		map[string]any{"fields": map[string]any{"mr_url": mrURL}})
	defer resp.Body.Close()
	return resp.StatusCode
}

// hasDepOfType reports whether a dependency of the given type from beadID to
// dependsOnID exists, read back via GET /v1/beads/{id}/dependencies?type=.
func hasDepOfType(t *testing.T, ts *httptest.Server, token, beadID, dependsOnID, depType string) bool {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodGet,
		"/v1/beads/"+beadID+"/dependencies?type="+depType, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("get dependencies: expected 200, got %d; body: %s", resp.StatusCode, body)
	}
	var out struct {
		Dependencies []*model.Dependency `json:"dependencies"`
	}
	decodeBody(t, resp, &out)
	for _, d := range out.Dependencies {
		if d.DependsOnID == dependsOnID && string(d.Type) == depType {
			return true
		}
	}
	return false
}

// beadMRURL reads back the bead's mr_url field.
func beadMRURL(t *testing.T, ts *httptest.Server, token, beadID string) string {
	t.Helper()
	resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads/"+beadID, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("get bead: expected 200, got %d; body: %s", resp.StatusCode, body)
	}
	var bead model.Bead
	decodeBody(t, resp, &bead)
	if len(bead.Fields) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal bead fields: %v", err)
	}
	v, _ := fields["mr_url"].(string)
	return v
}

// TestBridgeCrossOwnerMRLinkingE2E is the fails-pre-fix / passes-post-fix
// guard. A PRIVILEGED bridge fleet token (identity B) must be able to perform
// the bridge's system-managed graph writes against a task bead owned by
// identity A:
//
//  1. produces edge task→MR,
//  2. triggered-by edge MR→task,
//  3. mr_url back-fill on the task bead.
//
// Pre-T1: `privileged` is ignored, the token is ordinary fleet, every write
// 403s, and this test FAILS (demonstrating the regression). Post-T1: the
// privileged bypass lets the writes through and this test PASSES.
func TestBridgeCrossOwnerMRLinkingE2E(t *testing.T) {
	const (
		adminToken    = "admin-xowner-e2e"
		bridgeToken   = "bridge-priv-xowner-tok"
		bridgeFleet   = "gitlab-bridge"
		bridgeSubject = "bridge:gitlab"
	)

	fc := NewFleetConfig("gardenio", nil)
	fx := newCrossOwnerFixture(t, fc, adminToken)

	// Register the bridge fleet token AS PRIVILEGED (the capability T1 adds).
	registerFleetToken(fc, bridgeToken, bridgeFleet, bridgeSubject, true)

	// Sanity: it is a fleet token, distinct from the bead owner.
	if got := fc.ResolveFleet(bridgeToken); got != bridgeFleet {
		t.Fatalf("bridge token fleet = %q, want %q", got, bridgeFleet)
	}

	t.Run("ProducesEdge_TaskToMR", func(t *testing.T) {
		if code := addDep(t, fx.ts, bridgeToken, fx.taskBeadID, fx.mrBeadID, "produces"); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("privileged bridge fleet token should add produces edge on cross-owner task bead, got HTTP %d (regression: ownership gate denied the bridge — needs T1 ka-0000001946)", code)
		}
		if !hasDepOfType(t, fx.ts, adminToken, fx.taskBeadID, fx.mrBeadID, "produces") {
			t.Fatal("produces edge task→MR not present after privileged write")
		}
	})

	t.Run("TriggeredByEdge_MRToTask", func(t *testing.T) {
		if code := addDep(t, fx.ts, bridgeToken, fx.mrBeadID, fx.taskBeadID, "triggered-by"); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("privileged bridge fleet token should add triggered-by edge on cross-owner MR bead, got HTTP %d (regression — needs T1 ka-0000001946)", code)
		}
		if !hasDepOfType(t, fx.ts, adminToken, fx.mrBeadID, fx.taskBeadID, "triggered-by") {
			t.Fatal("triggered-by edge MR→task not present after privileged write")
		}
	})

	t.Run("MRURLBackfill_OnCrossOwnerTask", func(t *testing.T) {
		if code := setMRURL(t, fx.ts, bridgeToken, fx.taskBeadID, fx.mrURL); code != http.StatusOK {
			t.Fatalf("privileged bridge fleet token should back-fill mr_url on cross-owner task bead, got HTTP %d (regression — needs T1 ka-0000001946)", code)
		}
		if got := beadMRURL(t, fx.ts, adminToken, fx.taskBeadID); got != fx.mrURL {
			t.Fatalf("mr_url back-fill = %q, want %q", got, fx.mrURL)
		}
	})
}

// TestBridgeOrdinaryFleetCrossOwnerStillDenied is the security lock-in. An
// ORDINARY (non-privileged) fleet token attempting the same cross-owner writes
// must STILL get 403 ownership denied — both before and after T1. This guards
// against the privileged bypass accidentally widening to every fleet token.
func TestBridgeOrdinaryFleetCrossOwnerStillDenied(t *testing.T) {
	const (
		adminToken    = "admin-xowner-neg-e2e"
		workerToken   = "ordinary-fleet-xowner-tok"
		workerFleet   = "some-worker-fleet"
		workerSubject = "fleet:some-worker"
	)

	fc := NewFleetConfig("gardenio", nil)
	fx := newCrossOwnerFixture(t, fc, adminToken)

	// Register an ORDINARY fleet token (privileged=false).
	registerFleetToken(fc, workerToken, workerFleet, workerSubject, false)

	t.Run("ProducesEdge_Denied", func(t *testing.T) {
		if code := addDep(t, fx.ts, workerToken, fx.taskBeadID, fx.mrBeadID, "produces"); code != http.StatusForbidden {
			t.Fatalf("ordinary fleet token must be DENIED produces edge on cross-owner bead, got HTTP %d (security boundary regressed — ownership gate must still apply to non-privileged fleet tokens)", code)
		}
	})

	t.Run("MRURLBackfill_Denied", func(t *testing.T) {
		if code := setMRURL(t, fx.ts, workerToken, fx.taskBeadID, fx.mrURL); code != http.StatusForbidden {
			t.Fatalf("ordinary fleet token must be DENIED mr_url back-fill on cross-owner bead, got HTTP %d (security boundary regressed)", code)
		}
		// The write must not have taken effect.
		if got := beadMRURL(t, fx.ts, adminToken, fx.taskBeadID); got != "" {
			t.Fatalf("denied write should not set mr_url, but got %q", got)
		}
	})
}

// TestBridgeReverseReconcilerPrivilegedSetsMRURL covers the reverse MR→bead
// reconciler path explicitly (gitlab_mr_backlink.go): a privileged fleet
// identity sets mr_url on a bead it does not own. This is the write that the
// live reverse reconciler reported as "errors=22 ... ownership denied" before
// the fix. Passes only post-T1.
func TestBridgeReverseReconcilerPrivilegedSetsMRURL(t *testing.T) {
	const (
		adminToken    = "admin-reverse-e2e"
		bridgeToken   = "bridge-reverse-priv-tok"
		bridgeFleet   = "gitlab-bridge"
		bridgeSubject = "bridge:gitlab"
	)

	fc := NewFleetConfig("gardenio", nil)
	ts, _, _ := newE2EServer(t, adminToken, fc)

	const ownerA = "carol@agent.example.com"
	mrURL := "https://gitlab.com/acme/platform/gardenio/-/merge_requests/777"

	// A task bead owned by identity A with NO mr_url yet — the reconciler's
	// input shape (an MR comment/back-link matched this bead but it lacks the
	// projection field).
	taskBeadID := createTaskBeadOwnedBy(t, ts, adminToken, "E2E reverse: unlinked task", ownerA)

	registerFleetToken(fc, bridgeToken, bridgeFleet, bridgeSubject, true)

	if code := setMRURL(t, ts, bridgeToken, taskBeadID, mrURL); code != http.StatusOK {
		t.Fatalf("privileged bridge fleet token (reverse reconciler) should set mr_url on non-owned bead, got HTTP %d (regression — needs T1 ka-0000001946)", code)
	}
	if got := beadMRURL(t, ts, adminToken, taskBeadID); got != mrURL {
		t.Fatalf("reverse reconciler mr_url = %q, want %q", got, mrURL)
	}
}
