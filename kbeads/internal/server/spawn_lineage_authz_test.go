package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Trigger-lineage spawn authorization (gd-is1H5NmI05, decision gd-wNOUd0MCJo):
// the trigger-engine's spawn of a privileged role for an admin-authored trigger
// is authorized as that admin; every other case escalates exactly as before.

const (
	lineageAdmin = "oidc:admin@example.com"
	lineageHuman = "oidc:viewer@example.com"
)

type lineageFixture struct {
	t   *testing.T
	srv *BeadsServer
	ms  *mockStore
	now time.Time // the server's lineage clock
}

func newLineageFixture(t *testing.T) *lineageFixture {
	ms := newMockStore()
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	makeBypassRoleBead(ms, "gd-role-captain", "captain")
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	// The agents that write on their own per-agent tokens: the write gate
	// looks their role up before a trigger write (restricted_ops.go).
	seedAgent(ms, "gd-crew1", "crew", "gardenio")
	seedAgent(ms, "gd-crew2", "crew", "gardenio")
	seedAgent(ms, approvalThread, "thread", "gardenio")
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	f := &lineageFixture{t: t, srv: srv, ms: ms, now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	srv.lineageClock = func() time.Time { return f.now }
	return f
}

const lineageTriggerFields = `{"action":"spawn_agent","role":"reviewer","project":"gardenio"}`

// trigger creates a trigger the way an operator does: through createBead as an
// OIDC gardenio-admin session, so the server seals it. mutate then edits the
// stored bead directly (as a test of what a later writer could leave behind).
func (f *lineageFixture) trigger(id string, mutate func(*model.Bead)) {
	f.t.Helper()
	f.triggerAs(adminOIDCCtx(), id, mutate)
}

// triggerAs is trigger with an explicit authoring credential.
func (f *lineageFixture) triggerAs(ctx context.Context, id string, mutate func(*model.Bead)) {
	f.t.Helper()
	b, err := f.srv.createBead(ctx, createBeadInput{
		Title: "nightly review", Type: "trigger", Kind: "data",
		Labels: []string{"project:gardenio"}, Fields: json.RawMessage(lineageTriggerFields),
	})
	if err != nil {
		f.t.Fatalf("create trigger: %v", err)
	}
	stored := f.ms.beads[b.ID]
	delete(f.ms.beads, b.ID)
	stored.ID = id
	if mutate != nil {
		mutate(stored)
	}
	f.ms.beads[id] = stored
	f.ms.labels[id] = stored.Labels // mockStore.GetBead serves labels from here
}

// withFields rewrites keys in a stored bead's fields without touching the seal.
func withFields(kv map[string]any) func(*model.Bead) {
	return func(b *model.Bead) {
		m := fieldsAsMap(b.Fields)
		for k, v := range kv {
			m[k] = v
		}
		b.Fields, _ = json.Marshal(m)
	}
}

func spawnFieldsWithTrigger(project, role, triggerID string) json.RawMessage {
	f, _ := json.Marshal(map[string]string{"project": project, "role": role, "agent": "test", "trigger_id": triggerID})
	return f
}

// spawn runs the gate the way createBead does: compute the lineage
// authorization, then the privileged-role gate.
func (f *lineageFixture) spawn(ctx context.Context, fields json.RawMessage) error {
	return authorizeAgentSpawn(f.srv.withTriggerLineageSpawnAuth(ctx, fields), f.ms, fields)
}

func engineCtx() context.Context {
	return machineCtx("gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops")
}

func requireEscalates(t *testing.T, err error) {
	t.Helper()
	var d *AuthzDenial
	if !errors.As(err, &d) || !d.Escalatable || d.SpawnRole == "" {
		t.Fatalf("want an escalatable privileged-spawn denial, got %v", err)
	}
}

func TestSpawnLineage_AdminAuthoredTriggerSpawnsPrivilegedRoleWithoutEscalation(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")); err != nil {
		t.Fatalf("admin-authored trigger spawn must pass the gate, got %v", err)
	}
}

