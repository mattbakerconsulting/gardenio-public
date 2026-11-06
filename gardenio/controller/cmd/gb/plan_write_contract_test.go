// plan_write_contract_test.go — the beadsapi CONTRACT test for the decoupled
// plan WRITE path (task ka-0000000697, epic ka-0000005932).
//
// This is the write-side twin of plan_read_contract_test.go. plan_write_test.go
// proves each verb's mint/link/order/lifecycle logic with a hand-written fake
// (fakePlanWriter). That fake can silently drift from the real wire: it never
// speaks HTTP, never sends an X-Operated-By header, and never exercises the
// daemon's actual /v1/beads[/…] JSON shapes — so it CANNOT prove the one
// invariant this task owns:
//
//	After the viewer-POST → daemon repoint, every gb plan WRITE preserves the
//	operator-identity validation (operatorEmailForViewer's isHumanOperator
//	mirror) AND stamps oidc:<email> DIRECTLY on the daemon write via
//	beadsapi.WithOperator — i.e. the wire carries `X-Operated-By: oidc:<email>`,
//	NOT the deleted viewer X-Auth-Request-Email header.
//
// This file closes that gap by driving the REAL *beadsapi.Client — the exact
// client `gb` carries in main.go — against an in-process daemon that speaks the
// daemon's write contract, and asserting on the ACTUAL wire header every mutating
// verb sends. A green run here is the CI-permanent, hermetic proof that:
//
//   - the create / add-step / add-phase / reorder-phase / complete / reopen verbs
//     each authenticate with only a Bearer token and stamp X-Operated-By:
//     oidc:<email> on every mutating request (never the viewer's
//     X-Auth-Request-Email / X-Auth-Request-User);
//   - the value stamped is the canonical oidc:<lowercased-email> the daemon's
//     isHumanOperator accepts — so the kbeads R2 fail-closed audit guard
//     (ka-0000005772) admits the write;
//   - operatorEmailForViewer still REJECTS a non-human operator (slack:U…, a bare
//     agent name, a sentinel) BEFORE any request reaches the daemon, so a
//     mis-attributed write can never leave the CLI.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// planWriteContractDaemon is a minimal in-process stand-in for the beads daemon
// that serves exactly the read + write endpoints the plan WRITE verbs touch, over
// the daemon's real JSON envelopes. It is seeded with a bead store + parent-child
// edge map and records, per mutating request, the operator header it saw — so the
// test asserts the daemon-alone auth contract (Bearer-only, oidc:<email> operator),
// not merely the resolved shape.
type planWriteContractDaemon struct {
	t     *testing.T
	token string // the one Bearer token this daemon accepts

	mu    sync.Mutex
	beads map[string]*wireBead // id → mutable wire bead
	kids  map[string][]string  // parent id → ordered parent-child child ids
	seq   int                  // id minter for POST /v1/beads

	// mutating-request audit: one row per write (create/dep/append/reorder/
	// patch/close/comment) with the operator + auth headers the wire carried.
	writes []wireWrite
	// legacyAuthSeen records if ANY request (read or write) carried a deleted
	// viewer auth header — its presence is a contract failure.
	legacyAuthSeen bool
}

// wireBead is the mutable server-side bead. Fields is the daemon's string→string
// field map; phase_order rides it as a JSON-array string, exactly as the daemon
// serves it (ParseFieldsJSON re-marshals array values back to their JSON text).
type wireBead struct {
	ID     string
	Title  string
	Type   string
	Status string
	Labels []string
	Fields map[string]string
}

// wireWrite captures the auth surface of a single mutating request: which verb/
// path, the Bearer token, the resolved operator header, and whether any deleted
// viewer auth header rode along.
type wireWrite struct {
	method     string
	path       string
	bearer     string
	operatedBy string // X-Operated-By
	legacyAuth bool   // any viewer X-Auth-Request-* header present
}

