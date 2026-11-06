package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Operator BLOCK #3 on PR #426 (gd-is1H5NmI05). The single-field endpoints and
// the close merge skipped updateBead's seal strip and the enable gate, so a
// static token could switch a sealed schedule on (`fields/enabled/edit
// {op:append,element:"true"}`) or zero run_budget and the seal survived.
//
// On a trigger/schedule: `enabled`, every fingerprinted field and the gardener
// blob are writable through the field endpoints only by an OIDC gardenio-admin;
// the run counters (run_budget, runs_spawned and the daily runs_day/runs_today)
// are writable on every path only by an OIDC admin or the trigger-engine
// machine principal.

type autonomousHTTPFixture struct {
	f       *lineageFixture
	handler http.Handler
	engine  string // machine:gardenio-trigger-engine
	canary  string // another machine principal
	admin   string // OIDC gardenio-admin
	member  string // OIDC session without gardenio-admin
}

func newAutonomousHTTPFixture(t *testing.T) *autonomousHTTPFixture {
	t.Helper()
	h := newMachineHarness(t, []string{"gardenio-trigger-engine", "gardenio-authz-canary"})
	f := newLineageFixture(t)
	f.srv.userProvisioner = nil
	f.srv.SetFleetAuth(h.fc)
	f.srv.SetRateLimit(RateLimitConfig{}) // the matrix sends hundreds of writes per identity
	// A sealed trigger with no `enabled` field, and a sealed schedule that is
	// disabled by default (absent `enabled` == off) -- the operator's example.
	f.trigger("gd-trig", nil)
	f.trigger("gd-sched", func(b *model.Bead) { b.Type = "schedule" })
	return &autonomousHTTPFixture{
		f:       f,
		handler: f.srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken)),
		engine:  h.saToken(t, "gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops"),
		canary:  h.saToken(t, "gardenio-authz-canary", "gardenio-operator", "gardenio-machine-trigger-ops"),
		admin:   h.humanToken(t, "gardenio-cli", "admin@example.com", RoleAdmin),
		member:  h.humanToken(t, "gardenio-cli", "member@example.com", RoleOperator),
	}
}

type autonomousWrite struct {
	name   string
	method string
	path   func(id string) string
	body   any
	// counters: the write touches the run counters only (the engine may
	// do it); otherwise only an OIDC admin may.
	counters bool
}

func autonomousWrites() []autonomousWrite {
	bead := func(suffix string) func(string) string {
		return func(id string) string { return "/v1/beads/" + id + suffix }
	}
	post := http.MethodPost
	return []autonomousWrite{
		// enable flag, per endpoint
		{"edit enabled append true", post, bead("/fields/enabled/edit"), editSubFieldRequest{Op: "append", Element: "true"}, false},
		{"edit enabled remove false", post, bead("/fields/enabled/edit"), editSubFieldRequest{Op: "remove", Element: "false"}, false},
		{"append enabled", post, bead("/fields/enabled/append"), appendArrayFieldRequest{Element: "true"}, false},
		{"reorder enabled", post, bead("/fields/enabled/reorder"), casArrayFieldRequest{Expected: []string{"false"}, Next: []string{"true"}}, false},
		{"increment enabled", post, bead("/increment"), map[string]float64{"enabled": 1}, false},
		// fingerprinted fields, per endpoint
		{"edit role", post, bead("/fields/role/edit"), editSubFieldRequest{Op: "append", Element: "captain"}, false},
		{"edit project map", post, bead("/fields/project/edit"), editSubFieldRequest{Op: "set", Key: "x", Value: "y"}, false},
		{"append triggers", post, bead("/fields/triggers/append"), appendArrayFieldRequest{Element: "x"}, false},
		{"reorder chain", post, bead("/fields/chain/reorder"), casArrayFieldRequest{Expected: []string{}, Next: []string{"x"}}, false},
		{"increment max_runs", post, bead("/increment"), map[string]float64{"max_runs": 100}, false},
		{"edit gardener blob", post, bead("/fields/gardener/edit"), editSubFieldRequest{Op: "set", Key: "max_runs", Value: "99"}, false},
		// run counters, per endpoint
		{"increment run_budget", post, bead("/increment"), map[string]float64{"run_budget": -5}, true},
		{"increment runs_spawned", post, bead("/increment"), map[string]float64{"runs_spawned": 1}, true},
		{"increment counters + enabled", post, bead("/increment"), map[string]float64{"runs_spawned": 1, "enabled": 1}, false},
		{"edit run_budget", post, bead("/fields/run_budget/edit"), editSubFieldRequest{Op: "append", Element: "0"}, true},
		{"append runs_spawned", post, bead("/fields/runs_spawned/append"), appendArrayFieldRequest{Element: "0"}, true},
		{"reorder run_budget", post, bead("/fields/run_budget/reorder"), casArrayFieldRequest{Expected: []string{}, Next: []string{"0"}}, true},
		{"close smuggling run_budget", post, bead("/close"), map[string]any{"run_budget": 0}, true},
		{"PATCH run_budget", http.MethodPatch, bead(""), map[string]any{"fields": map[string]any{"run_budget": 0}}, true},
		{"PATCH runs_spawned", http.MethodPatch, bead(""), map[string]any{"fields": map[string]any{"runs_spawned": 0}}, true},
		{"PATCH gardener blob run_budget", http.MethodPatch, bead(""), map[string]any{"fields": map[string]any{"gardener": `{"run_budget":0}`}}, true},
		// the daily cap's counters (a steward's only run bound)
		{"PATCH gardener blob runs_today", http.MethodPatch, bead(""), map[string]any{"fields": map[string]any{"gardener": `{"runs_today":0}`}}, true},
		{"PATCH gardener blob runs_day", http.MethodPatch, bead(""), map[string]any{"fields": map[string]any{"gardener": `{"runs_day":"2000-01-01"}`}}, true},
		{"edit runs_today", post, bead("/fields/runs_today/edit"), editSubFieldRequest{Op: "append", Element: "0"}, true},
	}
}

