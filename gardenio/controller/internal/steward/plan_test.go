package steward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/heraldry"
)

const planMD = `Some preamble the parser skips.
# Add a watering schedule
Water the beds on a timer so nobody has to remember.

## Add the schedule model
A schedule per bed: days and minutes. Add a Schedule type in beds/schedule.go
with the bed id, the weekdays it waters and the minutes per run, and a loader
that reads them from beds.yaml. Done when a test loads the sample file.

### Notes for the gardener
Keep it in one file.
## Run the timer
Fire the valves from the schedule. Add a timer in valves/timer.go that wakes
each minute, reads the schedules, and opens a bed's valve for its minutes on
its days. Done when a test with a fake clock opens the right valve at 06:00.
`

func TestParsePlan(t *testing.T) {
	d, err := ParsePlan(planMD)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if d.Title != "Add a watering schedule" || d.Body != "Water the beds on a timer so nobody has to remember." {
		t.Fatalf("title %q body %q", d.Title, d.Body)
	}
	if len(d.Steps) != 2 || d.Steps[0].Title != "Add the schedule model" || d.Steps[1].Title != "Run the timer" {
		t.Fatalf("steps %+v", d.Steps)
	}
	if !strings.Contains(d.Steps[0].Body, "### Notes for the gardener\nKeep it in one file.") {
		t.Errorf("a deeper heading stays in its step's body: %q", d.Steps[0].Body)
	}
}

func TestParsePlan_Refuses(t *testing.T) {
	many := "# Big\n"
	for i := range MaxPlanSteps + 1 {
		many += fmt.Sprintf("## step %d\n", i)
	}
	for name, md := range map[string]string{
		"no title":       "## a step\n",
		"no steps":       "# A plan\nwith a body only\n",
		"empty step":     "# A plan\n## \nbody\n",
		"too many steps": many,
		"long title":     "# " + strings.Repeat("x", maxPlanTitle+1) + "\n## s\n",
		"empty body":     "# A plan\n## Add the model\n",
	} {
		if _, err := ParsePlan(md); err == nil {
			t.Errorf("%s: parsed, want a refusal", name)
		}
	}
}

// A step a gardener could not build from is refused, and the error names it.
func TestParsePlan_RefusesAThinStep(t *testing.T) {
	md := strings.Replace(planMD, "Fire the valves from the schedule. Add a timer", "Fire the valves.\n\n## Later\nAdd a timer", 1)
	_, err := ParsePlan(md)
	if err == nil || !strings.Contains(err.Error(), "step 2 (Run the timer) has a 16-character body; write at least 200") {
		t.Fatalf("ParsePlan: %v, want step 2 named", err)
	}
}

// planFarm is the farm plus what the plan verbs read and write.
type planFarm struct {
	*notesFarm
	members []beadsapi.LandMember
	deps    []beadsapi.Dependency
	keys    map[string]string
}

func newPlanFarm() *planFarm {
	return &planFarm{
		notesFarm: &notesFarm{fakeDaemon: newFarm()},
		members: []beadsapi.LandMember{
			{Principal: "oidc:ada@example.com", Name: "ada", Role: "owner"},
			{Principal: "oidc:bob@example.com", Name: "bob", Role: "member"},
		},
		keys: map[string]string{},
	}
}

func (f *planFarm) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if id, ok := f.keys[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		return id, nil
	}
	id, err := f.fakeDaemon.CreateBead(ctx, req)
	if err != nil {
		return "", err
	}
	if req.IdempotencyKey != "" {
		f.keys[req.IdempotencyKey] = id
	}
	if b := f.bead(id); b != nil {
		b.Description = req.Description
		b.Heraldry = req.Heraldry
		if req.IdempotencyKey != "" {
			b.Fields["idempotency_key"] = req.IdempotencyKey
		}
		// The daemon reads a non-string field (a decision's options) back as
		// its JSON text; the farm keeps only strings.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(req.Fields, &fields)
		for k, v := range fields {
			if _, ok := b.Fields[k]; !ok {
				b.Fields[k] = string(v)
			}
		}
	}
	return id, nil
}

