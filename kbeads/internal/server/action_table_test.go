package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"gardenio/kbeads/internal/model"
)

// reviewedMemberDecisions is the reviewed decision for a land member: the
// actions it may take on each type in its own land. Anything else is denied.
var reviewedMemberDecisions = map[model.BeadType][]string{
	model.TypeTask:       {"create", "update", "close", "comment"},
	model.TypeBug:        {"create", "update", "close", "comment"},
	model.TypeFeature:    {"create", "update", "close", "comment"},
	model.TypeDecision:   {"update", "close", "comment"},
	model.TypeEscalation: {"create", actionApprove},
	typeMessage:          {"create", scopedActionReact},
	typeAgent:            {actionSpawn},
	typeChannel:          {scopedActionJoin},
	typeNotification:     {"create"},
	typeDoc:              {scopedActionEditHolds},
}

func reviewedOwnerDecisions() map[model.BeadType][]string {
	d := maps.Clone(reviewedMemberDecisions)
	d[model.TypeProject] = []string{actionStewardDelegate}
	d[model.TypeRelease] = []string{actionReleaseDeploy}
	return d
}

// reviewedTierDecisions is the reviewed actionTable, one row per tier (Lands
// Commons spec §6.4). A tier added to actionTable with no row here, or a row
// here with no tier, fails the build.
var reviewedTierDecisions = map[tier]map[model.BeadType][]string{
	// A member plus steward_delegate on its land's project bead
	// (escalation-routing D8) and release_deploy on its land's releases
	// (releases plan gd-UZKZ9H25UD step 6).
	tierOwner:  reviewedOwnerDecisions(),
	tierMember: reviewedMemberDecisions,
	tierGuest: {
		typeMessage:      {"create", scopedActionReact},
		typeChannel:      {scopedActionJoin},
		typeNotification: {"create"},
	},
	// A member less spawn, approve and answering decisions (10.11
	// gd-x0kJF1wHk7): the acts that need a human. It files a decision for a
	// human to answer (the steward's plan decision, gd-mh9ANzCHIE).
	tierLand: {
		model.TypeTask:       {"create", "update", "close", "comment"},
		model.TypeBug:        {"create", "update", "close", "comment"},
		model.TypeFeature:    {"create", "update", "close", "comment"},
		model.TypeDecision:   {"create", "comment"},
		model.TypeEscalation: {"create"},
		typeMessage:          {"create", scopedActionReact},
	},
}

// reviewedActions is every action a scoped write check is made with. An action
// in actionTable that is not here fails the build, so a new verb is reviewed
// for every tier and type before it can be granted.
var reviewedActions = []string{"create", "update", "close", "delete", "comment", scopedActionReact, scopedActionJoin, scopedActionEditHolds, actionApprove, actionSpawn, actionStewardDelegate, actionReleaseDeploy, "hook-emit"}

// Every tier x every known type x every action: actionTable says exactly what
// the reviewed decisions say.
func TestActionTable_AllTypesAllTiers(t *testing.T) {
	for tr := range actionTable {
		if _, ok := reviewedTierDecisions[tr]; !ok {
			t.Errorf("tier %q is in actionTable with no reviewed decisions", tr)
		}
	}
	types := allWritableTypeCandidates()
	for tr, row := range reviewedTierDecisions {
		if _, ok := actionTable[tr]; !ok {
			t.Errorf("tier %q has reviewed decisions but no actionTable row", tr)
		}
		for typ, acts := range actionTable[tr] {
			if !slices.Contains(types, typ) {
				t.Errorf("tier %q: actionTable names type %q, which no registry or model constant knows", tr, typ)
			}
			for act := range acts {
				if !slices.Contains(reviewedActions, act) {
					t.Errorf("tier %q: actionTable grants unreviewed action %q on %s", tr, act, typ)
				}
			}
		}
		for _, typ := range types {
			for _, act := range reviewedActions {
				want := slices.Contains(row[typ], act)
				if got := tierAllows(tr, typ, act); got != want {
					t.Errorf("tierAllows(%s, %s, %s) = %v, want %v", tr, typ, act, got, want)
				}
			}
		}
	}
	// No row, no rights: a caller with no tier in a land is denied everything.
	for _, typ := range types {
		for _, act := range reviewedActions {
			if tierAllows("", typ, act) || tierAllows(tierOperator, typ, act) || tierAllows(tierRunner, typ, act) {
				t.Errorf("%s %s allowed for a tier with no row", act, typ)
			}
		}
	}
}

