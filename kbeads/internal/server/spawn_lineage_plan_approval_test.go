package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// A plan approved from a thread binds a SEALED plan-gardener (gd-YaoqmhIx8x):
// the thread binds on its own token, and the server seals the trigger to the
// admin who answered approve on the plan's decision, so the engine's first
// scheduled fire spawns without an escalation.

const (
	approvalPlan     = "gd-plan"
	approvalDecision = "gd-dec"
	// approvalThread is the thread agent bead that filed the plan's decision.
	approvalThread     = "gd-thread1"
	approvalThreadName = "thread-1793060214-927339"
)

// threadCtx is a thread agent writing on its per-agent token, its writes
// attributed to the owner it acts for.
func threadCtx() context.Context {
	return ownerAgentCtx(approvalThread, approvalThreadName)
}

// ownerAgentCtx is any agent writing on its own per-agent token on the owner's
// behalf.
func ownerAgentCtx(beadID, name string) context.Context {
	return WithOperatedByHeader(withAuthMethod(agentFleetCtx(beadID, name), authMethodStatic), lineageAdmin)
}

type planApprovalFixture struct {
	*lineageFixture
}

// newPlanApprovalFixture seeds a building plan and the owner's approve answer
// on its decision, answered two minutes before the server's clock.
func newPlanApprovalFixture(t *testing.T) *planApprovalFixture {
	f := &planApprovalFixture{newLineageFixture(t)}
	makeBypassRoleBead(f.ms, "gd-role-plan-gardener", planGardenerSpawnRole)
	f.ms.beads[approvalPlan] = &model.Bead{
		ID: approvalPlan, Type: model.TypeEpic, Kind: model.KindIssue, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"plan_state":"building"}`),
	}
	f.ms.labels[approvalPlan] = []string{"plan", "project:gardenio"}
	f.decision(approvalDecision, f.now.Add(-2*time.Minute), nil)
	return f
}

// decision stores a closed decision gating the plan, filed by approvalThread on
// its own token and answered at closedAt. mutate edits its fields.
func (f *planApprovalFixture) decision(id string, closedAt time.Time, mutate map[string]any) {
	fields := map[string]any{
		"requesting_task":                    approvalPlan,
		decisionRequestingAgentField:         approvalThread,
		decisionRequestingAgentVerifiedField: "true",
		"chosen":                             planApprovalOption,
		"responded_by":                       "admin@example.com",
		decisionResolverSubjectField:         "admin@example.com",
		decisionResolverAdminField:           true,
	}
	for k, v := range mutate {
		if v == nil {
			delete(fields, k)
			continue
		}
		fields[k] = v
	}
	raw, _ := json.Marshal(fields)
	f.ms.beads[id] = &model.Bead{
		ID: id, Type: model.TypeDecision, Kind: model.KindData, Status: model.StatusClosed,
		CreatedBy: approvalThreadName, ClosedAt: &closedAt, Fields: raw,
	}
	f.ms.labels[id] = []string{"project:gardenio"}
	f.ms.deps[approvalPlan] = append(f.ms.deps[approvalPlan], &model.Dependency{
		BeadID: approvalPlan, DependsOnID: id, Type: model.DepBlocks,
	})
}

const planGardenerTriggerFields = `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","cron":"12,42 * * * *","prompt":"You are the bound plan-gardener for plan gd-plan"}`

func planGardenerLabels(plan string) []string {
	return []string{"project:gardenio", "role:gardener", "plan-gardener", "trigger:schedule", gardenerEpicLabelPrefix + plan}
}

// bind creates the plan's gardener the way plangardener.Bind does, as ctx, and
// stores it under id.
func (f *planApprovalFixture) bind(ctx context.Context, id, fields string, labels []string) {
	f.t.Helper()
	b, err := f.srv.createBead(ctx, createBeadInput{
		Title: "plan gardener", Type: "trigger", Kind: "data",
		Labels: labels, Fields: json.RawMessage(fields),
	})
	if err != nil {
		f.t.Fatalf("bind plan gardener: %v", err)
	}
	stored := f.ms.beads[b.ID]
	delete(f.ms.beads, b.ID)
	stored.ID = id
	f.ms.beads[id] = stored
	f.ms.labels[id] = labels
}

func (f *planApprovalFixture) engineFire(id string) error {
	return f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", planGardenerSpawnRole, id))
}

func TestPlanApprovalSeal_ThreadBindAfterOwnerApproves(t *testing.T) {
	f := newPlanApprovalFixture(t)
	f.bind(threadCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))

	if got := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageField]; got == nil {
		t.Fatal("a plan-gardener bound from a thread after the owner approved must carry spawn_lineage")
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != lineageAdmin {
		t.Fatalf("seal author = %q, want the approving admin %q", got, lineageAdmin)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("the first scheduled fire must spawn without an escalation, got %v", err)
	}
	// An agent's own token still cannot spawn the privileged role: the seal
	// authorizes the engine's fires only.
	requireEscalates(t, f.spawn(WithOperatedByHeader(staticCtx(), lineageAdmin), spawnFieldsWithTrigger("gardenio", planGardenerSpawnRole, "gd-trig")))
}

