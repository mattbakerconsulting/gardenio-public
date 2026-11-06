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

// Standing consent to push (gd-QV2WuUjely): the consent is a gated user field
// only an admin or the person may set, and the daemon, not the filing agent,
// answers a consent-to-push decision for a person who gave it.

// consentDecisionFields is the body gb consent-to-push files, addressed to
// operatedBy.
func consentDecisionFields(t *testing.T, operatedBy string, extra map[string]any) json.RawMessage {
	t.Helper()
	f := map[string]any{
		"prompt": "CI fix ready — push to your MR?",
		"options": []map[string]string{
			{"id": "push-to-my-mr", "short": "Push to my MR", "label": "Push the fix to my MR branch"},
			{"id": "open-separate-mr", "short": "Open a separate MR", "label": "Open a separate MR with the fix"},
			{"id": "no-thanks", "short": "No thanks", "label": "No thanks — discard the prepared fix"},
		},
		"consent_push":        "true",
		"consent_push_mr":     "https://gitlab.com/acme/app/-/merge_requests/1",
		"consent_push_branch": "ci-fix/x",
		"mr_url":              "https://gitlab.com/acme/app/-/merge_requests/1",
		"operated_by":         operatedBy,
	}
	for k, v := range extra {
		f[k] = v
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileConsentDecision(t *testing.T, srv *BeadsServer, fields json.RawMessage) *model.Bead {
	t.Helper()
	bead, err := srv.createBead(agentFleetCtx("gd-crew", "crew-a"), createBeadInput{
		Title: "CI fix ready", Type: "decision", Kind: "data", Labels: []string{"project:gardenio"},
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("create decision: %v", err)
	}
	return bead
}

func TestStandingConsent_DaemonAnswersForConsentingPerson(t *testing.T) {
	for name, operatedBy := range map[string]string{
		"oidc addressee":  "oidc:Alice@Example.com",
		"slack addressee": "slack:U0ALICE",
	} {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			seedLinkUserBead(ms, "u-alice", "admin@example.com", map[string]any{
				"email": "alice@example.com", "slack_id": "U0ALICE", userConsentPushAutoField: true,
			})

			bead := fileConsentDecision(t, srv, consentDecisionFields(t, operatedBy, nil))
			stored := ms.beads[bead.ID]
			if bead.Status != model.StatusClosed || stored.Status != model.StatusClosed {
				t.Fatalf("decision not answered: returned %s, stored %s", bead.Status, stored.Status)
			}
			f := parseFieldsToStringMap(stored.Fields)
			want := map[string]string{
				"chosen":                     "push-to-my-mr",
				"responded_by":               standingConsentActor,
				decisionResolverSubjectField: standingConsentActor,
				decisionResolverAdminField:   "false",
				standingConsentUserField:     "u-alice",
			}
			for k, v := range want {
				if f[k] != v {
					t.Errorf("%s = %q, want %q", k, f[k], v)
				}
			}
			if answeredByHumanOrSlackBridge(f["responded_by"]) {
				t.Errorf("a standing-consent answer must not count as a human's for continuation")
			}
		})
	}
}

// The old location, a key in preferences, is writable by any agent and is
// not consent.
func TestStandingConsent_PreferencesKeyIsNotConsent(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedLinkUserBead(ms, "u-alice", "static", map[string]any{
		"email":       "alice@example.com",
		"preferences": map[string]any{"consent_push_to_mr_auto": true},
	})
	bead := fileConsentDecision(t, srv, consentDecisionFields(t, "oidc:alice@example.com", nil))
	if ms.beads[bead.ID].Status == model.StatusClosed {
		t.Fatalf("a preferences key answered the decision")
	}
}

func TestStandingConsent_LeavesOtherDecisionsOpen(t *testing.T) {
	cases := map[string]struct {
		users  map[string]map[string]any
		fields func(t *testing.T) json.RawMessage
	}{
		"no consent": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com"}},
			fields: func(t *testing.T) json.RawMessage {
				return consentDecisionFields(t, "oidc:alice@example.com", nil)
			},
		},
		"consent withdrawn": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com", userConsentPushAutoField: false}},
			fields: func(t *testing.T) json.RawMessage {
				return consentDecisionFields(t, "oidc:alice@example.com", nil)
			},
		},
		"not a consent-to-push decision": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com", userConsentPushAutoField: true}},
			fields: func(t *testing.T) json.RawMessage {
				return decisionFields(t, map[string]any{"operated_by": "oidc:alice@example.com"})
			},
		},
		"flag without the push option": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com", userConsentPushAutoField: true}},
			fields: func(t *testing.T) json.RawMessage {
				return decisionFields(t, map[string]any{"operated_by": "oidc:alice@example.com", "consent_push": "true"})
			},
		},
		"addressed to someone else": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com", userConsentPushAutoField: true}},
			fields: func(t *testing.T) json.RawMessage {
				return consentDecisionFields(t, "oidc:bob@example.com", nil)
			},
		},
		"ambiguous addressee": {
			users: map[string]map[string]any{
				"u-alice":  {"email": "alice@example.com", userConsentPushAutoField: true},
				"u-alice2": {"email": "alice@example.com"},
			},
			fields: func(t *testing.T) json.RawMessage {
				return consentDecisionFields(t, "oidc:alice@example.com", nil)
			},
		},
		"no addressee": {
			users: map[string]map[string]any{"u-alice": {"email": "alice@example.com", userConsentPushAutoField: true}},
			fields: func(t *testing.T) json.RawMessage {
				return consentDecisionFields(t, "", nil)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			for id, f := range tc.users {
				seedLinkUserBead(ms, id, "admin@example.com", f)
			}
			bead := fileConsentDecision(t, srv, tc.fields(t))
			stored := ms.beads[bead.ID]
			if stored.Status == model.StatusClosed || bead.Status == model.StatusClosed {
				t.Fatalf("decision was answered")
			}
			if f := parseFieldsToStringMap(stored.Fields); f["chosen"] != "" || f["responded_by"] != "" {
				t.Fatalf("answer fields written: %v", f)
			}
		})
	}
}

