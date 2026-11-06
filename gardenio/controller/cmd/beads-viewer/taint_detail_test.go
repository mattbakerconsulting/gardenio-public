package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func newTestServerForTaintDetail(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "http://test", "", nil)
}

// TestHandleTaintDetail_PolicyTab_Renders covers the default (Policy) tab for
// an unrestricted taint, including the raw-JSON disclosure and the auto-
// needs-a-cap hint text (task gd-nOnc1Q8Zcv acceptance criteria).
func TestHandleTaintDetail_PolicyTab_Renders(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{
		Name:        "acme-production",
		Description: "Customer PII from prod.",
		OwnerTeam:   "data-platform",
		Effects:     []string{"no-egress", "no-third-party-llm"},
		ScrubPolicy: []byte(`{"default":"human","rules":[{"when":{"kind":"export"},"mode":"human-four-eyes","approvers":["team:data-platform"]}]}`),
		UpdatedAt:   time.Now().Add(-time.Hour),
	}
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"acme-production",
		"data-platform",
		"Customer PII from prod.",
		"no-egress",
		"human-four-eyes",
		"export",
		"An &#34;auto&#34; rule requires",
	} {
		if !strings.Contains(body, want) {
			// html/template escapes quotes; try the literal too in case escaping differs.
			if want == `An &#34;auto&#34; rule requires` && strings.Contains(body, `auto" rule requires`) {
				continue
			}
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestHandleTaintDetail_Restricted verifies a caller with no grant/membership
// sees the owner team hidden and no management form, but no daemon error.
func TestHandleTaintDetail_Restricted(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{
		Name:       "acme-production",
		Restricted: true,
	}
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Owner team hidden") {
		t.Errorf("expected restricted notice, got:\n%s", body)
	}
	if strings.Contains(body, "Save policy") {
		t.Errorf("restricted caller must not see the policy save button:\n%s", body)
	}
}

func TestHandleTaintDetail_NotFound(t *testing.T) {
	daemon := newFake()
	daemon.getTaintErr = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such taint"}
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/does-not-exist", nil)
	req.SetPathValue("name", "does-not-exist")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=\n%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTaintDetail_GrantsTab(t *testing.T) {
	expires := time.Now().Add(24 * time.Hour)
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.taintGrantsResp = []beadsapi.TaintGrantEntry{
		{PrincipalKind: "team", Principal: "growth", GrantedBy: 7, ExpiresAt: &expires},
		{PrincipalKind: "agent", Principal: "gd-agent1", GrantedBy: 7},
	}
	daemon.beads = map[string]*beadsapi.BeadDetail{
		"gd-agent1": {ID: "gd-agent1", Title: "some-agent", OperatedByDisplay: "Root Human"},
	}

	srv := newTestServerForTaintDetail(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production?tab=grants", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "team:growth") {
		t.Errorf("expected team:growth grant row, got:\n%s", body)
	}
	if !strings.Contains(body, "grant-row-agent") {
		t.Errorf("expected agent grant row to carry grant-row-agent (greyed) class, got:\n%s", body)
	}
}

// TestScrubPolicyFromForm_AutoRuleRequiresCap is the primary acceptance-
// criteria test: "Policy tab save rejects an auto rule with no
// max_beads/max_per_day, matching p1-taint-store's server-side validation".
func TestScrubPolicyFromForm_AutoRuleRequiresCap(t *testing.T) {
	form := url.Values{
		"rule_kind":        {"export"},
		"rule_mode":        {"auto"},
		"rule_approvers":   {""},
		"rule_max_beads":   {""},
		"rule_max_per_day": {""},
	}
	policy, errMsg := scrubPolicyFromForm(form)
	if policy != nil {
		t.Fatalf("expected nil policy on validation failure, got %+v", policy)
	}
	if !strings.Contains(errMsg, `an "auto" rule requires max_beads or max_per_day`) {
		t.Errorf("unexpected error message: %q", errMsg)
	}
}

func TestScrubPolicyFromForm_AutoRuleWithCap_OK(t *testing.T) {
	form := url.Values{
		"rule_kind":      {"export"},
		"rule_mode":      {"auto"},
		"rule_max_beads": {"5"},
	}
	policy, errMsg := scrubPolicyFromForm(form)
	if errMsg != "" {
		t.Fatalf("unexpected error: %q", errMsg)
	}
	if len(policy.Rules) != 1 || policy.Rules[0].MaxBeads == nil || *policy.Rules[0].MaxBeads != 5 {
		t.Fatalf("unexpected rules: %+v", policy.Rules)
	}
}

func TestScrubPolicyFromForm_DefaultCannotBeAuto(t *testing.T) {
	form := url.Values{"default": {"auto"}}
	policy, errMsg := scrubPolicyFromForm(form)
	if policy != nil {
		t.Fatalf("expected nil policy, got %+v", policy)
	}
	if !strings.Contains(errMsg, `default may not be "auto"`) {
		t.Errorf("unexpected error message: %q", errMsg)
	}
}

// TestHandleTaintPolicyUpdate_AutoRuleWithoutCap_Rejected verifies the
// end-to-end handler path re-renders the Policy tab with an inline error and
// does not call UpdateTaintPolicy at all.
func TestHandleTaintPolicyUpdate_AutoRuleWithoutCap_Rejected(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("rule_kind=export&rule_mode=auto")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/policy", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintPolicyUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (full-page re-render with inline error); body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateTaintPolicyReq != nil {
		t.Error("UpdateTaintPolicy must not be called when the form fails client-side validation")
	}
	if !strings.Contains(rec.Body.String(), "requires max_beads or max_per_day") {
		t.Errorf("expected inline validation error, got:\n%s", rec.Body.String())
	}
}

func TestHandleTaintPolicyUpdate_Success(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("default=human&rule_kind=export&rule_mode=human&rule_approvers=team:data-platform")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/policy", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintPolicyUpdate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateTaintPolicyReq == nil {
		t.Fatal("expected UpdateTaintPolicy to be called")
	}
	if !strings.Contains(string(daemon.updateTaintPolicyReq), `"mode":"human"`) {
		t.Errorf("unexpected policy payload: %s", daemon.updateTaintPolicyReq)
	}
}