func TestPlanApprovalSeal_ThreadEnablesPausedGardenerAfterApproval(t *testing.T) {
	f := newPlanApprovalFixture(t)
	paused := `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","enabled":false}`
	f.bind(threadCtx(), "gd-trig", paused, planGardenerLabels(approvalPlan))
	if sealedSpawnAuthor(f.ms.beads["gd-trig"]) != "" {
		t.Fatal("a paused gardener is not sealed at bind")
	}
	if _, err := f.srv.updateBead(threadCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != lineageAdmin {
		t.Fatalf("enabling the approved plan's gardener must seal it to the approver, got %q", got)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("sealed gardener must spawn, got %v", err)
	}
}

func TestPlanApprovalSeal_NotSealedWhen(t *testing.T) {
	ago := func(d time.Duration) func(f *planApprovalFixture) time.Time {
		return func(f *planApprovalFixture) time.Time { return f.now.Add(-d) }
	}
	cases := []struct {
		name   string
		setup  func(f *planApprovalFixture)
		ctx    context.Context // the binder; threadCtx when nil
		fields string
		labels []string
	}{
		{name: "no approval decision", setup: func(f *planApprovalFixture) { delete(f.ms.deps, approvalPlan) }},
		// The approval is the thread's to cash in, not the project's: a second
		// agent that writes the trigger (with any prompt) gets no seal, even in
		// the window before the plan's real gardener is bound.
		{name: "another agent in the project binds the trigger",
			ctx: ownerAgentCtx("gd-crew2", "gardenio-crew-other")},
		{name: "the decision was filed on another agent's credential", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{decisionRequestingAgentField: "gd-crew2"})
		}},
		{name: "the decision's requesting agent is not credential-stamped", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{decisionRequestingAgentVerifiedField: nil})
		}},
		{name: "the owner chose change", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{"chosen": "change"})
		}},
		{name: "answered by a relayed or bridge caller, not an admin session", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{decisionResolverAdminField: false})
		}},
		{name: "resolver stamp missing", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{decisionResolverSubjectField: nil})
		}},
		{name: "the decision asked about another bead", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(time.Minute)(f), map[string]any{"requesting_task": "gd-other"})
		}},
		{name: "the approval is older than the window", setup: func(f *planApprovalFixture) {
			delete(f.ms.deps, approvalPlan)
			f.decision("gd-dec2", ago(planApprovalSealWindow+time.Minute)(f), nil)
		}},
		{name: "the decision is still open", setup: func(f *planApprovalFixture) {
			f.ms.beads[approvalDecision].Status = model.StatusOpen
		}},
		// draft and ready plans never get this far: the R6 enable gate refuses
		// an enabled gardener for them.
		{name: "the plan is complete", setup: func(f *planApprovalFixture) {
			f.ms.beads[approvalPlan].Fields = json.RawMessage(`{"plan_state":"complete"}`)
		}},
		{name: "the plan never entered the lifecycle", setup: func(f *planApprovalFixture) {
			f.ms.beads[approvalPlan].Fields = nil
		}},
		{name: "the epic is not a plan", setup: func(f *planApprovalFixture) {
			f.ms.labels[approvalPlan] = []string{"project:gardenio"}
		}},
		{name: "the trigger spawns another role",
			fields: `{"action":"spawn_agent","role":"reviewer","project":"gardenio"}`},
		{name: "the trigger is in another project",
			fields: `{"action":"spawn_agent","role":"plan-gardener","project":"other"}`,
			labels: []string{"project:other", "role:gardener", gardenerEpicLabelPrefix + approvalPlan}},
		{name: "the trigger is not pinned to the plan",
			labels: []string{"project:gardenio", "role:gardener", "plan-gardener"}},
		{name: "another lane for the plan is already sealed", setup: func(f *planApprovalFixture) {
			f.bind(threadCtx(), "gd-first", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
			if sealedSpawnAuthor(f.ms.beads["gd-first"]) == "" {
				t.Fatal("setup: the first lane should be sealed")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanApprovalFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			fields, labels := tc.fields, tc.labels
			if fields == "" {
				fields = planGardenerTriggerFields
			}
			if labels == nil {
				labels = planGardenerLabels(approvalPlan)
			}
			ctx := tc.ctx
			if ctx == nil {
				ctx = threadCtx()
			}
			f.bind(ctx, "gd-trig", fields, labels)
			if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != "" {
				t.Fatalf("trigger sealed to %q, want unsealed", got)
			}
		})
	}
}

