package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// makeEscBead is a test helper for an escalation BeadDetail. The Type is
// "escalation" + Kind "data", mirroring what kbeads stores. The bead is
// returned in the "issue" key of /v1/escalations — the "escalation" key is
// a typed-fields envelope with no id/title/status (see daemon
// http_escalations.go:extractEscalationFields).
func makeEscBead(id, title, opType, sensitivity, status string, created time.Time, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"operation_type": opType,
		"sensitivity":    sensitivity,
		"audience":       "owner", // the owner's unless a test says otherwise (gd-bmOiteQi36)
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Type:      "escalation",
		Status:    status,
		Labels:    []string{"project:gardenio"},
		Fields:    f,
		CreatedAt: created,
		UpdatedAt: created,
	}
}

func newTestServerForEsc(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "http://test", "", nil)
	return srv
}

func TestHandleEscalations_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-2 * time.Hour)
	even_earlier := now.Add(-3 * time.Hour)

	daemon := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-esc1", "Grant ec2:DeleteVolume", "aws_iam", "internal", "open", earlier, map[string]string{
				"requesting_agent": "agent-7",
				"requesting_task":  "gd-task1",
				"context":          "EBS GC needs DescribeVolumes + DeleteVolume — controller SA only has S3.",
			})},
			{Issue: makeEscBead("gd-esc2", "Mint Mezmo viewer service key", "credential_provisioning", "secret", "in_progress", even_earlier, map[string]string{
				"requesting_task": "gd-task2",
			})},
		},
	}

	srv := newTestServerForEsc(t, daemon)
	srv.escalationReviewURL = "https://gardenio.example.test/escalations"
	req := httptest.NewRequest(http.MethodGet, "/escalations", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Grant ec2:DeleteVolume",
		"gd-esc1",
		"aws_iam",
		"internal",
		"Mint Mezmo viewer service key",
		"gd-esc2",
		"credential_provisioning",
		"secret",
		"in progress", // status chip remap
		"https://gardenio.example.test/escalations?bead_id=gd-esc1",
		"https://gardenio.example.test/escalations?bead_id=gd-esc2",
		"EBS GC needs DescribeVolumes", // context snippet
		"agent-7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}

	// status="" means "open + in_progress" per kbeads default — verify the
	// handler doesn't accidentally narrow.
	if daemon.escalationsLastSt != "" {
		t.Errorf("ListEscalations status = %q, want empty", daemon.escalationsLastSt)
	}
	if daemon.escalationsLastLim != escalationsListLimit {
		t.Errorf("ListEscalations limit = %d, want %d", daemon.escalationsLastLim, escalationsListLimit)
	}
}

func TestHandleEscalations_EmptyState(t *testing.T) {
	daemon := &fakeDaemon{escalationsResp: nil}
	srv := newTestServerForEsc(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/escalations", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No pending escalations.") {
		t.Errorf("empty-state copy missing; body=\n%s", rec.Body.String())
	}
}

func TestHandleEscalations_DaemonError(t *testing.T) {
	daemon := &fakeDaemon{escalationsErr: errors.New("daemon down")}
	srv := newTestServerForEsc(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/escalations", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, req)

	// We render the page with an inline banner — same shape as /decisions on
	// failure — rather than emitting an HTTP error code. That way the user
	// sees navigation + status without a raw error page.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inline banner)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to load escalations.") {
		t.Errorf("error banner missing; body=\n%s", rec.Body.String())
	}
}

func TestHandleEscalations_ProjectFilter(t *testing.T) {
	now := time.Now()
	daemon := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: &beadsapi.BeadDetail{
				ID: "gd-a", Title: "gardenio work", Type: "escalation", Status: "open",
				Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "aws_iam"},
				CreatedAt: now, UpdatedAt: now,
			}},
			{Issue: &beadsapi.BeadDetail{
				ID: "gd-b", Title: "kbeads work", Type: "escalation", Status: "open",
				Labels: []string{"project:kbeads"}, Fields: map[string]string{"operation_type": "k8s_rbac"},
				CreatedAt: now, UpdatedAt: now,
			}},
		},
	}
	srv := newTestServerForEsc(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/escalations?project=gardenio", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "gd-a") {
		t.Errorf("expected gardenio escalation to render; body=\n%s", body)
	}
	if strings.Contains(body, "gd-b") {
		t.Errorf("expected kbeads escalation to be filtered out; body=\n%s", body)
	}
}