func TestHandleTaintPolicyUpdate_ServerRejectsAnyway(t *testing.T) {
	// Even if client validation somehow passed something the daemon refuses
	// (p1-taint-store is authoritative), the handler must surface the
	// daemon's error rather than pretend success.
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.updateTaintPolicyErr = &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: `auto rule requires a cap`}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("default=human&rule_kind=export&rule_mode=human&rule_max_beads=5")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/policy", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintPolicyUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to save policy") {
		t.Errorf("expected daemon error surfaced, got:\n%s", rec.Body.String())
	}
}

func TestHandleTaintGrantCreate_Success(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("principal_kind=team&principal_value=growth")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/grants", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintGrantCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.grantTaintReq == nil || daemon.grantTaintReq.Principal != "team:growth" {
		t.Fatalf("unexpected grant request: %+v", daemon.grantTaintReq)
	}
}

func TestHandleTaintGrantCreate_MissingFields(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("principal_kind=team&principal_value=")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/grants", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintGrantCreate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.grantTaintReq != nil {
		t.Error("GrantTaint must not be called with a missing principal value")
	}
}

func TestHandleTaintGrantRevoke_Success(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("principal=team:growth")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/grants/revoke", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintGrantRevoke(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.revokeTaintPrincipal != "team:growth" {
		t.Errorf("revokeTaintPrincipal = %q, want %q", daemon.revokeTaintPrincipal, "team:growth")
	}
}

// TestHandleTaintDetail_BeadsTab_NeverListsUnreadableBead relies on the fact
// that ListTaintBeads is the sole read gate (scopeListFilter) — the view
// builder must not add any further client-side filtering, so every row the
// daemon returns is rendered as-is (acceptance criteria: "never lists a bead
// the viewer cannot read").
func TestHandleTaintDetail_BeadsTab_Renders(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.taintBeadsResp = []beadsapi.TaintBeadEntry{
		{
			BeadDetail:   &beadsapi.BeadDetail{ID: "gd-abc123", Title: "Customer export", Status: "open", UpdatedAt: time.Now()},
			Source:       "explicit",
			SourceBeadID: "gd-parent1",
		},
	}
	daemon.taintBeadsTotal = 1
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production?tab=beads", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "gd-abc123") || !strings.Contains(body, "Customer export") {
		t.Errorf("expected tainted bead row, got:\n%s", body)
	}
	if !strings.Contains(body, "gd-parent1") {
		t.Errorf("expected source-bead link, got:\n%s", body)
	}
}

// TestHandleTaintDetail_ScrubsTab_ApproveRejectVisibility verifies the
// Approve/Reject forms render only when CanApprove/CanReject are true
// (acceptance criteria: "visible only to callers in the relevant rule's
// approvers").
func TestHandleTaintDetail_ScrubsTab_ApproveRejectVisibility(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.taintScrubRequestsResp = []beadsapi.TaintScrubRequestEntry{
		{
			BeadDetail: &beadsapi.BeadDetail{
				ID:        "gd-scrub1",
				Fields:    map[string]string{"kind": "delete", "scope": `{"bead_ids":["gd-a","gd-b"]}`},
				Status:    "open",
				CreatedAt: time.Now(),
			},
			CanApprove: true,
			CanReject:  false,
		},
		{
			BeadDetail: &beadsapi.BeadDetail{
				ID:        "gd-scrub2",
				Fields:    map[string]string{"kind": "delete"},
				Status:    "open",
				CreatedAt: time.Now(),
			},
			CanApprove: false,
			CanReject:  false,
		},
	}
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production?tab=scrubs", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "/taints/acme-production/scrub-requests/gd-scrub1/approve") {
		t.Errorf("expected an approve form for the CanApprove row, got:\n%s", body)
	}
	if strings.Contains(body, "/taints/acme-production/scrub-requests/gd-scrub1/reject") {
		t.Errorf("did not expect a reject form for a CanReject=false row, got:\n%s", body)
	}
	if strings.Contains(body, "/taints/acme-production/scrub-requests/gd-scrub2/approve") ||
		strings.Contains(body, "/taints/acme-production/scrub-requests/gd-scrub2/reject") {
		t.Errorf("did not expect any approve/reject form for a row with neither flag set, got:\n%s", body)
	}
	if !strings.Contains(body, "2 beads") {
		t.Errorf("expected scope-size display '2 beads', got:\n%s", body)
	}
}