func newPlanWriteContractDaemon(t *testing.T, token string) *planWriteContractDaemon {
	return &planWriteContractDaemon{
		t:     t,
		token: token,
		beads: map[string]*wireBead{},
		kids:  map[string][]string{},
	}
}

func (d *planWriteContractDaemon) add(b *wireBead) {
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	d.beads[b.ID] = b
}

func (d *planWriteContractDaemon) child(parent string, kids ...string) {
	d.kids[parent] = append(d.kids[parent], kids...)
}

// operatorWrites returns the operator header stamped on every MUTATING request,
// in order — the sequence the test asserts is uniformly oidc:<email>.
func (d *planWriteContractDaemon) operatorWrites() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.writes))
	for _, w := range d.writes {
		out = append(out, w.operatedBy)
	}
	return out
}

// isMutating reports whether a method mutates — GET is the only read verb the
// plan write path issues (loadPlanEpic, reverse-deps, batch reads).
func isMutating(method string) bool { return method != http.MethodGet }

// ServeHTTP implements the daemon write contract. Every path first enforces the
// Bearer token (a missing/blank/wrong token is a 401, exactly as an OIDC-gated
// daemon rejects an unauthenticated write), records the auth surface of every
// mutating request, then dispatches to the matching handler.
func (d *planWriteContractDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")

	// A deleted viewer auth header on ANY request is a hard contract violation —
	// the repoint removed the X-Auth-Request-Email machinery for good.
	legacy := r.Header.Get("X-Auth-Request-Email") != "" ||
		r.Header.Get("X-Auth-Request-User") != "" ||
		r.Header.Get("X-Forwarded-Email") != ""

	d.mu.Lock()
	if legacy {
		d.legacyAuthSeen = true
	}
	if isMutating(r.Method) {
		d.writes = append(d.writes, wireWrite{
			method:     r.Method,
			path:       r.URL.Path,
			bearer:     strings.TrimPrefix(authz, "Bearer "),
			operatedBy: r.Header.Get("X-Operated-By"),
			legacyAuth: legacy,
		})
	}
	d.mu.Unlock()

	// Auth gate: reads AND writes REQUIRE a valid Bearer, nothing else.
	if authz != "Bearer "+d.token {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
		d.handleCreate(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
		d.handleList(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dependencies"):
		d.handleAddDependency(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dependents"):
		d.handleDependents(w, r)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/fields/") && strings.HasSuffix(r.URL.Path, "/append"):
		d.handleAppend(w, r)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/fields/") && strings.HasSuffix(r.URL.Path, "/reorder"):
		d.handleReorder(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
		d.handleClose(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		w.WriteHeader(http.StatusNoContent) // best-effort audit comment
	case r.URL.Path == "/v1/batch/beads":
		d.handleBatchBeads(w, r)
	case r.URL.Path == "/v1/batch/dependencies":
		d.handleBatchDeps(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
		d.handlePatch(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
		d.handleGet(w, r)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

// beadIDFromPath extracts the {id} from /v1/beads/{id}[/suffix…].
func beadIDFromPath(path string, trimSuffixes ...string) string {
	rest := strings.TrimPrefix(path, "/v1/beads/")
	for _, s := range trimSuffixes {
		rest = strings.TrimSuffix(rest, s)
	}
	id, _ := url.PathUnescape(rest)
	return id
}

// wireJSON is the on-wire bead JSON the daemon serves (subset). Fields is an
// object whose values are strings / JSON — ParseFieldsJSON on the client re-reads
// it into the string map. Emitting it as map[string]json.RawMessage lets the
// phase_order array ride as its native JSON text.
func (d *planWriteContractDaemon) wireJSON(b *wireBead) map[string]any {
	fields := map[string]json.RawMessage{}
	for k, v := range b.Fields {
		// phase_order / step_order are stored as JSON-array strings; emit them as
		// the raw JSON so the client decodes an array. Everything else is a plain
		// JSON string.
		if json.Valid([]byte(v)) && (strings.HasPrefix(strings.TrimSpace(v), "[") || strings.HasPrefix(strings.TrimSpace(v), "{")) {
			fields[k] = json.RawMessage(v)
		} else {
			enc, _ := json.Marshal(v)
			fields[k] = enc
		}
	}
	return map[string]any{
		"id":     b.ID,
		"title":  b.Title,
		"type":   b.Type,
		"status": b.Status,
		"labels": b.Labels,
		"fields": fields,
	}
}

func (d *planWriteContractDaemon) handleGet(w http.ResponseWriter, r *http.Request) {
	id := beadIDFromPath(r.URL.Path)
	d.mu.Lock()
	b, ok := d.beads[id]
	d.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	d.writeJSON(w, d.wireJSON(b))
}

func (d *planWriteContractDaemon) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req beadsapi.CreateBeadRequest
	d.decode(r, &req)
	d.mu.Lock()
	d.seq++
	id := "gd-W" + string(rune('A'+d.seq-1))
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		var fm map[string]string
		if json.Unmarshal(req.Fields, &fm) == nil {
			fields = fm
		}
	}
	b := &wireBead{ID: id, Title: req.Title, Type: req.Type, Status: "open", Labels: append([]string(nil), req.Labels...), Fields: fields}
	d.beads[id] = b
	if req.ParentBeadID != "" {
		d.kids[req.ParentBeadID] = append(d.kids[req.ParentBeadID], id)
	}
	d.mu.Unlock()
	d.writeJSON(w, d.wireJSON(b))
}

func (d *planWriteContractDaemon) handleAddDependency(w http.ResponseWriter, r *http.Request) {
	child := beadIDFromPath(r.URL.Path, "/dependencies")
	var body struct {
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	}
	d.decode(r, &body)
	if body.Type == "parent-child" {
		d.mu.Lock()
		d.kids[body.DependsOnID] = append(d.kids[body.DependsOnID], child)
		d.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *planWriteContractDaemon) handleDependents(w http.ResponseWriter, r *http.Request) {
	id := beadIDFromPath(r.URL.Path, "/dependents")
	d.writeJSON(w, map[string]any{"dependencies": d.dependentsOf(id)})
}

func (d *planWriteContractDaemon) dependentsOf(parent string) []beadsapi.Dependency {
	d.mu.Lock()
	defer d.mu.Unlock()
	var deps []beadsapi.Dependency
	for _, cid := range d.kids[parent] {
		deps = append(deps, beadsapi.Dependency{BeadID: cid, DependsOnID: parent, Type: "parent-child"})
	}
	return deps
}

func (d *planWriteContractDaemon) handleAppend(w http.ResponseWriter, r *http.Request) {
	// /v1/beads/{id}/fields/{field}/append
	rest := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
	parts := strings.Split(rest, "/")
	id, _ := url.PathUnescape(parts[0])
	field, _ := url.PathUnescape(parts[2])
	var body struct {
		Element string `json:"element"`
	}
	d.decode(r, &body)
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.beads[id]
	if b == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	cur := decodeArray(b.Fields[field])
	if slices.Contains(cur, body.Element) { // append-if-absent
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cur = append(cur, body.Element)
	b.Fields[field] = encodeArray(cur)
	w.WriteHeader(http.StatusNoContent)
}

func (d *planWriteContractDaemon) handleReorder(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
	parts := strings.Split(rest, "/")
	id, _ := url.PathUnescape(parts[0])
	field, _ := url.PathUnescape(parts[2])
	var body struct {
		Expected []string `json:"expected"`
		Next     []string `json:"next"`
	}
	d.decode(r, &body)
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.beads[id]
	if b == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Compare-and-swap: write next ONLY IF the stored array still equals expected.
	if !slices.Equal(decodeArray(b.Fields[field]), body.Expected) {
		http.Error(w, `{"error":"conflict"}`, http.StatusConflict)
		return
	}
	b.Fields[field] = encodeArray(body.Next)
	w.WriteHeader(http.StatusNoContent)
}

func (d *planWriteContractDaemon) handlePatch(w http.ResponseWriter, r *http.Request) {
	id := beadIDFromPath(r.URL.Path)
	var body map[string]json.RawMessage
	d.decode(r, &body)
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.beads[id]
	if b == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if raw, ok := body["status"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			b.Status = s
		}
	}
	if raw, ok := body["fields"]; ok {
		var fm map[string]any
		if json.Unmarshal(raw, &fm) == nil {
			for k, v := range fm {
				if s, isStr := v.(string); isStr {
					b.Fields[k] = s
				} else {
					enc, _ := json.Marshal(v)
					b.Fields[k] = string(enc)
				}
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *planWriteContractDaemon) handleClose(w http.ResponseWriter, r *http.Request) {
	id := beadIDFromPath(r.URL.Path, "/close")
	d.mu.Lock()
	if b := d.beads[id]; b != nil {
		b.Status = "closed"
	}
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleList answers GET /v1/beads with the {"beads":[…],"total":N} envelope.
// completePlan's gardener-disable path calls ListBeadsFiltered; an empty result
// (no bound gardener) is the common case, so return no beads.
func (d *planWriteContractDaemon) handleList(w http.ResponseWriter, _ *http.Request) {
	d.writeJSON(w, map[string]any{"beads": []any{}, "total": 0})
}

func (d *planWriteContractDaemon) handleBatchBeads(w http.ResponseWriter, r *http.Request) {
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	type result struct {
		ID   string         `json:"id"`
		Bead map[string]any `json:"bead,omitempty"`
	}
	var out []result
	d.mu.Lock()
	for _, id := range ids {
		if b, ok := d.beads[id]; ok {
			out = append(out, result{ID: id, Bead: d.wireJSON(b)})
		}
	}
	d.mu.Unlock()
	d.writeJSON(w, map[string]any{"results": out})
}

func (d *planWriteContractDaemon) handleBatchDeps(w http.ResponseWriter, r *http.Request) {
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

func (d *planWriteContractDaemon) decode(r *http.Request, v any) {
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		if err := json.Unmarshal(body, v); err != nil {
			d.t.Fatalf("decoding %s %s body: %v", r.Method, r.URL.Path, err)
		}
	}
}

func (d *planWriteContractDaemon) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		d.t.Fatalf("encoding response: %v", err)
	}
}

// decodeArray / encodeArray mirror the daemon's JSON-array field storage.
func decodeArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ids []string
	_ = json.Unmarshal([]byte(raw), &ids)
	return ids
}

func encodeArray(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

// realClientForContract builds the REAL *beadsapi.Client the daemon-alone way —
// HTTPAddr + a static Bearer Token ONLY, no viewer URL, no operator default —
// exactly the shape main.go produces from BEADS_AUTH_TOKEN with both BEADS_VIEWER_*
// unset. The per-call operator rides ctx via withPlanOperator, never a client
// default, so the wire header proves the WithOperator stamping (not SetOperatedBy).
func realClientForContract(t *testing.T, addr, token string) planDaemonWriter {
	t.Helper()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr, Token: token})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	// Compile-time we already know *beadsapi.Client satisfies planDaemonWriter;
	// bind through the interface so the verbs use nothing beyond that surface.
	return client
}

const contractOperatorEmail = "matt@acme.example"
const contractOperatorID = "oidc:" + contractOperatorEmail

// assertDaemonAloneWrites is the shared post-condition every verb subtest asserts:
// at least one mutating request landed, EVERY mutating request carried the one
// valid Bearer AND X-Operated-By: oidc:<email>, and NO request (read or write)
// ever carried a deleted viewer auth header.
func assertDaemonAloneWrites(t *testing.T, d *planWriteContractDaemon, token string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.writes) == 0 {
		t.Fatal("daemon saw no mutating requests — the verb never wrote to the daemon")
	}
	if d.legacyAuthSeen {
		t.Fatal("a request carried a deleted viewer auth header (X-Auth-Request-Email/User) — the repoint must send X-Operated-By only")
	}
	for _, wr := range d.writes {
		if wr.bearer != token {
			t.Errorf("%s %s: Bearer = %q, want the one configured token %q", wr.method, wr.path, wr.bearer, token)
		}
		if wr.operatedBy != contractOperatorID {
			t.Errorf("%s %s: X-Operated-By = %q, want %q (oidc:<email> stamped via WithOperator)", wr.method, wr.path, wr.operatedBy, contractOperatorID)
		}
		if wr.legacyAuth {
			t.Errorf("%s %s: carried a viewer X-Auth-Request-* header — must be X-Operated-By only", wr.method, wr.path)
		}
	}
}

// TestPlanWriteContract_StampsOIDCOperatorOverWire is the contract, driven for
// every gb plan WRITE verb: build the REAL client daemon-alone (Bearer only, no
// operator default), run the verb on behalf of an oidc:<email> operator, and
// assert the daemon saw every mutating request stamped with the ONE Bearer token
// AND X-Operated-By: oidc:<email> — and never a deleted viewer auth header. This
// is the wire-level proof of the invariant the task owns: operator identity is
// preserved and stamped DIRECTLY on the daemon write via WithOperator.
func TestPlanWriteContract_StampsOIDCOperatorOverWire(t *testing.T) {
	const token = "write-contract-bearer-xyz789"

	// Each subtest gets a fresh daemon + a plan epic seeded to exercise its verb.
	// The verbs are the composition helpers the cobra RunE bodies call (plan.go /
	// plan_phase.go / plan_create.go), so this asserts the exact production path.
	t.Run("create", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		if _, err := createPlanEpicDaemon(context.Background(), w, "Decouple the CLI writes", "gardenio", contractOperatorEmail); err != nil {
			t.Fatalf("createPlanEpicDaemon: %v", err)
		}
		assertDaemonAloneWrites(t, d, token)
	})

	t.Run("add-step", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		if _, err := addPlanStep(context.Background(), w, "gd-PLAN", "Wire the mirror", "", contractOperatorEmail); err != nil {
			t.Fatalf("addPlanStep: %v", err)
		}
		assertDaemonAloneWrites(t, d, token)
	})

	t.Run("add-phase", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		res, err := addPlanPhase(context.Background(), w, "gd-PLAN", "Research & design", contractOperatorEmail)
		if err != nil {
			t.Fatalf("addPlanPhase: %v", err)
		}
		// The phase id must have landed in phase_order via the atomic append.
		d.mu.Lock()
		order := decodeArray(d.beads["gd-PLAN"].Fields[planPhaseFieldKey])
		d.mu.Unlock()
		if len(order) != 1 || order[0] != res.ID {
			t.Fatalf("phase_order = %v, want [%s] (atomic append landed)", order, res.ID)
		}
		assertDaemonAloneWrites(t, d, token)
	})

	t.Run("reorder-phase", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		// A plan with two rendered phases; move the second up over the wire CAS.
		d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open",
			Labels: []string{"plan", "project:gardenio"},
			Fields: map[string]string{planPhaseFieldKey: `["gd-PH1","gd-PH2"]`}})
		d.add(&wireBead{ID: "gd-PH1", Type: "epic", Status: "open"})
		d.add(&wireBead{ID: "gd-PH2", Type: "epic", Status: "open"})
		d.child("gd-PLAN", "gd-PH1", "gd-PH2")
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		res, err := reorderPlanPhase(context.Background(), w, "gd-PLAN", "gd-PH2", "up", contractOperatorEmail)
		if err != nil {
			t.Fatalf("reorderPlanPhase: %v", err)
		}
		if res.Status != "ok" {
			t.Fatalf("reorder status = %q, want ok (a real permute landed)", res.Status)
		}
		d.mu.Lock()
		order := decodeArray(d.beads["gd-PLAN"].Fields[planPhaseFieldKey])
		d.mu.Unlock()
		if !slices.Equal(order, []string{"gd-PH2", "gd-PH1"}) {
			t.Fatalf("phase_order after reorder = %v, want [gd-PH2 gd-PH1]", order)
		}
		assertDaemonAloneWrites(t, d, token)
	})

	t.Run("complete", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open",
			Labels: []string{"plan", "project:gardenio"},
			Fields: map[string]string{planStateFieldKey: "building"}})
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		res, err := completePlan(context.Background(), w, "gd-PLAN", contractOperatorEmail)
		if err != nil {
			t.Fatalf("completePlan: %v", err)
		}
		if res.Status != "closed" || res.PlanState != "complete" {
			t.Fatalf("complete result = %+v, want {closed, complete}", res)
		}
		assertDaemonAloneWrites(t, d, token)
	})

	t.Run("reopen", func(t *testing.T) {
		d := newPlanWriteContractDaemon(t, token)
		d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "closed",
			Labels: []string{"plan", "project:gardenio"},
			Fields: map[string]string{planStateFieldKey: "complete"}})
		srv := httptest.NewServer(d)
		defer srv.Close()
		w := realClientForContract(t, srv.URL, token)

		res, err := reopenPlan(context.Background(), w, "gd-PLAN", contractOperatorEmail)
		if err != nil {
			t.Fatalf("reopenPlan: %v", err)
		}
		if res.Status != "open" || res.PlanState != "building" {
			t.Fatalf("reopen result = %+v, want {open, building}", res)
		}
		assertDaemonAloneWrites(t, d, token)
	})
}