func (f *planFarm) LandMembers(context.Context, string) ([]beadsapi.LandMember, error) {
	return f.members, nil
}

func (f *planFarm) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	f.deps = append(f.deps, beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOnID, Type: depType})
	return nil
}

func (f *planFarm) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var out []beadsapi.Dependency
	for _, d := range f.deps {
		if d.DependsOnID == id {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *planFarm) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	b := f.bead(id)
	if b == nil {
		return fmt.Errorf("no bead %s", id)
	}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			b.Fields[k] = s
			continue
		}
		raw, _ := json.Marshal(v)
		b.Fields[k] = string(raw)
	}
	return nil
}

func (f *planFarm) CloseBeadAs(_ context.Context, id, _ string, _ map[string]string) error {
	b := f.bead(id)
	if b == nil {
		return fmt.Errorf("no bead %s", id)
	}
	b.Status = "closed"
	return nil
}

func (f *planFarm) hasDep(bead, on, typ string) bool {
	return slices.Contains(f.deps, beadsapi.Dependency{BeadID: bead, DependsOnID: on, Type: typ})
}

// answer closes the plan's decision as the Commons card does.
func (f *planFarm) answer(t *testing.T, decision string, fields map[string]string) {
	t.Helper()
	b := f.bead(decision)
	if b == nil {
		t.Fatalf("no decision %s", decision)
	}
	b.Status = "closed"
	for k, v := range fields {
		b.Fields[k] = v
	}
}

func writeOpts() WritePlanOptions {
	return WritePlanOptions{Thread: "tp-msg-1", StepEstimateCredits: 5, Actor: "tomato-patch-steward-abc", Agent: "tp-agent-9", GiftedBy: "oidc:op@example.com"}
}

func writeTestPlan(t *testing.T, f *planFarm) *WrittenPlan {
	t.Helper()
	d, err := ParsePlan(planMD)
	if err != nil {
		t.Fatal(err)
	}
	w, err := WritePlan(context.Background(), f, land, d, writeOpts())
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	return w
}

