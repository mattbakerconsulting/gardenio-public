package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Role-string parsing and the agent-role update gate (gd-Yhbc5wdjwj). The
// controller resolves an agent's role beads by splitting `role` on commas and
// trimming; kbeads used to compare the whole string, so "crew,infra", " infra"
// and "infra " each resolved to the privileged infra role in the pod spec
// without ever tripping the privileged-role spawn gate.

func TestAuthorizeAgentSpawn_PrivilegedAnyElementOfRoleString(t *testing.T) {
	cases := []struct {
		role       string
		privileged bool
	}{
		// Positive controls: plain roles, and lists of them, are unaffected.
		{"crew", false},
		{"crew,reviewer", false},
		{"crew, reviewer", false},
		// The bypass shapes.
		{"infra", true},
		{"x,infra", true},
		{"crew,infra", true},
		{" infra", true},
		{"infra ", true},
		{" crew , infra ", true},
		{"crew,,infra", true},
		{"infra,crew", true},
		// Elements, not substrings.
		{"infrastructure", false},
		{"crew,infra-reader", false},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			ms := newMockStore()
			makeRoleBead(ms, "gd-role-infra", "infra", true)
			makeRoleBead(ms, "gd-role-crew", "crew", false)
			ctx := withAuthMethod(context.Background(), authMethodStatic)

			err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", tc.role))
			if !tc.privileged {
				if err != nil {
					t.Fatalf("role %q should not be gated, got: %v", tc.role, err)
				}
				return
			}
			var denial *AuthzDenial
			if !errors.As(err, &denial) {
				t.Fatalf("role %q should be denied as privileged, got %T: %v", tc.role, err, err)
			}
			if !denial.Escalatable || denial.SpawnRole != tc.role || denial.Action != "create" {
				t.Fatalf("denial = %+v, want escalatable create naming spawn_role %q", denial, tc.role)
			}
		})
	}
}

func TestAuthorizeAgentSpawn_OIDCAllowedRolesCheckedPerElement(t *testing.T) {
	ms := newMockStore()
	userFields, _ := json.Marshal(map[string]any{"email": "op@example.com", "allowed_roles": []string{"crew", "reviewer"}})
	ms.beads["gd-user-op"] = &model.Bead{
		ID: "gd-user-op", Title: "op", Type: "user", Kind: "config", Status: model.StatusOpen, Fields: userFields,
	}
	ctx := oidcCtx("op@example.com", []string{RoleOperator})

	if err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew, reviewer")); err != nil {
		t.Fatalf("every element allowed: want ok, got %v", err)
	}
	if err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew,infra")); err == nil {
		t.Fatal("one element outside allowed_roles must refuse the spawn")
	}
}

func TestIsNeverAutoApproveRole_AnyElement(t *testing.T) {
	cases := map[string]bool{
		"infra-admin":       true,
		" infra-admin":      true,
		"crew,infra-admin":  true,
		"infra-admin ,crew": true,
		"infra":             false,
		"crew,infra":        false,
		"crew":              false,
		"":                  false,
	}
	for role, want := range cases {
		if got := isNeverAutoApproveRole(role); got != want {
			t.Errorf("isNeverAutoApproveRole(%q) = %v, want %v", role, got, want)
		}
	}
}

