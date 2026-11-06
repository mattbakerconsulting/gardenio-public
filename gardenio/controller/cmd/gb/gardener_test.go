package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerStub is a minimal kbeads HTTP stub for the gb gardener tests. It
// stores a single primary bead (the parent epic) plus a map of additional
// beads keyed by id, and implements the subset of endpoints gardener.go
// touches: GET /v1/beads/<id>, GET /v1/beads (list), POST /v1/beads,
// PATCH /v1/beads/<id>, POST /v1/beads/<id>/close, POST/GET
// /v1/beads/<id>/comments.
type gardenerStub struct {
	mu       sync.Mutex
	beads    map[string]map[string]any
	creates  []map[string]any
	patches  map[string][]map[string]any
	closes   map[string][]map[string]any
	comments map[string][]map[string]any
	// ops records the ordered write operations the mode-flip verb drives
	// (fields/addlabel/rmlabel/comment) so tests can assert the double-fire
	// guard ordering end-to-end through the HTTP client.
	ops []gardenerStubOp
	// subtree maps a parent (epic) id → the set of descendant bead ids it
	// contains, modeling the reverse parent-child subtree the `parent=` query
	// resolves via the daemon's recursive CTE. Populated by addChild.
	subtree map[string]map[string]bool
	nextID  int
	// escalateAgentCreates makes an agent create answer 202 with an escalation,
	// as the daemon does for an X-Escalate create of a privileged role.
	escalateAgentCreates bool
	// sealOnReseal models a gardenio-admin write: a PATCH carrying the
	// reseal_spawn_lineage control key consumes it and stamps spawn_lineage.
	sealOnReseal bool
	// reportSpawnDigest makes a trigger/schedule GET carry the daemon's
	// spawn_lineage_content/_digest and makes a reseal PATCH that pins a digest
	// 409 when the stored content no longer hashes to it.
	reportSpawnDigest bool
	// beforePatch, when set, runs (once) just before a PATCH is applied,
	// modelling a concurrent writer that lands between the CLI's read and write.
	beforePatch func()
	// resealDigests records reseal_expected_digest on each reseal PATCH.
	resealDigests []string
	// escalateHeaders records X-Escalate on each POST /v1/beads.
	escalateHeaders []string
	// sealTriggerCreates stamps fields.spawn_lineage on each created trigger,
	// as the daemon does for a plan gardener bound after the owner approved.
	sealTriggerCreates bool
	// whoamiSource is what GET /v1/whoami reports ("auth" for an OIDC session,
	// "token" for an agent's); empty answers 404.
	whoamiSource string
	// restampSealOnPatch models a gardenio-admin OIDC write to a sealed trigger:
	// after a PATCH the daemon re-stamps spawn_lineage over the new content.
	restampSealOnPatch bool
}

// stubSeal is a spawn_lineage seal over b's current content, in the daemon's
// shape, so spawnSealValid accepts it while the content is unchanged.
func (s *gardenerStub) stubSeal(b map[string]any) string {
	raw, _ := json.Marshal(s.spawnContent(b))
	sum := sha256.Sum256(raw)
	seal, _ := json.Marshal(map[string]any{"v": 1, "author": "oidc:owner@example.com", "digest": hex.EncodeToString(sum[:]), "iat": 1})
	return string(seal)
}

// spawnContent stands in for the daemon's spawnLineageContent: the bead's
// fields minus the server-owned seal and the observability stamp.
func (s *gardenerStub) spawnContent(b map[string]any) map[string]any {
	out := map[string]any{}
	fields, _ := b["fields"].(map[string]any)
	for k, v := range fields {
		if k == "spawn_lineage" || k == "last_checked" {
			continue
		}
		out[k] = v
	}
	return out
}

// gardenerStubOp is one recorded write against the stub, in arrival order.
type gardenerStubOp struct {
	op    string // "fields" | "addlabel" | "rmlabel" | "comment"
	id    string
	label string // for addlabel/rmlabel
}

func newGardenerStub() *gardenerStub {
	return &gardenerStub{
		beads:    map[string]map[string]any{},
		patches:  map[string][]map[string]any{},
		closes:   map[string][]map[string]any{},
		comments: map[string][]map[string]any{},
		subtree:  map[string]map[string]bool{},
		nextID:   1,
	}
}

// opsOf returns the recorded op names (fields/addlabel/rmlabel/comment) in
// arrival order for the given bead id.
func (s *gardenerStub) opsOf(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, o := range s.ops {
		if o.id == id {
			out = append(out, o.op)
		}
	}
	return out
}

func (s *gardenerStub) addBead(b map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := b["id"].(string)
	s.beads[id] = b
}

// addChild registers b as a descendant of parentID in the reverse subtree and
// stores it as a bead, so a `GET /v1/beads?parent=<parentID>` query returns it.
func (s *gardenerStub) addChild(parentID string, b map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := b["id"].(string)
	s.beads[id] = b
	if s.subtree[parentID] == nil {
		s.subtree[parentID] = map[string]bool{}
	}
	s.subtree[parentID][id] = true
}

func (s *gardenerStub) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/v1/beads":
			s.mu.Lock()
			defer s.mu.Unlock()
			beads := make([]map[string]any, 0, len(s.beads))
			typeFilter := r.URL.Query().Get("type")
			statusFilter := r.URL.Query().Get("status")
			// parent=<id> restricts to the reverse parent-child subtree (the
			// recursive-CTE query `kd list --parent` / the gardener stop guard
			// uses). The stub resolves it from the addChild-populated map.
			parentFilter := r.URL.Query().Get("parent")
			// field=key=value applies custom-field equality (fields->>'key' =
			// 'value'), the daemon filter `gb gardener status` uses to scope a
			// gardener's spawned iterations by trigger_id/schedule_id. Each
			// entry splits on the first '=' (mirrors kbeads http_beads.go).
			fieldFilters := r.URL.Query()["field"]
			var subtree map[string]bool
			if parentFilter != "" {
				subtree = s.subtree[parentFilter]
			}
			for _, b := range s.beads {
				if parentFilter != "" && !subtree[asString(b["id"])] {
					continue
				}
				if typeFilter != "" && !containsCSV(typeFilter, asString(b["type"])) {
					continue
				}
				if statusFilter != "" && !containsCSV(statusFilter, asString(b["status"])) {
					continue
				}
				if !matchesFieldFilters(b, fieldFilters) {
					continue
				}
				beads = append(beads, b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": beads,
				"total": len(beads),
			})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/") && strings.HasSuffix(path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/comments")
			s.mu.Lock()
			cs := s.comments[id]
			s.mu.Unlock()
			out := []map[string]any{}
			for i, c := range cs {
				out = append(out, map[string]any{
					"id":     int64(i + 1),
					"author": c["author"],
					"text":   c["text"],
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": out})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			s.mu.Lock()
			b, ok := s.beads[id]
			s.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			if s.reportSpawnDigest && (b["type"] == "trigger" || b["type"] == "schedule") {
				// Model the daemon's GET: the covered content and its digest.
				cp := map[string]any{}
				for k, v := range b {
					cp[k] = v
				}
				content := s.spawnContent(b)
				raw, _ := json.Marshal(content)
				sum := sha256.Sum256(raw)
				cp["spawn_lineage_content"] = content
				cp["spawn_lineage_digest"] = hex.EncodeToString(sum[:])
				b = cp
			}
			_ = json.NewEncoder(w).Encode(b)

		case r.Method == http.MethodGet && path == "/v1/whoami":
			if s.whoamiSource == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"operator_id": "someone", "source": s.whoamiSource})

		case r.Method == http.MethodPost && path == "/v1/beads":
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.escalateHeaders = append(s.escalateHeaders, r.Header.Get("X-Escalate"))
			if s.escalateAgentCreates && parsed["type"] == "agent" {
				s.mu.Unlock()
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"escalation_id": "gd-esc1", "message": "escalation created: pending operator approval"})
				return
			}
			s.creates = append(s.creates, parsed)
			id := fmt.Sprintf("gd-new%d", s.nextID)
			s.nextID++
			fields := map[string]any{}
			if rawFields, ok := parsed["fields"].(map[string]any); ok {
				for k, v := range rawFields {
					fields[k] = v
				}
			}
			if s.sealTriggerCreates && parsed["type"] == "trigger" {
				fields["spawn_lineage"] = `{"v":1,"author":"oidc:op@example.com"}`
			}
			b := map[string]any{
				"id":     id,
				"title":  parsed["title"],
				"type":   parsed["type"],
				"kind":   parsed["kind"],
				"status": "open",
				"fields": fields,
				"labels": parsed["labels"],
			}
			s.beads[id] = b
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			if s.beforePatch != nil {
				s.beforePatch()
				s.beforePatch = nil
			}
			s.patches[id] = append(s.patches[id], parsed)
			s.ops = append(s.ops, gardenerStubOp{op: "fields", id: id})
			if fieldsRaw, ok := parsed["fields"].(map[string]any); ok {
				if _, reseal := fieldsRaw["reseal_spawn_lineage"]; reseal {
					delete(fieldsRaw, "reseal_spawn_lineage")
					expected, _ := fieldsRaw["reseal_expected_digest"].(string)
					delete(fieldsRaw, "reseal_expected_digest")
					s.resealDigests = append(s.resealDigests, expected)
					if expected != "" && s.reportSpawnDigest {
						raw, _ := json.Marshal(s.spawnContent(s.beads[id]))
						sum := sha256.Sum256(raw)
						if hex.EncodeToString(sum[:]) != expected {
							s.mu.Unlock()
							w.WriteHeader(http.StatusConflict)
							_ = json.NewEncoder(w).Encode(map[string]string{"error": "spawn-lineage reseal refused: the trigger changed since it was read"})
							return
						}
					}
					if s.sealOnReseal {
						fieldsRaw["spawn_lineage"] = fmt.Sprintf("sealed-%d", len(s.patches[id]))
					}
				}
				if existing, ok := s.beads[id]["fields"].(map[string]any); ok {
					for k, v := range fieldsRaw {
						existing[k] = v
					}
				} else {
					s.beads[id]["fields"] = fieldsRaw
				}
				if s.restampSealOnPatch {
					if existing, ok := s.beads[id]["fields"].(map[string]any); ok && existing["spawn_lineage"] != nil {
						existing["spawn_lineage"] = s.stubSeal(s.beads[id])
					}
				}
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/labels")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.ops = append(s.ops, gardenerStubOp{op: "addlabel", id: id, label: parsed["label"]})
			s.beads[id]["labels"] = appendLabel(s.beads[id]["labels"], parsed["label"])
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodDelete && strings.Contains(path, "/labels/"):
			rest := strings.TrimPrefix(path, "/v1/beads/")
			id, label, _ := strings.Cut(rest, "/labels/")
			if dec, err := url.PathUnescape(label); err == nil {
				label = dec
			}
			s.mu.Lock()
			s.ops = append(s.ops, gardenerStubOp{op: "rmlabel", id: id, label: label})
			s.beads[id]["labels"] = removeLabel(s.beads[id]["labels"], label)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/close")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.closes[id] = append(s.closes[id], parsed)
			s.beads[id]["status"] = "closed"
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/comments")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.comments[id] = append(s.comments[id], parsed)
			s.ops = append(s.ops, gardenerStubOp{op: "comment", id: id})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			http.NotFound(w, r)
		}
	})
}

func containsCSV(csv, v string) bool {
	for _, p := range strings.Split(csv, ",") {
		if strings.TrimSpace(p) == v {
			return true
		}
	}
	return false
}