// The same table end to end: every tier's token x every type x {create,
// update, close, delete} x {own land, other project, no label}. Each tier is
// allowed exactly its reviewed cells in its own land; every other cell is a
// 403, except that an update or close of a bead outside its land answers 404,
// as for a missing bead (gd-7APTAIhdC5).
// refusedActionCode is the status a refused action answers in a cell: an
// update or close of a bead outside the caller's land is answered as missing.
// Delete is refused by route class before any bead is looked up.
func refusedActionCode(cell, action string) int {
	if cell != "own" && (action == "update" || action == "close") {
		return http.StatusNotFound
	}
	return http.StatusForbidden
}

func TestActionTable_AllTypesAllTiersHTTP(t *testing.T) {
	tokens := map[tier]string{tierOwner: tokScopedOwner, tierMember: tokScopedWriter, tierGuest: tokScopedGuest, tierLand: tokLandClaimlayer}
	for tr, tok := range tokens {
		t.Run(string(tr), func(t *testing.T) {
			f := newScopedFixture(t)
			for _, typ := range allWritableTypeCandidates() {
				allowedActs := reviewedTierDecisions[tr][typ]
				for _, cell := range scopedWriteCells {
					id := "w-" + string(typ) + "-" + cell.name
					f.seedTyped(id, typ, cell.label)
					var labels []string
					if cell.label != "" {
						labels = []string{cell.label}
					}

					type op struct {
						action string
						call   func() (int, string)
					}
					ops := []op{
						{"create", func() (int, string) {
							body := map[string]any{"title": "t-" + id, "type": string(typ), "labels": labels}
							switch typ {
							case model.TypeEscalation:
								body["fields"] = map[string]any{"operation_type": "x", "operation_spec": map[string]any{"a": "b"}}
							case model.TypeDecision:
								body["fields"] = landDoorDecision["fields"]
							}
							return f.do(t, tok, http.MethodPost, "/v1/beads", body)
						}},
						{"update", func() (int, string) {
							return f.do(t, tok, http.MethodPatch, "/v1/beads/"+id, map[string]any{"description": "d"})
						}},
						{"close", func() (int, string) {
							return f.do(t, tok, http.MethodPost, "/v1/beads/"+id+"/close", map[string]any{})
						}},
						{"delete", func() (int, string) {
							return f.do(t, tok, http.MethodDelete, "/v1/beads/"+id, nil)
						}},
					}
					for _, o := range ops {
						want := cell.name == "own" && slices.Contains(allowedActs, o.action)
						code, body := o.call()
						if want {
							if code == http.StatusForbidden {
								t.Errorf("%s %s in %s: %s denied (%d %s), want allowed", o.action, typ, cell.name, tr, code, body)
							}
							if (typ == model.TypeTask || typ == model.TypeBug || typ == model.TypeFeature || ((typ == model.TypeEscalation || typ == model.TypeDecision || typ == typeMessage) && o.action == "create")) && code >= 300 {
								t.Errorf("%s %s in %s: status %d %s, want 2xx", o.action, typ, cell.name, code, body)
							}
						} else if wantCode := refusedActionCode(cell.name, o.action); code != wantCode {
							t.Errorf("%s %s in %s: %s got %d %s, want %d", o.action, typ, cell.name, tr, code, body, wantCode)
						}
					}
				}
			}
		})
	}
}