// A person's ask becomes a plan /plans shows, in the land, with its steps in
// order, ready, and blocked on one decision to the land's owner that names
// the plan.
func TestWritePlan_WritesThePlanAndAsksTheOwner(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)

	plan := f.bead(w.Plan)
	if plan == nil || plan.Type != "epic" || plan.Title != "Add a watering schedule" ||
		!slices.Contains(plan.Labels, "plan") || !slices.Contains(plan.Labels, "project:"+land) {
		t.Fatalf("plan epic %+v", plan)
	}
	if plan.Fields["plan_state"] != "ready" {
		t.Errorf("plan_state %q, want ready", plan.Fields["plan_state"])
	}
	if threadOf(plan.Description) != "tp-msg-1" {
		t.Errorf("plan does not record its thread: %q", plan.Description)
	}
	if len(w.Steps) != 2 {
		t.Fatalf("steps %v", w.Steps)
	}
	if got := stepOrder(plan.Fields["step_order"]); !slices.Equal(got, w.Steps) {
		t.Errorf("step_order %v, want %v", got, w.Steps)
	}
	for i, id := range w.Steps {
		s := f.bead(id)
		if s.Type != "task" || !slices.Equal(s.Labels, []string{"project:" + land}) || !f.hasDep(id, w.Plan, "parent-child") {
			t.Errorf("step %s: %+v", id, s)
		}
		if i > 0 && !f.hasDep(id, w.Steps[i-1], "blocks") {
			t.Errorf("step %s does not wait on %s", id, w.Steps[i-1])
		}
	}

	dec := f.bead(w.Decision)
	if dec == nil || dec.Type != "decision" || !slices.Equal(dec.Labels, []string{"project:" + land}) {
		t.Fatalf("decision %+v", dec)
	}
	if w.Owner != "oidc:ada@example.com" || dec.Fields["operated_by"] != w.Owner || dec.Fields["gifted_by"] != "oidc:op@example.com" {
		t.Errorf("decision is for %q (operated_by %q, gifted_by %q), want ada", w.Owner, dec.Fields["operated_by"], dec.Fields["gifted_by"])
	}
	if dec.Title != "Plan "+w.Plan+": Add a watering schedule - run it?" || dec.Fields["requesting_task"] != w.Plan ||
		dec.Fields["requesting_agent_bead_id"] != "tp-agent-9" {
		t.Errorf("decision %q fields %v", dec.Title, dec.Fields)
	}
	for _, want := range []string{"Water the beds", "1. Add the schedule model (" + w.Steps[0] + ")", "2. Run the timer", "About 10.00 credits: 2 steps"} {
		if !strings.Contains(dec.Fields["context"], want) {
			t.Errorf("decision context missing %q:\n%s", want, dec.Fields["context"])
		}
	}
	// The options owe no artifact, so an answer never leaves the steward a
	// report to file before it may stop.
	var opts []map[string]string
	if err := json.Unmarshal([]byte(dec.Fields["options"]), &opts); err != nil || len(opts) != 3 {
		t.Fatalf("options %s (%v)", dec.Fields["options"], err)
	}
	for i, id := range []string{PlanApprove, PlanChange, PlanDrop} {
		if opts[i]["id"] != id || opts[i]["artifact_type"] != "" {
			t.Errorf("option %d = %v", i, opts[i])
		}
	}
	if !f.hasDep(w.Plan, w.Decision, "blocks") {
		t.Error("the plan is not blocked on its decision")
	}
	// The steward's decision wake matches a decision of the land created by
	// a steward run (<land>-steward-<suffix>): this run, by its actor.
	for _, c := range f.created {
		if c.Type == "decision" && c.CreatedBy != "tomato-patch-steward-abc" {
			t.Errorf("decision created by %q, want the steward run", c.CreatedBy)
		}
	}
}

func TestWritePlan_OwnerMustBeOne(t *testing.T) {
	d, _ := ParsePlan(planMD)
	ctx := context.Background()

	f := newPlanFarm()
	f.members[0].Role = "member"
	if _, err := WritePlan(ctx, f, land, d, writeOpts()); err == nil || !strings.Contains(err.Error(), "no owner recorded") {
		t.Errorf("no owner: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("wrote %d beads before finding nobody to ask", len(f.created))
	}

	f = newPlanFarm()
	f.members[1].Role = "owner"
	if _, err := WritePlan(ctx, f, land, d, writeOpts()); err == nil || !strings.Contains(err.Error(), "2 owners") {
		t.Errorf("two owners: %v", err)
	}
	o := writeOpts()
	o.Owner = "Bob@example.com"
	w, err := WritePlan(ctx, f, land, d, o)
	if err != nil || w.Owner != "oidc:bob@example.com" {
		t.Errorf("--for bob: %+v, %v", w, err)
	}
}

func TestWritePlan_RefusesAnOperatorLand(t *testing.T) {
	d, _ := ParsePlan(planMD)
	if _, err := WritePlan(context.Background(), newPlanFarm(), "gardenio", d, writeOpts()); err == nil {
		t.Error("wrote a steward plan in an operator land")
	}
}

// The home garden takes plans: the operator land on the Commons, whose owner
// is the person its project bead names as operator. Another operator land,
// with no Commons, still takes none.
func TestWritePlan_HomeGarden(t *testing.T) {
	f := newPlanFarm()
	home := f.bead("kd-proj")
	home.Fields["land_default_channel"] = "kd-general"
	home.OperatedBy = "oidc:op@example.com"
	d, _ := ParsePlan(planMD)
	w, err := WritePlan(context.Background(), f, "gardenio", d, writeOpts())
	if err != nil {
		t.Fatalf("WritePlan in the home garden: %v", err)
	}
	if !w.Home || w.Owner != "oidc:op@example.com" {
		t.Errorf("home %v owner %q, want the operator", w.Home, w.Owner)
	}
	plan := f.bead(w.Plan)
	if !slices.Contains(plan.Labels, "project:gardenio") || plan.Fields["plan_state"] != "ready" {
		t.Errorf("plan %+v", plan)
	}
	if dec := f.bead(w.Decision); dec.Fields["operated_by"] != "oidc:op@example.com" || dec.Fields["gifted_by"] != "" {
		t.Errorf("decision fields %v", dec.Fields)
	}

	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove})
	a, err := AnswerPlan(context.Background(), f, "gardenio", w.Decision, "x")
	if err != nil || !a.Home || a.State != "building" || a.Owner != "oidc:op@example.com" {
		t.Errorf("answer %+v, %v; want home, building, the operator", a, err)
	}

	home.OperatedBy = "agent:someone"
	if _, err := WritePlan(context.Background(), f, "gardenio", d, writeOpts()); err == nil || !strings.Contains(err.Error(), "not the home garden") {
		t.Errorf("an operator land with no person as operator: %v", err)
	}
}

