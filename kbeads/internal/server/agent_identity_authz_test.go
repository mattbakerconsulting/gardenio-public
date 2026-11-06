package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-Gpvi7wld87: a non-admin must not be able to name a pod ServiceAccount or
// turn its API token on from an agent bead. Every "denied" case sits next to a
// positive control (the admin, or an unrelated field, succeeding) so the denial
// cannot be a test that always passes.

const agentBeadFieldsJSON = `{"project":"gardenio","role":"crew","agent":"a1","agent_state":"working"}`

func seedAuthzAgentBead(t *testing.T, ms *mockStore, id, fields string) {
	t.Helper()
	ms.beads[id] = &model.Bead{
		ID: id, Title: "a1", Type: "agent", Kind: model.KindData,
		Status: model.StatusOpen, Fields: json.RawMessage(fields),
	}
}

func TestUpdateAgentBead_K8sIdentityFields_MachineDenied(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, patch := range []string{
			`{"service_account":"gardenio-infra-reader"}`,
			`{"needs_k8s_api":true}`,
			`{"needs_k8s_api":"true"}`,
		} {
			t.Run(name+"/"+patch, func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
				_, err := s.updateBead(ctx, "gd-agent1", updateBeadInput{Fields: json.RawMessage(patch)})
				requireDenied(t, err)
				if got := string(ms.beads["gd-agent1"].Fields); got != agentBeadFieldsJSON {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
		}
	}
}

func TestUpdateAgentBead_UnrelatedAndNarrowingFields_MachineAllowed(t *testing.T) {
	for _, patch := range []string{
		`{"agent_state":"idle"}`,
		`{"model":"opus"}`,
		`{"needs_k8s_api":false}`,
		`{"needs_k8s_api":"false"}`,
		`{"service_account":""}`,
	} {
		t.Run(patch, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
			if _, err := s.updateBead(machineUserCtx(), "gd-agent1", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
				t.Fatalf("write denied: %v", err)
			}
		})
	}
}

// An agent bead that already carries the field (it predates the gate) stays
// updatable when a read-modify-write client echoes the unchanged value back.
func TestUpdateAgentBead_EchoedK8sIdentity_MachineAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedAuthzAgentBead(t, ms, "gd-agent1", `{"project":"gardenio","role":"crew","agent":"a1","service_account":"gardenio-agent","needs_k8s_api":true}`)
	patch := json.RawMessage(`{"service_account":"gardenio-agent","needs_k8s_api":true,"agent_state":"idle"}`)
	if _, err := s.updateBead(machineUserCtx(), "gd-agent1", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("echo of unchanged value denied: %v", err)
	}
	// Control: changing the value is still refused.
	_, err := s.updateBead(machineUserCtx(), "gd-agent1", updateBeadInput{Fields: json.RawMessage(`{"service_account":"gardenio-infra-reader"}`)})
	requireDenied(t, err)
}

func TestCreateAgentBead_K8sIdentityFields_MachineDenied(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(machineUserCtx(), createBeadInput{
		Title: "a2", Type: "agent", Fields: json.RawMessage(`{"project":"gardenio","role":"crew","agent":"a2","service_account":"gardenio-infra-reader"}`),
	})
	requireDenied(t, err)
	// Control: the same create without the field succeeds.
	if _, err := s.createBead(machineUserCtx(), createBeadInput{
		Title: "a2", Type: "agent", Fields: json.RawMessage(`{"project":"gardenio","role":"crew","agent":"a2"}`),
	}); err != nil {
		t.Fatalf("agent create without the field denied: %v", err)
	}
}

func TestUpdateAgentBead_K8sIdentityFields_AdminAndReplayAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
	if _, err := s.updateBead(adminOIDCCtx(), "gd-agent1", updateBeadInput{Fields: json.RawMessage(`{"service_account":"gardenio-infra-reader"}`)}); err != nil {
		t.Fatalf("admin write denied: %v", err)
	}
	if d := s.authorizeAgentIdentityFields(withMachineWrite(machineUserCtx()), "update", "agent",
		json.RawMessage(`{"needs_k8s_api":true}`), ms.beads["gd-agent1"].Fields); d != nil {
		t.Fatalf("escalation replay denied: %v", d)
	}
}

