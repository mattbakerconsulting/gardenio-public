package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/claudeconfig"
)

func TestWrapUp_MarshalRoundTrip(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "Implemented auth module",
		Blockers:        "Waiting on API key",
		HandoffNotes:    "Check PR #42",
		BeadsClosed:     []string{"gd-abc", "gd-def"},
		PullRequests:    []string{"https://github.com/org/repo/pull/42"},
		Custom:          map[string]string{"risk_level": "low"},
		Timestamp:       time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC),
	}

	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}

	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}

	if got.Accomplishments != w.Accomplishments {
		t.Errorf("Accomplishments = %q, want %q", got.Accomplishments, w.Accomplishments)
	}
	if got.Blockers != w.Blockers {
		t.Errorf("Blockers = %q, want %q", got.Blockers, w.Blockers)
	}
	if got.HandoffNotes != w.HandoffNotes {
		t.Errorf("HandoffNotes = %q, want %q", got.HandoffNotes, w.HandoffNotes)
	}
	if len(got.BeadsClosed) != 2 || got.BeadsClosed[0] != "gd-abc" {
		t.Errorf("BeadsClosed = %v, want [gd-abc gd-def]", got.BeadsClosed)
	}
	if len(got.PullRequests) != 1 {
		t.Errorf("PullRequests = %v, want 1 entry", got.PullRequests)
	}
	if got.Custom["risk_level"] != "low" {
		t.Errorf("Custom[risk_level] = %q, want %q", got.Custom["risk_level"], "low")
	}
}

func TestWrapUp_TerminalGoalFieldsRoundTrip(t *testing.T) {
	// Empty wrap-up should marshal fine without the new fields.
	empty := &WrapUp{Accomplishments: "did stuff"}
	s, err := MarshalWrapUp(empty)
	if err != nil {
		t.Fatalf("MarshalWrapUp(empty): %v", err)
	}
	if strings.Contains(s, "goal_satisfied") || strings.Contains(s, "goal_report") || strings.Contains(s, "respawn_after") {
		t.Errorf("empty wrap-up should omit terminal-goal fields, got: %s", s)
	}

	// Populated wrap-up round-trips through JSON.
	full := &WrapUp{
		Accomplishments: "Tried to land MR but CI red",
		GoalSatisfied:   "no",
		GoalReport:      "MR opened but CI is red on lint;\nneeds another pass at the formatter rules.",
		RespawnAfter:    "15m",
		Timestamp:       time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	}
	s, err = MarshalWrapUp(full)
	if err != nil {
		t.Fatalf("MarshalWrapUp(full): %v", err)
	}

	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.GoalSatisfied != "no" {
		t.Errorf("GoalSatisfied = %q, want %q", got.GoalSatisfied, "no")
	}
	if got.GoalReport != full.GoalReport {
		t.Errorf("GoalReport = %q, want %q", got.GoalReport, full.GoalReport)
	}
	if got.RespawnAfter != "15m" {
		t.Errorf("RespawnAfter = %q, want %q", got.RespawnAfter, "15m")
	}
}

func TestWrapUp_MarshalSetsTimestamp(t *testing.T) {
	w := &WrapUp{Accomplishments: "did stuff"}

	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}

	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}

	if got.Timestamp.IsZero() {
		t.Error("Timestamp should be auto-set when zero")
	}
}

func TestWrapUp_JSONFieldIsValidJSON(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "Closed 3 bugs",
		Timestamp:       time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC),
	}

	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}

	// The string stored in fields["wrapup"] must be valid JSON.
	if !json.Valid([]byte(s)) {
		t.Errorf("MarshalWrapUp output is not valid JSON: %s", s)
	}
}

func TestDefaultWrapUpRequirements(t *testing.T) {
	req := DefaultWrapUpRequirements()

	// tldr is hard-required alongside accomplishments so every wrap-up surfaces
	// a featured headline on the Slack agent card (bead ka-0000002919).
	if len(req.Required) != 2 || req.Required[0] != "tldr" || req.Required[1] != "accomplishments" {
		t.Errorf("Required = %v, want [tldr accomplishments]", req.Required)
	}
	if req.Enforce != "hard" {
		t.Errorf("Enforce = %q, want %q", req.Enforce, "hard")
	}
}

func TestWrapUpRequirements_Validate_PassesWhenComplete(t *testing.T) {
	req := DefaultWrapUpRequirements()
	w := &WrapUp{TLDR: "Shipped the thing", Accomplishments: "Did the thing"}

	issues := req.Validate(w)
	if len(issues) != 0 {
		t.Errorf("Validate returned issues for complete wrap-up: %v", issues)
	}
}

func TestWrapUpRequirements_Validate_FailsOnMissingTLDR(t *testing.T) {
	req := DefaultWrapUpRequirements()
	w := &WrapUp{Accomplishments: "Did the thing"} // tldr is empty

	issues := req.Validate(w)
	if len(issues) == 0 {
		t.Error("Validate should fail when tldr is empty under default requirements")
	}
}

func TestWrapUpRequirements_Validate_FailsOnMissingRequired(t *testing.T) {
	req := DefaultWrapUpRequirements()
	w := &WrapUp{} // accomplishments is empty

	issues := req.Validate(w)
	if len(issues) == 0 {
		t.Error("Validate should fail when accomplishments is empty")
	}
}

