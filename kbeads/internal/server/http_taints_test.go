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

// taintTestVerifier is a test-only IdentityVerifier + RoleExtractor: the
// bearer token is treated as the subject verbatim (like bearerSubjectVerifier
// in http_test.go), but its role is looked up from a per-test map instead of
// being hardcoded to admin — needed to exercise the non-admin / non-owner
// paths that newTestServerAuth's always-admin verifier can't reach.
type taintTestVerifier struct {
	roles map[string][]string // subject -> roles; default RoleViewer
}

func (v *taintTestVerifier) Verify(_ context.Context, _ string) error { return nil }

func (v *taintTestVerifier) VerifyAndIdentify(_ context.Context, raw string) (string, error) {
	return raw, nil
}

func (v *taintTestVerifier) ExtractRoles(_ context.Context, raw string) ([]string, error) {
	if roles, ok := v.roles[raw]; ok {
		return roles, nil
	}
	return []string{RoleViewer}, nil
}

// newTaintTestServer wires a server whose OIDC verifier grants each bearer
// subject the roles given in `roles` (default RoleViewer), so tests can
// authenticate as an admin, an owner-team viewer, or an unrelated viewer.
func newTaintTestServer(roles map[string][]string) (*BeadsServer, *mockStore, http.Handler) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	return s, ms, s.NewHTTPHandler(&taintTestVerifier{roles: roles})
}

// taintDoJSONAs mirrors doJSONAs (http_test.go) against the given handler.
func taintDoJSONAs(t *testing.T, handler http.Handler, method, path string, body any, subject string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+subject)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// putTaint seeds a taint directly into the mock store's registry, bypassing
// the HTTP layer, so list/show tests can fix up owner_team and scrub_policy
// without going through SetTaint's (real-store-only) validation.
func putTaint(ms *mockStore, t *model.Taint) {
	if ms.taintRegistry == nil {
		ms.taintRegistry = make(map[string]*model.Taint)
	}
	ms.taintRegistry[t.Name] = t
}

// --- POST /v1/taints ---

func TestHandleCreateTaint_AdminOnly(t *testing.T) {
	_, _, h := newTaintTestServer(map[string][]string{
		"admin@example.com":  {RoleAdmin},
		"viewer@example.com": {RoleViewer},
	})

	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name":       "customer-pii",
		"owner_team": "gd-team-prod",
	}, "viewer@example.com")
	requireStatus(t, rec, http.StatusForbidden)
}

// TestHandleCreateTaint_StaticTokenDenied pins the fix for the reopened
// review finding: requireRole(RoleAdmin) alone lets every non-OIDC auth
// method (the shared primary static token every agent and coopmux hold)
// through unchecked, so without callerMayCreateTaint's explicit gate a bare
// static-token caller could create a taint, name any owner team, and pick
// the scratch preset (auto-purge). A relayed X-Operated-By identity for a
// human who is a member of the target owner team must not change the
// outcome: the owner team is caller-supplied on this very request, so
// "caller belongs to the team they named" is not a legitimate admin
// substitute (see callerMayCreateTaint's doc comment).
func TestHandleCreateTaint_StaticTokenDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := s.NewHTTPHandler(&taintTestVerifier{}, withTestToken(nil, "admin-token"))
	ms.operatorAliases = map[string]int64{"oidc\x00owner@example.com": 7}
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":["oidc:owner@example.com"]}`)

	body, _ := json.Marshal(map[string]any{
		"name":       "customer-pii",
		"owner_team": "gd-team-prod",
		"preset":     "scratch",
	})
	req := httptest.NewRequest("POST", "/v1/taints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "oidc:owner@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	if _, ok := ms.taintRegistry["customer-pii"]; ok {
		t.Fatal("static-token caller must not be able to create a taint")
	}
}

// TestHandleCreateTaint_FleetTokenDenied mirrors the static-token case for a
// fleet-scoped token, which is likewise a non-OIDC auth method that must not
// bypass the admin-role gate on taint creation.
func TestHandleCreateTaint_FleetTokenDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("gardenio", map[string]string{"tok-fleet": "some-bridge"})
	h := s.NewHTTPHandler(&taintTestVerifier{}, withTestToken(fc, "admin-token"))

	body, _ := json.Marshal(map[string]any{
		"name":       "customer-pii",
		"owner_team": "gd-team-prod",
	})
	req := httptest.NewRequest("POST", "/v1/taints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-fleet")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)
}