// An OIDC operator who is not an admin is a human but not an authority over a
// pod's Kubernetes identity.
func TestUpdateAgentBead_K8sIdentityFields_OIDCNonAdminDenied(t *testing.T) {
	s, ms, _ := newTestServer()
	seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
	op := WithRoles(oidcCtx("op@example.com", nil), []string{RoleOperator})
	_, err := s.updateBead(op, "gd-agent1", updateBeadInput{Fields: json.RawMessage(`{"service_account":"x"}`)})
	requireDenied(t, err)
}

func TestAuthorizeAgentIdentityFields_OtherTypesUnaffected(t *testing.T) {
	s, _, _ := newTestServer()
	for _, typ := range []model.BeadType{"task", "config", "report"} {
		if d := s.authorizeAgentIdentityFields(machineUserCtx(), "update", typ, json.RawMessage(`{"service_account":"x","needs_k8s_api":true,"privileged":true}`), nil); d != nil {
			t.Fatalf("type %s gated: %v", typ, d)
		}
	}
	// Control: role and project are gated for the same caller and patch.
	for _, typ := range []model.BeadType{"role", "project"} {
		if d := s.authorizeAgentIdentityFields(machineUserCtx(), "update", typ, json.RawMessage(`{"service_account":"x"}`), nil); d == nil {
			t.Fatalf("type %s not gated", typ)
		}
	}
}

// agentFieldWriteCases drive the real HTTP stack with the shared static
// token every agent pod holds (primary-token) and an OIDC admin.
func agentFieldWriteCases() []fieldWriteCase {
	const id = "gd-agent1"
	field := func(f, op string) string { return "/v1/beads/" + id + "/fields/" + f + "/" + op }
	return []fieldWriteCase{
		{"PATCH service_account", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"service_account": "x"}}, true},
		{"PATCH needs_k8s_api=true", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"needs_k8s_api": true}}, true},
		{"append service_account", http.MethodPost, field("service_account", "append"), appendArrayFieldRequest{Element: "x"}, true},
		{"increment needs_k8s_api", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"needs_k8s_api": 1}, true},
		{"close smuggling service_account", http.MethodPost, "/v1/beads/" + id + "/close", map[string]any{"service_account": "x"}, true},
		{
			"POST create type=agent with service_account", http.MethodPost, "/v1/beads",
			map[string]any{"title": "evil", "type": "agent", "fields": map[string]any{"project": "gardenio", "role": "crew", "agent": "evil", "service_account": "x"}},
			true,
		},

		{"PATCH agent_state", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"agent_state": "idle"}}, false},
		{"PATCH needs_k8s_api=false", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"needs_k8s_api": false}}, false},
		{"increment restart_count", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"restart_count": 1}, false},
	}
}