// matchesFieldFilters returns true when the bead's fields satisfy every
// field=key=value equality filter (ANDed, mirroring the daemon). An empty
// filter list matches everything.
func matchesFieldFilters(b map[string]any, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	fields, _ := b["fields"].(map[string]any)
	for _, f := range filters {
		key, want, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		if asString(fields[key]) != want {
			return false
		}
	}
	return true
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// labelSlice normalizes a bead's stored labels ([]any from JSON round-trips,
// or []string from in-test fixtures) to a []string.
func labelSlice(v any) []string {
	switch ls := v.(type) {
	case []string:
		return append([]string{}, ls...)
	case []any:
		out := make([]string, 0, len(ls))
		for _, l := range ls {
			if s, ok := l.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func appendLabel(v any, label string) []string {
	ls := labelSlice(v)
	for _, l := range ls {
		if l == label {
			return ls
		}
	}
	return append(ls, label)
}

func removeLabel(v any, label string) []string {
	ls := labelSlice(v)
	out := ls[:0]
	for _, l := range ls {
		if l != label {
			out = append(out, l)
		}
	}
	return out
}

func setupGardenerStub(t *testing.T, stub *gardenerStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetActor("test-actor")
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	oldActor := actor
	actor = "test-actor"
	t.Cleanup(func() { actor = oldActor })
}

func makeEpic(id, title string, labels ...string) map[string]any {
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   "epic",
		"kind":   "issue",
		"status": "open",
		"fields": map[string]any{},
		"labels": labels,
	}
}

func makeGardenerSchedule(id, assignBead string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["role"] = "gardener"
	fields["assign_bead"] = assignBead
	if _, ok := fields["enabled"]; !ok {
		fields["enabled"] = "true"
	}
	return map[string]any{
		"id":     id,
		"title":  "Gardener for " + assignBead,
		"type":   "schedule",
		"kind":   "data",
		"status": "open",
		"fields": fields,
	}
}

// makeGardenerTrigger builds the current (Phase 3b/D10) gardener shape: a
// type=trigger / trigger:schedule bead with role=gardener, pinned to its epic
// via the gardener-epic:<id> label (no assign_bead field).
func makeGardenerTrigger(id, epicID string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["role"] = "gardener"
	fields["action"] = "spawn_agent"
	if _, ok := fields["enabled"]; !ok {
		fields["enabled"] = "true"
	}
	return map[string]any{
		"id":     id,
		"title":  "Gardener for " + epicID,
		"type":   "trigger",
		"kind":   "data",
		"status": "open",
		"fields": fields,
		"labels": []any{"trigger:schedule", "role:gardener", "gardener-epic:" + epicID},
	}
}

// resetGardenerFlags restores command flags to their defaults so tests do
// not bleed state across cases.
func resetGardenerFlags(t *testing.T) {
	t.Helper()
	flags := []struct{ cmd, name, val string }{
		{"start", "cron", ""},
		{"start", "tz", ""},
		{"start", "project", ""},
		{"start", "title", ""},
		{"start", "terminal-goal", ""},
		{"start", "prompt", ""},
		{"start", "max-runs", "20"},
		{"start", "loop-wall-clock", "168h"},
		{"start", "run-wall-clock", "30m"},
		{"start", "max-concurrent", "3"},
		{"start", "cooldown", "5m"},
		{"start", "wake-on-mr", ""},
		{"start", "stop-on-mr-merged", ""},
		// The read-the-instructions gate defaults to false; every existing
		// success-path test drives the CREATE path, so ack the guide here. The
		// dedicated gate tests below clear it explicitly to exercise the refusal.
		{"start", "i-read-the-gardener-instructions", "true"},
	}
	for _, f := range flags {
		_ = gardenerStartCmd.Flags().Set(f.name, f.val)
	}
	_ = gardenerStopCmd.Flags().Set("reason", "")
	_ = gardenerStopCmd.Flags().Set("ignore-open-children", "false")
	_ = gardenerForceCloseCmd.Flags().Set("reason", "")
	_ = gardenerModeCmd.Flags().Set("event", "false")
	_ = gardenerModeCmd.Flags().Set("timed", "false")
	_ = gardenerModeCmd.Flags().Set("fallback-cron", "")
	_ = gardenerModeCmd.Flags().Set("no-fallback-cron", "false")
	_ = gardenerCadenceCmd.Flags().Set("cron", "")
	_ = gardenerCadenceCmd.Flags().Set("none", "false")
}

// ── renderGardenerPrompt + exit guidance ──────────────────────────────────

func TestRenderGardenerPrompt_NamesEpic(t *testing.T) {
	prompt, err := renderGardenerPrompt("gd-EPIC1")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(prompt, "gd-EPIC1") {
		t.Errorf("prompt did not name the epic: %q", prompt)
	}
}

func TestAppendGardenerExitGuidance_FoldsLoopExitContract(t *testing.T) {
	out := appendGardenerExitGuidance("base prompt", gardenerExitGuidance{
		EpicID:        "gd-EPIC1",
		Terminal:      "All children of gd-EPIC1 are closed.",
		LoopWallClock: "168h",
		RunWallClock:  "30m",
	})
	if !strings.HasPrefix(out, "base prompt") {
		t.Errorf("guidance should append to the original prompt, got: %q", out)
	}
	for _, want := range []string{
		"## Loop exit",
		"All children of gd-EPIC1 are closed.",
		// P8.2 (gd-WHnRx2Gj3A): the gardener is told the engine re-arms its
		// budget, so it never files an "extend budget?" card.
		"The engine re-arms your budget; never ask to extend it.",
		"If your runs stop making progress it parks you until the plan changes.",
		"loop 168h",
		"single run 30m",
		"gb stop --wrapup",
		"gd-EPIC1",
		// Self-disable invariant (gd-RpQSkQKo3O): the gardener must not sever its
		// own wake path; dormancy is the cron pre-flight's job. Reframed from a
		// shout-directive to positive intent + WHY (task ka-0000004887, the
		// gardener-prompt over-constraint pass), so the invariant is asserted in
		// its judgment-framed form — the leave-it-enabled instruction, the
		// concrete anti-pattern it rules out, the consequence (severed wake
		// path), and the one way to idle it (awaiting a named bead whose close
		// re-enables it, gd-V68T7v5JPP) all still present.
		"Leave your own trigger enabled",
		"enabled=false",
		"severs the auto-wake path",
		"awaiting=<bead-id>",
		"turns the trigger back on the moment that bead closes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exit guidance missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Run budget:") || strings.Contains(out, "wrap up the loop") {
		t.Errorf("exit guidance still names a run count to wrap up at\n---\n%s", out)
	}
}

// ── gb gardener start ─────────────────────────────────────────────────────

// TestGardenerStart_CreatesFiringTriggerShape locks in the Phase 3b/D10 firing
// model (bug ka-0000005960): `gb gardener start` must emit a type=trigger /
// trigger:schedule / action=spawn_agent bead — the only shape the
// trigger-engine actually loads and fires — NOT a bare type=schedule bead
// (which loads into no source and silently never runs).
func TestGardenerStart_CreatesFiringTriggerShape(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	// No operator: the E2E-fixture / static-gardener path. The gardener owner
	// blob must NOT be stamped (asserted via the forbidden-field check below).
	oldOperated := operatedBy
	operatedBy = ""
	t.Cleanup(func() { operatedBy = oldOperated })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	c := stub.creates[0]
	// REGRESSION GUARD (ka-0000005960): the bead MUST be type=trigger, not
	// type=schedule — the trigger-engine only fires type=trigger beads.
	if got := c["type"]; got != "trigger" {
		t.Errorf("created bead type = %v, want trigger (type=schedule never fires post-D10)", got)
	}
	if got, _ := c["title"].(string); !strings.Contains(got, "gd-EPIC1") {
		t.Errorf("title = %q, expected to mention parent bead id", got)
	}
	fields, _ := c["fields"].(map[string]any)
	if got := fields["role"]; got != "gardener" {
		t.Errorf("fields.role = %v, want gardener", got)
	}
	// action=spawn_agent is what makes the schedule source actually spawn a
	// gardener agent when the cron fires.
	if got := fields["action"]; got != "spawn_agent" {
		t.Errorf("fields.action = %v, want spawn_agent", got)
	}
	// enabled must be a JSON boolean (regression guard for gd-MKlVDuFwQO).
	if got, ok := fields["enabled"].(bool); !ok || !got {
		t.Errorf("fields.enabled = %#v, want JSON bool true", fields["enabled"])
	}
	// cron + timezone drive the schedule source. The default cron is now a
	// deterministic staggered expression seeded from the epic id (kills the
	// literal "7,37" spawn herd — spec 2026-07-13-gardener-mode-flip-design.md).
	if want := gardenermode.StaggeredCron("gd-EPIC1", 30*time.Minute); fields["cron"] != want {
		t.Errorf("fields.cron = %v, want staggered default %v", fields["cron"], want)
	}
	if got := fields["timezone"]; got != "America/Los_Angeles" {
		t.Errorf("fields.timezone = %v, want default America/Los_Angeles", got)
	}
	// cooldown_minutes + max_concurrent must be JSON numbers, not strings or
	// empty (regression guard for ka-0000004844: HTTP 400 "must be an
	// integer"). Defaults mirror `gb trigger create`: max-concurrent 3,
	// cooldown 5m → 5 minutes. (The stub round-trips the request body through
	// JSON, so integers decode as float64.)
	if got, ok := fields["cooldown_minutes"].(float64); !ok || got != 5 {
		t.Errorf("fields.cooldown_minutes = %#v, want JSON number 5", fields["cooldown_minutes"])
	}
	if got, ok := fields["max_concurrent"].(float64); !ok || got != 3 {
		t.Errorf("fields.max_concurrent = %#v, want JSON number 3", fields["max_concurrent"])
	}
	// The structured gardener loop spec lives INSIDE the nested `gardener` JSON
	// blob (the only place the trigger-engine decodes it), never as flat
	// top-level fields. The flat keys must stay absent — the engine ignores
	// them and the type:trigger schema would only carry inert copies.
	for _, forbidden := range []string{"assign_bead", "stop", "progress", "triggers", "max_runs", "terminal_goal"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("fields.%s present as a flat field — the loop spec belongs inside the gardener blob", forbidden)
		}
	}

	// The gardener blob IS present and carries the REAL structured loop spec
	// (the default self-block mandated by advice ka-0000000974). Even without an
	// operator to attribute, the loop spec must be stamped so the scheduler
	// enforces stop conditions — see assertGardenerSelfBlock for the full check.
	blob := gardenerBlobFromCreate(t, c)
	if blob == nil {
		t.Fatal("gardener blob absent — the default structured loop spec must always be stamped")
	}
	assertGardenerSelfBlock(t, blob, "gd-EPIC1")
	// Ownerless gardener: no operator → no owner fields inside the blob.
	if _, ok := blob["slack_user_id"]; ok {
		t.Errorf("gardener.slack_user_id present %v, want absent (no operator)", blob["slack_user_id"])
	}
	if _, ok := blob["user_bead_id"]; ok {
		t.Errorf("gardener.user_bead_id present %v, want absent (no operator)", blob["user_bead_id"])
	}
	// project inherited from parent label.
	if got := fields["project"]; got != "gardenio" {
		t.Errorf("fields.project = %v, want gardenio (inherited)", got)
	}

	// conditions is a valid JSON array pre-flight scoping the cron fire to the
	// epic's open children (the loop-end analogue).
	condRaw, _ := fields["conditions"].(string)
	if strings.TrimSpace(condRaw) == "" {
		t.Fatal("fields.conditions is empty — gardener would fire forever")
	}
	var conds []map[string]any
	if err := json.Unmarshal([]byte(condRaw), &conds); err != nil {
		t.Fatalf("fields.conditions is not a valid JSON array: %v\n%s", err, condRaw)
	}
	if len(conds) != 1 {
		t.Fatalf("conditions len = %d, want 1", len(conds))
	}
	if got, _ := conds[0]["parent"].(string); got != "gd-EPIC1" {
		t.Errorf("conditions[0].parent = %q, want gd-EPIC1", got)
	}

	// labels include trigger:schedule (source selector), role:gardener,
	// gardener-epic:<id> (epic pin), and project:gardenio.
	labels, _ := c["labels"].([]any)
	want := map[string]bool{
		"trigger:schedule":       false,
		"role:gardener":          false,
		"gardener-epic:gd-EPIC1": false,
		"project:gardenio":       false,
	}
	for _, l := range labels {
		if s, _ := l.(string); s != "" {
			if _, ok := want[s]; ok {
				want[s] = true
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("labels missing %q", k)
		}
	}

	// The gardener prompt mentions the parent bead and carries the loop-exit
	// guidance (the bare shape can't store structured stop fields).
	prompt, _ := fields["prompt"].(string)
	if !strings.Contains(prompt, "gd-EPIC1") {
		t.Errorf("gardener prompt did not interpolate parent bead id: %q", prompt)
	}
	if !strings.Contains(prompt, "## Loop exit") {
		t.Errorf("gardener prompt missing folded loop-exit guidance: %q", prompt)
	}
}

// TestGardenerStart_WritesNoLegacyCaptainTwins: `gb gardener start` (and its
// `gb captain start` alias) stamps only the role:gardener / gardener-epic:<id> /
// `gardener` blob markers; the pre-rename role:captain / captain-epic:<id> /
// `captain` twins are retired (gd-5O9QUlA8zr phase 5).
func TestGardenerStart_WritesNoLegacyCaptainTwins(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	c := stub.creates[0]

	labels := map[string]bool{}
	for _, l := range labelSlice(c["labels"]) {
		labels[l] = true
	}
	for _, want := range []string{"role:gardener", "gardener-epic:gd-EPIC1"} {
		if !labels[want] {
			t.Errorf("labels %v missing %q", labelSlice(c["labels"]), want)
		}
	}
	for l := range labels {
		if l == "role:captain" || strings.HasPrefix(l, "captain-epic:") {
			t.Errorf("labels %v carry retired legacy label %q", labelSlice(c["labels"]), l)
		}
	}

	fields, _ := c["fields"].(map[string]any)
	gardener, _ := fields["gardener"].(string)
	if strings.TrimSpace(gardener) == "" {
		t.Fatal("fields.gardener blob absent")
	}
	if _, ok := fields["captain"]; ok {
		t.Error("fields.captain legacy twin must no longer be written")
	}
}

// makeGuideAdvice returns the canonical gardener-authoring guide advice bead
// the read-the-instructions gate echoes (ka-0000007415). Carries the
// gardener-authoring-guide label so the label-keyed lookup resolves it.
func makeGuideAdvice() map[string]any {
	return map[string]any{
		"id":          gardenerGuideID,
		"title":       "Gardener authoring guide (canonical checklist)",
		"type":        "advice",
		"kind":        "data",
		"status":      "open",
		"description": "## The five non-negotiable rules\n1. Self-block.\n2. Cadence.\n3. Blob, not flat fields.",
		"fields":      map[string]any{},
		"labels":      []any{gardenerGuideLabel, "g0:role:gardener"},
	}
}

// TestGardenerStart_RefusesWithoutAckFlag is the read-the-instructions gate
// (ka-0000007415, Matt's "echo a special advice label's contents" idea).
// Without --i-read-the-gardener-instructions the command must exit non-zero,
// create NO trigger, and echo the canonical guide advice inline so the author
// loads it into context before re-running with the flag.
func TestGardenerStart_RefusesWithoutAckFlag(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	stub.addBead(makeGuideAdvice())
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	// Clear the gate flag resetGardenerFlags sets — this test exercises the refusal.
	if err := gardenerStartCmd.Flags().Set("i-read-the-gardener-instructions", "false"); err != nil {
		t.Fatal(err)
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) })

	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatal("want a refusal error without the ack flag, got nil")
	}
	if !strings.Contains(err.Error(), "--i-read-the-gardener-instructions") {
		t.Errorf("refusal must name the required flag, got: %v", err)
	}
	// No trigger may be created — the whole point of the gate.
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created when the gate refuses; got %d create(s)", len(stub.creates))
	}
	// The guide is echoed inline (resolved by label) so the author reads it first.
	echoed := out.String()
	if !strings.Contains(echoed, "Gardener authoring guide") {
		t.Errorf("guide title not echoed inline; got:\n%s", echoed)
	}
	if !strings.Contains(echoed, "five non-negotiable rules") {
		t.Errorf("guide body not echoed inline; got:\n%s", echoed)
	}
}

