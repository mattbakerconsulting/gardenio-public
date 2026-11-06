package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// fakeChainSpawnDaemon is a gardenerDaemon that ALSO satisfies chainSpawnDaemon,
// recording the heir CreateBead/AddLabel/AddDependency/AddComment calls so the
// engine-side v1 succession path (maybeSpawnChainHeir) can be asserted without
// a live daemon. GetMentions returns the seeded succeeds-edges so ChainHeirGuard
// walks a real (test-controlled) chain.
type fakeChainSpawnDaemon struct {
	beads    map[string]*beadsapi.BeadDetail
	mentions map[string][]beadsapi.BeadReferenceSummary

	created   []beadsapi.CreateBeadRequest
	labels    map[string][]string
	deps      [][3]string // {beadID, dependsOnID, depType}
	comments  []string    // "<beadID>: <text>"
	createID  string      // ID returned by CreateBead
	createErr error
}

func newFakeChainSpawnDaemon() *fakeChainSpawnDaemon {
	return &fakeChainSpawnDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		mentions: map[string][]beadsapi.BeadReferenceSummary{},
		labels:   map[string][]string{},
		createID: "gd-heir00001",
	}
}

func (f *fakeChainSpawnDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, req)
	return f.createID, nil
}

func (f *fakeChainSpawnDaemon) AddLabel(_ context.Context, beadID, label string) error {
	f.labels[beadID] = append(f.labels[beadID], label)
	return nil
}

func (f *fakeChainSpawnDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	f.deps = append(f.deps, [3]string{beadID, dependsOnID, depType})
	return nil
}

func (f *fakeChainSpawnDaemon) AddComment(_ context.Context, beadID, _, text string) error {
	f.comments = append(f.comments, beadID+": "+text)
	return nil
}

func (f *fakeChainSpawnDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	return f.beads[beadID], nil
}

func (f *fakeChainSpawnDaemon) GetMentions(_ context.Context, beadID string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	return f.mentions[beadID], nil
}

// The remaining gardenerDaemon methods are unused by the chain-spawn path; stub
// them so the fake satisfies the full interface.
func (f *fakeChainSpawnDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}
func (f *fakeChainSpawnDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeChainSpawnDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}
func (f *fakeChainSpawnDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeChainSpawnDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}
func (f *fakeChainSpawnDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// predTrig builds a predecessor TriggerDef with a spawn-mode chain.to and a
// seeded predecessor bead carrying the gardener-epic label + binding fields.
func predTrig(d *fakeChainSpawnDaemon, predID, epic string, to *ChainTo) TriggerDef {
	chain := &ChainSpec{To: to}
	chain.Normalize()
	d.beads[predID] = &beadsapi.BeadDetail{
		ID:     predID,
		Labels: []string{"gardener-epic:" + epic, "project:gardenio", "role:gardener"},
		Fields: map[string]string{
			"project":     "gardenio",
			"operated_by": "oidc:matt@x",
			"timezone":    "America/Los_Angeles",
		},
	}
	return TriggerDef{ID: predID, Project: "gardenio", OperatedBy: "oidc:matt@x", Chain: chain}
}

func TestMaybeSpawnChainHeir_HappyPath(t *testing.T) {
	d := newFakeChainSpawnDaemon()
	trig := predTrig(d, "gd-pred00001", "gd-epic00001", &ChainTo{
		Phase:        "impl",
		TerminalGoal: "All children merged & green.",
		PromptAdvice: "gd-implprmpt",
		Epic:         "{{same}}",
	})

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed)

	if len(d.created) != 1 {
		t.Fatalf("created %d heirs, want 1", len(d.created))
	}
	req := d.created[0]
	if req.Type != "trigger" || req.Kind != "data" {
		t.Errorf("heir type/kind = %s/%s, want trigger/data", req.Type, req.Kind)
	}
	if !strings.Contains(req.Description, "succeeds: gd-pred00001") {
		t.Errorf("heir description missing succeeds provenance: %q", req.Description)
	}
	if !strings.Contains(req.Title, "impl") {
		t.Errorf("heir title = %q, want phase-derived name", req.Title)
	}
	// Fields: enabled, inherited epic, operated_by carried.
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal heir fields: %v", err)
	}
	// `enabled` must be a JSON bool — the type:trigger schema rejects the
	// string "true" (ka-0000003627).
	if got, ok := fields["enabled"].(bool); !ok || !got {
		t.Errorf("heir enabled = %#v, want JSON bool true", fields["enabled"])
	}
	if fields["operated_by"] != "oidc:matt@x" {
		t.Errorf("heir operated_by = %q, want inherited", fields["operated_by"])
	}
	gardenerBlob, _ := fields["gardener"].(string)
	if !strings.Contains(gardenerBlob, "gd-epic00001") {
		t.Errorf("heir gardener block should target inherited epic: %q", gardenerBlob)
	}
	if fields["prompt_advice"] != "gd-implprmpt" {
		t.Errorf("heir prompt_advice = %q", fields["prompt_advice"])
	}
	// Labels + parent edge.
	gotLabels := strings.Join(d.labels[d.createID], ",")
	for _, want := range []string{"role:gardener", "gardener-epic:gd-epic00001", "project:gardenio"} {
		if !strings.Contains(gotLabels, want) {
			t.Errorf("heir labels %q missing %q", gotLabels, want)
		}
	}
	foundParent := false
	for _, dep := range d.deps {
		if dep[0] == d.createID && dep[1] == "gd-epic00001" && dep[2] == "parent-child" {
			foundParent = true
		}
	}
	if !foundParent {
		t.Errorf("heir missing parent-child edge to epic, deps=%v", d.deps)
	}
}

