package server

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
)

// Review of PR #246 (gd-uAvleAZUvV): the identity-field gate sat in
// createBead/updateBead only, so the single-field mutation endpoints
// (append / reorder / edit / increment) — which write through store primitives
// keyed on the bead id — let a static token append to email_aliases and get
// 200. These tests drive every route that writes a field of an existing bead
// through the real HTTP stack as the shared static token (the credential every
// agent pod holds) and as an OIDC gardenio-admin.

const userBeadFieldsJSON = `{"email":"owner@example.com","email_aliases":["a@example.com"],"allowed_projects":"p1","capabilities":{"x":"1"},"status_message":"hi"}`

type fieldWriteCase struct {
	name   string
	method string
	path   string
	body   any
	// denied: a static/fleet token must get 403 and the bead must be unchanged.
	// !denied: a non-identity field; every caller (static included) gets 200.
	denied bool
}

func fieldWriteCases() []fieldWriteCase {
	const id = "gd-user1"
	field := func(f, op string) string { return "/v1/beads/" + id + "/fields/" + f + "/" + op }
	edit := func(f string, req editSubFieldRequest, denied bool, name string) fieldWriteCase {
		return fieldWriteCase{name: name, method: http.MethodPost, path: field(f, "edit"), body: req, denied: denied}
	}
	return []fieldWriteCase{
		// --- identity fields: the bypass the reviewer reproduced and its siblings.
		{"append email_aliases", http.MethodPost, field("email_aliases", "append"), appendArrayFieldRequest{Element: "attacker@example.com"}, true},
		{"reorder email_aliases (CAS)", http.MethodPost, field("email_aliases", "reorder"), casArrayFieldRequest{Expected: []string{"a@example.com"}, Next: []string{"attacker@example.com"}}, true},
		{"append slack_id", http.MethodPost, field("slack_id", "append"), appendArrayFieldRequest{Element: "UATTACKER"}, true},
		edit("capabilities", editSubFieldRequest{Op: "set", Key: "admin", Value: "1"}, true, "edit capabilities set-key"),
		edit("capabilities", editSubFieldRequest{Op: "unset-key", Key: "x"}, true, "edit capabilities unset-key"),
		edit("allowed_projects", editSubFieldRequest{Op: "append", Element: "p2"}, true, "edit allowed_projects csv append"),
		edit("allowed_projects", editSubFieldRequest{Op: "remove", Element: "p1"}, true, "edit allowed_projects csv remove"),
		{"increment email_aliases", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"email_aliases": 1}, true},
		{"increment mixed identity+benign", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"login_count": 1, "jira_user_id": 1}, true},
		{"close smuggling email_aliases", http.MethodPost, "/v1/beads/" + id + "/close", map[string]any{"email_aliases": []string{"attacker@example.com"}}, true},
		{"PATCH email_aliases", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"email_aliases": []string{"attacker@example.com"}}}, true},
		{"PATCH slack_id", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"slack_id": "UATTACKER"}}, true},
		{"POST create type=user with email_aliases", http.MethodPost, "/v1/beads",
			map[string]any{"title": "evil", "type": "user", "fields": map[string]any{"email_aliases": []string{"attacker@example.com"}}}, true},
		// --- terms acceptance: only the person, through POST /v1/users/me/terms.
		{"PATCH terms_version", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"terms_version": "v9"}}, true},
		{"PATCH terms_accepted_at", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"terms_accepted_at": "2026-10-19T00:00:00Z"}}, true},
		{"increment terms_version", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"terms_version": 1}, true},
		{"close smuggling terms_accepted_at", http.MethodPost, "/v1/beads/" + id + "/close", map[string]any{"terms_accepted_at": "2026-10-19T00:00:00Z"}, true},
		// --- read positions: only the person, through POST /v1/users/me/read-cursors.
		{"PATCH read_cursors", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"read_cursors": map[string]string{"gd-ch1": "gd-m9"}}}, true},
		edit("read_cursors", editSubFieldRequest{Op: "set", Key: "gd-ch1", Value: "gd-m9"}, true, "edit read_cursors set-key"),
		{"close smuggling read_cursors", http.MethodPost, "/v1/beads/" + id + "/close", map[string]any{"read_cursors": map[string]string{"gd-ch1": "gd-m9"}}, true},
		// --- the welcome tour: only the person, through POST /v1/users/me/welcome-tour.
		{"PATCH welcome_tour_seen_at", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"welcome_tour_seen_at": "2026-11-01T00:00:00Z"}}, true},

		// --- non-identity fields keep working for the machine allow-list.
		{"append recent_tabs", http.MethodPost, field("recent_tabs", "append"), appendArrayFieldRequest{Element: "inbox"}, false},
		edit("profile_prefs", editSubFieldRequest{Op: "set", Key: "theme", Value: "dark"}, false, "edit profile_prefs set-key"),
		edit("pinned", editSubFieldRequest{Op: "append", Element: "gd-1"}, false, "edit pinned csv append"),
		{"increment login_count", http.MethodPost, "/v1/beads/" + id + "/increment", map[string]float64{"login_count": 1}, false},
		{"PATCH status_message", http.MethodPatch, "/v1/beads/" + id, map[string]any{"fields": map[string]any{"status_message": "away"}}, false},
	}
}

