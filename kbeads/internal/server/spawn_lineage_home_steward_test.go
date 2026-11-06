package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The home garden's steward binds and re-enables its plans' gardeners SEALED to
// the home garden's owner (owner, 11-02), so the engine's fires spawn without
// an escalation. A tenant steward, another agent, another project's plan and a
// plan that is not building get no seal.

const (
	homeStewardAgent = "gd-homestew"
	homeOwner        = "oidc:owner@example.com"
)

func homeStewardCtx() context.Context {
	return withAuthMethod(agentFleetCtx(homeStewardAgent, "gardenio-steward-abcd"), authMethodStatic)
}

// newHomeStewardFixture is the plan-approval fixture with no approval at all,
// the home garden's project bead and its steward.
func newHomeStewardFixture(t *testing.T) *planApprovalFixture {
	f := newPlanApprovalFixture(t)
	f.ms.deps[approvalPlan] = nil
	makeTierRoleBead(f.ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	f.ms.beads[homeStewardAgent] = &model.Bead{ID: homeStewardAgent, Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}
	homeProjectBead(f.ms, "gardenio", homeFields, homeOwner)
	return f
}

func TestHomeStewardSeal_BindIsSealedToTheOwner(t *testing.T) {
	f := newHomeStewardFixture(t)
	f.bind(homeStewardCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != homeOwner {
		t.Fatalf("seal author = %q, want the home owner %q", got, homeOwner)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("the engine's fire must spawn without an escalation, got %v", err)
	}
	// One lane per plan: a second gardener for the same plan is not sealed.
	f.bind(homeStewardCtx(), "gd-trig2", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if sealedSpawnAuthor(f.ms.beads["gd-trig2"]) != "" {
		t.Fatal("a second gardener lane for the plan was sealed")
	}
}

func TestHomeStewardSeal_StewardFiresItsSealedGardener(t *testing.T) {
	f := newHomeStewardFixture(t)
	f.bind(homeStewardCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	fire := spawnFieldsWithTrigger("gardenio", planGardenerSpawnRole, "gd-trig")
	if err := f.spawn(homeStewardCtx(), fire); err != nil {
		t.Fatalf("the home steward's fire-now of its sealed gardener must spawn, got %v", err)
	}
	// Another agent citing the same trigger still escalates.
	f.ms.beads["gd-crew9"] = &model.Bead{ID: "gd-crew9", Type: "agent", Fields: agentFields("gardenio", "crew")}
	makeRoleBead(f.ms, "gd-role-crew", "crew", false)
	requireEscalates(t, f.spawn(withAuthMethod(agentFleetCtx("gd-crew9", "crew-9"), authMethodStatic), fire))
	// Once the home garden changes hands, a gardener sealed to the old owner
	// is not the steward's to fire.
	homeProjectBead(f.ms, "gardenio", homeFields, "oidc:someone-else@example.com")
	requireEscalates(t, f.spawn(homeStewardCtx(), fire))
}

func TestHomeStewardSeal_ReEnableAndModeFlipReseal(t *testing.T) {
	f := newHomeStewardFixture(t)
	paused := `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","enabled":false}`
	f.bind(homeStewardCtx(), "gd-trig", paused, planGardenerLabels(approvalPlan))
	if sealedSpawnAuthor(f.ms.beads["gd-trig"]) != "" {
		t.Fatal("a paused gardener is not sealed at bind")
	}
	if _, err := f.srv.updateBead(homeStewardCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != homeOwner {
		t.Fatalf("re-enabled gardener seal author = %q, want %q", got, homeOwner)
	}
	// A content change (a mode flip rewrites the cron) re-seals it.
	if _, err := f.srv.updateBead(homeStewardCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"cron":"5 7 * * *"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("fire after the steward's rewrite: %v", err)
	}
}

func TestHomeStewardSeal_OnlyTheHomeStewardsBuildingPlans(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    func(*planApprovalFixture) context.Context
		mutate func(*planApprovalFixture)
	}{
		"another agent": {ctx: func(*planApprovalFixture) context.Context {
			return withAuthMethod(agentFleetCtx("gd-crew9", "crew-9"), authMethodStatic)
		}},
		"a tenant steward": {mutate: func(f *planApprovalFixture) {
			homeProjectBead(f.ms, "gardenio", `{"land_kind":"tenant","land_default_channel":"#general"}`, homeOwner)
		}},
		"a plan not building": {mutate: func(f *planApprovalFixture) {
			f.ms.beads[approvalPlan].Fields = json.RawMessage(`{"plan_state":"ready"}`)
		}},
		"a plan in another project": {mutate: func(f *planApprovalFixture) {
			f.ms.labels[approvalPlan] = []string{"plan", "project:other"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHomeStewardFixture(t)
			f.ms.beads["gd-crew9"] = &model.Bead{ID: "gd-crew9", Type: "agent", Fields: agentFields("gardenio", "crew")}
			makeRoleBead(f.ms, "gd-role-crew", "crew", false)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			ctx := homeStewardCtx()
			if tc.ctx != nil {
				ctx = tc.ctx(f)
			}
			// Refused outright (a tenant writes no trigger; a plan not building
			// takes no enabled gardener) or created unsealed: never sealed.
			b, err := f.srv.createBead(ctx, createBeadInput{
				Title: "plan gardener", Type: "trigger", Kind: "data",
				Labels: planGardenerLabels(approvalPlan), Fields: json.RawMessage(planGardenerTriggerFields),
			})
			if err == nil {
				if got := sealedSpawnAuthor(f.ms.beads[b.ID]); got != "" {
					t.Fatalf("sealed to %q, want no seal", got)
				}
			}
		})
	}
}