// scopeFromContext resolves the read scope and the tiers from one token, so a
// land is in Projects exactly when it has a tier.
func TestScopeFromContext_Tiers(t *testing.T) {
	d := scopeFromContext(scopedCtx(t, []string{"/project-a/owners", "/project-a", "/project-b/guests", "/project-c", "/project-d/odd", "/team-x"}))
	want := map[string]tier{"a": tierOwner, "b": tierGuest, "c": tierMember, "d": tierGuest}
	if d.Unrestricted || !slices.Equal(d.Projects, []string{"a", "b", "c", "d"}) {
		t.Fatalf("scope = %+v, want projects a b c d", d)
	}
	for p, tr := range want {
		if d.Tiers[p] != tr {
			t.Errorf("tier in %s = %q, want %q", p, d.Tiers[p], tr)
		}
	}
	if len(d.Tiers) != len(want) {
		t.Errorf("tiers = %v, want %v", d.Tiers, want)
	}
}

func TestMe(t *testing.T) {
	f := newScopedFixture(t)
	get := func(tok string) meJSON {
		t.Helper()
		code, body := f.do(t, tok, http.MethodGet, "/v1/me", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/me as %s: %d %s", tok, code, body)
		}
		var me meJSON
		if err := json.Unmarshal([]byte(body), &me); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return me
	}
	cases := []struct {
		tok          string
		operator     string
		unrestricted bool
		lands        []meLand
	}{
		{tokScopedGuestOther, "clgo@example.com", false, []meLand{{"claimlayer", tierGuest}, {"other", tierMember}}},
		{tokScopedOwner, "clo@example.com", false, []meLand{{"claimlayer", tierOwner}}},
		{tokScopedWriter, "clw@example.com", false, []meLand{{"claimlayer", tierMember}}},
		{tokScopedNoGroup, "nogroup@example.com", false, []meLand{}},
		{tokOwnerAdmin, "owner@example.com", true, []meLand{}},
	}
	for _, c := range cases {
		me := get(c.tok)
		if me.OperatorID != c.operator || me.Source != whoamiSourceAuth || me.Unrestricted != c.unrestricted || !slices.Equal(me.Lands, c.lands) {
			t.Errorf("%s: /v1/me = %+v, want operator %s unrestricted %v lands %v", c.tok, me, c.operator, c.unrestricted, c.lands)
		}
	}
}

// A per-agent fleet token reports its agent and agent bead, from the
// credential; no other token does, whatever its X-Actor says (gd-P5wjHDweih:
// the slack-bridge takes an agent's identity from this).
func TestMe_AgentTokenNamesItsAgent(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	get := func(tok string, headers map[string]string) meJSON {
		t.Helper()
		rec := doBearerJSON(t, h, http.MethodGet, "/v1/me", tok, nil, headers)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/me as %s: %d %s", tok, rec.Code, rec.Body)
		}
		var me meJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
		return me
	}
	if me := get(testAgentToken, map[string]string{"X-Actor": "crew-b"}); me.Agent != "crew-a" || me.AgentBead != "gd-a1" || me.TokenProject != "gardenio" {
		t.Errorf("agent token: /v1/me = %+v, want agent crew-a, bead gd-a1, project gardenio", me)
	}
	for _, tok := range []string{testStaticToken, testBridgeToken, testOtherFleetToken} {
		if me := get(tok, map[string]string{"X-Actor": "crew-a"}); me.Agent != "" || me.AgentBead != "" {
			t.Errorf("%s: /v1/me = %+v, want no agent", tok, me)
		}
	}
}

// An unrestricted OIDC caller sees the lands its groups name, as operator.
func TestComputeMe_UnrestrictedListsGroupLands(t *testing.T) {
	ctx := withAuthMethod(t.Context(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer, RoleOperator, RoleAdmin})
	ctx = WithGroups(ctx, []string{"/project-b/owners", "/project-a"})
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "admin@example.com"})
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/me", nil)
	me := computeMe(r)
	if !me.Unrestricted || !slices.Equal(me.Lands, []meLand{{"a", tierOperator}, {"b", tierOperator}}) {
		t.Errorf("computeMe = %+v, want unrestricted with lands a, b as operator", me)
	}
}