// TestGardenerStart_AckFlagAllowsCreate confirms the flag is the escape hatch:
// with --i-read-the-gardener-instructions the create path runs normally.
func TestGardenerStart_AckFlagAllowsCreate(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t) // sets the ack flag true
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE with the ack flag should create normally, got: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected exactly 1 CreateBead call with the ack flag, got %d", len(stub.creates))
	}
}

// TestGardenerStart_GateFallsBackToGuideID confirms the gate degrades to a
// static pointer (naming the guide id and the flag) when the guide advice
// cannot be resolved at all — an offline/empty daemon must never wedge the
// refusal or crash it.
func TestGardenerStart_GateFallsBackToGuideID(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	// Deliberately DO NOT add the guide advice: both label lookup and id
	// fallback miss, so echoGardenerGuide takes the degraded path.
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })
	if err := gardenerStartCmd.Flags().Set("i-read-the-gardener-instructions", "false"); err != nil {
		t.Fatal(err)
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) })

	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatal("want a refusal error without the ack flag, got nil")
	}
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created when the gate refuses; got %d create(s)", len(stub.creates))
	}
	echoed := out.String()
	if !strings.Contains(echoed, gardenerGuideID) {
		t.Errorf("degraded gate must name the guide id %s; got:\n%s", gardenerGuideID, echoed)
	}
}

// TestGardenerStart_RefusesEmptyProject locks in the authoring-time project
// guard (ka-0000007480, SEV-1 residual of friction report ka-0000004281). A
// gardener on a project-less epic with no --project and no $KD_PROJECT /
// $BOAT_PROJECT must FAIL LOUDLY at authoring time and create NO trigger —
// otherwise the project-less trigger silently mints a repo-/credential-less
// pod that only fails much later at fire time in the "Please run /login" loop.
func TestGardenerStart_RefusesEmptyProject(t *testing.T) {
	// Clear the environment fallbacks so project resolution genuinely yields "".
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")

	stub := newGardenerStub()
	// Epic carries NO project: label — the classic project-less origin.
	stub.addBead(makeEpic("gd-EPIC1", "Orphan epic"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatal("want an error for a project-less gardener, got nil")
	}
	if !strings.Contains(err.Error(), "project is empty") {
		t.Errorf("error should explain the empty project, got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created when the project is empty; got %d create(s)", len(stub.creates))
	}
}

// TestGardenerStart_RejectsTriggerAnchor is the regression guard for bug
// ka-0000000141: `gb gardener start` pointed at a type=trigger bead (the prod
// repro anchored at a standing release-owner gardener) must FAIL LOUDLY, create
// NO trigger, name the actual type, and point the operator at `gb trigger
// enable <id>` — not silently mint a duplicate wrong-shape generic gardener
// whose bogus gardener-epic label then made the dark trigger unclearable.
func TestGardenerStart_RejectsTriggerAnchor(t *testing.T) {
	stub := newGardenerStub()
	// A standing gardener trigger — a valid bead of the WRONG type. The old
	// GetBead-only guard proved it existed and sailed straight through.
	stub.addBead(map[string]any{
		"id":     "gd-TRIG1",
		"title":  "Gardenio gardener-release-owner v3",
		"type":   "trigger",
		"kind":   "data",
		"status": "open",
		"fields": map[string]any{},
		"labels": []any{"project:gardenio", "role:gardener-release-owner"},
	})
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-TRIG1"})
	if err == nil {
		t.Fatal("want an error for a type=trigger anchor, got nil")
	}
	// The message must name the actual type so the operator knows what they
	// mis-targeted.
	if !strings.Contains(err.Error(), `"trigger"`) {
		t.Errorf("error should name the actual type (trigger), got: %v", err)
	}
	// ...and point at the fix for the trigger case specifically.
	if !strings.Contains(err.Error(), "gb trigger enable gd-TRIG1") {
		t.Errorf("error should point at `gb trigger enable gd-TRIG1`, got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created for a non-epic anchor; got %d create(s)", len(stub.creates))
	}
}

// TestGardenerStart_RejectsNonContainerAnchor covers the general case: any
// non-container anchor (here a plain task) is rejected with its type named,
// but WITHOUT the trigger-specific `gb trigger enable` hint (which only makes
// sense when the anchor is itself a trigger).
func TestGardenerStart_RejectsNonContainerAnchor(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-TASK1",
		"title":  "A plain task, not an epic",
		"type":   "task",
		"kind":   "issue",
		"status": "open",
		"fields": map[string]any{},
		"labels": []any{"project:gardenio"},
	})
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-TASK1"})
	if err == nil {
		t.Fatal("want an error for a type=task anchor, got nil")
	}
	if !strings.Contains(err.Error(), `"task"`) {
		t.Errorf("error should name the actual type (task), got: %v", err)
	}
	if strings.Contains(err.Error(), "gb trigger enable") {
		t.Errorf("the trigger-enable hint must only appear for a trigger anchor, got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created for a non-epic anchor; got %d create(s)", len(stub.creates))
	}
}

// TestGardenerStart_AcceptsMoleculeAnchor is the positive twin: a poured
// molecule acts as an epic with children (beadsapi.IsPhaseContainer treats it
// as a container), so anchoring a gardener to one is legitimate and must
// succeed. This proves the guard rejects by SHAPE, not a bare `Type=="epic"`
// check that would drop the molecule case.
func TestGardenerStart_AcceptsMoleculeAnchor(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-MOL1",
		"title":  "A poured workflow molecule",
		"type":   "molecule",
		"kind":   "issue",
		"status": "open",
		"fields": map[string]any{},
		"labels": []any{"project:gardenio"},
	})
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-MOL1"}); err != nil {
		t.Fatalf("RunE on a molecule anchor should succeed, got: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call for a molecule anchor, got %d", len(stub.creates))
	}
}

// TestGardenerStart_RejectsStandingProseWithoutMarker locks in the
// authoring-time standing-marker guard (bug ka-0000004311). A gardener whose
// --terminal-goal DECLARES it perpetual ("STANDING …", "does NOT
// self-complete") but carries no machine-readable standing marker
// (gardener.standing:true or the perpetual:true label) must FAIL LOUDLY and
// create NO trigger — otherwise the marker-less gardener is silently reaped into
// the closed-but-enabled dark shape.
func TestGardenerStart_RejectsStandingProseWithoutMarker(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Intake sweep", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	if err := gardenerStartCmd.Flags().Set("terminal-goal",
		"STANDING sweep gardener: goes dormant between sweeps; does NOT self-complete on an empty queue."); err != nil {
		t.Fatalf("set terminal-goal: %v", err)
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatal("want an error for standing prose without a marker, got nil")
	}
	if !strings.Contains(err.Error(), "ka-0000004311") {
		t.Errorf("error should name the bug id, got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("no trigger must be created when standing prose lacks a marker; got %d create(s)", len(stub.creates))
	}
}

// TestGardenerStart_AcceptsStandingProseWithPerpetualLabel is the positive twin:
// the SAME standing terminal_goal is accepted once the author attaches the
// perpetual:true label (--label), because the engine can then read
// standing-ness from a machine marker.
func TestGardenerStart_AcceptsStandingProseWithPerpetualLabel(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Intake sweep", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	if err := gardenerStartCmd.Flags().Set("terminal-goal",
		"STANDING sweep gardener: goes dormant between sweeps; does NOT self-complete on an empty queue."); err != nil {
		t.Fatalf("set terminal-goal: %v", err)
	}
	if err := gardenerStartCmd.Flags().Set("label", "perpetual:true"); err != nil {
		t.Fatalf("set label: %v", err)
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE with perpetual:true label should succeed, got: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	// The perpetual:true marker landed in the persisted label set.
	labels, _ := stub.creates[0]["labels"].([]any)
	var sawMarker bool
	for _, l := range labels {
		if s, _ := l.(string); s == "perpetual:true" {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Errorf("persisted labels missing perpetual:true marker: %v", labels)
	}
}

// TestGardenerStart_CoercesCooldownAndMaxConcurrent verifies the
// --cooldown/--max-concurrent flags are coerced to JSON integers before the
// CreateBead call (ka-0000004844). A go-style duration cooldown ("90m") must
// be rounded to whole minutes, and an explicit max-concurrent must override
// the default.
func TestGardenerStart_CoercesCooldownAndMaxConcurrent(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	if err := gardenerStartCmd.Flags().Set("cooldown", "90m"); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	if err := gardenerStartCmd.Flags().Set("max-concurrent", "7"); err != nil {
		t.Fatalf("set max-concurrent: %v", err)
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	fields, _ := stub.creates[0]["fields"].(map[string]any)
	if got, ok := fields["cooldown_minutes"].(float64); !ok || got != 90 {
		t.Errorf("fields.cooldown_minutes = %#v, want JSON number 90 (from 90m)", fields["cooldown_minutes"])
	}
	if got, ok := fields["max_concurrent"].(float64); !ok || got != 7 {
		t.Errorf("fields.max_concurrent = %#v, want JSON number 7", fields["max_concurrent"])
	}
}

// TestGardenerStart_StructuredStopDecodesAndScheduler verifies the gardener blob
// `gb gardener start` emits round-trips through the REAL trigger-engine decode
// path (bridge.ParseTriggerDef → GardenerSpec) AND parses cleanly via the
// scheduler's StopSpec / progress.Spec parsers. This is the load-bearing
// acceptance check for gd-Lee2eG1pST: the scheduler reads the loop spec ONLY
// from the nested gardener blob, so a fresh gardener must produce one the
// scheduler can enforce — not just prose the agent has to honor.
func TestGardenerStart_StructuredStopDecodesAndScheduler(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = ""
	t.Cleanup(func() { operatedBy = oldOperated })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	fields, _ := stub.creates[0]["fields"].(map[string]any)
	capRaw, _ := fields["gardener"].(string)
	if strings.TrimSpace(capRaw) == "" {
		t.Fatal("gardener blob absent")
	}

	// Decode through the SAME path the trigger-engine uses at load time.
	detail := &beadsapi.BeadDetail{
		ID:     "gd-CAP1",
		Title:  "Gardener for gd-EPIC1",
		Fields: map[string]string{"action": "spawn_agent", "role": "gardener", "gardener": capRaw},
	}
	def := bridge.ParseTriggerDef(detail, "schedule")
	if def.Gardener == nil {
		t.Fatal("ParseTriggerDef produced a nil Gardener — the blob is not a valid GardenerSpec")
	}
	cap := def.Gardener

	// run_budget is the engine's self-block run cap (gardenerPreFireCheck gates
	// on RunBudget > 0).
	if cap.RunBudget <= 0 {
		t.Errorf("GardenerSpec.RunBudget = %d, want > 0 (mandatory self-block, ka-0000000974)", cap.RunBudget)
	}
	if cap.AssignBead != "gd-EPIC1" {
		t.Errorf("GardenerSpec.AssignBead = %q, want gd-EPIC1", cap.AssignBead)
	}
	if strings.TrimSpace(cap.TerminalGoal) == "" {
		t.Error("GardenerSpec.TerminalGoal is empty")
	}

	// The stop blob must parse via the scheduler's StopSpec parser (the one
	// gardenerPreFireCheck calls) and be non-empty with the required kinds.
	stopSpec, err := scheduler.ParseStopSpec(string(cap.Stop))
	if err != nil {
		t.Fatalf("scheduler.ParseStopSpec failed on emitted stop blob: %v", err)
	}
	if stopSpec.IsZero() {
		t.Fatal("emitted stop spec is zero — the scheduler would enforce nothing")
	}
	loopKinds := map[string]bool{}
	for _, e := range stopSpec.LoopEnd.Any {
		loopKinds[e.Kind] = true
	}
	for _, want := range []string{scheduler.StopKindBeadQuery, scheduler.StopKindMaxRuns, scheduler.StopKindNoProgressFor} {
		if !loopKinds[want] {
			t.Errorf("stop.loop_end missing kind %q (have %v)", want, loopKinds)
		}
	}
	hardKinds := map[string]bool{}
	for _, e := range stopSpec.HardStop.Any {
		hardKinds[e.Kind] = true
	}
	if !hardKinds[scheduler.StopKindWallClock] {
		t.Errorf("stop.hard_stop missing kind %q (have %v)", scheduler.StopKindWallClock, hardKinds)
	}

	// The progress blob must parse via the scheduler's progress parser and
	// surface in the wrap-up so gb stop enforces progress_score.
	progSpec, err := progress.ParseSpec(string(cap.Progress))
	if err != nil {
		t.Fatalf("progress.ParseSpec failed on emitted progress blob: %v", err)
	}
	if !progSpec.Surface.InWrapup {
		t.Error("progress.surface.in_wrapup = false, want true (gb stop must enforce progress_score)")
	}
	if len(progSpec.Events) == 0 {
		t.Error("progress spec has no events — no_progress_for has nothing to count")
	}
}

// makeUserBead builds a type=user / kind=config identity bead, the shape
// resolveGardenerOwner looks up to resolve user_bead_id from an operator alias.
func makeUserBead(id, slackID, email string) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  "user " + slackID,
		"type":   "user",
		"kind":   "config",
		"status": "open",
		"fields": map[string]any{"slack_id": slackID, "email": email},
		"labels": []any{},
	}
}

