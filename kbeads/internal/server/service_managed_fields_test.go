package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

// ka-0000003039 / gd-dESFU8oLfb: fleet-token services (jira-bridge, gardener
// evaluator, etc.) must be able to stamp observability fields (last_checked,
// last_run) on long-lived shared trigger/schedule beads they did not create.
// The ownership gate is bypassed when the diff between the incoming patch and
// the bead's existing fields touches ONLY the bead-type's whitelisted
// service-managed fields. Echoed-back user-authored fields (the result of
// read-modify-write in beadsapi.Client.UpdateBeadFields) do not block the
// bypass because their values are unchanged.

const triggerExistingFields = `{"source":"jira","jql":"project = FSM","action":"spawn_agent"}`

func triggerBead(id, owner string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "FSM trigger",
		Type:      "trigger",
		Kind:      model.KindData,
		Status:    model.StatusOpen,
		CreatedBy: owner,
		Labels:    []string{"project:monorepo", "trigger:jira"},
		Fields:    json.RawMessage(triggerExistingFields),
	}
}

func TestPatchOnlyChangesServiceManagedFields(t *testing.T) {
	existing := json.RawMessage(triggerExistingFields)

	t.Run("trigger: last_checked only → allowed", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"source":"jira","jql":"project = FSM","action":"spawn_agent","last_checked":"2026-05-25T21:00:00Z"}`)}
		if !patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("trigger last_checked patch (echoing existing fields) should bypass ownership")
		}
	})

	t.Run("trigger: last_checked-only narrow patch → allowed", func(t *testing.T) {
		// The grpc API can also be invoked with a narrow patch directly (no
		// read-modify-write echo). Empty existing means the keys in the patch
		// must all be whitelisted.
		in := &updateBeadInput{Fields: json.RawMessage(`{"last_checked":"2026-05-25T21:00:00Z"}`)}
		if !patchOnlyChangesServiceManagedFields("trigger", in, nil) {
			t.Fatal("trigger last_checked patch should bypass ownership")
		}
	})

	t.Run("trigger: last_checked + last_run echo → allowed", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"source":"jira","jql":"project = FSM","action":"spawn_agent","last_checked":"x","last_run":"y"}`)}
		if !patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("trigger last_checked+last_run echo patch should bypass ownership")
		}
	})

	t.Run("trigger: jql changed → denied", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"source":"jira","jql":"project = NEW","action":"spawn_agent","last_checked":"x"}`)}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("jql change must NOT bypass ownership")
		}
	})

	t.Run("trigger: title change → denied", func(t *testing.T) {
		title := "renamed"
		in := &updateBeadInput{Title: &title, Fields: json.RawMessage(`{"last_checked":"x"}`)}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("title change must NOT bypass ownership")
		}
	})

	t.Run("trigger: status change → denied", func(t *testing.T) {
		st := "closed"
		in := &updateBeadInput{Status: &st}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("status change must NOT bypass ownership")
		}
	})

	t.Run("trigger: labels change → denied", func(t *testing.T) {
		in := &updateBeadInput{Labels: []string{"project:other"}, labelsSet: true}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("labels change must NOT bypass ownership")
		}
	})

	t.Run("trigger: empty fields → denied (no net change)", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{}`)}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("empty fields patch must NOT bypass ownership (changed=false)")
		}
	})

	t.Run("trigger: echo only (no net change) → denied", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(triggerExistingFields)}
		if patchOnlyChangesServiceManagedFields("trigger", in, existing) {
			t.Fatal("no-op echo patch must NOT bypass ownership (changed=false)")
		}
	})

	t.Run("task: last_checked → denied (no whitelist for task)", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"last_checked":"x"}`)}
		if patchOnlyChangesServiceManagedFields("task", in, nil) {
			t.Fatal("task type has no service-managed fields; must not bypass")
		}
	})

	t.Run("schedule: last_checked_agent + runs_spawned → allowed", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"last_checked_agent":"x","runs_spawned":5}`)}
		if !patchOnlyChangesServiceManagedFields("schedule", in, nil) {
			t.Fatal("schedule observability patch should bypass ownership")
		}
	})

	// "last_checked" was removed from serviceManagedFieldsByType["schedule"]
	// as a DEAD entry (ka-0000005214). It was unreachable three times over:
	// type:schedule never declared a `last_checked` field, so such a write
	// 400s "unknown field" before authorization is consulted; no scheduler
	// stamp helper writes it (the only producer anywhere is
	// TriggerEngine.stampCheckedOutcome, which stamps TRIGGER beads); and
	// type:schedule is kind=config, which authorizeOwnership skips outright.
	// This case pins the removal so it is not silently reintroduced by
	// pattern-matching against the trigger entry.
	t.Run("schedule: last_checked alone → not allowed (dead entry removed)", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"last_checked":"x"}`)}
		if patchOnlyChangesServiceManagedFields("schedule", in, nil) {
			t.Fatal("schedule last_checked must NOT be service-managed: the field is not " +
				"declared on type:schedule and nothing writes it")
		}
	})

	t.Run("schedule: cron change → denied", func(t *testing.T) {
		existingSched := json.RawMessage(`{"cron":"0 * * * *","prompt":"do it"}`)
		in := &updateBeadInput{Fields: json.RawMessage(`{"cron":"* * * * *","prompt":"do it","last_run":"x"}`)}
		if patchOnlyChangesServiceManagedFields("schedule", in, existingSched) {
			t.Fatal("schedule cron change must NOT bypass ownership")
		}
	})

	t.Run("agent: context_tokens + live_cost_usd + live_turns → allowed", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"context_tokens":"90000","live_cost_usd":"1.5","live_turns":"42"}`)}
		if !patchOnlyChangesServiceManagedFields("agent", in, nil) {
			t.Fatal("agent observability patch (context_tokens/live_cost_usd/live_turns) should bypass ownership")
		}
	})

	t.Run("agent: non-observability field → denied", func(t *testing.T) {
		in := &updateBeadInput{Fields: json.RawMessage(`{"context_tokens":"90000","agent_state":"working"}`)}
		if patchOnlyChangesServiceManagedFields("agent", in, nil) {
			t.Fatal("agent_state is not service-managed; must not bypass")
		}
	})
}