// An agent cannot be born with the standing-consent marker, nor write it later.
func TestStandingConsent_UserMarkerIsServerStamped(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	bead := fileConsentDecision(t, srv, consentDecisionFields(t, "oidc:nobody@example.com", map[string]any{
		standingConsentUserField: "u-alice",
		"responded_by":           standingConsentActor,
	}))
	f := parseFieldsToStringMap(ms.beads[bead.ID].Fields)
	if f[standingConsentUserField] != "" || f["responded_by"] != "" {
		t.Fatalf("client-supplied standing-consent marker survived create: %v", f)
	}
	if d := denyDecisionStampedKeyWrite("update", ms.beads[bead.ID], []string{standingConsentUserField}); d == nil {
		t.Fatalf("%s writable after create", standingConsentUserField)
	}
}

// consent_push_to_mr_auto shares the identity fields' gate: no agent token and
// no non-admin human may set it on a user bead through the generic paths.
func TestStandingConsent_GenericWriteGate(t *testing.T) {
	s, ms, _ := newTestServer()
	seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com"}`)
	patch, _ := json.Marshal(map[string]any{userConsentPushAutoField: true})
	existing := ms.beads["gd-user1"].Fields
	if d := s.authorizeUserIdentityFields(machineUserCtx(), "update", "user", patch, existing); d == nil {
		t.Errorf("static token may set standing consent")
	}
	if d := s.authorizeUserIdentityFields(fleetUserCtx(), "update", "user", patch, existing); d == nil {
		t.Errorf("fleet token may set standing consent")
	}
	if d := s.authorizeUserIdentityFields(WithRoles(oidcCtx("owner@example.com", nil), []string{RoleOperator}), "update", "user", patch, existing); d == nil {
		t.Errorf("non-admin OIDC caller may set standing consent through the generic path")
	}
	if d := s.authorizeUserIdentityFields(adminOIDCCtx(), "update", "user", patch, existing); d != nil {
		t.Errorf("admin refused: %v", d)
	}
	if err := s.mergeBeadFields(machineUserCtx(), "gd-user1", map[string]any{userConsentPushAutoField: true}); err == nil {
		t.Errorf("mergeBeadFields let the static token set standing consent")
	}
	if userHasStandingConsentToPush(ms.beads["gd-user1"]) {
		t.Fatalf("standing consent was written")
	}
}

