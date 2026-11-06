package plangardener

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"
)

// fakeClient is a minimal plangardener.Client: it records CreateBead requests
// and serves ListBeadsFiltered off an in-memory bead slice. The FindAnyGardener
// list query is Types=[trigger, schedule] with no labels, and the owner-lookup
// is Types=[user]; both are served here so Bind's two round-trips resolve.
type fakeClient struct {
	beads   []*beadsapi.BeadDetail
	creates []beadsapi.CreateBeadRequest
	nextID  string
	listErr error
}

func (f *fakeClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// user-bead owner lookup — no gardeners match, return empty.
	for _, t := range q.Types {
		if t == "user" {
			return &beadsapi.ListBeadsResult{}, nil
		}
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if b != nil && (b.Type == "trigger" || b.Type == "schedule") {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeClient) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.creates = append(f.creates, req)
	id := f.nextID
	if id == "" {
		id = "gd-cap-new"
	}
	// Persist a minimal bead so a subsequent FindAnyGardener adopts it.
	var fields map[string]any
	_ = json.Unmarshal(req.Fields, &fields)
	sfields := map[string]string{}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			sfields[k] = s
		}
	}
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: id, Title: req.Title, Type: req.Type, Kind: req.Kind,
		Status: "open", Labels: append([]string(nil), req.Labels...), Fields: sfields,
	})
	return id, nil
}

func planEpic(id, title string, planState string, labels ...string) *beadsapi.BeadDetail {
	all := append([]string{PlanLabel, "project:gardenio"}, labels...)
	fields := map[string]string{}
	if planState != "" {
		fields[planStateFieldKey] = planState
	}
	return &beadsapi.BeadDetail{
		ID: id, Title: title, Type: "epic", Kind: "issue",
		Status: "open", Labels: all, Fields: fields,
	}
}

// decodeFields pulls the trigger fields off the single recorded create.
func decodeFields(t *testing.T, req beadsapi.CreateBeadRequest) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("decoding create fields: %v", err)
	}
	return fields
}

// TestBind_BornPausedWhenDraft: a plan in the authoring lifecycle (draft) is
// gated — the gardener is born enabled=false so the scheduler can't fire an
// unapproved plan. This is the R6 approve→build gate.
func TestBind_BornPausedWhenDraft(t *testing.T) {
	for _, st := range []string{"draft", "ready"} {
		t.Run(st, func(t *testing.T) {
			f := &fakeClient{nextID: "gd-cap1"}
			res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", st), BindSpec{})
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if res.Adopted {
				t.Fatal("expected a fresh bind, not an adopt")
			}
			if res.Enabled {
				t.Errorf("plan_state=%s must be born PAUSED (enabled=false)", st)
			}
			fields := decodeFields(t, f.creates[0])
			if got, ok := fields["enabled"].(bool); !ok || got {
				t.Errorf("fields.enabled = %#v, want JSON bool false", fields["enabled"])
			}
		})
	}
}

// TestBind_BornEnabledWhenUnsetOrBuilding: a legacy / hand-marked plan with no
// plan_state (or one already building) is UNGATED — the gardener is born
// enabled, the historical behaviour the backfill relies on.
func TestBind_BornEnabledWhenUnsetOrBuilding(t *testing.T) {
	for _, st := range []string{"", "building"} {
		name := st
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeClient{nextID: "gd-cap1"}
			res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", st), BindSpec{})
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if !res.Enabled {
				t.Errorf("plan_state=%q must be born ENABLED (ungated)", st)
			}
			fields := decodeFields(t, f.creates[0])
			if got, ok := fields["enabled"].(bool); !ok || !got {
				t.Errorf("fields.enabled = %#v, want JSON bool true", fields["enabled"])
			}
		})
	}
}

// TestBind_IdempotentAdopt: a second Bind on a plan that already has a gardener
// (enabled OR paused) adopts the existing one and creates nothing — the
// re-mark / double-submit / retry guard.
func TestBind_IdempotentAdopt(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	epic := planEpic("gd-PLAN1", "P", "draft")

	first, err := Bind(context.Background(), f, epic, BindSpec{})
	if err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	if first.Adopted {
		t.Fatal("first Bind should mint, not adopt")
	}
	if len(f.creates) != 1 {
		t.Fatalf("first Bind should create exactly 1 gardener, got %d", len(f.creates))
	}

	second, err := Bind(context.Background(), f, epic, BindSpec{})
	if err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if !second.Adopted {
		t.Error("second Bind must adopt the existing gardener")
	}
	if second.ID != first.ID {
		t.Errorf("adopt returned %q, want the existing %q", second.ID, first.ID)
	}
	if len(f.creates) != 1 {
		t.Errorf("second Bind must NOT create a duplicate; total creates = %d", len(f.creates))
	}
}