// TestGRPCUpdateBead_TriggerLastCheckedBypassesOwnership exercises the
// canonical ka-0000003039 case end-to-end: a fleet-token service updates
// last_checked on a trigger bead it does not own, and the update succeeds.
func TestGRPCUpdateBead_TriggerLastCheckedBypassesOwnership(t *testing.T) {
	srv, ms, _ := newTestServer()
	// Trigger bead created by a Slack thread identity; the fleet bridge that
	// updates last_checked has a totally different subject.
	ms.beads["gd-trig1"] = triggerBead("gd-trig1", "thread-1777560916-494139")

	ctx := fleetCtx("gardenio-jira-bridge")
	// Patch ONLY last_checked.
	fields, _ := json.Marshal(map[string]any{"last_checked": "2026-05-25T21:00:00Z"})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig1",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("fleet bridge last_checked update should succeed, got: %v", err)
	}

	// Confirm the field was actually written.
	got := ms.beads["gd-trig1"]
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if stored["last_checked"] != "2026-05-25T21:00:00Z" {
		t.Errorf("last_checked not persisted: got %v", stored["last_checked"])
	}
}

// TestGRPCUpdateBead_TriggerLastCheckedEchoBypassesOwnership exercises the
// real-world path: the controller's beadsapi.Client.UpdateBeadFields does a
// read-modify-write and PATCHes the full merged fields map. The server must
// diff against existing fields so the echoed user-authored values do not
// block the bypass. (gd-dESFU8oLfb)
func TestGRPCUpdateBead_TriggerLastCheckedEchoBypassesOwnership(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig-echo"] = triggerBead("gd-trig-echo", "thread-1777560916-494139")

	ctx := fleetCtx("gardenio-jira-bridge")
	// Echo the full existing fields with last_checked added — exactly what
	// beadsapi.Client.UpdateBeadFields sends.
	fields, _ := json.Marshal(map[string]any{
		"source":       "jira",
		"jql":          "project = FSM",
		"action":       "spawn_agent",
		"last_checked": "2026-05-25T21:00:00Z",
	})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig-echo",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("read-modify-write last_checked echo should succeed, got: %v", err)
	}

	got := ms.beads["gd-trig-echo"]
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if stored["last_checked"] != "2026-05-25T21:00:00Z" {
		t.Errorf("last_checked not persisted: got %v", stored["last_checked"])
	}
	if stored["jql"] != "project = FSM" {
		t.Errorf("jql clobbered: got %v", stored["jql"])
	}
}

// TestGRPCUpdateBead_TriggerNonWhitelistedFieldDenied confirms that a
// fleet-token update to a non-whitelisted field on a trigger bead it does
// not own is still denied. The bypass is field-scoped.
func TestGRPCUpdateBead_TriggerNonWhitelistedFieldDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig2"] = triggerBead("gd-trig2", "thread-xyz")

	ctx := fleetCtx("gardenio-jira-bridge")
	// Patch a user-authored field — must be denied for non-owner.
	fields, _ := json.Marshal(map[string]any{"jql": "project = OTHER"})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig2",
		Fields: fields,
	})
	requireCode(t, err, codes.PermissionDenied)
}

