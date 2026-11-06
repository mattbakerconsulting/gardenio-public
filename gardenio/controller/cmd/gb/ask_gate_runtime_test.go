package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// askGateTestServer is a mock daemon serving the routes the ask-gate touches:
// GET /v1/beads (decisions), GET /v1/escalations, and GET/PATCH /v1/beads/{id}
// for the agent-bead telemetry stamp. decisionsBy / escalationsBy seed the
// created_by of a single handle so handle-exemption can be exercised.
func askGateTestServer(t *testing.T, decisionsBy, escalationsBy string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			beads := []map[string]any{}
			if decisionsBy != "" {
				beads = append(beads, map[string]any{
					"id": "gd-DEC001", "type": "decision", "created_by": decisionsBy, "status": "open",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/escalations":
			escs := []map[string]any{}
			if escalationsBy != "" {
				// Mirror kbeads' real shape: `escalation` carries ONLY the
				// extracted escalation-specific fields (no id/status/created_by)
				// and the full bead is under `issue`. Exercises the toDetail
				// Escalation<-Issue backfill (gd-tnvUfT11f3).
				escs = append(escs, map[string]any{
					"escalation": map[string]any{
						"operation_type": "secret_create",
					},
					"issue": map[string]any{
						"id": "gd-ESC001", "type": "escalation", "created_by": escalationsBy, "status": "open",
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"escalations": escs})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// Agent bead read for the stamp read-modify-write.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "type": "agent", "fields": map[string]any{},
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useTempMarker points the one-shot marker file at a temp path so tests do not
// collide with each other or with /tmp state.
func useTempMarker(t *testing.T) {
	t.Helper()
	old := askGateMarkerFile
	askGateMarkerFile = filepath.Join(t.TempDir(), "ask-gate-nudged")
	t.Cleanup(func() { askGateMarkerFile = old })
}

const askWrap = `{"accomplishments":"wired ESO","blockers":"A human must mint AWS Secrets Manager secret gardenio/foo."}`

func parseWrap(t *testing.T, s string) *WrapUp {
	t.Helper()
	var w WrapUp
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		t.Fatalf("bad wrap: %v", err)
	}
	return &w
}

// TestEvaluateAskGate_WarnNeverBlocks: in warn mode a detected, unhandled ask
// nudges + records telemetry but NEVER blocks.
func TestEvaluateAskGate_WarnNeverBlocks(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "warn")
	t.Setenv("BOAT_ROLE", "gardener") // non-despawn so we isolate the mode effect
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("warn mode must never block")
	}
	if !res.detected || res.kind != AskEscalation {
		t.Errorf("expected escalation detected, got detected=%v kind=%s", res.detected, res.kind)
	}
	if res.disposition != "warned" {
		t.Errorf("expected disposition=warned, got %s", res.disposition)
	}
	if !askGateAlreadyNudged() {
		t.Error("warn mode should still record the one-shot marker")
	}
}

// TestEvaluateAskGate_EnforceBlocksThenReleases: enforce mode blocks the first
// unhandled stop and self-releases the second (one-shot, anti-loop).
func TestEvaluateAskGate_EnforceBlocksThenReleases(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	first := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if !first.block || first.disposition != "blocked" {
		t.Fatalf("first enforce stop must block, got block=%v disp=%s", first.block, first.disposition)
	}
	second := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if second.block {
		t.Fatal("second identical stop must self-release (one-shot)")
	}
	if second.disposition != "released" {
		t.Errorf("expected disposition=released, got %s", second.disposition)
	}
	if askGateAlreadyNudged() {
		t.Error("marker should be cleared after self-release")
	}
}

// TestEvaluateAskGate_DespawnClassNeverBlocks: a never-respawn role is never
// hard-blocked even in enforce mode (would be a permanent trap).
func TestEvaluateAskGate_DespawnClassNeverBlocks(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "thread")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("despawn-class role must never be hard-blocked")
	}
	if res.disposition != "warned" {
		t.Errorf("expected disposition=warned for despawn-class, got %s", res.disposition)
	}
}

// TestEvaluateAskGate_HandleExemptsDecision: an agent that filed a decision
// passes regardless of mode.
func TestEvaluateAskGate_HandleExemptsDecision(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "alice", "") // alice authored a decision
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("agent with a filed decision must pass")
	}
	if res.filed != "decision" || res.disposition != "filed" {
		t.Errorf("expected filed=decision disposition=filed, got filed=%s disp=%s", res.filed, res.disposition)
	}
}

