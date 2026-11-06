package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- Test helpers ---

func newAccountSource() *AccountTriggerSource {
	return NewAccountTriggerSource(slog.Default())
}

func accountTriggerBead(id, title, condType, field, op, threshold, accountFilter, action, prompt string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"condition_type": condType,
		"field":          field,
		"op":             op,
		"threshold":      threshold,
		"account_filter": accountFilter,
		"action":         action,
		"prompt":         prompt,
		"enabled":        "true",
		// A spawn_agent-action fixture needs a project or the fire-time
		// project guard (D6) refuses the spawn; notify-action fixtures don't
		// consume this field, so setting it unconditionally is harmless.
		"project": "gardenio",
		// Defaults last_checked to 1 hour ago so engine runtime-bootstrap does not
		// suppress first-evaluation firing in these tests.
		"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Title:  title,
		Status: "open",
		Fields: fields,
		Labels: []string{"trigger:account"},
	}
}

func accountBead(id, name, provider string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"provider": provider}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "account",
		Title:  name,
		Status: "open",
		Fields: f,
	}
}

// --- Name test ---

func TestAccountTriggerSource_Name(t *testing.T) {
	s := newAccountSource()
	if got := s.Name(); got != "account" {
		t.Errorf("Name() = %q, want %q", got, "account")
	}
}

// --- LoadTriggers tests ---

func TestAccountTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Spend alert", "spend_threshold", "total_cost_usd", ">=", "100", "", "notify", "Spend exceeded"),
			accountTriggerBead("t2", "No condition", "", "", "", "", "", "notify", ""),
			func() *beadsapi.BeadDetail {
				b := accountTriggerBead("t3", "Disabled", "spend_threshold", "", ">=", "50", "", "notify", "")
				b.Fields["enabled"] = "false"
				return b
			}(),
		},
	}

	source := newAccountSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	// t1 loaded, t2 skipped (no condition_type), t3 skipped (disabled)
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	data := triggers[0].SourceData.(accountTriggerData)
	if data.ConditionType != "spend_threshold" {
		t.Errorf("ConditionType = %q, want %q", data.ConditionType, "spend_threshold")
	}
	if data.Threshold != 100 {
		t.Errorf("Threshold = %f, want 100", data.Threshold)
	}
}

func TestAccountTriggerSource_LoadTriggers_InvalidThreshold(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Bad threshold", "spend_threshold", "", ">=", "not_a_number", "", "notify", ""),
		},
	}
	source := newAccountSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	if len(triggers) != 0 {
		t.Fatalf("expected 0 triggers (invalid threshold), got %d", len(triggers))
	}
}

func TestAccountTriggerSource_LoadTriggers_DefaultOp(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Default op", "spend_threshold", "", "", "50", "", "notify", ""),
		},
	}
	source := newAccountSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	data := triggers[0].SourceData.(accountTriggerData)
	if data.Op != ">=" {
		t.Errorf("Op = %q, want %q (default)", data.Op, ">=")
	}
}

// --- Evaluate: spend_threshold ---

