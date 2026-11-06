package bridge

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- checkTriggerAuth tests ---

func TestCheckTriggerAuth_SystemTrigger(t *testing.T) {
	// Triggers without a creator are always allowed.
	trig := TriggerDef{
		ID:      "t1",
		Project: "gardenio",
		Role:    "crew",
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected system trigger to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_AllowedWithNoRestrictions(t *testing.T) {
	// Trigger with creator but no allowed_projects/allowed_roles is allowed.
	trig := TriggerDef{
		ID:        "t1",
		CreatedBy: "user@example.com",
		Project:   "gardenio",
		Role:      "crew",
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected trigger with no restrictions to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_ProjectAllowed(t *testing.T) {
	trig := TriggerDef{
		ID:              "t1",
		CreatedBy:       "user@example.com",
		Project:         "gardenio",
		AllowedProjects: []string{"gardenio", "kbeads"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected project in allowed list to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_ProjectDenied(t *testing.T) {
	trig := TriggerDef{
		ID:              "t1",
		CreatedBy:       "user@example.com",
		Project:         "secret-project",
		AllowedProjects: []string{"gardenio", "kbeads"},
	}
	result := checkTriggerAuth(trig)
	if result.Allowed {
		t.Fatal("expected project not in allowed list to be denied")
	}
	if result.Reason == "" {
		t.Fatal("expected deny reason to be set")
	}
}

func TestCheckTriggerAuth_ProjectWildcard(t *testing.T) {
	trig := TriggerDef{
		ID:              "t1",
		CreatedBy:       "user@example.com",
		Project:         "any-project",
		AllowedProjects: []string{"*"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected wildcard project to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_RoleAllowed(t *testing.T) {
	trig := TriggerDef{
		ID:           "t1",
		CreatedBy:    "user@example.com",
		Role:         "crew",
		AllowedRoles: []string{"crew", "gardener"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected role in allowed list to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_RoleDenied(t *testing.T) {
	trig := TriggerDef{
		ID:           "t1",
		CreatedBy:    "user@example.com",
		Role:         "admin",
		AllowedRoles: []string{"crew", "gardener"},
	}
	result := checkTriggerAuth(trig)
	if result.Allowed {
		t.Fatal("expected role not in allowed list to be denied")
	}
}

func TestCheckTriggerAuth_RoleWildcard(t *testing.T) {
	trig := TriggerDef{
		ID:           "t1",
		CreatedBy:    "user@example.com",
		Role:         "admin",
		AllowedRoles: []string{"*"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected wildcard role to be allowed, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_BothProjectAndRoleDenied(t *testing.T) {
	// When both restrictions are set and project fails, deny on project.
	trig := TriggerDef{
		ID:              "t1",
		CreatedBy:       "user@example.com",
		Project:         "secret",
		Role:            "admin",
		AllowedProjects: []string{"gardenio"},
		AllowedRoles:    []string{"crew"},
	}
	result := checkTriggerAuth(trig)
	if result.Allowed {
		t.Fatal("expected both restrictions to deny")
	}
}

func TestCheckTriggerAuth_EmptyProjectSkipsCheck(t *testing.T) {
	// If trigger has no target project, allowed_projects check is skipped.
	trig := TriggerDef{
		ID:              "t1",
		CreatedBy:       "user@example.com",
		AllowedProjects: []string{"gardenio"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected empty project to skip check, got: %s", result.Reason)
	}
}

func TestCheckTriggerAuth_EmptyRoleSkipsCheck(t *testing.T) {
	// If trigger has no target role, allowed_roles check is skipped.
	trig := TriggerDef{
		ID:           "t1",
		CreatedBy:    "user@example.com",
		AllowedRoles: []string{"crew"},
	}
	result := checkTriggerAuth(trig)
	if !result.Allowed {
		t.Fatalf("expected empty role to skip check, got: %s", result.Reason)
	}
}

// --- parseStringList tests ---

func TestParseStringList_Empty(t *testing.T) {
	if got := parseStringList(""); got != nil {
		t.Fatalf("expected nil for empty string, got %v", got)
	}
}

func TestParseStringList_JSONArray(t *testing.T) {
	got := parseStringList(`["gardenio","kbeads"]`)
	if len(got) != 2 || got[0] != "gardenio" || got[1] != "kbeads" {
		t.Fatalf("expected [gardenio kbeads], got %v", got)
	}
}

func TestParseStringList_CommaSeparated(t *testing.T) {
	got := parseStringList("gardenio, kbeads, beads3d")
	if len(got) != 3 || got[0] != "gardenio" || got[1] != "kbeads" || got[2] != "beads3d" {
		t.Fatalf("expected [gardenio kbeads beads3d], got %v", got)
	}
}

func TestParseStringList_SingleValue(t *testing.T) {
	got := parseStringList("gardenio")
	if len(got) != 1 || got[0] != "gardenio" {
		t.Fatalf("expected [gardenio], got %v", got)
	}
}

func TestParseStringList_Wildcard(t *testing.T) {
	got := parseStringList(`["*"]`)
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("expected [*], got %v", got)
	}
}

// --- ParseTriggerDef permission fields ---

func TestParseTriggerDef_ExecutionTarget(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "t1",
		Title: "Test Trigger",
		Fields: map[string]string{
			"action":           "spawn_agent",
			"role":             "plan-gardener",
			"project":          "gardenio",
			"execution_target": " lambda_microvm ",
		},
	}

	trig := ParseTriggerDef(bead, "schedule")

	if trig.ExecutionTarget != "lambda_microvm" {
		t.Errorf("expected ExecutionTarget=lambda_microvm (trimmed), got %q", trig.ExecutionTarget)
	}
}

func TestParseTriggerDef_PermissionFields(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:        "t1",
		Title:     "Test Trigger",
		CreatedBy: "user@example.com",
		Fields: map[string]string{
			"action":           "spawn_agent",
			"role":             "crew",
			"project":          "gardenio",
			"allowed_projects": `["gardenio","kbeads"]`,
			"allowed_roles":    "crew,gardener",
		},
	}

	trig := ParseTriggerDef(bead, "jira")

	if trig.CreatedBy != "user@example.com" {
		t.Errorf("expected CreatedBy=user@example.com, got %q", trig.CreatedBy)
	}
	if len(trig.AllowedProjects) != 2 || trig.AllowedProjects[0] != "gardenio" {
		t.Errorf("expected AllowedProjects=[gardenio kbeads], got %v", trig.AllowedProjects)
	}
	if len(trig.AllowedRoles) != 2 || trig.AllowedRoles[0] != "crew" {
		t.Errorf("expected AllowedRoles=[crew gardener], got %v", trig.AllowedRoles)
	}
}

func TestParseTriggerDef_NoPermissionFields(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "t1",
		Title: "Test Trigger",
		Fields: map[string]string{
			"action": "notify",
		},
	}

	trig := ParseTriggerDef(bead, "jira")

	if trig.CreatedBy != "" {
		t.Errorf("expected empty CreatedBy, got %q", trig.CreatedBy)
	}
	if trig.AllowedProjects != nil {
		t.Errorf("expected nil AllowedProjects, got %v", trig.AllowedProjects)
	}
	if trig.AllowedRoles != nil {
		t.Errorf("expected nil AllowedRoles, got %v", trig.AllowedRoles)
	}
}

// --- Audit trail tests ---

func TestRecordTriggerAudit_Success(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	logger := slog.Default()

	recordTriggerAudit(context.Background(), daemon, logger, TriggerAuditEntry{
		TriggerID:    "t1",
		TriggerTitle: "Spend Alert",
		Source:       "account",
		Action:       TriggerActionNotify,
		MatchKey:     "spend:primary:total_cost_usd:100",
		MatchTitle:   "Account primary: spend_threshold",
		Project:      "gardenio",
		CreatedBy:    "user@example.com",
		Outcome:      "allow",
		Reason:       "authorized",
		Timestamp:    time.Now(),
	})

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 audit bead, got %d", len(daemon.createdBeads))
	}

	bead := daemon.createdBeads[0]
	if bead.Type != "event" {
		t.Errorf("expected type=event, got %q", bead.Type)
	}
	if !trigPermHasLabel(bead.Labels, "auto:trigger-audit") {
		t.Error("expected auto:trigger-audit label")
	}
	if !trigPermHasLabel(bead.Labels, "source:account") {
		t.Error("expected source:account label")
	}
	if !trigPermHasLabel(bead.Labels, "trigger:t1") {
		t.Error("expected trigger:t1 label")
	}
	if bead.CreatedBy != "trigger-engine" {
		t.Errorf("expected created_by=trigger-engine, got %q", bead.CreatedBy)
	}
}

func TestRecordTriggerAudit_DenialFields(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	logger := slog.Default()

	recordTriggerAudit(context.Background(), daemon, logger, TriggerAuditEntry{
		TriggerID:    "t1",
		TriggerTitle: "Bad Trigger",
		Source:       "webhook",
		Action:       TriggerActionSpawnAgent,
		MatchKey:     "hook:123",
		Project:      "secret-project",
		Role:         "admin",
		CreatedBy:    "rogue@example.com",
		Outcome:      "deny",
		Reason:       "project not in allowed_projects",
		Timestamp:    time.Now(),
	})

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 audit bead, got %d", len(daemon.createdBeads))
	}
}

// --- Engine integration: auth denial ---

func TestTriggerEngine_AuthDenial(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:              "t1",
		Title:           "Spend alert",
		Source:          "account",
		Action:          TriggerActionNotify,
		Project:         "gardenio",
		CreatedBy:       "user@example.com",
		AllowedProjects: []string{"kbeads"}, // gardenio NOT in the list
		MaxConcurrent:   3,
		Enabled:         true,
	}

	match := TriggerMatch{
		Key:   "spend:primary:total_cost_usd:100",
		Title: "Account primary: spend_threshold",
	}

	// Fire the action — should be denied.
	_, err := engine.fireAction(context.Background(), trig, match)
	if err == nil {
		t.Fatal("expected error from denied action")
	}
	if !trigPermContains(err.Error(), "denied") {
		t.Errorf("expected denial error, got: %v", err)
	}

	// Should have created an audit bead with outcome=deny.
	var foundDeny bool
	for _, b := range daemon.createdBeads {
		if trigPermHasLabel(b.Labels, "auto:trigger-audit") {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Error("expected audit bead for denial")
	}
}

func TestTriggerEngine_AuthAllowed_AuditTrail(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	notified := false
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
		Notifier: func(ctx context.Context, title, msg string, _ string) {
			notified = true
		},
	})

	trig := TriggerDef{
		ID:              "t1",
		Title:           "Spend alert",
		Source:          "account",
		Action:          TriggerActionNotify,
		Project:         "gardenio",
		CreatedBy:       "user@example.com",
		AllowedProjects: []string{"gardenio"}, // gardenio IS in the list
		MaxConcurrent:   3,
		Enabled:         true,
		Prompt:          "Account exceeded threshold",
	}

	match := TriggerMatch{
		Key:   "spend:primary:total_cost_usd:100",
		Title: "Account primary: spend_threshold",
	}

	// Fire the action — should succeed.
	_, err := engine.fireAction(context.Background(), trig, match)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !notified {
		t.Error("expected notifier to be called")
	}

	// Should have created an audit bead with outcome=allow.
	var foundAllow bool
	for _, b := range daemon.createdBeads {
		if trigPermHasLabel(b.Labels, "auto:trigger-audit") {
			foundAllow = true
		}
	}
	if !foundAllow {
		t.Error("expected audit bead for allowed action")
	}
}

// TestTriggerEngine_AuditOperatorStamped is a regression test for ka-0000006696:
// recordTriggerAudit must run with an X-Operated-By header on its context so
// the kbeads R2 enforcement does not reject the audit-bead write with HTTP
// 400 "operator could not be resolved". fireAction stamps the trigger's
// operator (slack:/jira:/gitlab:/oidc: or system:<source>-trigger) onto ctx
// before any daemon write — the audit-bead create on both the deny and the
// allow path must see it.
func TestTriggerEngine_AuditOperatorStamped(t *testing.T) {
	t.Run("typed_created_by_propagates_to_audit", func(t *testing.T) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{
			Daemon: daemon,
			Logger: slog.Default(),
			Notifier: func(_ context.Context, _, _ string, _ string) {
			},
		})

		trig := TriggerDef{
			ID:              "t-typed",
			Title:           "Typed creator",
			Source:          "bead-state",
			Action:          TriggerActionNotify,
			Project:         "gardenio",
			CreatedBy:       "slack:U123",
			AllowedProjects: []string{"gardenio"},
			MaxConcurrent:   3,
			Enabled:         true,
			Prompt:          "ping",
		}
		match := TriggerMatch{Key: "gd-abc", Title: "match"}

		if _, err := engine.fireAction(context.Background(), trig, match); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}

		if len(daemon.createdBeads) != 1 {
			t.Fatalf("expected 1 audit bead, got %d", len(daemon.createdBeads))
		}
		if got := daemon.createOperators[0]; got != "slack:U123" {
			t.Errorf("audit-bead ctx operator: got %q, want %q", got, "slack:U123")
		}
	})

	t.Run("untyped_created_by_passthrough_to_fleet_auth", func(t *testing.T) {
		// Under fleet-all (ka-0000005187 / decision ka-0000001054) the
		// previous `system:<source>-trigger` fallback was removed because the
		// kbeads operator sanitizer (ka-0000002601) rejects system:* shapes.
		// When no typed operator is available, ctx is returned unchanged so
		// the caller's fleet-token identity flows through audit attribution.
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{
			Daemon: daemon,
			Logger: slog.Default(),
			Notifier: func(_ context.Context, _, _ string, _ string) {
			},
		})

		trig := TriggerDef{
			ID:              "t-untyped",
			Title:           "Untyped creator",
			Source:          "webhook",
			Action:          TriggerActionNotify,
			Project:         "gardenio",
			CreatedBy:       "legacy-actor", // not typed: no provider prefix
			AllowedProjects: []string{"gardenio"},
			MaxConcurrent:   3,
			Enabled:         true,
			Prompt:          "ping",
		}
		match := TriggerMatch{Key: "hook:1", Title: "match"}

		if _, err := engine.fireAction(context.Background(), trig, match); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}

		if len(daemon.createdBeads) != 1 {
			t.Fatalf("expected 1 audit bead, got %d", len(daemon.createdBeads))
		}
		if got := daemon.createOperators[0]; got != "" {
			t.Errorf("audit-bead ctx operator: got %q, want %q (passthrough to fleet auth)", got, "")
		}
	})

	t.Run("deny_path_also_stamps_operator", func(t *testing.T) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{
			Daemon: daemon,
			Logger: slog.Default(),
		})

		// AllowedProjects mismatch → checkTriggerAuth denies before any action.
		trig := TriggerDef{
			ID:              "t-deny",
			Title:           "Denied",
			Source:          "account",
			Action:          TriggerActionNotify,
			Project:         "gardenio",
			CreatedBy:       "slack:U999",
			AllowedProjects: []string{"kbeads"},
			MaxConcurrent:   3,
			Enabled:         true,
		}
		match := TriggerMatch{Key: "spend:1", Title: "match"}

		_, err := engine.fireAction(context.Background(), trig, match)
		if err == nil {
			t.Fatal("expected denial error")
		}
		if len(daemon.createdBeads) != 1 {
			t.Fatalf("expected 1 audit bead (deny), got %d", len(daemon.createdBeads))
		}
		if got := daemon.createOperators[0]; got != "slack:U999" {
			t.Errorf("deny audit-bead ctx operator: got %q, want %q", got, "slack:U999")
		}
	})

	t.Run("caller_supplied_operator_is_preserved", func(t *testing.T) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{
			Daemon: daemon,
			Logger: slog.Default(),
			Notifier: func(_ context.Context, _, _ string, _ string) {
			},
		})

		trig := TriggerDef{
			ID:              "t-caller-op",
			Title:           "Caller-supplied op",
			Source:          "webhook",
			Action:          TriggerActionNotify,
			Project:         "gardenio",
			CreatedBy:       "slack:U123", // would be used, but caller op wins
			AllowedProjects: []string{"gardenio"},
			MaxConcurrent:   3,
			Enabled:         true,
		}
		match := TriggerMatch{Key: "hook:2", Title: "match"}

		ctx := beadsapi.WithOperator(context.Background(), "gitlab:42")
		if _, err := engine.fireAction(ctx, trig, match); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if len(daemon.createdBeads) != 1 {
			t.Fatalf("expected 1 audit bead, got %d", len(daemon.createdBeads))
		}
		if got := daemon.createOperators[0]; got != "gitlab:42" {
			t.Errorf("audit-bead ctx operator: got %q, want %q (caller op should win)", got, "gitlab:42")
		}
	})
}

