package beadsapi

import (
	"encoding/json"
	"testing"
)

// TestResolveGardenerID covers the four-tier priority order that guarantees
// every agent gets a non-empty owner (task ka-0000006626, design memo §2.3).
func TestResolveGardenerID(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "tier1 explicit gardener_id wins over everything",
			fields: map[string]string{"gardener_id": "gd-explicit", "schedule_id": "gd-sched", "spawn_source": "slack-thread"},
			want:   "gd-explicit",
		},
		{
			name:   "tier1 trims whitespace-padded explicit id",
			fields: map[string]string{"gardener_id": "  gd-explicit  "},
			want:   "gd-explicit",
		},
		{
			name:   "tier2 schedule_id is the real gardener",
			fields: map[string]string{"schedule_id": "gd-gardener-loop", "trigger_id": "gd-gardener-loop", "spawn_source": "schedule"},
			want:   "gd-gardener-loop",
		},
		{
			name:   "tier2 schedule_id takes precedence over trigger_id when they differ",
			fields: map[string]string{"schedule_id": "gd-sched", "trigger_id": "gd-trig"},
			want:   "gd-sched",
		},
		{
			name:   "tier2 trigger_id used when schedule_id absent",
			fields: map[string]string{"trigger_id": "gd-webhook-trigger", "spawn_source": ""},
			want:   "gd-webhook-trigger",
		},
		{
			name:   "tier3 slack-thread synthetic gardener",
			fields: map[string]string{"spawn_source": "slack-thread"},
			want:   "synthetic-gardener:slack-thread",
		},
		{
			name:   "tier3 gitlab-mr-review synthetic gardener",
			fields: map[string]string{"spawn_source": "gitlab-mr-review"},
			want:   "synthetic-gardener:gitlab-mr-review",
		},
		{
			name:   "tier3 jira-mention synthetic gardener",
			fields: map[string]string{"spawn_source": "jira-mention"},
			want:   "synthetic-gardener:jira-mention",
		},
		{
			name:   "tier4 no gardener and no spawn_source falls back to unknown",
			fields: map[string]string{"agent": "orphan-bot", "role": "crew"},
			want:   "synthetic-gardener:unknown",
		},
		{
			name:   "tier4 empty field map still owned",
			fields: map[string]string{},
			want:   "synthetic-gardener:unknown",
		},
		{
			name:   "whitespace-only spawn_source is treated as absent (falls to tier4)",
			fields: map[string]string{"spawn_source": "   "},
			want:   "synthetic-gardener:unknown",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveGardenerID(tc.fields); got != tc.want {
				t.Errorf("resolveGardenerID(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

// TestResolveGardenerID_NeverEmpty is the load-bearing invariant: no reachable
// field combination yields an empty owner, so the "Unowned" bucket is empty by
// construction.
func TestResolveGardenerID_NeverEmpty(t *testing.T) {
	for _, fields := range []map[string]string{
		nil,
		{},
		{"spawn_source": ""},
		{"gardener_id": "", "schedule_id": "", "trigger_id": "", "spawn_source": ""},
		{"role": "crew"},
	} {
		if got := resolveGardenerID(fields); got == "" {
			t.Errorf("resolveGardenerID(%v) returned empty owner — Unowned invariant violated", fields)
		}
	}
}

// TestStampGardenerID_Idempotent verifies the map-mutating helper only writes
// when gardener_id is absent, and never overwrites a present value.
func TestStampGardenerID_Idempotent(t *testing.T) {
	// Present value: no write, returns false.
	fields := map[string]string{"gardener_id": "gd-existing", "spawn_source": "slack-thread"}
	if wrote := stampGardenerID(fields); wrote {
		t.Errorf("stampGardenerID reported a write over an existing gardener_id")
	}
	if fields["gardener_id"] != "gd-existing" {
		t.Errorf("stampGardenerID clobbered existing gardener_id = %q", fields["gardener_id"])
	}

	// Absent value: writes the resolved synthetic id, returns true.
	fields = map[string]string{"spawn_source": "jira-mention"}
	if wrote := stampGardenerID(fields); !wrote {
		t.Errorf("stampGardenerID reported no write when gardener_id was absent")
	}
	if got, want := fields["gardener_id"], "synthetic-gardener:jira-mention"; got != want {
		t.Errorf("stampGardenerID wrote gardener_id = %q, want %q", got, want)
	}

	// Nil map: no panic, returns false.
	if wrote := stampGardenerID(nil); wrote {
		t.Errorf("stampGardenerID(nil) reported a write")
	}
}

// TestStampAgentGardenerID_TypeGated verifies the create-choke-point wrapper only
// stamps agent beads and passes every other type through untouched.
func TestStampAgentGardenerID_TypeGated(t *testing.T) {
	raw := json.RawMessage(`{"agent":"bot","spawn_source":"slack-thread"}`)

	// Non-agent type: passthrough, byte-identical.
	for _, typ := range []string{"task", "epic", "message", "trigger", ""} {
		got := stampAgentGardenerID(typ, raw)
		if string(got) != string(raw) {
			t.Errorf("stampAgentGardenerID(%q, ...) mutated non-agent fields: got %s", typ, got)
		}
	}

	// Agent type: gardener_id stamped.
	got := stampAgentGardenerID("agent", raw)
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("stamped agent fields not valid JSON: %v", err)
	}
	if m["gardener_id"] != "synthetic-gardener:slack-thread" {
		t.Errorf("agent gardener_id = %q, want synthetic-gardener:slack-thread", m["gardener_id"])
	}
	// Original fields preserved.
	if m["agent"] != "bot" || m["spawn_source"] != "slack-thread" {
		t.Errorf("stamp dropped existing fields: %v", m)
	}
}

// TestStampAgentGardenerID_RealGardenerWins confirms a gardener-spawned agent is
// owned by its real gardener (trigger) id, not a synthetic one.
func TestStampAgentGardenerID_RealGardenerWins(t *testing.T) {
	raw := json.RawMessage(`{"agent":"crew","schedule_id":"gd-cap","spawn_source":"schedule"}`)
	got := stampAgentGardenerID("agent", raw)
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["gardener_id"] != "gd-cap" {
		t.Errorf("gardener_id = %q, want gd-cap (real gardener, not synthetic)", m["gardener_id"])
	}
}

// TestStampAgentGardenerID_PreservesExplicit confirms an inherited/explicit
// gardener_id (e.g. from the successor path) survives the choke point untouched.
func TestStampAgentGardenerID_PreservesExplicit(t *testing.T) {
	raw := json.RawMessage(`{"agent":"successor","gardener_id":"gd-inherited","spawn_source":"successor"}`)
	got := stampAgentGardenerID("agent", raw)
	if string(got) != string(raw) {
		var m map[string]string
		_ = json.Unmarshal(got, &m)
		if m["gardener_id"] != "gd-inherited" {
			t.Errorf("explicit gardener_id changed to %q, want gd-inherited", m["gardener_id"])
		}
	}
}

// TestStampAgentGardenerID_EmptyFields covers an agent create with no fields
// payload: the wrapper still stamps a (tier-4) owner so the bead is never
// unowned.
func TestStampAgentGardenerID_EmptyFields(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage(`{}`)} {
		got := stampAgentGardenerID("agent", raw)
		var m map[string]string
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("stamped fields (%q) not valid JSON: %v", raw, err)
		}
		if m["gardener_id"] != syntheticGardenerUnknown {
			t.Errorf("empty-fields agent gardener_id = %q, want %q", m["gardener_id"], syntheticGardenerUnknown)
		}
	}
}