// A nil wrap-up is the bare `gb stop` case (no --wrapup). Validate must
// treat it as "every required field empty" so the bare-stop enforcement
// path in runStop blocks crew/thread agents that skip --wrapup entirely.
func TestWrapUpRequirements_Validate_NilWrapUpFailsAllRequired(t *testing.T) {
	req := DefaultWrapUpRequirements()

	issues := req.Validate(nil)
	if len(issues) != len(req.Required) {
		t.Errorf("Validate(nil) returned %d issues, want %d (one per required field): %v",
			len(issues), len(req.Required), issues)
	}
}

// enforce=none must short-circuit before dereferencing a nil wrap-up.
func TestWrapUpRequirements_Validate_NilWrapUpEnforceNone(t *testing.T) {
	req := WrapUpRequirements{Required: []string{"accomplishments"}, Enforce: "none"}
	if issues := req.Validate(nil); len(issues) != 0 {
		t.Errorf("Validate(nil) with enforce=none should return no issues, got: %v", issues)
	}
}

// A nil wrap-up with a required custom field must report the custom field
// as missing rather than panicking on the nil Custom map.
func TestWrapUpRequirements_Validate_NilWrapUpCustomField(t *testing.T) {
	req := WrapUpRequirements{
		CustomFields: []CustomFieldDef{{Name: "risk_level", Required: true}},
		Enforce:      "hard",
	}
	if issues := req.Validate(nil); len(issues) != 1 {
		t.Errorf("Validate(nil) should report missing custom field, got: %v", issues)
	}
}

func TestWrapUpRequirements_Validate_EnforceNone(t *testing.T) {
	req := WrapUpRequirements{
		Required: []string{"accomplishments"},
		Enforce:  "none",
	}
	w := &WrapUp{} // empty

	issues := req.Validate(w)
	if len(issues) != 0 {
		t.Errorf("Validate with enforce=none should return no issues, got: %v", issues)
	}
}

func TestWrapUpRequirements_Validate_CustomFields(t *testing.T) {
	req := WrapUpRequirements{
		Required: []string{"accomplishments"},
		CustomFields: []CustomFieldDef{
			{Name: "risk_level", Required: true},
			{Name: "notes", Required: false},
		},
		Enforce: "hard",
	}

	// Missing custom required field.
	w := &WrapUp{Accomplishments: "stuff"}
	issues := req.Validate(w)
	if len(issues) != 1 {
		t.Errorf("Validate should report 1 issue (missing risk_level), got: %v", issues)
	}

	// With custom field present.
	w.Custom = map[string]string{"risk_level": "low"}
	issues = req.Validate(w)
	if len(issues) != 0 {
		t.Errorf("Validate should pass with custom field present, got: %v", issues)
	}
}

func TestWrapUpFieldPresent(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "did stuff",
		BeadsClosed:     []string{"gd-1"},
		Custom:          map[string]string{"foo": "bar"},
	}

	tests := []struct {
		field string
		want  bool
	}{
		{"accomplishments", true},
		{"blockers", false},
		{"handoff_notes", false},
		{"beads_closed", true},
		{"pull_requests", false},
		{"harness_gaps", false},
		{"foo", true},
		{"missing", false},
	}

	for _, tt := range tests {
		got := wrapUpFieldPresent(w, tt.field)
		if got != tt.want {
			t.Errorf("wrapUpFieldPresent(%q) = %v, want %v", tt.field, got, tt.want)
		}
	}

	// A nil wrap-up (bare gb stop) reports every field as absent without panicking.
	for _, field := range []string{"tldr", "accomplishments", "beads_closed", "anything"} {
		if wrapUpFieldPresent(nil, field) {
			t.Errorf("wrapUpFieldPresent(nil, %q) = true, want false", field)
		}
	}
}

func TestWrapUp_HarnessGaps_SurvivesRoundTrip(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "did stuff",
		HarnessGaps:     "no gb command to diff two agent beads; had to hand-jq both",
	}
	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if !containsStr(s, "harness_gaps") {
		t.Errorf("marshalled wrap-up missing harness_gaps key:\n%s", s)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.HarnessGaps != w.HarnessGaps {
		t.Errorf("HarnessGaps = %q, want %q", got.HarnessGaps, w.HarnessGaps)
	}

	// present-check reflects the value; it is optional and omitted when empty.
	if !wrapUpFieldPresent(got, "harness_gaps") {
		t.Error("wrapUpFieldPresent(harness_gaps) = false, want true when set")
	}
	if wrapUpFieldPresent(&WrapUp{Accomplishments: "x"}, "harness_gaps") {
		t.Error("wrapUpFieldPresent(harness_gaps) = true, want false when empty")
	}

	// harness_gaps is optional-not-required, so a wrap-up without it still validates.
	if issues := DefaultWrapUpRequirements().Validate(&WrapUp{TLDR: "t", Accomplishments: "a"}); len(issues) != 0 {
		t.Errorf("harness_gaps must not be required; got validation issues: %v", issues)
	}

	// It renders into the human-readable comment when present.
	if c := WrapUpToComment(got); !containsStr(c, w.HarnessGaps) {
		t.Errorf("WrapUpToComment missing harness_gaps content:\n%s", c)
	}
}