// --- createTaskBead includes trigger_created_by ---

func TestCreateTaskBead_IncludesCreatedBy(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:        "t1",
		Title:     "Test Trigger",
		Source:    "jira",
		Project:   "gardenio",
		CreatedBy: "author@example.com",
	}
	match := TriggerMatch{
		Key:    "JIRA-123",
		Title:  "Test issue",
		Fields: map[string]string{"jira_key": "JIRA-123"},
		Labels: []string{"source:jira"},
	}

	_, err := engine.createTaskBead(context.Background(), trig, match)
	if err != nil {
		t.Fatalf("createTaskBead error: %v", err)
	}

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(daemon.createdBeads))
	}

	// The fields JSON should contain trigger_created_by.
	bead := daemon.createdBeads[0]
	fieldsStr := string(bead.Fields)
	if !trigPermContains(fieldsStr, "trigger_created_by") {
		t.Error("expected trigger_created_by in bead fields")
	}
	if !trigPermContains(fieldsStr, "author@example.com") {
		t.Error("expected author@example.com in bead fields")
	}
}

// TestCreateTaskBead_ScheduleSourceFields is a regression test for
// gd-ddvlYtK42A: the schedule trigger source populates
// schedule_fire_key / schedule_fire_time / schedule_cron in TriggerMatch.Fields,
// and createTaskBead must pass them through onto the type=task bead so that
// CatchUpKeys (which reads schedule_fire_key off prior beads after restart)
// keeps working. The kbeads type:task config registers these names.
func TestCreateTaskBead_ScheduleSourceFields(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:      "gd-sched-1",
		Title:   "Hourly Gardener",
		Source:  "schedule",
		Project: "gardenio",
	}
	match := TriggerMatch{
		Key:   "2026-05-10T20:00",
		Title: "Schedule: Hourly Gardener",
		Fields: map[string]string{
			"schedule_fire_key":  "2026-05-10T20:00",
			"schedule_fire_time": "2026-05-10T20:00:00Z",
			"schedule_cron":      "0 * * * *",
		},
		Labels: []string{"source:schedule"},
	}

	_, err := engine.createTaskBead(context.Background(), trig, match)
	if err != nil {
		t.Fatalf("createTaskBead error: %v", err)
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(daemon.createdBeads))
	}

	req := daemon.createdBeads[0]
	if req.Type != "task" {
		t.Errorf("expected Type=task, got %q", req.Type)
	}
	fieldsStr := string(req.Fields)
	for _, want := range []string{
		"schedule_fire_key", "2026-05-10T20:00",
		"schedule_fire_time", "2026-05-10T20:00:00Z",
		"schedule_cron", "0 * * * *",
	} {
		if !trigPermContains(fieldsStr, want) {
			t.Errorf("expected %q in bead fields JSON, got %s", want, fieldsStr)
		}
	}
}