func TestAgentFieldMutationEndpoints_K8sIdentityGate(t *testing.T) {
	for _, c := range agentFieldWriteCases() {
		t.Run("static/"+c.name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			srv.userProvisioner = nil
			h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
			seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
			before := string(ms.beads["gd-agent1"].Fields)
			nBefore := countBeadsOfType(ms, "agent")

			rec := fieldWriteRequest(t, h, c, "primary-token", "slack:U0TESTER1")
			if c.denied {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				if after := string(ms.beads["gd-agent1"].Fields); after != before {
					t.Errorf("fields changed despite 403:\n before %s\n after  %s", before, after)
				}
				if countBeadsOfType(ms, "agent") != nBefore {
					t.Errorf("an agent bead was created despite 403")
				}
				return
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("unrelated write: code = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
		})
	}
	// Positive control: the same denied PATCH succeeds as an OIDC gardenio-admin.
	t.Run("admin/PATCH service_account", func(t *testing.T) {
		ms := newMockStore()
		srv := NewBeadsServer(ms, &events.NoopPublisher{})
		srv.userProvisioner = nil
		h := srv.NewHTTPHandler(&bearerSubjectVerifier{})
		seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
		c := agentFieldWriteCases()[0]
		rec := fieldWriteRequest(t, h, c, "admin@example.com", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("admin: code = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
	})
}

func TestMergeBeadFields_AgentK8sIdentityKeyDeniedForMachine(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedAuthzAgentBead(t, ms, "gd-agent1", agentBeadFieldsJSON)
	err := srv.mergeBeadFields(machineUserCtx(), "gd-agent1", map[string]any{"service_account": "x"})
	requireDenied(t, err)
	if err := srv.mergeBeadFields(machineUserCtx(), "gd-agent1", map[string]any{"agent_state": "idle"}); err != nil {
		t.Errorf("unrelated merge: %v", err)
	}
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-agent1", map[string]any{"service_account": "x"}); err != nil {
		t.Errorf("admin merge: %v", err)
	}
}

// gd-Gpvi7wld87 review D1: the static token may legitimately write project and
// role beads (machineAllowedWrites), but not the fields the controller turns
// into a pod's ServiceAccount, API token or privileged-role gate.

const (
	projectBeadFieldsJSON = `{"name":"gardenio","git_url":"https://example.com/r.git"}`
)

func seedConfigBead(t *testing.T, ms *mockStore, id string, typ model.BeadType, fields string) {
	t.Helper()
	ms.beads[id] = &model.Bead{
		ID: id, Title: id, Type: typ, Kind: model.KindData,
		Status: model.StatusOpen, Fields: json.RawMessage(fields),
	}
}

// project/update is machine-allowed (gb pool enable, /schedule), so these
// denials are the field gate and not the type gate; the control test below
// shows the same caller writing unrelated project fields. (role/update is not
// machine-allowed at all; role create is covered by
// TestCreateProjectAndRoleBead_K8sIdentityFields.)
func TestUpdateProjectBead_K8sIdentityFields_MachineDenied(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, patch := range []string{
			`{"service_account":"gardenio-infra-reader"}`,
			`{"needs_k8s_api":true}`,
			`{"needs_k8s_api":"true"}`,
			`{"privileged":true}`,
		} {
			t.Run(name+"/"+patch, func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
				_, err := s.updateBead(ctx, "gd-cfg1", updateBeadInput{Fields: json.RawMessage(patch)})
				requireDenied(t, err)
				if got := string(ms.beads["gd-cfg1"].Fields); got != projectBeadFieldsJSON {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
		}
	}
}

// Positive control: project/update IS machine-allowed, so the denial above is
// the field gate and not the type gate. An unrelated, narrowing or echoed
// write by the same caller succeeds.
func TestUpdateProjectBead_UnrelatedNarrowingAndEchoedFields_MachineAllowed(t *testing.T) {
	for _, patch := range []string{
		`{"schedules_enabled":true}`,
		`{"git_url":"https://example.com/other.git"}`,
		`{"needs_k8s_api":false}`,
		`{"service_account":""}`,
	} {
		t.Run(patch, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
			if _, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
				t.Fatalf("write denied: %v", err)
			}
		})
	}
	// Echo of an admin-written value (the viewer's project form re-sends
	// needs_k8s_api on every save) is not a change.
	s, ms, _ := newTestServer()
	seedConfigBead(t, ms, "gd-cfg1", "project", `{"name":"gardenio","needs_k8s_api":true,"service_account":"gardenio-infra-reader"}`)
	echo := json.RawMessage(`{"needs_k8s_api":true,"service_account":"gardenio-infra-reader","model":"opus"}`)
	if _, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: echo}); err != nil {
		t.Fatalf("echo of unchanged value denied: %v", err)
	}
	// Control: changing the stored value is still refused.
	_, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(`{"service_account":"other"}`)})
	requireDenied(t, err)
}

func TestUpdateProjectBead_K8sIdentityFields_AdminAndReplayAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
	if _, err := s.updateBead(adminOIDCCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(`{"service_account":"gardenio-infra-reader","needs_k8s_api":true}`)}); err != nil {
		t.Fatalf("admin write denied: %v", err)
	}
	if d := s.authorizeAgentIdentityFields(withMachineWrite(machineUserCtx()), "update", "project",
		json.RawMessage(`{"needs_k8s_api":true}`), nil); d != nil {
		t.Fatalf("escalation replay denied: %v", d)
	}
}

