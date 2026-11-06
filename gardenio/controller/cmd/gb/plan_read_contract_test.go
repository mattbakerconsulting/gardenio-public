// plan_read_contract_test.go — the beadsapi CONTRACT test for the decoupled
// plan read path (task ka-0000004277, epic ka-0000003451).
//
// plan_read_test.go proves the resolver's ordering/membership logic with a hand
// -written fake (fakePlanReader). That fake can silently drift from the real
// wire: it never speaks HTTP, never sends an Authorization header, and never
// exercises the daemon's actual /v1/beads, /v1/beads/{id}/dependents,
// /v1/batch/beads, /v1/batch/dependencies JSON shapes. This file closes that
// gap by driving the REAL *beadsapi.Client — the exact client `gb` carries in
// main.go — against an in-process daemon that speaks the daemon's wire contract,
// and asserts the property the epic's beachhead rests on:
//
//	gb plan reads a plan + phases + steps from the DAEMON ALONE, authenticating
//	with only a Bearer token — no viewer URL, and (unlike the operator-gated
//	viewer POST verbs that 401 an agent pod) no operator/ForwardAuth header.
//
// A green run here is the CI-permanent, hermetic twin of the live proof the
// task's from-scratch assertion runs against a Keycloak-OIDC daemon (see
// tests/e2e/scripts/test-plan-read-daemon-alone.sh): same client, same four
// endpoints, same read model — no cluster required.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// planContractDaemon is a minimal in-process stand-in for the beads daemon that
// serves exactly the four read endpoints planDaemonReader touches, over the
// daemon's real JSON envelopes (kbeads internal/server/http_batch.go +
// http_beads.go). It is seeded with a bead store + parent-child edge map and
// records every Authorization header + operator header it saw, so the test can
// assert the daemon-alone auth contract, not just the resolved shape.
type planContractDaemon struct {
	t     *testing.T
	beads map[string]beadWire // id → wire bead
	kids  map[string][]string // parent id → ordered parent-child child ids
	token string              // the one Bearer token this daemon accepts

	mu            sync.Mutex
	sawAuthBearer map[string]struct{} // distinct Bearer tokens presented
	sawOperator   bool                // did any request carry X-Operated-By / edge-auth?
	requests      int
}

// beadWire is the subset of the daemon's bead JSON the plan read path consumes.
// Field names mirror the daemon's on-wire shape (beadJSON in client.go) so the
// real client's decoder — not a bespoke one — parses it.
type beadWire struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Type   string            `json:"type"`
	Status string            `json:"status"`
	Labels []string          `json:"labels"`
	Fields map[string]string `json:"fields"`
}

func newPlanContractDaemon(t *testing.T, token string) *planContractDaemon {
	return &planContractDaemon{
		t:             t,
		beads:         map[string]beadWire{},
		kids:          map[string][]string{},
		token:         token,
		sawAuthBearer: map[string]struct{}{},
	}
}

func (d *planContractDaemon) add(b beadWire)                      { d.beads[b.ID] = b }
func (d *planContractDaemon) child(parent string, kids ...string) { d.kids[parent] = kids }

// dependentsOf renders parent→child parent-child edges as the daemon's
// /dependents rows: each row reads "BeadID depends on DependsOnID".
func (d *planContractDaemon) dependentsOf(parent string) []beadsapi.Dependency {
	var deps []beadsapi.Dependency
	for _, cid := range d.kids[parent] {
		deps = append(deps, beadsapi.Dependency{BeadID: cid, DependsOnID: parent, Type: "parent-child"})
	}
	return deps
}