// TestCreateTaskBead_BeadSourceAssertFields is the writer-side regression for
// ka-0000005502: a BEAD-source assert trigger with on_violation=create_bead
// builds its match via beadEventToMatch + beadAssertViolationToMatch, which
// stamp bead_event_key / source_bead_id / source_event (+ assert_violated /
// assert_values) into TriggerMatch.Fields. createTaskBead must pass all of
// them through onto the filed bead — bead_event_key in particular, because
// CatchUpKeys reads it back off prior beads after a restart to re-seed the
// dedup set. The companion kbeads TestResolveTypeConfig_BeadSourceCreateBeadFields
// proves the type:task/bug schema now ACCEPTS this exact write (it used to 400
// "bead_event_key: unknown field", leaving the trigger inert).
func TestCreateTaskBead_BeadSourceAssertFields(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:       "gd-assert-1",
		Title:    "Dark gardener alert",
		Source:   "bead",
		Project:  "gardenio",
		BeadType: "bug",
	}
	// The exact match an assert trigger produces on a violated bead event.
	match := beadAssertViolationToMatch(
		beadStateEvent{
			Topic: "beads.bead.updated",
			Bead:  &BeadEvent{ID: "gd-abc", Type: "trigger", Title: "Hourly Gardener", Status: "closed"},
		},
		`bead.status == "open"`,
		"status=closed",
	)

	_, err := engine.createTaskBead(context.Background(), trig, match)
	if err != nil {
		t.Fatalf("createTaskBead error: %v", err)
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(daemon.createdBeads))
	}

	req := daemon.createdBeads[0]
	if req.Type != "bug" {
		t.Errorf("expected Type=bug, got %q", req.Type)
	}
	fieldsStr := string(req.Fields)
	for _, want := range []string{
		"bead_event_key",
		"source_bead_id", "gd-abc",
		"source_event", "updated",
		"assert_violated",
		"assert_values", "status=closed",
	} {
		if !trigPermContains(fieldsStr, want) {
			t.Errorf("expected %q in bead fields JSON, got %s", want, fieldsStr)
		}
	}
}