func (x *autonomousHTTPFixture) do(t *testing.T, tok string, w autonomousWrite, id string) int {
	t.Helper()
	return machineDo(t, x.handler, tok, w.method, w.path(id), w.body).Code
}

// Every field endpoint (and close/PATCH for the counters) refuses a static
// token, another machine principal and a non-admin OIDC session, leaves the
// bead and its seal untouched, and the sealed spawn still qualifies afterwards.
func TestAutonomousFieldWrites_NonAdminDenied(t *testing.T) {
	x := newAutonomousHTTPFixture(t)
	callers := map[string]string{"static": machineStaticToken, "other machine": x.canary, "oidc non-admin": x.member}
	for _, id := range []string{"gd-trig", "gd-sched"} {
		for _, w := range autonomousWrites() {
			for who, tok := range callers {
				t.Run(id+"/"+w.name+"/"+who, func(t *testing.T) {
					before := string(x.f.ms.beads[id].Fields)
					if code := x.do(t, tok, w, id); code != http.StatusForbidden {
						t.Fatalf("status=%d, want 403", code)
					}
					if after := string(x.f.ms.beads[id].Fields); after != before {
						t.Fatalf("fields changed despite 403:\n before %s\n after  %s", before, after)
					}
				})
			}
		}
	}
	if err := x.f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")); err == nil {
		// the trigger was never enabled-toggled and the seal is intact
	} else {
		t.Fatalf("seal must survive refused writes: %v", err)
	}
}

// The trigger-engine machine principal may write the counters on every path but
// nothing else; an OIDC admin may write all of it.
func TestAutonomousFieldWrites_EngineAndAdmin(t *testing.T) {
	x := newAutonomousHTTPFixture(t)
	for _, w := range autonomousWrites() {
		t.Run("engine/"+w.name, func(t *testing.T) {
			code := x.do(t, x.engine, w, "gd-trig")
			if w.counters && code == http.StatusForbidden {
				t.Fatalf("engine counter write refused: status=%d", code)
			}
			if !w.counters && code != http.StatusForbidden {
				t.Fatalf("engine must not write enabled/spawn content via this route: status=%d", code)
			}
		})
		t.Run("admin/"+w.name, func(t *testing.T) {
			if code := x.do(t, x.admin, w, "gd-sched"); code == http.StatusForbidden {
				t.Fatalf("OIDC admin refused: status=%d", code)
			}
		})
	}
}

// The incident: a static token appending "true" to a sealed schedule's absent
// `enabled`, and removing a stored "false", no longer changes anything.
func TestAutonomousFieldWrites_EnableBypassClosed(t *testing.T) {
	x := newAutonomousHTTPFixture(t)
	x.f.ms.beads["gd-sched"].Fields = mustMergeFields(t, x.f.ms.beads["gd-sched"].Fields, map[string]any{"enabled": false})
	enabled := func() bool { return effectiveGardenerEnabled("schedule", x.f.ms.beads["gd-sched"].Fields) }
	for _, w := range []autonomousWrite{
		{"append", http.MethodPost, func(id string) string { return "/v1/beads/" + id + "/fields/enabled/append" }, appendArrayFieldRequest{Element: "true"}, false},
		{"edit", http.MethodPost, func(id string) string { return "/v1/beads/" + id + "/fields/enabled/edit" }, editSubFieldRequest{Op: "append", Element: "true"}, false},
		{"increment", http.MethodPost, func(id string) string { return "/v1/beads/" + id + "/increment" }, map[string]float64{"enabled": 1}, false},
	} {
		if code := x.do(t, machineStaticToken, w, "gd-sched"); code != http.StatusForbidden || enabled() {
			t.Fatalf("%s: status=%d enabled=%v; want 403 and still disabled", w.name, code, enabled())
		}
	}
}