func TestSpawnLineage_ScheduleAuthoredByAdmin(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-sched", func(b *model.Bead) {
		withFields(map[string]any{"enabled": true})(b)
		b.Type, b.Kind = "schedule", model.KindConfig
	})
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-sched")); err != nil {
		t.Fatalf("enabled admin-sealed schedule must pass, got %v", err)
	}
	f.trigger("gd-sched-off", func(b *model.Bead) { // a schedule is disabled unless enabled=true
		b.Type, b.Kind = "schedule", model.KindConfig
	})
	requireEscalates(t, f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-sched-off")))
}

func TestSpawnLineage_EscalatesWhen(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Bead)
		fields func() json.RawMessage
		noBead bool
		as     context.Context // authoring credential; default is an OIDC admin session
	}{
		{name: "closed trigger", mutate: func(b *model.Bead) { b.Status = model.StatusClosed }},
		{name: "disabled trigger", mutate: withFields(map[string]any{"enabled": false})},
		{name: "cross-project trigger", mutate: func(b *model.Bead) { b.Labels = []string{"project:other"} }},
		{name: "trigger with no project label", mutate: func(b *model.Bead) { b.Labels = nil }},
		{name: "trigger whose project field differs from its label", mutate: withFields(map[string]any{"project": "other"})},
		{name: "non-trigger bead", mutate: func(b *model.Bead) { b.Type = "task" }},
		{name: "seal removed", mutate: func(b *model.Bead) {
			m := fieldsAsMap(b.Fields)
			delete(m, spawnLineageField)
			b.Fields, _ = json.Marshal(m)
		}},
		{name: "missing trigger bead", noBead: true},
		{name: "missing trigger_id", fields: func() json.RawMessage { return agentFields("gardenio", "reviewer") }},
		{name: "empty trigger_id", fields: func() json.RawMessage { return spawnFieldsWithTrigger("gardenio", "reviewer", "") }},
		{name: "fields project differs from trigger project", fields: func() json.RawMessage {
			return spawnFieldsWithTrigger("other", "reviewer", "gd-trig")
		}},
		// (d) The engine asks for a privileged role the trigger is not configured to spawn.
		{name: "role mismatch", fields: func() json.RawMessage { return spawnFieldsWithTrigger("gardenio", "captain", "gd-trig") }},
		{name: "role mismatch by case-variant key", fields: func() json.RawMessage {
			f, _ := json.Marshal(map[string]string{"project": "gardenio", "Role": "captain", "role": "reviewer", "trigger_id": "gd-trig"})
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLineageFixture(t)
			if !tc.noBead {
				if tc.as != nil {
					f.triggerAs(tc.as, "gd-trig", tc.mutate)
				} else {
					f.trigger("gd-trig", tc.mutate)
				}
			}
			fields := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
			if tc.fields != nil {
				fields = tc.fields()
			}
			if err := f.spawn(engineCtx(), fields); err == nil {
				t.Fatal("must not pass the privileged-role gate")
			} else if tc.name == "role mismatch by case-variant key" {
				return // refused outright as an input error, never authorized
			}
			requireEscalates(t, f.spawn(engineCtx(), fields))
		})
	}
}

