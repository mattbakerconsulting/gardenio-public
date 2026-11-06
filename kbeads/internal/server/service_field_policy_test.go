package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// ka-0000006237 / epic ka-0000005107: a sandbox jira-bridge wired to the prod
// daemon used the serviceManagedFieldsByType ownership bypass (ka-0000003039)
// to stamp last_checked on prod trigger beads ~10s before every prod poll,
// permanently tripping prod's cooldown gate. These tests pin the writer-fleet
// policy that closes the class:
//   - BEADS_SERVICE_FIELD_FLEETS allowlist: only listed fleets get the bypass
//     on trigger/schedule beads; foreign fleets fall back to (and fail) the
//     plain ownership gate.
//   - BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT: unrestricted fleet tokens
//     are refused the bypass on project-labeled beads.
//   - accepted cross-fleet writes emit the Alert-A crossfleet metric.

// fleetCtxWithFleet builds a fleet-token context with both the audit identity
// and the resolved fleet name set — mirroring what AuthInterceptor injects for
// a registered bridge token (fleetCtx only sets the identity, leaving
// FleetFromContext at "default").
func fleetCtxWithFleet(fleet, subject string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject})
	return WithFleet(ctx, fleet)
}

func lastCheckedPatch(t *testing.T) []byte {
	t.Helper()
	fields, err := json.Marshal(map[string]any{"last_checked": "2026-07-01T18:00:00Z"})
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	return fields
}

// TestServiceFieldBypass_AllowlistedFleetAllowed pins the legitimate prod
// case: the prod jira-bridge fleet is on the allowlist and keeps stamping
// last_checked on trigger beads it does not own.
func TestServiceFieldBypass_AllowlistedFleetAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge", "slack-bridge"}, false)
	ms.beads["gd-sfp1"] = triggerBead("gd-sfp1", "thread-1777560916-494139")

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge")
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp1",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("allowlisted prod fleet last_checked stamp should succeed, got: %v", err)
	}
}

// TestServiceFieldBypass_ForeignFleetDenied pins the outage case: the sandbox
// bridge fleet is NOT on the allowlist, so its last_checked stamp on a prod
// trigger bead it does not own is denied.
func TestServiceFieldBypass_ForeignFleetDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, false)
	ms.beads["gd-sfp2"] = triggerBead("gd-sfp2", "thread-1777560916-494139")

	ctx := fleetCtxWithFleet("gardenio-jira-sandbox", "jira:gardenio-jira-sandbox")
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp2",
		Fields: lastCheckedPatch(t),
	})
	requireCode(t, err, codes.PermissionDenied)

	// The field must NOT have been written.
	var stored map[string]any
	if err := json.Unmarshal(ms.beads["gd-sfp2"].Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if _, ok := stored["last_checked"]; ok {
		t.Error("denied sandbox write must not persist last_checked")
	}
}

// TestServiceFieldBypass_EmptyAllowlistIsNoOp pins back-compat: a deployment
// that has not set BEADS_SERVICE_FIELD_FLEETS keeps the permissive
// ka-0000003039 behavior — any fleet may stamp.
func TestServiceFieldBypass_EmptyAllowlistIsNoOp(t *testing.T) {
	srv, ms, _ := newTestServer()
	// No SetServiceFieldPolicy call at all.
	ms.beads["gd-sfp3"] = triggerBead("gd-sfp3", "thread-abc")

	ctx := fleetCtxWithFleet("gardenio-jira-sandbox", "jira:gardenio-jira-sandbox")
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp3",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("empty allowlist must preserve permissive back-compat, got: %v", err)
	}
}

// TestServiceFieldBypass_PrivilegedFleetUnaffected pins that a PRIVILEGED
// service fleet (e.g. gitlab-bridge, ka-0000001946) bypasses the writer-fleet
// policy entirely — it already bypasses ownership everywhere.
func TestServiceFieldBypass_PrivilegedFleetUnaffected(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, false)
	ms.beads["gd-sfp4"] = triggerBead("gd-sfp4", "thread-abc")

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "gitlab-bridge", Privileged: true})
	ctx = WithFleet(ctx, "gitlab-bridge")

	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp4",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("privileged fleet must be unaffected by the allowlist, got: %v", err)
	}
}

// TestServiceFieldBypass_NonListedFleetOwnBeadStillAllowed pins the fallback:
// a fleet refused the bypass falls back to the plain ownership gate, so a
// non-listed fleet that actually OWNS the bead (created it) is still allowed.
func TestServiceFieldBypass_NonListedFleetOwnBeadStillAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, false)
	b := triggerBead("gd-sfp5", "jira:gardenio-jira-sandbox") // created by the sandbox subject
	ms.beads["gd-sfp5"] = b

	ctx := fleetCtxWithFleet("gardenio-jira-sandbox", "jira:gardenio-jira-sandbox")
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp5",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("non-listed fleet stamping its OWN trigger bead should pass the ownership fallback, got: %v", err)
	}
}

// TestServiceFieldBypass_AgentTypeUngated pins that the agent-bead bypass
// (context_tokens / live_cost_usd) is NOT subject to the writer-fleet policy —
// only the shared cross-env types (trigger/schedule) are gated.
func TestServiceFieldBypass_AgentTypeUngated(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, false)
	ms.beads["gd-sfp6"] = &model.Bead{
		ID:        "gd-sfp6",
		Title:     "agent bead",
		Type:      "agent",
		Kind:      model.KindConfig,
		Status:    model.StatusOpen,
		CreatedBy: "someone-else",
		Fields:    json.RawMessage(`{"agent":"crew-1","role":"crew","project":"gardenio"}`),
	}

	ctx := fleetCtxWithFleet("gardenio-jira-sandbox", "jira:gardenio-jira-sandbox")
	fields, _ := json.Marshal(map[string]any{"context_tokens": 12345})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp6",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("agent-type service-field bypass must stay ungated, got: %v", err)
	}
}