func TestHandleCreateTaint_AdminCreatesWithImplicitOwnerGrant(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name":        "customer-pii",
		"description": "customer PII",
		"owner_team":  "gd-team-prod",
	}, "admin@example.com")
	requireStatus(t, rec, http.StatusCreated)

	var got taintDetail
	decodeJSON(t, rec, &got)
	if got.Name != "customer-pii" || got.OwnerTeam != "gd-team-prod" {
		t.Fatalf("unexpected created taint: %+v", got)
	}

	stored, ok := ms.taintRegistry["customer-pii"]
	if !ok {
		t.Fatalf("expected taint to be persisted in the store")
	}
	if stored.OwnerTeam != "gd-team-prod" {
		t.Fatalf("expected owner_team gd-team-prod, got %q", stored.OwnerTeam)
	}
	// ADR §10.1: ownership implies the grant — creating a taint must never
	// write a taint_grants row. There is no grants store call anywhere in
	// handleCreateTaint, so nothing to assert beyond the taint itself
	// existing; this comment documents the invariant the handler upholds.
}

func TestHandleCreateTaint_PresetAndPolicyMutuallyExclusive(t *testing.T) {
	_, _, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name":         "customer-pii",
		"owner_team":   "gd-team-prod",
		"preset":       "production",
		"scrub_policy": json.RawMessage(`{"default":"human"}`),
	}, "admin@example.com")
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleCreateTaint_UnknownPresetRejected(t *testing.T) {
	_, _, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name":       "customer-pii",
		"owner_team": "gd-team-prod",
		"preset":     "yolo",
	}, "admin@example.com")
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestHandleCreateTaint_PresetResolvesServerSide pins that --preset is
// resolved to the docs/adr/taints.md §10.2 policy inside the handler (so any
// caller, not just the CLI, gets the same result), and that the persisted
// scrub_policy is byte-for-byte what model.PresetScrubPolicy produces.
func TestHandleCreateTaint_PresetResolvesServerSide(t *testing.T) {
	for _, preset := range []model.PresetName{model.PresetProduction, model.PresetInternal, model.PresetScratch} {
		t.Run(string(preset), func(t *testing.T) {
			_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

			name := "taint-" + string(preset)
			rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
				"name":       name,
				"owner_team": "gd-team-prod",
				"preset":     preset,
			}, "admin@example.com")
			requireStatus(t, rec, http.StatusCreated)

			var got taintDetail
			decodeJSON(t, rec, &got)

			var gotPolicy model.ScrubPolicy
			if err := json.Unmarshal(got.ScrubPolicy, &gotPolicy); err != nil {
				t.Fatalf("failed to decode returned scrub_policy: %v", err)
			}
			wantPolicy := model.PresetScrubPolicy(preset, "gd-team-prod")
			wantEncoded, _ := json.Marshal(wantPolicy)
			var want model.ScrubPolicy
			_ = json.Unmarshal(wantEncoded, &want)

			gotEncoded, _ := json.Marshal(&gotPolicy)
			if string(gotEncoded) != string(wantEncoded) {
				t.Fatalf("preset %q: scrub_policy mismatch\n got: %s\nwant: %s", preset, gotEncoded, wantEncoded)
			}

			// Also assert what's actually persisted matches — the handler
			// must not just echo a resolved value in the response while
			// storing something else.
			stored := ms.taintRegistry[name]
			if stored == nil {
				t.Fatalf("expected taint %q to be persisted", name)
			}
			if string(stored.ScrubPolicy) != string(wantEncoded) {
				t.Fatalf("preset %q: persisted scrub_policy mismatch\n got: %s\nwant: %s", preset, stored.ScrubPolicy, wantEncoded)
			}
		})
	}
}

// --- GET /v1/taints (list) ---

