package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Writes made while impersonating (impersonation.go, gd-L8LeYDLACc).

const (
	impTenant    = "imp-tenant-as-owner-tok" // ada@example.com, impersonated by owner
	impTenantB   = "imp-tenant-as-owner-tok-2nd-session"
	impTenantOwn = "imp-tenant-own-tok" // ada@example.com, her own session
	impStatic    = "imp-static-tok"
)

// impersonatingVerifier is the test issuer plus the impersonator and sid
// claims, keyed by token.
type impersonatingVerifier struct {
	*multiIssuerMock
	impersonation map[string][2]string // token -> {impersonator, sid}
}

func (v *impersonatingVerifier) ExtractImpersonation(ctx context.Context, rawToken string) (string, string, error) {
	if err := v.Verify(ctx, rawToken); err != nil {
		return "", "", err
	}
	imp := v.impersonation[rawToken]
	return imp[0], imp[1], nil
}

type impersonationFixture struct {
	srv *BeadsServer
	ms  *mockStore
	ts  *httptest.Server
}

func newImpersonationFixture(t *testing.T) *impersonationFixture {
	t.Helper()
	ada := tokenInfo{email: "ada@example.com", roles: []string{RoleViewer, RoleOperator}, azp: testBrowserClient}
	verifier := &impersonatingVerifier{
		multiIssuerMock: &multiIssuerMock{issuers: []*oidcIssuerMock{{tokens: map[string]tokenInfo{
			impTenant: ada, impTenantB: ada, impTenantOwn: ada,
		}}}},
		impersonation: map[string][2]string{
			impTenant:  {"owner", "kc-session-1"},
			impTenantB: {"owner", "kc-session-2"},
		},
	}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("gardenio", nil)
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	ts := httptest.NewServer(srv.NewHTTPHandler(verifier, withTestToken(fc, impStatic)))
	t.Cleanup(ts.Close)
	return &impersonationFixture{srv: srv, ms: ms, ts: ts}
}