// Operator BLOCK on PR #426, test (a): a static token that relays
// X-Operated-By: oidc:<admin> creates a trigger. operated_by and created_by read
// as the admin, but no OIDC admin session wrote it, so it is unsealed and the
// engine's spawn escalates.
func TestSpawnLineage_StaticTokenWithForgedOperatedByCreatesTrigger(t *testing.T) {
	f := newLineageFixture(t)
	ctx := WithOperatedByHeader(withAuthMethod(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"}),
		authMethodStatic), lineageAdmin)
	f.triggerAs(ctx, "gd-trig", nil)
	if got := f.ms.beads["gd-trig"]; got.OperatedBy != lineageAdmin {
		t.Fatalf("setup: forged operated_by not recorded (got %q), the premise of the bypass is gone", got.OperatedBy)
	}
	requireEscalates(t, f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")))
}

// Test (b): an agent that an admin spawned creates a trigger. Its writes are
// attributed to that admin, but it is not an OIDC session.
func TestSpawnLineage_AgentSpawnedByAdminCreatesTrigger(t *testing.T) {
	f := newLineageFixture(t)
	ctx := WithOperatedByHeader(withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic), lineageAdmin)
	f.triggerAs(ctx, "gd-trig", nil)
	if got := f.ms.beads["gd-trig"].OperatedBy; got != lineageAdmin {
		t.Fatalf("setup: agent write not attributed to the admin (got %q)", got)
	}
	requireEscalates(t, f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")))
}

// Test (c): the static token edits an admin's sealed trigger so it spawns a
// different privileged role. The seal no longer matches, so the lineage is void
// even though operated_by is still the admin and the engine asks for exactly the
// new configured role.
func TestSpawnLineage_StaticTokenEditsAdminTriggerRole(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")); err != nil {
		t.Fatalf("setup: sealed trigger must pass before the edit, got %v", err)
	}
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"captain"}`)}); err != nil {
		t.Fatalf("setup: the static token is expected to be able to edit the trigger: %v", err)
	}
	if got := f.ms.beads["gd-trig"].OperatedBy; got != lineageAdmin {
		t.Fatalf("setup: operated_by=%q should still be the admin", got)
	}
	requireEscalates(t, f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "captain", "gd-trig")))
	// Restoring the old role by hand does not resurrect a seal an admin gave to
	// different content... but it DOES match the sealed digest again.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"reviewer"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")); err != nil {
		t.Fatalf("content identical to what the admin sealed must pass again, got %v", err)
	}
}

// The engine's own bookkeeping (last_checked, run counters) must not void the
// seal, or lineage would silently stop working for every live trigger.
func TestSpawnLineage_EngineBookkeepingKeepsSeal(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	// runs_spawned is a run counter: only the engine principal (or an admin)
	// may write it (operator BLOCK #3).
	for patch, ctx := range map[string]context.Context{
		`{"last_checked":"2026-10-15T00:00:00Z","last_outcome":"fired"}`:               staticCtx(),
		`{"runs_spawned":3,"last_run":"2026-10-15T00:00:00Z","last_agent_id":"gd-a1"}`: engineCtx(),
	} {
		if _, err := f.srv.updateBead(ctx, "gd-trig", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
			t.Fatalf("patch %s: %v", patch, err)
		}
	}
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")); err != nil {
		t.Fatalf("bookkeeping writes must not void the seal, got %v", err)
	}
}

// Operator BLOCK #2, must-fix B. Content equality cannot see "someone turned a
// stopped trigger back on", so a reopen or re-enable by anything but an OIDC
// gardenio-admin session strips the seal. Disabling or closing only removes
// authority and leaves it alone.
func TestSpawnLineage_NonAdminReopenOrReenableVoidsSeal(t *testing.T) {
	req := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
	closed, open := string(model.StatusClosed), string(model.StatusOpen)
	type step struct {
		ctx   context.Context
		patch updateBeadInput
	}
	patch := func(fields string) updateBeadInput { return updateBeadInput{Fields: json.RawMessage(fields)} }
	status := func(st string) updateBeadInput { return updateBeadInput{Status: &st} }
	// A privileged-role agent (after the per-agent-token flip) writes like the
	// static token and gets its attribution from the human who spawned it.
	agent := WithOperatedByHeader(staticCtx(), lineageAdmin)

	cases := []struct {
		name       string
		stop       step // an OIDC admin stops the trigger first
		start      step // then someone starts it again
		wantSealed bool
	}{
		{"static token re-enables a trigger the admin disabled",
			step{adminOIDCCtx(), patch(`{"enabled":false}`)}, step{staticCtx(), patch(`{"enabled":true}`)}, false},
		{"agent re-enables a trigger the admin disabled",
			step{adminOIDCCtx(), patch(`{"enabled":false}`)}, step{agent, patch(`{"enabled":true}`)}, false},
		{"static token reopens a trigger the admin closed",
			step{adminOIDCCtx(), status(closed)}, step{staticCtx(), status(open)}, false},
		{"agent reopens a trigger the admin closed",
			step{adminOIDCCtx(), status(closed)}, step{agent, status(open)}, false},
		{"engine reopens a trigger the admin closed",
			step{adminOIDCCtx(), status(closed)}, step{engineCtx(), status(open)}, false},
		{"admin re-enables their own trigger",
			step{adminOIDCCtx(), patch(`{"enabled":false}`)}, step{adminOIDCCtx(), patch(`{"enabled":true}`)}, true},
		{"admin reopens their own trigger",
			step{adminOIDCCtx(), status(closed)}, step{adminOIDCCtx(), status(open)}, true},
		{"a non-admin disabling keeps the seal (it only removes authority)",
			step{staticCtx(), patch(`{"enabled":false}`)}, step{staticCtx(), patch(`{"last_checked":"x"}`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLineageFixture(t)
			f.trigger("gd-trig", nil)
			for _, st := range []step{tc.stop, tc.start} {
				if _, err := f.srv.updateBead(st.ctx, "gd-trig", st.patch); err != nil {
					t.Fatalf("update: %v", err)
				}
			}
			if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]) != ""; got != tc.wantSealed {
				t.Fatalf("sealed after the sequence = %v, want %v", got, tc.wantSealed)
			}
			// The trigger is open and enabled again in the sealed cases; in the
			// others the seal is gone, so the spawn escalates once it is on.
			bead := f.ms.beads["gd-trig"]
			if bead.Status == model.StatusClosed || !effectiveGardenerEnabled(bead.Type, bead.Fields) {
				return
			}
			err := f.spawn(engineCtx(), req)
			if tc.wantSealed && err != nil {
				t.Fatalf("sealed trigger must spawn, got %v", err)
			}
			if !tc.wantSealed {
				requireEscalates(t, err)
			}
		})
	}
}

// Operator BLOCK #2, must-fix A. The engine rewrites the gardener blob on every
// fire (runs_spawned, run_budget, ...); only the gardener's CONFIGURATION is
// fingerprinted, so a sealed gardener keeps its seal across fires but a change
// to what it does voids it.
func TestSpawnLineage_GardenerSealSurvivesFires(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	cfg := `{"name":"release-owner","terminal_goal":"drive READY beads to merge","max_runs":50,"resume_budget":3,"standing":true,"stop":{"kind":"wall_clock","value":"72h"}}`
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(
		`{"gardener":` + strconv.Quote(cfg) + `}`)}); err != nil {
		t.Fatal(err)
	}
	req := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("setup: sealed gardener must spawn, got %v", err)
	}

	// Three simulated fires, written the way gardenerPostFire does: the whole
	// blob (counters changed) plus the top-level mirrors.
	for fire := 1; fire <= 3; fire++ {
		blob := map[string]any{}
		_ = json.Unmarshal([]byte(cfg), &blob)
		blob["runs_spawned"] = fire
		blob["runs_completed"] = fire - 1
		blob["run_budget"] = 10 + fire
		blob["consecutive_failures"] = fire % 2
		blob["last_agent_id"] = "gd-agent-" + strconv.Itoa(fire)
		blob["lineage_root"] = "gd-agent-1"
		blob["last_resume_reason"] = "max_runs"
		blob["resumes_without_progress"] = fire
		blob["progress_state"] = map[string]any{"runs": []any{map[string]any{"score": fire}}}
		blob["run_log"] = []any{"fired"}
		blob["dormant_on_escalation"] = fire == 2
		raw, _ := json.Marshal(blob)
		patch, _ := json.Marshal(map[string]any{"gardener": string(raw), "runs_spawned": fire, "last_run": "2026-10-15T00:00:00Z"})
		if _, err := f.srv.updateBead(engineCtx(), "gd-trig", updateBeadInput{Fields: patch}); err != nil {
			t.Fatalf("fire %d: %v", fire, err)
		}
		if err := f.spawn(engineCtx(), req); err != nil {
			t.Fatalf("fire %d voided the seal: %v", fire, err)
		}
	}

	// A change to the gardener's configuration voids it.
	for _, change := range []string{`"max_runs":500`, `"terminal_goal":"something else"`, `"standing":false`, `"resume_budget":99`} {
		blob := map[string]any{}
		_ = json.Unmarshal([]byte(cfg), &blob)
		var edit map[string]any
		_ = json.Unmarshal([]byte("{"+change+"}"), &edit)
		for k, v := range edit {
			blob[k] = v
		}
		raw, _ := json.Marshal(blob)
		patch, _ := json.Marshal(map[string]any{"gardener": string(raw)})
		f2 := newLineageFixture(t)
		f2.trigger("gd-trig", nil)
		if _, err := f2.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"gardener":` + strconv.Quote(cfg) + `}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f2.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: patch}); err != nil {
			t.Fatal(err)
		}
		requireEscalates(t, f2.spawn(engineCtx(), req))
	}
}

// Operator BLOCK #2, should-fix: every field that changes which work runs or how
// often is part of the fingerprint.
func TestSpawnLineage_FingerprintCoversWhatAndWhenFields(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"condition_type", `"account_cost"`}, {"field", `"cost"`}, {"op", `">"`}, {"threshold", `5`},
		{"match", `"x"`}, {"cron", `"* * * * *"`}, {"timed_cron", `"0 0 * * *"`},
		{"channel_filter", `"C1"`}, {"account_filter", `"acct"`}, {"trigger_mode", `"event"`},
		{"max_concurrent", `9`}, {"max_concurrent_runs", `9`}, {"cooldown_minutes", `0`}, {"max_runs", `999`},
		{"allowed_roles", `"captain"`}, {"allowed_projects", `"other"`},
		{"prompt", `"do something else"`}, {"formula_vars", `"{}"`}, {"extra_labels", `"x"`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			f := newLineageFixture(t)
			f.trigger("gd-trig", nil)
			if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"` + tc.key + `":` + tc.val + `}`)}); err != nil {
				t.Fatalf("setup: schema refuses %s=%s: %v", tc.key, tc.val, err)
			}
			requireEscalates(t, f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")))
		})
	}
}