func TestCreateProjectAndRoleBead_K8sIdentityFields(t *testing.T) {
	for _, typ := range []model.BeadType{"project", "role"} {
		t.Run(string(typ), func(t *testing.T) {
			s, ms, _ := newTestServer()
			registerUntrustedInput(t, ms)
			// The non-admin creator: the static token for a role (role/create
			// is machine-allowed); the Lands provisioner opening a tenant land
			// for a project, the only non-admin that creates one (gd-WCM9rfOL8L).
			creator, base := machineUserCtx(), `"model":"opus"`
			if typ == "project" {
				creator, base = landsProvisionerCtx(), `"model":"opus","land_kind":"tenant"`
			}
			evil := json.RawMessage(`{` + base + `,"service_account":"gardenio-infra-reader"}`)
			if typ == "role" {
				// A role naming a ServiceAccount must also be privileged to pass
				// shape validation, so the denial below is the authz gate.
				evil = json.RawMessage(`{"model":"opus","service_account":"gardenio-infra-reader","privileged":true}`)
			}
			_, err := s.createBead(creator, createBeadInput{Title: "x", Type: string(typ), Fields: evil})
			requireDenied(t, err)
			// Controls: the same create without the field succeeds for that
			// creator, and with the field succeeds for an admin.
			if _, err := s.createBead(creator, createBeadInput{Title: "x", Type: string(typ), Fields: json.RawMessage(`{` + base + `}`)}); err != nil {
				t.Fatalf("create without the field denied: %v", err)
			}
			if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "y", Type: string(typ), Fields: evil}); err != nil {
				t.Fatalf("admin create denied: %v", err)
			}
		})
	}
}

func TestMergeBeadFields_ConfigK8sIdentityKeyDeniedForMachine(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
	requireDenied(t, srv.mergeBeadFields(machineUserCtx(), "gd-cfg1", map[string]any{"needs_k8s_api": true}))
	if err := srv.mergeBeadFields(machineUserCtx(), "gd-cfg1", map[string]any{"model": "opus"}); err != nil {
		t.Errorf("unrelated merge: %v", err)
	}
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-cfg1", map[string]any{"needs_k8s_api": true}); err != nil {
		t.Errorf("admin merge: %v", err)
	}
}