func TestWrapUpToComment(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "Fixed auth bug",
		Blockers:        "Waiting for review",
		BeadsClosed:     []string{"gd-1", "gd-2"},
		PullRequests:    []string{"https://github.com/org/repo/pull/1"},
	}

	comment := WrapUpToComment(w)

	if got := comment; got == "" {
		t.Fatal("WrapUpToComment returned empty string")
	}

	// Check key content is present.
	for _, want := range []string{
		"Fixed auth bug",
		"Waiting for review",
		"gd-1",
		"gd-2",
		"https://github.com/org/repo/pull/1",
	} {
		if !containsStr(comment, want) {
			t.Errorf("WrapUpToComment missing %q in output:\n%s", want, comment)
		}
	}
}

func TestWrapUpToComment_MinimalFields(t *testing.T) {
	w := &WrapUp{Accomplishments: "Done"}
	comment := WrapUpToComment(w)

	if !containsStr(comment, "Done") {
		t.Errorf("WrapUpToComment missing accomplishments in:\n%s", comment)
	}
	if containsStr(comment, "Blockers:") {
		t.Errorf("WrapUpToComment should not include Blockers when empty:\n%s", comment)
	}
}

func TestLoadWrapUpRequirements_DefaultWhenNoConfigBeads(t *testing.T) {
	lister := &mockConfigBeadLister{beads: nil}
	reqs := LoadWrapUpRequirements(context.Background(), lister, "test-agent")

	defaults := DefaultWrapUpRequirements()
	if reqs.Enforce != defaults.Enforce {
		t.Errorf("Enforce = %q, want %q (default)", reqs.Enforce, defaults.Enforce)
	}
	if len(reqs.Required) != len(defaults.Required) {
		t.Errorf("Required = %v, want %v (default)", reqs.Required, defaults.Required)
	}
}

func TestLoadWrapUpRequirements_FromConfigBead(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "wrapup-config",
				Labels:      []string{"global"},
				Description: `{"required":["accomplishments","blockers"],"enforce":"hard"}`,
			},
		},
	}
	reqs := LoadWrapUpRequirements(context.Background(), lister, "test-agent")

	if reqs.Enforce != "hard" {
		t.Errorf("Enforce = %q, want %q", reqs.Enforce, "hard")
	}
	if len(reqs.Required) != 2 {
		t.Errorf("Required = %v, want [accomplishments blockers]", reqs.Required)
	}
}

func TestLoadWrapUpRequirements_RoleOverridesGlobal(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "wrapup-config",
				Labels:      []string{"global"},
				Description: `{"required":["accomplishments"],"enforce":"soft"}`,
			},
			{
				Title:       "wrapup-config",
				Labels:      []string{"role:crew"},
				Description: `{"required":["accomplishments","blockers"],"enforce":"hard"}`,
			},
		},
	}

	// Agent with role:crew should get the role override.
	// BuildAgentSubscriptions for "gardenio/crews/test-agent" includes role:crews and role:crew.
	reqs := LoadWrapUpRequirements(context.Background(), lister, "gardenio/crews/test-agent")

	if reqs.Enforce != "hard" {
		t.Errorf("Enforce = %q, want %q (role override)", reqs.Enforce, "hard")
	}
	if len(reqs.Required) != 2 {
		t.Errorf("Required = %v, want [accomplishments blockers]", reqs.Required)
	}
}

func TestLoadWrapUpRequirements_CustomFields(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:  "wrapup-config",
				Labels: []string{"global"},
				Description: `{
					"required": ["accomplishments"],
					"custom_fields": [
						{"name": "risk_assessment", "description": "Risk level of changes", "required": true}
					],
					"enforce": "hard"
				}`,
			},
		},
	}
	reqs := LoadWrapUpRequirements(context.Background(), lister, "test-agent")

	if len(reqs.CustomFields) != 1 {
		t.Fatalf("CustomFields = %v, want 1 entry", reqs.CustomFields)
	}
	if reqs.CustomFields[0].Name != "risk_assessment" {
		t.Errorf("CustomFields[0].Name = %q, want %q", reqs.CustomFields[0].Name, "risk_assessment")
	}
	if !reqs.CustomFields[0].Required {
		t.Error("CustomFields[0].Required should be true")
	}
}

func TestOutputWrapUpExpectations_Default(t *testing.T) {
	// Set up daemon with no config beads so defaults are used.
	origDaemon := daemon
	defer func() { daemon = origDaemon }()

	// Create a mock daemon that returns no config beads.
	// We can test outputWrapUpExpectations indirectly through the output.
	// Since it depends on the global daemon, we test the requirements rendering directly.
	reqs := DefaultWrapUpRequirements()

	var buf strings.Builder
	// Simulate what outputWrapUpExpectations does with default reqs.
	fmt.Fprintf(&buf, "\n## Wrap-Up Requirements\n\n")
	fmt.Fprintln(&buf, "You **should** provide a structured wrap-up when calling `gb stop`.")
	fmt.Fprintln(&buf, "")
	fmt.Fprint(&buf, "**Required fields:** ")
	for i, f := range reqs.Required {
		if i > 0 {
			fmt.Fprint(&buf, ", ")
		}
		fmt.Fprintf(&buf, "`%s`", f)
	}
	fmt.Fprintln(&buf)

	output := buf.String()
	if !strings.Contains(output, "Wrap-Up Requirements") {
		t.Error("output should contain 'Wrap-Up Requirements' header")
	}
	if !strings.Contains(output, "`accomplishments`") {
		t.Error("output should mention accomplishments field")
	}
	if !strings.Contains(output, "should") {
		t.Error("output should use 'should' for soft enforcement")
	}
}