// TestMaybeSpawnChainHeir_FanOut proves a predecessor declaring MORE THAN ONE
// heir (chain.to array, C3 §3.3) materializes each heir independently.
func TestMaybeSpawnChainHeir_FanOut(t *testing.T) {
	d := newFakeChainSpawnDaemon()
	trig := predTrig(d, "gd-dw53qqk1gy", "gd-epicfan01", &ChainTo{Phase: "impl-a", Epic: "gd-ANVVMrCTqf"})
	// Add two more heirs so the chain fans out to three.
	trig.Chain.Extra = []*ChainTo{
		{Phase: "impl-b", Epic: "gd-U0ogvpVjao"},
		{Phase: "impl-c", Epic: "gd-39etI0NI1M"},
	}
	trig.Chain.Normalize()

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed)

	if len(d.created) != 3 {
		t.Fatalf("fan-out created %d heirs, want 3", len(d.created))
	}
	// Each heir carries the succeeds provenance + its own phase-derived title.
	wantPhases := map[string]bool{"impl-a": false, "impl-b": false, "impl-c": false}
	for _, req := range d.created {
		if !strings.Contains(req.Description, "succeeds: gd-dw53qqk1gy") {
			t.Errorf("heir %q missing succeeds provenance", req.Title)
		}
		for p := range wantPhases {
			if strings.Contains(req.Title, p) {
				wantPhases[p] = true
			}
		}
	}
	for p, seen := range wantPhases {
		if !seen {
			t.Errorf("fan-out missing heir for phase %q", p)
		}
	}
	// Each heir targets its OWN epic, not a shared one.
	for _, want := range []string{"gd-ANVVMrCTqf", "gd-U0ogvpVjao", "gd-39etI0NI1M"} {
		found := false
		for _, req := range d.created {
			var fields map[string]any
			_ = json.Unmarshal(req.Fields, &fields)
			if blob, _ := fields["gardener"].(string); strings.Contains(blob, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no fan-out heir targets epic %q", want)
		}
	}
}

