package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// postAs sends body to handler with ctx attached.
func postAs(t *testing.T, ctx context.Context, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func adminOIDCCtx() context.Context {
	return WithRoles(oidcCtx("admin@example.com", nil), []string{RoleAdmin})
}

var ownerAndAttacker = []OperatorAliasRef{
	{Kind: "oidc", Subject: "owner@example.com"},
	{Kind: "oidc", Subject: "attacker@example.com"},
}

// seedTwoOperators makes the owner and the attacker two separate operators.
func seedTwoOperators(t *testing.T, ms *mockStore) (ownerID, attackerID int64) {
	t.Helper()
	ctx := context.Background()
	ownerID, _ = ms.UpsertOperator(ctx, "oidc", "owner@example.com")
	_ = ms.UpsertOperatorAlias(ctx, ownerID, "oidc", "owner@example.com", "resolver")
	attackerID, _ = ms.UpsertOperator(ctx, "oidc", "attacker@example.com")
	_ = ms.UpsertOperatorAlias(ctx, attackerID, "oidc", "attacker@example.com", "resolver")
	return ownerID, attackerID
}

// A static or fleet token may not merge operators: without no_merge the
// request is a 403 and nothing moves.
func TestLinkAliases_MachineTokenWithoutNoMerge_403(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			ownerID, attackerID := seedTwoOperators(t, ms)
			rec := postAs(t, ctx, srv.handleAdminLinkOperatorAliases, "/v1/admin/operators/link-aliases",
				AdminLinkOperatorAliasesRequest{Aliases: ownerAndAttacker})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			if ms.operatorAliases["oidc\x00owner@example.com"] != ownerID || ms.operatorAliases["oidc\x00attacker@example.com"] != attackerID {
				t.Errorf("aliases moved despite 403")
			}
			if len(ms.operatorMerges) != 0 {
				t.Errorf("MergeOperators called: %v", ms.operatorMerges)
			}
		})
	}
}

const linkPath = "/v1/admin/operators/link-aliases"

// resync posts {resync_bead_id} as ctx.
func resync(t *testing.T, ctx context.Context, srv *BeadsServer, beadID string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, ctx, srv.handleAdminLinkOperatorAliases, linkPath, AdminLinkOperatorAliasesRequest{ResyncBeadID: beadID})
}

// BYPASS REPRODUCTION (review of PR #246): a static or fleet token used the
// no_merge mode to attach slack:UATTACKER to the owner's operator. A machine
// caller may not choose aliases at all, with or without no_merge.
func TestLinkAliases_MachineTokenExplicitAliases_403(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, noMerge := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: "/no_merge"}[noMerge], func(t *testing.T) {
				ms := newMockStore()
				srv := NewBeadsServer(ms, &events.NoopPublisher{})
				ownerID, _ := seedTwoOperators(t, ms)
				rec := postAs(t, ctx, srv.handleAdminLinkOperatorAliases, linkPath, AdminLinkOperatorAliasesRequest{
					NoMerge: noMerge,
					Aliases: []OperatorAliasRef{{Kind: "oidc", Subject: "owner@example.com"}, {Kind: "slack", Subject: "UATTACKER"}},
				})
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				if id, ok := ms.operatorAliases["slack\x00UATTACKER"]; ok {
					t.Errorf("slack:UATTACKER now resolves to operator %d (owner is %d)", id, ownerID)
				}
				if len(ms.operatorMerges) != 0 {
					t.Errorf("MergeOperators called: %v", ms.operatorMerges)
				}
			})
		}
	}
}

// The machine path reads the aliases from the bead the caller names.
func TestLinkAliases_MachineResync_LinksAliasesReadFromBead(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedIdentityUserBead(t, ms, "gd-alice",
		`{"email":"Alice@Example.com","email_aliases":["alice2@example.com"],"slack_id":"U1","github_username":"AliceGH"}`)
	for i := 0; i < 2; i++ { // replay is a no-op
		if rec := resync(t, machineUserCtx(), srv, "gd-alice"); rec.Code != http.StatusOK {
			t.Fatalf("call %d: code = %d; body %s", i, rec.Code, rec.Body.String())
		}
	}
	op := ms.operatorAliases["oidc\x00alice@example.com"]
	if op == 0 {
		t.Fatalf("oidc alias not created: %v", ms.operatorAliases)
	}
	for _, k := range []string{"slack\x00U1", "github\x00alicegh"} {
		if ms.operatorAliases[k] != op {
			t.Errorf("%q -> %d, want operator %d", k, ms.operatorAliases[k], op)
		}
	}
	// email_aliases is a display field: it never becomes an operator alias
	// (gd-L55f9EYFjN).
	if id, ok := ms.operatorAliases["oidc\x00alice2@example.com"]; ok {
		t.Errorf("email_aliases entry became an oidc alias of operator %d", id)
	}
}