func mustMergeFields(t *testing.T, fields json.RawMessage, kv map[string]any) json.RawMessage {
	t.Helper()
	m := fieldsAsMap(fields)
	for k, v := range kv {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Other bead types and unrelated trigger fields are untouched by the gate.
func TestAutonomousFieldWrites_Unaffected(t *testing.T) {
	x := newAutonomousHTTPFixture(t)
	x.f.ms.beads["gd-task"] = &model.Bead{ID: "gd-task", Title: "t", Type: "task", Kind: "issue", Status: model.StatusOpen,
		Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{}`)}
	x.f.ms.labels["gd-task"] = x.f.ms.beads["gd-task"].Labels
	w := autonomousWrite{"increment run_budget on a task", http.MethodPost, func(id string) string { return "/v1/beads/" + id + "/increment" }, map[string]float64{"run_budget": 1}, true}
	if code := x.do(t, machineStaticToken, w, "gd-task"); code == http.StatusForbidden {
		t.Fatalf("task bead must not be gated, status=%d", code)
	}
	patch := autonomousWrite{"PATCH last_checked", http.MethodPatch, func(id string) string { return "/v1/beads/" + id },
		map[string]any{"fields": map[string]any{"last_checked": "2026-10-15T00:00:00Z"}}, false}
	if code := x.do(t, machineStaticToken, patch, "gd-trig"); code != http.StatusOK {
		t.Fatalf("service stamps on a trigger must keep working, status=%d", code)
	}
	// An unchanged echo of the counters from a read-modify-write client is fine.
	echo := autonomousWrite{"PATCH echo", http.MethodPatch, func(id string) string { return "/v1/beads/" + id },
		map[string]any{"fields": map[string]any{"runs_spawned": 3}}, true}
	if code := x.do(t, x.engine, echo, "gd-trig"); code != http.StatusOK {
		t.Fatalf("engine PATCH runs_spawned: status=%d", code)
	}
	if code := x.do(t, machineStaticToken, echo, "gd-trig"); code != http.StatusOK {
		t.Fatalf("unchanged runs_spawned echo by a non-engine caller must be tolerated, status=%d", code)
	}
}

func TestAutonomousCounterPatchDenial_Unit(t *testing.T) {
	existing := json.RawMessage(`{"runs_spawned":3,"run_budget":8,"gardener":"{\"run_budget\":8,\"runs_spawned\":3,\"max_runs\":5,\"runs_day\":\"2026-10-23\",\"runs_today\":24}"}`)
	for name, tc := range map[string]struct {
		patch string
		deny  bool
	}{
		"unchanged top-level":     {`{"run_budget":8}`, false},
		"unchanged number format": {`{"run_budget":8.0}`, false},
		"changed top-level":       {`{"run_budget":0}`, true},
		"removed top-level":       {`{"run_budget":null}`, true},
		"unchanged blob":          {`{"gardener":"{\"run_budget\":8,\"runs_spawned\":3,\"max_runs\":9,\"runs_day\":\"2026-10-23\",\"runs_today\":24}"}`, false},
		"blob counter changed":    {`{"gardener":"{\"run_budget\":0,\"runs_spawned\":3,\"max_runs\":5}"}`, true},
		"blob counter dropped":    {`{"gardener":"{\"max_runs\":5}"}`, true},
		"blob as object, changed": {`{"gardener":{"run_budget":1,"runs_spawned":3}}`, true},
		"daily count zeroed":      {`{"gardener":"{\"run_budget\":8,\"runs_spawned\":3,\"max_runs\":5,\"runs_day\":\"2026-10-23\",\"runs_today\":0}"}`, true},
		"daily day rolled back":   {`{"gardener":"{\"run_budget\":8,\"runs_spawned\":3,\"max_runs\":5,\"runs_day\":\"2000-01-01\",\"runs_today\":24}"}`, true},
		"top-level runs_today":    {`{"runs_today":0}`, true},
		"unrelated field":         {`{"last_checked":"x"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := autonomousCounterPatchDenial(staticCtx(), "trigger", json.RawMessage(tc.patch), existing)
			if (got != nil) != tc.deny {
				t.Fatalf("denial=%v, want deny=%v", got, tc.deny)
			}
			for who, ctx := range map[string]bool{"admin": true, "engine": false} {
				c := adminOIDCCtx()
				if !ctx {
					c = engineCtx()
				}
				if autonomousCounterPatchDenial(c, "trigger", json.RawMessage(tc.patch), existing) != nil {
					t.Fatalf("%s must be allowed", who)
				}
			}
		})
	}
	if autonomousCounterPatchDenial(staticCtx(), "task", json.RawMessage(`{"run_budget":0}`), existing) != nil {
		t.Fatal("only trigger/schedule beads are gated")
	}
}