func TestAgentRoleFieldChange(t *testing.T) {
	existing := json.RawMessage(`{"role":"crew","agent":"a"}`)
	cases := []struct {
		name    string
		patch   string
		wantNew string
		changed bool
	}{
		{"no role key", `{"agent_state":"working"}`, "", false},
		{"empty patch", ``, "", false},
		{"echo", `{"role":"crew"}`, "crew", false},
		{"respaced echo", `{"role":" crew "}`, " crew ", false},
		{"change", `{"role":"infra"}`, "infra", true},
		{"widen", `{"role":"crew,infra"}`, "crew,infra", true},
		{"clear", `{"role":""}`, "", true},
		{"null role is no change", `{"role":null}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := agentRoleFieldChange(json.RawMessage(tc.patch), existing)
			if changed != tc.changed || (changed && got != tc.wantNew) {
				t.Fatalf("agentRoleFieldChange(%s) = (%q, %v), want (%q, %v)", tc.patch, got, changed, tc.wantNew, tc.changed)
			}
		})
	}
	if _, changed := agentRoleFieldChange(json.RawMessage(`{"role":"crew"}`), nil); !changed {
		t.Error("setting a role on an agent bead that had none is a change")
	}
}

func TestEvaluateSpawnEscalation_MultiRoleLeftForHuman(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	ctx := context.Background()

	// A spawn_policy bead is keyed by ONE role, and the privileged escalation
	// for "crew,infra" would otherwise match nothing or the wrong policy. A
	// comma-joined role has no single policy that speaks for all its
	// elements, so it is left for a human even though infra has an auto rule.
	for i, tc := range []struct {
		role       string
		wantStatus model.Status
	}{
		{"infra", model.StatusClosed},     // control: single role, auto rule, approved
		{" infra ", model.StatusClosed},   // control: looked up by its parsed name
		{"crew,infra", model.StatusOpen},  // multi-role: human only
		{"infra,infra", model.StatusOpen}, // multi-role even when repeated
		{"infra-admin", model.StatusOpen}, // never-auto role
		{"crew,infra-admin", model.StatusOpen},
	} {
		id := fmt.Sprintf("gd-esc-multi-%d", i)
		t.Run(tc.role, func(t *testing.T) {
			s, ms := newSpawnPolicyServer(t, tc.role)
			seedSpawnPolicyRequesterAgent(ms, "gd-captain-1", "gardenio-captain-1", "captain", "gardenio", "gardenio")
			seedSpawnPolicyBead(ms, "gd-policy-infra", "infra", []map[string]any{
				spawnPolicyAutoRule(map[string]any{}, 6, 2),
			})
			esc := seedSpawnEscalation(ms, id, "gardenio-captain-1", tc.role, "gardenio-multi-crew-1", "gardenio")

			s.evaluateSpawnEscalation(ctx, esc)

			if got := ms.beads[id].Status; got != tc.wantStatus {
				t.Fatalf("escalation status for role %q = %s, want %s", tc.role, got, tc.wantStatus)
			}
		})
	}
}

// newAgentRoleGateServer starts the full HTTP stack with a privileged `infra`
// role bead, a plain `crew` role bead, an OIDC admin and an OIDC operator, and
// returns an agent bead created by the static token with role crew.
func newAgentRoleGateServer(t *testing.T) (call func(method, path, token string, body any, hdr map[string]string) (int, []byte), agentID string, ms *mockStore) {
	t.Helper()
	const adminToken = "static-role-gate"
	fc := NewFleetConfig("role-gate-fleet", nil)
	srvTS, _, store := newEscalationE2EServer(t, adminToken, fc, &escOIDCMock{tokens: map[string]escTokenInfo{
		"oidc-admin": {email: "admin@example.com", roles: []string{RoleAdmin}},
	}})
	makeRoleBead(store, "gd-role-infra", "infra", true)
	makeRoleBead(store, "gd-role-crew", "crew", false)

	call = func(method, path, token string, body any, hdr map[string]string) (int, []byte) {
		// A static token must name the operator it acts for (X-Operated-By).
		h := map[string]string{"X-Operated-By": "oidc:admin@example.com"}
		for k, v := range hdr {
			h[k] = v
		}
		resp := escRequest(t, srvTS, method, path, token, body, h)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, b := call("POST", "/v1/beads", adminToken, map[string]any{
		"title":  "role-gate-agent",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{"agent": "role-gate-agent", "mode": "crew", "role": "crew", "project": "gardenio"},
	}, nil)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("creating plain crew agent: %d %s", code, b)
	}
	var created model.Bead
	if err := json.Unmarshal(b, &created); err != nil || created.ID == "" {
		t.Fatalf("decoding created agent: %v %s", err, b)
	}
	return call, created.ID, store
}

func TestAgentRoleUpdate_GatedLikeASpawn(t *testing.T) {
	const staticTok = "static-role-gate"
	call, id, ms := newAgentRoleGateServer(t)
	roleOf := func() string {
		b, _ := ms.GetBead(context.Background(), id)
		return roleFromFields(b.Fields)
	}
	patch := func(tok string, fields map[string]any, hdr map[string]string) (int, []byte) {
		return call("PATCH", "/v1/beads/"+id, tok, map[string]any{"fields": fields}, hdr)
	}

	// Positive control: an unrelated agent field still updates.
	if code, b := patch(staticTok, map[string]any{"agent_state": "working"}, nil); code != http.StatusOK {
		t.Fatalf("unrelated field update: %d %s", code, b)
	}
	// Positive control: echoing the stored role, or re-spacing it, is no change.
	if code, b := patch(staticTok, map[string]any{"role": " crew "}, nil); code != http.StatusOK {
		t.Fatalf("echoed role update: %d %s", code, b)
	}
	// Positive control: moving to another non-privileged role is fine.
	if code, b := patch(staticTok, map[string]any{"role": "crew,reviewer"}, nil); code != http.StatusOK {
		t.Fatalf("non-privileged role change: %d %s", code, b)
	}

	// Non-admin static-token caller: every privileged shape is refused,
	// escalatable, and leaves the role untouched.
	for _, role := range []string{"infra", "crew,infra", " infra", "infra "} {
		code, b := patch(staticTok, map[string]any{"role": role}, nil)
		if code != http.StatusForbidden {
			t.Fatalf("role %q: want 403, got %d %s", role, code, b)
		}
		var denial AuthzDenial
		if err := json.Unmarshal(b, &denial); err != nil {
			t.Fatalf("role %q: decoding denial: %v", role, err)
		}
		if !denial.Escalatable || denial.SpawnRole != role || denial.Action != "update" || denial.BeadType != "agent" {
			t.Fatalf("role %q: denial = %+v", role, denial)
		}
		if got := roleOf(); got != "crew,reviewer" {
			t.Fatalf("role %q: refused PATCH changed the stored role to %q", role, got)
		}
	}

	// --escalate files an update escalation instead of changing the role.
	code, b := patch(staticTok, map[string]any{"role": "crew,infra"}, map[string]string{"X-Escalate": "true"})
	if code != http.StatusAccepted {
		t.Fatalf("escalate: want 202, got %d %s", code, b)
	}
	var esc EscalationResponse
	if err := json.Unmarshal(b, &esc); err != nil || esc.EscalationID == "" {
		t.Fatalf("decoding escalation: %v %s", err, b)
	}
	if got := roleOf(); got != "crew,reviewer" {
		t.Fatalf("escalated PATCH changed the stored role to %q", got)
	}

	// An operator approving it IS the authorization: the replay sets the role.
	code, b = call("POST", "/v1/escalations/"+esc.EscalationID+"/approve", "oidc-admin",
		map[string]any{"approval_method": "cli"}, nil)
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	var approved map[string]any
	_ = json.Unmarshal(b, &approved)
	replay, _ := approved["replay"].(map[string]any)
	if replay == nil || replay["status"] != "success" {
		t.Fatalf("approved role-change replay = %v", approved)
	}
	if got := roleOf(); got != "crew,infra" {
		t.Fatalf("after approved replay role = %q, want crew,infra", got)
	}

	// The same privileged PATCH as an OIDC admin succeeds directly.
	if code, b := patch("oidc-admin", map[string]any{"role": "infra"}, nil); code != http.StatusOK {
		t.Fatalf("admin role change: %d %s", code, b)
	}
	if got := roleOf(); got != "infra" {
		t.Fatalf("after admin PATCH role = %q, want infra", got)
	}
}

func TestAgentRoleUpdate_FieldEndpointsRefuseRoleKeyForNonOIDC(t *testing.T) {
	const staticTok = "static-role-gate"
	call, id, ms := newAgentRoleGateServer(t)

	// Positive control: the same endpoint on another key is untouched.
	if code, b := call("POST", "/v1/beads/"+id+"/increment", staticTok, map[string]any{"restarts": 1}, nil); code != http.StatusOK {
		t.Fatalf("increment unrelated key: %d %s", code, b)
	}
	// Closing with `role` in the extra-fields body is a role write too.
	code, _ := call("POST", "/v1/beads/"+id+"/close", staticTok, map[string]any{"role": "infra"}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("close body carrying role: want 403, got %d", code)
	}
	if b, _ := ms.GetBead(context.Background(), id); roleFromFields(b.Fields) != "crew" || b.Status == model.StatusClosed {
		t.Fatalf("refused close must change nothing, got status=%s role=%q", b.Status, roleFromFields(b.Fields))
	}
}

// A struct decode matches JSON keys case-insensitively; the controller reads
// fields["role"] by exact key. The gate must read the exact key, and refuse a
// type:agent write whose fields carry a case-variant of it (gd-Yhbc5wdjwj, D1).
func TestAgentRoleKeys_ExactKeyOnly(t *testing.T) {
	t.Run("exactFieldString ignores case variants", func(t *testing.T) {
		got, present, err := exactFieldString(json.RawMessage(`{"ROLE":"infra"}`), "role")
		if got != "" || present || err != nil {
			t.Fatalf("variant key read as role: (%q, %v, %v)", got, present, err)
		}
		got, present, err = exactFieldString(json.RawMessage(`{"Role":"infra","role":"crew"}`), "role")
		if got != "crew" || !present || err != nil {
			t.Fatalf("exact key beside a variant: (%q, %v, %v), want crew", got, present, err)
		}
		if _, present, err = exactFieldString(json.RawMessage(`{"role":7}`), "role"); !present || err == nil {
			t.Fatalf("non-string role must be reported, got present=%v err=%v", present, err)
		}
	})

	t.Run("create refuses variant and non-string role keys", func(t *testing.T) {
		ms := newMockStore()
		makeRoleBead(ms, "gd-role-infra", "infra", true)
		ctx := withAuthMethod(context.Background(), authMethodStatic)
		for _, fields := range []string{
			`{"role":"crew","Role":"infra"}`,
			`{"Role":"infra","role":"crew"}`,
			`{"ROLE":"infra"}`,
			`{"role":"crew","Project":"other"}`,
			`{"role":["infra"]}`,
		} {
			if err := authorizeAgentSpawn(ctx, ms, json.RawMessage(fields)); err == nil {
				t.Errorf("fields %s must be refused", fields)
			}
		}
		// Positive control: the exact keys alone are fine.
		if err := authorizeAgentSpawn(ctx, ms, json.RawMessage(`{"role":"crew","project":"gardenio"}`)); err != nil {
			t.Fatalf("exact keys: %v", err)
		}
	})

	t.Run("PATCH naming a privileged exact role is refused beside a variant key", func(t *testing.T) {
		const staticTok = "static-role-gate"
		call, id, ms := newAgentRoleGateServer(t)
		for _, fields := range []map[string]any{
			{"role": "infra", "Role": "crew"},
			{"Role": "crew", "role": "crew,infra"},
			{"ROLE": "infra"},
			{"role": 7},
		} {
			code, b := call("PATCH", "/v1/beads/"+id, staticTok, map[string]any{"fields": fields}, nil)
			if code < 400 || code >= 500 {
				t.Fatalf("PATCH %v: want 4xx, got %d %s", fields, code, b)
			}
			if bead, _ := ms.GetBead(context.Background(), id); roleFromFields(bead.Fields) != "crew" {
				t.Fatalf("refused PATCH %v changed the stored role to %q", fields, roleFromFields(bead.Fields))
			}
		}
		// Positive control: a plain non-privileged change still passes.
		if code, b := call("PATCH", "/v1/beads/"+id, staticTok, map[string]any{"fields": map[string]any{"role": "crew,reviewer"}}, nil); code != http.StatusOK {
			t.Fatalf("plain role change: %d %s", code, b)
		}
	})
}

// The stored role is canonical (the parsed elements, comma-joined), so every
// whitespace spelling of one role set is one string for exact-match lookups.
func TestAgentRole_StoredCanonical(t *testing.T) {
	const staticTok = "static-role-gate"
	call, id, ms := newAgentRoleGateServer(t)

	if code, b := call("PATCH", "/v1/beads/"+id, staticTok, map[string]any{"fields": map[string]any{"role": " crew , reviewer "}}, nil); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, b)
	}
	if bead, _ := ms.GetBead(context.Background(), id); roleFromFields(bead.Fields) != "crew,reviewer" {
		t.Fatalf("stored role after update = %q, want crew,reviewer", roleFromFields(bead.Fields))
	}

	code, b := call("POST", "/v1/beads", staticTok, map[string]any{
		"title": "canon-agent", "type": "agent", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"agent": "canon-agent", "mode": "crew", "role": " crew ", "project": "gardenio"},
	}, nil)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create: %d %s", code, b)
	}
	var created model.Bead
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	if got, _ := ms.GetBead(context.Background(), created.ID); roleFromFields(got.Fields) != "crew" {
		t.Fatalf("stored role after create = %q, want crew", roleFromFields(got.Fields))
	}
}

// spawnPolicyWithinCaps counts agents by the stored role string while the
// policy lookup keys on the parsed role: two spawns whose roles differ only by
// whitespace must share one (role, project) budget, whichever spelling came
// first (D2).
func TestEvaluateSpawnEscalation_CapsCountedByParsedRole(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	ctx := context.Background()

	for _, tc := range []struct{ first, second string }{
		{"infra", " infra"},  // canonical first, then a respaced spawn
		{" infra ", "infra"}, // respaced first: stored canonical, so the canonical one is counted
		{"infra ", " infra"},
	} {
		t.Run(fmt.Sprintf("%q then %q", tc.first, tc.second), func(t *testing.T) {
			s, ms := newSpawnPolicyServer(t, "infra")
			seedSpawnPolicyRequesterAgent(ms, "gd-captain-1", "gardenio-captain-1", "captain", "gardenio", "gardenio")
			seedSpawnPolicyBead(ms, "gd-policy-infra", "infra", []map[string]any{
				spawnPolicyAutoRule(map[string]any{}, 6, 1),
				spawnPolicyHumanRule(map[string]any{}),
			})

			// Control: the first spawn auto-approves and consumes max_concurrent=1.
			first := seedSpawnEscalation(ms, "gd-esc-caps-1", "gardenio-captain-1", tc.first, "gardenio-infra-crew-1", "gardenio")
			s.evaluateSpawnEscalation(ctx, first)
			if got := ms.beads["gd-esc-caps-1"]; got.Status != model.StatusClosed {
				t.Fatalf("control: first spawn status = %s, want closed (auto-approved)", got.Status)
			}

			// The second is over the cap and falls to the human rule.
			second := seedSpawnEscalation(ms, "gd-esc-caps-2", "gardenio-captain-1", tc.second, "gardenio-infra-crew-2", "gardenio")
			s.evaluateSpawnEscalation(ctx, second)
			if got := ms.beads["gd-esc-caps-2"]; got.Status != model.StatusOpen {
				t.Fatalf("second spawn status = %s, want open (over cap, left for a human)", got.Status)
			}
			var f map[string]any
			_ = json.Unmarshal(ms.beads["gd-esc-caps-2"].Fields, &f)
			if _, approved := f["approved_by"]; approved {
				t.Fatalf("over-cap spawn must not be auto-approved, fields=%v", f)
			}
		})
	}
}