// gardenerBlobFromCreate decodes the `gardener` JSON field off a captured
// CreateBead request, returning nil when absent.
func gardenerBlobFromCreate(t *testing.T, create map[string]any) map[string]any {
	t.Helper()
	fields, _ := create["fields"].(map[string]any)
	raw, ok := fields["gardener"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		t.Fatalf("gardener field is not valid JSON: %v\n%s", err, raw)
	}
	return blob
}

// assertGardenerSelfBlock verifies the gardener blob carries the REAL structured
// self-block mandated by advice ka-0000000974 and the acceptance criteria of
// gd-Lee2eG1pST: a run_budget, a loop_end.any containing [bead_query
// open-children==0, max_runs, no_progress_for], a hard_stop with a wall_clock,
// progress.surface.in_wrapup=true, terminal_goal, and assign_bead. These are
// the fields the trigger-engine's GardenerSpec decodes and the scheduler
// enforces, so a fresh gardener stops itself instead of looping forever.
func assertGardenerSelfBlock(t *testing.T, blob map[string]any, epicID string) {
	t.Helper()

	// run_budget > 0 — the self-block run cap (JSON numbers decode as float64).
	if got, ok := blob["run_budget"].(float64); !ok || got <= 0 {
		t.Errorf("gardener.run_budget = %#v, want a positive integer (self-block run cap)", blob["run_budget"])
	}
	if got, _ := blob["assign_bead"].(string); got != epicID {
		t.Errorf("gardener.assign_bead = %q, want %q", got, epicID)
	}
	if got, _ := blob["terminal_goal"].(string); strings.TrimSpace(got) == "" {
		t.Error("gardener.terminal_goal is empty — the loop has no declared goal")
	}

	stop, ok := blob["stop"].(map[string]any)
	if !ok {
		t.Fatalf("gardener.stop is not an object: %#v", blob["stop"])
	}

	// loop_end.any must contain bead_query (open-children==0, parent=epic),
	// max_runs, and no_progress_for.
	loopEnd := stopSlotKinds(t, stop, "loop_end")
	for _, kind := range []string{"bead_query", "max_runs", "no_progress_for"} {
		if !loopEnd[kind] {
			t.Errorf("gardener.stop.loop_end.any missing kind %q (have %v)", kind, loopEnd)
		}
	}
	// The bead_query loop_end must be parent-scoped to the epic with op "=="
	// and value "0" — "all open children of the epic are gone".
	assertBeadQueryLoopEnd(t, stop, epicID)

	// hard_stop.any must contain a wall_clock (the absolute ceiling).
	hardStop := stopSlotKinds(t, stop, "hard_stop")
	if !hardStop["wall_clock"] {
		t.Errorf("gardener.stop.hard_stop.any missing kind %q (have %v)", "wall_clock", hardStop)
	}

	// progress.surface.in_wrapup must be true so `gb stop` enforces
	// progress_score / progress_summary on each gardener wrap-up.
	prog, ok := blob["progress"].(map[string]any)
	if !ok {
		t.Fatalf("gardener.progress is not an object: %#v", blob["progress"])
	}
	surface, ok := prog["surface"].(map[string]any)
	if !ok {
		t.Fatalf("gardener.progress.surface is not an object: %#v", prog["surface"])
	}
	if got, _ := surface["in_wrapup"].(bool); !got {
		t.Errorf("gardener.progress.surface.in_wrapup = %#v, want true (so gb stop enforces progress_score)", surface["in_wrapup"])
	}
}

// stopSlotKinds returns the set of stop-entry kinds present in stop.<slot>.any.
func stopSlotKinds(t *testing.T, stop map[string]any, slot string) map[string]bool {
	t.Helper()
	s, ok := stop[slot].(map[string]any)
	if !ok {
		t.Fatalf("gardener.stop.%s is not an object: %#v", slot, stop[slot])
	}
	entries, ok := s["any"].([]any)
	if !ok {
		t.Fatalf("gardener.stop.%s.any is not an array: %#v", slot, s["any"])
	}
	kinds := map[string]bool{}
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok {
			if k, _ := entry["kind"].(string); k != "" {
				kinds[k] = true
			}
		}
	}
	return kinds
}

// assertBeadQueryLoopEnd verifies the loop_end bead_query entry is the
// open-children gate: parent=epic, op="==", value="0".
func assertBeadQueryLoopEnd(t *testing.T, stop map[string]any, epicID string) {
	t.Helper()
	s, _ := stop["loop_end"].(map[string]any)
	entries, _ := s["any"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if k, _ := entry["kind"].(string); k != "bead_query" {
			continue
		}
		if got, _ := entry["parent"].(string); got != epicID {
			t.Errorf("loop_end bead_query parent = %q, want %q", got, epicID)
		}
		if got, _ := entry["op"].(string); got != "==" {
			t.Errorf("loop_end bead_query op = %q, want ==", got)
		}
		if got, _ := entry["value"].(string); got != "0" {
			t.Errorf("loop_end bead_query value = %q, want 0", got)
		}
		return
	}
	t.Error("loop_end.any has no bead_query entry")
}

// TestGardenerStart_StampsOwnerFromSlackOperator verifies that a slack:<id>
// operator stamps a gardener JSON blob carrying both slack_user_id (from the
// alias) and user_bead_id (resolved via the matching user bead) — so the
// beads-viewer owner resolver's tiers 1-2 work for thread-bridge / schedule-
// mirror gardeners (ka-0000003284, Gardener Page v2 R6).
func TestGardenerStart_StampsOwnerFromSlackOperator(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	stub.addBead(makeUserBead("gd-USER1", "U0OPERATOR1", "matthew.baker@acme.example"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	blob := gardenerBlobFromCreate(t, stub.creates[0])
	if blob == nil {
		t.Fatal("gardener owner blob not stamped for slack operator")
	}
	if got, _ := blob["slack_user_id"].(string); got != "U0OPERATOR1" {
		t.Errorf("gardener.slack_user_id = %q, want U0OPERATOR1", got)
	}
	if got, _ := blob["user_bead_id"].(string); got != "gd-USER1" {
		t.Errorf("gardener.user_bead_id = %q, want gd-USER1", got)
	}
}

// TestGardenerStart_StampsOwnerFromOidcOperator verifies that an oidc:<email>
// operator resolves the user bead by email and derives the canonical slack id
// from it, so both owner fields are populated even when the operator alias is
// email-shaped.
func TestGardenerStart_StampsOwnerFromOidcOperator(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	stub.addBead(makeUserBead("gd-USER1", "U0OPERATOR1", "matthew.baker@acme.example"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "oidc:matthew.baker@acme.example"
	t.Cleanup(func() { operatedBy = oldOperated })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	blob := gardenerBlobFromCreate(t, stub.creates[0])
	if blob == nil {
		t.Fatal("gardener owner blob not stamped for oidc operator")
	}
	if got, _ := blob["user_bead_id"].(string); got != "gd-USER1" {
		t.Errorf("gardener.user_bead_id = %q, want gd-USER1 (resolved by email)", got)
	}
	if got, _ := blob["slack_user_id"].(string); got != "U0OPERATOR1" {
		t.Errorf("gardener.slack_user_id = %q, want U0OPERATOR1 (derived from user bead)", got)
	}
}

// TestGardenerStart_SlackOperatorWithoutUserBead still stamps the slack id from
// the alias even when no user bead exists yet — tier-2 resolution must work
// before the identity is seeded. user_bead_id is simply omitted.
func TestGardenerStart_SlackOperatorWithoutUserBead(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0NOBEAD99"
	t.Cleanup(func() { operatedBy = oldOperated })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	blob := gardenerBlobFromCreate(t, stub.creates[0])
	if blob == nil {
		t.Fatal("gardener owner blob not stamped — slack id should stamp even without a user bead")
	}
	if got, _ := blob["slack_user_id"].(string); got != "U0NOBEAD99" {
		t.Errorf("gardener.slack_user_id = %q, want U0NOBEAD99", got)
	}
	if _, ok := blob["user_bead_id"]; ok {
		t.Errorf("gardener.user_bead_id present %v, want absent (no user bead)", blob["user_bead_id"])
	}
}

// TestGardenerStart_NoOwnerForSentinelOperator verifies E2E fixture / static
// gardeners stay ownerless: a non-human sentinel operator (or empty operator)
// stamps the structured loop spec but NO owner fields. The gardener blob is
// always present now (it carries the mandatory self-block, ka-0000000974) — what
// must be absent for a sentinel is slack_user_id / user_bead_id.
func TestGardenerStart_NoOwnerForSentinelOperator(t *testing.T) {
	for _, op := range []string{"", "system:scheduler", "fleet:gardenio", "garbage-no-colon"} {
		t.Run(op, func(t *testing.T) {
			stub := newGardenerStub()
			stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
			setupGardenerStub(t, stub)
			resetGardenerFlags(t)
			t.Cleanup(func() { resetGardenerFlags(t) })

			oldOperated := operatedBy
			operatedBy = op
			t.Cleanup(func() { operatedBy = oldOperated })

			cmd := gardenerStartCmd
			cmd.SetContext(context.Background())
			if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
			blob := gardenerBlobFromCreate(t, stub.creates[0])
			if blob == nil {
				t.Fatalf("gardener blob absent for operator %q — the loop spec must always be stamped", op)
			}
			// Loop spec present; owner fields absent.
			assertGardenerSelfBlock(t, blob, "gd-EPIC1")
			if _, ok := blob["slack_user_id"]; ok {
				t.Errorf("gardener.slack_user_id stamped for sentinel operator %q: %v", op, blob["slack_user_id"])
			}
			if _, ok := blob["user_bead_id"]; ok {
				t.Errorf("gardener.user_bead_id stamped for sentinel operator %q: %v", op, blob["user_bead_id"])
			}
		})
	}
}

func TestGardenerStart_IdempotentReturnsExisting(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatalf("expected non-zero exit when gardener already exists")
	}
	if !strings.Contains(err.Error(), "gd-CAP1") {
		t.Errorf("error did not name existing schedule: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("expected 0 CreateBead calls when gardener exists, got %d", len(stub.creates))
	}
}

// TestGardenerStart_IdempotentAgainstTriggerGardener is the realistic case
// post-D10: a gardener already exists in the current trigger:schedule shape
// (epic pinned by the gardener-epic:<id> label, no assign_bead field), and a
// second `gb gardener start` must dedup against it.
func TestGardenerStart_IdempotentAgainstTriggerGardener(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatalf("expected non-zero exit when gardener already exists")
	}
	if !strings.Contains(err.Error(), "gd-CAP1") {
		t.Errorf("error did not name existing gardener: %v", err)
	}
	// idempotency message must keep the "already exists" phrase the bridge's
	// epic-gardener-mention handler matches on.
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error must contain 'already exists' (bot_mentions idempotency contract): %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("expected 0 CreateBead calls when gardener exists, got %d", len(stub.creates))
	}
}

func TestGardenerStart_DisabledScheduleDoesNotBlock(t *testing.T) {
	// A gardener schedule that is enabled=false shouldn't dedup-block a fresh
	// start: the operator already explicitly disabled the loop.
	// The epic carries a project: label so gardener-start's project resolution
	// is hermetic — without it the test only passes when $BOAT_PROJECT/$KD_PROJECT
	// happen to be set in the env (green locally, red on CI's clean env). Matches
	// the sibling gardener-start tests (ka-0000003920).
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2", "project:gardenio"))
	stub.addBead(makeGardenerSchedule("gd-CAP-OLD", "gd-EPIC1", map[string]any{"enabled": "false"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected new schedule to be created when prior is disabled, got %d", len(stub.creates))
	}
}

// ── gb gardener stop ──────────────────────────────────────────────────────

func TestGardenerStop_ClosesSchedule(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "epic complete")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.closes["gd-CAP1"]) != 1 {
		t.Fatalf("expected 1 close call on gd-CAP1, got %d", len(stub.closes["gd-CAP1"]))
	}
	if len(stub.comments["gd-CAP1"]) != 1 {
		t.Fatalf("expected 1 audit comment, got %d", len(stub.comments["gd-CAP1"]))
	}
	text, _ := stub.comments["gd-CAP1"][0]["text"].(string)
	if !strings.Contains(text, "epic complete") {
		t.Errorf("comment did not include reason: %q", text)
	}
}

func TestGardenerStop_RequiresReason(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "")
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil || !strings.Contains(err.Error(), "--reason is required") {
		t.Fatalf("expected --reason required error, got %v", err)
	}
}

func TestGardenerStop_NoGardenerFound(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "ad-hoc cleanup")
	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil || !strings.Contains(err.Error(), "no gardener schedule found") {
		t.Fatalf("expected no-gardener error, got %v", err)
	}
}

// makeGardenerAgent builds a type=agent bead the way the trigger engine stamps a
// spawned gardener iteration: trigger_id + schedule_id both point at the gardener
// bead. status="closed" marks a completed run. Used to seed the ground-truth
// spawn count `gb gardener status` derives (bug ka-0000005640).
func makeGardenerAgent(id, gardenerID, status string) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  "agent " + id,
		"type":   "agent",
		"kind":   "config",
		"status": status,
		"fields": map[string]any{
			"trigger_id":  gardenerID,
			"schedule_id": gardenerID,
		},
		"labels": []string{},
	}
}

// makeChild builds an issue-kind child bead for the subtree map.
func makeChild(id, title, typ, status string) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   typ,
		"kind":   "issue",
		"status": status,
		"fields": map[string]any{},
		"labels": []string{},
	}
}