// TestBind_AdoptsPausedGardener: adoption is enabled-state-agnostic — a gardener
// bound PAUSED behind the gate is still recognised on re-run (otherwise every
// re-bind on a still-draft plan would mint a duplicate).
func TestBind_AdoptsPausedGardener(t *testing.T) {
	f := &fakeClient{}
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: "gd-existing", Title: "Plan gardener for gd-PLAN1", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", GardenerEpicLabel("gd-PLAN1"), MarkerLabel},
		Fields: map[string]string{"enabled": "false", "role": SpawnRole},
	})
	res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", "draft"), BindSpec{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if !res.Adopted || res.ID != "gd-existing" {
		t.Errorf("expected adopt of the paused gardener gd-existing, got %+v", res)
	}
	if len(f.creates) != 0 {
		t.Errorf("must not mint over a paused gardener; got %d creates", len(f.creates))
	}
}

// TestBind_GardenerShape locks the gardener bead shape: type=trigger, spawn
// role=plan-gardener, the canonical gardener markers + plan-gardener marker label,
// one_spawn_at_a_time, and the DefaultMaxRuns steering budget.
func TestBind_GardenerShape(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	if _, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "Ship it", ""), BindSpec{}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	req := f.creates[0]
	if req.Type != "trigger" {
		t.Errorf("type = %q, want trigger", req.Type)
	}
	labelSet := map[string]bool{}
	for _, l := range req.Labels {
		labelSet[l] = true
	}
	for _, want := range []string{"trigger:multi", "role:gardener", GardenerEpicLabel("gd-PLAN1"), MarkerLabel, "project:gardenio"} {
		if !labelSet[want] {
			t.Errorf("labels missing %q (have %v)", want, req.Labels)
		}
	}
	if labelSet["trigger:schedule"] {
		t.Errorf("a default bind is event mode and must not carry trigger:schedule (have %v)", req.Labels)
	}
	fields := decodeFields(t, req)
	if got := fields["role"]; got != SpawnRole {
		t.Errorf("fields.role = %v, want %q", got, SpawnRole)
	}
	if got := fields["action"]; got != "spawn_agent" {
		t.Errorf("fields.action = %v, want spawn_agent", got)
	}
	blobRaw, _ := fields["gardener"].(string)
	var blob map[string]any
	if err := json.Unmarshal([]byte(blobRaw), &blob); err != nil {
		t.Fatalf("gardener blob not valid JSON: %v", err)
	}
	if got, _ := blob["one_spawn_at_a_time"].(bool); !got {
		t.Error("gardener.one_spawn_at_a_time = false, want true (a plan never races two agents)")
	}
	if got, ok := blob["run_budget"].(float64); !ok || int(got) != DefaultMaxRuns {
		t.Errorf("gardener.run_budget = %#v, want %d", blob["run_budget"], DefaultMaxRuns)
	}
	if got, _ := blob["assign_bead"].(string); got != "gd-PLAN1" {
		t.Errorf("gardener.assign_bead = %q, want gd-PLAN1", got)
	}
	prompt, _ := fields["prompt"].(string)
	if !strings.Contains(prompt, "gd-PLAN1") {
		t.Error("prompt does not name the plan epic")
	}
	// STEER must teach a steering path the gardener can actually EXECUTE
	// (gd-01nlBEQlLV / bug gd-0BLDvtYoQw). Every `gb plan` write verb rejects an
	// agent identity — it demands an oidc:<email> operator, and agent sessions
	// carry slack:U…/agent ids — so mandating `gb plan add-step` sent every
	// gardener into a dead end. The agent-callable path is kd create + kd dep add.
	if !strings.Contains(prompt, "kd dep add <step-id> gd-PLAN1 --type parent-child") {
		t.Error("prompt must teach the agent-callable step path (kd create + kd dep add parent-child onto the plan)")
	}
	if strings.Contains(prompt, "add it ONLY via") || strings.Contains(prompt, "NEVER kd create + dep add") {
		t.Error("prompt must not mandate the oidc-only `gb plan add-step` nor forbid the kd path — agents cannot call the former")
	}
	// The playbook must still teach the phase primitive, not just steps — agents
	// were baking "Phase N" into flat task titles instead of minting phase
	// sub-epics (gd-rNl3a0iWz1). Phase membership is the phase_order overlay,
	// which a gardener writes with `kd update -f phase_order=…`.
	if !strings.Contains(prompt, "phase_order") || !strings.Contains(prompt, "--type=epic") {
		t.Error("prompt must teach phase_order + minting the phase as an epic, so agents group steps into real phases, not flat 'Phase N' tasks")
	}
}

