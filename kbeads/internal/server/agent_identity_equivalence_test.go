package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// An agent on its own per-agent token (fleet "agent-<bead id>") must be treated
// like the static-token agent it replaces by every server-side step that was
// written for "a static-token agent": taint context and grants, the spawn-chain
// walk, auto-parent and thread_id inheritance, project scope, and the ownership
// bypass for coordinating roles (P7 review items 2, 4 and f).

func agentFleetCtx(beadID, name string) context.Context {
	return WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: name}), agentFleetPrefix+beadID)
}

func TestAgentBeadIDAndActingAgentRef(t *testing.T) {
	ctx := agentFleetCtx("gd-A1b2C3", "crew-a")
	if id, ok := agentBeadIDFromContext(ctx); !ok || id != "gd-A1b2C3" {
		t.Fatalf("agent fleet bead id = (%q,%v), want gd-A1b2C3", id, ok)
	}
	if ref, ok := actingAgentRef(ctx); !ok || ref != "gd-A1b2C3" {
		t.Fatalf("agent fleet ref = (%q,%v), want the bead id, not the name", ref, ok)
	}

	// The retired shared token's X-Actor self-attribution names no agent.
	static := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-a"})
	if _, ok := actingAgentRef(static); ok {
		t.Fatal("a Subject alone, with no agent-* fleet credential, is not an agent")
	}
	bridge := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "slack-bridge")
	if _, ok := actingAgentRef(bridge); ok {
		t.Fatal("a bridge fleet identity is not an agent")
	}
	if _, ok := agentBeadIDFromContext(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "x"}), "agent-gd-a1")); ok {
		t.Fatal("only a fleet identity can be an agent token")
	}
}

