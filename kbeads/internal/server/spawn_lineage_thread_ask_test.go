package server

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// A plan the home garden's owner asked for in their own thread starts building
// with no approve card (P8.7), so the thread's bind of its gardener is sealed to
// the owner from the ask: the thread head the owner opened, the plan bound to
// that thread, the thread agent bound to it too. Anything else stays unsealed.

const askThreadHead = "gd-head1"

// newThreadAskFixture is a building home-garden plan the thread agent wrote in
// the owner's thread a minute ago, with no decision at all.
func newThreadAskFixture(t *testing.T) *planApprovalFixture {
	f := newPlanApprovalFixture(t)
	f.ms.deps[approvalPlan] = nil
	homeProjectBead(f.ms, "gardenio", homeFields, homeOwner)
	f.ms.beads[askThreadHead] = &model.Bead{
		ID: askThreadHead, Type: model.TypeTask, Kind: model.KindIssue, Status: model.StatusOpen,
		CreatedBy: "Owner@Example.com",
	}
	f.ms.labels[askThreadHead] = []string{"project:gardenio", "source:slack", threadHeadLabel}
	f.ms.beads[approvalThread].Fields = json.RawMessage(`{"role":"thread","project":"gardenio","thread_id":"` + askThreadHead + `"}`)
	plan := f.ms.beads[approvalPlan]
	plan.Fields = json.RawMessage(`{"plan_state":"building","thread_id":"` + askThreadHead + `"}`)
	plan.CreatedAt = f.now.Add(-time.Minute)
	return f
}

func TestThreadAskSeal_ThreadBindIsSealedToTheOwner(t *testing.T) {
	f := newThreadAskFixture(t)
	f.bind(threadCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != homeOwner {
		t.Fatalf("seal author = %q, want the home owner %q", got, homeOwner)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("the engine's first fire must spawn without an escalation, got %v", err)
	}
	// One ask buys one gardener lane.
	f.bind(threadCtx(), "gd-trig2", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if sealedSpawnAuthor(f.ms.beads["gd-trig2"]) != "" {
		t.Fatal("a second gardener lane for the plan was sealed")
	}
}

func TestThreadAskSeal_EnablingAPausedGardenerSeals(t *testing.T) {
	f := newThreadAskFixture(t)
	paused := `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","enabled":false}`
	f.bind(threadCtx(), "gd-trig", paused, planGardenerLabels(approvalPlan))
	if sealedSpawnAuthor(f.ms.beads["gd-trig"]) != "" {
		t.Fatal("a paused gardener is not sealed at bind")
	}
	if _, err := f.srv.updateBead(threadCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != homeOwner {
		t.Fatalf("enabled gardener seal author = %q, want %q", got, homeOwner)
	}
}

func TestThreadAskSeal_NotSealedWhen(t *testing.T) {
	setFields := func(id, raw string) func(*planApprovalFixture) {
		return func(f *planApprovalFixture) { f.ms.beads[id].Fields = json.RawMessage(raw) }
	}
	for name, mutate := range map[string]func(*planApprovalFixture){
		"an agent opened the thread": func(f *planApprovalFixture) {
			f.ms.beads[askThreadHead].CreatedBy = "gardenio-crew-abc"
		},
		"another person opened the thread": func(f *planApprovalFixture) {
			f.ms.beads[askThreadHead].CreatedBy = "someone@example.com"
		},
		"the head is not a thread": func(f *planApprovalFixture) {
			f.ms.labels[askThreadHead] = []string{"project:gardenio"}
		},
		"the plan is bound to another thread": setFields(approvalPlan, `{"plan_state":"building","thread_id":"gd-head2"}`),
		"the plan names no thread":            setFields(approvalPlan, `{"plan_state":"building"}`),
		"the caller is bound to another thread": setFields(approvalThread,
			`{"role":"thread","project":"gardenio","thread_id":"gd-head2"}`),
		"the caller is not a thread agent": setFields(approvalThread,
			`{"role":"crew","project":"gardenio","thread_id":"`+askThreadHead+`"}`),
		"the plan is older than the window": func(f *planApprovalFixture) {
			f.ms.beads[approvalPlan].CreatedAt = f.now.Add(-planApprovalSealWindow - time.Minute)
		},
		"the home garden changed hands": func(f *planApprovalFixture) {
			homeProjectBead(f.ms, "gardenio", homeFields, "oidc:someone-else@example.com")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newThreadAskFixture(t)
			mutate(f)
			b, err := f.srv.createBead(threadCtx(), createBeadInput{
				Title: "plan gardener", Type: "trigger", Kind: "data",
				Labels: planGardenerLabels(approvalPlan), Fields: json.RawMessage(planGardenerTriggerFields),
			})
			if err != nil {
				t.Fatalf("createBead: %v", err)
			}
			if got := sealedSpawnAuthor(f.ms.beads[b.ID]); got != "" {
				t.Fatalf("sealed to %q, want no seal", got)
			}
		})
	}
}

// In a tenant land the thread agent is a tenant (gd-PtbbUxKWds), so it may not
// write a trigger bead at all, sealed or not.
func TestThreadAskSeal_TenantLandRefusesTheBind(t *testing.T) {
	f := newThreadAskFixture(t)
	homeProjectBead(f.ms, "gardenio", `{"land_kind":"tenant","land_default_channel":"#general"}`, homeOwner)
	_, err := f.srv.createBead(threadCtx(), createBeadInput{
		Title: "plan gardener", Type: "trigger", Kind: "data",
		Labels: planGardenerLabels(approvalPlan), Fields: json.RawMessage(planGardenerTriggerFields),
	})
	requireTenantDenial(t, err)
}

// A caller with no agent bead (a static token) is not the thread agent, so a
// bind is not sealed even when it names the home owner as its operator.
func TestThreadAskSeal_StaticCallerNotSealed(t *testing.T) {
	f := newThreadAskFixture(t)
	f.bind(WithOperatedByHeader(staticCtx(), homeOwner), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != "" {
		t.Fatalf("a static-token bind was sealed to %q, want no seal", got)
	}
}