// Approve refuses a plan with a step its gardener could not build from, names
// the step, and changes nothing; once the step is filled in it applies.
func TestAnswerPlan_RefusesAThinStep(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	f.bead(w.Steps[1]).Description = ""
	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove})
	_, err := AnswerPlan(context.Background(), f, land, w.Decision, "x")
	if err == nil || !strings.Contains(err.Error(), "step 2 "+w.Steps[1]+" (Run the timer) has a 0-character body") {
		t.Fatalf("AnswerPlan: %v, want step 2 named", err)
	}
	if st := f.bead(w.Plan).Fields["plan_state"]; st != "ready" {
		t.Errorf("plan_state %q after a refused approve, want ready", st)
	}
	f.bead(w.Steps[1]).Description = strings.Repeat("Open the valve. ", 15)
	if a, err := AnswerPlan(context.Background(), f, land, w.Decision, "x"); err != nil || a.State != "building" {
		t.Errorf("after the fill: %+v, %v", a, err)
	}
}

// A closed step is never built, so a thin body on one does not stop approve.
func TestAnswerPlan_ClosedThinStepApproves(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	f.bead(w.Steps[0]).Description = ""
	f.bead(w.Steps[0]).Status = "closed"
	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove})
	a, err := AnswerPlan(context.Background(), f, land, w.Decision, "x")
	if err != nil || a.State != "building" || a.Next != w.Steps[1] {
		t.Errorf("AnswerPlan: %+v, %v; want building, next %s", a, err, w.Steps[1])
	}
}

// A phased plan's steps sit under its phases, not the plan: approve checks
// their bodies and names the first open step in phase_order. A child epic the
// plan does not list in phase_order is not a phase.
func TestAnswerPlan_PhasedPlan(t *testing.T) {
	ctx := context.Background()
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	f.deps = slices.DeleteFunc(f.deps, func(d beadsapi.Dependency) bool {
		return d.DependsOnID == w.Plan && d.Type == "parent-child"
	})
	child := func(typ, parent string) string {
		id, err := f.CreateBead(ctx, beadsapi.CreateBeadRequest{Title: typ + " under " + parent, Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		_ = f.AddDependency(ctx, id, parent, "parent-child", "x")
		return id
	}
	first, second, stray := child("epic", w.Plan), child("epic", w.Plan), child("epic", w.Plan)
	_ = f.AddDependency(ctx, w.Steps[1], first, "parent-child", "x")
	_ = f.AddDependency(ctx, w.Steps[0], second, "parent-child", "x")
	child("task", stray)
	_ = f.UpdateBeadFieldsTyped(ctx, w.Plan, map[string]any{"phase_order": []string{first, second}})

	f.bead(w.Steps[1]).Description = ""
	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove})
	if _, err := AnswerPlan(ctx, f, land, w.Decision, "x"); err == nil || !strings.Contains(err.Error(), "step 1 "+w.Steps[1]) {
		t.Fatalf("AnswerPlan: %v, want the thin step 1 %s named", err, w.Steps[1])
	}
	f.bead(w.Steps[1]).Description = strings.Repeat("Open the valve. ", 15)
	a, err := AnswerPlan(ctx, f, land, w.Decision, "x")
	if err != nil || a.State != "building" || a.Next != w.Steps[1] {
		t.Fatalf("approve: %+v, %v; want building, next %s", a, err, w.Steps[1])
	}
	f.bead(w.Steps[1]).Status = "closed"
	if a, err := AnswerPlan(ctx, f, land, w.Decision, "x"); err != nil || a.Next != w.Steps[0] {
		t.Errorf("second run: %+v, %v; want next %s from the second phase", a, err, w.Steps[0])
	}
}