// TestBind_PreflightExcludesOperatorBlockedChildren pins the pre-flight
// dormancy fix (ka-0000000318). A plan gardener's cron pre-flight is an
// open-children bead_query over the plan subtree; without
// exclude_blocked_by_open_types it fires a spin-only run every tick even when
// the ENTIRE open frontier is transitively blocked_by an open
// escalation/decision (the 26%-yield failure mode measured on ka-0000001682).
// The daemon's leaf_daemon transitive exclusion (ka-0000004214) only engages
// when the condition opts in, so the generator MUST emit the field.
func TestBind_PreflightExcludesOperatorBlockedChildren(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	if _, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), BindSpec{}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	fields := decodeFields(t, f.creates[0])
	condRaw, _ := fields["conditions"].(string)
	if condRaw == "" {
		t.Fatal("gardener has no conditions field")
	}
	var conds []map[string]any
	if err := json.Unmarshal([]byte(condRaw), &conds); err != nil {
		t.Fatalf("conditions not valid JSON: %v", err)
	}
	if len(conds) != 1 {
		t.Fatalf("want exactly 1 pre-flight condition, got %d: %s", len(conds), condRaw)
	}
	c := conds[0]
	if c["type"] != "bead_query" || c["parent"] != "gd-PLAN1" {
		t.Errorf("pre-flight is not the plan open-children gate: %v", c)
	}
	ex, _ := c["exclude_blocked_by_open_types"].([]any)
	got := map[string]bool{}
	for _, v := range ex {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	for _, want := range []string{"escalation", "decision"} {
		if !got[want] {
			t.Errorf("pre-flight condition must exclude children blocked by open %q so an all-operator-gated plan goes dormant instead of spinning; have %v", want, ex)
		}
	}
}

// TestRenderPrompt_CloseCriteriaCompletesPlan pins the CLOSE CRITERIA behavior
// (task gd-wL1LR2yxbq): when the plan's close criteria hold, the gardener
// completes the plan itself and tells, rather than filing a decision card. Owner
// ruling 1 on gd-7qUpu15BBK reverses ka-0000004595 (PROPOSE).
func TestRenderPrompt_CloseCriteriaCompletesPlan(t *testing.T) {
	prompt, err := RenderPrompt("gd-PLAN1")
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	i := strings.Index(prompt, "CLOSE CRITERIA")
	j := strings.Index(prompt, "2. STEER")
	if i < 0 || j < i {
		t.Fatalf("prompt has no CLOSE CRITERIA block before STEER")
	}
	block := prompt[i:j]
	for _, want := range []string{
		"gd-7qUpu15BBK",                      // the ruling that authorises self-completion
		"comment the evidence on gd-PLAN1",   // evidence is visible on the plan
		"gb plan complete gd-PLAN1",          // the completion verb
		"gb squawk \"Plan gd-PLAN1 complete", // one line to the plan's channel
		"'gb stop'",
		"gb plan reopen gd-PLAN1", // the owner's undo
	} {
		if !strings.Contains(block, want) {
			t.Errorf("CLOSE CRITERIA block must contain %q", want)
		}
	}
	// No card: the block must not tell the gardener to file a decision.
	if strings.Contains(block, "gb decision create") {
		t.Error("CLOSE CRITERIA must not instruct filing a decision (gb decision create); the gardener completes the plan itself")
	}
	if strings.Contains(block, "do NOT complete the plan yourself") ||
		strings.Contains(block, "do NOT\n          complete the plan yourself") {
		t.Error("CLOSE CRITERIA still forbids the gardener completing the plan")
	}
}

