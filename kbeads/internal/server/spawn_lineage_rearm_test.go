package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// A plan gardener the engine closed is re-armed by reopening the SAME trigger
// (gd-miuZqFACwL): the home steward keeps its original seal, so the engine's
// next fire passes the lineage gate with no escalation. Every other reopen
// still voids it (the steward's own bind rule may then seal it to the home
// owner, which is not the original author here).

// newRearmFixture binds the plan's gardener as an OIDC admin (sealed to
// lineageAdmin, who is not the home owner) and closes it the way the engine
// does: disarmed, with its terminate marker.
func newRearmFixture(t *testing.T) *planApprovalFixture {
	f := newHomeStewardFixture(t)
	f.bind(adminOIDCCtx(), "gd-trig", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != lineageAdmin {
		t.Fatalf("fixture seal author = %q, want %q", got, lineageAdmin)
	}
	engineClose(f, "gd-trig")
	return f
}

func engineClose(f *planApprovalFixture, id string) {
	b := f.ms.beads[id]
	b.Status = model.StatusClosed
	withFields(map[string]any{"enabled": false, "gardener_terminated_kind": "max_runs"})(b)
}

// rearm reopens and re-enables trigger id as ctx, with extra patched fields.
func rearm(t *testing.T, f *planApprovalFixture, ctx context.Context, id string, extra map[string]any) {
	t.Helper()
	patch := map[string]any{"enabled": true}
	for k, v := range extra {
		patch[k] = v
	}
	raw, _ := json.Marshal(patch)
	open := string(model.StatusOpen)
	if _, err := f.srv.updateBead(ctx, id, updateBeadInput{Status: &open, Fields: raw}); err != nil {
		t.Fatalf("re-arm %s: %v", id, err)
	}
}

func TestRearmSeal_KeepsTheOriginalSeal(t *testing.T) {
	f := newRearmFixture(t)
	rearm(t, f, homeStewardCtx(), "gd-trig", nil)
	b := f.ms.beads["gd-trig"]
	if b.Status != model.StatusOpen || !effectiveGardenerEnabled(b.Type, b.Fields) || !carriesTerminateMarker(b) {
		t.Fatalf("re-armed trigger: status %s, fields %s; want open, enabled, marker kept for the engine", b.Status, b.Fields)
	}
	if got := sealedSpawnAuthor(b); got != lineageAdmin {
		t.Fatalf("re-armed seal author = %q, want the original %q", got, lineageAdmin)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("the re-armed gardener's fire must pass the lineage gate, got %v", err)
	}
}

func TestRearmSeal_OtherReopensDoNotKeepIt(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    func() context.Context
		extra  map[string]any
		mutate func(*planApprovalFixture)
	}{
		"another agent": {ctx: func() context.Context {
			return withAuthMethod(agentFleetCtx("gd-crew1", "crew-1"), authMethodStatic)
		}},
		"the re-arm also rewrites the prompt":    {extra: map[string]any{"prompt": "do something else"}},
		"the re-arm clears the terminate marker": {extra: map[string]any{"gardener_terminated_kind": ""}},
		"closed by hand, not by the engine": {mutate: func(f *planApprovalFixture) {
			withFields(map[string]any{"gardener_terminated_kind": ""})(f.ms.beads["gd-trig"])
		}},
		"a plan not building": {mutate: func(f *planApprovalFixture) {
			f.ms.beads[approvalPlan].Fields = json.RawMessage(`{"plan_state":"done"}`)
		}},
		"another sealed lane is open": {mutate: func(f *planApprovalFixture) {
			f.bind(adminOIDCCtx(), "gd-trig2", planGardenerTriggerFields, planGardenerLabels(approvalPlan))
		}},
		"an expired seal": {mutate: func(f *planApprovalFixture) {
			f.now = f.now.Add(defaultSpawnLineageSealTTL + time.Hour)
		}},
		"a tenant steward": {mutate: func(f *planApprovalFixture) {
			homeProjectBead(f.ms, "gardenio", `{"land_kind":"tenant","land_default_channel":"#general"}`, homeOwner)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRearmFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			raw, _ := json.Marshal(map[string]any{"enabled": true})
			if tc.extra != nil {
				m := map[string]any{"enabled": true}
				for k, v := range tc.extra {
					m[k] = v
				}
				raw, _ = json.Marshal(m)
			}
			ctx := homeStewardCtx
			if tc.ctx != nil {
				ctx = tc.ctx
			}
			open := string(model.StatusOpen)
			// Most of these are refused outright; one that lands keeps nothing.
			_, _ = f.srv.updateBead(ctx(), "gd-trig", updateBeadInput{Status: &open, Fields: raw})
			b := f.ms.beads["gd-trig"]
			if b.Status != model.StatusClosed && sealedSpawnAuthor(b) == lineageAdmin {
				t.Fatal("the reopen kept the original seal")
			}
		})
	}
}