// A retried write of the same plan for the same ask finds the epic it wrote
// and refuses, so the owner is never asked twice.
func TestWritePlan_RetryDoesNotAskTwice(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	d, _ := ParsePlan(planMD)
	_, err := WritePlan(context.Background(), f, land, d, writeOpts())
	if !errors.Is(err, ErrPlanWritten) || !strings.Contains(err.Error(), w.Plan) {
		t.Fatalf("retry: %v, want ErrPlanWritten naming %s", err, w.Plan)
	}
	decisions := 0
	for _, c := range f.created {
		if c.Type == "decision" {
			decisions++
		}
	}
	if decisions != 1 {
		t.Errorf("%d decisions, want 1", decisions)
	}
}

// Approve moves the plan to building and names the first step; a second run
// on the same answer changes nothing and names the step still to do.
func TestAnswerPlan_Approve(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove, "rationale": "Chosen by ada via the Commons", "responded_by": "oidc:ada@example.com"})

	a, err := AnswerPlan(context.Background(), f, land, w.Decision, "tomato-patch-steward-def")
	if err != nil {
		t.Fatalf("AnswerPlan: %v", err)
	}
	if a.Answer != PlanApprove || a.State != "building" || !a.Applied || a.Plan != w.Plan || a.Thread != "tp-msg-1" {
		t.Fatalf("answer %+v", a)
	}
	if a.Next != w.Steps[0] || a.NextStatus != "open" || a.Words != "" {
		t.Errorf("next %s (%s), words %q; want %s open and no words (the surface's stamp is not theirs)", a.Next, a.NextStatus, a.Words, w.Steps[0])
	}
	if f.bead(w.Plan).Fields["plan_state"] != "building" {
		t.Error("plan_state not building")
	}

	f.bead(w.Steps[0]).Status = "closed"
	a, err = AnswerPlan(context.Background(), f, land, w.Decision, "tomato-patch-steward-ghi")
	if err != nil || a.Applied || a.Next != w.Steps[1] {
		t.Errorf("second run: %+v, %v; want unchanged, next %s", a, err, w.Steps[1])
	}
}