// TestCreateTaskBead_BeadTypeOverride verifies that a trigger carrying a
// bead_type override (plus priority, parent, and extra_labels passthrough)
// files a bead of the requested type with the passed priority, a parent-child
// dep to the configured parent, and the extra labels. See ka-0000003075.
func TestCreateTaskBead_BeadTypeOverride(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	prio := 0
	trig := TriggerDef{
		ID:           "t-bug",
		Title:        "Bug Filer",
		Source:       "webhook",
		Project:      "gardenio",
		BeadType:     "bug",
		BeadPriority: &prio,
		ParentBead:   "gd-Epic1",
		ExtraLabels:  []string{"kind:slack-feedback"},
	}
	match := TriggerMatch{
		Key:    "hook:9",
		Title:  "Something broke",
		Labels: []string{"source:webhook"},
	}

	beadID, err := engine.createTaskBead(context.Background(), trig, match)
	if err != nil {
		t.Fatalf("createTaskBead error: %v", err)
	}

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	req := daemon.createdBeads[0]
	if req.Type != "bug" {
		t.Errorf("Type = %q, want bug", req.Type)
	}
	if req.Priority != 0 {
		t.Errorf("Priority = %d, want 0", req.Priority)
	}
	if !slices.Contains(req.Labels, "kind:slack-feedback") {
		t.Errorf("expected extra label kind:slack-feedback, got %v", req.Labels)
	}

	// Parent-child dep: filed bead → parent epic.
	var foundDep bool
	for _, d := range daemon.addedDeps {
		if d.BeadID == beadID && d.DependsOnID == "gd-Epic1" && d.DepType == "parent-child" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Errorf("expected parent-child dep %s → gd-Epic1, got %+v", beadID, daemon.addedDeps)
	}
}