// ServeHTTP implements the daemon read contract. Every handled path first
// enforces the Bearer token (a missing/blank/wrong token is a 401, exactly as
// an OIDC-gated daemon rejects an unauthenticated read), then records whether
// the caller stamped an operator header (it must not need to for reads).
func (d *planContractDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.requests++
	authz := r.Header.Get("Authorization")
	if tok := strings.TrimPrefix(authz, "Bearer "); tok != "" && tok != authz {
		d.sawAuthBearer[tok] = struct{}{}
	}
	if r.Header.Get("X-Operated-By") != "" || r.Header.Get("X-Operator") != "" || r.Header.Get("X-Edge-Auth-Sig") != "" {
		d.sawOperator = true
	}
	d.mu.Unlock()

	// Auth gate: reads REQUIRE a valid Bearer, nothing else.
	if authz != "Bearer "+d.token {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	switch {
	case r.URL.Path == "/v1/batch/beads":
		d.serveBatchBeads(w, r)
	case r.URL.Path == "/v1/batch/dependencies":
		d.serveBatchDeps(w, r)
	case strings.HasSuffix(r.URL.Path, "/dependents"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/dependents")
		id, _ = url.PathUnescape(id)
		d.writeJSON(w, map[string]any{"dependencies": d.dependentsOf(id)})
	case strings.HasPrefix(r.URL.Path, "/v1/beads/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		id, _ = url.PathUnescape(id)
		b, ok := d.beads[id]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		d.writeJSON(w, b)
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotImplemented)
	}
}

// serveBatchBeads answers GET /v1/batch/beads?ids=a,b,c with the daemon's
// {"results":[{"id","bead"}]} envelope (absent ids simply omitted).
func (d *planContractDaemon) serveBatchBeads(w http.ResponseWriter, r *http.Request) {
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	type result struct {
		ID   string    `json:"id"`
		Bead *beadWire `json:"bead,omitempty"`
	}
	var out []result
	for _, id := range ids {
		if b, ok := d.beads[id]; ok {
			bc := b
			out = append(out, result{ID: id, Bead: &bc})
		}
	}
	d.writeJSON(w, map[string]any{"results": out})
}

// serveBatchDeps answers GET /v1/batch/dependencies?ids=...&direction=reverse
// with {"results":[{"id","deps"}]} — every requested id echoed back.
func (d *planContractDaemon) serveBatchDeps(w http.ResponseWriter, r *http.Request) {
	if got := r.URL.Query().Get("direction"); got != "reverse" {
		d.t.Fatalf("batch deps: want direction=reverse, got %q", got)
	}
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	type result struct {
		ID   string                `json:"id"`
		Deps []beadsapi.Dependency `json:"deps"`
	}
	var out []result
	for _, id := range ids {
		out = append(out, result{ID: id, Deps: d.dependentsOf(id)})
	}
	d.writeJSON(w, map[string]any{"results": out})
}

func (d *planContractDaemon) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		d.t.Fatalf("encoding response: %v", err)
	}
}

// TestPlanReadContract_DaemonAloneBearerOnly is the contract: build the REAL
// *beadsapi.Client the way main.go does (HTTPAddr + a static Bearer Token, no
// viewer URL, no operator), point it at the in-process daemon, and prove
// resolvePlanRead resolves a phased plan — while the daemon confirms it only
// ever saw that one Bearer token and no operator header. This is the exact env
// the task calls out: reads work with only BEADS_AUTH_TOKEN, where the viewer
// write verbs 401.
func TestPlanReadContract_DaemonAloneBearerOnly(t *testing.T) {
	const token = "contract-test-bearer-abc123"
	d := newPlanContractDaemon(t, token)

	// A two-phase plan mirroring the live plan the task drives (ka-0000000187):
	// phase_order names two sub-epics, each holding steps whose order is flipped
	// by the phase epic's own step_order, plus one flat step and a bundle child
	// that must be dropped as a non-step type.
	d.add(beadWire{
		ID: "gd-PLAN", Type: "epic", Title: "Decouple CLI reads", Status: "open",
		Labels: []string{"plan", "project:gardenio"},
		Fields: map[string]string{
			"plan_state":  "building",
			"phase_order": `["gd-PH1","gd-PH2"]`,
			"step_order":  `["gd-FLAT"]`,
		},
	})
	d.add(beadWire{ID: "gd-PH1", Type: "epic", Title: "Beachhead", Status: "closed",
		Fields: map[string]string{"step_order": `["gd-S2","gd-S1"]`}})
	d.add(beadWire{ID: "gd-PH2", Type: "epic", Title: "Promote fields", Status: "open"})
	d.add(beadWire{ID: "gd-S1", Type: "task", Title: "S1", Status: "closed"})
	d.add(beadWire{ID: "gd-S2", Type: "task", Title: "S2", Status: "open"})
	d.add(beadWire{ID: "gd-S3", Type: "bug", Title: "S3", Status: "open"})
	d.add(beadWire{ID: "gd-FLAT", Type: "task", Title: "Flat step", Status: "open"})
	d.add(beadWire{ID: "gd-BUNDLE", Type: "bundle", Title: "Narrative", Status: "open"})
	d.child("gd-PLAN", "gd-PH1", "gd-PH2", "gd-FLAT", "gd-BUNDLE")
	d.child("gd-PH1", "gd-S1", "gd-S2")
	d.child("gd-PH2", "gd-S3")

	srv := httptest.NewServer(d)
	defer srv.Close()

	// The REAL client, built the daemon-alone way: HTTPAddr + Token ONLY. No
	// TokenSource, no cookie, no viewer URL, no operatedBy — the exact shape
	// main.go produces from BEADS_AUTH_TOKEN with both BEADS_VIEWER_* unset.
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: token})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	// Compile-time we already know *beadsapi.Client satisfies planDaemonReader
	// (plan_read.go). Bind it through the interface to prove the read path uses
	// nothing beyond that narrow surface.
	var reader planDaemonReader = client

	view, err := resolvePlanRead(context.Background(), reader, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead against live client: %v", err)
	}

	// ── Shape: the plan resolved end-to-end over the wire ────────────────────
	if view.PlanState != "building" || view.Project != "gardenio" || view.Status != "open" {
		t.Fatalf("header wrong: %+v", view)
	}
	if len(view.Phases) != 2 {
		t.Fatalf("want 2 phases, got %d", len(view.Phases))
	}
	ph1 := view.Phases[0]
	if ph1.ID != "gd-PH1" || ph1.Number != 1 || !ph1.Closed {
		t.Fatalf("phase1 header wrong: %+v", ph1)
	}
	if got := []string{ph1.Steps[0].ID, ph1.Steps[1].ID}; got[0] != "gd-S2" || got[1] != "gd-S1" {
		t.Fatalf("phase1 step order = %v, want [gd-S2 gd-S1] (its step_order overlay)", got)
	}
	if ph1.Steps[0].Number != 1 || ph1.Steps[1].Number != 2 {
		t.Fatalf("phase1 numbering wrong: %+v", ph1.Steps)
	}
	ph2 := view.Phases[1]
	if ph2.ID != "gd-PH2" || ph2.Number != 2 || len(ph2.Steps) != 1 || ph2.Steps[0].ID != "gd-S3" {
		t.Fatalf("phase2 wrong: %+v", ph2)
	}
	if len(view.Steps) != 1 || view.Steps[0].ID != "gd-FLAT" {
		t.Fatalf("flat steps = %+v, want just gd-FLAT (bundle dropped, phase steps excluded)", view.Steps)
	}

	// ── Auth contract: daemon-alone, Bearer-only, no operator header ─────────
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.requests == 0 {
		t.Fatal("daemon saw no requests — resolvePlanRead did not talk to the daemon")
	}
	if len(d.sawAuthBearer) != 1 {
		t.Fatalf("want exactly one distinct Bearer token across all reads, saw %d: %v", len(d.sawAuthBearer), d.sawAuthBearer)
	}
	if _, ok := d.sawAuthBearer[token]; !ok {
		t.Fatalf("daemon never saw the configured Bearer token %q; saw %v", token, d.sawAuthBearer)
	}
	if d.sawOperator {
		t.Fatal("a read request carried an operator/edge-auth header — plan reads must need only a Bearer token (the viewer-gated write path is what 401s)")
	}
}