// TestGardenerStop_RefusesWithOpenChildren is the regression test for the
// fail-closed guard (ka-0000008241): stop must refuse to close a gardener whose
// epic subtree still has open/in_progress/blocked issue-kind children, even
// when an upstream count claimed the plan was terminal. The child here is
// project:monorepo under a project:gardenio plan — exactly the cross-project
// shape the undercount missed. Nothing must be closed.
func TestGardenerStop_RefusesWithOpenChildren(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "unify plan-gardener", "project:gardenio"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	// A live cross-project child in the subtree.
	stub.addChild("gd-EPIC1", makeChild("gd-KID1", "monorepo task", "task", "in_progress"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "terminal goal met")

	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil || !strings.Contains(err.Error(), "open child") {
		t.Fatalf("expected refusal citing open children, got %v", err)
	}
	if !strings.Contains(err.Error(), "gd-KID1") {
		t.Errorf("refusal should name the open child, got: %v", err)
	}
	if len(stub.closes["gd-CAP1"]) != 0 {
		t.Fatalf("gardener must NOT be closed while children are open, got %d close calls", len(stub.closes["gd-CAP1"]))
	}
}

// TestGardenerStop_IgnoreOpenChildrenOverride covers the explicit override:
// --ignore-open-children lets an operator stop past the guard once they have
// confirmed the remaining children are not blocking closure.
func TestGardenerStop_IgnoreOpenChildrenOverride(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "unify plan-gardener", "project:gardenio"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	stub.addChild("gd-EPIC1", makeChild("gd-KID1", "monorepo task", "task", "open"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "children reparented, safe to stop")
	_ = cmd.Flags().Set("ignore-open-children", "true")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE with override: %v", err)
	}
	if len(stub.closes["gd-CAP1"]) != 1 {
		t.Fatalf("expected 1 close call under --ignore-open-children, got %d", len(stub.closes["gd-CAP1"]))
	}
}

// TestGardenerStop_ClosesWhenSubtreeHasOnlyClosedAndBundle confirms the guard
// does NOT trip on a drained subtree: closed children and a (never-closing)
// type=bundle narrative child must not keep the gardener alive.
func TestGardenerStop_ClosesWhenSubtreeHasOnlyClosedAndBundle(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "unify plan-gardener", "project:gardenio"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	stub.addChild("gd-EPIC1", makeChild("gd-KID1", "done task", "task", "closed"))
	// An OPEN bundle (the plan's narrative doc container) must be excluded —
	// it never closes on its own and is not a unit of remaining work.
	stub.addChild("gd-EPIC1", makeChild("gd-BUND", "plan-of-record", "bundle", "open"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStopCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "epic drained")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE on drained subtree: %v", err)
	}
	if len(stub.closes["gd-CAP1"]) != 1 {
		t.Fatalf("expected 1 close call on a drained subtree, got %d", len(stub.closes["gd-CAP1"]))
	}
}

// ── gb gardener force-close ───────────────────────────────────────────────

func TestGardenerForceClose_SetsFieldAndAuditsComment(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerForceCloseCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "gardener in a wedge")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	patches := stub.patches["gd-CAP1"]
	if len(patches) != 1 {
		t.Fatalf("expected 1 PATCH, got %d", len(patches))
	}
	fields, _ := patches[0]["fields"].(map[string]any)
	if got := fields["force_close"]; got != "true" {
		t.Errorf("fields.force_close = %v, want \"true\"", got)
	}
	if got, _ := fields["force_close_reason"].(string); got != "gardener in a wedge" {
		t.Errorf("fields.force_close_reason = %q, want %q", got, "gardener in a wedge")
	}
	if got, _ := fields["force_close_set_by"].(string); got != "test-actor" {
		t.Errorf("fields.force_close_set_by = %q, want test-actor", got)
	}
	if got, _ := fields["force_close_set_at"].(string); strings.TrimSpace(got) == "" {
		t.Errorf("fields.force_close_set_at is empty")
	}
	// No close call — force-close intentionally leaves the schedule open
	// so T4's hard_stop.manual_force tears it down with a single reason.
	if len(stub.closes["gd-CAP1"]) != 0 {
		t.Errorf("force-close should not close the schedule directly, got %d closes", len(stub.closes["gd-CAP1"]))
	}
	if len(stub.comments["gd-CAP1"]) != 1 {
		t.Errorf("expected 1 audit comment, got %d", len(stub.comments["gd-CAP1"]))
	}
}

func TestGardenerForceClose_AlreadySetIsNoop(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", map[string]any{"force_close": "true"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerForceCloseCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "second attempt")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.patches["gd-CAP1"]) != 0 {
		t.Errorf("expected 0 PATCH calls when force_close already true, got %d", len(stub.patches["gd-CAP1"]))
	}
}

// TestGardenerForceClose_TriggerShapeDisablesAndCloses: a trigger:schedule
// gardener has no force_close stop spec (the schema rejects the field), so
// force-close disables (enabled=false) and closes the bead immediately.
func TestGardenerForceClose_TriggerShapeDisablesAndCloses(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerForceCloseCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "gardener in a wedge")

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	patches := stub.patches["gd-CAP1"]
	if len(patches) != 1 {
		t.Fatalf("expected 1 PATCH (enabled=false), got %d", len(patches))
	}
	fields, _ := patches[0]["fields"].(map[string]any)
	if got, ok := fields["enabled"].(bool); !ok || got {
		t.Errorf("fields.enabled = %#v, want JSON bool false", fields["enabled"])
	}
	if _, ok := fields["force_close"]; ok {
		t.Errorf("force_close must NOT be written on a trigger gardener (schema rejects it)")
	}
	if len(stub.closes["gd-CAP1"]) != 1 {
		t.Errorf("expected 1 close call on the trigger gardener, got %d", len(stub.closes["gd-CAP1"]))
	}
}

// ── gb gardener status ────────────────────────────────────────────────────

func TestGardenerStatus_GoldenOutput(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":           "7,37 * * * *",
		"timezone":       "America/Los_Angeles",
		"runs_spawned":   "3",
		"runs_completed": "2",
		"max_runs":       "20",
		"last_run":       "2026-04-29T01:30:00Z",
		"last_agent_id":  "gd-AGENT9",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Gardener schedule gd-CAP1",
		"assign_bead:  gd-EPIC1",
		"role:         gardener",
		"enabled:      true",
		"7,37 * * * *",
		"America/Los_Angeles",
		"spawned=3",
		"completed=2",
		"max=20",
		"last_run:     2026-04-29T01:30:00Z",
		"last_agent:   gd-AGENT9",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestGardenerStatus_JSONNeverFiredEmitsZero pins the JSON projection for a
// gardener that has NEVER fired: runs_spawned/runs_completed must serialize as
// "0", never "" (bug ka-0000002620). The flat field and the blob are both
// absent, reconcileRunCount returns the empty counter unchanged, and the text
// render floors it via defaultIfEmpty — but the --json projection historically
// emitted the raw "". A caller writing the natural liveness guard
// `gb gardener status --json | jq -r '.runs_spawned'` then numeric-comparing
// gets "" and `"" != "0"` is TRUE, so a dark gardener is falsely certified live.
func TestGardenerStatus_JSONNeverFiredEmitsZero(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "just-armed gardener"))
	// No flat run fields, no gardener-blob counters, no spawned agents: every
	// counter source is empty, so spawnedStr/completedStr end as "".
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron": "*/10 * * * *",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON; resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())

	out := captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-EPIC1"}) })

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal status JSON: %v\nraw:\n%s", err, out)
	}
	for _, key := range []string{"runs_spawned", "runs_completed"} {
		if got[key] != "0" {
			t.Errorf("%s = %q, want \"0\" — a never-fired gardener must emit 0, not \"\", so a numeric liveness guard cannot certify it live (ka-0000002620)", key, got[key])
		}
	}
}

// TestGardenerStatus_ProgressSummaryGap covers the audit-F2 conformance note
// (ka-0000005466): a gardener whose rolling progress_state records a run with a
// non-zero progress_score but an empty progress_summary must be flagged on the
// status scorecard, in both the text render and the --json projection. The gap
// leaks past the score/summary co-require under --force stops and soft schedules.
func TestGardenerStatus_ProgressSummaryGap(t *testing.T) {
	// Two scored runs: one summarized (conformant), one with an empty summary
	// (the gap), plus a score-0 run that never counts either way.
	progState := `{"runs":[` +
		`{"progress_score":2,"progress_summary":"closed gd-A"},` +
		`{"progress_score":1,"progress_summary":""},` +
		`{"progress_score":0,"progress_summary":""}` +
		`]}`

	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "conformance gardener"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":           "*/10 * * * *",
		"progress_state": progState,
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	// Text render carries the flag.
	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "progress_summary gap") {
		t.Errorf("text status missing progress_summary gap flag\nfull output:\n%s", out)
	}
	if !strings.Contains(out, "1 of 2 scored run") {
		t.Errorf("text status missing the 1-of-2 conformance ratio\nfull output:\n%s", out)
	}

	// --json projection carries the machine-readable counts the meta scorecard reads.
	resetGardenerFlags(t)
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	jcmd := gardenerStatusCmd
	jcmd.SetContext(context.Background())
	jout := captureStdout(t, func() error { return jcmd.RunE(jcmd, []string{"gd-EPIC1"}) })
	var got map[string]any
	if err := json.Unmarshal(jout, &got); err != nil {
		t.Fatalf("unmarshal status JSON: %v\nraw:\n%s", err, jout)
	}
	if got["progress_summary_gaps"] != float64(1) {
		t.Errorf("progress_summary_gaps = %v, want 1", got["progress_summary_gaps"])
	}
	if got["progress_summary_scored"] != float64(2) {
		t.Errorf("progress_summary_scored = %v, want 2", got["progress_summary_scored"])
	}
}

// TestGardenerStatus_FallsBackToGardenerBlob covers the real-world drift behind
// ka-0000005388: the scheduler records runtime counters inside the nested
// `gardener` JSON blob (via gardenerPostFire) and never mirrors them to the flat
// schedule fields. A gardener that has genuinely fired must therefore render its
// real spawned/completed/max/last_agent from the blob, not spawned=0 / —.
func TestGardenerStatus_FallsBackToGardenerBlob(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "monorepo-dark Phase 0"))
	// Flat run fields intentionally ABSENT — only the nested blob carries state.
	capBlob := `{"run_budget":12,"max_runs":12,"runs_spawned":3,"runs_completed":2,` +
		`"last_agent_id":"gd-Y7Cj8EhoZ8",` +
		`"run_log":[{"agent":"gd-3veHGPaNYU","ended_at":"2026-06-09T00:20:26Z","outcome":"done"},` +
		`{"agent":"gd-RJU9penvOv","ended_at":"2026-06-09T00:44:53Z","outcome":"done"}]}`
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":     "7,37 * * * *",
		"timezone": "America/Los_Angeles",
		"gardener": capBlob,
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"spawned=3",
		"completed=2",
		"max=12",
		"last_agent:   gd-Y7Cj8EhoZ8",
		"last_run:     2026-06-09T00:44:53Z", // newest run_log entry's ended_at
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q (blob fallback failed)\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "spawned=0") {
		t.Errorf("status still shows spawned=0 despite populated gardener blob\nfull output:\n%s", out)
	}
}

// TestGardenerStatus_FlatFieldsWinOverBlob guards precedence: when both the flat
// schedule fields and the nested blob are present, the explicit flat fields are
// authoritative (the blob is only a fallback).
func TestGardenerStatus_FlatFieldsWinOverBlob(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "monorepo-dark Phase 0"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"runs_spawned":  "9",
		"last_agent_id": "gd-FLAT",
		"gardener":      `{"runs_spawned":3,"last_agent_id":"gd-BLOB"}`,
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "spawned=9") || !strings.Contains(out, "last_agent:   gd-FLAT") {
		t.Errorf("flat fields should win over blob\nfull output:\n%s", out)
	}
}

// TestGardenerStatus_DerivesCountsFromSpawnedAgents is the regression guard for
// bug ka-0000005640: a gardener whose mutable run counters are frozen at 0 (the
// flat field was never populated and gardenerPostFire's blob write was dropped by
// the daemon's integer-field rejection, ka-0000007976) must STILL report its real
// spawned/completed count, derived from the agent beads it actually spawned. This
// is the exact ayce-eater symptom: 'runs: spawned=0 completed=0' while the engine
// had demonstrably fired runs.
func TestGardenerStatus_DerivesCountsFromSpawnedAgents(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "ayce-eater governor"))
	// No flat run fields, no gardener blob counters — both mutable sources dead.
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron": "*/10 * * * *",
	}))
	// Three real spawned iterations; two closed (completed), one still running.
	stub.addBead(makeGardenerAgent("gd-AG1", "gd-CAP1", "closed"))
	stub.addBead(makeGardenerAgent("gd-AG2", "gd-CAP1", "closed"))
	stub.addBead(makeGardenerAgent("gd-AG3", "gd-CAP1", "in_progress"))
	// A decoy agent spawned by a DIFFERENT gardener must not be counted.
	stub.addBead(makeGardenerAgent("gd-OTHER", "gd-CAPX", "closed"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "spawned=3") {
		t.Errorf("status must derive spawned=3 from the spawned agent beads (bug ka-0000005640)\nfull output:\n%s", out)
	}
	if !strings.Contains(out, "completed=2") {
		t.Errorf("status must derive completed=2 from the closed spawned agents\nfull output:\n%s", out)
	}
	if strings.Contains(out, "spawned=0") {
		t.Errorf("status still shows spawned=0 despite 3 spawned agent beads\nfull output:\n%s", out)
	}
}

