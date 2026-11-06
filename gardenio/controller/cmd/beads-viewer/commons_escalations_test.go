package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/oidcauth"
)

// The daemon client answers can-approve, so newCommonsHandler wires it as the
// Commons' Approvals (escalation routing T12).
var _ commons.Approvals = (*beadsapi.Client)(nil)

// commonsEscReq is a Commons card POST as the signed-in approver.
func commonsEscReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/garden/c/action/cl-ecard1", nil)
	return r.WithContext(oidcauth.ContextWithEmail(r.Context(), "approver@acme.example"))
}

func commonsEscAdapter(fake *fakeDaemon) commonsEscalations {
	return commonsEscalations{s: NewServer(fake, slog.Default(), "", "http://test", "", nil)}
}

// TestCommonsEscalations_RefusesOnDaemonNo: the card's Approve and Reject
// re-ask the daemon's can-approve (D9) and refuse with its reason, without
// calling approve or reject, even for a viewer who carries approve:escalation.
func TestCommonsEscalations_RefusesOnDaemonNo(t *testing.T) {
	for _, reject := range []bool{false, true} {
		fake := escActionFake("open", `["approve:escalation"]`)
		fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "held by coordinator:merge-desk"}
		e := commonsEscAdapter(fake)
		var err error
		if reject {
			err = e.Reject(commonsEscReq(), "gd-esc", "no")
		} else {
			err = e.Approve(commonsEscReq(), "gd-esc")
		}
		var refusal *commons.CardRefusal
		if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
			t.Fatalf("reject=%v: err = %v, want a 403 CardRefusal", reject, err)
		}
		if !strings.Contains(refusal.Message, "held by coordinator:merge-desk") {
			t.Errorf("reject=%v: refusal %q does not carry the daemon's reason", reject, refusal.Message)
		}
		if len(fake.approveEscIDs) != 0 || len(fake.rejectEscIDs) != 0 {
			t.Errorf("reject=%v: daemon called (approve %v, reject %v) after can-approve said no", reject, fake.approveEscIDs, fake.rejectEscIDs)
		}
	}
}

// TestCommonsEscalations_ApprovesOnDaemonYes is the control: can-approve says
// yes, so the approve goes to the daemon as a Commons approval, even for a
// viewer without the legacy capability.
func TestCommonsEscalations_ApprovesOnDaemonYes(t *testing.T) {
	fake := escActionFake("open", `[]`)
	if err := commonsEscAdapter(fake).Approve(commonsEscReq(), "gd-esc"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(fake.approveEscCalls) != 1 || fake.approveEscIDs[0] != "gd-esc" {
		t.Fatalf("approve calls = %v, want one for gd-esc", fake.approveEscIDs)
	}
	if got := fake.approveEscCalls[0]; got.ApprovalMethod != "commons" || got.ApprovedBy != "approver@acme.example" {
		t.Errorf("approve request = %+v, want approval_method commons by approver@acme.example", got)
	}
	if len(fake.canApproveIDs) != 1 || fake.canApproveIDs[0] != "gd-esc" {
		t.Errorf("can-approve asked about %v, want gd-esc once", fake.canApproveIDs)
	}
}

// TestCommonsEscalations_StatusBeforeGate: a closed escalation answers 409
// and a non-escalation 400, as the bead page does, before can-approve is asked.
func TestCommonsEscalations_StatusBeforeGate(t *testing.T) {
	for _, c := range []struct {
		id, status string
		want       int
	}{{"gd-esc", "closed", http.StatusConflict}, {"gd-not", "open", http.StatusBadRequest}} {
		fake := escActionFake(c.status, `[]`)
		err := commonsEscAdapter(fake).Approve(commonsEscReq(), c.id)
		var refusal *commons.CardRefusal
		if !errors.As(err, &refusal) || refusal.Status != c.want {
			t.Errorf("%s (%s): err = %v, want %d", c.id, c.status, err, c.want)
		}
		if len(fake.canApproveIDs) != 0 {
			t.Errorf("%s (%s): can-approve asked before the status check", c.id, c.status)
		}
	}
}

// TestNewCommonsHandlerWiresApprovals: a daemon that answers can-approve
// becomes the handler's Approvals, so cards can show Approve and Reject.
func TestNewCommonsHandlerWiresApprovals(t *testing.T) {
	if h := newCommonsHandler(escActionFake("open", `[]`), nil, nil, "", nil); h.Approvals == nil {
		t.Fatal("Approvals is nil: no escalation card would ever offer Approve")
	}
}