// No Keycloak credential: a demoted admin's seals lapse with the TTL, and an
// admin write (or an explicit reseal) renews them.
func TestSpawnLineage_SealExpires(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	req := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")

	f.now = f.now.Add(defaultSpawnLineageSealTTL - time.Hour)
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("a seal inside the TTL must pass, got %v", err)
	}
	f.now = f.now.Add(2 * time.Hour)
	requireEscalates(t, f.spawn(engineCtx(), req))

	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"reseal_spawn_lineage":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("an admin reseal must renew an expired seal, got %v", err)
	}

	f.srv.SetSpawnLineageSealTTL(time.Hour)
	f.now = f.now.Add(2 * time.Hour)
	requireEscalates(t, f.spawn(engineCtx(), req))
	f.srv.SetSpawnLineageSealTTL(0)
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("a non-positive TTL must restore the default, got %v", err)
	}

	// A seal stamped in the future is not valid.
	f.now = f.now.Add(-40 * 24 * time.Hour)
	requireEscalates(t, f.spawn(engineCtx(), req))
}

// Nobody can write, copy or clear the seal: not the static token, not an agent,
// and not even an OIDC admin (the server stamps it).
func TestSpawnLineage_SealIsServerOwned(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	sealed := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageField].(string)
	if sealed == "" {
		t.Fatal("setup: an OIDC admin create must seal the trigger")
	}
	callers := map[string]context.Context{
		"static":     machineUserCtx(),
		"agent":      WithSystemOperator(withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic), "slack:U0TESTER1"),
		"oidc admin": adminOIDCCtx(),
	}
	for name, ctx := range callers {
		t.Run(name, func(t *testing.T) {
			// Create carrying a copied seal.
			_, err := f.srv.createBead(ctx, createBeadInput{
				Title: "copy", Type: "trigger", Kind: "data", Labels: []string{"project:gardenio"},
				Fields: json.RawMessage(`{"action":"spawn_agent","role":"reviewer","project":"gardenio","spawn_lineage":` + strconv.Quote(sealed) + `}`),
			})
			if err == nil {
				t.Fatal("a create carrying a seal must be refused")
			}
			// Update setting, changing or clearing it.
			for _, patch := range []string{`{"spawn_lineage":"x"}`, `{"spawn_lineage":null}`} {
				if _, err := f.srv.updateBead(ctx, "gd-trig", updateBeadInput{Fields: json.RawMessage(patch)}); err == nil {
					t.Fatalf("update %s must be refused", patch)
				}
			}
			if got := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageField]; got != sealed {
				t.Fatalf("seal changed to %v", got)
			}
			// The single-field mutation gate and the merge primitive.
			if d := f.srv.authorizeAgentBeadFieldWrite(ctx, "update", f.ms.beads["gd-trig"], spawnLineageField); d == nil {
				t.Fatal("single-field gate must refuse the seal key")
			}
			if err := f.srv.mergeBeadFields(ctx, "gd-trig", map[string]any{spawnLineageField: "x"}); err == nil {
				t.Fatal("mergeBeadFields must refuse the seal key")
			}
		})
	}
	// The gate itself refuses even where field validation would not (a type that
	// allows additional fields).
	for _, typ := range []model.BeadType{"trigger", "schedule"} {
		if spawnLineageSealPatchDenial("create", typ, json.RawMessage(`{"spawn_lineage":"x"}`), nil) == nil ||
			spawnLineageSealPatchDenial("update", typ, json.RawMessage(`{"spawn_lineage":null}`), json.RawMessage(`{"spawn_lineage":"x"}`)) == nil {
			t.Fatalf("%s: seal patch gate must refuse a new or changed seal", typ)
		}
		if spawnLineageSealPatchDenial("update", typ, json.RawMessage(`{"spawn_lineage":"x"}`), json.RawMessage(`{"spawn_lineage":"x"}`)) != nil {
			t.Fatalf("%s: seal patch gate must tolerate an unchanged echo", typ)
		}
	}
	if spawnLineageSealPatchDenial("create", "task", json.RawMessage(`{"spawn_lineage":"x"}`), nil) != nil {
		t.Fatal("seal gate must not touch other bead types")
	}
	// An unchanged echo from a read-modify-write client is tolerated.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"spawn_lineage":` + strconv.Quote(sealed) + `,"last_checked":"x"}`)}); err != nil {
		t.Fatalf("echoing the unchanged seal must be tolerated: %v", err)
	}
}