// TestGardenerStatus_KeepsHigherCounterWhenBeadsPruned guards the pruning-safe
// half of the reconcile: when the stored lifetime counter exceeds the live
// spawned-bead count (older iteration beads were pruned), the higher counter
// must win so the readout does not regress below the true lifetime tally.
func TestGardenerStatus_KeepsHigherCounterWhenBeadsPruned(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "long-lived gardener"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"runs_spawned":   "40",
		"runs_completed": "38",
	}))
	// Only 2 iteration beads survive (the rest pruned).
	stub.addBead(makeGardenerAgent("gd-AG1", "gd-CAP1", "closed"))
	stub.addBead(makeGardenerAgent("gd-AG2", "gd-CAP1", "in_progress"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "spawned=40") || !strings.Contains(out, "completed=38") {
		t.Errorf("higher lifetime counter must win over a pruned live count\nfull output:\n%s", out)
	}
}

// TestReconcileRunCount_MaxAndPreserveEmpty unit-tests the fusion rule directly:
// the displayed count is max(counter, ground-truth); an all-zero input preserves
// the original counter string verbatim so the existing defaultIfEmpty rendering
// is untouched.
func TestReconcileRunCount_MaxAndPreserveEmpty(t *testing.T) {
	cases := []struct {
		name        string
		counter     string
		groundTruth int
		haveGT      bool
		want        string
	}{
		{"frozen counter healed by ground truth", "", 3, true, "3"},
		{"zero counter healed by ground truth", "0", 5, true, "5"},
		{"higher counter wins (pruned beads)", "40", 2, true, "40"},
		{"equal is stable", "3", 3, true, "3"},
		{"no ground truth keeps counter", "7", 0, false, "7"},
		{"all zero preserves empty string", "", 0, true, ""},
		{"all zero preserves literal zero", "0", 0, true, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconcileRunCount(tc.counter, tc.groundTruth, tc.haveGT); got != tc.want {
				t.Errorf("reconcileRunCount(%q, %d, %v) = %q, want %q",
					tc.counter, tc.groundTruth, tc.haveGT, got, tc.want)
			}
		})
	}
}

// TestGardenerStatus_BoundedVerdictFromBlob is the regression guard for bug
// ka-0000004852: a normally-authored gardener carries its self-block (run_budget
// + hard_stop wall_clock) ONLY inside the nested `gardener` blob, and the flat
// top-level fields (run_budget / stop / loop_end / terminal_goal) are null. The
// trigger-reviver survey used to eyeball those null flat fields and false-
// negative "missing self-block" on EVERY bounded gardener. `gb gardener status`
// must instead report bounded=yes by running the engine's own
// ValidateGardenerBlob against the blob.
func TestGardenerStatus_BoundedVerdictFromBlob(t *testing.T) {
	// A complete, bounded blob mirroring what `gb gardener start` persists:
	// positive run_budget + a hard_stop wall_clock of 168h. Flat fields stay
	// absent — exactly the shape the reviver misjudged.
	capBlob := `{"run_budget":20,"max_runs":20,"terminal_goal":"All children closed.",` +
		`"stop":{"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-EPIC1"},` +
		`{"kind":"max_runs"},{"kind":"no_progress_for","runs":3}]},` +
		`"hard_stop":{"any":[{"kind":"consecutive_failures","value":"5"},` +
		`{"kind":"wall_clock","duration":"168h"}]},"on_loop_end":"close"}}`

	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Test-Health Director"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":     "0 13 * * 1-5",
		"gardener": capBlob,
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "backstop:     yes") {
		t.Errorf("bounded gardener must render backstop=yes (bug ka-0000004852)\nfull output:\n%s", out)
	}
	if strings.Contains(out, "backstop:     NO") {
		t.Errorf("bounded gardener must NOT render backstop=NO\nfull output:\n%s", out)
	}
	// Two-axis reconciliation (bug ka-0000000849): the same status output must
	// now also carry the self-block axis. This blob has run_budget +
	// no_progress_for loop_end but NO `progress` spec, so the wedge detector is
	// unbacked — self-block weak (backstop yes, self-block weak is a legitimate,
	// common shape).
	if !strings.Contains(out, "self-block:   ") {
		t.Errorf("status must render a self-block axis line (bug ka-0000000849)\nfull output:\n%s", out)
	}
}

// TestGardenerStatus_UnboundedVerdictWhenBlobMissing confirms the verdict is a
// TRUE reflection of the guard, not a rubber stamp: a gardener with no `gardener`
// blob (or an unbounded one) is reported bounded=NO with the guard's reason, so
// the surveyor can act on a genuine miss.
func TestGardenerStatus_UnboundedVerdictWhenBlobMissing(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Unbounded legacy gardener"))
	// role:gardener via label but NO gardener blob — the pre-guard legacy shape.
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron": "0 13 * * 1-5",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "backstop:     NO") {
		t.Errorf("gardener with no blob must render backstop=NO\nfull output:\n%s", out)
	}
	// A gardener with no blob has no self-block either (bug ka-0000000849).
	if !strings.Contains(out, "self-block:   NONE") {
		t.Errorf("gardener with no blob must render self-block=NONE\nfull output:\n%s", out)
	}
}

// makeGardenerTriggerCreatedAt is makeGardenerTrigger with a stamped created_at,
// so the wall-clock expiry readout (ka-0000007024) has a real baseline to
// measure from. The stub encodes the bead map directly, and beadJSON parses the
// `created_at` string into BeadDetail.CreatedAt.
func makeGardenerTriggerCreatedAt(id, epicID, createdAt string, fields map[string]any) map[string]any {
	b := makeGardenerTrigger(id, epicID, fields)
	b["created_at"] = createdAt
	return b
}

// boundedGardenerBlob is a minimal bounded gardener blob with a single hard_stop
// wall_clock of the given duration (positive run_budget so the bounds verdict
// is "yes"), matching what `gb gardener start` persists.
func boundedGardenerBlob(wallClock string) string {
	return `{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"` + wallClock + `"}]}}}`
}

// TestGardenerWallClockExpiryStatus_Unit exercises the expiry computation with an
// injected `now`, so every case is deterministic regardless of the real clock.
// It is the crux of ka-0000007024: a pre-epoch gardener measures its wall_clock
// from the grandfather epoch, so a duration that is live for a fresh gardener has
// already elapsed for the grandfathered one — yet the bounds verdict still says
// "bounded". The status readout must report that expiry, floored to the epoch.
func TestGardenerWallClockExpiryStatus_Unit(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	scheduler.SetWallClockGrandfatherEpochForTest(t, epoch)

	preEpoch := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC) // the bug's ka-0000005191 date
	postEpoch := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	t.Run("pre_epoch_168h_is_expired_and_grandfathered", func(t *testing.T) {
		sched := &beadsapi.BeadDetail{
			ID:        "gd-cap",
			CreatedAt: preEpoch,
			Fields:    map[string]string{"gardener": boundedGardenerBlob("168h")},
		}
		// "now" well after the floored+staggered expiry. The baseline is
		// epoch+stagger(id,168h) and the ceiling is that +168h — the SAME instant
		// the engine fires (ka-0000001868), NOT the shared unstaggered epoch+168h.
		wantBaseline := scheduler.GardenerWallClockBaseline(sched.ID, preEpoch, 168*time.Hour)
		wantExpiry := wantBaseline.Add(168 * time.Hour)
		now := wantExpiry.Add(24 * time.Hour) // a day past the real expiry
		v := gardenerWallClockExpiryStatus(sched, now)
		if !v.present {
			t.Fatal("expected a present expiry readout for a bounded gardener with a hard_stop wall_clock")
		}
		if !v.grandfathered {
			t.Error("pre-epoch gardener must be flagged grandfathered (baseline floored to epoch)")
		}
		if !v.expired {
			t.Errorf("real expiry %v is before now %v — must be flagged expired", wantExpiry, now)
		}
		if !v.expiry.Equal(wantExpiry) {
			t.Errorf("expiry = %v, want %v (epoch+stagger+window — the engine's real close)", v.expiry, wantExpiry)
		}
		if !v.baseline.Equal(wantBaseline) {
			t.Errorf("baseline = %v, want epoch+stagger %v", v.baseline, wantBaseline)
		}
	})

	t.Run("post_epoch_measures_from_created_at", func(t *testing.T) {
		sched := &beadsapi.BeadDetail{
			ID:        "gd-cap",
			CreatedAt: postEpoch,
			Fields:    map[string]string{"gardener": boundedGardenerBlob("168h")},
		}
		now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC) // before postEpoch+168h
		v := gardenerWallClockExpiryStatus(sched, now)
		if !v.present || v.grandfathered || v.expired {
			t.Fatalf("post-epoch live gardener: present=%v grandfathered=%v expired=%v (want true,false,false)", v.present, v.grandfathered, v.expired)
		}
		if want := postEpoch.Add(168 * time.Hour); !v.expiry.Equal(want) {
			t.Errorf("expiry = %v, want created_at+168h %v", v.expiry, want)
		}
	})

	t.Run("largest_hard_stop_wall_clock_is_the_ceiling", func(t *testing.T) {
		blob := `{"run_budget":20,"stop":{"hard_stop":{"any":[` +
			`{"kind":"wall_clock","duration":"168h"},{"kind":"wall_clock","duration":"720h"}]}}}`
		sched := &beadsapi.BeadDetail{
			CreatedAt: postEpoch,
			Fields:    map[string]string{"gardener": blob},
		}
		v := gardenerWallClockExpiryStatus(sched, postEpoch)
		if !v.present || v.duration != 720*time.Hour {
			t.Fatalf("effective ceiling = %v, want 720h (the largest hard_stop wall_clock)", v.duration)
		}
	})

	t.Run("no_readout_without_created_at", func(t *testing.T) {
		sched := &beadsapi.BeadDetail{Fields: map[string]string{"gardener": boundedGardenerBlob("168h")}}
		if v := gardenerWallClockExpiryStatus(sched, time.Now()); v.present {
			t.Error("a gardener with no created_at must yield no expiry readout (wall_clock not enforceable)")
		}
	})

	t.Run("no_readout_without_hard_stop_wall_clock", func(t *testing.T) {
		// A loop_end wall_clock is NOT a hard ceiling — must be ignored.
		blob := `{"run_budget":20,"stop":{"loop_end":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`
		sched := &beadsapi.BeadDetail{CreatedAt: postEpoch, Fields: map[string]string{"gardener": blob}}
		if v := gardenerWallClockExpiryStatus(sched, time.Now()); v.present {
			t.Error("a loop_end (non-hard_stop) wall_clock must not produce a hard-ceiling expiry readout")
		}
	})
}