func consentPushRequest(ctx context.Context, method string, body any) *http.Request {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/v1/users/me/consent-push", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(ctx)
}

func TestMyConsentPush_PersonSetsOwnConsent(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	seedLinkUserBead(ms, "u-bob", "auto-provision", map[string]any{"email": "bob@example.com"})
	bobBefore := string(ms.beads["u-bob"].Fields)
	alice := oidcCtx("alice@example.com", []string{RoleOperator})

	rec := httptest.NewRecorder()
	s.handleSetMyConsentPush(rec, consentPushRequest(alice, http.MethodPost, map[string]any{"auto": true, "user_bead_id": "u-bob"}))
	requireStatus(t, rec, http.StatusOK)
	var resp UserConsentPushResponse
	decodeJSON(t, rec, &resp)
	if resp.UserBeadID != "u-alice" || !resp.Auto || !userHasStandingConsentToPush(ms.beads["u-alice"]) {
		t.Fatalf("consent not recorded on alice's bead: resp %+v, fields %s", resp, ms.beads["u-alice"].Fields)
	}
	if string(ms.beads["u-bob"].Fields) != bobBefore {
		t.Fatalf("another person's bead changed")
	}

	rec = httptest.NewRecorder()
	s.handleMyConsentPush(rec, consentPushRequest(alice, http.MethodGet, nil))
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &resp)
	if !resp.Auto {
		t.Fatalf("GET reports no consent after opt-in")
	}

	rec = httptest.NewRecorder()
	s.handleSetMyConsentPush(rec, consentPushRequest(alice, http.MethodPost, map[string]any{"auto": false}))
	requireStatus(t, rec, http.StatusOK)
	if userHasStandingConsentToPush(ms.beads["u-alice"]) {
		t.Fatalf("opt-out did not withdraw consent")
	}
}

func TestMyConsentPush_Refusals(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	before := string(ms.beads["u-alice"].Fields)

	for name, ctx := range map[string]context.Context{
		"static token":    WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"}),
		"agent fleet":     agentFleetCtx("gd-crew", "crew-a"),
		"machine account": WithAuthIdentity(withAuthMethod(context.Background(), authMethodMachine), AuthIdentity{Type: "machine", Subject: "machine:beads-viewer"}),
		"applicant":       oidcCtx("alice@example.com", nil),
	} {
		rec := httptest.NewRecorder()
		s.handleSetMyConsentPush(rec, consentPushRequest(ctx, http.MethodPost, map[string]any{"auto": true}))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	s.handleSetMyConsentPush(rec, consentPushRequest(oidcCtx("alice@example.com", []string{RoleOperator}), http.MethodPost, map[string]any{}))
	requireStatus(t, rec, http.StatusBadRequest)

	if string(ms.beads["u-alice"].Fields) != before {
		t.Fatalf("a refused request wrote: %s", ms.beads["u-alice"].Fields)
	}

	rec = httptest.NewRecorder()
	s.handleSetMyConsentPush(rec, consentPushRequest(oidcCtx("nobody@example.com", []string{RoleOperator}), http.MethodPost, map[string]any{"auto": true}))
	requireStatus(t, rec, http.StatusNotFound)
}