// TestGRPCUpdateBead_TriggerEchoWithJQLChangeDenied confirms that an echoed
// patch which also mutates a user-authored field (jql) is still denied — the
// diff must contain ONLY whitelisted changes. (gd-dESFU8oLfb)
func TestGRPCUpdateBead_TriggerEchoWithJQLChangeDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig-mixed"] = triggerBead("gd-trig-mixed", "thread-xyz")

	ctx := fleetCtx("gardenio-jira-bridge")
	fields, _ := json.Marshal(map[string]any{
		"source":       "jira",
		"jql":          "project = HIJACKED",
		"action":       "spawn_agent",
		"last_checked": "2026-05-25T21:00:00Z",
	})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig-mixed",
		Fields: fields,
	})
	requireCode(t, err, codes.PermissionDenied)
}

// TestGRPCUpdateBead_TriggerMixedFieldsDenied confirms that a narrow patch
// which touches both a whitelisted field AND a non-whitelisted field is
// denied.
func TestGRPCUpdateBead_TriggerMixedFieldsDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig3"] = triggerBead("gd-trig3", "thread-xyz")

	ctx := fleetCtx("gardenio-jira-bridge")
	fields, _ := json.Marshal(map[string]any{
		"last_checked": "2026-05-25T21:00:00Z",
		"jql":          "project = OTHER",
	})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig3",
		Fields: fields,
	})
	requireCode(t, err, codes.PermissionDenied)
}

// TestGRPCUpdateBead_TriggerTitleChangeDenied confirms title changes still
// require ownership even when the fields patch is whitelisted.
func TestGRPCUpdateBead_TriggerTitleChangeDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig4"] = triggerBead("gd-trig4", "thread-xyz")

	ctx := fleetCtx("gardenio-jira-bridge")
	title := "renamed by bridge"
	fields, _ := json.Marshal(map[string]any{"last_checked": "2026-05-25T21:00:00Z"})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig4",
		Title:  &title,
		Fields: fields,
	})
	requireCode(t, err, codes.PermissionDenied)
}

// TestGRPCUpdateBead_TriggerStampCheckedOutcomeBypassesOwnership is the
// ka-0000004245 regression: the OUTAGE shape, not a synthetic one.
//
// On 2026-08-14 commit 6a8511bce (ka-0000001513) folded last_outcome /
// last_outcome_at into the SAME patch as last_checked — see
// TriggerEngine.stampCheckedOutcome. Those two keys were never added to
// serviceManagedFieldsByType["trigger"], so from that commit on EVERY
// engine-side liveness stamp carried a non-allowlisted key,
// patchOnlyChangesServiceManagedFields returned false, the ownership bypass
// was skipped, and authorizeOwnership denied the write.
//
// Effect: three prod jira intake triggers froze last_checked at
// 2026-08-14T04:3x and the daemon logged ~180 authz:deny:update:trigger audit
// beads/hour for 11 days. stampLastRun kept working the whole time (last_run /
// last_agent_id WERE allowlisted), which is why the lanes still fired and the
// freeze read as a pure liveness-signal failure rather than a dead lane.
func TestGRPCUpdateBead_TriggerStampCheckedOutcomeBypassesOwnership(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig-outcome"] = triggerBead("gd-trig-outcome", "monorepo-time-to-add-1y5")

	ctx := fleetCtx("bridge:jira")
	// The literal stampCheckedOutcome patch.
	fields, _ := json.Marshal(map[string]any{
		"last_checked":    "2026-08-25T18:00:00Z",
		"last_outcome":    "no new matches",
		"last_outcome_at": "2026-08-25T18:00:00Z",
	})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig-outcome",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("stampCheckedOutcome patch should bypass ownership, got: %v", err)
	}

	var stored map[string]any
	if err := json.Unmarshal(ms.beads["gd-trig-outcome"].Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if stored["last_checked"] != "2026-08-25T18:00:00Z" {
		t.Errorf("last_checked not persisted: got %v", stored["last_checked"])
	}
	if stored["last_outcome"] != "no new matches" {
		t.Errorf("last_outcome not persisted: got %v", stored["last_outcome"])
	}
}

// TestGRPCUpdateBead_TriggerFireErrorBypassesOwnership covers the sibling
// stamp path (TriggerEngine.stampFireError / clearFireError, ka-0000002991) for
// a NON-gardener trigger. It writes last_fire_error / last_fire_error_at only,
// and hit the identical denial — silently, because that write is best-effort.
func TestGRPCUpdateBead_TriggerFireErrorBypassesOwnership(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig-err"] = triggerBead("gd-trig-err", "monorepo-time-to-add-1y5")

	ctx := fleetCtx("bridge:jira")
	fields, _ := json.Marshal(map[string]any{
		"last_fire_error":    "spawn failed: quota exceeded",
		"last_fire_error_at": "2026-08-25T18:00:00Z",
	})
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig-err",
		Fields: fields,
	}); err != nil {
		t.Fatalf("stampFireError patch should bypass ownership, got: %v", err)
	}
}