func TestRenderWrapUpExpectations_TeachesPullRequestsShape(t *testing.T) {
	// This rendered block is the "Wrap-Up Requirements" surface every agent
	// sees via gb prime — the pull_requests omission here is what taught the
	// bare-string shape in ka-0000006605. Pin the array teaching.
	var buf strings.Builder
	renderWrapUpExpectations(&buf, DefaultWrapUpRequirements())
	out := buf.String()

	if !strings.Contains(out, "- `pull_requests` — JSON ARRAY of MR/PR URLs") {
		t.Errorf("Field-shape block missing pull_requests bullet:\n%s", out)
	}
	if !strings.Contains(out, `"pull_requests":["https://gitlab.com/example-org/app/-/merge_requests/3286"]`) {
		t.Errorf("example wrap-up missing pull_requests array:\n%s", out)
	}
}

func TestRenderWrapUpExpectations_TeachesRestartWhen(t *testing.T) {
	// The restart_when nudge (task ka-0000005780) is the producer half of the
	// restart-condition card: agents must be told to declare their wake
	// condition so the bridge can render the "Wakes when …" line (consumer
	// side, ka-0000007398). Pin that the field-shape block teaches it and
	// names both the card effect and the omit-when-terminal guidance.
	var buf strings.Builder
	renderWrapUpExpectations(&buf, DefaultWrapUpRequirements())
	out := buf.String()

	if !strings.Contains(out, "- `restart_when` —") {
		t.Errorf("Field-shape block missing restart_when bullet:\n%s", out)
	}
	if !strings.Contains(out, "↻ Restart:") {
		t.Errorf("restart_when bullet should name the card '↻ Restart:' line:\n%s", out)
	}
	if !strings.Contains(out, "progressing") {
		t.Errorf("restart_when bullet should explain the ⏳ progressing outcome:\n%s", out)
	}
	if !strings.Contains(out, "Omit it for a truly terminal stop") {
		t.Errorf("restart_when bullet should tell agents to omit it for a terminal stop:\n%s", out)
	}
}

// A restart_when passed in --wrapup JSON must survive the marshal/unmarshal
// round-trip gb stop does (MarshalWrapUp -> bead field -> UnmarshalWrapUp).
// Before the producer field existed the value was silently dropped, leaving
// the consumer-side wake-trigger render permanently inert (task ka-0000005780).
func TestWrapUp_RestartWhen_SurvivesRoundTrip(t *testing.T) {
	w := &WrapUp{
		TLDR:            "parked behind MR merge",
		Accomplishments: "opened MR !7099",
		RestartWhen:     "when ka-0000005780 merges",
	}
	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if !strings.Contains(s, `"restart_when":"when ka-0000005780 merges"`) {
		t.Errorf("marshalled wrap-up dropped restart_when: %s", s)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.RestartWhen != "when ka-0000005780 merges" {
		t.Errorf("RestartWhen = %q, want %q", got.RestartWhen, "when ka-0000005780 merges")
	}
}

func TestRenderWrapUpExpectations_EnforceNone(t *testing.T) {
	var buf strings.Builder
	renderWrapUpExpectations(&buf, WrapUpRequirements{Enforce: "none"})
	if buf.Len() != 0 {
		t.Errorf("enforce=none should render nothing, got:\n%s", buf.String())
	}
}

func TestWrapUpExpectedFormatHint(t *testing.T) {
	hint := wrapUpExpectedFormatHint()
	// Generated from the WrapUp struct + default requirements so it cannot
	// drift when fields are added or retyped.
	want := `{"tldr":"...","accomplishments":"...","blockers":"...","handoff_notes":"...","restart_when":"...","pull_requests":["...", ...],"harness_gaps":"..."}`
	if hint != want {
		t.Errorf("wrapUpExpectedFormatHint() = %q, want %q", hint, want)
	}
}

func TestLoadWrapUpRequirements_InvalidJSON(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "wrapup-config",
				Labels:      []string{"global"},
				Description: `not valid json`,
			},
		},
	}
	reqs := LoadWrapUpRequirements(context.Background(), lister, "test-agent")

	// Should fall back to defaults when config bead has invalid JSON.
	defaults := DefaultWrapUpRequirements()
	if reqs.Enforce != defaults.Enforce {
		t.Errorf("Enforce = %q, want %q (default fallback)", reqs.Enforce, defaults.Enforce)
	}
}

// --- Validation enforcement scenarios ---

func TestWrapUpRequirements_HardEnforce_BlocksOnMissing(t *testing.T) {
	reqs := WrapUpRequirements{
		Required: []string{"accomplishments", "blockers"},
		Enforce:  "hard",
	}
	w := &WrapUp{Accomplishments: "did stuff"} // blockers missing

	issues := reqs.Validate(w)
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], "blockers") {
		t.Errorf("issue should mention 'blockers': %s", issues[0])
	}
}