func TestHandleEscalations_ReviewURLFallsBackToBeadPage_WhenEnvUnset(t *testing.T) {
	now := time.Now()
	daemon := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-z", "anything", "aws_iam", "internal", "open", now, nil)},
		},
	}
	srv := newTestServerForEsc(t, daemon)
	// escalationReviewURL deliberately left empty -> fall back to bead page.
	req := httptest.NewRequest(http.MethodGet, "/escalations", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, req)

	body := rec.Body.String()
	// The SPA deep link must NOT appear (env var unset).
	if strings.Contains(body, "bead_id=gd-z") {
		t.Errorf("SPA deep link should not render when ESCALATION_REVIEW_URL is unset; body=\n%s", body)
	}
	// The "Review ›" anchor SHOULD now appear, pointing at the in-viewer bead page.
	if !strings.Contains(body, "Review&nbsp;&rsaquo;") {
		t.Errorf("Review label should render (falls back to bead page); body=\n%s", body)
	}
	if !strings.Contains(body, `href="/gd-z"`) {
		t.Errorf("Review link should point at the bead page /gd-z; body=\n%s", body)
	}
}

func TestBuildEscalationsTileView_OrdersOldestFirst(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	mid := now.Add(-6 * time.Hour)
	recent := now.Add(-5 * time.Minute)
	fresh := now.Add(-1 * time.Minute)

	daemon := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			// Deliberately out of order.
			{Issue: makeEscBead("gd-recent", "newest", "aws_iam", "internal", "open", recent, nil)},
			{Issue: makeEscBead("gd-old", "oldest", "k8s_rbac", "internal", "open", old, nil)},
			{Issue: makeEscBead("gd-mid", "middle", "credential_rotation", "secret", "in_progress", mid, nil)},
			{Issue: makeEscBead("gd-fresh", "freshest", "aws_iam", "internal", "open", fresh, nil)},
		},
	}
	srv := newTestServerForEsc(t, daemon)
	view := srv.buildEscalationsTileView(context.Background(), now)

	if view.Error != "" {
		t.Fatalf("unexpected error: %q", view.Error)
	}
	if view.Pending != 4 {
		t.Errorf("Pending = %d, want 4", view.Pending)
	}
	if got, want := len(view.Next), 3; got != want {
		t.Fatalf("Next length = %d, want %d", got, want)
	}
	wantIDs := []string{"gd-old", "gd-mid", "gd-recent"}
	for i, want := range wantIDs {
		if view.Next[i].ID != want {
			t.Errorf("Next[%d].ID = %q, want %q (full: %+v)", i, view.Next[i].ID, want, view.Next)
		}
	}
}

func TestBuildEscalationsTileView_EmptyQueue(t *testing.T) {
	daemon := &fakeDaemon{escalationsResp: nil}
	srv := newTestServerForEsc(t, daemon)
	view := srv.buildEscalationsTileView(context.Background(), time.Now())

	if view.Error != "" {
		t.Errorf("Error should be empty for nil response, got %q", view.Error)
	}
	if view.Pending != 0 {
		t.Errorf("Pending = %d, want 0", view.Pending)
	}
	if len(view.Next) != 0 {
		t.Errorf("Next should be empty, got %d rows", len(view.Next))
	}
}

func TestBuildEscalationsTileView_DaemonError(t *testing.T) {
	daemon := &fakeDaemon{escalationsErr: errors.New("boom")}
	srv := newTestServerForEsc(t, daemon)
	view := srv.buildEscalationsTileView(context.Background(), time.Now())

	if view.Error == "" {
		t.Errorf("expected Error to be populated on daemon failure")
	}
	if view.Pending != 0 {
		t.Errorf("Pending should be zero on error, got %d", view.Pending)
	}
}