// The caller cannot smuggle aliases in alongside the bead id, nor point the
// resync at a bead that is not a user bead, nor at nothing.
func TestLinkAliases_MachineResync_RejectsSmuggledAndInvalid(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedIdentityUserBead(t, ms, "gd-alice", `{"email":"alice@example.com","slack_id":"U1"}`)
	ms.beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"email":"alice@example.com","slack_id":"UATTACKER"}`)}

	rec := postAs(t, machineUserCtx(), srv.handleAdminLinkOperatorAliases, linkPath, AdminLinkOperatorAliasesRequest{
		ResyncBeadID: "gd-alice", Aliases: []OperatorAliasRef{{Kind: "slack", Subject: "UATTACKER"}, {Kind: "oidc", Subject: "alice@example.com"}}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("aliases+resync_bead_id: code %d, want 400", rec.Code)
	}
	if rec := resync(t, machineUserCtx(), srv, "gd-task"); rec.Code != http.StatusBadRequest {
		t.Errorf("non-user bead: code %d, want 400", rec.Code)
	}
	if rec := resync(t, machineUserCtx(), srv, "gd-missing"); rec.Code != http.StatusNotFound {
		t.Errorf("missing bead: code %d, want 404", rec.Code)
	}
	if _, ok := ms.operatorAliases["slack\x00UATTACKER"]; ok {
		t.Errorf("an alias the caller supplied was linked")
	}
}

// A bead with fewer than two identities has nothing to link: 200, no rows.
func TestLinkAliases_MachineResync_SingleIdentityNothingToLink(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedIdentityUserBead(t, ms, "gd-solo", `{"email":"solo@example.com","status_message":"hi"}`)
	rec := resync(t, machineUserCtx(), srv, "gd-solo")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	var resp AdminLinkOperatorAliasesResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.NothingToLink || len(ms.operatorAliases) != 0 {
		t.Errorf("resp %+v, aliases %v", resp, ms.operatorAliases)
	}
}

// A bead whose aliases already belong to different operators is refused with
// 409 on the machine path; nothing is repointed and MergeOperators never runs.
func TestLinkAliases_MachineResync_CollisionRefused409NoMerge(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	ownerID, attackerID := seedTwoOperators(t, ms)
	_ = ms.UpsertOperatorAlias(context.Background(), attackerID, "slack", "UATTACKER", "resolver")
	seedIdentityUserBead(t, ms, "gd-mixed", `{"email":"owner@example.com","slack_id":"UATTACKER"}`)
	rec := resync(t, machineUserCtx(), srv, "gd-mixed")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if ms.operatorAliases["oidc\x00owner@example.com"] != ownerID || ms.operatorAliases["slack\x00UATTACKER"] != attackerID {
		t.Errorf("aliases moved on a refused link")
	}
	if len(ms.operatorMerges) != 0 {
		t.Errorf("MergeOperators called: %v", ms.operatorMerges)
	}
}

// An address in email_aliases is a label: a bead listing another operator's
// email there links nothing, so there is no collision to refuse and the other
// operator's alias is left alone (gd-L55f9EYFjN).
func TestLinkAliases_MachineResync_EmailAliasesAreNotIdentity(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	ownerID, attackerID := seedTwoOperators(t, ms)
	seedIdentityUserBead(t, ms, "gd-mixed", `{"email":"owner@example.com","email_aliases":["attacker@example.com"]}`)
	rec := resync(t, machineUserCtx(), srv, "gd-mixed")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	if ms.operatorAliases["oidc\x00owner@example.com"] != ownerID || ms.operatorAliases["oidc\x00attacker@example.com"] != attackerID {
		t.Errorf("email_aliases moved an operator alias: %v", ms.operatorAliases)
	}
	if len(ms.operatorMerges) != 0 {
		t.Errorf("MergeOperators called: %v", ms.operatorMerges)
	}
}

// An OIDC gardenio-admin keeps the explicit alias list and the merge.
func TestLinkAliases_AdminMerges(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	ownerID, attackerID := seedTwoOperators(t, ms)
	rec := postAs(t, adminOIDCCtx(), srv.handleAdminLinkOperatorAliases, linkPath,
		AdminLinkOperatorAliasesRequest{Aliases: ownerAndAttacker})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	if ms.operatorMerges[attackerID] != ownerID {
		t.Errorf("admin merge did not run: %v", ms.operatorMerges)
	}
}

// An admin may also ask for the refuse-instead-of-merge behaviour.
func TestLinkAliases_AdminNoMergeCollision_409(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTwoOperators(t, ms)
	rec := postAs(t, adminOIDCCtx(), srv.handleAdminLinkOperatorAliases, linkPath,
		AdminLinkOperatorAliasesRequest{Aliases: ownerAndAttacker, NoMerge: true})
	if rec.Code != http.StatusConflict || len(ms.operatorMerges) != 0 {
		t.Fatalf("code = %d, merges %v", rec.Code, ms.operatorMerges)
	}
}

// Mixed-case oidc/github subjects on a bead are stored lowercase and resolve
// either way.
func TestLinkAliases_MixedCaseResolves(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedIdentityUserBead(t, ms, "gd-joy", `{"email":"Joy@Example.COM","github_username":"JoyousAversion"}`)
	if rec := resync(t, machineUserCtx(), srv, "gd-joy"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	if ms.operatorAliases["oidc\x00joy@example.com"] == 0 || ms.operatorAliases["github\x00joyousaversion"] == 0 {
		t.Fatalf("aliases not stored lowercase: %v", ms.operatorAliases)
	}
	if _, ok := ms.operatorAliases["oidc\x00Joy@Example.COM"]; ok {
		t.Errorf("mixed-case alias row was created")
	}
	for _, subject := range []string{"Joy@Example.COM", "joy@example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/operators/by-alias?kind=oidc&subject="+subject, nil)
		rr := httptest.NewRecorder()
		srv.handleLookupOperatorByAlias(rr, req)
		var resp LookupOperatorByAliasResponse
		_ = json.NewDecoder(rr.Body).Decode(&resp)
		if !resp.Found {
			t.Errorf("lookup %q: not found", subject)
		}
	}
}

func TestUserBeadAliasRefs(t *testing.T) {
	cases := []struct {
		name, fields string
		want         []OperatorAliasRef
	}{
		{"all kinds, deterministic order", `{"github_username":"GH","jira_user_id":"J1","gitlab_username":"gl","slack_id":"U1","email_aliases":["b@x.io","A@X.io"],"email":"A@x.io"}`,
			[]OperatorAliasRef{{"oidc", "a@x.io"}, {"slack", "U1"}, {"gitlab", "gl"}, {"jira", "J1"}, {"github", "gh"}}},
		{"gitlab login case-folded, slack and jira kept", `{"gitlab_username":" JoyousAversion ","slack_id":"UABC","jira_user_id":"JiraX"}`,
			[]OperatorAliasRef{{"slack", "UABC"}, {"gitlab", "joyousaversion"}, {"jira", "JiraX"}}},
		{"slack_user_id and jira_user fallbacks", `{"email":"a@x.io","slack_user_id":"U2","jira_user":"J2"}`,
			[]OperatorAliasRef{{"oidc", "a@x.io"}, {"slack", "U2"}, {"jira", "J2"}}},
		{"email_aliases ignored (array, JSON text, csv)", `{"email":"a@x.io","email_aliases":["b@x.io"]}`, []OperatorAliasRef{{"oidc", "a@x.io"}}},
		{"email_aliases JSON text ignored", `{"email":"a@x.io","email_aliases":"[\"b@x.io\"]"}`, []OperatorAliasRef{{"oidc", "a@x.io"}}},
		{"csv aliases ignored", `{"email_aliases":"b@x.io, c@x.io"}`, nil},
		{"non-email oidc dropped", `{"email":"not-an-email","email_aliases":["ok@x.io"]}`, nil},
		{"blank and duplicate dropped", `{"email":" ","slack_id":"U1","slack_user_id":"U1","email_aliases":[]}`, []OperatorAliasRef{{"slack", "U1"}}},
		{"not json", `nope`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := userBeadAliasRefs(json.RawMessage(c.fields))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

const prunePath = "/v1/admin/operators/prune-aliases"

// seedBridgeAliases links the aliases of the given beads as the bridge would.
func seedBridgeAliases(t *testing.T, srv *BeadsServer, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if rec := resync(t, machineUserCtx(), srv, id); rec.Code != http.StatusOK {
			t.Fatalf("resync %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

// BYPASS REPRODUCTION (review of PR #246): a machine prune with a dummy keep
// list deleted every bridge alias, including the owner's Slack identity. A
// machine caller may not supply keep, and the owner's alias survives.
func TestPruneAliases_MachineDummyKeepList_403AndNothingDeleted(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			seedIdentityUserBead(t, ms, "gd-owner", `{"email":"owner@example.com","slack_id":"U0OWNER"}`)
			seedBridgeAliases(t, srv, "gd-owner")
			rec := postAs(t, ctx, srv.handleAdminPruneOperatorAliases, prunePath,
				AdminPruneOperatorAliasesRequest{Source: "user-bead-bridge", Keep: []OperatorAliasRef{{Kind: "x", Subject: "y"}}})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			if ms.operatorAliases["slack\x00U0OWNER"] == 0 {
				t.Errorf("the owner's slack alias was deleted")
			}
		})
	}
}

// Revocation: the keep set is computed from the user beads. An alias removed
// from its bead stops resolving; aliases still on a bead, and aliases of other
// sources, survive.
func TestPruneAliases_ServerComputesKeep_RemovedAliasStopsResolving(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := context.Background()
	opID, _ := ms.UpsertOperator(ctx, "oidc", "alice@example.com")
	_ = ms.UpsertOperatorAlias(ctx, opID, "oidc", "alice@example.com", "resolver")
	seedIdentityUserBead(t, ms, "gd-alice", `{"email":"alice@example.com","slack_id":"U1","gitlab_username":"alice-gl"}`)
	seedBridgeAliases(t, srv, "gd-alice")
	if ms.operatorAliases["gitlab\x00alice-gl"] != opID {
		t.Fatalf("setup: gitlab alias not linked: %v", ms.operatorAliases)
	}

	// An admin removes the gitlab identity from the bead.
	ms.beads["gd-alice"].Fields = json.RawMessage(`{"email":"alice@example.com","slack_id":"U1"}`)

	for _, ctx := range []context.Context{machineUserCtx(), fleetUserCtx()} {
		rec := postAs(t, ctx, srv.handleAdminPruneOperatorAliases, prunePath, AdminPruneOperatorAliasesRequest{Source: "user-bead-bridge"})
		if rec.Code != http.StatusOK {
			t.Fatalf("prune: %d %s", rec.Code, rec.Body.String())
		}
	}
	if _, ok := ms.operatorAliases["gitlab\x00alice-gl"]; ok {
		t.Errorf("removed alias still resolves")
	}
	if ms.operatorAliases["slack\x00U1"] != opID {
		t.Errorf("an alias still on the bead was removed")
	}
	if ms.operatorAliases["oidc\x00alice@example.com"] != opID {
		t.Errorf("alias of another source (resolver) was revoked")
	}
}

// TestPruneAliases_Guards guards gd-uAvleAZUvV: only an OIDC admin chooses the
// keep set, and the user-bead-bridge source's keep set is always daemon-computed.
func TestPruneAliases_Guards(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	keep := []OperatorAliasRef{{Kind: "slack", Subject: "U1"}}
	prune := func(ctx context.Context, req AdminPruneOperatorAliasesRequest) int {
		return postAs(t, ctx, srv.handleAdminPruneOperatorAliases, prunePath, req).Code
	}
	// A machine token may only prune the user-bead-bridge source.
	if c := prune(machineUserCtx(), AdminPruneOperatorAliasesRequest{Source: "resolver"}); c != http.StatusForbidden {
		t.Errorf("machine prune of source=resolver: code %d, want 403", c)
	}
	// Even an admin cannot override the daemon-computed keep set for the bridge source.
	if c := prune(adminOIDCCtx(), AdminPruneOperatorAliasesRequest{Source: "user-bead-bridge", Keep: keep}); c != http.StatusBadRequest {
		t.Errorf("admin keep for bridge source: code %d, want 400", c)
	}
	// An admin pruning another source must say what to keep.
	if c := prune(adminOIDCCtx(), AdminPruneOperatorAliasesRequest{Source: "resolver"}); c != http.StatusBadRequest {
		t.Errorf("admin prune without keep: code %d, want 400", c)
	}
	if c := prune(adminOIDCCtx(), AdminPruneOperatorAliasesRequest{Source: "resolver", Keep: keep}); c != http.StatusOK {
		t.Errorf("admin prune: code %d, want 200", c)
	}
	// No user bead carries an alias: the computed keep set is empty, so
	// nothing is pruned (a bridge row must survive).
	ctx := context.Background()
	opID, _ := ms.UpsertOperator(ctx, "oidc", "z@example.com")
	_ = ms.UpsertOperatorAlias(ctx, opID, "slack", "UZ", "user-bead-bridge")
	if c := prune(machineUserCtx(), AdminPruneOperatorAliasesRequest{Source: "user-bead-bridge"}); c != http.StatusOK {
		t.Errorf("empty computed keep: code %d, want 200", c)
	}
	if ms.operatorAliases["slack\x00UZ"] != opID {
		t.Errorf("empty computed keep set wiped a bridge alias")
	}
}