// TestRenderPrompt_ArmsDuty pins the arms duty (gd-YvWv4mR4fu): at the start of
// a run the gardener gives the plan and each armless phase epic its arms,
// choosing, checking and writing them with kd, and names the choice in the run
// comment on the plan.
func TestRenderPrompt_ArmsDuty(t *testing.T) {
	prompt, err := RenderPrompt("gd-PLAN1")
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{
		"0. ARMS",
		"kd heraldry library --search",
		"kd heraldry check",
		"kd update <id> --heraldry",
		"comment on gd-PLAN1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing the arms duty's %q", want)
		}
	}
	if strings.Index(prompt, "0. ARMS") > strings.Index(prompt, "1. ORPHAN WATCHDOG") {
		t.Error("the arms duty must come before the watchdog: it runs at the start of the run")
	}
}

// TestBind_RejectsNonPlanEpic: an epic without the plan label (or a non-epic)
// is refused, keeping "the plan's one gardener" a meaningful invariant.
func TestBind_RejectsNonPlanEpic(t *testing.T) {
	f := &fakeClient{}
	notPlan := &beadsapi.BeadDetail{ID: "gd-E", Type: "epic", Kind: "issue", Status: "open", Labels: []string{"project:gardenio"}}
	if _, err := Bind(context.Background(), f, notPlan, BindSpec{}); err == nil {
		t.Fatal("want an error for an epic without the plan label")
	} else if !strings.Contains(err.Error(), "'plan' label") {
		t.Errorf("error should explain the missing plan label, got: %v", err)
	}

	task := &beadsapi.BeadDetail{ID: "gd-T", Type: "task", Kind: "issue", Status: "open", Labels: []string{PlanLabel}}
	if _, err := Bind(context.Background(), f, task, BindSpec{}); err == nil {
		t.Fatal("want an error for a non-epic bead")
	} else if !strings.Contains(err.Error(), "not an epic") {
		t.Errorf("error should explain the type mismatch, got: %v", err)
	}
	if len(f.creates) != 0 {
		t.Errorf("no gardener must be created for a rejected bead; got %d", len(f.creates))
	}
}

// TestBind_InheritsProjectFromPlan: with no explicit project the gardener
// inherits the plan epic's project label.
func TestBind_InheritsProjectFromPlan(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), BindSpec{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.Project != "gardenio" {
		t.Errorf("project = %q, want gardenio (inherited from the plan label)", res.Project)
	}
}

// TestBind_RefusesEmptyProject locks in the authoring-time project guard
// (ka-0000007480, SEV-1 residual of friction report ka-0000004281). A plan
// epic with no project: label and no --project must FAIL LOUDLY before any
// CreateBead — a project-less gardener would otherwise mint a trigger that
// spawns a repo-/credential-less pod stuck in the "Please run /login" loop.
func TestBind_RefusesEmptyProject(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	// A plan epic that carries the plan label but NO project: label.
	plan := &beadsapi.BeadDetail{
		ID: "gd-PLAN1", Type: "epic", Kind: "issue", Status: "open",
		Labels: []string{PlanLabel}, Fields: map[string]string{},
	}
	_, err := Bind(context.Background(), f, plan, BindSpec{})
	if err == nil {
		t.Fatal("want an error for a project-less plan gardener, got nil")
	}
	if !strings.Contains(err.Error(), "project is empty") {
		t.Errorf("error should explain the empty project, got: %v", err)
	}
	if len(f.creates) != 0 {
		t.Errorf("no gardener must be created when the project is empty; got %d", len(f.creates))
	}
}

// startClient is fakeClient plus the enable write Start makes.
type startClient struct {
	fakeClient
	updates  map[string]map[string]any
	comments []string
}

func (f *startClient) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	if f.updates == nil {
		f.updates = map[string]map[string]any{}
	}
	f.updates[id] = fields
	return nil
}

func (f *startClient) AddComment(_ context.Context, id, _, text string) error {
	f.comments = append(f.comments, id+": "+text)
	return nil
}