func TestHandleListTaints_NonOwnerNonAdminOmitsPolicy(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"outsider@example.com": {RoleViewer}})
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`)})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "outsider@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 1 {
		t.Fatalf("expected 1 taint, got %d", len(resp.Taints))
	}
	row := resp.Taints[0]
	if _, present := row["scrub_policy"]; present {
		t.Fatalf("non-owner/non-admin list row must omit scrub_policy, got %v", row)
	}
	if row["name"] != "customer-pii" || row["owner_team"] != "gd-team-prod" {
		t.Fatalf("expected name/owner_team to remain visible, got %v", row)
	}
}

func TestHandleListTaints_OwnerTeamSeesOwnPolicyOnly(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"owner@example.com": {RoleViewer}})
	ms.operatorAliases = map[string]int64{"oidc\x00owner@example.com": 7}
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":["oidc:owner@example.com"]}`)
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`)})
	putTaint(ms, &model.Taint{ID: 2, Name: "internal-logs", OwnerTeam: "gd-team-infra", ScrubPolicy: json.RawMessage(`{"default":"human"}`)})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "owner@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 2 {
		t.Fatalf("expected 2 taints, got %d", len(resp.Taints))
	}
	for _, row := range resp.Taints {
		_, hasPolicy := row["scrub_policy"]
		switch row["name"] {
		case "customer-pii":
			if !hasPolicy {
				t.Fatalf("owner team must see scrub_policy for its own taint, got %v", row)
			}
		case "internal-logs":
			if hasPolicy {
				t.Fatalf("owner team must NOT see scrub_policy for another team's taint, got %v", row)
			}
		default:
			t.Fatalf("unexpected taint row %v", row)
		}
	}
}

func TestHandleListTaints_AdminSeesAllPolicies(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`)})
	putTaint(ms, &model.Taint{ID: 2, Name: "internal-logs", OwnerTeam: "gd-team-infra", ScrubPolicy: json.RawMessage(`{"default":"human"}`)})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	for _, row := range resp.Taints {
		if _, hasPolicy := row["scrub_policy"]; !hasPolicy {
			t.Fatalf("admin must see scrub_policy for every taint, got %v", row)
		}
	}
}

// --- GET /v1/taints/{name} (show) ---

