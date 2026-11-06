package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// P10.B (gd-4mEAhIE01U): presence is the page. A floor escalation is approved
// or rejected only from the page session (the forward-auth browser client),
// never from a `kd login` / `gl` token, though both carry gardenio-admin. The
// lane side (a non-floor item approved by its coordinator without the owner)
// is TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge.

// pageDoor drives verb ("approve" or "reject") on one escalation with fields
// through the real HTTP auth chain, as the owner (gardenio-admin) signed in
// through client azp.
func pageDoor(t *testing.T, azp, verb string, fields map[string]any) (*httptest.ResponseRecorder, *model.Bead) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetForwardAuthClients([]string{testBrowserClient})
	h := srv.NewHTTPHandler(&azpVerifier{clientID: azp}, withTestToken(nil, "primary-token"))
	raw, _ := json.Marshal(fields)
	ms.beads["gd-esc-pg"] = &model.Bead{
		ID: "gd-esc-pg", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: raw,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/escalations/gd-esc-pg/"+verb, strings.NewReader(`{"reason":"no"}`))
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, ms.beads["gd-esc-pg"]
}

func floorFields() map[string]any {
	return map[string]any{"operation_type": "test", "operation_spec": map[string]any{}, "sensitivity": "secret"}
}

func TestPagePresence_CLIDoesNotApproveOrRejectAFloorItem(t *testing.T) {
	for _, verb := range []string{"approve", "reject"} {
		t.Run(verb, func(t *testing.T) {
			rec, b := pageDoor(t, DeviceGrantClientID, verb, floorFields())
			requireStatus(t, rec, http.StatusForbidden)
			if !strings.Contains(rec.Body.String(), "on its page") {
				t.Errorf("the refusal must send the person to the page, got %s", rec.Body.String())
			}
			if b.Status != model.StatusOpen || strings.Contains(string(b.Fields), "_by") {
				t.Errorf("a refused %s must leave the escalation untouched: %s %s", verb, b.Status, b.Fields)
			}
		})
	}
}

func TestPagePresence_BrowserApprovesAndRejectsAFloorItem(t *testing.T) {
	rec, b := pageDoor(t, testBrowserClient, "approve", floorFields())
	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(string(b.Fields), `"approved_by":"owner@example.com"`) {
		t.Errorf("approved_by should be the page session's subject, fields=%s", b.Fields)
	}
	rec, b = pageDoor(t, testBrowserClient, "reject", floorFields())
	requireStatus(t, rec, http.StatusOK)
	if b.Status != model.StatusClosed {
		t.Errorf("a page reject closes the escalation, got %s", b.Status)
	}
}

// operatorHeldFields is a non-floor escalation held for the operators.
func operatorHeldFields() map[string]any {
	return map[string]any{"operation_type": "test", "operation_spec": map[string]any{}, "approvers": []string{"operator"},
		"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "operator_only": false}
}

// Off the floor too, a person rejects only on the page, as they approve
// (gd-yRGb2j1s56): a `kd login` reject is refused and leaves the bead alone.
func TestPagePresence_CLIDoesNotRejectOffTheFloor(t *testing.T) {
	for azp, want := range map[string]string{
		DeviceGrantClientID: "escalation rejection is not available to a `kd login` session; " + escalationOnThePage,
		"some-other-client": "escalation rejection requires the page session; " + escalationOnThePage,
	} {
		rec, b := pageDoor(t, azp, "reject", operatorHeldFields())
		requireStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s reject: want %q, got %s", azp, want, rec.Body.String())
		}
		if b.Status != model.StatusOpen || strings.Contains(string(b.Fields), "rejected_by") {
			t.Errorf("%s reject must leave the escalation untouched: %s %s", azp, b.Status, b.Fields)
		}
	}
}

func TestPagePresence_PageAndCoordinatorRejectOffTheFloor(t *testing.T) {
	rec, b := pageDoor(t, testBrowserClient, "reject", operatorHeldFields())
	requireStatus(t, rec, http.StatusOK)
	if b.Status != model.StatusClosed || !strings.Contains(string(b.Fields), `"rejected_by":"owner@example.com"`) {
		t.Errorf("a page reject closes the escalation as the person: %s %s", b.Status, b.Fields)
	}

	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-esc-co", "gardenio", operatorHeldFields())
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleRejectEscalation, "gd-esc-co", "reject", map[string]any{"reason": "no"})
	requireStatus(t, rec, http.StatusOK)
	if ms.beads["gd-esc-co"].Status != model.StatusClosed {
		t.Errorf("a coordinator reject closes the escalation, got %s", ms.beads["gd-esc-co"].Status)
	}
	if got := escField(t, ms, "gd-esc-co", "rejected_by"); got != "machine:gardenio-laptop-operator" {
		t.Errorf("coordinator rejected_by = %v, want the principal", got)
	}
}