func TestAccountTriggerSource_Evaluate_SpendThreshold_Fires(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Spend alert", "spend_threshold", "total_cost_usd", ">=", "100", "", "notify", "Over budget"),
			accountBead("acct1", "primary-account", "anthropic", map[string]string{
				"total_cost_usd": "150.50",
			}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:    "t1",
		Title: "Spend alert",
		SourceData: accountTriggerData{
			ConditionType: "spend_threshold",
			Field:         "total_cost_usd",
			Op:            ">=",
			Threshold:     100,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Key != "spend:primary-account:total_cost_usd:100" {
		t.Errorf("Key = %q, want %q", matches[0].Key, "spend:primary-account:total_cost_usd:100")
	}
	if matches[0].TemplateVars["{{account_name}}"] != "primary-account" {
		t.Errorf("account_name = %q", matches[0].TemplateVars["{{account_name}}"])
	}
}

func TestAccountTriggerSource_Evaluate_SpendThreshold_BelowThreshold(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary-account", "anthropic", map[string]string{
				"total_cost_usd": "50.00",
			}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "spend_threshold",
			Field:         "total_cost_usd",
			Op:            ">=",
			Threshold:     100,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (below threshold), got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_SpendThreshold_AccountFilter(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"total_cost_usd": "200"}),
			accountBead("acct2", "secondary", "anthropic", map[string]string{"total_cost_usd": "200"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "spend_threshold",
			Field:         "total_cost_usd",
			Op:            ">=",
			Threshold:     100,
			AccountFilter: "primary",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (filtered to primary only), got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_SpendThreshold_DefaultField(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"total_cost_usd": "200"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "spend_threshold",
			Field:         "", // should default to total_cost_usd
			Op:            ">=",
			Threshold:     100,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (default field), got %d", len(matches))
	}
}

// --- Evaluate: utilization_threshold ---

func TestAccountTriggerSource_Evaluate_UtilizationThreshold(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"utilization_5h": "0.92"}),
			accountBead("acct2", "secondary", "anthropic", map[string]string{"utilization_5h": "0.40"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "utilization_threshold",
			Field:         "utilization_5h",
			Op:            ">=",
			Threshold:     0.85,
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (primary at 0.92), got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_UtilizationThreshold_DefaultField(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"utilization_5h": "0.90"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "utilization_threshold",
			Field:         "", // defaults to utilization_5h
			Op:            ">=",
			Threshold:     0.80,
		},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

// --- Evaluate: all_rate_limited ---

func TestAccountTriggerSource_Evaluate_AllRateLimited_AllRejected(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"throttle_status": "rejected"}),
			accountBead("acct2", "secondary", "anthropic", map[string]string{"throttle_status": "rejected"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID: "t1",
		SourceData: accountTriggerData{
			ConditionType: "all_rate_limited",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].TemplateVars["{{account_count}}"] != "2" {
		t.Errorf("account_count = %q, want %q", matches[0].TemplateVars["{{account_count}}"], "2")
	}
}

func TestAccountTriggerSource_Evaluate_AllRateLimited_SomeAllowed(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{"throttle_status": "rejected"}),
			accountBead("acct2", "secondary", "anthropic", map[string]string{"throttle_status": "allowed"}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "all_rate_limited"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (not all rate-limited), got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_AllRateLimited_NoAccounts(t *testing.T) {
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{}}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "all_rate_limited"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (no accounts), got %d", len(matches))
	}
}

// --- Evaluate: account_recovered ---

func TestAccountTriggerSource_Evaluate_AccountRecovered(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{
				"throttle_status": "allowed",
				"rotation_count":  "3",
			}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "account_recovered"},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Key != "recovered:primary" {
		t.Errorf("Key = %q, want %q", matches[0].Key, "recovered:primary")
	}
}

func TestAccountTriggerSource_Evaluate_AccountRecovered_NeverRateLimited(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{
				"throttle_status": "allowed",
				// no rotation_count or exhausted_at → never been rate-limited
			}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "account_recovered"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (never rate-limited), got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_AccountRecovered_StillRejected(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", map[string]string{
				"throttle_status": "rejected",
				"rotation_count":  "1",
			}),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "account_recovered"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (still rejected), got %d", len(matches))
	}
}

// --- Evaluate: bad source data ---

func TestAccountTriggerSource_Evaluate_BadSourceData(t *testing.T) {
	source := newAccountSource()
	trig := TriggerDef{ID: "t1", SourceData: "not account data"}
	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestAccountTriggerSource_Evaluate_UnknownConditionType(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountBead("acct1", "primary", "anthropic", nil),
		},
	}

	source := newAccountSource()
	source.mu.Lock()
	source.daemon = daemon
	source.mu.Unlock()

	trig := TriggerDef{
		ID:         "t1",
		SourceData: accountTriggerData{ConditionType: "unknown_type"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for unknown type, got %d", len(matches))
	}
}

// --- CatchUpKeys ---

func TestAccountTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "b1",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{"account_event_key": "spend:primary:total_cost_usd:100"},
				Labels: []string{"trigger:t1"},
			},
			{
				ID:     "b2",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{},
				Labels: []string{"trigger:t1"},
			},
		},
	}

	source := newAccountSource()
	keys, err := source.CatchUpKeys(context.Background(), daemon, "t1")
	if err != nil {
		t.Fatalf("CatchUpKeys error: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
}

// --- compareFloat ---

func TestCompareFloat(t *testing.T) {
	tests := []struct {
		val       float64
		op        string
		threshold float64
		want      bool
	}{
		{100.0, ">", 50.0, true},
		{50.0, ">", 100.0, false},
		{100.0, ">=", 100.0, true},
		{99.0, ">=", 100.0, false},
		{50.0, "<", 100.0, true},
		{100.0, "<", 100.0, false},
		{100.0, "<=", 100.0, true},
		{101.0, "<=", 100.0, false},
		{100.0, "==", 100.0, true},
		{100.0, "==", 99.0, false},
		{100.0, "invalid", 100.0, true}, // defaults to >=
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%.0f_%s_%.0f", tt.val, tt.op, tt.threshold)
		t.Run(name, func(t *testing.T) {
			got := compareFloat(tt.val, tt.op, tt.threshold)
			if got != tt.want {
				t.Errorf("compareFloat(%.1f, %q, %.1f) = %v, want %v",
					tt.val, tt.op, tt.threshold, got, tt.want)
			}
		})
	}
}

// --- Engine integration tests ---

func TestAccountTriggerSource_EngineIntegration_Notify(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Spend alert", "spend_threshold", "total_cost_usd", ">=", "100", "", "notify", "Account {{account_name}} over budget"),
			accountBead("acct1", "primary", "anthropic", map[string]string{"total_cost_usd": "200"}),
		},
	}

	source := newAccountSource()
	var notified bool
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) { notified = true }
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())

	if !notified {
		t.Error("expected notification to fire")
	}
}

// TestAccountTriggerSource_SpawnedTask_DependsOnTriggerBead drives an account
// threshold breach → engine → actionSpawnAgent path and asserts the spawned
// tracking task carries a `triggered-by` dep to the trigger definition bead.
// Auto-link M2a (ka-0000007272, epic ka-0000000933).
func TestAccountTriggerSource_SpawnedTask_DependsOnTriggerBead(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("acct-m2a", "Spend remediation", "spend_threshold", "total_cost_usd", ">=", "100", "", "spawn_agent", "Investigate {{account_name}}"),
			accountBead("acct1", "primary", "anthropic", map[string]string{"total_cost_usd": "200"}),
		},
	}

	source := newAccountSource()
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}

	var sawTriggerDep bool
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == "acct-m2a" {
			sawTriggerDep = true
		}
	}
	if !sawTriggerDep {
		t.Errorf("missing triggered-by dep to trigger bead acct-m2a (deps=%+v)", daemon.addedDeps)
	}
}

func TestAccountTriggerSource_EngineIntegration_Dedup(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Spend alert", "spend_threshold", "total_cost_usd", ">=", "100", "", "notify", "Over budget"),
			accountBead("acct1", "primary", "anthropic", map[string]string{"total_cost_usd": "200"}),
		},
	}

	source := newAccountSource()
	notifyCount := 0
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) { notifyCount++ }
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())
	engine.Evaluate(context.Background())

	if notifyCount != 1 {
		t.Errorf("expected 1 notify (deduped), got %d", notifyCount)
	}
}

func TestAccountTriggerSource_EngineIntegration_CatchUp(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			accountTriggerBead("t1", "Spend alert", "spend_threshold", "total_cost_usd", ">=", "100", "", "notify", "Over budget"),
			{
				ID:     "existing1",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{"account_event_key": "spend:primary:total_cost_usd:100"},
				Labels: []string{"trigger:t1"},
			},
		},
	}

	source := newAccountSource()
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)

	engine.CatchUp(context.Background())

	seen := engine.SeenKeys("t1")
	if !seen["spend:primary:total_cost_usd:100"] {
		t.Error("expected catch-up key in seen set")
	}
}
