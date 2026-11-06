package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// A plan's gardener may idle its own trigger until a named bead closes, and the
// close turns it back on (gd-V68T7v5JPP).

func selfIdleFixture(t *testing.T) (*BeadsServer, *mockStore, context.Context) {
	t.Helper()
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	ms.beads["gd-trig"].Fields = json.RawMessage(`{"action":"spawn_agent","role":"plan-gardener","enabled":true}`)
	gardener := planAgent(ms, "gd-a-gardener", "gardenio-plan-gardener-for-gd-plan-x1", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	return s, ms, gardener
}

func requireSelfIdleDenied(t *testing.T, err error, what string) {
	t.Helper()
	var d *AuthzDenial
	if !errors.As(err, &d) {
		t.Fatalf("%s: err = %v, want a 403", what, err)
	}
}

func TestGardenerSelfIdle_OwnTriggerIdlesAndWakesOnClose(t *testing.T) {
	s, ms, gardener := selfIdleFixture(t)

	patch := json.RawMessage(`{"enabled":false,"awaiting":"gd-step"}`)
	if _, err := s.updateBead(gardener, "gd-trig", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("the bound gardener must idle its own trigger: %v", err)
	}
	trig := ms.beads["gd-trig"]
	if effectiveGardenerEnabled(trig.Type, trig.Fields) || fieldsAsMap(trig.Fields)[gardenerAwaitingField] != "gd-step" {
		t.Fatalf("trigger after idle = %s, want disabled awaiting gd-step", trig.Fields)
	}

	if _, err := s.CloseBead(gardener, &beadsv1.CloseBeadRequest{Id: "gd-step"}); err != nil {
		t.Fatalf("close the awaited step: %v", err)
	}
	trig = ms.beads["gd-trig"]
	if !effectiveGardenerEnabled(trig.Type, trig.Fields) {
		t.Fatalf("the awaited close must turn the trigger back on: %s", trig.Fields)
	}
	if _, still := fieldsAsMap(trig.Fields)[gardenerAwaitingField]; still {
		t.Fatalf("the wake must clear awaiting: %s", trig.Fields)
	}
}

// A close of some other bead leaves an idled trigger alone.
func TestGardenerSelfIdle_OtherCloseDoesNotWake(t *testing.T) {
	s, ms, gardener := selfIdleFixture(t)
	if _, err := s.updateBead(gardener, "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"enabled":false,"awaiting":"gd-step"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBead(gardener, &beadsv1.CloseBeadRequest{Id: "gd-bundle"}); err != nil {
		t.Fatal(err)
	}
	if trig := ms.beads["gd-trig"]; effectiveGardenerEnabled(trig.Type, trig.Fields) {
		t.Fatalf("closing a bead the trigger does not await woke it: %s", trig.Fields)
	}
}

func TestGardenerSelfIdle_Denied(t *testing.T) {
	s, ms, gardener := selfIdleFixture(t)
	ms.beads["gd-trig2"] = &model.Bead{
		ID: "gd-trig2", Kind: model.KindData, Type: "trigger", Title: "gardener", Status: model.StatusOpen,
		CreatedBy: "owner@example.com", Fields: json.RawMessage(`{"action":"spawn_agent","role":"plan-gardener","enabled":true}`),
	}
	ms.labels["gd-trig2"] = []string{"project:gardenio", "role:gardener", gardenerEpicLabelPrefix + "gd-plan2", "plan-gardener"}
	ms.beads["gd-done"] = &model.Bead{ID: "gd-done", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed}
	crew := planAgent(ms, "gd-a-crew", "crew-fake", "crew", map[string]string{"trigger_id": "gd-trig"})
	retired := planAgent(ms, "gd-a-old", "gardenio-plan-gardener-for-gd-plan-x0", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	ms.beads["gd-a-old"].Status = model.StatusClosed

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		trigger string
		patch   string
	}{
		{"another plan's trigger", gardener, "gd-trig2", `{"enabled":false,"awaiting":"gd-step"}`},
		{"no awaiting", gardener, "gd-trig", `{"enabled":false}`},
		{"empty awaiting", gardener, "gd-trig", `{"enabled":false,"awaiting":""}`},
		{"awaiting a closed bead", gardener, "gd-trig", `{"enabled":false,"awaiting":"gd-done"}`},
		{"awaiting a bead that does not exist", gardener, "gd-trig", `{"enabled":false,"awaiting":"gd-nope"}`},
		{"turning the trigger on", gardener, "gd-trig", `{"enabled":true,"awaiting":"gd-step"}`},
		{"a field outside the row", gardener, "gd-trig", `{"enabled":false,"awaiting":"gd-step","prompt":"x"}`},
		{"trigger link without the plan-gardener role", crew, "gd-trig", `{"enabled":false,"awaiting":"gd-step"}`},
		{"a closed iteration", retired, "gd-trig", `{"enabled":false,"awaiting":"gd-step"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.updateBead(tc.ctx, tc.trigger, updateBeadInput{Fields: json.RawMessage(tc.patch)})
			requireSelfIdleDenied(t, err, tc.name)
		})
	}
}

// A cron change voids the seal's digest; the self-idle write carries the seal
// over the new content with the same author and iat, so the trigger keeps
// firing and the seal ages as before.
func TestGardenerSelfIdle_CarriesSeal(t *testing.T) {
	s, ms, gardener := selfIdleFixture(t)
	trig := ms.beads["gd-trig"]
	trig.Fields = json.RawMessage(`{"action":"spawn_agent","role":"plan-gardener","enabled":true,"cron":"*/30 * * * *"}`)
	if !s.sealSpawnLineageAs(trig, lineageAdmin) {
		t.Fatal("seal fixture")
	}
	prior := validSpawnSeal(trig)

	if _, err := s.updateBead(gardener, "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"cron":"0 */6 * * *","awaiting":"gd-step"}`)}); err != nil {
		t.Fatalf("the bound gardener must slow its own cron: %v", err)
	}
	seal := validSpawnSeal(ms.beads["gd-trig"])
	if seal == nil || seal.Author != lineageAdmin || seal.IAT != prior.IAT {
		t.Fatalf("seal after self-idle = %+v, want author %s iat %d", seal, lineageAdmin, prior.IAT)
	}
	if fieldsAsMap(ms.beads["gd-trig"].Fields)["cron"] != "0 */6 * * *" {
		t.Fatalf("cron not stored: %s", ms.beads["gd-trig"].Fields)
	}
}