func (f *impersonationFixture) do(t *testing.T, token, method, path string, body any) (int, string) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-No-Auto-Parent", "1")
	req.Header.Set("User-Agent", "imp-test-browser")
	if token == impStatic {
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// create posts a task and returns its id.
func (f *impersonationFixture) create(t *testing.T, token string, fields map[string]any, want int) string {
	t.Helper()
	body := map[string]any{"title": "imp task", "type": "task", "labels": []string{"project:gardenio"}}
	if fields != nil {
		body["fields"] = fields
	}
	code, out := f.do(t, token, http.MethodPost, "/v1/beads", body)
	if code != want {
		t.Fatalf("create as %s with %v: %d %s, want %d", token, fields, code, out, want)
	}
	if code != http.StatusCreated {
		return ""
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil || b.ID == "" {
		t.Fatalf("create response %q: %v", out, err)
	}
	return b.ID
}

func (f *impersonationFixture) stamp(t *testing.T, id string) (string, bool) {
	t.Helper()
	b, err := f.ms.GetBead(context.Background(), id)
	if err != nil || b == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	v, ok := fieldsAsMap(b.Fields)[impersonatedByField]
	s, _ := v.(string)
	return s, ok
}

func TestParseImpersonatorClaim(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{``, ""},
		{`null`, ""},
		{`""`, ""},
		{`"  "`, ""},
		{`"owner"`, "owner"},
		{`" owner "`, "owner"},
		{`42`, impersonatorUnreadable},
		{`["owner"]`, impersonatorUnreadable},
		{`"own\ner"`, impersonatorUnreadable},
		{`"` + strings.Repeat("a", impersonatorMaxBytes+1) + `"`, impersonatorUnreadable},
	} {
		if got := parseImpersonatorClaim(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("parseImpersonatorClaim(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// A create or update under the claim is stamped; created_by stays the tenant.
// Without the claim nothing is stamped.
func TestImpersonation_StampsCreateAndUpdate(t *testing.T) {
	f := newImpersonationFixture(t)

	id := f.create(t, impTenant, nil, http.StatusCreated)
	if got, _ := f.stamp(t, id); got != "owner" {
		t.Errorf("create under impersonation: impersonated_by = %q, want owner", got)
	}
	if b, _ := f.ms.GetBead(context.Background(), id); b.CreatedBy != "ada@example.com" {
		t.Errorf("created_by = %q, want the tenant", b.CreatedBy)
	}

	own := f.create(t, impTenantOwn, nil, http.StatusCreated)
	if got, ok := f.stamp(t, own); ok {
		t.Errorf("create without the claim: impersonated_by = %q, want absent", got)
	}

	// The tenant's own session leaves the stamp alone on an unrelated update.
	if code, out := f.do(t, impTenantOwn, http.MethodPatch, "/v1/beads/"+own, map[string]any{"description": "mine"}); code != http.StatusOK {
		t.Fatalf("own update: %d %s", code, out)
	}
	if got, ok := f.stamp(t, own); ok {
		t.Errorf("update without the claim: impersonated_by = %q, want absent", got)
	}
	// An impersonated update stamps it.
	if code, out := f.do(t, impTenant, http.MethodPatch, "/v1/beads/"+own, map[string]any{"description": "as ada"}); code != http.StatusOK {
		t.Fatalf("impersonated update: %d %s", code, out)
	}
	if got, _ := f.stamp(t, own); got != "owner" {
		t.Errorf("update under impersonation: impersonated_by = %q, want owner", got)
	}
	// A later update by the tenant herself, echoing the fields back, is fine.
	b, _ := f.ms.GetBead(context.Background(), own)
	if code, out := f.do(t, impTenantOwn, http.MethodPatch, "/v1/beads/"+own, map[string]any{"fields": json.RawMessage(b.Fields)}); code != http.StatusOK {
		t.Errorf("echoing the stored stamp: %d %s, want 200", code, out)
	}
}

// impersonated_by is the daemon's: a client may not set it, in any letter
// case, on any write path.
func TestImpersonation_RefusesClientSupplied(t *testing.T) {
	f := newImpersonationFixture(t)

	for _, c := range []struct {
		name, token string
		fields      map[string]any
		want        int
	}{
		{"own session names an admin", impTenantOwn, map[string]any{"impersonated_by": "owner"}, http.StatusForbidden},
		// impersonated_by is a universal field, so on a typed bead field
		// validation refuses another letter case first (400).
		{"own session, other case", impTenantOwn, map[string]any{"Impersonated_By": "owner"}, http.StatusBadRequest},
		{"static token names an admin", impStatic, map[string]any{"impersonated_by": "owner"}, http.StatusForbidden},
		{"impersonated session names another admin", impTenant, map[string]any{"impersonated_by": "mallory"}, http.StatusForbidden},
		{"impersonated session clears it", impTenant, map[string]any{"impersonated_by": ""}, http.StatusForbidden},
		// What the daemon would stamp anyway.
		{"impersonated session names its own admin", impTenant, map[string]any{"impersonated_by": "owner"}, http.StatusCreated},
	} {
		t.Run(c.name, func(t *testing.T) { f.create(t, c.token, c.fields, c.want) })
	}

	stamped := f.create(t, impTenant, nil, http.StatusCreated)
	for _, c := range []struct {
		name, token string
		fields      map[string]any
	}{
		{"own session rewrites it", impTenantOwn, map[string]any{"impersonated_by": "someone-else"}},
		{"own session clears it", impTenantOwn, map[string]any{"impersonated_by": nil}},
		{"static token rewrites it", impStatic, map[string]any{"impersonated_by": "someone-else"}},
	} {
		if code, out := f.do(t, c.token, http.MethodPatch, "/v1/beads/"+stamped, map[string]any{"fields": c.fields}); code != http.StatusForbidden {
			t.Errorf("update, %s: %d %s, want 403", c.name, code, out)
		}
	}
	if got, _ := f.stamp(t, stamped); got != "owner" {
		t.Errorf("after refused writes impersonated_by = %q, want owner", got)
	}

	// The field endpoints and the close body name keys, not values.
	if code, out := f.do(t, impStatic, http.MethodPost, "/v1/beads/"+stamped+"/increment", map[string]any{"impersonated_by": 1}); code != http.StatusForbidden {
		t.Errorf("increment impersonated_by: %d %s, want 403", code, out)
	}
	if code, out := f.do(t, impTenantOwn, http.MethodPost, "/v1/beads/"+stamped+"/close", map[string]any{"impersonated_by": "x"}); code != http.StatusForbidden {
		t.Errorf("close carrying impersonated_by: %d %s, want 403", code, out)
	}

	// A comment body naming it is refused too.
	if code, out := f.do(t, impTenantOwn, http.MethodPost, "/v1/beads/"+stamped+"/comments", map[string]any{"text": "hi", "impersonated_by": "owner"}); code != http.StatusForbidden {
		t.Errorf("comment carrying impersonated_by: %d %s, want 403", code, out)
	}
}

// Comments and closes under the claim carry the admin; others do not.
func TestImpersonation_StampsCommentAndClose(t *testing.T) {
	f := newImpersonationFixture(t)
	id := f.create(t, impTenantOwn, nil, http.StatusCreated)

	for _, c := range []struct {
		token, want string
	}{{impTenant, "owner"}, {impTenantOwn, ""}} {
		code, out := f.do(t, c.token, http.MethodPost, "/v1/beads/"+id+"/comments", map[string]any{"text": "hello from " + c.token})
		if code != http.StatusCreated {
			t.Fatalf("comment as %s: %d %s", c.token, code, out)
		}
		var cm model.Comment
		if err := json.Unmarshal([]byte(out), &cm); err != nil {
			t.Fatal(err)
		}
		if cm.ImpersonatedBy != c.want || cm.Author != "ada@example.com" {
			t.Errorf("comment as %s: impersonated_by=%q author=%q, want %q and the tenant", c.token, cm.ImpersonatedBy, cm.Author, c.want)
		}
	}
	comments, _ := f.ms.GetComments(context.Background(), id)
	if len(comments) != 2 || comments[0].ImpersonatedBy != "owner" || comments[1].ImpersonatedBy != "" {
		t.Errorf("stored comments = %+v", comments)
	}

	if got, ok := f.stamp(t, id); ok {
		t.Fatalf("before close impersonated_by = %q, want absent", got)
	}
	if code, out := f.do(t, impTenant, http.MethodPost, "/v1/beads/"+id+"/close", map[string]any{}); code != http.StatusOK {
		t.Fatalf("close: %d %s", code, out)
	}
	if got, _ := f.stamp(t, id); got != "owner" {
		t.Errorf("close under impersonation: impersonated_by = %q, want owner", got)
	}
}

// One audit bead per impersonated session, written on its first request (a
// read here), on the tenant's user bead. The tenant's own session writes none.
func TestImpersonation_OneAuditBeadPerSession(t *testing.T) {
	f := newImpersonationFixture(t)
	audits := func() []*model.Bead {
		t.Helper()
		bs, _, err := f.ms.ListBeads(context.Background(), model.BeadFilter{Type: []model.BeadType{"audit"}, Labels: []string{impersonationAuditLabel}})
		if err != nil {
			t.Fatal(err)
		}
		return bs
	}

	if code, out := f.do(t, impTenantOwn, http.MethodGet, "/v1/beads", nil); code != http.StatusOK {
		t.Fatalf("own read: %d %s", code, out)
	}
	if n := len(audits()); n != 0 {
		t.Fatalf("own session wrote %d impersonation audits, want 0", n)
	}

	for i := 0; i < 3; i++ {
		if code, out := f.do(t, impTenant, http.MethodGet, "/v1/beads", nil); code != http.StatusOK {
			t.Fatalf("impersonated read %d: %d %s", i, code, out)
		}
	}
	f.create(t, impTenant, nil, http.StatusCreated)
	got := audits()
	if len(got) != 1 {
		t.Fatalf("one session wrote %d audits, want 1", len(got))
	}
	a := fieldsAsMap(got[0].Fields)
	user, err := userBeadByEmail(context.Background(), f.ms, "ada@example.com")
	if err != nil || user == nil {
		t.Fatalf("tenant user bead: %v", err)
	}
	for k, want := range map[string]string{
		"action": impersonationAuditAction, "outcome": "allow", "actor": "owner",
		impersonatedByField: "owner", "subject": "ada@example.com", "session_id": "kc-session-1",
		"client_id": testBrowserClient, "resource": user.ID, "resource_type": "user",
		"user_agent": "imp-test-browser",
	} {
		if v, _ := a[k].(string); v != want {
			t.Errorf("audit %s = %q, want %q", k, v, want)
		}
	}
	if s, _ := a["source_ip"].(string); s == "" {
		t.Error("audit source_ip is empty")
	}

	// A replica that has not seen the session (a restart) does not write a
	// second bead: the idempotency key holds.
	f.srv.impersonationAudited = impersonationSessionSet{}
	if code, out := f.do(t, impTenant, http.MethodGet, "/v1/beads", nil); code != http.StatusOK {
		t.Fatalf("read after restart: %d %s", code, out)
	}
	if n := len(audits()); n != 1 {
		t.Errorf("after a restart the session has %d audits, want 1", n)
	}

	// A second impersonated session gets its own.
	if code, out := f.do(t, impTenantB, http.MethodGet, "/v1/beads", nil); code != http.StatusOK {
		t.Fatalf("second session read: %d %s", code, out)
	}
	if n := len(audits()); n != 2 {
		t.Errorf("two sessions wrote %d audits, want 2", n)
	}
}

func TestImpersonationSessionSet_Bounded(t *testing.T) {
	var s impersonationSessionSet
	for i := 0; i < impersonationSessionCap+10; i++ {
		if !s.claim(fmt.Sprint(i)) {
			t.Fatalf("claim %d: already seen", i)
		}
	}
	if len(s.seen) > impersonationSessionCap {
		t.Errorf("set holds %d, cap %d", len(s.seen), impersonationSessionCap)
	}
	if s.claim(fmt.Sprint(impersonationSessionCap + 9)) {
		t.Error("a just-claimed key was claimed again")
	}
	s.release("x")
	if !s.claim("x") || s.claim("x") {
		t.Error("claim after release")
	}
}

func TestImpersonationSessionKey(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if got := impersonationSessionKey(AuthIdentity{ImpersonatedBy: "owner", Subject: "ada@example.com", SessionID: "s1"}, now); got != "impersonation-session:s1" {
		t.Errorf("with sid: %q", got)
	}
	if got := impersonationSessionKey(AuthIdentity{ImpersonatedBy: "owner", Subject: "ada@example.com"}, now); got != "impersonation-session:owner:ada@example.com:2026-10-31" {
		t.Errorf("without sid: %q", got)
	}
}

// The escalation result's fields and every field endpoint name keys, not
// values: none may write impersonated_by or created_impersonated_by, for a
// static, fleet or impersonated caller (#1685 send-back).
func TestImpersonation_RefusesFieldEndpointsAndResult(t *testing.T) {
	f := newImpersonationFixture(t)
	stamped := f.create(t, impTenant, nil, http.StatusCreated)

	for _, field := range []string{impersonatedByField, createdImpersonatedByField} {
		for _, c := range []struct {
			name, path string
			body       any
		}{
			{"append", "/fields/" + field + "/append", map[string]any{"element": "mallory"}},
			{"reorder", "/fields/" + field + "/reorder", map[string]any{"expected": []string{}, "next": []string{"mallory"}}},
			{"edit set", "/fields/" + field + "/edit", map[string]any{"op": "set", "key": "k", "value": "mallory"}},
			{"edit append", "/fields/" + field + "/edit", map[string]any{"op": "append", "element": "mallory"}},
		} {
			for _, token := range []string{impStatic, impTenant} {
				if code, out := f.do(t, token, http.MethodPost, "/v1/beads/"+stamped+c.path, c.body); code != http.StatusForbidden {
					t.Errorf("%s %s as %s: %d %s, want 403", c.name, field, token, code, out)
				}
			}
		}
	}
	if got, _ := f.stamp(t, stamped); got != "owner" {
		t.Errorf("after refused field writes impersonated_by = %q, want owner", got)
	}

	for _, key := range []string{impersonatedByField, "Impersonated_By", createdImpersonatedByField} {
		esc := &model.Bead{ID: "gd-imp-esc-" + key, Type: "escalation", Kind: model.KindData, Status: model.StatusOpen,
			Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"operation_type":"aws_iam"}`)}
		f.ms.beads[esc.ID] = esc
		code, out := f.do(t, impStatic, http.MethodPost, "/v1/escalations/"+esc.ID+"/result", map[string]any{
			"result_status": "success", "fields": map[string]string{key: "owner"},
		})
		if code != http.StatusForbidden {
			t.Errorf("result carrying %s: %d %s, want 403", key, code, out)
		}
		b, _ := f.ms.GetBead(context.Background(), esc.ID)
		if b.Status != model.StatusOpen {
			t.Errorf("result carrying %s closed the escalation", key)
		}
		for k := range fieldsAsMap(b.Fields) {
			if isImpersonationField(k) {
				t.Errorf("result carrying %s wrote %s", key, k)
			}
		}
	}
}

// mergeBeadFields refuses an impersonation key for every caller but
// stampImpersonatedByOn's own merge, so a future extra-field merge cannot
// become a bypass.
func TestMergeBeadFields_RefusesImpersonationKeys(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-imp-m"] = &model.Bead{ID: "gd-imp-m", Type: "task", Status: model.StatusOpen, Fields: json.RawMessage(`{}`)}
	for _, key := range []string{impersonatedByField, "IMPERSONATED_BY", createdImpersonatedByField} {
		err := srv.mergeBeadFields(context.Background(), "gd-imp-m", map[string]any{key: "owner"})
		var denial *AuthzDenial
		if !errors.As(err, &denial) {
			t.Errorf("merge %s: err = %v, want an AuthzDenial", key, err)
		}
	}
	imp := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "ada@example.com", ImpersonatedBy: "owner"}), authMethodOIDC)
	if err := srv.stampImpersonatedByOn(imp, "gd-imp-m"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if v := fieldsAsMap(ms.beads["gd-imp-m"].Fields)[impersonatedByField]; v != "owner" {
		t.Errorf("after stamp impersonated_by = %v, want owner", v)
	}
}

// created_impersonated_by says who wrote the bead: a create under impersonation
// sets it, and no later write does, so an impersonated close of a bead its
// author wrote themselves leaves it absent (#1685 send-back).
func TestImpersonation_CreatedStampOnlyOnCreate(t *testing.T) {
	f := newImpersonationFixture(t)
	created := func(id string) (string, bool) {
		b, _ := f.ms.GetBead(context.Background(), id)
		v, ok := fieldsAsMap(b.Fields)[createdImpersonatedByField]
		s, _ := v.(string)
		return s, ok
	}

	imp := f.create(t, impTenant, nil, http.StatusCreated)
	if got, _ := created(imp); got != "owner" {
		t.Errorf("impersonated create: created_impersonated_by = %q, want owner", got)
	}

	own := f.create(t, impTenantOwn, nil, http.StatusCreated)
	if code, out := f.do(t, impTenant, http.MethodPatch, "/v1/beads/"+own, map[string]any{"description": "as ada"}); code != http.StatusOK {
		t.Fatalf("impersonated update: %d %s", code, out)
	}
	if code, out := f.do(t, impTenant, http.MethodPost, "/v1/beads/"+own+"/close", map[string]any{}); code != http.StatusOK {
		t.Fatalf("impersonated close: %d %s", code, out)
	}
	if got, _ := f.stamp(t, own); got != "owner" {
		t.Errorf("impersonated close: impersonated_by = %q, want owner", got)
	}
	if got, ok := created(own); ok {
		t.Errorf("impersonated update and close of the tenant's own bead: created_impersonated_by = %q, want absent", got)
	}

	// An update may not claim authorship, even with the session's own admin;
	// a create may name it, which the stamp writes anyway.
	other := f.create(t, impTenantOwn, nil, http.StatusCreated)
	if code, out := f.do(t, impTenant, http.MethodPatch, "/v1/beads/"+other, map[string]any{"fields": map[string]any{createdImpersonatedByField: "owner"}}); code != http.StatusForbidden {
		t.Errorf("update naming created_impersonated_by: %d %s, want 403", code, out)
	}
	f.create(t, impTenant, map[string]any{createdImpersonatedByField: "owner"}, http.StatusCreated)
	f.create(t, impTenantOwn, map[string]any{createdImpersonatedByField: "owner"}, http.StatusForbidden)

	// Echoing the stored value back on a read-modify-write is fine.
	b, _ := f.ms.GetBead(context.Background(), imp)
	if code, out := f.do(t, impTenantOwn, http.MethodPatch, "/v1/beads/"+imp, map[string]any{"fields": json.RawMessage(b.Fields)}); code != http.StatusOK {
		t.Errorf("echoing the stored created stamp: %d %s, want 200", code, out)
	}
}