// TestPlanWriteContract_StampsSlackOperatorOverWire proves an AGENT GARDENER's
// operator — slack:U… (the human who spawned it, the shape $BEADS_OPERATED_BY
// carries in an agent pod) — reaches the daemon stamped VERBATIM on X-Operated-By.
// This is the wire-level proof of the ka-0000005347 design: agent gardeners drive
// the gb plan write verbs attributed to the real human behind them, NOT a svc:
// machine identity, so the "operator is always a human" invariant (ka-0000006809)
// holds. slack:U… passes canonicalPlanOperator UNCHANGED (no re-prefixing).
func TestPlanWriteContract_StampsSlackOperatorOverWire(t *testing.T) {
	const token = "slack-op-token"
	const slackOperator = "slack:U0OPERATOR1"
	d := newPlanWriteContractDaemon(t, token)
	d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	srv := httptest.NewServer(d)
	defer srv.Close()
	w := realClientForContract(t, srv.URL, token)

	if _, err := addPlanStep(context.Background(), w, "gd-PLAN", "step by a gardener", "", slackOperator); err != nil {
		t.Fatalf("addPlanStep with slack operator: %v", err)
	}
	wrote := d.operatorWrites()
	if len(wrote) == 0 {
		t.Fatal("daemon saw no operator-stamped writes")
	}
	for _, op := range wrote {
		if op != slackOperator {
			t.Errorf("stamped operator = %q, want %q (slack:U… verbatim, never re-prefixed)", op, slackOperator)
		}
	}
}