// Change and drop abandon the plan and close it with its open steps; the
// owner's words come back for the rewrite.
func TestAnswerPlan_ChangeAndDrop(t *testing.T) {
	for _, tc := range []struct{ chosen, want string }{{PlanChange, PlanChange}, {"3", PlanDrop}} {
		f := newPlanFarm()
		w := writeTestPlan(t, f)
		f.answer(t, w.Decision, map[string]string{"chosen": tc.chosen, "rationale": "water at dawn, not dusk"})
		a, err := AnswerPlan(context.Background(), f, land, w.Decision, "tomato-patch-steward-def")
		if err != nil {
			t.Fatalf("%s: %v", tc.chosen, err)
		}
		if a.Answer != tc.want || a.State != "abandoned" || !a.Applied || a.Words != "water at dawn, not dusk" {
			t.Errorf("%s: %+v", tc.chosen, a)
		}
		plan := f.bead(w.Plan)
		if plan.Status != "closed" || plan.Fields["plan_state"] != "abandoned" {
			t.Errorf("%s: plan %s %s", tc.chosen, plan.Status, plan.Fields["plan_state"])
		}
		for _, s := range w.Steps {
			if f.bead(s).Status != "closed" {
				t.Errorf("%s: step %s left open", tc.chosen, s)
			}
		}
		if a, err := AnswerPlan(context.Background(), f, land, w.Decision, "x"); err != nil || a.Applied {
			t.Errorf("%s: second run %+v, %v", tc.chosen, a, err)
		}
	}
}

// The changed plan replaces the old one: linked, and said on the card.
func TestWritePlan_Replaces(t *testing.T) {
	f := newPlanFarm()
	old := writeTestPlan(t, f)
	d, _ := ParsePlan(strings.Replace(planMD, "# Add a watering schedule", "# Water at dawn", 1))
	o := writeOpts()
	o.Replaces = old.Plan
	w, err := WritePlan(context.Background(), f, land, d, o)
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	if !f.hasDep(w.Plan, old.Plan, "related") || !strings.Contains(f.bead(w.Decision).Fields["context"], "replaces plan "+old.Plan) {
		t.Error("the new plan does not say what it replaces")
	}
	o.Replaces = general
	if _, err := WritePlan(context.Background(), f, land, d, o); err == nil {
		t.Error("replaced a bead that is not a plan")
	}
}

// A dismissed card, a written answer with no option, an open decision, and a
// decision of another land change nothing.
func TestAnswerPlan_NothingToApply(t *testing.T) {
	ctx := context.Background()
	for name, fields := range map[string]map[string]string{
		"dismissed": {"chosen": "dismissed"},
		"written":   {"response_text": "what would it cost per month?"},
	} {
		f := newPlanFarm()
		w := writeTestPlan(t, f)
		f.answer(t, w.Decision, fields)
		a, err := AnswerPlan(ctx, f, land, w.Decision, "x")
		if err != nil || a.Applied || a.State != "ready" {
			t.Errorf("%s: %+v, %v", name, a, err)
		}
		if name == "written" && a.Words != "what would it cost per month?" {
			t.Errorf("written answer lost: %q", a.Words)
		}
	}

	f := newPlanFarm()
	w := writeTestPlan(t, f)
	if _, err := AnswerPlan(ctx, f, land, w.Decision, "x"); err == nil {
		t.Error("applied an open decision")
	}
	f.answer(t, w.Decision, map[string]string{"chosen": PlanApprove})
	if _, err := AnswerPlan(ctx, f, "other-land", w.Decision, "x"); err == nil {
		t.Error("applied another land's decision")
	}
}

// The run is told the wake that fired it: the entry's label and its bead
// (none on the digest), and where each entry starts.
func TestRenderPrompt_ThisWake(t *testing.T) {
	l := testLand()
	l.WakeEntry, l.WakeBead = WakeDecision, "tp-dec-1"
	got, err := RenderPrompt(l)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "## This wake\n\nWoken by: decision-resolved, bead tp-dec-1\n") {
		t.Errorf("wake not named:\n%s", got)
	}
	l.WakeEntry, l.WakeBead = WakeDigest, ""
	if got, _ = RenderPrompt(l); !strings.Contains(got, "Woken by: daily-digest\n") {
		t.Errorf("digest wake:\n%s", got)
	}
	for _, label := range []string{WakeGeneral, WakeMention, WakeDecision, WakeGardenerFinished, WakeMachines, WakeNotes, WakeCreditGrant, WakePurseThreshold, WakeOpened, WakeDigest} {
		if !strings.Contains(got, "- "+label) && !strings.Contains(got, " or "+label) {
			t.Errorf("the playbook does not say where %s starts", label)
		}
	}
}

