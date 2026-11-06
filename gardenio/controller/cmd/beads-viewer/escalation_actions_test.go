package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func userBead(id, email, caps string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{"email": email, "capabilities": caps},
	}
}

// escActionFake builds a fake daemon holding one open escalation whose
// can-approve says yes, plus a user directory with the given capabilities for
// approver@acme.example (read only by the gate's 404 fallback).
func escActionFake(status, caps string) *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-esc": escBead2(status),
			"gd-not": {ID: "gd-not", Type: "task", Status: "open"},
		},
		userResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{userBead("gd-u1", "approver@acme.example", caps)},
			Total: 1,
		},
		canApproveResp: map[string]beadsapi.CanApproveResult{
			"gd-esc": {Allowed: true, Method: "web_ui", As: "operator"},
		},
	}
}

func escBead2(status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-esc",
		Title:  "Merge site-deployment!1274",
		Type:   "escalation",
		Status: status,
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": realSpec,
			"context":        "needs a human",
		},
	}
}

func escPost(t *testing.T, fake *fakeDaemon, id, action, email, reason string) *httptest.ResponseRecorder {
	t.Helper()
	return escPostForm(t, fake, id, action, email, "reason="+reason)
}

// escPostForm posts the url-encoded form as written.
func escPostForm(t *testing.T, fake *fakeDaemon, id, action, email, form string) *httptest.ResponseRecorder {
	t.Helper()
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	body := strings.NewReader(form)
	req := httptest.NewRequest(http.MethodPost, "/b/"+id+"/escalation/"+action, body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, req)
	return rec
}

func TestEscalationApprove_HappyPath(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "looks+good")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fake.approveEscCalls) != 1 {
		t.Fatalf("ApproveEscalation calls = %d, want 1", len(fake.approveEscCalls))
	}
	if fake.approveEscIDs[0] != "gd-esc" {
		t.Errorf("approved id = %q", fake.approveEscIDs[0])
	}
	if fake.approveEscCalls[0].ApprovedBy != "approver@acme.example" {
		t.Errorf("ApprovedBy = %q", fake.approveEscCalls[0].ApprovedBy)
	}
	if fake.approveEscCalls[0].ApprovalMethod != "web_ui" {
		t.Errorf("ApprovalMethod = %q", fake.approveEscCalls[0].ApprovalMethod)
	}
	if body := rec.Body.String(); !strings.Contains(body, "escalation-card") {
		t.Errorf("response is not the re-rendered card: %s", body)
	}
	if len(fake.rejectEscCalls) != 0 {
		t.Error("approve must not call RejectEscalation")
	}
}

// Approve with comment (gd-yuOG6cX6VI): the garden's answer box sends comment=, which rides the approve request for the
// daemon to record; the bead card's reason= box, which has always offered a note on approve, is the comment when no
// comment= is sent. A reject keeps reason= as its reason.
func TestEscalationApprove_WithComment(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPostForm(t, fake, "gd-esc", "approve", "approver@acme.example", "comment=merge+after+the+cut&reason=ignored")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := fake.approveEscCalls[0].Comment; got != "merge after the cut" {
		t.Errorf("Comment = %q, want %q", got, "merge after the cut")
	}

	fake = escActionFake("open", `["approve:escalation"]`)
	escPostForm(t, fake, "gd-esc", "approve", "approver@acme.example", "reason=from+the+card")
	if got := fake.approveEscCalls[0].Comment; got != "from the card" {
		t.Errorf("Comment from the card's reason box = %q, want %q", got, "from the card")
	}

	fake = escActionFake("open", `["approve:escalation"]`)
	escPostForm(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if got := fake.approveEscCalls[0].Comment; got != "" {
		t.Errorf("no note: Comment = %q, want empty", got)
	}

	fake = escActionFake("open", `["approve:escalation"]`)
	rec = escPostForm(t, fake, "gd-esc", "approve", "approver@acme.example", "comment="+strings.Repeat("x", maxEscalationReasonLen+1))
	if rec.Code != http.StatusRequestEntityTooLarge || len(fake.approveEscCalls) != 0 {
		t.Errorf("too long a comment: status %d, approve calls %d", rec.Code, len(fake.approveEscCalls))
	}
}

func TestEscalationReject_RecordsReason(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-esc", "reject", "approver@acme.example", "wrong+branch")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fake.rejectEscCalls) != 1 {
		t.Fatalf("RejectEscalation calls = %d, want 1", len(fake.rejectEscCalls))
	}
	if got := fake.rejectEscCalls[0].Reason; got != "wrong branch" {
		t.Errorf("Reason = %q, want %q", got, "wrong branch")
	}
	if fake.rejectEscCalls[0].RejectedBy != "approver@acme.example" {
		t.Errorf("RejectedBy = %q", fake.rejectEscCalls[0].RejectedBy)
	}
}

func TestEscalationApprove_Unauthenticated(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-esc", "approve", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(fake.approveEscCalls) != 0 {
		t.Error("unauthenticated request reached the daemon")
	}
}

// TestEscalationApprove_DaemonSaysNo: can-approve's "no" is a 403 with its reason,
// and nothing reaches the approve endpoint. TestEscalationApprove_HappyPath is the
// positive control on the same fixture with the daemon's yes.
func TestEscalationApprove_DaemonSaysNo(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "caller is not in the approver lane that holds this escalation (coordinator:laptop)"}
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fake.approveEscCalls) != 0 {
		t.Error("a viewer the daemon refused reached ApproveEscalation")
	}
	if !strings.Contains(rec.Body.String(), "holds this escalation (coordinator:laptop)") {
		t.Errorf("403 body should carry the daemon's reason: %s", rec.Body.String())
	}
	if !slices.Contains(fake.canApproveIDs, "gd-esc") {
		t.Error("the POST did not ask can-approve")
	}
}