// The real HTTP stack with the shared static token: PATCH, the field endpoints
// and POST /v1/beads on project and role beads.
func TestConfigBeadK8sIdentity_HTTPStaticTokenGate(t *testing.T) {
	const id = "gd-cfg1"
	cases := []fieldWriteCase{
		{"PATCH project service_account", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"service_account": "x"}}, true},
		{"PATCH project needs_k8s_api=true", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"needs_k8s_api": true}}, true},
		{"close smuggling needs_k8s_api", http.MethodPost, "/v1/beads/" + id + "/close", map[string]any{"needs_k8s_api": true}, true},
		{
			"POST create project with service_account", http.MethodPost, "/v1/beads",
			map[string]any{"title": "p", "type": "project", "fields": map[string]any{"git_url": "https://example.com/r.git", "service_account": "x"}},
			true,
		},
		{
			"POST create role with service_account", http.MethodPost, "/v1/beads",
			map[string]any{"title": "r", "type": "role", "fields": map[string]any{"service_account": "x", "privileged": true}},
			true,
		},

		// Controls: the same caller's unrelated project writes still succeed.
		{"PATCH project git_url", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"git_url": "https://example.com/o.git"}}, false},
		{"PATCH project needs_k8s_api=false", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"needs_k8s_api": false}}, false},
	}
	for _, c := range cases {
		t.Run("static/"+c.name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			srv.userProvisioner = nil
			h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
			seedConfigBead(t, ms, id, "project", projectBeadFieldsJSON)
			before := string(ms.beads[id].Fields)
			nProj, nRole := countBeadsOfType(ms, "project"), countBeadsOfType(ms, "role")

			rec := fieldWriteRequest(t, h, c, "primary-token", "slack:U0TESTER1")
			if !c.denied {
				if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
					t.Fatalf("unrelated write: code = %d, want 200; body %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			if after := string(ms.beads[id].Fields); after != before {
				t.Errorf("fields changed despite 403:\n before %s\n after  %s", before, after)
			}
			if countBeadsOfType(ms, "project") != nProj || countBeadsOfType(ms, "role") != nRole {
				t.Errorf("a project/role bead was created despite 403")
			}
		})
	}
	// Positive control: the same denied PATCH succeeds as an OIDC admin.
	t.Run("admin/PATCH project service_account", func(t *testing.T) {
		ms := newMockStore()
		srv := NewBeadsServer(ms, &events.NoopPublisher{})
		srv.userProvisioner = nil
		h := srv.NewHTTPHandler(&bearerSubjectVerifier{})
		seedConfigBead(t, ms, id, "project", projectBeadFieldsJSON)
		rec := fieldWriteRequest(t, h, cases[0], "admin@example.com", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("admin: code = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
	})
}

// ownership_grants widens what every agent holding a role may write, so only
// an admin sets it on a role bead; a machine token (every agent pod, and the
// shared static token, which may create role beads) is refused on create,
// update and the field-mutation paths. Narrowing (an empty list) is not gated.
func TestRoleBead_OwnershipGrants_MachineDenied(t *testing.T) {
	s, _, _ := newTestServer()
	grant := json.RawMessage(`{"image":"registry/agent:latest","ownership_grants":` + bypassRows + `}`)

	requireDenied(t, func() error {
		_, err := s.createBead(machineUserCtx(), createBeadInput{Title: "reviewer", Type: "role", Fields: grant})
		return err
	}())
	// The JSON-encoded string spelling is the same grant at the gate.
	if got := changedAgentIdentityFields(configK8sIdentityFields, json.RawMessage(`{"ownership_grants":"[{\"types\":[\"task\"],\"actions\":[\"update\"]}]"}`), nil); len(got) != 1 {
		t.Fatalf("a string-encoded list must count as a grant, got %v", got)
	}

	// Controls: an empty list from a machine, and a grant from an admin, both succeed.
	if _, err := s.createBead(machineUserCtx(), createBeadInput{Title: "reviewer", Type: "role", Fields: json.RawMessage(`{"ownership_grants":[]}`)}); err != nil {
		t.Fatalf("narrowing create denied: %v", err)
	}
	if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "reviewer", Type: "role", Fields: grant}); err != nil {
		t.Fatalf("admin create denied: %v", err)
	}

	// Update and single-field mutation go through the same decision.
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-role1", "role", `{"image":"registry/agent:latest"}`)
	if d := srv.authorizeAgentIdentityFields(machineUserCtx(), "update", "role", json.RawMessage(bypassRoleFields), json.RawMessage(`{"image":"registry/agent:latest"}`)); d == nil {
		t.Fatal("machine update setting ownership_grants must be denied")
	}
	if d := srv.authorizeAgentBeadFieldWrite(machineUserCtx(), "update", ms.beads["gd-role1"], roleOwnershipGrantsField); d == nil {
		t.Fatal("machine single-field write of ownership_grants must be denied")
	}
	if d := srv.authorizeAgentIdentityFields(adminOIDCCtx(), "update", "role", json.RawMessage(bypassRoleFields), json.RawMessage(`{}`)); d != nil {
		t.Fatalf("admin update denied: %v", d)
	}
	// Echoing an unchanged stored value (read-modify-write) is not a change.
	if d := srv.authorizeAgentIdentityFields(machineUserCtx(), "update", "role", json.RawMessage(bypassRoleFields), json.RawMessage(bypassRoleFields)); d != nil {
		t.Fatalf("unchanged echo denied: %v", d)
	}
}

// Neither retired field is gated or read any more: a machine write of either
// is refused by the type:role schema as an unknown field, not by this gate
// (gd-OsLTduO6u9).
func TestRoleBead_RetiredGrantFieldsUngated(t *testing.T) {
	for _, k := range []string{"bypass_ownership", "labeled_ownership"} {
		if configK8sIdentityFields[k] || podIdentityKeys[k] {
			t.Errorf("%s must not be a gated key any more", k)
		}
	}
	s, _, _ := newTestServer()
	if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "reviewer", Type: "role", Fields: json.RawMessage(`{"bypass_ownership":true}`)}); err == nil {
		t.Fatal("the type:role schema must refuse the retired bypass_ownership field")
	}
}