// The fire-time renderer passes the engine's FireEvent through to the
// playbook.
func TestTriggerPromptRenderer_NamesTheWake(t *testing.T) {
	f := newPlanFarm()
	trig := bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}
	got, err := TriggerPromptRenderer(f)(context.Background(), trig, bridge.FireEvent{Entry: WakeDecision, BeadID: "tp-dec-1", EventType: "closed"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "Woken by: decision-resolved, bead tp-dec-1") {
		t.Errorf("wake not passed through:\n%s", got)
	}
}

// A member's recorded land role reaches the playbook, so the steward can see
// who the owner is; an unknown role is not guessed up.
func TestLandRole(t *testing.T) {
	for in, want := range map[string]string{"owner": "owner", "member": "member", "guest": "guest", "admin": "", "": ""} {
		if got := string(landRole(in)); got != want {
			t.Errorf("landRole(%q) = %q, want %q", in, got, want)
		}
	}
}

// armedPlanMD is planMD with an Arms: line in the owner's paragraph.
var armedPlanMD = strings.Replace(planMD, "# Add a watering schedule\n", "# Add a watering schedule\nArms: 🚿 ⏰📅\n", 1)

// The Arms: line is the plan's arms, in the library's spelling, and leaves
// the owner's paragraph; one in a step's body is the step's words.
func TestParsePlan_Arms(t *testing.T) {
	d, err := ParsePlan(armedPlanMD)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if d.Arms != "🚿⏰📅" || d.Body != "Water the beds on a timer so nobody has to remember." {
		t.Errorf("arms %q body %q", d.Arms, d.Body)
	}
	if d, _ := ParsePlan(planMD); d.Arms != "" {
		t.Errorf("a plan with no Arms: line has arms %q", d.Arms)
	}
	inStep := strings.Replace(planMD, "Keep it in one file.", "Arms: not the plan's.\nKeep it in one file.", 1)
	if d, err := ParsePlan(inStep); err != nil || d.Arms != "" || !strings.Contains(d.Steps[0].Body, "Arms: not the plan's.") {
		t.Errorf("Arms: in a step: arms %q, %v", d.Arms, err)
	}
}

// Arms kd heraldry check would refuse are refused with its words, and so is
// a second Arms: line.
func TestParsePlan_RefusesBadArms(t *testing.T) {
	for _, tc := range []struct{ arms, why string }{
		{"🚿⏰", "arms are three to five emoji"},
		{"🚿⏰📅🔥🧭📦", "arms are three to five emoji"},
		{"🚿⏰🚿", "🚿 is used twice"},
		{"🚿⏰💧", "💧 is not in the library"},
		{"", "arms are three to five emoji"},
	} {
		md := strings.Replace(planMD, "# Add a watering schedule\n", "# Add a watering schedule\nArms: "+tc.arms+"\n", 1)
		_, err := ParsePlan(md)
		if want := "the plan's Arms: line: invalid arms: " + tc.why + ". " + heraldry.Help; err == nil || err.Error() != want {
			t.Errorf("Arms: %q: %v, want %q", tc.arms, err, want)
		}
	}
	twice := strings.Replace(armedPlanMD, "Water the beds", "arms: 🔥🧭📦\nWater the beds", 1)
	if _, err := ParsePlan(twice); err == nil || !strings.Contains(err.Error(), "2 Arms: lines") {
		t.Errorf("two Arms: lines: %v", err)
	}
}

func writeArmedPlan(t *testing.T, f *planFarm) (*WrittenPlan, error) {
	t.Helper()
	d, err := ParsePlan(armedPlanMD)
	if err != nil {
		t.Fatal(err)
	}
	return WritePlan(context.Background(), f, land, d, writeOpts())
}