// TestStampAgentGardenerID_MalformedFieldsPassthrough verifies the fail-open
// posture: a fields payload that is valid JSON but not a string map is passed
// through unchanged rather than dropped, so a spawn can never wedge.
func TestStampAgentGardenerID_MalformedFieldsPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"agent":123}`) // number, not a string — unmarshal into map[string]string fails
	got := stampAgentGardenerID("agent", raw)
	if string(got) != string(raw) {
		t.Errorf("malformed fields payload was rewritten: got %s, want passthrough %s", got, raw)
	}
}

// TestDeriveGardenerID verifies the exported read-only projection mirrors
// resolveGardenerID exactly across the four-tier priority order (task
// ka-0000002015). It must be pure and total — always non-empty.
func TestDeriveGardenerID(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "tier1 explicit gardener_id",
			fields: map[string]string{"gardener_id": "gd-explicit", "schedule_id": "gd-sched"},
			want:   "gd-explicit",
		},
		{
			name:   "tier2 schedule_id legacy fallback",
			fields: map[string]string{"schedule_id": "gd-gardener-loop"},
			want:   "gd-gardener-loop",
		},
		{
			name:   "tier2 trigger_id legacy fallback",
			fields: map[string]string{"trigger_id": "gd-trigger"},
			want:   "gd-trigger",
		},
		{
			name:   "tier3 synthetic per-source",
			fields: map[string]string{"spawn_source": "slack-thread"},
			want:   syntheticGardenerPrefix + "slack-thread",
		},
		{
			name:   "tier4 unknown synthetic",
			fields: map[string]string{},
			want:   syntheticGardenerUnknown,
		},
		{
			name:   "nil fields resolve to unknown synthetic",
			fields: nil,
			want:   syntheticGardenerUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveGardenerID(tt.fields); got != tt.want {
				t.Errorf("DeriveGardenerID(%v) = %q, want %q", tt.fields, got, tt.want)
			}
		})
	}
}

// TestDeriveGardenerID_DoesNotMutate confirms the projection is read-only: it
// must never write a gardener_id back into the caller's field map.
func TestDeriveGardenerID_DoesNotMutate(t *testing.T) {
	fields := map[string]string{"spawn_source": "manual"}
	_ = DeriveGardenerID(fields)
	if _, ok := fields["gardener_id"]; ok {
		t.Errorf("DeriveGardenerID mutated caller fields: %v", fields)
	}
}

// TestIsSyntheticGardenerID covers the --no-gardener leak predicate: synthetic
// and empty ids are "no real gardener"; a real gd- id is not.
func TestIsSyntheticGardenerID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{syntheticGardenerUnknown, true},
		{syntheticGardenerPrefix + "slack-thread", true},
		{"  " + syntheticGardenerPrefix + "manual  ", true},
		{"", true},
		{"   ", true},
		{"gd-realgardener", false},
		{"  gd-realgardener  ", false},
	}
	for _, tt := range tests {
		if got := IsSyntheticGardenerID(tt.id); got != tt.want {
			t.Errorf("IsSyntheticGardenerID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