// TestMaybeSpawnChainHeir_FanOutPartialGuard proves one heir's guard refusal
// does NOT block its siblings — each heir is guarded independently.
func TestMaybeSpawnChainHeir_FanOutPartialGuard(t *testing.T) {
	d := newFakeChainSpawnDaemon()
	// First heir is a cycle (its identity equals the predecessor's ancestor A);
	// the second is clean.
	trig := predTrig(d, "gd-3a2nLEER13", "gd-epic00099", &ChainTo{Phase: "impl", PromptAdvice: "gd-cycleadvc", Epic: "gd-epic00099"})
	trig.Chain.Extra = []*ChainTo{{Phase: "verify", PromptAdvice: "gd-cleanadvc", Epic: "gd-epic00099"}}
	trig.Chain.Normalize()
	// B succeeds A; A's identity carries the first heir's prompt_advice → cycle.
	rel := "succeeds"
	d.mentions["gd-3a2nLEER13"] = []beadsapi.BeadReferenceSummary{{ID: "gd-Dn2jazGSMc", RelationshipType: &rel}}
	d.beads["gd-Dn2jazGSMc"] = &beadsapi.BeadDetail{
		ID:     "gd-Dn2jazGSMc",
		Fields: map[string]string{"chain": `{"to":{"prompt_advice":"gd-cycleadvc","phase":"impl"}}`},
	}

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindBeadQuery)

	// Exactly the clean heir is created; the cycle heir is refused.
	if len(d.created) != 1 {
		t.Fatalf("partial-guard created %d heirs, want 1 (clean sibling only)", len(d.created))
	}
	if !strings.Contains(d.created[0].Title, "verify") {
		t.Errorf("surviving heir = %q, want the clean 'verify' sibling", d.created[0].Title)
	}
	// A refuse comment names the blocked heir.
	foundRefuse := false
	for _, c := range d.comments {
		if strings.Contains(c, "guard refused") {
			foundRefuse = true
		}
	}
	if !foundRefuse {
		t.Errorf("expected guard-refuse comment for the cycle heir, got %v", d.comments)
	}
}

func TestMaybeSpawnChainHeir_GuardCycleRefuses(t *testing.T) {
	d := newFakeChainSpawnDaemon()
	// Predecessor B succeeds A; the heir's identity (advice gd-blbide9tg7) equals
	// A's identity → adding it is a cycle A→B→A. Seed A with that prompt_advice.
	to := &ChainTo{Phase: "impl", PromptAdvice: "gd-blbide9tg7", Epic: "gd-epic00001"}
	trig := predTrig(d, "gd-HBj46Q2YNf", "gd-epic00001", to)
	// B succeeds A.
	rel := "succeeds"
	d.mentions["gd-HBj46Q2YNf"] = []beadsapi.BeadReferenceSummary{{ID: "gd-3rZigwZifb", RelationshipType: &rel}}
	// A's identity carries the same prompt_advice the heir would.
	d.beads["gd-3rZigwZifb"] = &beadsapi.BeadDetail{
		ID:     "gd-3rZigwZifb",
		Fields: map[string]string{"chain": `{"to":{"prompt_advice":"gd-blbide9tg7","phase":"impl"}}`},
	}

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindBeadQuery)

	if len(d.created) != 0 {
		t.Fatalf("created %d heirs on cycle, want 0", len(d.created))
	}
	// A refuse comment should be stamped on the predecessor.
	foundRefuse := false
	for _, c := range d.comments {
		if strings.HasPrefix(c, "gd-HBj46Q2YNf:") && strings.Contains(c, "guard refused") {
			foundRefuse = true
		}
	}
	if !foundRefuse {
		t.Errorf("expected guard-refuse comment, got %v", d.comments)
	}
}

func TestMaybeSpawnChainHeir_NoOpCases(t *testing.T) {
	cases := []struct {
		name  string
		slot  string
		kind  string
		chain *ChainSpec
	}{
		{"no chain", scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed, nil},
		{"premature max_runs", scheduler.StopSlotLoopEnd, scheduler.StopKindMaxRuns, nil},
		{"hard_stop slot", scheduler.StopSlotHardStop, scheduler.StopKindSpecificBeadClosed, nil},
		{"operator manual_force", scheduler.StopSlotLoopEnd, scheduler.StopKindManualForce, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeChainSpawnDaemon()
			trig := predTrig(d, "gd-pred00009", "gd-epic00009", &ChainTo{Phase: "impl"})
			if tc.name == "no chain" {
				trig.Chain = nil
			}
			maybeSpawnChainHeir(context.Background(), d, testLogger(), trig, tc.slot, tc.kind)
			if len(d.created) != 0 {
				t.Errorf("created %d heirs, want 0 for %s", len(d.created), tc.name)
			}
		})
	}
}