// A plan written with arms bears them from its creation, before its
// decision is filed, and the card's context names them as the run.
func TestWritePlan_Arms(t *testing.T) {
	f := newPlanFarm()
	w, err := writeArmedPlan(t, f)
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	if w.Arms != "🚿⏰📅" || len(w.Warnings) != 0 {
		t.Errorf("written arms %q warnings %q", w.Arms, w.Warnings)
	}
	if got := f.bead(w.Plan).Heraldry; got != "🚿⏰📅" {
		t.Errorf("plan epic heraldry %q", got)
	}
	if c := f.created[0]; c.Type != "epic" || c.Heraldry != "🚿⏰📅" {
		t.Errorf("first write %s heraldry %q, want the epic with its arms", c.Type, c.Heraldry)
	}
	if desc := f.bead(w.Plan).Description; strings.Contains(desc, "Arms:") {
		t.Errorf("the Arms: line stayed in the description: %q", desc)
	}
	if ctx := f.bead(w.Decision).Fields["context"]; !strings.HasPrefix(ctx, "Arms: 🚿⏰📅\n\nWater the beds") {
		t.Errorf("decision context does not open with the arms:\n%s", ctx)
	}
	// A retry of the same ask finds its own plan, not a bead bearing the arms.
	if _, err := writeArmedPlan(t, f); !errors.Is(err, ErrPlanWritten) {
		t.Errorf("retry: %v, want ErrPlanWritten", err)
	}
}

// A plan without an Arms: line is written, and the writer is told to choose
// its arms and how.
func TestWritePlan_NoArmsWarns(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	if w.Arms != "" || f.bead(w.Plan).Heraldry != "" || len(w.Warnings) != 1 {
		t.Fatalf("arms %q warnings %q", w.Arms, w.Warnings)
	}
	for _, want := range []string{"plan " + w.Plan + " has no arms", "before ada@example.com looks",
		"kd heraldry check \"<emoji>\" --project " + land, "kd update " + w.Plan + " --heraldry"} {
		if !strings.Contains(w.Warnings[0], want) {
			t.Errorf("warning missing %q: %s", want, w.Warnings[0])
		}
	}
	if strings.Contains(f.bead(w.Decision).Fields["context"], "Arms:") {
		t.Error("a plan with no arms names arms on its card")
	}
}

// Arms another bead of the land bears are refused in the daemon's words
// before anything is written; the same emoji in another order is a warning.
func TestWritePlan_ArmsRegister(t *testing.T) {
	f := newPlanFarm()
	f.beads = append(f.beads, &beadsapi.BeadDetail{ID: "tp-old", Type: "epic", Status: "open", ShortTitle: "old plan",
		Heraldry: "🚿⏰📅", Labels: []string{"project:" + land}, Fields: map[string]string{}})
	_, err := writeArmedPlan(t, f)
	want := "the arms 🚿⏰📅 are already borne by tp-old (old plan) in project " + land + "; arms are unique in a project, so choose other emoji or another order"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("taken arms: %v, want %q", err, want)
	}
	if len(f.created) != 0 {
		t.Errorf("wrote %d beads before refusing the arms", len(f.created))
	}

	f = newPlanFarm()
	f.beads = append(f.beads,
		&beadsapi.BeadDetail{ID: "tp-old", Type: "epic", Status: "open", Heraldry: "📅⏰🚿", Labels: []string{"project:" + land}, Fields: map[string]string{}},
		&beadsapi.BeadDetail{ID: "xx-other", Type: "epic", Status: "open", Heraldry: "🚿⏰📅", Labels: []string{"project:elsewhere"}, Fields: map[string]string{}})
	w, err := writeArmedPlan(t, f)
	if err != nil {
		t.Fatalf("look-alike or another land's arms: %v", err)
	}
	if len(w.Warnings) != 1 || !strings.Contains(w.Warnings[0], "look like tp-old's, 📅⏰🚿: the same emoji in another order") {
		t.Errorf("warnings %q", w.Warnings)
	}
}