// Taint context: an agent token resolves the taints on the agent bead named by
// its fleet, not on a bead named after its subject.
func TestTaintContext_AgentFleetKeyedByBeadID(t *testing.T) {
	ms := newMockStore()
	ms.taints = map[string][]int64{"gd-a1": {7}, "crew-a": {99}}
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	set, err := s.taintContextOf(agentFleetCtx("gd-a1", "crew-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !set[7] || set[99] || len(set) != 1 {
		t.Fatalf("taint context = %v, want exactly {7} (keyed by bead id gd-a1)", set)
	}

	// Positive control: the same bead's taints on a non-agent fleet are not carried.
	bridge := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gd-a1"}), "slack-bridge")
	if set, _ := s.taintContextOf(bridge); len(set) != 0 {
		t.Fatalf("a service fleet carries no taints, got %v", set)
	}
}

// Taint grants: an agent token holds the agent grants of its bead id, not fleet
// grants under its subject.
func TestGrantsOf_AgentFleetUsesAgentGrants(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.taintGrants = map[string]*model.TaintGrant{}
	for _, g := range []*model.TaintGrant{
		{TaintID: 5, PrincipalKind: model.PrincipalAgent, Principal: "gd-a1"},
		{TaintID: 6, PrincipalKind: model.PrincipalFleet, Principal: "crew-a"},
	} {
		ms.taintGrants[taintGrantKey(g.TaintID, g.PrincipalKind, g.Principal)] = g
	}
	got, err := s.grantsOf(agentFleetCtx("gd-a1", "crew-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 5 {
		t.Fatalf("grants = %v, want [5] (agent grant of gd-a1 only)", got)
	}

	kind, principal, ok := s.resolveRequesterPrincipal(agentFleetCtx("gd-a1", "crew-a"))
	if !ok || kind != model.PrincipalAgent || principal != "gd-a1" {
		t.Fatalf("requester principal = (%q,%q,%v), want (agent, gd-a1)", kind, principal, ok)
	}

	// Positive control: a plain service fleet still holds fleet grants.
	svc := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "qa-fleet")
	got, err = s.grantsOf(svc)
	if err != nil || len(got) != 1 || got[0] != 6 {
		t.Fatalf("service fleet grants = %v err=%v, want [6]", got, err)
	}
}

// The spawn-chain walk (owner defaulting and agent grants) starts at the agent
// bead named by the credential.
func TestResolveSpawnChainRoot_AgentFleet(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{ID: "gd-a1", Type: "agent", Fields: []byte(`{"spawned_by":"gd-root"}`)},
		&model.Bead{ID: "gd-root", Type: "agent", Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`)},
	)
	root, ok := srv.resolveSpawnChainRoot(agentFleetCtx("gd-a1", "crew-a"))
	if !ok || root != "slack:U0OPERATOR1" {
		t.Fatalf("spawn chain root = (%q,%v), want slack:U0OPERATOR1", root, ok)
	}
}

// roleBeadWith returns a type:role bead named name carrying the given raw fields.
func roleBeadWith(id, name, fields string, status model.Status) *model.Bead {
	return &model.Bead{ID: id, Title: name, Type: "role", Kind: model.KindConfig, Status: status, Fields: json.RawMessage(fields)}
}

// Ownership: whether an agent on its own token mutates beads it did not create
// is decided by the ownership_grants rows on the role bead(s) its role names,
// not by a hard-coded role-name list. The default is no row.
func TestAuthorizeOwnership_AgentFleetBypassIsRoleBeadField(t *testing.T) {
	foreign := &model.Bead{ID: "gd-x", Type: "task", CreatedBy: "someone-else", Status: model.StatusOpen}
	open, closed := model.StatusOpen, model.StatusClosed
	for _, tc := range []struct {
		name      string
		agentRole string
		roles     []*model.Bead
		wantOK    bool
	}{
		// The merge desk: role reviewer with the field set.
		{"reviewer with field", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", bypassRoleFields, open)}, true},
		{"gardener with field", "gardener", []*model.Bead{roleBeadWith("gd-r1", "gardener", bypassRoleFields, open)}, true},
		{"captain with string-encoded rows", "gardenio-plan-captain", []*model.Bead{roleBeadWith("gd-r1", "gardenio-plan-captain", `{"ownership_grants":"[{\"types\":[\"*\"],\"except\":[\"decision\"],\"actions\":[\"update\"]}]"}`, open)}, true},
		{"any element of a comma list", "crew, reviewer", []*model.Bead{
			roleBeadWith("gd-r1", "crew", `{}`, open),
			roleBeadWith("gd-r2", "reviewer", bypassRoleFields, open),
		}, true},
		// Controls: the same agents without the field are gated, including the
		// roles the old hard-coded list named.
		{"reviewer without field", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", `{"image":"x"}`, open)}, false},
		{"gardener without field", "gardener", []*model.Bead{roleBeadWith("gd-r1", "gardener", `{}`, open)}, false},
		{"captain without field", "gardenio-plan-captain", []*model.Bead{roleBeadWith("gd-r1", "gardenio-plan-captain", `{}`, open)}, false},
		{"field empty", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", `{"ownership_grants":[]}`, open)}, false},
		{"field unparsable", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", `{"ownership_grants":"yes please"}`, open)}, false},
		{"retired bypass_ownership", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", `{"bypass_ownership":true}`, open)}, false},
		{"no role bead at all", "reviewer", nil, false},
		{"role bead is closed", "reviewer", []*model.Bead{roleBeadWith("gd-r1", "reviewer", bypassRoleFields, closed)}, false},
		// The field is on ANOTHER role, not on the agent's.
		{"other role has the field", "crew", []*model.Bead{
			roleBeadWith("gd-r1", "crew", `{}`, open),
			roleBeadWith("gd-r2", "reviewer", bypassRoleFields, open),
		}, false},
		// Elements, not substrings.
		{"substring of a role name", "reviewers", []*model.Bead{roleBeadWith("gd-r1", "reviewer", bypassRoleFields, open)}, false},
		{"empty role", "", []*model.Bead{roleBeadWith("gd-r1", "", bypassRoleFields, open)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"role": tc.agentRole})
			beads := append([]*model.Bead{{ID: "gd-a1", Type: "agent", Fields: raw}}, tc.roles...)
			srv := newChainWalkServer(beads...)
			denial := srv.authorizeOwnership(agentFleetCtx("gd-a1", "agent-a"), "update", foreign)
			if (denial == nil) != tc.wantOK {
				t.Fatalf("denial=%v, want bypass=%v", denial, tc.wantOK)
			}
		})
	}

	// The role does not carry over to a caller that is not an agent token: a
	// service fleet whose subject names the agent is still gated, whatever the
	// role bead says.
	raw, _ := json.Marshal(map[string]any{"role": "reviewer"})
	srv := newChainWalkServer(
		&model.Bead{ID: "gd-a1", Type: "agent", Fields: raw},
		roleBeadWith("gd-r1", "reviewer", bypassRoleFields, open),
	)
	svc := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "agent-a"}), "qa-fleet")
	if denial := srv.authorizeOwnership(svc, "update", foreign); denial == nil {
		t.Fatal("a non-agent fleet must stay ownership-gated")
	}
	// Positive control for the same server: the agent token itself passes.
	if denial := srv.authorizeOwnership(agentFleetCtx("gd-a1", "agent-a"), "update", foreign); denial != nil {
		t.Fatalf("positive control: the agent token with the field must pass, got %v", denial)
	}
}

// The merge desk over HTTP: role reviewer, on its own per-agent token, updates
// and closes a bead another crew created. Allowed when the reviewer role bead
// carries an all-types ownership_grants row; 403 without it. The escalation resolution verb
// (gb escalation close, POST /v1/escalations/{id}/result) and label/comment
// writes are NOT ownership-gated, so they work either way (asserted below as a
// control that the 403s come from the ownership gate and nothing else).
func TestAgentToken_ReviewerWritesOtherCrewsBeads(t *testing.T) {
	for _, tc := range []struct {
		name       string
		roleFields string
		wantStatus int
	}{
		{"with an all-types row", bypassRoleFields, http.StatusOK},
		{"without a row", `{}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newClassedTestServer(t)
			agentFields, _ := json.Marshal(map[string]any{"agent": "merge-desk-1", "role": "reviewer", "project": "gardenio"})
			ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Kind: model.KindConfig, Type: "agent", Title: "merge-desk-1", Status: model.StatusInProgress, Fields: agentFields}
			ms.beads["gd-rrev"] = roleBeadWith("gd-rrev", "reviewer", tc.roleFields, model.StatusOpen)
			for _, id := range []string{"gd-task1", "gd-task2"} {
				ms.beads[id] = &model.Bead{
					ID: id, Kind: model.KindIssue, Type: "task", Title: "crew task", Status: model.StatusOpen,
					CreatedBy: "gardenio-crew-other",
				}
				ms.labels[id] = []string{"project:gardenio"}
			}
			ms.beads["gd-esc1"] = &model.Bead{
				ID: "gd-esc1", Kind: model.KindData, Type: "escalation", Title: "Merge PR", Status: model.StatusOpen,
				CreatedBy: "gardenio-crew-other",
				// A verdict record (P7 Task 10): an agent that did not file it may resolve it.
				Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`),
			}
			ms.labels["gd-esc1"] = []string{"project:gardenio"}

			rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-task1", testAgentToken, map[string]any{"priority": 1}, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("update status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			rec = doBearerJSON(t, h, "POST", "/v1/beads/gd-task2/close", testAgentToken, map[string]any{}, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("close status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			// Controls: not ownership-gated, so allowed either way.
			rec = doBearerJSON(t, h, "POST", "/v1/beads/gd-esc1/labels", testAgentToken, map[string]any{"label": "merge-desk:seen"}, nil)
			requireStatus(t, rec, http.StatusCreated)
			rec = doBearerJSON(t, h, "POST", "/v1/escalations/gd-esc1/result", testAgentToken,
				map[string]any{"result_status": "success", "executor": "merge-desk-1"}, nil)
			requireStatus(t, rec, http.StatusOK)
		})
	}
}

// Auto-parent and thread_id inheritance fire for an agent token, keyed by the
// bead id in the fleet name.
func TestAgentToken_AutoParentAndThreadInheritance(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	raw, _ := json.Marshal(map[string]any{
		"agent": "crew-a", "task_id": "gd-task1", "thread_id": "gd-wish1", "slack_user_id": "U0OPERATOR2",
	})
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Kind: model.KindConfig, Type: "agent", Title: "crew-a", Status: model.StatusInProgress, Fields: raw}
	ms.beads["gd-task1"] = &model.Bead{ID: "gd-task1", Kind: model.KindIssue, Type: "task", Title: "task", Status: model.StatusOpen}
	ms.beads["gd-wish1"] = &model.Bead{ID: "gd-wish1", Kind: model.KindIssue, Type: "task", Title: "wish", Status: model.StatusOpen}

	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken,
		map[string]any{"title": "derived", "type": "task", "labels": []string{"project:gardenio"}},
		map[string]string{"X-Operated-By": "oidc:alice@example.com"})
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := created["id"].(string)

	var parented bool
	for _, d := range ms.deps[id] {
		if d.Type == model.DepParentChild && d.DependsOnID == "gd-task1" {
			parented = true
		}
	}
	if !parented {
		t.Fatalf("expected a parent-child dep to the agent's task_id, got %+v", ms.deps[id])
	}
	var fields map[string]any
	_ = json.Unmarshal(ms.beads[id].Fields, &fields)
	if fields["thread_id"] != "gd-wish1" {
		t.Fatalf("thread_id = %v, want it inherited from the agent bead (gd-wish1)", fields["thread_id"])
	}
}

// A project-scoped agent token creating a bead with no project label gets its
// own project stamped, so it can read the bead back; a different project is
// still refused.
func TestAgentToken_CreateStampsTokenProject(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	hdr := map[string]string{"X-Operated-By": "oidc:alice@example.com"}

	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{"title": "no label", "type": "task"}, hdr)
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := created["id"].(string)
	if !hasLabel(ms.beads[id].Labels, "project:gardenio") {
		t.Fatalf("labels = %v, want project:gardenio stamped", ms.beads[id].Labels)
	}
	requireStatus(t, doBearerJSON(t, h, "GET", "/v1/beads/"+id, testAgentToken, nil, nil), http.StatusOK)

	rec = doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken,
		map[string]any{"title": "other project", "type": "task", "labels": []string{"project:other"}}, hdr)
	if rec.Code == http.StatusCreated {
		t.Fatal("a bead naming another project must stay refused for a project-scoped token")
	}
}

// Mint-time subject validation (review item 1) and the SCIF portal identity.
func TestRegisterBridgeToken_AgentFleetSubjectValidation(t *testing.T) {
	srv, _, h := newClassedTestServer(t)
	// agent-* fleets are minted by the controller (token-mint machine principal),
	// never by the primary static token (gd-VuYwGGYLLT).
	mint := func(token, fleet, subject string) int {
		return mintReq(t, srv, machineCtx("gardenio-controller", "gardenio-machine-token-mint"), "machine-jwt",
			registerBridgeTokenRequest{Token: token, Fleet: fleet, Project: "gardenio", Subject: subject}).Code
	}
	if code := mint("t-ok", "agent-gd-ok1", "gardenio-crew-x1"); code != http.StatusCreated {
		t.Fatalf("plain agent subject: status %d, want 201", code)
	}
	for i, subj := range []string{"bridge:acme-scif", "bob@example.com", "a:b"} {
		if code := mint("t-bad"+string(rune('a'+i)), "agent-gd-bad1", subj); code != http.StatusBadRequest {
			t.Errorf("agent subject %q: status %d, want 400", subj, code)
		}
	}
	post := func(token, fleet, subject string) int {
		return doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testStaticToken, map[string]any{
			"token": token, "bridge": fleet, "project": "gardenio", "subject": subject,
		}, nil).Code
	}
	// The primary token (held by every agent pod) must not mint an agent-* fleet
	// at all: the bead id in the name is the identity (gd-VuYwGGYLLT).
	if code := post("t-agent", "agent-gd-forged1", "gardenio-crew-x1"); code != http.StatusForbidden {
		t.Fatalf("agent-* fleet from the primary token: status %d, want 403", code)
	}
	// ... nor a non-agent fleet with a 'bridge:<name>' subject either
	// (gd-ua4DGNtZdG); the token-mint capability is covered in
	// http_fleet_tokens_test.go.
	if code := post("t-bridge", "acme-scif-portal", "bridge:acme-scif"); code != http.StatusForbidden {
		t.Fatalf("service fleet bridge:<name> subject from the primary token: status %d, want 403", code)
	}
}

func TestCallerIsScifApprovalPortal_NeverAnAgentToken(t *testing.T) {
	portal := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:acme-scif"}), "acme-scif-portal")
	if !callerIsScifApprovalPortal(portal) {
		t.Fatal("positive control: the portal's own fleet identity must be recognised")
	}
	agent := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:acme-scif"}), "agent-gd-a1")
	if callerIsScifApprovalPortal(agent) {
		t.Fatal("an agent token must never be the SCIF approval portal, whatever its subject")
	}
}

// The gRPC interceptor honours the agent-pod headers for an agent token and
// ignores them for a service fleet; X-Actor never overrides the agent's identity.
func TestAuthInterceptor_AgentToken_HonoursAgentPodHeaders(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	fc.RegisterTokenFull("svc-tok", "qa-fleet", "", nil, nil, time.Time{}, "", false)

	run := func(tok string) (trigger string, noParent bool, subject string) {
		handler := func(ctx context.Context, _ any) (any, error) {
			trigger = triggerBeadFromContext(ctx)
			noParent = noAutoParentFromContext(ctx)
			subject = AuthIdentityFromContext(ctx).Subject
			return "ok", nil
		}
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"authorization", "Bearer "+tok,
			"x-trigger-bead", "gd-trigger1",
			"x-no-auto-parent", "1",
			"x-actor", "impostor",
		))
		if _, err := AuthInterceptor(withTestToken(fc, "secret"), nil)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, handler); err != nil {
			t.Fatal(err)
		}
		return
	}

	trigger, noParent, subject := run("agent-tok")
	if trigger != "gd-trigger1" || !noParent {
		t.Errorf("agent token: trigger=%q noParent=%v, want both honoured", trigger, noParent)
	}
	if subject != "crew-a" {
		t.Errorf("agent token subject = %q, want crew-a (X-Actor must not override it)", subject)
	}
	trigger, noParent, _ = run("svc-tok")
	if trigger != "" || noParent {
		t.Errorf("service fleet: trigger=%q noParent=%v, want neither honoured", trigger, noParent)
	}
}