func TestMaybeSpawnChainHeir_ArmModeNoOp(t *testing.T) {
	d := newFakeChainSpawnDaemon()
	trig := predTrig(d, "gd-pred00002", "gd-epic00002", &ChainTo{Phase: "impl"})
	trig.Chain.Mode = "arm" // dormant successor — engine does not spawn

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed)

	if len(d.created) != 0 {
		t.Errorf("arm mode created %d heirs, want 0", len(d.created))
	}
}

func TestBuildHeirBeadSpec_ExplicitEpicAndInherit(t *testing.T) {
	to := &ChainTo{
		Phase:        "verify",
		TerminalGoal: "End-to-end chain fires.",
		Epic:         "gd-d4mmidmgvj", // explicit epic, not {{same}}
		Inherit:      []string{"project", "operated_by"},
	}
	chain := &ChainSpec{To: to}
	chain.Normalize()
	trig := TriggerDef{ID: "gd-pred00003", Project: "gardenio", Chain: chain}
	pred := &beadsapi.BeadDetail{
		ID:     "gd-pred00003",
		Labels: []string{"gardener-epic:gd-otherep01"}, // ignored: explicit epic wins
		Fields: map[string]string{"project": "gardenio", "operated_by": "slack:U1"},
	}

	spec, err := buildHeirBeadSpec(trig, pred, trig.Chain.To)
	if err != nil {
		t.Fatalf("buildHeirBeadSpec: %v", err)
	}
	if spec.Epic != "gd-d4mmidmgvj" {
		t.Errorf("epic = %q, want explicit gd-d4mmidmgvj", spec.Epic)
	}
	if !strings.Contains(strings.Join(spec.Labels, ","), "gardener-epic:gd-d4mmidmgvj") {
		t.Errorf("labels %v missing explicit epic", spec.Labels)
	}
	var fields map[string]any
	_ = json.Unmarshal(spec.Req.Fields, &fields)
	if fields["operated_by"] != "slack:U1" {
		t.Errorf("operated_by = %v, want inherited slack:U1", fields["operated_by"])
	}
}

// TestBuildHeirBeadSpec_SingleCronStaysSchedule asserts the heir-spawn path
// (the engine-side gardener authoring chokepoint) labels a normal single-cron
// heir trigger:schedule — the heir carries a flat cron + conditions, never a
// triggers[] array, so the mislabel guardrail (bug ka-0000001688) is a no-op for
// it. This pins the common case so a future refactor that accidentally drops
// triggers[] onto the heir would flip the label and fail the sibling assertion.
func TestBuildHeirBeadSpec_SingleCronStaysSchedule(t *testing.T) {
	to := &ChainTo{Phase: "verify", TerminalGoal: "End-to-end chain fires.", Epic: "{{same}}"}
	chain := &ChainSpec{To: to}
	chain.Normalize()
	trig := TriggerDef{ID: "gd-pred00009", Project: "gardenio", Chain: chain}
	pred := &beadsapi.BeadDetail{
		ID:     "gd-pred00009",
		Labels: []string{"gardener-epic:gd-epic00009"},
		Fields: map[string]string{"project": "gardenio", "cron": "7,37 * * * *"},
	}

	spec, err := buildHeirBeadSpec(trig, pred, trig.Chain.To)
	if err != nil {
		t.Fatalf("buildHeirBeadSpec: %v", err)
	}
	joined := strings.Join(spec.Labels, ",")
	if !strings.Contains(joined, "trigger:schedule") {
		t.Errorf("labels %v missing trigger:schedule for a single-cron heir", spec.Labels)
	}
	if strings.Contains(joined, "trigger:multi") {
		t.Errorf("labels %v wrongly promoted a single-cron heir to trigger:multi", spec.Labels)
	}
}