// TestEvaluateAskGate_HandleExemptsEscalation: an agent that filed an
// escalation passes (decision-OR-escalation acceptance).
func TestEvaluateAskGate_HandleExemptsEscalation(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "alice") // alice authored an escalation
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("agent with a filed escalation must pass")
	}
	if res.filed != "escalation" {
		t.Errorf("expected filed=escalation, got %s", res.filed)
	}
}

// TestEvaluateAskGate_HandleExemptsReferencedOpen: an agent whose wrap-up NAMES
// an already-open escalation (filed by someone else) passes via the
// referenced_open path — the "defer to an existing handle" healthy bucket. This
// exercises the Escalation<-Issue backfill in openHandleExists: without it, the
// escalation's real id lives under `issue` and refSet[e.Escalation.ID] never
// matches, so a legitimate defer would trip the gate and coach a duplicate.
func TestEvaluateAskGate_HandleExemptsReferencedOpen(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	// bob authored the open escalation gd-ESC001; alice merely references it.
	srv := askGateTestServer(t, "", "bob")
	swapDaemon(t, srv.URL)

	wrap := `{"accomplishments":"wired ESO","handoff_notes":"Blocked on open escalation gd-ESC001 — operator must mint the AWS Secrets Manager secret."}`
	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, wrap), false)
	if res.block {
		t.Fatal("agent referencing an open escalation must pass")
	}
	if res.filed != "referenced_open" {
		t.Errorf("expected filed=referenced_open, got %s", res.filed)
	}
}

// TestEvaluateAskGate_ForceReleases: --force always releases but records
// telemetry (disposition=forced) so forced-after-flag is measurable.
func TestEvaluateAskGate_ForceReleases(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), true)
	if res.block {
		t.Fatal("--force must release")
	}
	if res.disposition != "forced" {
		t.Errorf("expected disposition=forced, got %s", res.disposition)
	}
}

// TestEvaluateAskGate_OffDisables: GB_STOP_ASK_GATE=off short-circuits to clean.
func TestEvaluateAskGate_OffDisables(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "off")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block || res.detected || res.disposition != "clean" {
		t.Errorf("off mode must be a clean no-op, got %+v", res)
	}
}

// TestEvaluateAskGate_NoAskClean: a clean done wrap-up with no ask is a clean
// pass and clears any stale marker.
func TestEvaluateAskGate_NoAskClean(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	_ = os.WriteFile(askGateMarkerFile, []byte("1\n"), 0o644) // stale marker
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT",
		parseWrap(t, `{"accomplishments":"merged MR 1; closed 3 beads"}`), false)
	if res.block || res.detected {
		t.Fatal("clean done wrap-up must pass")
	}
	if askGateAlreadyNudged() {
		t.Error("clean stop should clear a stale one-shot marker")
	}
}

// TestEvaluateAskGate_DaemonUnreachableFailsOpen: if the daemon is nil the gate
// fails open (no block) so a flaky daemon never wedges a stop.
func TestEvaluateAskGate_DaemonUnreachableFailsOpen(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("nil daemon must fail open (no block)")
	}
	if res.filed != "unverifiable" {
		t.Errorf("expected filed=unverifiable on nil daemon, got %s", res.filed)
	}
}

// TestStampAskGateBead_UnverifiableDistinct pins the telemetry-fidelity fix:
// the fail-open "unverifiable" handle must be stamped as its own distinct
// stranded_ask_filed value (not "true"), so post-ship analysis can separate
// genuinely-handled asks from un-checkable ones.
func TestStampAskGateBead_UnverifiableDistinct(t *testing.T) {
	var gotFiled string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-AGENT", "type": "agent", "fields": map[string]any{}})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if v, ok := body.Fields["stranded_ask_filed"].(string); ok {
				gotFiled = v
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-AGENT"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)

	cases := map[string]string{
		"decision":        "true",
		"escalation":      "true",
		"referenced_open": "true",
		"unverifiable":    "unverifiable",
		"none":            "false",
	}
	for filed, want := range cases {
		gotFiled = ""
		stampAskGateBead(context.Background(), "gd-AGENT",
			askGateResult{detected: true, kind: AskEscalation, filed: filed, disposition: "x"})
		if gotFiled != want {
			t.Errorf("filed=%q: stamped stranded_ask_filed=%q, want %q", filed, gotFiled, want)
		}
	}
}