// countUserBeads counts type:user beads (a denial also writes an audit bead).
func countUserBeads(ms *mockStore) int {
	n := 0
	for _, b := range ms.beads {
		if b.Type == "user" {
			n++
		}
	}
	return n
}

func fieldWriteRequest(t *testing.T, h http.Handler, c fieldWriteCase, bearer, operator string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(c.body)
	req := httptest.NewRequest(c.method, c.path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if operator != "" {
		req.Header.Set("X-Operated-By", operator)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestFieldMutationEndpoints_UserIdentityGate is the table-driven proof that
// no HTTP route that writes a field of an existing type:user bead lets the
// shared static token touch an identity field, while its legitimate
// non-identity writes still return 200.
func TestFieldMutationEndpoints_UserIdentityGate(t *testing.T) {
	for _, c := range fieldWriteCases() {
		t.Run("static/"+c.name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			srv.userProvisioner = nil
			h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
			seedIdentityUserBead(t, ms, "gd-user1", userBeadFieldsJSON)
			before := string(ms.beads["gd-user1"].Fields)
			nBefore := countUserBeads(ms)

			rec := fieldWriteRequest(t, h, c, "primary-token", "slack:U0TESTER1")
			if c.denied {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				if after := string(ms.beads["gd-user1"].Fields); after != before {
					t.Errorf("fields changed despite 403:\n before %s\n after  %s", before, after)
				}
				if countUserBeads(ms) != nBefore {
					t.Errorf("a user bead was created despite 403")
				}
				return
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("non-identity write: code = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The gate is selective, not a blanket: an OIDC gardenio-admin performs the
// same identity writes (the four single-field endpoints plus PATCH).
func TestFieldMutationEndpoints_UserIdentityGate_AdminAllowed(t *testing.T) {
	for _, c := range fieldWriteCases() {
		if !c.denied || strings.HasSuffix(c.path, "/close") || c.path == "/v1/beads" {
			continue
		}
		t.Run("admin/"+c.name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			srv.userProvisioner = nil
			h := srv.NewHTTPHandler(&bearerSubjectVerifier{}) // grants gardenio-admin
			seedIdentityUserBead(t, ms, "gd-user1", userBeadFieldsJSON)
			rec := fieldWriteRequest(t, h, c, "admin@example.com", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("admin: code = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Other bead types are untouched by the user-bead gate: a static token still
// appends to a task's field named like an identity field.
func TestFieldMutationEndpoints_NonUserBeadUnaffected(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	newProjectBead(t, ms, "gd-task1", "p")
	rec := fieldWriteRequest(t, h, fieldWriteCase{method: http.MethodPost, path: "/v1/beads/gd-task1/fields/email_aliases/append",
		body: appendArrayFieldRequest{Element: "x@example.com"}}, "primary-token", "slack:U0TESTER1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
}

// mergeBeadFields is the shared chokepoint for the server's own lifecycle
// merges: an identity key aimed at a user bead by a non-OIDC caller is refused.
func TestMergeBeadFields_UserIdentityKeyDeniedForMachine(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedIdentityUserBead(t, ms, "gd-user1", userBeadFieldsJSON)
	before := string(ms.beads["gd-user1"].Fields)
	err := srv.mergeBeadFields(machineUserCtx(), "gd-user1", map[string]any{"slack_id": "UATTACKER"})
	requireDenied(t, err)
	if string(ms.beads["gd-user1"].Fields) != before {
		t.Errorf("fields changed despite denial")
	}
	if err := srv.mergeBeadFields(machineUserCtx(), "gd-user1", map[string]any{"status_message": "ok"}); err != nil {
		t.Errorf("non-identity merge: %v", err)
	}
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-user1", map[string]any{"slack_id": "U2"}); err != nil {
		t.Errorf("admin identity merge: %v", err)
	}
}

// fieldWriters are the store methods that write a bead's fields. Every call
// site of one of these in non-test server code must be listed below with the
// reason it is covered; a new, unlisted caller fails this test, forcing its
// author to route it through the identity gate (or justify it here).
var fieldWriters = map[string]bool{
	"CreateBead": true, "UpdateBead": true, "MergeBeadFields": true, "IncrementBeadFields": true,
	"AppendBeadArrayField": true, "InsertBeadArrayField": true, "CASBeadArrayField": true,
	"MergeBeadMapField": true, "EditBeadCSVField": true,
}

var gatedFieldWriteCallers = map[string]string{
	// HTTP/gRPC entry points: gated by authorizeUserIdentityFields.
	"createBead": "authorizeUserIdentityFields (create)",
	"updateBead": "authorizeUserIdentityFields (update); also reached by undo, gRPC and escalation replay",
	// Single-field endpoints: gated by tokenAuthorizedForBead -> authorizeUserBeadFieldWrite.
	"handleIncrementBeadFields":   "tokenAuthorizedForBead",
	"handleAppendBeadArrayField":  "tokenAuthorizedForBead",
	"handleReorderBeadArrayField": "tokenAuthorizedForBead",
	"handleEditBeadSubField":      "tokenAuthorizedForBead",
	// Shared merge helper: identity keys are gated inside it.
	"mergeBeadFields": "authorizeUserBeadFieldWrite on identity keys",
	// Server-internal writes that never take caller-chosen field keys or touch user beads.
	"handlePourFormula":          "plan/molecule phase_order (fixed key, non-user beads)",
	"handleRevokeWindow":         "revoked_at/revoked_by on a type=window bead (fixed keys)",
	"flushWindowViews":           "view_count/last_viewed_at on a type=window bead (fixed keys)",
	"parkDecisionGatedBead":      "server state transition on a non-user bead",
	"parkOrphanedChild":          "server state transition on a non-user bead",
	"wakeGardenersAwaiting":      "enabled/awaiting (fixed keys) on a trigger or schedule bead",
	"maybeCopyThreadID":          "server-derived thread id on the new bead",
	"maybeStampOpenedByOperator": "server-derived thread opener on the new bead",
	"autoApproveSpawnEscalation": "escalation bead status",
	"commitReleaseWrites":        "fixed keys (deployments, status) on type=release beads and a declared target, for writeDeployment and repairReleaseDeployments after releaseWriterDenial",
	"finishEscalationApproval":   "escalation bead status (handleApproveEscalation tail)",
	"maybeApproveByRuling":       "escalation bead status (P7.5 ruling approval, the approve tail)",
	"createUserBead":             "server's own OIDC-login provisioning (no caller-chosen fields)",
	"syncGroupProjects":          "server's own OIDC group sync (token-verified groups)",
	// Slack account linking: fixed keys (identity_link.slack, slack_id) on the
	// OIDC caller's OWN user bead, after the handler's own conflict checks.
	"handleLinkSlackIdentity":   "server-side bind of fixed keys on the caller's own user bead (OIDC only)",
	"handleUnlinkSlackIdentity": "server-side clear of fixed keys on the caller's own user bead (OIDC only)",
	// Lands terms acceptance: fixed keys on the OIDC caller's OWN user bead.
	"handleAcceptTerms": "server-side merge of terms_version/terms_accepted_at on the caller's own user bead (OIDC only)",
	"writeReadCursors":  "server-side merge of read_cursors only, on the caller's own user bead (readCursorsCaller: OIDC only)",
	// Standing consent to push (gd-QV2WuUjely): one fixed key on the OIDC
	// caller's OWN user bead.
	"handleSetMyConsentPush": "server-side merge of consent_push_to_mr_auto only, on the caller's own user bead (OIDC only)",
	// The welcome tour (step 9.14b): one fixed key, stamped by the daemon, on
	// the OIDC caller's OWN user bead.
	"handleSeenMyWelcomeTour": "server-side merge of welcome_tour_seen_at only, on the caller's own user bead (OIDC only)",
}

func TestStoreFieldWriters_AreGated(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !fieldWriters[sel.Sel.Name] {
					return true
				}
				// Only calls on a store (s.store.X, up.store.X, tx.X), not s.createBead.
				if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name != "store" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name != "tx" {
					return true
				}
				found[fd.Name.Name] = append(found[fd.Name.Name], f+":"+sel.Sel.Name)
				return true
			})
		}
	}
	var unlisted []string
	for fn, sites := range found {
		if _, ok := gatedFieldWriteCallers[fn]; !ok {
			unlisted = append(unlisted, fn+" ("+strings.Join(sites, ", ")+")")
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Fatalf("store field-write call sites not covered by the user identity gate (route them through "+
			"tokenAuthorizedForBead / authorizeUserIdentityFields / mergeBeadFields, or list them in "+
			"gatedFieldWriteCallers with a reason):\n  %s", strings.Join(unlisted, "\n  "))
	}
	for fn := range gatedFieldWriteCallers {
		if _, ok := found[fn]; !ok {
			t.Errorf("gatedFieldWriteCallers lists %q but it no longer calls a store field writer; remove it", fn)
		}
	}
}