// gd-ekYHj07AtJ: the pod-spec keys (docker, secrets, image, runtime class, ziti)
// are authority grants on project and role beads, same as service_account. The
// shared static / fleet token can still write unrelated fields (positive control
// in TestUpdateProjectBead_PodSpecFields_NarrowingEchoAndAdmin).
var podSpecGrantPatches = []string{
	`{"docker":true}`,
	`{"secrets":[{"env":"X","secret":"gardenio-claude-oauth-gardenio2","key":"token"}]}`,
	`{"secrets":"[{\"env\":\"X\",\"secret\":\"gardenio-x\",\"key\":\"k\"}]"}`,
	`{"image":"evil.example/agent:latest"}`,
	`{"runtime_class":"runc"}`,
	`{"channel_runtime_class":{"C123":"runsc"}}`,
	`{"ziti_access":true}`,
	`{"ziti_service_id":"abc"}`,
	`{"ziti_bind":true}`,
}

func TestUpdateProjectBead_PodSpecFields_MachineDenied(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, patch := range podSpecGrantPatches {
			t.Run(name+"/"+patch, func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
				_, err := s.updateBead(ctx, "gd-cfg1", updateBeadInput{Fields: json.RawMessage(patch)})
				requireDenied(t, err)
				if got := string(ms.beads["gd-cfg1"].Fields); got != projectBeadFieldsJSON {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
		}
	}
}

func TestUpdateProjectBead_PodSpecFields_NarrowingEchoAndAdmin(t *testing.T) {
	for _, patch := range []string{
		`{"docker":false}`,
		`{"secrets":[]}`,
		`{"secrets":"[]"}`,
		`{"image":""}`,
		`{"ziti_access":false}`,
		`{"model":"opus"}`,
	} {
		t.Run(patch, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
			if _, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
				t.Fatalf("write denied: %v", err)
			}
		})
	}
	// Echo of an admin-written value (the viewer's project form re-sends docker,
	// image and secrets on every save) is not a change; changing it still is.
	stored := `{"name":"gardenio","docker":true,"image":"reg/agent:1","secrets":[{"env":"X","secret":"gardenio-x","key":"k"}]}`
	s, ms, _ := newTestServer()
	seedConfigBead(t, ms, "gd-cfg1", "project", stored)
	echo := json.RawMessage(`{"docker":true,"image":"reg/agent:1","secrets":[{"env":"X","secret":"gardenio-x","key":"k"}],"model":"opus"}`)
	if _, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: echo}); err != nil {
		t.Fatalf("echo of unchanged value denied: %v", err)
	}
	_, err := s.updateBead(machineUserCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(`{"image":"reg/agent:2"}`)})
	requireDenied(t, err)
	// Admin and approved-escalation replay are allowed.
	for _, patch := range podSpecGrantPatches {
		s, ms, _ := newTestServer()
		seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
		if _, err := s.updateBead(adminOIDCCtx(), "gd-cfg1", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
			t.Fatalf("admin write %s denied: %v", patch, err)
		}
		if d := s.authorizeAgentIdentityFields(withMachineWrite(machineUserCtx()), "update", "project", json.RawMessage(patch), nil); d != nil {
			t.Fatalf("escalation replay %s denied: %v", patch, d)
		}
	}
}

func TestCreateRoleAndProjectBead_PodSpecFields_MachineDenied(t *testing.T) {
	for _, typ := range []model.BeadType{"project", "role"} {
		for _, patch := range podSpecGrantPatches {
			// Role shape validation rejects these before authz (docker / ziti_bind
			// need privileged, which is itself gated; channel_runtime_class is
			// project-only), so they cannot isolate the field gate on a role.
			if typ == "role" && (strings.Contains(patch, `"docker"`) || strings.Contains(patch, "ziti_bind") || strings.Contains(patch, "channel_runtime_class")) {
				continue
			}
			t.Run(string(typ)+"/"+patch, func(t *testing.T) {
				s, _, _ := newTestServer()
				_, err := s.createBead(machineUserCtx(), createBeadInput{Title: "x", Type: string(typ), Fields: json.RawMessage(patch)})
				requireDenied(t, err)
			})
		}
	}
}

func TestMergeBeadFields_PodSpecKeysDeniedForMachine(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-cfg1", "project", projectBeadFieldsJSON)
	for _, k := range []string{"docker", "secrets", "image", "runtime_class", "ziti_access"} {
		requireDenied(t, srv.mergeBeadFields(machineUserCtx(), "gd-cfg1", map[string]any{k: "x"}))
	}
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-cfg1", map[string]any{"docker": true}); err != nil {
		t.Errorf("admin merge: %v", err)
	}
}