// TestPlanWriteContract_LowercasesOperatorLikeDaemon proves the stamped operator
// is the CANONICAL oidc:<lowercased-email> the daemon's isHumanOperator keys on:
// a mixed-case operator email must reach the wire lowercased (canonicalPlanOperator
// mirrors the viewer's canonicalOIDC), so the kbeads audit resolver admits it.
func TestPlanWriteContract_LowercasesOperatorLikeDaemon(t *testing.T) {
	const token = "case-token"
	d := newPlanWriteContractDaemon(t, token)
	d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	srv := httptest.NewServer(d)
	defer srv.Close()
	w := realClientForContract(t, srv.URL, token)

	// operatorEmailForViewer returns the bare email verbatim; withPlanOperator
	// lowercases it into the canonical id. Feed a mixed-case address.
	if _, err := addPlanStep(context.Background(), w, "gd-PLAN", "step", "", "Matt.Baker@acme.example"); err != nil {
		t.Fatalf("addPlanStep: %v", err)
	}
	for _, op := range d.operatorWrites() {
		if op != "oidc:matt.baker@acme.example" {
			t.Errorf("stamped operator = %q, want oidc:matt.baker@acme.example (canonical lowercased)", op)
		}
	}
}

// TestPlanWriteContract_RejectsWithoutToken pins the meaning of the happy path:
// the SAME real client with a WRONG Bearer gets 401s, so the write verb fails.
// The verbs succeed above because of the token, not an unauthenticated daemon.
func TestPlanWriteContract_RejectsWithoutToken(t *testing.T) {
	const realToken = "the-only-good-write-token"
	d := newPlanWriteContractDaemon(t, realToken)
	d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	srv := httptest.NewServer(d)
	defer srv.Close()
	w := realClientForContract(t, srv.URL, "wrong-token")

	if _, err := addPlanStep(context.Background(), w, "gd-PLAN", "step", "", contractOperatorEmail); err == nil {
		t.Fatal("want error adding a step with a bad Bearer token, got nil")
	}
}