func TestEscalationApprove_AlreadyClosed(t *testing.T) {
	fake := escActionFake("closed", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if len(fake.approveEscCalls) != 0 {
		t.Error("closed escalation reached the daemon")
	}
	if body := rec.Body.String(); !strings.Contains(body, "already closed") {
		t.Errorf("409 body should explain the conflict: %s", body)
	}
}

func TestEscalationApprove_WrongBeadType(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-not", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestEscalationApprove_NotFound(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	rec := escPost(t, fake, "gd-missing", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestEscalationApprove_DaemonFailureShowsInlineError asserts a daemon-side
// failure re-renders the card with an error banner rather than a blank 502.
func TestEscalationApprove_DaemonFailureShowsInlineError(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	fake.approveEscErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Failed to approve") {
		t.Errorf("inline error missing: %s", body)
	}
}

// TestEscalationApprove_DaemonRefusalIs403 asserts a daemon 403 (the relayed
// static token is not an approval-capable identity) renders a clear refusal
// with the daemon's reason, not a 502 (gd-WJ09iJNvFM).
func TestEscalationApprove_DaemonRefusalIs403(t *testing.T) {
	for _, verb := range []string{"approve", "reject"} {
		fake := escActionFake("open", `["approve:escalation"]`)
		refusal := &beadsapi.APIError{StatusCode: 403, Message: "escalation approval requires an OIDC-authenticated identity"}
		fake.approveEscErr = refusal
		fake.rejectEscErr = refusal
		rec := escPost(t, fake, "gd-esc", verb, "approver@acme.example", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", verb, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "refused this "+verb) || !strings.Contains(body, "OIDC-authenticated identity") {
			t.Errorf("%s: refusal copy missing: %s", verb, body)
		}
	}
}

func TestEscalationApproveGate_Reasons(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	ctx := t.Context()

	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", ""); ok || !strings.Contains(why, "Sign in") {
		t.Errorf("anonymous: ok=%v why=%q", ok, why)
	}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); !ok {
		t.Error("daemon yes was refused")
	}
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "operator_only"}
	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); ok || !strings.Contains(why, "operator_only") {
		t.Errorf("daemon no: ok=%v why=%q", ok, why)
	}
	// Fail closed on a daemon error ...
	fake.canApproveErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); ok || !strings.Contains(why, "Couldn't check") {
		t.Errorf("daemon error: ok=%v why=%q", ok, why)
	}
	// ... except a daemon that predates can-approve (404): the legacy capability
	// gate, which that daemon's approve endpoint also expects.
	fake.canApproveErr = &beadsapi.APIError{StatusCode: 404, Message: "not found"}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); !ok {
		t.Error("404 fallback refused a viewer holding approve:escalation")
	}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "stranger@acme.example"); ok {
		t.Error("404 fallback admitted a viewer without approve:escalation")
	}
}

// TestBeadShow_EscalationRendersRichCard is the end-to-end render assertion:
// the escalation bead page shows the clickable MR pill and the approve chips
// instead of the old raw <pre> field dump.
func TestBeadShow_EscalationRendersRichCard(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/gd-esc", nil)
	req.Header.Set("X-Auth-Request-Email", "approver@acme.example")
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"escalation-card",
		`href="https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1274"`,
		"site-deployment!1274",
		"escalation-approve-chip",
		"escalation-reject-chip",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("escalation page missing %q", want)
		}
	}
}

func TestBeadShow_EscalationHidesChipsWhenDaemonSaysNo(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "held by coordinator:merge-desk"}
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/gd-esc", nil)
	req.Header.Set("X-Auth-Request-Email", "approver@acme.example")
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "escalation-approve-chip") {
		t.Error("approve chip rendered although the daemon said no")
	}
	if !strings.Contains(body, "held by coordinator:merge-desk") {
		t.Errorf("blocked reason not shown: %s", body)
	}
	if !strings.Contains(body, "site-deployment!1274") {
		t.Error("MR target hidden from a viewer who cannot approve")
	}
}

// TestEscalationApprove_DuplicateUserBeadsFailClosed: two open type=user beads
// share one email and only the second carries approve:escalation. Unioning
// would let the extra bead add a grant to the first (gd-W4FkF80BlX), so the
// shared email grants nothing and the approval is refused.
func TestEscalationApprove_DuplicateUserBeadsFailClosed(t *testing.T) {
	fake := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-esc": escBead2("open"),
		},
		userResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{
				userBead("gd-u1", "approver@acme.example", `[]`),
				userBead("gd-u2", "Approver@acme.example", `["approve:escalation"]`),
			},
			Total: 2,
		},
	}
	// The duplicate rule only runs on the legacy fallback (a daemon without
	// can-approve), so make that the path that refuses.
	fake.canApproveErr = &beadsapi.APIError{StatusCode: 404}
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("duplicate-email beads granted approval: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.approveEscCalls) != 0 {
		t.Fatalf("ApproveEscalation calls = %d, want 0", len(fake.approveEscCalls))
	}
}

// TestBuildUserCapsIndex_ListsOnlyLiveUserBeads: a closed user bead is a
// removed person, so the index asks the daemon for open/in_progress only.
func TestBuildUserCapsIndex_ListsOnlyLiveUserBeads(t *testing.T) {
	fake := &fakeDaemon{userResp: &beadsapi.ListBeadsResult{}}
	s := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	if _, err := s.buildUserCapsIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := fake.listLastQ
	if q == nil {
		t.Fatal("no user bead query issued")
	}
	if len(q.Statuses) != 2 || q.Statuses[0] != "open" || q.Statuses[1] != "in_progress" {
		t.Fatalf("user bead query statuses = %v, want [open in_progress]", q.Statuses)
	}
}