// An admin edit continues their own sealed work, but must not bless content a
// non-admin rewrote in between; adopting it takes an explicit reseal.
func TestSpawnLineage_AdminUpdateDoesNotBlessTamperedContent(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	req := func(role string) json.RawMessage { return spawnFieldsWithTrigger("gardenio", role, "gd-trig") }

	// Continuing sealed work: an admin edit re-seals the new content.
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"prompt":"review nightly"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.spawn(engineCtx(), req("reviewer")); err != nil {
		t.Fatalf("admin edit of a sealed trigger must keep it sealed, got %v", err)
	}

	// A non-admin retargets the role; an admin then edits something unrelated.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"captain"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"cooldown_minutes":10}`)}); err != nil {
		t.Fatal(err)
	}
	requireEscalates(t, f.spawn(engineCtx(), req("captain")))

	// The admin reviews and adopts the content: the control key is consumed.
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"reseal_spawn_lineage":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.spawn(engineCtx(), req("captain")); err != nil {
		t.Fatalf("explicit reseal must adopt the current content, got %v", err)
	}
	if _, stored := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageResealKey]; stored {
		t.Fatal("the reseal control key must never be stored")
	}
	// A non-admin cannot reseal.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"reviewer","reseal_spawn_lineage":true}`)}); err != nil {
		t.Fatal(err)
	}
	requireEscalates(t, f.spawn(engineCtx(), req("reviewer")))
}