// TestGardenerStatus_WallClockExpiredBanner drives the full `gb gardener status`
// render and asserts the loud EXPIRED banner appears for a bounded-but-expired
// grandfathered gardener — the exact revive footgun of ka-0000007024: bounded=yes
// yet an already-elapsed ceiling. The epoch is pinned to a FAR-past instant so
// epoch+168h is before the real clock regardless of when the test runs.
func TestGardenerStatus_WallClockExpiredBanner(t *testing.T) {
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler.SetWallClockGrandfatherEpochForTest(t, epoch)

	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Long-dark gardener up for revival"))
	stub.addBead(makeGardenerTriggerCreatedAt("gd-CAP1", "gd-EPIC1", "2019-06-01T00:00:00Z", map[string]any{
		"cron":     "0 13 * * 1-5",
		"gardener": boundedGardenerBlob("168h"),
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	// backstop is still yes — the whole point is the guard passes yet the window is past.
	if !strings.Contains(out, "backstop:     yes") {
		t.Errorf("expected backstop=yes (the guard passes the parse check)\n%s", out)
	}
	if !strings.Contains(out, "wall_clock:   ⚠️  EXPIRED") {
		t.Errorf("expected the EXPIRED wall_clock banner\n%s", out)
	}
	if !strings.Contains(out, "ka-0000007024") {
		t.Errorf("expired banner should cite the bug id\n%s", out)
	}
	if !strings.Contains(out, "grandfather epoch") {
		t.Errorf("expired banner should note the epoch floor for a pre-epoch gardener\n%s", out)
	}
}

// TestGardenerStatus_WallClockExpiryJSON asserts the --json readout carries the
// structured wall_clock expiry object (ka-0000007024) so surveyors/automation
// (the trigger-reviver) can gate revival on `expired` without scraping text.
func TestGardenerStatus_WallClockExpiryJSON(t *testing.T) {
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler.SetWallClockGrandfatherEpochForTest(t, epoch)

	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "JSON expiry readout"))
	stub.addBead(makeGardenerTriggerCreatedAt("gd-CAP1", "gd-EPIC1", "2019-06-01T00:00:00Z", map[string]any{
		"cron":     "0 13 * * 1-5",
		"gardener": boundedGardenerBlob("168h"),
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	t.Cleanup(func() { resetGardenerFlags(t) })

	out := captureStdout(t, func() error {
		cmd := gardenerStatusCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-EPIC1"})
	})
	var payload struct {
		Bounded   bool `json:"bounded"`
		WallClock *struct {
			Duration      string `json:"duration"`
			Baseline      string `json:"baseline"`
			Expiry        string `json:"expiry"`
			Grandfathered bool   `json:"grandfathered"`
			Expired       bool   `json:"expired"`
		} `json:"wall_clock"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}
	if !payload.Bounded {
		t.Error("expected bounded=true in JSON (the guard passes the parse check)")
	}
	if payload.WallClock == nil {
		t.Fatalf("expected a wall_clock object in JSON\n%s", out)
	}
	if !payload.WallClock.Expired {
		t.Error("wall_clock.expired must be true for the epoch+168h < now case")
	}
	if !payload.WallClock.Grandfathered {
		t.Error("wall_clock.grandfathered must be true for a pre-epoch gardener")
	}
	// The expiry is the floored+staggered baseline + 168h — the engine's real
	// close instant (ka-0000001868), NOT the shared unstaggered epoch+168h. The
	// stagger seeds off the resolved gardener trigger bead id (gd-CAP1), the same
	// id the engine hashes. Derive it from the SAME pre-epoch created_at the
	// stub carries (2019-06-01) so the floor actually applies.
	createdAt := time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)
	wantExpiry := scheduler.GardenerWallClockBaseline("gd-CAP1", createdAt, 168*time.Hour).Add(168 * time.Hour).Format(time.RFC3339)
	if payload.WallClock.Expiry != wantExpiry {
		t.Errorf("wall_clock.expiry = %q, want %q (epoch+stagger+168h)", payload.WallClock.Expiry, wantExpiry)
	}
}

// TestGardenerBoundedVerdict_MatchesEngineGuard is the load-bearing invariant of
// the fix: the status verdict and the engine's fire-time guard MUST agree on
// every input, because both call the same bridge.ValidateGardenerBlob. If they
// could diverge, the reviver/status could green-light a gardener the engine
// rejects (or vice-versa) — the exact class of disagreement the bug is about
// (ka-0000004852).
func TestGardenerBoundedVerdict_MatchesEngineGuard(t *testing.T) {
	cases := map[string]string{
		"bounded":            `{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
		"missing run_budget": `{"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
		"missing wall_clock": `{"run_budget":20}`,
		"unbounded wall":     `{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"100000h"}]}}}`,
		"empty blob":         "",
		"absent field":       "\x00none", // sentinel: no gardener field set at all
	}
	for name, blob := range cases {
		t.Run(name, func(t *testing.T) {
			sched := &beadsapi.BeadDetail{Fields: map[string]string{}}
			if blob != "\x00none" {
				sched.Fields["gardener"] = blob
			}
			gotBounded, _ := gardenerBoundedVerdict(sched)

			// The engine's fire-time answer for the same bytes.
			var engineBlob string
			if blob != "\x00none" {
				engineBlob = blob
			}
			wantBounded := bridge.ValidateGardenerBlob(engineBlob) == nil

			if gotBounded != wantBounded {
				t.Errorf("verdict/engine disagree for %q: status bounded=%v, engine bounded=%v",
					name, gotBounded, wantBounded)
			}
		})
	}
}

// ── Flat-vs-blob self-block divergence (bug ka-0000004562) ────────────────

// TestGardenerSelfBlockDivergences_NormalGardenerIsClean is the load-bearing
// false-positive guard: a gardener authored the normal way (all flat self-block
// fields absent, the self-block living ONLY in the nested `gardener` blob, as
// `gb gardener start` persists it) must report ZERO divergences. If this ever
// fires, every healthy gardener would grow a spurious warning banner.
func TestGardenerSelfBlockDivergences_NormalGardenerIsClean(t *testing.T) {
	capBlob := `{"run_budget":20,"max_runs":20,"terminal_goal":"All children closed.",` +
		`"stop":{"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-EPIC1"}]},` +
		`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`
	sched := &beadsapi.BeadDetail{Fields: map[string]string{"gardener": capBlob}}
	if got := gardenerSelfBlockDivergences(sched); len(got) != 0 {
		t.Errorf("normal gardener must have no divergences, got %+v", got)
	}
}

// TestGardenerSelfBlockDivergences_NoBlobIsClean covers a legacy/bare gardener
// with no `gardener` blob at all: there is no second copy to disagree with, so
// no divergence is reported (the bounded verdict already flags it unbounded).
func TestGardenerSelfBlockDivergences_NoBlobIsClean(t *testing.T) {
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"terminal_goal": "Some goal set only on the flat field.",
		"run_budget":    "42",
	}}
	if got := gardenerSelfBlockDivergences(sched); len(got) != 0 {
		t.Errorf("gardener with no blob must have no divergences, got %+v", got)
	}
}

// TestGardenerSelfBlockDivergences_FlatEqualsBlobIsClean covers the users-camp
// post-workaround shape: the flat terminal_goal was written but then MADE to
// match the blob (the operator patched the blob to the same text). Equal values
// — even across the quoted-in-blob vs bare-in-flat encoding — must NOT diverge.
func TestGardenerSelfBlockDivergences_FlatEqualsBlobIsClean(t *testing.T) {
	goal := "Every dimension issues[] empty AND zero open kind:users-camp bugs."
	capBlob := `{"run_budget":86,"terminal_goal":` + strconv.Quote(goal) + `}`
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener":      capBlob,
		"terminal_goal": goal, // stored bare (unquoted) in the flat field
		"run_budget":    "86",
	}}
	if got := gardenerSelfBlockDivergences(sched); len(got) != 0 {
		t.Errorf("flat==blob must have no divergences, got %+v", got)
	}
}

// TestGardenerSelfBlockDivergences_DetectsTerminalGoalDrift is the exact bug: a
// tighter terminal_goal was written to the inert flat field while the blob kept
// the loose bar. The divergence must be reported with both values.
func TestGardenerSelfBlockDivergences_DetectsTerminalGoalDrift(t *testing.T) {
	capBlob := `{"run_budget":20,"terminal_goal":"scores >= 8 (loose bar)"}`
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener":      capBlob,
		"terminal_goal": "scores >= 8 AND every issues[] empty (tightened)",
	}}
	got := gardenerSelfBlockDivergences(sched)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 divergence, got %d: %+v", len(got), got)
	}
	if got[0].Field != "terminal_goal" {
		t.Errorf("divergence field = %q, want terminal_goal", got[0].Field)
	}
	if !strings.Contains(got[0].Flat, "tightened") {
		t.Errorf("Flat value should carry the tightened text, got %q", got[0].Flat)
	}
	if !strings.Contains(got[0].Blob, "loose bar") {
		t.Errorf("Blob value should carry the loose bar the engine reads, got %q", got[0].Blob)
	}
}

// TestGardenerSelfBlockDivergences_DetectsRunBudgetDrift covers the users-camp
// self-contradiction: flat run_budget=60, blob run_budget=86. The number vs
// string encoding must not mask a genuine value mismatch.
func TestGardenerSelfBlockDivergences_DetectsRunBudgetDrift(t *testing.T) {
	capBlob := `{"run_budget":86,"terminal_goal":"g"}`
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener":   capBlob,
		"run_budget": "60",
	}}
	got := gardenerSelfBlockDivergences(sched)
	if len(got) != 1 || got[0].Field != "run_budget" {
		t.Fatalf("expected 1 run_budget divergence, got %+v", got)
	}
	if got[0].Flat != "60" || got[0].Blob != "86" {
		t.Errorf("run_budget divergence flat=%q blob=%q, want 60/86", got[0].Flat, got[0].Blob)
	}
}

// TestGardenerSelfBlockDivergences_RunBudgetNumericEqualIsClean pins that a flat
// integer field equal to the blob value (60 vs "60") is normalized to equal and
// NOT flagged — the canonicalization must see through the number/string split.
func TestGardenerSelfBlockDivergences_RunBudgetNumericEqualIsClean(t *testing.T) {
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener":   `{"run_budget":60}`,
		"run_budget": "60",
	}}
	if got := gardenerSelfBlockDivergences(sched); len(got) != 0 {
		t.Errorf("run_budget 60==60 must be clean, got %+v", got)
	}
}

// TestGardenerSelfBlockDivergences_StopKeyOrderIsClean pins that the structured
// stop field compares by JSON VALUE, so a cosmetic key-order/whitespace
// difference between the flat and blob copies is not a false divergence.
func TestGardenerSelfBlockDivergences_StopKeyOrderIsClean(t *testing.T) {
	blobStop := `{"loop_end":{"any":[{"kind":"max_runs"}]},"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`
	// Same value, keys reordered + whitespace added.
	flatStop := `{ "hard_stop": {"any":[{"duration":"168h","kind":"wall_clock"}]}, "loop_end": {"any":[{"kind":"max_runs"}]} }`
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener": `{"run_budget":20,"stop":` + blobStop + `}`,
		"stop":     flatStop,
	}}
	if got := gardenerSelfBlockDivergences(sched); len(got) != 0 {
		t.Errorf("reordered-but-equal stop must be clean, got %+v", got)
	}
}

// TestGardenerSelfBlockDivergences_LoopEndNested pins the one nested path
// (stop.loop_end): the flat loop_end field is compared against the loop_end key
// INSIDE the blob's stop object, and a genuine mismatch is reported.
func TestGardenerSelfBlockDivergences_LoopEndNested(t *testing.T) {
	sched := &beadsapi.BeadDetail{Fields: map[string]string{
		"gardener": `{"run_budget":20,"stop":{"loop_end":{"any":[{"kind":"max_runs"}]}}}`,
		"loop_end": `{"any":[{"kind":"no_progress_for","runs":3}]}`,
	}}
	got := gardenerSelfBlockDivergences(sched)
	if len(got) != 1 || got[0].Field != "loop_end" {
		t.Fatalf("expected 1 loop_end divergence, got %+v", got)
	}
}

// TestGardenerStatus_DivergenceBannerAndJSON drives the full command surface:
// the human output prints a ⚠️ DIVERGENCE banner and --json carries a
// field_divergences array, so surveyors (the trigger-reviver reads --json) and
// operators both see the silent-no-op footgun.
func TestGardenerStatus_DivergenceBannerAndJSON(t *testing.T) {
	capBlob := `{"run_budget":86,"terminal_goal":"loose bar",` +
		`"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "users-camp"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":          "0 13 * * 1-5",
		"gardener":      capBlob,
		"terminal_goal": "TIGHT bar every issues[] empty",
		"run_budget":    "60",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	// Human output.
	func() {
		cmd := gardenerStatusCmd
		cmd.SetContext(context.Background())
		var buf strings.Builder
		cmd.SetOut(&buf)
		if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
			t.Fatalf("RunE (human): %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "DIVERGENCE terminal_goal") {
			t.Errorf("human output missing terminal_goal divergence banner\n%s", out)
		}
		if !strings.Contains(out, "DIVERGENCE run_budget") {
			t.Errorf("human output missing run_budget divergence banner\n%s", out)
		}
		if !strings.Contains(out, "ka-0000004562") {
			t.Errorf("human output should cite the bug id in the remediation hint\n%s", out)
		}
	}()

	// JSON output.
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	out := captureStdout(t, func() error {
		cmd := gardenerStatusCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-EPIC1"})
	})
	var payload struct {
		FieldDivergences []gardenerFieldDivergence `json:"field_divergences"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}
	if len(payload.FieldDivergences) != 2 {
		t.Fatalf("expected 2 field_divergences in json, got %d: %+v", len(payload.FieldDivergences), payload.FieldDivergences)
	}
}

func TestGardenerStatus_AcceptsScheduleIDDirectly(t *testing.T) {
	// status <schedule-id> should also work — useful for `gb gardener status
	// <schedule-id>` from a gardener agent that knows its own bead id.
	stub := newGardenerStub()
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(buf.String(), "Gardener schedule gd-CAP1") {
		t.Errorf("status by schedule-id failed:\n%s", buf.String())
	}
}

// TestBeadIsGardener_RoleLabelAndBlob covers ka-0000005048: a gardener trigger
// whose `role` field names its *spawn-role* (not "gardener") must still be
// recognized as a gardener via the role:gardener label or a non-empty gardener
// blob, so `gb gardener status|stop` can resolve it.
func TestBeadIsGardener_RoleLabelAndBlob(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{
			name: "role field == gardener",
			bead: &beadsapi.BeadDetail{Type: "trigger", Fields: map[string]string{"role": "gardener"}},
			want: true,
		},
		{
			name: "spawn-role field + role:gardener label",
			bead: &beadsapi.BeadDetail{
				Type:   "trigger",
				Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-EPIC1"},
				Fields: map[string]string{"role": "auto-inspector"},
			},
			want: true,
		},
		{
			name: "spawn-role field + gardener blob, no label",
			bead: &beadsapi.BeadDetail{
				Type:   "trigger",
				Fields: map[string]string{"role": "auto-inspector", "gardener": `{"prompt":"x"}`},
			},
			want: true,
		},
		{
			name: "plain scheduled job — no gardener markers",
			bead: &beadsapi.BeadDetail{
				Type:   "schedule",
				Labels: []string{"trigger:schedule"},
				Fields: map[string]string{"role": "crew"},
			},
			want: false,
		},
		{
			name: "nil bead",
			bead: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := beadIsGardener(tc.bead); got != tc.want {
				t.Errorf("beadIsGardener() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBeadIsGardenerForEpic_LabelResolvesSpawnRoleGardener covers the resolver
// path used by `gb gardener status|stop <epic>`: a gardener pinned via the
// gardener-epic:<id> label resolves even when its role field is the spawn-role.
func TestBeadIsGardenerForEpic_LabelResolvesSpawnRoleGardener(t *testing.T) {
	cap := &beadsapi.BeadDetail{
		Type:   "trigger",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-EPIC1"},
		Fields: map[string]string{"role": "auto-inspector", "gardener": `{"prompt":"x"}`},
	}
	if !beadIsGardenerForEpic(cap, "gd-EPIC1") {
		t.Error("spawn-role gardener pinned by gardener-epic label should resolve for its epic")
	}
	if beadIsGardenerForEpic(cap, "gd-OTHER") {
		t.Error("gardener must not resolve for an epic it is not pinned to")
	}
	// Legacy assign_bead path still works with a spawn-role field.
	legacy := &beadsapi.BeadDetail{
		Type:   "schedule",
		Labels: []string{"role:gardener"},
		Fields: map[string]string{"role": "auto-inspector", "assign_bead": "gd-EPIC2", "gardener": `{"prompt":"y"}`},
	}
	if !beadIsGardenerForEpic(legacy, "gd-EPIC2") {
		t.Error("legacy assign_bead gardener with spawn-role should resolve")
	}
}

// TestGardenerStatus_FireErrorSurfaced pins the fire-error diagnostic seam (bug
// ka-0000002991): a gardener carrying last_fire_error must render it prominently
// in the text view and expose it (plus last_fire_error_at and the
// consecutive_failures streak the fire error bumps) in --json. This is the
// command an operator reaches for first, and it was previously silent on
// exactly the hard-fireAction-failure mode that had two agents mis-diagnose a
// dead gardener as a silent decline (ka-0000004529).
func TestGardenerStatus_FireErrorSurfaced(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "trigger engine hardening"))
	stub.addBead(makeGardenerSchedule("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":                 "*/10 * * * *",
		"last_fire_error":      "spawn_agent: HTTP 400: operator could not be resolved",
		"last_fire_error_at":   "2026-08-14T06:00:00Z",
		"consecutive_failures": "4",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	// (1) Text render.
	cmd := gardenerStatusCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE (text): %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"last_fire_error:",
		"operator could not be resolved",
		"2026-08-14T06:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text status output missing %q\nfull output:\n%s", want, out)
		}
	}

	// (2) JSON projection.
	resetGardenerFlags(t)
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON; resetGardenerFlags(t) })
	cmd.SetContext(context.Background())
	rawJSON := captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-EPIC1"}) })
	var got map[string]any
	if err := json.Unmarshal(rawJSON, &got); err != nil {
		t.Fatalf("unmarshal status JSON: %v\nraw:\n%s", err, rawJSON)
	}
	if fe, _ := got["last_fire_error"].(string); !strings.Contains(fe, "operator could not be resolved") {
		t.Errorf("json last_fire_error = %q, want it to contain the spawn error", fe)
	}
	if at, _ := got["last_fire_error_at"].(string); at != "2026-08-14T06:00:00Z" {
		t.Errorf("json last_fire_error_at = %q, want the stamped time", at)
	}
	if cf, _ := got["consecutive_failures"].(string); cf != "4" {
		t.Errorf("json consecutive_failures = %q, want \"4\"", cf)
	}
}