// Start on a building plan binds a gardener born enabled; on a plan whose
// gardener was bound paused it enables that one instead of minting a second;
// on a plan not yet building it writes nothing.
func TestStart(t *testing.T) {
	f := &startClient{fakeClient: fakeClient{nextID: "gd-cap1"}}
	res, enabled, err := Start(context.Background(), f, planEpic("gd-PLAN1", "P", "building"), BindSpec{CreatedBy: "x"})
	if err != nil || res.Adopted || !res.Enabled || enabled || len(f.creates) != 1 {
		t.Fatalf("fresh start: %+v enabled=%v err=%v creates=%d", res, enabled, err, len(f.creates))
	}

	f = &startClient{}
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: "gd-cap0", Type: "trigger", Status: "open",
		Labels: []string{"role:gardener", GardenerEpicLabel("gd-PLAN2")},
		Fields: map[string]string{"enabled": "false"},
	})
	res, enabled, err = Start(context.Background(), f, planEpic("gd-PLAN2", "P", "building"), BindSpec{CreatedBy: "x"})
	if err != nil || !res.Adopted || res.ID != "gd-cap0" || !enabled || len(f.creates) != 0 {
		t.Fatalf("paused adopt: %+v enabled=%v err=%v creates=%d", res, enabled, err, len(f.creates))
	}
	if f.updates["gd-cap0"]["enabled"] != true || len(f.comments) != 1 {
		t.Errorf("enable write %v, comments %v", f.updates, f.comments)
	}

	f = &startClient{}
	if _, _, err := Start(context.Background(), f, planEpic("gd-PLAN3", "P", "ready"), BindSpec{}); err == nil || len(f.creates) != 0 {
		t.Errorf("ready plan: err=%v creates=%d, want a refusal and no write", err, len(f.creates))
	}
}

// TestBind_DefaultsToEventMode pins the owner ruling of 2026-11-01
// (gd-gsvlS6YDj3): timed plan gardeners ran 779 times a day and at least 80% of
// runs dispatched nothing, so a bind with no Cron is born in EVENT mode. The
// triggers[] must be exactly what `gb gardener mode --event` builds
// (gardenermode.EventTriggers), with a staggered DAILY watchdog carrying the
// open-children pre-flight, and there must be no timed cron field.
func TestBind_DefaultsToEventMode(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), BindSpec{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	wantWatchdog := gardenermode.StaggeredCron("gd-PLAN1", 24*time.Hour)
	if res.Mode != gardenermode.ModeEvent || res.Cron != wantWatchdog {
		t.Errorf("result mode=%q cron=%q, want event / %q", res.Mode, res.Cron, wantWatchdog)
	}
	fields := decodeFields(t, f.creates[0])
	if _, ok := fields["cron"]; ok {
		t.Errorf("an event-mode gardener must carry no cron field, got %v", fields["cron"])
	}
	if _, ok := fields["timed_cron"]; ok {
		t.Errorf("a freshly bound gardener has no timed cron to restore, got %v", fields["timed_cron"])
	}
	condRaw, _ := fields["conditions"].(string)
	if condRaw == "" {
		t.Fatal("the conditions gate must stay on the bead so a later flip or cadence edit carries it")
	}
	raw, _ := fields["triggers"].(string)
	got, err := scheduler.ParseTriggers(raw)
	if err != nil {
		t.Fatalf("triggers do not parse: %v (%s)", err, raw)
	}
	project, _ := fields["project"].(string)
	want, _ := json.Marshal(gardenermode.EventTriggers("gd-PLAN1", project, wantWatchdog, DefaultTimezone, gardenermode.PreflightFromConditions(condRaw)))
	if raw != string(want) {
		t.Fatalf("triggers = %s\nwant the gb gardener mode --event shape %s", raw, want)
	}
	if project != "gardenio" {
		t.Fatalf("the gardener inherits the plan's project, got %q", project)
	}
	if len(got) != 3 || got[0].Label != "plan-activity" || got[1].Label != "steward-nudge" || got[2].Label != "fallback-watchdog" {
		t.Fatalf("want [plan-activity, steward-nudge, fallback-watchdog], got %+v", got)
	}
	if !reflect.DeepEqual(got[0].Match, gardenermode.PlanActivityMatch("gd-PLAN1")) {
		t.Errorf("plan-activity must match the plan subtree, got %+v", got[0].Match)
	}
	if pf := got[2].Preflight; pf == nil || pf.Type != "bead_query" || pf.Parent != "gd-PLAN1" {
		t.Errorf("watchdog must carry the open-children pre-flight, got %+v", pf)
	}
	var m, h int
	var rest string
	if _, err := fmtSscan(got[2].Cron, &m, &h, &rest); err != nil || !strings.HasSuffix(got[2].Cron, " * * *") {
		t.Errorf("watchdog cron %q is not a daily M H * * * cron", got[2].Cron)
	}
}