func TestHandleTaintScrubApprove_Success(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("reason=looks+fine")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/scrub-requests/gd-scrub1/approve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("id", "gd-scrub1")
	rec := httptest.NewRecorder()
	srv.handleTaintScrubApprove(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
}

// TestHandleTaintScrubApprove_NoForwardableBearer covers the forced-OIDC
// design decision: a relay-only caller (no forwardable user bearer) must
// fail closed with a friendly message, never a raw daemon error and never a
// silent success.
func TestHandleTaintScrubApprove_NoForwardableBearer(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.approveScrubRequestErr = beadsapi.ErrScrubApprovalRequiresUserBearer
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("reason=")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/scrub-requests/gd-scrub1/approve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("id", "gd-scrub1")
	rec := httptest.NewRecorder()
	srv.handleTaintScrubApprove(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (full-page re-render with inline error); body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires signing in directly") {
		t.Errorf("expected the forwardable-token guidance message, got:\n%s", rec.Body.String())
	}
}

func TestHandleTaintScrubReject_GenericError(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.rejectScrubRequestErr = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not an approver"}
	srv := newTestServerForTaintDetail(t, daemon)

	form := strings.NewReader("reason=")
	req := httptest.NewRequest(http.MethodPost, "/taints/acme-production/scrub-requests/gd-scrub1/reject", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("id", "gd-scrub1")
	rec := httptest.NewRecorder()
	srv.handleTaintScrubReject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to reject") {
		t.Errorf("expected daemon error surfaced, got:\n%s", rec.Body.String())
	}
}

// TestHandleTaintDetail_AuditTab_NeverRendersFullHash verifies the acceptance
// criteria "hashes truncated, no content ever rendered" — the full
// ContentSHA must never appear verbatim in the rendered page.
func TestHandleTaintDetail_AuditTab_NeverRendersFullHash(t *testing.T) {
	fullHash := "abcdef0123456789abcdef0123456789abcdef01"
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", OwnerTeam: "data-platform"}
	daemon.taintAuditResp = []beadsapi.TaintAuditEntry{
		{
			ID:         1,
			Action:     "grant",
			Actor:      "oidc:someone@example.com",
			BeadID:     "gd-abc123",
			ContentSHA: fullHash,
			CreatedAt:  time.Now(),
		},
	}
	daemon.taintAuditTotal = 1
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production?tab=audit", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, fullHash) {
		t.Errorf("full content hash must never be rendered verbatim, got:\n%s", body)
	}
	if !strings.Contains(body, truncateHash(fullHash)) {
		t.Errorf("expected truncated hash %q in body, got:\n%s", truncateHash(fullHash), body)
	}
	if !strings.Contains(body, "oidc:someone@example.com") {
		t.Errorf("expected actor in audit row, got:\n%s", body)
	}
}

func TestHandleTaintDetail_AuditTab_Restricted(t *testing.T) {
	daemon := newFake()
	daemon.getTaintResp = &beadsapi.TaintDetail{Name: "acme-production", Restricted: true}
	srv := newTestServerForTaintDetail(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/acme-production?tab=audit", nil)
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	srv.handleTaintDetail(rec, req)

	if daemon.taintAuditResp != nil && strings.Contains(rec.Body.String(), "audit row(s)") {
		t.Errorf("restricted caller must not see audit rows, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "contact") {
		t.Errorf("expected a restricted-access message, got:\n%s", rec.Body.String())
	}
}

func TestScrubScopeSizeDisplay(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "—"},
		{`{"bead_ids":["a"]}`, "1 bead"},
		{`{"bead_ids":["a","b","c"]}`, "3 beads"},
		{`{"predicate":{"kind":"export"}}`, "predicate"},
		{`not json`, "predicate"},
	}
	for _, tc := range cases {
		if got := scrubScopeSizeDisplay(tc.in); got != tc.want {
			t.Errorf("scrubScopeSizeDisplay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncateHash(t *testing.T) {
	if got := truncateHash(""); got != "—" {
		t.Errorf("truncateHash(\"\") = %q, want %q", got, "—")
	}
	short := "abc123"
	if got := truncateHash(short); got != short {
		t.Errorf("truncateHash(%q) = %q, want unchanged", short, got)
	}
	long := "0123456789abcdef0123456789"
	got := truncateHash(long)
	if len(got) >= len(long) {
		t.Errorf("truncateHash(%q) = %q, want a truncated value", long, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncateHash(%q) = %q, want an ellipsis suffix", long, got)
	}
}