// ── --wake-on-mr / --stop-on-mr-merged (task ka-0000005598) ──────────────────

const testWakeMRURL = "https://gitlab.com/acme/app/-/merge_requests/42"

// runGardenerStartForMRTest is the shared harness for the MR-flag tests: seed an
// epic, reset flags, set the requested MR flags, run the create, and return the
// single captured CreateBead request.
func runGardenerStartForMRTest(t *testing.T, wakeOnMR, stopOnMRMerged string) map[string]any {
	t.Helper()
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Deliver MR", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = ""
	t.Cleanup(func() { operatedBy = oldOperated })

	if wakeOnMR != "" {
		if err := gardenerStartCmd.Flags().Set("wake-on-mr", wakeOnMR); err != nil {
			t.Fatalf("set wake-on-mr: %v", err)
		}
	}
	if stopOnMRMerged != "" {
		if err := gardenerStartCmd.Flags().Set("stop-on-mr-merged", stopOnMRMerged); err != nil {
			t.Fatalf("set stop-on-mr-merged: %v", err)
		}
	}

	cmd := gardenerStartCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	return stub.creates[0]
}

// TestGardenerStart_WakeOnMR verifies --wake-on-mr flips the gardener to event
// mode (trigger:multi + triggers[]) with a bead_event entry scoped to the exact
// MR's mirror bead plus a fallback cron floor — the §8.2 hybrid recipe.
func TestGardenerStart_WakeOnMR(t *testing.T) {
	c := runGardenerStartForMRTest(t, testWakeMRURL, "")
	fields, _ := c["fields"].(map[string]any)

	// Event mode: the trigger:multi label (not trigger:schedule) is what makes
	// the MultiTriggerSource load the triggers[] array. A trigger:schedule bead
	// silently drops triggers[] (ka-0000002562).
	labels, _ := c["labels"].([]any)
	hasMulti, hasSchedule := false, false
	for _, l := range labels {
		switch s, _ := l.(string); s {
		case "trigger:multi":
			hasMulti = true
		case "trigger:schedule":
			hasSchedule = true
		}
	}
	if !hasMulti {
		t.Errorf("wake-on-mr gardener missing trigger:multi label (labels=%v)", labels)
	}
	if hasSchedule {
		t.Errorf("wake-on-mr gardener must NOT carry trigger:schedule (its triggers[] would be dropped)")
	}

	// triggers[] carries the bead_event MR wake + a fallback cron.
	trigRaw, _ := fields["triggers"].(string)
	if strings.TrimSpace(trigRaw) == "" {
		t.Fatal("fields.triggers is empty — the MR event wake was not wired")
	}
	entries, err := scheduler.ParseTriggers(trigRaw)
	if err != nil {
		t.Fatalf("triggers[] is not valid: %v\n%s", err, trigRaw)
	}
	var beadEvent, cron *scheduler.TriggerEntry
	for i := range entries {
		switch entries[i].Kind {
		case scheduler.TriggerKindBeadEvent:
			beadEvent = &entries[i]
		case scheduler.TriggerKindCron:
			cron = &entries[i]
		}
	}
	if beadEvent == nil {
		t.Fatalf("triggers[] missing a bead_event entry (have %+v)", entries)
	}
	if cron == nil {
		t.Fatalf("triggers[] missing the fallback cron floor (have %+v)", entries)
	}

	// The bead_event fires on created/updated/closed so a new commit, comment,
	// status change, OR merge (all restamp the mirror bead) wakes the gardener.
	wantEvents := map[string]bool{"created": false, "updated": false, "closed": false}
	for _, e := range beadEvent.Events {
		if _, ok := wantEvents[e]; ok {
			wantEvents[e] = true
		}
	}
	for e, seen := range wantEvents {
		if !seen {
			t.Errorf("bead_event.events missing %q (have %v)", e, beadEvent.Events)
		}
	}

	// The match must scope to the EXACT MR: type==merge-request AND mr_url==URL.
	if beadEvent.Match == nil {
		t.Fatal("bead_event.match is nil — the wake would fire on every bead event fleet-wide")
	}
	foundType, foundURL := false, false
	for _, leaf := range beadEvent.Match.All {
		switch leaf.Field {
		case "type":
			if leaf.Op == "==" && leaf.Value == "merge-request" {
				foundType = true
			}
		case "mr_url":
			if leaf.Op == "==" && leaf.Value == testWakeMRURL {
				foundURL = true
			}
		}
	}
	if !foundType {
		t.Errorf("bead_event.match missing type==merge-request leaf (match=%+v)", beadEvent.Match)
	}
	if !foundURL {
		t.Errorf("bead_event.match missing mr_url==%q leaf (match=%+v)", testWakeMRURL, beadEvent.Match)
	}

	// The fallback cron carries the open-children preflight so an idle floor tick
	// is suppressed once the epic drains.
	if cron.Preflight == nil {
		t.Error("fallback cron missing the open-children preflight — it would fire forever")
	} else if cron.Preflight.Parent != "gd-EPIC1" {
		t.Errorf("fallback cron preflight parent = %q, want gd-EPIC1", cron.Preflight.Parent)
	}
}

// TestGardenerStart_StopOnMRMerged verifies --stop-on-mr-merged adds a terminal
// specific_mr_merged loop_end entry scoped to the exact MR URL.
func TestGardenerStart_StopOnMRMerged(t *testing.T) {
	c := runGardenerStartForMRTest(t, "", testWakeMRURL)
	blob := gardenerBlobFromCreate(t, c)
	if blob == nil {
		t.Fatal("gardener blob absent")
	}
	stop, ok := blob["stop"].(map[string]any)
	if !ok {
		t.Fatalf("gardener.stop is not an object: %#v", blob["stop"])
	}
	loopEnd, ok := stop["loop_end"].(map[string]any)
	if !ok {
		t.Fatalf("gardener.stop.loop_end is not an object: %#v", stop["loop_end"])
	}
	anyArr, _ := loopEnd["any"].([]any)
	found := false
	for _, e := range anyArr {
		m, _ := e.(map[string]any)
		if m["kind"] == "specific_mr_merged" {
			found = true
			if got, _ := m["mr_url"].(string); got != testWakeMRURL {
				t.Errorf("specific_mr_merged mr_url = %q, want %q", got, testWakeMRURL)
			}
		}
	}
	if !found {
		t.Errorf("gardener.stop.loop_end.any missing a specific_mr_merged entry (have %v)", anyArr)
	}
	// The default self-block entries must remain — the MR stop is ADDED, not a
	// replacement.
	assertGardenerSelfBlock(t, blob, "gd-EPIC1")
}

// TestGardenerStart_MRFlagsOptional confirms that with neither flag set the
// gardener stays in the default trigger:schedule shape with no triggers[] and no
// specific_mr_merged entry — the flags are strictly opt-in.
func TestGardenerStart_MRFlagsOptional(t *testing.T) {
	c := runGardenerStartForMRTest(t, "", "")
	fields, _ := c["fields"].(map[string]any)
	if _, ok := fields["triggers"]; ok {
		t.Error("fields.triggers present without --wake-on-mr — the flag must be opt-in")
	}
	labels, _ := c["labels"].([]any)
	for _, l := range labels {
		if s, _ := l.(string); s == "trigger:multi" {
			t.Error("trigger:multi label present without --wake-on-mr")
		}
	}
	blob := gardenerBlobFromCreate(t, c)
	stop, _ := blob["stop"].(map[string]any)
	loopEnd, _ := stop["loop_end"].(map[string]any)
	anyArr, _ := loopEnd["any"].([]any)
	for _, e := range anyArr {
		m, _ := e.(map[string]any)
		if m["kind"] == "specific_mr_merged" {
			t.Error("specific_mr_merged entry present without --stop-on-mr-merged")
		}
	}
}

// TestGardenerStart_BothMRFlags verifies the two flags compose: wake-on-mr flips
// event mode AND stop-on-mr-merged adds the terminal stop, in one gardener.
func TestGardenerStart_BothMRFlags(t *testing.T) {
	c := runGardenerStartForMRTest(t, testWakeMRURL, testWakeMRURL)

	// Event mode wired.
	fields, _ := c["fields"].(map[string]any)
	if _, ok := fields["triggers"]; !ok {
		t.Error("fields.triggers absent with --wake-on-mr set")
	}
	// Terminal MR stop wired, and the stop spec is still engine-valid.
	blob := gardenerBlobFromCreate(t, c)
	stopRaw, err := json.Marshal(blob["stop"])
	if err != nil {
		t.Fatalf("marshal stop: %v", err)
	}
	if err := scheduler.ValidateStopSpec(string(stopRaw)); err != nil {
		t.Errorf("the composed stop spec is not engine-valid: %v", err)
	}
}

// TestGardenerStart_WakeOnMRFires is the wake-side integration test (task
// ka-0000005598 AC #2 + #6): the bead_event entry --wake-on-mr produces must
// FIRE on an update to the target MR's mirror bead (a new commit / comment /
// pipeline-status flip / merge all restamp it) and must NOT fire on an update to
// a DIFFERENT MR. It drives the CLI-produced triggers[] through the real
// scheduler EntryEngine, so it validates the actual recipe end to end.
func TestGardenerStart_WakeOnMRFires(t *testing.T) {
	c := runGardenerStartForMRTest(t, testWakeMRURL, "")
	fields, _ := c["fields"].(map[string]any)
	trigRaw, _ := fields["triggers"].(string)
	entries, err := scheduler.ParseTriggers(trigRaw)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	var beadEvent scheduler.TriggerEntry
	idx := -1
	for i := range entries {
		if entries[i].Kind == scheduler.TriggerKindBeadEvent {
			beadEvent = entries[i]
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no bead_event entry in the CLI-produced triggers[]")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()
	tctx := scheduler.TriggerEvalContext{TriggerID: "gd-cap-wake", Triggers: entries}

	// An update to the TARGET MR mirror bead fires (payload carries the exact
	// mr_url + type=merge-request, exactly the SSE full-payload shape).
	fireEng := scheduler.NewEntryEngine()
	targetEvent := []scheduler.TriggerEvent{{
		Kind:      scheduler.TriggerKindBeadEvent,
		EventType: "updated",
		BeadID:    "gd-mr-target",
		Payload: map[string]any{
			"type":               "merge-request",
			"mr_url":             testWakeMRURL,
			"mr_pipeline_status": "success",
		},
		Time: now,
	}}
	if !scheduler.EvaluateEventEntry(context.Background(), fireEng, nil, logger, tctx, beadEvent, idx, targetEvent, 0, now).Fire {
		t.Error("wake-on-mr bead_event must FIRE on an update to the target MR")
	}

	// An update to a DIFFERENT MR must NOT fire (the match scopes to one URL).
	noFireEng := scheduler.NewEntryEngine()
	otherEvent := []scheduler.TriggerEvent{{
		Kind:      scheduler.TriggerKindBeadEvent,
		EventType: "updated",
		BeadID:    "gd-mr-other",
		Payload: map[string]any{
			"type":   "merge-request",
			"mr_url": "https://gitlab.com/acme/app/-/merge_requests/999",
		},
		Time: now,
	}}
	if scheduler.EvaluateEventEntry(context.Background(), noFireEng, nil, logger, tctx, beadEvent, idx, otherEvent, 0, now).Fire {
		t.Error("wake-on-mr bead_event must NOT fire on an update to a different MR")
	}

	// A non-MR bead update must NOT fire (the type leaf guards it).
	nonMREng := scheduler.NewEntryEngine()
	nonMREvent := []scheduler.TriggerEvent{{
		Kind:      scheduler.TriggerKindBeadEvent,
		EventType: "updated",
		BeadID:    "gd-task-1",
		Payload:   map[string]any{"type": "task", "mr_url": testWakeMRURL},
		Time:      now,
	}}
	if scheduler.EvaluateEventEntry(context.Background(), nonMREng, nil, logger, tctx, beadEvent, idx, nonMREvent, 0, now).Fire {
		t.Error("wake-on-mr bead_event must NOT fire on a non-merge-request bead")
	}
}