func TestHandleGetTaint_NonOwnerNonAdminGetsNameAndDescriptionOnly(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"outsider@example.com": {RoleViewer}})
	putTaint(ms, &model.Taint{
		ID: 1, Name: "customer-pii", Description: "customer PII",
		OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`),
	})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints/customer-pii", nil, "outsider@example.com")
	requireStatus(t, rec, http.StatusOK)

	var row map[string]any
	decodeJSON(t, rec, &row)
	if row["name"] != "customer-pii" || row["description"] != "customer PII" {
		t.Fatalf("expected name/description, got %v", row)
	}
	for _, forbidden := range []string{"scrub_policy", "owner_team", "id", "created_at", "updated_at"} {
		if _, present := row[forbidden]; present {
			t.Fatalf("non-owner/non-admin show must not reveal %q, got %v", forbidden, row)
		}
	}
}

func TestHandleGetTaint_OwnerTeamGetsFullDetail(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"owner@example.com": {RoleViewer}})
	ms.operatorAliases = map[string]int64{"oidc\x00owner@example.com": 7}
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":["oidc:owner@example.com"]}`)
	putTaint(ms, &model.Taint{
		ID: 1, Name: "customer-pii", Description: "customer PII",
		OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`),
	})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints/customer-pii", nil, "owner@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got taintDetail
	decodeJSON(t, rec, &got)
	if got.OwnerTeam != "gd-team-prod" {
		t.Fatalf("expected owner_team visible to owner team, got %+v", got)
	}
	if string(got.ScrubPolicy) == "" || string(got.ScrubPolicy) == "null" {
		t.Fatalf("expected scrub_policy visible to owner team, got %+v", got)
	}
}

func TestHandleGetTaint_AdminGetsFullDetail(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	putTaint(ms, &model.Taint{
		ID: 1, Name: "customer-pii", Description: "customer PII",
		OwnerTeam: "gd-team-prod", ScrubPolicy: json.RawMessage(`{"default":"human"}`),
	})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints/customer-pii", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got taintDetail
	decodeJSON(t, rec, &got)
	if got.OwnerTeam != "gd-team-prod" {
		t.Fatalf("expected owner_team visible to admin, got %+v", got)
	}
	if string(got.ScrubPolicy) == "" || string(got.ScrubPolicy) == "null" {
		t.Fatalf("expected scrub_policy visible to admin, got %+v", got)
	}
}

func TestHandleGetTaint_NotFound(t *testing.T) {
	_, _, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints/does-not-exist", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusNotFound)
}

// --- GET /v1/taints registry stats (task gd-QXSHroFXwB) ---

// TestHandleListTaints_BeadCountDashWithoutGrant pins the ADR §9.1 acceptance
// criteria: a viewer without a grant for a given taint sees its row with a
// dash (nil) bead count, not an error or an omitted row, even though beads
// carrying that taint exist.
func TestHandleListTaints_BeadCountDashWithoutGrant(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"outsider@example.com": {RoleViewer}})
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod"})
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t1", Fleet: "gardenio"}
	ms.taints = map[string][]int64{"gd-bead1": {1}}

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "outsider@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 1 {
		t.Fatalf("expected 1 taint row, got %d", len(resp.Taints))
	}
	row := resp.Taints[0]
	if _, present := row["bead_count"]; !present {
		t.Fatalf("expected bead_count key to be present (as null), got %v", row)
	}
	if row["bead_count"] != nil {
		t.Fatalf("expected bead_count dash (nil) for a caller with no grant, got %v", row)
	}
}

// TestHandleListTaints_BeadCountVisibleWithGrant pins that a caller holding a
// grant for the taint sees the live count of beads carrying it.
func TestHandleListTaints_BeadCountVisibleWithGrant(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"grantee@example.com": {RoleViewer}})
	ms.operatorAliases = map[string]int64{"oidc\x00grantee@example.com": 7}
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod"})
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t1", Fleet: "gardenio"}
	ms.beads["gd-bead2"] = &model.Bead{ID: "gd-bead2", Kind: model.KindIssue, Type: model.TypeTask, Title: "t2", Fleet: "gardenio"}
	ms.taints = map[string][]int64{"gd-bead1": {1}, "gd-bead2": {1}}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 7}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "grantee@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 1 {
		t.Fatalf("expected 1 taint row, got %d", len(resp.Taints))
	}
	got, ok := resp.Taints[0]["bead_count"].(float64)
	if !ok {
		t.Fatalf("expected numeric bead_count, got %v", resp.Taints[0]["bead_count"])
	}
	if got != 2 {
		t.Fatalf("expected bead_count 2, got %v", got)
	}
}

// TestHandleListTaints_OpenScrubCountAndLastScrub pins the other two live
// registry figures ADR §9.1 names: open scrub requests, and last scrub
// (the most recent approved scrub-request's completion time).
func TestHandleListTaints_OpenScrubCountAndLastScrub(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod"})

	ms.beads["gd-open1"] = &model.Bead{
		ID: "gd-open1", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "open scrub",
		Fleet: "gardenio", Status: model.StatusOpen,
		Fields: []byte(`{"taint":"customer-pii","kind":"detaint"}`),
	}
	ms.beads["gd-approved1"] = &model.Bead{
		ID: "gd-approved1", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "approved scrub",
		Fleet: "gardenio", Status: model.StatusClosed,
		Fields: []byte(`{"taint":"customer-pii","kind":"detaint","approval_status":"approved","executed_at":"2026-09-20T12:00:00Z"}`),
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 1 {
		t.Fatalf("expected 1 taint row, got %d", len(resp.Taints))
	}
	row := resp.Taints[0]
	if open, ok := row["open_scrub_requests"].(float64); !ok || open != 1 {
		t.Fatalf("expected open_scrub_requests 1, got %v", row["open_scrub_requests"])
	}
	lastScrub, ok := row["last_scrub_at"].(string)
	if !ok || lastScrub == "" {
		t.Fatalf("expected non-empty last_scrub_at, got %v", row["last_scrub_at"])
	}
	if lastScrub != "2026-09-20T12:00:00Z" {
		t.Fatalf("expected last_scrub_at 2026-09-20T12:00:00Z, got %v", lastScrub)
	}
}

// TestHandleListTaints_NoScrubsOmitsLastScrub pins that a taint with no
// approved scrub request reports a nil last_scrub_at, not an error.
func TestHandleListTaints_NoScrubsOmitsLastScrub(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod"})

	rec := taintDoJSONAs(t, h, "GET", "/v1/taints", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Taints []map[string]any `json:"taints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Taints) != 1 {
		t.Fatalf("expected 1 taint row, got %d", len(resp.Taints))
	}
	if v, present := resp.Taints[0]["last_scrub_at"]; present && v != nil {
		t.Fatalf("expected no last_scrub_at, got %v", v)
	}
	if open, ok := resp.Taints[0]["open_scrub_requests"].(float64); !ok || open != 0 {
		t.Fatalf("expected open_scrub_requests 0, got %v", resp.Taints[0]["open_scrub_requests"])
	}
}
