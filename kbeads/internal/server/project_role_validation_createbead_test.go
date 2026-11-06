package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ctxAsStaticAdmin returns a context that satisfies createBead's pre-validator
// gates for a type:project / type:role write: an OIDC identity (so the
// fail-closed operator resolver does not refuse the call) carrying the
// gardenio-admin role (so the min_role=gardenio-admin write policy on
// type:project / type:role passes). This mirrors how an operator actually
// writes these config beads, and lets the test observe the shape-validator
// verdict rather than an authz/operator 400 masking it.
func ctxAsStaticAdmin() context.Context {
	ctx := WithAuthIdentity(withAuthMethod(context.Background(), authMethodOIDC), AuthIdentity{
		Type: "oidc", Subject: defaultTestSubject,
	})
	return WithRoles(ctx, []string{RoleAdmin})
}

// TestCreateBead_ProjectMalformedSecretsRejected is the core regression: a
// type:project bead with object-shaped secrets[] (the silent-drop shape) must
// be rejected as an inputError (HTTP 400) at write time, not persisted.
func TestCreateBead_ProjectMalformedSecretsRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"git_url":"https://gitlab.com/o/r.git","secrets":{"env":"X"}}`),
	})
	if err == nil {
		t.Fatal("expected inputError for malformed project secrets, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "secrets") {
		t.Errorf("error should name the bad field: %v", err)
	}
	for id, b := range ms.beads {
		if b.Type == "project" {
			t.Fatalf("malformed project bead %s must NOT be persisted", id)
		}
	}
}

// TestCreateBead_ProjectGoodPersisted verifies a well-formed project bead is
// accepted and persisted unchanged (no regression to existing beads). A
// missing-git_url / mis-prefixed-secret bead would be a soft warning, but this
// one is fully clean.
func TestCreateBead_ProjectGoodPersisted(t *testing.T) {
	s, ms, _ := newTestServer()
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/org/repo.git",
		"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-git-token","key":"token"}],
		"repos":[{"url":"https://gitlab.com/org/x.git"}]
	}`)
	bead, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title: "gardenio", Type: "project", Fields: fields,
	})
	if err != nil {
		t.Fatalf("good project bead should be accepted: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("good project bead %s not persisted", bead.ID)
	}
}