// TestPlanWriteContract_ValidationRejectsNonHumanBeforeWire proves the operator
// -identity validation this task preserves fires BEFORE any request reaches the
// daemon. It drives the REAL cobra RunE (runPlanAddStep) — bound to the REAL
// client via the planCmdWriter seam — for each NON-HUMAN operated-by (a machine
// sink svc:/system:, a bare agent name, an over-narrow oidc id, a malformed slack
// id, an empty id). The RunE must error at the operatorEmailForViewer gate, and
// the daemon must record ZERO mutating requests: a mis-attributed write can never
// leave the CLI, even though a live client is wired and ready. A well-formed
// slack:U… (a real human behind an agent) is NOT here — it is accepted, proven by
// TestPlanWriteContract_StampsSlackOperatorOverWire. This is the end-to-end
// companion to the happy-path stamping tests — same seam, same wiring, opposite
// outcome.
func TestPlanWriteContract_ValidationRejectsNonHumanBeforeWire(t *testing.T) {
	const token = "validation-token"
	nonHuman := []string{
		"svc:steering-gardener",      // a machine/service sink → rejected (ka-0000005347)
		"system:machine",             // the machine operator sink → not a human
		"gardenio-steering-gardener", // a bare agent name
		"system:controller",          // a sentinel
		"oidc:matt@localhost",        // no dotted domain (daemon rejects too)
		"slack:lowercaseid",          // slack shape but not a valid U<id>
		"",                           // no operator at all
	}
	for _, op := range nonHuman {
		t.Run(op, func(t *testing.T) {
			d := newPlanWriteContractDaemon(t, token)
			d.add(&wireBead{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
			srv := httptest.NewServer(d)
			defer srv.Close()

			// Bind the REAL client behind the planCmdWriter seam the RunE reads, so
			// the ONLY thing standing between the command and a live write is the
			// operatorEmailForViewer gate this task preserves.
			withFakePlanWriter(t, realClientForContract(t, srv.URL, token))
			restorePlanGlobals(t)
			planAddStepTitle = "step"
			planAddStepPhase = ""
			operatedBy = op
			jsonOutput = false

			cmd := planAddStepCmd
			cmd.SetContext(context.Background())
			if err := runPlanAddStep(cmd, []string{"gd-PLAN"}); err == nil {
				t.Fatalf("runPlanAddStep with operated-by %q = nil, want rejection (non-human operator)", op)
			}
			// No mutating request may have reached the daemon: the gate fires first.
			d.mu.Lock()
			n := len(d.writes)
			d.mu.Unlock()
			if n != 0 {
				t.Fatalf("daemon saw %d writes for a rejected operator — validation must fire before the wire", n)
			}
		})
	}
}