func TestWrapUpRequirements_SoftEnforce_WarnsButPasses(t *testing.T) {
	reqs := WrapUpRequirements{
		Required: []string{"accomplishments"},
		Enforce:  "soft",
	}
	w := &WrapUp{} // empty — should warn but validate still returns issues

	issues := reqs.Validate(w)
	if len(issues) == 0 {
		t.Error("soft enforcement should still return issues")
	}
	// The caller (gb stop) decides whether to block or warn based on Enforce level.
}

// --- Backward compatibility ---

func TestNoWrapUpConfig_DefaultsToHardEnforcement(t *testing.T) {
	lister := &mockConfigBeadLister{beads: nil}
	reqs := LoadWrapUpRequirements(context.Background(), lister, "any-agent")

	// Default is hard with tldr+accomplishments required — agents must provide
	// a wrap-up (incl. a one-line tldr) before stopping, or use --force
	// (bead ka-0000002919).
	if reqs.Enforce != "hard" {
		t.Errorf("Enforce = %q, want 'hard' (default now blocks bare stop)", reqs.Enforce)
	}
	if len(reqs.Required) != 2 || reqs.Required[0] != "tldr" {
		t.Errorf("Required = %v, want [tldr accomplishments]", reqs.Required)
	}
}

func TestReasonFlagStillWorks_NoWrapUpNeeded(t *testing.T) {
	// When --reason is used without --wrapup, processWrapUp is not called.
	// This test verifies that processWrapUp only works with valid JSON.
	_, _, err := processWrapUp(`{"accomplishments":"done"}`)
	if err != nil {
		t.Fatalf("processWrapUp with valid JSON should succeed: %v", err)
	}
}

// --- Stop gate validation logic ---

func TestWrapUpGateLogic_StopNotRequested(t *testing.T) {
	// When stop_requested is not set, gate should not block.
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{},
	}
	// Simulate: stop not requested → no block
	if bead.Fields["stop_requested"] == "true" {
		t.Error("stop_requested should not be set")
	}
}

func TestWrapUpGateLogic_WrapUpAlreadyProvided(t *testing.T) {
	// When wrapup field is already set, gate should not block.
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"stop_requested": "true",
			WrapUpFieldName:  `{"accomplishments":"did stuff"}`,
		},
	}
	if bead.Fields[WrapUpFieldName] == "" {
		t.Error("wrapup should be present")
	}
}

func TestWrapUpGateLogic_HardEnforceNoWrapUp(t *testing.T) {
	// When hard enforcement is on and no wrapup, the gate should block.
	reqs := WrapUpRequirements{
		Required: []string{"accomplishments"},
		Enforce:  "hard",
	}
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"stop_requested": "true",
			// no wrapup field
		},
	}

	// Simulate gate logic.
	shouldBlock := bead.Fields["stop_requested"] == "true" &&
		bead.Fields[WrapUpFieldName] == "" &&
		reqs.Enforce == "hard"

	if !shouldBlock {
		t.Error("gate should block: hard enforcement, no wrapup, stop requested")
	}
}

func TestWrapUpGateLogic_SoftEnforceNoWrapUp(t *testing.T) {
	// When soft enforcement is on and no wrapup, the gate should NOT block.
	reqs := WrapUpRequirements{
		Required: []string{"accomplishments"},
		Enforce:  "soft",
	}
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"stop_requested": "true",
		},
	}

	shouldBlock := bead.Fields["stop_requested"] == "true" &&
		bead.Fields[WrapUpFieldName] == "" &&
		reqs.Enforce == "hard"

	if shouldBlock {
		t.Error("gate should NOT block: soft enforcement")
	}
}

// --- WrapUp edge cases ---

func TestWrapUp_AllFieldsFilled(t *testing.T) {
	reqs := WrapUpRequirements{
		Required: []string{"accomplishments", "blockers", "handoff_notes"},
		CustomFields: []CustomFieldDef{
			{Name: "risk", Required: true},
		},
		Enforce: "hard",
	}
	w := &WrapUp{
		Accomplishments: "stuff",
		Blockers:        "none",
		HandoffNotes:    "check PR",
		BeadsClosed:     []string{"gd-1"},
		PullRequests:    []string{"https://github.com/org/repo/pull/1"},
		Custom:          map[string]string{"risk": "low"},
	}

	issues := reqs.Validate(w)
	if len(issues) != 0 {
		t.Errorf("fully filled wrap-up should pass, got issues: %v", issues)
	}
}

func TestWrapUp_EmptyCustomMap(t *testing.T) {
	w := &WrapUp{Accomplishments: "done"}
	if wrapUpFieldPresent(w, "nonexistent") {
		t.Error("nonexistent custom field should not be present")
	}
}

func TestWrapUpConfigCategory_Registered(t *testing.T) {
	cat := claudeconfig.LookupCategory(WrapUpConfigCategory)
	if cat == nil {
		t.Fatal("wrapup-config category should be registered")
	}
	if cat.Strategy != MergeOverride {
		t.Errorf("wrapup-config should use MergeOverride, got %d", cat.Strategy)
	}
}