// TestGRPCUpdateBead_TriggerGardenerBlobStillDenied is the NARROWNESS guard for
// ka-0000004245. The gardener arm of stampFireError also patches the `gardener`
// spec blob. That blob is CONFIG (schedule, prompt, hard_stop limits), not
// fire-state, so it is deliberately NOT allowlisted: a service actor must not
// be able to rewrite a gardener's spec on a bead it does not own. This test
// fails if a future change widens the allowlist to cover it.
func TestGRPCUpdateBead_TriggerGardenerBlobStillDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-trig-cap"] = triggerBead("gd-trig-cap", "monorepo-time-to-add-1y5")

	ctx := fleetCtx("bridge:jira")
	fields, _ := json.Marshal(map[string]any{
		"last_fire_error": "boom",
		"gardener":        `{"hard_stop":{"consecutive_failures":99}}`,
	})
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-trig-cap",
		Fields: fields,
	}); err == nil {
		t.Fatal("patching the gardener spec blob must NOT bypass ownership")
	}
}

// TestGRPCUpdateBead_TriggerConfigFieldsStillDenied is the explicit
// NON-WIDENING proof for the ka-0000004245 allowlist change. Adding the
// fire-state keys must not let a service actor touch anything that changes
// what a trigger DOES on a bead it does not own. Each of these carries real
// authority:
//
//	enabled     — flipping it false silently kills an intake lane
//	jql         — repoints the lane at other people's tickets
//	role        — escalates what the spawned agent may do
//	formula_id  — swaps the work the lane performs
//	prompt      — rewrites the spawned agent's instructions
//	max_concurrent / cooldown_minutes — resource + rate authority
//
// Each is submitted ALONE (the strongest form: if a lone config key were
// allowed, a mixed patch certainly would be).
func TestGRPCUpdateBead_TriggerConfigFieldsStillDenied(t *testing.T) {
	for _, key := range []string{
		"enabled", "jql", "role", "formula_id", "prompt",
		"max_concurrent", "cooldown_minutes", "gardener", "source", "action",
	} {
		t.Run(key, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-trig-cfg"] = triggerBead("gd-trig-cfg", "monorepo-time-to-add-1y5")
			ctx := fleetCtx("bridge:jira")
			fields, _ := json.Marshal(map[string]any{key: "hijacked"})
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
				Id:     "gd-trig-cfg",
				Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)
		})
	}
}

// TestGRPCUpdateBead_FireStateFieldsAreTriggerScoped proves the new keys did
// not leak into any other bead type's bypass. A fleet actor patching
// last_outcome / last_fire_error on a TASK it does not own stays denied —
// serviceManagedFieldsByType has no entry for "task", so there is no bypass
// to inherit.
func TestGRPCUpdateBead_FireStateFieldsAreTriggerScoped(t *testing.T) {
	for _, key := range []string{"last_outcome", "last_outcome_at", "last_fire_error", "last_fire_error_at"} {
		t.Run(key, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-task1"] = ownedBead("gd-task1", "someone-else")
			ctx := fleetCtx("bridge:jira")
			fields, _ := json.Marshal(map[string]any{key: "x"})
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
				Id:     "gd-task1",
				Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)
		})
	}
}

// TestTriggerFireStateAllowlistMatchesEngineStamps is the drift guard for the
// sync note on serviceManagedFieldsByType. The ka-0000004245 outage was caused
// by the engine's stamp patch and this allowlist drifting apart, so pin the
// exact expected set: a future field added to a stamp helper (or removed from
// here) fails this test and names the file to update.
func TestTriggerFireStateAllowlistMatchesEngineStamps(t *testing.T) {
	// Union of every key the TriggerEngine stamp helpers write via
	// UpdateBeadFields on a NON-gardener trigger:
	//   stampCheckedOutcome → last_checked, last_outcome, last_outcome_at
	//   stampLastRun        → last_run, last_agent_id, last_match
	//   stampFireError      → last_fire_error, last_fire_error_at
	//   clearFireError      → last_fire_error, last_fire_error_at
	want := map[string]bool{
		"last_checked": true, "last_outcome": true, "last_outcome_at": true,
		"last_run": true, "last_agent_id": true, "last_match": true,
		"last_fire_error": true, "last_fire_error_at": true,
	}
	got := map[string]bool{}
	for _, k := range serviceManagedFieldsByType["trigger"] {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("engine stamps %q but serviceManagedFieldsByType[trigger] omits it — "+
				"the whole stamp patch will be ownership-denied (ka-0000004245)", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("serviceManagedFieldsByType[trigger] allows %q which no engine stamp "+
				"helper writes — remove it or update this test's `want` set", k)
		}
	}
}