// TestBuildHeirBeadSpec_NonEmptyTriggersPromotesMulti exercises the guardrail on
// the engine path directly: if a heir's fields ever carry a non-empty triggers[]
// array, the heir MUST be born trigger:multi, not a silently-never-firing
// trigger:schedule. buildHeirBeadSpec does not itself set triggers[] today, so
// this drives TriggerSourceLabelForFields with the field value the heir builder
// passes it, guarding the shared helper at the engine call site.
func TestBuildHeirBeadSpec_NonEmptyTriggersPromotesMulti(t *testing.T) {
	const nonEmpty = `[{"kind":"bead_event","events":["closed"]},{"kind":"manual"}]`
	if got := TriggerSourceLabelForFields("trigger:schedule", nonEmpty); got != "trigger:multi" {
		t.Errorf("engine heir label for non-empty triggers[] = %q, want trigger:multi", got)
	}
}

// TestBuildHeirBeadSpec_InheritThreadID asserts that the wish (thread head)
// pointer thread_id propagates predecessor → heir through both chain gates: the
// inherit allowlist (defaultChainInherit / explicit Inherit, read by
// inheritedHeirFields) AND the restamp switch in buildHeirBeadSpec that copies
// inherited bindings onto the heir's fields. Without thread_id in BOTH gates an
// heir chained from a wish-bound predecessor loses its wish binding
// (ka-0000006315). The legacy "slack_thread" key carries nothing (no real
// field) and must never appear on the heir.
func TestBuildHeirBeadSpec_InheritThreadID(t *testing.T) {
	tests := []struct {
		name         string
		inherit      []string // explicit Inherit; nil ⇒ Normalize() fills the default set
		predThreadID string
		wantThreadID string // heir fields["thread_id"]
	}{
		{
			name:         "default inherit carries thread_id",
			inherit:      nil, // Normalize() applies defaultChainInherit
			predThreadID: "gd-6mhyk7bgll",
			wantThreadID: "gd-6mhyk7bgll",
		},
		{
			name:         "explicit inherit naming thread_id carries it",
			inherit:      []string{"project", "thread_id"},
			predThreadID: "gd-h8glcftqzq",
			wantThreadID: "gd-h8glcftqzq",
		},
		{
			name:         "explicit inherit omitting thread_id does not carry it",
			inherit:      []string{"project", "operated_by"},
			predThreadID: "gd-4nx6ves2eg",
			wantThreadID: "",
		},
		{
			name:         "predecessor without thread_id yields none",
			inherit:      nil,
			predThreadID: "",
			wantThreadID: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to := &ChainTo{
				Phase:        "verify",
				TerminalGoal: "Chain carries the wish.",
				Epic:         "gd-m8nmxssdvm",
				Inherit:      tc.inherit,
			}
			chain := &ChainSpec{To: to}
			chain.Normalize()
			trig := TriggerDef{ID: "gd-jc5yshppfy", Project: "gardenio", Chain: chain}
			predFields := map[string]string{"project": "gardenio", "operated_by": "slack:U1"}
			if tc.predThreadID != "" {
				predFields["thread_id"] = tc.predThreadID
			}
			pred := &beadsapi.BeadDetail{ID: "gd-jc5yshppfy", Fields: predFields}

			spec, err := buildHeirBeadSpec(trig, pred, trig.Chain.To)
			if err != nil {
				t.Fatalf("buildHeirBeadSpec: %v", err)
			}
			var fields map[string]any
			if err := json.Unmarshal(spec.Req.Fields, &fields); err != nil {
				t.Fatalf("unmarshal heir fields: %v", err)
			}
			gotThreadID, _ := fields["thread_id"].(string)
			if gotThreadID != tc.wantThreadID {
				t.Errorf("heir thread_id = %q, want %q", gotThreadID, tc.wantThreadID)
			}
			// The dead "slack_thread" alias must never be stamped on the heir.
			if _, ok := fields["slack_thread"]; ok {
				t.Errorf("heir unexpectedly carries dead slack_thread field: %v", fields)
			}
		})
	}
}