// TestBind_FallbackCronOverridesWatchdog: an explicit FallbackCron replaces the
// daily default and stays in event mode.
func TestBind_FallbackCronOverridesWatchdog(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), BindSpec{FallbackCron: "5 3 * * 1"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.Mode != gardenermode.ModeEvent || res.Cron != "5 3 * * 1" {
		t.Errorf("result mode=%q cron=%q, want event / 5 3 * * 1", res.Mode, res.Cron)
	}
	raw, _ := decodeFields(t, f.creates[0])["triggers"].(string)
	got, err := scheduler.ParseTriggers(raw)
	if err != nil || len(got) != 3 || got[2].Cron != "5 3 * * 1" {
		t.Errorf("watchdog = %s (err %v), want cron 5 3 * * 1", raw, err)
	}
}

// TestBind_TimedOptIn: Cron or Timed keeps today's timed shape exactly —
// trigger:schedule, a flat cron (the given one, or the staggered 30m default),
// the top-level conditions gate, and no triggers[].
func TestBind_TimedOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec BindSpec
		cron string
	}{
		{"timed", BindSpec{Timed: true}, gardenermode.StaggeredCron("gd-PLAN1", 30*time.Minute)},
		{"cron", BindSpec{Cron: "0 9 * * *"}, "0 9 * * *"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{nextID: "gd-cap1"}
			res, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), tc.spec)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if res.Mode != gardenermode.ModeTimed || res.Cron != tc.cron {
				t.Errorf("result mode=%q cron=%q, want timed / %q", res.Mode, res.Cron, tc.cron)
			}
			req := f.creates[0]
			labels := strings.Join(req.Labels, ",")
			if !strings.Contains(labels, "trigger:schedule") || strings.Contains(labels, "trigger:multi") {
				t.Errorf("timed labels = %v, want trigger:schedule and no trigger:multi", req.Labels)
			}
			fields := decodeFields(t, req)
			if fields["cron"] != tc.cron {
				t.Errorf("fields.cron = %v, want %q", fields["cron"], tc.cron)
			}
			if _, ok := fields["triggers"]; ok {
				t.Errorf("a timed gardener carries no triggers[], got %v", fields["triggers"])
			}
			if c, _ := fields["conditions"].(string); c == "" {
				t.Error("a timed gardener keeps the top-level conditions gate")
			}
		})
	}
}

// TestBind_FallbackCronWithTimedIsRefused: a watchdog only means something in
// event mode, so asking for both is a usage error and nothing is created.
func TestBind_FallbackCronWithTimedIsRefused(t *testing.T) {
	f := &fakeClient{nextID: "gd-cap1"}
	if _, err := Bind(context.Background(), f, planEpic("gd-PLAN1", "P", ""), BindSpec{Timed: true, FallbackCron: "0 4 * * *"}); err == nil {
		t.Fatal("want an error for a fallback cron on a timed bind")
	}
	if len(f.creates) != 0 {
		t.Errorf("no gardener must be created; got %d", len(f.creates))
	}
}

func fmtSscan(s string, m, h *int, rest *string) (int, error) {
	return fmt.Sscanf(s, "%d %d %s", m, h, rest)
}

// TestRenderPrompt_OverlapHold pins the dispatch hold (gd-h7awQXGs8Z): DRIVE
// runs gb ready with --hold-overlaps, waits on a HELD row until its PR merges,
// logs the hold and the later dispatch on the plan, and dispatches an expired
// hold with a comment.
func TestRenderPrompt_OverlapHold(t *testing.T) {
	prompt, err := RenderPrompt("gd-PLAN1")
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	i := strings.Index(prompt, "3. DRIVE")
	j := strings.Index(prompt, "4. CONVERGE")
	if i < 0 || j < i {
		t.Fatalf("prompt has no DRIVE block before CONVERGE")
	}
	block := prompt[i:j]
	for _, want := range []string{
		"gb ready --plan gd-PLAN1\n   --hold-overlaps",
		"HELD",
		"held <step> behind #<pr>",
		"dispatched <step>, #<pr> merged",
		"HOLD EXPIRED",
		"after a 12h hold",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("DRIVE block must contain %q", want)
		}
	}
}