// TestIsDespawnClassRole covers the role table, including COMMA-SEPARATED
// multi-role BOAT_ROLE values (the controller preserves "crew,thread" verbatim).
// A despawn-class role anywhere in the list must make the whole agent
// despawn-class so it is never hard-blocked (adversarial review HIGH finding).
func TestIsDespawnClassRole(t *testing.T) {
	despawn := []string{
		"thread", "crew", "auto-inspector", "fsm-triage", "jira-mention",
		// multi-role values: any despawn-class component wins.
		"crew,thread", "thread,crew", "jira-mention,crew", "gardener,thread",
		" crew , gardener ", "monorepo-dark,fsm-triage",
	}
	for _, r := range despawn {
		t.Setenv("BOAT_ROLE", r)
		if !isDespawnClassRole() {
			t.Errorf("%q should be despawn-class", r)
		}
	}
	notDespawn := []string{"gardener", "first-mate", "", "monorepo-dark", "gardener,first-mate"}
	for _, r := range notDespawn {
		t.Setenv("BOAT_ROLE", r)
		if isDespawnClassRole() {
			t.Errorf("%q should NOT be despawn-class", r)
		}
	}
}

// TestEvaluateAskGate_MultiRoleDespawnNeverBlocks pins the HIGH finding fix: a
// multi-role agent that INCLUDES a despawn-class role must never be hard-blocked
// in enforce mode.
func TestEvaluateAskGate_MultiRoleDespawnNeverBlocks(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "crew,thread")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("multi-role agent including a despawn-class role must never hard-block")
	}
}

// TestEvaluateAskGate_MarkerWriteFailureFailsOpen pins the CRITICAL finding fix:
// if the one-shot marker cannot be persisted, enforce mode must FAIL OPEN (warn,
// not block) so a write failure cannot cause an unbounded block loop.
func TestEvaluateAskGate_MarkerWriteFailureFailsOpen(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	// Point the marker at an unwritable path (a dir that does not exist).
	old := askGateMarkerFile
	askGateMarkerFile = "/nonexistent-dir-xyz/ask-gate-nudged"
	t.Cleanup(func() { askGateMarkerFile = old })
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	for i := range 3 {
		res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, askWrap), false)
		if res.block {
			t.Fatalf("attempt %d: marker write failure must fail OPEN (no block) to avoid a loop", i)
		}
		if res.disposition != "warned" {
			t.Errorf("attempt %d: expected disposition=warned on marker failure, got %s", i, res.disposition)
		}
	}
}

// TestEvaluateAskGate_UnknownActorFailsOpen pins the HIGH finding fix: the
// literal "unknown" actor (defaultActor when git user.name is unset) cannot be
// joined to a real decision/escalation, so it must fail open, not block.
func TestEvaluateAskGate_UnknownActorFailsOpen(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce")
	t.Setenv("BOAT_ROLE", "gardener")
	useTempMarker(t)
	srv := askGateTestServer(t, "", "")
	swapDaemon(t, srv.URL)

	res := evaluateAskGate(context.Background(), "unknown", "gd-AGENT", parseWrap(t, askWrap), false)
	if res.block {
		t.Fatal("actor=unknown must fail open (no block)")
	}
	if res.filed != "unverifiable" {
		t.Errorf("expected filed=unverifiable for actor=unknown, got %s", res.filed)
	}
}