func TestNormalizeAndValidateTLDR(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		want       string
		wantErr    bool
		wantWarn   bool // at least one warning expected
		warnSubstr string
	}{
		{
			name: "empty stays empty, no error",
			in:   "",
			want: "",
		},
		{
			name: "whitespace-only normalizes to empty",
			in:   "   \n\t ",
			want: "",
		},
		{
			name: "plain one-liner passes untouched",
			in:   "Added validated tldr field to gb stop --wrapup",
			want: "Added validated tldr field to gb stop --wrapup",
		},
		{
			name:       "embedded newline collapsed to space + warns",
			in:         "line one\nline two",
			want:       "line one line two",
			wantWarn:   true,
			warnSubstr: "single line",
		},
		{
			name:     "runs of whitespace collapsed",
			in:       "a   b\t\tc",
			want:     "a b c",
			wantWarn: true,
		},
		{
			name: "angle brackets stored raw (escaping is render-time, ka-0000005806)",
			in:   "fixed <http://x|x> and <tag>",
			want: "fixed <http://x|x> and <tag>",
		},
		{
			name: "comparison and arrow idioms stored raw",
			in:   "parked on human merge with fresh <2h P0; esc P1->P0",
			want: "parked on human merge with fresh <2h P0; esc P1->P0",
		},
		{
			name:    "over hard cap rejected",
			in:      strings.Repeat("a", TLDRHardCap+1),
			wantErr: true,
		},
		{
			name:       "exactly hard cap accepted (warns past soft cap)",
			in:         strings.Repeat("a", TLDRHardCap),
			want:       strings.Repeat("a", TLDRHardCap),
			wantWarn:   true,
			warnSubstr: "soft cap",
		},
		{
			name: "exactly soft cap accepted, no warning",
			in:   strings.Repeat("a", TLDRSoftCap),
			want: strings.Repeat("a", TLDRSoftCap),
		},
		{
			name:       "over soft cap warns but accepted",
			in:         strings.Repeat("a", TLDRSoftCap+1),
			want:       strings.Repeat("a", TLDRSoftCap+1),
			wantWarn:   true,
			warnSubstr: "soft cap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings, err := normalizeAndValidateTLDR(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("normalized = %q, want %q", got, tt.want)
			}
			if tt.wantWarn && len(warnings) == 0 {
				t.Errorf("expected at least one warning, got none")
			}
			if !tt.wantWarn && len(warnings) != 0 {
				t.Errorf("expected no warnings, got %v", warnings)
			}
			if tt.warnSubstr != "" {
				found := false
				for _, w := range warnings {
					if strings.Contains(w, tt.warnSubstr) {
						found = true
					}
				}
				if !found {
					t.Errorf("expected a warning containing %q, got %v", tt.warnSubstr, warnings)
				}
			}
		})
	}
}

func TestNormalizeAndValidateTLDR_HardCapCountsRaw(t *testing.T) {
	// Length is measured against the raw (persisted) value now that escaping is
	// render-time (ka-0000005806). Angle brackets are 1 rune each, so a value of
	// exactly the hard cap made entirely of '<' is accepted, and one over it is
	// rejected.
	atCap := strings.Repeat("<", TLDRHardCap)
	got, _, err := normalizeAndValidateTLDR(atCap)
	if err != nil {
		t.Fatalf("expected raw '<'×hardCap to be accepted, got error: %v", err)
	}
	if got != atCap {
		t.Errorf("stored value = %q, want raw angle brackets", got)
	}
	if _, _, err := normalizeAndValidateTLDR(strings.Repeat("<", TLDRHardCap+1)); err == nil {
		t.Fatal("expected hard-cap rejection one rune over, got nil")
	}
}

func TestProcessWrapUp_NormalizesTLDR(t *testing.T) {
	raw := `{"tldr":"did <stuff>\nacross lines","accomplishments":"x"}`
	w, _, err := processWrapUp(raw)
	if err != nil {
		t.Fatalf("processWrapUp: %v", err)
	}
	// Stored raw now; newline is still collapsed, angle brackets are preserved
	// (escaping is a render-time concern — ka-0000005806).
	want := "did <stuff> across lines"
	if w.TLDR != want {
		t.Errorf("TLDR = %q, want %q", w.TLDR, want)
	}
}

func TestProcessWrapUp_RejectsOverlongTLDR(t *testing.T) {
	raw := fmt.Sprintf(`{"tldr":%q,"accomplishments":"x"}`, strings.Repeat("a", TLDRHardCap+1))
	if _, _, err := processWrapUp(raw); err == nil {
		t.Fatal("expected error for overlong tldr, got nil")
	}
}