// TestPlanReadContract_RejectsWithoutToken proves the daemon-alone gate is real:
// the SAME client with a wrong token gets 401s, so resolvePlanRead fails. This
// pins the meaning of the happy-path test — the reads succeeded because of the
// Bearer token, not because the daemon is unauthenticated.
func TestPlanReadContract_RejectsWithoutToken(t *testing.T) {
	const realToken = "the-only-good-token"
	d := newPlanContractDaemon(t, realToken)
	d.add(beadWire{ID: "gd-PLAN", Type: "epic", Title: "P", Status: "open"})
	d.child("gd-PLAN")

	srv := httptest.NewServer(d)
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: "wrong-token"})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	if _, err := resolvePlanRead(context.Background(), client, "gd-PLAN"); err == nil {
		t.Fatal("want error resolving a plan with a bad Bearer token, got nil")
	}
}

// TestPlanReadContract_UnphasedPlan drives the flat (no phase_order) path over
// the real client: every direct issue child is a flat step in step_order, and
// resolvePlanRead needs only GetBead + one dependents walk + one batch fetch.
func TestPlanReadContract_UnphasedPlan(t *testing.T) {
	const token = "flat-plan-token"
	d := newPlanContractDaemon(t, token)
	d.add(beadWire{ID: "gd-PLAN", Type: "epic", Title: "Flat plan", Status: "open",
		Fields: map[string]string{"step_order": `["gd-B","gd-A"]`}})
	d.add(beadWire{ID: "gd-A", Type: "task", Title: "A", Status: "open"})
	d.add(beadWire{ID: "gd-B", Type: "feature", Title: "B", Status: "closed"})
	d.child("gd-PLAN", "gd-A", "gd-B")

	srv := httptest.NewServer(d)
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: token})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	view, err := resolvePlanRead(context.Background(), client, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead: %v", err)
	}
	if len(view.Phases) != 0 {
		t.Fatalf("want no phases, got %d", len(view.Phases))
	}
	if got := []string{view.Steps[0].ID, view.Steps[1].ID}; got[0] != "gd-B" || got[1] != "gd-A" {
		t.Fatalf("flat step order = %v, want [gd-B gd-A] (step_order overlay)", got)
	}
}