// TestDefaultChainInherit_IncludesThreadID guards the inherit allowlist so a
// future edit cannot silently drop thread_id from the default set and break
// wish propagation for chains that do not declare an explicit inherit list.
func TestDefaultChainInherit_IncludesThreadID(t *testing.T) {
	found := false
	for _, k := range defaultChainInherit {
		if k == "thread_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("defaultChainInherit %v must include thread_id", defaultChainInherit)
	}
}

func TestStopKindIsGoalAchievement(t *testing.T) {
	achieve := []string{
		scheduler.StopKindGoalSatisfied,
		scheduler.StopKindSpecificBeadClosed,
		scheduler.StopKindBeadQuery,
	}
	for _, k := range achieve {
		if !stopKindIsGoalAchievement(k) {
			t.Errorf("%q should be a goal achievement", k)
		}
	}
	notAchieve := []string{
		scheduler.StopKindMaxRuns,
		scheduler.StopKindWallClock,
		scheduler.StopKindManualForce,
		scheduler.StopKindNoProgressFor,
		"exit_condition",
	}
	for _, k := range notAchieve {
		if stopKindIsGoalAchievement(k) {
			t.Errorf("%q should NOT be a goal achievement", k)
		}
	}
}

// TestMaybeSpawnChainHeir_Enforce_RefusesNoHuman asserts that in enforce mode a
// gardener handing off to an heir is refused when neither the predecessor
// trigger nor its bead carries a resolvable human operator — the heir would
// otherwise spawn crew that boot unattributed (task ka-0000002839).
func TestMaybeSpawnChainHeir_Enforce_RefusesNoHuman(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")

	d := newFakeChainSpawnDaemon()
	// Predecessor with NO human operator (fleet sentinel only).
	d.beads["gd-pred00002"] = &beadsapi.BeadDetail{
		ID:     "gd-pred00002",
		Labels: []string{"gardener-epic:gd-epic00002", "project:gardenio", "role:gardener"},
		Fields: map[string]string{"project": "gardenio", "operated_by": "fleet:gardener"},
	}
	chain := &ChainSpec{To: &ChainTo{Phase: "impl", TerminalGoal: "ship", Epic: "{{same}}"}}
	chain.Normalize()
	trig := TriggerDef{ID: "gd-pred00002", Project: "gardenio", OperatedBy: "fleet:gardener", Chain: chain}

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed)

	if len(d.created) != 0 {
		t.Errorf("enforce mode: expected no heir created without a human operator, got %d", len(d.created))
	}
}

// TestMaybeSpawnChainHeir_Enforce_AllowsHuman asserts enforce mode still hands
// off when the predecessor carries a resolvable human operator.
func TestMaybeSpawnChainHeir_Enforce_AllowsHuman(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")

	d := newFakeChainSpawnDaemon()
	d.beads["gd-pred00003"] = &beadsapi.BeadDetail{
		ID:     "gd-pred00003",
		Labels: []string{"gardener-epic:gd-epic00003", "project:gardenio", "role:gardener"},
		Fields: map[string]string{"project": "gardenio", "operated_by": "slack:U0OPERATOR1"},
	}
	chain := &ChainSpec{To: &ChainTo{Phase: "impl", TerminalGoal: "ship", Epic: "{{same}}"}}
	chain.Normalize()
	trig := TriggerDef{ID: "gd-pred00003", Project: "gardenio", OperatedBy: "slack:U0OPERATOR1", Chain: chain}

	maybeSpawnChainHeir(context.Background(), d, testLogger(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindSpecificBeadClosed)

	if len(d.created) != 1 {
		t.Fatalf("enforce mode: expected one heir created for human operator, got %d", len(d.created))
	}
	var fields map[string]any
	if err := json.Unmarshal(d.created[0].Fields, &fields); err != nil {
		t.Fatalf("unmarshal heir fields: %v", err)
	}
	if fields["operated_by"] != "slack:U0OPERATOR1" {
		t.Errorf("heir operated_by = %v, want inherited human", fields["operated_by"])
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (f *fakeChainSpawnDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (f *fakeChainSpawnDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (f *fakeChainSpawnDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