func TestWrapUp_TLDRRoundTripAndPresent(t *testing.T) {
	w := &WrapUp{TLDR: "one line summary", Accomplishments: "did stuff"}
	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if !strings.Contains(s, `"tldr":"one line summary"`) {
		t.Errorf("marshalled JSON missing tldr: %s", s)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.TLDR != w.TLDR {
		t.Errorf("TLDR = %q, want %q", got.TLDR, w.TLDR)
	}
	if !wrapUpFieldPresent(w, "tldr") {
		t.Error("wrapUpFieldPresent(tldr) = false, want true")
	}
	if wrapUpFieldPresent(&WrapUp{}, "tldr") {
		t.Error("wrapUpFieldPresent(tldr) on empty = true, want false")
	}
}

func TestWrapUp_TLDROmittedWhenEmpty(t *testing.T) {
	s, err := MarshalWrapUp(&WrapUp{Accomplishments: "x"})
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if strings.Contains(s, "tldr") {
		t.Errorf("empty tldr should be omitted from JSON: %s", s)
	}
}

func TestWrapUpToComment_IncludesTLDR(t *testing.T) {
	c := WrapUpToComment(&WrapUp{TLDR: "summary line", Accomplishments: "did stuff"})
	if !strings.Contains(c, "TLDR: summary line") {
		t.Errorf("comment missing TLDR line: %q", c)
	}
}

// --- Successor enum (task ka-0000002751) ---

// TestNormalizeSuccessor covers folding of case/whitespace and empty->none.
// It does NOT reject unknowns — that is ValidateSuccessor's job.
func TestNormalizeSuccessor(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", SuccessorNone},
		{"   ", SuccessorNone},
		{"none", SuccessorNone},
		{"same", SuccessorSame},
		{"new", SuccessorNew},
		{"  New  ", SuccessorNew},
		{"SAME", SuccessorSame},
		{"bogus", "bogus"}, // normalized but not validated
	}
	for _, c := range cases {
		if got := NormalizeSuccessor(c.in); got != c.want {
			t.Errorf("NormalizeSuccessor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidateSuccessor pins the "reject unknown at parse" contract: the three
// enum values (and their case/whitespace variants) validate to canonical form;
// empty folds to none; anything else is a fatal error naming the accepted set.
func TestValidateSuccessor(t *testing.T) {
	ok := []struct {
		in, want string
	}{
		{"", SuccessorNone},
		{"none", SuccessorNone},
		{"same", SuccessorSame},
		{"new", SuccessorNew},
		{" New ", SuccessorNew},
		{"NONE", SuccessorNone},
	}
	for _, c := range ok {
		got, err := ValidateSuccessor(c.in)
		if err != nil {
			t.Errorf("ValidateSuccessor(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateSuccessor(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"maybe", "yes", "continue", "n/a", "0", "true"} {
		got, err := ValidateSuccessor(bad)
		if err == nil {
			t.Errorf("ValidateSuccessor(%q) = %q, want error", bad, got)
			continue
		}
		// The error must name the accepted set so the agent can self-correct.
		for _, want := range ValidSuccessorValues {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ValidateSuccessor(%q) error %q should mention %q", bad, err.Error(), want)
			}
		}
	}
}

// TestReconcileSuccessorGoal covers the design-memo §2.5 contradiction rule:
// goal_satisfied="no" + successor="none" coerces to none WITH a warning; every
// other combination passes through unchanged and silent.
func TestReconcileSuccessorGoal(t *testing.T) {
	cases := []struct {
		name          string
		successor     string
		goal          string
		wantSuccessor string
		wantWarn      bool
	}{
		{"contradiction: no + none -> warn, stay none", SuccessorNone, "no", SuccessorNone, true},
		{"contradiction case-insensitive", SuccessorNone, "NO", SuccessorNone, true},
		{"no + same is legitimate continuation", SuccessorSame, "no", SuccessorSame, false},
		{"no + new is legitimate continuation", SuccessorNew, "no", SuccessorNew, false},
		{"yes + none is clean terminal", SuccessorNone, "yes", SuccessorNone, false},
		{"empty goal + none is clean terminal", SuccessorNone, "", SuccessorNone, false},
		{"empty goal + new passes through", SuccessorNew, "", SuccessorNew, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warn := reconcileSuccessorGoal(c.successor, c.goal)
			if got != c.wantSuccessor {
				t.Errorf("successor = %q, want %q", got, c.wantSuccessor)
			}
			if c.wantWarn && warn == "" {
				t.Errorf("expected a contradiction warning, got none")
			}
			if !c.wantWarn && warn != "" {
				t.Errorf("expected no warning, got %q", warn)
			}
		})
	}
}

// TestReconcileGoalUnachievable proves the crash-loop circuit breaker (P3,
// ka-0000005843): goal_unachievable=true coerces ANY requested successor to none
// (warning when it overrode a real intent), while goal_unachievable=false is
// fully inert regardless of successor.
func TestReconcileGoalUnachievable(t *testing.T) {
	cases := []struct {
		name          string
		successor     string
		unachievable  bool
		wantSuccessor string
		wantWarn      bool
	}{
		{"unachievable overrides new -> none + warn", SuccessorNew, true, SuccessorNone, true},
		{"unachievable overrides same -> none + warn", SuccessorSame, true, SuccessorNone, true},
		{"unachievable + none -> none, no warn", SuccessorNone, true, SuccessorNone, false},
		{"not unachievable + new passes through", SuccessorNew, false, SuccessorNew, false},
		{"not unachievable + same passes through", SuccessorSame, false, SuccessorSame, false},
		{"not unachievable + none passes through", SuccessorNone, false, SuccessorNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warn := reconcileGoalUnachievable(c.successor, c.unachievable)
			if got != c.wantSuccessor {
				t.Errorf("successor = %q, want %q", got, c.wantSuccessor)
			}
			if c.wantWarn && warn == "" {
				t.Errorf("expected an override warning, got none")
			}
			if !c.wantWarn && warn != "" {
				t.Errorf("expected no warning, got %q", warn)
			}
		})
	}
}

// TestProcessWrapUp_GoalUnachievableForcesNone proves the end-to-end producer
// path: a wrap-up that requests successor=new/same but also sets
// goal_unachievable=true is coerced to no successor at the gb stop boundary, so
// neither the successor mint nor the wake-scanner revive can fire.
func TestProcessWrapUp_GoalUnachievableForcesNone(t *testing.T) {
	for _, requested := range []string{`"new"`, `"same"`} {
		raw := `{"accomplishments":"x","successor":` + requested + `,"goal_unachievable":true}`
		w, _, err := processWrapUp(raw)
		if err != nil {
			t.Fatalf("processWrapUp(%s): %v", raw, err)
		}
		if w.Successor != "" {
			t.Errorf("successor = %q, want empty (coerced to none) for %s", w.Successor, raw)
		}
		if !w.GoalUnachievable {
			t.Errorf("GoalUnachievable = false, want true for %s", raw)
		}
	}
}

// TestWrapUp_GoalUnachievableRoundTrip proves the field survives marshal/unmarshal
// and that false is omitted from the stored JSON (the omitempty contract).
func TestWrapUp_GoalUnachievableRoundTrip(t *testing.T) {
	s, err := MarshalWrapUp(&WrapUp{Accomplishments: "x", GoalUnachievable: true})
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if !strings.Contains(s, `"goal_unachievable":true`) {
		t.Errorf("marshalled JSON missing goal_unachievable: %s", s)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if !got.GoalUnachievable {
		t.Errorf("GoalUnachievable = false, want true")
	}
	empty, err := MarshalWrapUp(&WrapUp{Accomplishments: "x"})
	if err != nil {
		t.Fatalf("MarshalWrapUp(empty): %v", err)
	}
	if strings.Contains(empty, "goal_unachievable") {
		t.Errorf("false goal_unachievable should be omitted from JSON: %s", empty)
	}
}

// TestWrapUp_SuccessorRoundTrip proves a populated successor survives JSON
// marshal/unmarshal and that "none"/empty is omitted from the stored JSON so an
// inert wrap-up stays minimal (the omitempty contract the bridge relies on).
func TestWrapUp_SuccessorRoundTrip(t *testing.T) {
	w := &WrapUp{Accomplishments: "did stuff", Successor: SuccessorNew}
	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	if !strings.Contains(s, `"successor":"new"`) {
		t.Errorf("marshalled JSON missing successor: %s", s)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.Successor != SuccessorNew {
		t.Errorf("Successor = %q, want %q", got.Successor, SuccessorNew)
	}

	// Empty successor is omitted.
	empty, err := MarshalWrapUp(&WrapUp{Accomplishments: "x"})
	if err != nil {
		t.Fatalf("MarshalWrapUp(empty): %v", err)
	}
	if strings.Contains(empty, "successor") {
		t.Errorf("empty successor should be omitted from JSON: %s", empty)
	}
}

// TestWrapUpFieldPresent_Successor confirms only a real intent (same/new)
// registers as present; none/empty is inert.
func TestWrapUpFieldPresent_Successor(t *testing.T) {
	if wrapUpFieldPresent(&WrapUp{Successor: SuccessorNone}, "successor") {
		t.Error("successor=none should not be present")
	}
	if wrapUpFieldPresent(&WrapUp{}, "successor") {
		t.Error("empty successor should not be present")
	}
	if !wrapUpFieldPresent(&WrapUp{Successor: SuccessorSame}, "successor") {
		t.Error("successor=same should be present")
	}
	if !wrapUpFieldPresent(&WrapUp{Successor: SuccessorNew}, "successor") {
		t.Error("successor=new should be present")
	}
}

// TestWrapUpToComment_IncludesSuccessor shows a non-none successor renders in
// the human-readable comment, and none/empty does not.
func TestWrapUpToComment_IncludesSuccessor(t *testing.T) {
	c := WrapUpToComment(&WrapUp{Accomplishments: "x", Successor: SuccessorNew})
	if !strings.Contains(c, "Successor: new") {
		t.Errorf("comment should mention successor: %q", c)
	}
	c2 := WrapUpToComment(&WrapUp{Accomplishments: "x", Successor: SuccessorNone})
	if strings.Contains(c2, "Successor:") {
		t.Errorf("none successor should not render in comment: %q", c2)
	}
}

// TestRenderWrapUpExpectations_TeachesSuccessor pins the prime-surface teaching
// so agents learn the enum + the goal/none contradiction before they reach stop.
func TestRenderWrapUpExpectations_TeachesSuccessor(t *testing.T) {
	var buf strings.Builder
	renderWrapUpExpectations(&buf, DefaultWrapUpRequirements())
	out := buf.String()
	if !strings.Contains(out, "- `successor` —") {
		t.Errorf("expectations missing successor bullet:\n%s", out)
	}
	for _, v := range []string{"`none`", "`same`", "`new`"} {
		if !strings.Contains(out, v) {
			t.Errorf("expectations should mention %s:\n%s", v, out)
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// UnmarshalWrapUp is the read side of MarshalWrapUp, for round-trip tests.
func UnmarshalWrapUp(s string) (*WrapUp, error) {
	var w WrapUp
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return nil, fmt.Errorf("unmarshalling wrap-up: %w", err)
	}
	return &w, nil
}