// A forged fields.trigger_id gives nothing to any caller but the engine: the
// static token, a per-agent token, another machine principal, a machine
// without the lineage capability, and a fleet-style context all escalate.
func TestSpawnLineage_ForgedTriggerIDGivesNoEffect(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	fields := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
	f.ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}

	callers := map[string]context.Context{
		"static token":     withAuthMethod(context.Background(), authMethodStatic),
		"agent token":      withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic),
		"other machine":    machineCtx("gardenio-authz-canary", "gardenio-operator", "gardenio-machine-trigger-ops"),
		"engine, no cap":   machineCtx("gardenio-trigger-engine", "gardenio-operator"),
		"engine, wrong id": machineCtx("gardenio-trigger-engine2", "gardenio-operator", "gardenio-machine-trigger-ops"),
	}
	for name, ctx := range callers {
		t.Run(name, func(t *testing.T) {
			requireEscalates(t, f.spawn(ctx, fields))
		})
	}
}

// The marker is minted only by withTriggerLineageSpawnAuth: a context cannot
// acquire it from request data, and an update (re-role) never honours it.
func TestSpawnLineage_UpdateDoesNotHonourLineage(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	fields := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
	ctx := f.srv.withTriggerLineageSpawnAuth(engineCtx(), fields)
	if _, ok := triggerLineageSpawnFromContext(ctx); !ok {
		t.Fatal("setup: lineage marker should be present")
	}
	requireEscalates(t, authorizeAgentRole(ctx, f.ms, "update", fields))
}