func TestBuildEscalationReviewURL(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		basePath string
		id       string
		want     string
	}{
		// Fallback path: env var unset -> in-viewer bead page ({basePath}/{id}).
		{"unset falls back to bead page", "", "", "gd-1", "/gd-1"},
		{"unset with base path", "", "/beads", "gd-1", "/beads/gd-1"},
		{"unset encodes id", "", "", "gd-with/slash", "/gd-with%2Fslash"},
		{"empty id", "https://x.example", "", "", ""},
		{"empty id unset", "", "", "", ""},
		// Override path: env var set -> bridge SPA deep link.
		{"plain", "https://x.example/escalations", "", "gd-1", "https://x.example/escalations?bead_id=gd-1"},
		{"preexisting query", "https://x.example/escalations?theme=dark", "", "gd-2", "https://x.example/escalations?theme=dark&bead_id=gd-2"},
		{"needs encoding", "https://x.example/escalations", "", "gd-with/slash", "https://x.example/escalations?bead_id=gd-with%2Fslash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{escalationReviewURL: tc.base, basePath: tc.basePath}
			got := srv.buildEscalationReviewURL(tc.id)
			if got != tc.want {
				t.Errorf("buildEscalationReviewURL(base=%q, basePath=%q, id=%q) = %q, want %q", tc.base, tc.basePath, tc.id, got, tc.want)
			}
		})
	}
}

func TestHandleHealthTileEscalations_RendersFragment(t *testing.T) {
	now := time.Now()
	daemon := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: makeEscBead("gd-t1", "oldest tile row", "aws_iam", "internal", "open", now.Add(-2*time.Hour), nil)},
		},
	}
	srv := newTestServerForEsc(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/health/tile/escalations", nil)
	rec := httptest.NewRecorder()
	srv.handleHealthTileEscalations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gd-t1") {
		t.Errorf("tile body missing escalation row; body=\n%s", body)
	}
	if !strings.Contains(body, "Escalations waiting on operator") {
		t.Errorf("tile heading missing; body=\n%s", body)
	}
}

// TestHandleEscalations_RealDaemonJSONShape is a regression test for gd-QesPVjhM7F:
// the daemon /v1/escalations response has `escalation` as a typed-fields
// envelope (no id/title/status — see kbeads http_escalations.go
// extractEscalationFields) and the actual bead in `issue`. Prior to the fix
// the viewer keyed off ed.Escalation.ID and skipped every row, rendering an
// empty queue on /escalations and the health tile.
func TestHandleEscalations_RealDaemonJSONShape(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	daemonJSON := `{
	  "escalations": [
	    {
	      "escalation": {
	        "operation_type": "aws_iam",
	        "sensitivity": "internal",
	        "context": "EBS GC needs DescribeVolumes",
	        "requesting_agent": "agent-7",
	        "requesting_task": "gd-task1",
	        "audience": "owner"
	      },
	      "issue": {
	        "id": "gd-real1",
	        "title": "Grant ec2:DeleteVolume",
	        "type": "escalation",
	        "status": "open",
	        "labels": ["project:gardenio"],
	        "fields": {
	          "operation_type": "aws_iam",
	          "sensitivity": "internal",
	          "context": "EBS GC needs DescribeVolumes",
	          "requesting_agent": "agent-7",
	          "requesting_task": "gd-task1",
	          "audience": "owner"
	        },
	        "created_at": "2026-05-15T10:00:00Z",
	        "updated_at": "2026-05-15T10:00:00Z"
	      }
	    }
	  ]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(daemonJSON))
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	escalations, err := client.ListEscalations(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if len(escalations) != 1 {
		t.Fatalf("got %d escalations, want 1", len(escalations))
	}
	if escalations[0].Issue == nil || escalations[0].Issue.ID != "gd-real1" {
		t.Fatalf("expected Issue with id gd-real1, got %+v", escalations[0].Issue)
	}

	// Wire the real-shape fixture through the viewer.
	fake := &fakeDaemon{escalationsResp: escalations}
	viewer := newTestServerForEsc(t, fake)
	viewer.escalationReviewURL = "https://gardenio.example.test/escalations"

	req := httptest.NewRequest(http.MethodGet, "/escalations", nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	viewer.handleEscalations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/escalations status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"gd-real1", "Grant ec2:DeleteVolume", "aws_iam", "agent-7"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/escalations body missing %q\n--- body ---\n%s", want, rec.Body.String())
		}
	}

	// And the health tile fragment.
	tileReq := httptest.NewRequest(http.MethodGet, "/health/tile/escalations", nil)
	tileRec := httptest.NewRecorder()
	viewer.handleHealthTileEscalations(tileRec, tileReq)
	if tileRec.Code != http.StatusOK {
		t.Fatalf("tile status = %d, want 200", tileRec.Code)
	}
	if !strings.Contains(tileRec.Body.String(), "gd-real1") {
		t.Errorf("tile body missing gd-real1\n--- body ---\n%s", tileRec.Body.String())
	}
	_ = now
}