// gardenerHandoffTestServer serves the routes the gardener-handoff path touches:
// empty decision/escalation lists (so NO operator handle exists), the referenced
// handoff bead at GET /v1/beads/gd-MAIL01, the squawk create POST /v1/beads and
// its close, plus the generic agent-bead read/patch for telemetry. It records
// created message beads and closed ids so a test can assert the squawk fired.
func gardenerHandoffTestServer(t *testing.T, created *[]map[string]any, closed *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/escalations":
			_ = json.NewEncoder(w).Encode(map[string]any{"escalations": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*created = append(*created, body)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-SQUAWK1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			mu.Lock()
			*closed = append(*closed, id)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-MAIL01":
			// The referenced gardener-handoff mail, authored by the crew (alice).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-MAIL01", "type": "mail", "status": "open",
				"created_by": "alice", "assignee": "gardener-bot", "fields": map[string]any{},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "type": "agent", "fields": map[string]any{},
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEvaluateAskGate_GardenerHandoffAcceptedAndSquawks pins the taxonomy-B
// follow-up (ka-0000000039): a crew that punts an operator-directed ask up to its
// gardener AND references a real handoff artifact (a gb mail it sent) is ACCEPTED
// (filed=gardener_handoff, never blocks) — and the gate emits a squawk so the
// gardener inherits a VISIBLE handle.
func TestEvaluateAskGate_GardenerHandoffAcceptedAndSquawks(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "enforce") // even in enforce mode this must pass
	t.Setenv("BOAT_ROLE", "crew")
	useTempMarker(t)

	var (
		mu      sync.Mutex
		created []map[string]any
		closed  []string
	)
	srv := gardenerHandoffTestServer(t, &created, &closed, &mu)
	swapDaemon(t, srv.URL)

	wrap := `{"accomplishments":"did the work","handoff_notes":"Punting to the gardener to decide rollout aggressiveness; mailed the gardener, see gd-MAIL01. Operator must approve the plan."}`
	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, wrap), false)

	if res.block {
		t.Fatal("gardener-handoff must never block")
	}
	if res.filed != "gardener_handoff" || res.disposition != "handed_off" {
		t.Fatalf("expected filed=gardener_handoff disposition=handed_off, got filed=%s disp=%s", res.filed, res.disposition)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("expected exactly one squawk message bead, got %d", len(created))
	}
	if created[0]["type"] != "message" {
		t.Errorf("squawk bead type = %v, want message", created[0]["type"])
	}
	if title, _ := created[0]["title"].(string); !strings.Contains(title, "gd-MAIL01") {
		t.Errorf("squawk should reference the handle gd-MAIL01, got: %q", title)
	}
	if len(closed) == 0 {
		t.Error("squawk bead should be closed after creation (fire-and-forget)")
	}
}

// TestEvaluateAskGate_GardenerPuntWithoutHandleNotAccepted: a punt-to-gardener in
// prose with NO referenced handoff artifact is NOT accepted as a handle — it
// falls through to the normal warn/nudge path and emits no squawk. The prose
// alone is exactly the stranded shape the follow-up must not silently absorb.
func TestEvaluateAskGate_GardenerPuntWithoutHandleNotAccepted(t *testing.T) {
	t.Setenv("GB_STOP_ASK_GATE", "warn")
	t.Setenv("BOAT_ROLE", "crew")
	useTempMarker(t)

	var (
		mu      sync.Mutex
		created []map[string]any
		closed  []string
	)
	srv := gardenerHandoffTestServer(t, &created, &closed, &mu)
	swapDaemon(t, srv.URL)

	// Punts to the gardener but names NO gd- handle at all.
	wrap := `{"accomplishments":"did the work","handoff_notes":"Handing off to the gardener to decide rollout. Operator must approve the plan."}`
	res := evaluateAskGate(context.Background(), "alice", "gd-AGENT", parseWrap(t, wrap), false)

	if res.filed == "gardener_handoff" {
		t.Fatal("a bare-prose punt with no referenced handle must NOT be accepted as gardener_handoff")
	}
	if res.disposition != "warned" {
		t.Errorf("expected disposition=warned, got %s", res.disposition)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 0 {
		t.Errorf("no squawk should be emitted without an accepted handle, got %d created", len(created))
	}
}

// TestGardenerHandoffRef_MatchesMailAndHandoffBead unit-tests the corroboration
// helper directly against the referenced-bead shapes it must accept and reject.
func TestGardenerHandoffRef_RejectsWhenNoPunt(t *testing.T) {
	var (
		mu      sync.Mutex
		created []map[string]any
		closed  []string
	)
	srv := gardenerHandoffTestServer(t, &created, &closed, &mu)
	swapDaemon(t, srv.URL)

	// References gd-MAIL01 but the prose does NOT punt to a gardener — the helper
	// must not fire (the prose is what asserts "addressed to the gardener").
	w := &WrapUp{HandoffNotes: "Blocked on gd-MAIL01; operator must approve."}
	if ref := gardenerHandoffRef(context.Background(), "alice", w); ref != "" {
		t.Errorf("gardenerHandoffRef must be empty without a punt-to-gardener, got %q", ref)
	}
}
