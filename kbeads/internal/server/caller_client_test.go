package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// gd-EmqfpYQ3NZ: a reject, a decision answer and a close record the door they
// came through (callerClient), so an agent on the owner's `kd login` is told
// apart from the owner in one query, not by reading comment signatures.

func TestCallerClient(t *testing.T) {
	cli := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "owner@example.com", ClientID: DeviceGrantClientID})
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"kd login": {cli, DeviceGrantClientID},
		"runner":   {runnerCallerCtx("gd-runner1", "oidc:owner@example.com"), "runner:gd-runner1"},
		// A runner id the runner auth path did not set names no runner.
		"runner id off the runner path": {WithRunnerID(oidcCallerCtx("owner@example.com"), "gd-runner1"), ""},
		"machine":                       {machineCtx("gardenio-controller"), "gardenio-controller"},
		"fleet":                         {agentFleetCtx("gd-crew", "crew-a"), ""},
	} {
		if got := callerClient(tc.ctx); got != tc.want {
			t.Errorf("%s: callerClient = %q, want %q", name, got, tc.want)
		}
	}
}

// closedClientRows returns the new values of id's closed_client history rows.
func closedClientRows(ms *mockStore, id string) []string {
	var out []string
	for _, m := range ms.mutations {
		if m.BeadID == id && m.Field == closedClientField {
			out = append(out, m.NewValue)
		}
	}
	return out
}

func asClient(client string) func(*http.Request) {
	return withRoles(withMethod(identityMutator(AuthIdentity{Type: "oidc", Subject: "owner@example.com", ClientID: client}, ""), authMethodOIDC), RoleAdmin)
}

func TestDecisionResolve_StampsResolverClient(t *testing.T) {
	for _, client := range []string{DeviceGrantClientID, testBrowserClient, ""} {
		ms, h := resolutionAuthzFixture(t, "gardenio")
		if rec := postJSON(h, "/v1/decisions/gd-dec-target/resolve", map[string]any{"selected_option": "y"}, asClient(client)); rec.Code != http.StatusOK {
			t.Fatalf("%q: resolve = %d; body: %s", client, rec.Code, rec.Body.String())
		}
		var f map[string]any
		if err := json.Unmarshal(ms.beads["gd-dec-target"].Fields, &f); err != nil {
			t.Fatal(err)
		}
		if f[decisionResolverClientField] != client {
			t.Errorf("%q: resolver_client = %v", client, f[decisionResolverClientField])
		}
		if rows := closedClientRows(ms, "gd-dec-target"); (client == "") != (len(rows) == 0) || (client != "" && rows[0] != client) {
			t.Errorf("%q: closed_client rows = %v", client, rows)
		}
	}
}

func TestClose_RecordsClosedClient(t *testing.T) {
	for name, close := range map[string]func(*mockStore, http.Handler) int{
		"POST close": func(_ *mockStore, h http.Handler) int {
			return doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{}, asClient(DeviceGrantClientID)).Code
		},
		"PATCH status": func(_ *mockStore, h http.Handler) int {
			return doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"status": "closed"}, asClient(DeviceGrantClientID)).Code
		},
	} {
		ms, h, _ := decisionFilerFixture(t)
		if code := close(ms, h); code != http.StatusOK {
			t.Fatalf("%s: got %d", name, code)
		}
		if rows := closedClientRows(ms, "gd-dec-own"); len(rows) != 1 || rows[0] != DeviceGrantClientID {
			t.Errorf("%s: closed_client rows = %v, want [%s]", name, rows, DeviceGrantClientID)
		}
	}
	// A machine's actor (machine:<client>) already names it: no row.
	ms, h, _ := decisionFilerFixture(t)
	machine := func(r *http.Request) { *r = *r.WithContext(machineCtx("gardenio-controller", RoleAdmin)) }
	if rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{}, machine); rec.Code != http.StatusOK {
		t.Fatalf("machine close: got %d %s", rec.Code, rec.Body.String())
	}
	if rows := closedClientRows(ms, "gd-dec-own"); len(rows) != 0 {
		t.Errorf("machine close: closed_client rows = %v, want none", rows)
	}
}

func TestReject_StampsRejectedClient(t *testing.T) {
	rec, b := pageDoor(t, testBrowserClient, "reject", operatorHeldFields())
	requireStatus(t, rec, http.StatusOK)
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["rejected_client"] != testBrowserClient {
		t.Errorf("page reject: rejected_client = %v, want %s", f["rejected_client"], testBrowserClient)
	}
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-esc-co", "gardenio", operatorHeldFields())
	requireStatus(t, callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleRejectEscalation, "gd-esc-co", "reject", map[string]any{"reason": "no"}), http.StatusOK)
	if got := escFields(t, ms, "gd-esc-co")["rejected_client"]; got != "gardenio-laptop-operator" {
		t.Errorf("machine reject: rejected_client = %v, want its client", got)
	}
	if rows := closedClientRows(ms, "gd-esc-co"); len(rows) != 0 {
		t.Errorf("machine reject: closed_client rows = %v, want none", rows)
	}
}