// TestCreateTaskBead_InvalidBeadType verifies that a bead_type outside the
// allowlist is rejected with a clear error and no bead is created.
func TestCreateTaskBead_InvalidBeadType(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:       "t-bad",
		Title:    "Bad Type",
		Source:   "webhook",
		BeadType: "epic", // not in the allowlist
	}
	match := TriggerMatch{Key: "hook:10", Title: "nope"}

	_, err := engine.createTaskBead(context.Background(), trig, match)
	if err == nil {
		t.Fatal("expected error for invalid bead_type, got nil")
	}
	if !trigPermContains(err.Error(), "invalid bead_type") {
		t.Errorf("error %q does not mention invalid bead_type", err.Error())
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected no beads created on validation failure, got %d", len(daemon.createdBeads))
	}
}

// TestCreateTaskBead_DefaultsToTask verifies the unchanged default path: a
// trigger with no bead_type still files a type=task bead.
func TestCreateTaskBead_DefaultsToTask(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{ID: "t-def", Title: "Default", Source: "webhook"}
	match := TriggerMatch{Key: "hook:11", Title: "x"}

	if _, err := engine.createTaskBead(context.Background(), trig, match); err != nil {
		t.Fatalf("createTaskBead error: %v", err)
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Type != "task" {
		t.Errorf("Type = %q, want task", daemon.createdBeads[0].Type)
	}
	if len(daemon.addedDeps) != 0 {
		t.Errorf("expected no deps when ParentBead empty, got %+v", daemon.addedDeps)
	}
}

// --- Fire-time spawn authz recheck (against creator user-bead scopes) ---

// userBead builds a type=user config bead for use in mockTriggerDaemon.beads.
func userBead(id, email string, allowedProjects, allowedRoles string) *beadsapi.BeadDetail {
	fields := map[string]string{"email": email}
	if allowedProjects != "" {
		fields["allowed_projects"] = allowedProjects
	}
	if allowedRoles != "" {
		fields["allowed_roles"] = allowedRoles
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Title:  email,
		Fields: fields,
	}
}

func TestRecheckSpawnAuthzAtFire_NonSpawnAction(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionNotify,
		CreatedBy: "user@example.com",
		Project:   "gardenio",
		Role:      "admin",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected non-spawn action to be allowed, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_SystemTrigger(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:      "t1",
		Action:  TriggerActionSpawnAgent,
		Project: "gardenio",
		Role:    "admin",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected system trigger (no creator) to be allowed, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_StaticCreatorBypass(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionSpawnAgent,
		CreatedBy: "static", // not an OIDC email
		Project:   "gardenio",
		Role:      "admin",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected non-OIDC creator to bypass, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_DeprovisionedCreatorDenied(t *testing.T) {
	// Creator looks like an OIDC identity but no user bead exists → deny.
	daemon := &mockTriggerDaemon{beads: nil}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionSpawnAgent,
		CreatedBy: "ghost@example.com",
		Project:   "gardenio",
		Role:      "crew",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if got.Allowed {
		t.Fatal("expected missing user bead to deny")
	}
	if !trigPermContains(got.Reason, "deprovisioned") {
		t.Errorf("expected deprovisioned reason, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_OperatorDeniedForAdminRole(t *testing.T) {
	// Operator's user bead allows only "crew" / "gardener" — bead-advisory
	// AllowedRoles=["admin"] must NOT authorize; fire-time recheck denies.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio"]`, `["crew","gardener"]`),
		},
	}
	trig := TriggerDef{
		ID:              "t1",
		Action:          TriggerActionSpawnAgent,
		CreatedBy:       "operator@example.com",
		Project:         "gardenio",
		Role:            "admin",
		AllowedProjects: []string{"*"}, // advisory fields on bead — client-supplied, must be ignored
		AllowedRoles:    []string{"admin"},
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if got.Allowed {
		t.Fatal("expected operator to be denied admin role at fire time")
	}
	if !trigPermContains(got.Reason, "role") {
		t.Errorf("expected role-denial reason, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_CrossProjectDenied(t *testing.T) {
	// Operator scoped to gardenio cannot spawn into kbeads.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio"]`, `["crew"]`),
		},
	}
	trig := TriggerDef{
		ID:              "t1",
		Action:          TriggerActionSpawnAgent,
		CreatedBy:       "operator@example.com",
		Project:         "kbeads",
		Role:            "crew",
		AllowedProjects: []string{"*"}, // advisory — must be ignored
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if got.Allowed {
		t.Fatal("expected cross-project spawn to be denied")
	}
	if !trigPermContains(got.Reason, "project") {
		t.Errorf("expected project-denial reason, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_AllowedWithinScope(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio","kbeads"]`, `["crew","gardener"]`),
		},
	}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionSpawnAgent,
		CreatedBy: "operator@example.com",
		Project:   "gardenio",
		Role:      "crew",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected in-scope spawn to be allowed, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_WildcardUserBeadAllows(t *testing.T) {
	// Admin-like user with wildcard scopes.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "admin@example.com", `["*"]`, `["*"]`),
		},
	}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionSpawnAgent,
		CreatedBy: "admin@example.com",
		Project:   "any-project",
		Role:      "admin",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected wildcard scopes to allow, got: %s", got.Reason)
	}
}

func TestRecheckSpawnAuthzAtFire_EmptyUserBeadScopesAllows(t *testing.T) {
	// Empty allowed_projects / allowed_roles means unrestricted (matches
	// kbeads authorizeAgentSpawn: len==0 → skip check).
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "power@example.com", "", ""),
		},
	}
	trig := TriggerDef{
		ID:        "t1",
		Action:    TriggerActionSpawnAgent,
		CreatedBy: "power@example.com",
		Project:   "gardenio",
		Role:      "admin",
	}
	got := recheckSpawnAuthzAtFire(context.Background(), daemon, trig)
	if !got.Allowed {
		t.Fatalf("expected empty-scope user bead to be unrestricted, got: %s", got.Reason)
	}
}

// TestTriggerEngine_SpawnDeniedAtFireTime is the integration test covering the
// full fireAction path: an operator's user bead forbids admin role, but the
// trigger was crafted with AllowedRoles=["admin"]. fireAction must re-check
// against the creator's user bead and deny + audit.
func TestTriggerEngine_SpawnDeniedAtFireTime_AdminRoleRejected(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio"]`, `["crew"]`),
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:              "t1",
		Title:           "Rogue spawn trigger",
		Source:          "webhook",
		Action:          TriggerActionSpawnAgent,
		Project:         "gardenio",
		Role:            "admin",
		CreatedBy:       "operator@example.com",
		AllowedProjects: []string{"*"}, // advisory — operator tried to self-grant
		AllowedRoles:    []string{"*"},
		MaxConcurrent:   1,
		Enabled:         true,
	}
	match := TriggerMatch{Key: "hook:1", Title: "hook"}

	_, err := engine.fireAction(context.Background(), trig, match)
	if err == nil {
		t.Fatal("expected fire-time denial")
	}
	if !trigPermContains(err.Error(), "denied") {
		t.Errorf("expected denied error, got: %v", err)
	}
	// No agent should have been spawned.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no agent spawned, got: %v", daemon.spawnedNames)
	}
	// Denial audit bead must be recorded.
	var denied bool
	for _, b := range daemon.createdBeads {
		if !trigPermHasLabel(b.Labels, "auto:trigger-audit") {
			continue
		}
		if trigPermContains(string(b.Fields), `"outcome":"deny"`) {
			denied = true
		}
	}
	if !denied {
		t.Error("expected audit bead with outcome=deny")
	}
}

func TestTriggerEngine_SpawnDeniedAtFireTime_CrossProjectRejected(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio"]`, `["crew"]`),
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Cross-project spawn",
		Source:        "webhook",
		Action:        TriggerActionSpawnAgent,
		Project:       "kbeads", // operator's user bead only permits gardenio
		Role:          "crew",
		CreatedBy:     "operator@example.com",
		MaxConcurrent: 1,
		Enabled:       true,
	}
	match := TriggerMatch{Key: "hook:2", Title: "hook"}

	_, err := engine.fireAction(context.Background(), trig, match)
	if err == nil {
		t.Fatal("expected cross-project spawn to be denied at fire time")
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no agent spawned, got: %v", daemon.spawnedNames)
	}
}

// TestTriggerEngine_SpawnAllowed_WithinCreatorScopes confirms an in-scope
// spawn still succeeds end-to-end.
func TestTriggerEngine_SpawnAllowed_WithinCreatorScopes(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			userBead("gd-u1", "operator@example.com", `["gardenio"]`, `["crew","gardener"]`),
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Legit spawn",
		Source:        "webhook",
		Action:        TriggerActionSpawnAgent,
		Project:       "gardenio",
		Role:          "crew",
		CreatedBy:     "operator@example.com",
		MaxConcurrent: 1,
		Enabled:       true,
		Prompt:        "do a thing",
	}
	match := TriggerMatch{Key: "hook:3", Title: "hook"}

	if _, err := engine.fireAction(context.Background(), trig, match); err != nil {
		t.Fatalf("expected in-scope spawn to succeed, got: %v", err)
	}
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("expected 1 agent spawned, got: %v", daemon.spawnedNames)
	}
}

// --- helpers (prefixed to avoid collisions with other test files) ---

func trigPermHasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

func trigPermContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