// TestCreateBead_ProjectMissingGitURLAccepted proves the missing-git_url case
// is a SOFT warning: the bead is still accepted and persisted (not a 400).
func TestCreateBead_ProjectMissingGitURLAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"prefix":"kd"}`),
	})
	if err != nil {
		t.Fatalf("missing git_url must be a soft warning, not a 400: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("project bead %s not persisted despite soft warning", bead.ID)
	}
}

// TestCreateBead_RoleMalformedSecretsRejected mirrors the project case for
// type:role beads.
func TestCreateBead_RoleMalformedSecretsRejected(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "crew",
		Type:   "role",
		Fields: json.RawMessage(`{"secrets":"[bad json"}`),
	})
	if err == nil {
		t.Fatal("expected inputError for malformed role secrets, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
}

// TestCreateBead_RoleNeedsK8sAPIWithoutPrivilegedRejected proves the privilege
// gate is wired into the create path: a role that sets needs_k8s_api without
// privileged=true is rejected as an inputError (HTTP 400) and never persisted.
func TestCreateBead_RoleNeedsK8sAPIWithoutPrivilegedRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "k8s-live",
		Type:   "role",
		Fields: json.RawMessage(`{"needs_k8s_api":true}`),
	})
	if err == nil {
		t.Fatal("expected inputError for needs_k8s_api without privileged, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "needs_k8s_api") {
		t.Errorf("error should name the bad field: %v", err)
	}
	for id, b := range ms.beads {
		if b.Type == "role" {
			t.Fatalf("rejected privileged role bead %s must NOT be persisted", id)
		}
	}
}

// TestCreateBead_RolePrivilegedAccepted proves the escape hatch: setting
// privileged=true alongside needs_k8s_api persists cleanly.
func TestCreateBead_RolePrivilegedAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "k8s-live",
		Type:   "role",
		Fields: json.RawMessage(`{"needs_k8s_api":true,"privileged":true}`),
	})
	if err != nil {
		t.Fatalf("privileged role bead should be accepted: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("privileged role bead %s not persisted", bead.ID)
	}
}

// TestCreateBead_RoleGoodPersisted verifies a well-formed role bead persists.
func TestCreateBead_RoleGoodPersisted(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "crew",
		Type:   "role",
		Fields: json.RawMessage(`{"image":"registry/agent:latest","secrets":[{"env":"T","secret":"crew-t","key":"token"}]}`),
	})
	if err != nil {
		t.Fatalf("good role bead should be accepted: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("good role bead %s not persisted", bead.ID)
	}
}

// --- update path: grandfathering legacy malformed beads ------------------

// seedProjectBead directly inserts a project bead into the mock store,
// bypassing createBead's validation — this simulates a LEGACY bead that was
// accepted under the old (shape-unaware) rules and now carries a malformed
// structured field. Returns the bead id.
func seedProjectBead(ms *mockStore, id, title, fields string) {
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     title,
		Kind:      model.KindConfig,
		Type:      "project",
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(fields),
	}
}

// TestUpdateBead_ProjectLegacyMalformedNotWedged is the core regression for the
// update path: a legacy project bead with a pre-existing malformed secrets[]
// must NOT be wedged — a PATCH that touches only an UNRELATED field succeeds,
// because the byte-identical malformed value is grandfathered.
func TestUpdateBead_ProjectLegacyMalformedNotWedged(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-cnzn99cfo6"
	// Object-shaped secrets[] — malformed, but already persisted.
	seedProjectBead(ms, id, "gardenio", `{"git_url":"https://gitlab.com/o/r.git","secrets":{"env":"X"}}`)

	// Patch an unrelated field (model). The malformed secrets rides along in the
	// merged blob, but is byte-identical to the prior value → grandfathered.
	newModel := json.RawMessage(`{"model":"claude-opus-4-8"}`)
	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{Fields: newModel})
	if err != nil {
		t.Fatalf("unrelated-field PATCH on a legacy malformed bead must NOT be wedged, got: %v", err)
	}
}

// TestUpdateBead_ProjectNewMalformedRejected proves the create-time guarantee
// still holds on the update path: a PATCH that NEWLY introduces a malformed
// structured field is hard-rejected (HTTP 400), even on a legacy bead.
func TestUpdateBead_ProjectNewMalformedRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-cleanproj"
	// Clean prior state (no secrets).
	seedProjectBead(ms, id, "gardenio", `{"git_url":"https://gitlab.com/o/r.git"}`)

	// Patch in a NEWLY malformed secrets[] (object instead of array).
	patch := json.RawMessage(`{"secrets":{"env":"X"}}`)
	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{Fields: patch})
	if err == nil {
		t.Fatal("newly-introduced malformed secrets on update must be rejected")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
}

// TestUpdateBead_ProjectChangedToMalformedRejected proves that CHANGING an
// already-malformed field to a DIFFERENT malformed value is rejected (only the
// byte-identical prior value is grandfathered).
func TestUpdateBead_ProjectChangedToMalformedRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-1dl73lwp7g"
	seedProjectBead(ms, id, "gardenio", `{"secrets":{"env":"X"}}`)

	// Change secrets to a DIFFERENT malformed shape.
	patch := json.RawMessage(`{"secrets":{"env":"Y","extra":true}}`)
	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{Fields: patch})
	if err == nil {
		t.Fatal("changing a grandfathered malformed field to a new malformed value must be rejected")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
}

// TestUpdateBead_ProjectFixMalformedAccepted proves the operator's escape
// hatch: a legacy malformed bead CAN be fixed by patching the offending field
// to a well-formed value.
func TestUpdateBead_ProjectFixMalformedAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-fixproj"
	seedProjectBead(ms, id, "gardenio", `{"secrets":{"env":"X"}}`)

	// Fix secrets to a well-formed array.
	patch := json.RawMessage(`{"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-tok","key":"token"}]}`)
	if _, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("fixing a malformed field to a well-formed value must be accepted, got: %v", err)
	}
}

// TestUpdateBead_RoleLegacyMalformedNotWedged mirrors the project case for
// type:role beads: a legacy role bead with a pre-existing malformed secrets[]
// is not wedged against an unrelated-field PATCH; but a newly-introduced
// malformed env_json is still rejected.
func TestUpdateBead_RoleLegacyMalformedNotWedged(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-uys1itpvg3"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "crew",
		Kind:      model.KindConfig,
		Type:      "role",
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"secrets":{"env":"X"}}`),
	}

	// Unrelated-field PATCH succeeds (malformed secrets grandfathered).
	if _, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"image":"registry/agent:latest"}`),
	}); err != nil {
		t.Fatalf("unrelated-field PATCH on legacy malformed role bead must not be wedged, got: %v", err)
	}

	// Newly-introduced malformed env_json IS rejected.
	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"env_json":["not","a","map"]}`),
	})
	if err == nil {
		t.Fatal("newly-introduced malformed env_json on role update must be rejected")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
}

// TestUpdateBead_RolePrivilegeGateNotGrandfathered proves the privilege gate,
// unlike the shape checks above, is NOT grandfathered on update: a legacy role
// bead that already carries needs_k8s_api without privileged is rejected on
// its very next write, even one touching only an unrelated field.
func TestUpdateBead_RolePrivilegeGateNotGrandfathered(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-legacyk8slive"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "k8s-live",
		Kind:      model.KindConfig,
		Type:      "role",
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"needs_k8s_api":true}`),
	}

	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"image":"registry/agent:latest"}`),
	})
	if err == nil {
		t.Fatal("expected the privilege gate to reject an unrelated-field PATCH on an ungated legacy privileged role")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}

	// The escape hatch: adding privileged=true in the same patch succeeds.
	_, err = s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"privileged":true}`),
	})
	if err != nil {
		t.Fatalf("patching in privileged=true should fix the gate, got: %v", err)
	}
}