// End to end through the real mux: an admin-sealed trigger's spawn of a
// privileged role is created with operated_by = the admin; the same create by
// the canary, the static token, for a trigger whose seal has expired, or for a role the
// trigger does not spawn is refused/escalated.
func TestSpawnLineage_CreateBead_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-trigger-engine", "gardenio-authz-canary"})
	f := newLineageFixture(t)
	f.srv.userProvisioner = nil
	f.srv.SetFleetAuth(h.fc)
	f.trigger("gd-trig", nil)
	f.now = f.now.Add(-2 * defaultSpawnLineageSealTTL) // sealed long ago
	f.trigger("gd-trig-old", nil)
	f.now = f.now.Add(2 * defaultSpawnLineageSealTTL)
	handler := f.srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	engine := h.saToken(t, "gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops")
	canary := h.saToken(t, "gardenio-authz-canary", "gardenio-operator", "gardenio-machine-trigger-ops")

	spawn := func(trigger, role string) map[string]any {
		return map[string]any{
			"title": "gardenio-reviewer-abc", "type": "agent", "kind": "config",
			"fields": map[string]any{"agent": "gardenio-reviewer-abc", "role": role, "project": "gardenio", "trigger_id": trigger},
		}
	}

	rec := machineDo(t, handler, engine, http.MethodPost, "/v1/beads", spawn("gd-trig", "reviewer"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("engine spawn for admin trigger: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OperatedBy string `json:"operated_by"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OperatedBy != lineageAdmin {
		t.Fatalf("operated_by=%q; want %q", got.OperatedBy, lineageAdmin)
	}

	for name, tc := range map[string]struct{ token, trigger, role string }{
		"engine, expired seal":  {engine, "gd-trig-old", "reviewer"},
		"engine, role mismatch": {engine, "gd-trig", "captain"},
		"canary, admin trigger": {canary, "gd-trig", "reviewer"},
		"static, admin trigger": {machineStaticToken, "gd-trig", "reviewer"},
	} {
		rec := machineDo(t, handler, tc.token, http.MethodPost, "/v1/beads", spawn(tc.trigger, tc.role))
		if rec.Code == http.StatusCreated {
			t.Fatalf("%s: spawn of a privileged role must not be created, status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

// Only an OIDC session holding gardenio-admin seals; an OIDC session without the
// role does not, whatever the bead's attribution says.
func TestSpawnLineage_NonAdminOIDCSessionDoesNotSeal(t *testing.T) {
	bead := &model.Bead{Type: "trigger", Fields: json.RawMessage(lineageTriggerFields)}
	srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	if srv.stampSpawnLineage(oidcCtx("viewer@example.com", []string{"gardenio-operator"}), bead, true) {
		t.Fatal("a non-admin OIDC session must not seal")
	}
	if !srv.stampSpawnLineage(adminOIDCCtx(), bead, true) || sealedSpawnAuthor(bead) != lineageAdmin {
		t.Fatalf("an OIDC admin session must seal to its own subject, got %q", sealedSpawnAuthor(bead))
	}
}

// gd-VuYwGGYLLT: a gardener's second life is the trigger-engine reopening and
// re-arming the bead it closed on max_runs. That revival must keep the admin's
// seal (otherwise every revived lane's spawns escalate), while the same writes
// from anyone else, or an engine reopen of a trigger with no gardener blob,
// still void it.
func TestSpawnLineage_GardenerSecondLifeKeepsSeal(t *testing.T) {
	req := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")
	cfg := `{"name":"release-owner","terminal_goal":"merge READY beads","max_runs":50,"standing":true}`
	closed, open := string(model.StatusClosed), string(model.StatusOpen)
	revive := func(f *lineageFixture, ctx context.Context) {
		t.Helper()
		// The engine's terminal close, then the second-life reopen + re-arm.
		for _, in := range []updateBeadInput{
			{Status: &closed, Fields: json.RawMessage(`{"gardener_terminated_kind":"max_runs"}`)},
			{Fields: json.RawMessage(`{"enabled":false}`)},
		} {
			if _, err := f.srv.updateBead(engineCtx(), "gd-trig", in); err != nil {
				t.Fatal(err)
			}
		}
		for _, in := range []updateBeadInput{
			{Status: &open},
			{Fields: json.RawMessage(`{"gardener_terminated_kind":""}`)},
			{Fields: json.RawMessage(`{"enabled":true}`)},
		} {
			if _, err := f.srv.updateBead(ctx, "gd-trig", in); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		gardener   bool
		wantSealed bool
	}{
		{"engine revives a gardener", engineCtx(), true, true},
		{"static token revives a gardener", staticCtx(), true, false},
		{"engine reopens a trigger that has no gardener blob", engineCtx(), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLineageFixture(t)
			f.trigger("gd-trig", nil)
			if tc.gardener {
				if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(
					`{"gardener":` + strconv.Quote(cfg) + `}`)}); err != nil {
					t.Fatal(err)
				}
			}
			revive(f, tc.ctx)
			if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]) != ""; got != tc.wantSealed {
				t.Fatalf("sealed after revival = %v, want %v", got, tc.wantSealed)
			}
			err := f.spawn(engineCtx(), req)
			if tc.wantSealed && err != nil {
				t.Fatalf("revived sealed gardener must spawn, got %v", err)
			}
			if !tc.wantSealed {
				requireEscalates(t, err)
			}
		})
	}
}