// TestServiceFieldBypass_RequireProject_UnrestrictedDenied pins the opt-in
// stronger gate: with BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT on, an
// UNRESTRICTED fleet token (no project scope) is refused the bypass on a
// project-labeled trigger bead — even if its fleet is on the allowlist.
func TestServiceFieldBypass_RequireProject_UnrestrictedDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, true)
	ms.beads["gd-sfp7"] = triggerBead("gd-sfp7", "thread-abc")
	// mockStore.GetBead reads labels from ms.labels, not the bead struct.
	ms.labels["gd-sfp7"] = []string{"project:monorepo", "trigger:jira"}

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge") // no WithTokenProject
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp7",
		Fields: lastCheckedPatch(t),
	})
	requireCode(t, err, codes.PermissionDenied)
}

// TestServiceFieldBypass_RequireProject_ScopedTokenAllowed pins the target
// end-state (Layer-3 sequencing): a project-scoped bridge token keeps the
// bypass on beads in its project when the require-project gate is on.
// (The project-scope MATCH itself is enforced upstream by
// beadMatchesTokenProject; this gate only refuses unrestricted tokens.)
func TestServiceFieldBypass_RequireProject_ScopedTokenAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, true)
	ms.beads["gd-sfp8"] = triggerBead("gd-sfp8", "thread-abc")
	ms.labels["gd-sfp8"] = []string{"project:monorepo", "trigger:jira"}

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge")
	ctx = WithTokenProject(ctx, "monorepo") // matches project:monorepo label
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp8",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("project-scoped token on a matching bead should keep the bypass, got: %v", err)
	}
}

// TestServiceFieldBypass_RequireProject_UnlabeledBeadAllowed pins that the
// require-project gate keys off true ABSENCE of a project label: an
// unrestricted token may still stamp an UNLABELED trigger bead.
func TestServiceFieldBypass_RequireProject_UnlabeledBeadAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy(nil, true)
	b := triggerBead("gd-sfp9", "thread-abc")
	b.Labels = []string{"trigger:jira"} // no project: label
	ms.beads["gd-sfp9"] = b

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge")
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp9",
		Fields: lastCheckedPatch(t),
	})
	if err != nil {
		t.Fatalf("unlabeled trigger bead must not trip the require-project gate, got: %v", err)
	}
}

// TestServiceFieldBypass_CrossFleetMetric pins Alert A: an ACCEPTED bypass
// write from a fleet other than the bead's owning fleet increments
// gardenio_service_field_crossfleet_write_total with bounded labels.
func TestServiceFieldBypass_CrossFleetMetric(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, false)
	b := triggerBead("gd-sfp10", "thread-abc")
	b.Fleet = "default"
	ms.beads["gd-sfp10"] = b

	before := testutil.ToFloat64(serviceFieldCrossFleetWrite.WithLabelValues("trigger", "default", "jira-bridge"))

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge")
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp10",
		Fields: lastCheckedPatch(t),
	}); err != nil {
		t.Fatalf("allowlisted cross-fleet stamp should succeed, got: %v", err)
	}

	after := testutil.ToFloat64(serviceFieldCrossFleetWrite.WithLabelValues("trigger", "default", "jira-bridge"))
	if after != before+1 {
		t.Errorf("crossfleet metric = %v, want %v", after, before+1)
	}
}

// TestServiceFieldBypass_SameFleetNoMetric pins that a same-fleet stamp does
// not emit the crossfleet metric.
func TestServiceFieldBypass_SameFleetNoMetric(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy(nil, false)
	b := triggerBead("gd-sfp11", "thread-abc")
	b.Fleet = "jira-bridge"
	ms.beads["gd-sfp11"] = b

	before := testutil.ToFloat64(serviceFieldCrossFleetWrite.WithLabelValues("trigger", "jira-bridge", "jira-bridge"))

	ctx := fleetCtxWithFleet("jira-bridge", "jira-bridge")
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp11",
		Fields: lastCheckedPatch(t),
	}); err != nil {
		t.Fatalf("same-fleet stamp should succeed, got: %v", err)
	}

	after := testutil.ToFloat64(serviceFieldCrossFleetWrite.WithLabelValues("trigger", "jira-bridge", "jira-bridge"))
	if after != before {
		t.Errorf("same-fleet write must not emit crossfleet metric: got %v, want %v", after, before)
	}
}

// TestServiceFieldBypass_NonFleetCallersUnaffected pins that OIDC and static
// primary-token callers are outside the writer-fleet policy (they never
// depended on the fleet bypass in the first place).
func TestServiceFieldBypass_NonFleetCallersUnaffected(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetServiceFieldPolicy([]string{"jira-bridge"}, true)
	ms.beads["gd-sfp12"] = triggerBead("gd-sfp12", "someone-else")

	// Primary static token (daemon service account) bypasses ownership.
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "static", Subject: "static"})
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-sfp12",
		Fields: lastCheckedPatch(t),
	}); err != nil {
		t.Fatalf("primary static token must be unaffected by the policy, got: %v", err)
	}
}

func TestHasProjectLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"no project label", []string{"trigger:jira", "incident"}, false},
		{"project label", []string{"project:monorepo"}, true},
		{"empty project value", []string{"project:"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasProjectLabel(tc.labels); got != tc.want {
				t.Errorf("hasProjectLabel(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}