// Turning a paused gardener on is no back door for another agent: the enable
// re-checks the caller against the decision's requester.
func TestPlanApprovalSeal_OtherAgentEnablingGetsNoSeal(t *testing.T) {
	f := newPlanApprovalFixture(t)
	other := ownerAgentCtx("gd-crew2", "gardenio-crew-other")
	paused := `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","enabled":false,"prompt":"anything"}`
	f.bind(other, "gd-trig", paused, planGardenerLabels(approvalPlan))
	if _, err := f.srv.updateBead(other, "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != "" {
		t.Fatalf("another agent enabling a plan-gardener sealed it to %q, want unsealed", got)
	}
	requireEscalates(t, f.engineFire("gd-trig"))
}

// An answer cannot be moved onto the plan after the fact: the filer asks about
// another bead, the owner answers approve, and repointing requesting_task at the
// plan is refused, so the bind that follows stays unsealed.
func TestPlanApprovalSeal_RequestingTaskRewriteAfterAnswerRefused(t *testing.T) {
	f := newPlanApprovalFixture(t)
	delete(f.ms.deps, approvalPlan)
	f.decision("gd-dec2", f.now.Add(-time.Minute), map[string]any{"requesting_task": "gd-other"})

	_, err := f.srv.updateBead(threadCtx(), "gd-dec2", updateBeadInput{Fields: json.RawMessage(`{"requesting_task":"` + approvalPlan + `"}`)})
	if err == nil || !strings.Contains(err.Error(), "requesting_task is immutable") {
		t.Fatalf("repointing an answered decision's requesting_task must be refused, got %v", err)
	}
	if got := fieldsAsMap(f.ms.beads["gd-dec2"].Fields)["requesting_task"]; got != "gd-other" {
		t.Fatalf("requesting_task = %v, want it unchanged", got)
	}
	f.bind(threadCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != "" {
		t.Fatalf("trigger sealed to %q from an answer about another bead", got)
	}
}

// The owner's enable click (the viewer's gardener toggle and Approve & build
// both write enabled=true as the owner's OIDC session) on a gardener a thread
// minted seals it (gd-RL1uPwUqAF). So does reopening one. An admin edit that
// does not turn the trigger on still adopts nothing.
func TestSpawnLineage_AdminEnableOrReopenAdoptsThreadMintedTrigger(t *testing.T) {
	closed, open := string(model.StatusClosed), string(model.StatusOpen)
	cases := []struct {
		name  string
		stop  updateBeadInput // written by the thread before the owner acts
		start updateBeadInput // the owner's write
		want  bool
	}{
		{"owner enables a paused thread-minted trigger",
			updateBeadInput{Fields: json.RawMessage(`{"enabled":false}`)}, updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}, true},
		{"owner reopens a closed thread-minted trigger",
			updateBeadInput{Status: &closed}, updateBeadInput{Status: &open}, true},
		{"owner edits an unrelated field",
			updateBeadInput{Fields: json.RawMessage(`{"enabled":false}`)}, updateBeadInput{Fields: json.RawMessage(`{"cooldown_minutes":10}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLineageFixture(t)
			f.triggerAs(threadCtx(), "gd-trig", nil)
			if sealedSpawnAuthor(f.ms.beads["gd-trig"]) != "" {
				t.Fatal("setup: a thread-minted trigger is unsealed")
			}
			if _, err := f.srv.updateBead(threadCtx(), "gd-trig", tc.stop); err != nil {
				t.Fatal(err)
			}
			if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", tc.start); err != nil {
				t.Fatal(err)
			}
			got := sealedSpawnAuthor(f.ms.beads["gd-trig"])
			if tc.want && got != lineageAdmin {
				t.Fatalf("seal author = %q, want %q", got, lineageAdmin)
			}
			if !tc.want && got != "" {
				t.Fatalf("an admin edit that does not turn the trigger on sealed it to %q", got)
			}
		})
	}
}
