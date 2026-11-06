package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The Queues panel's reader and the R1/R2 retirement (Lands workbench-glass spec WB-6, §2.6; step 5.5, gd-HuJdsFabSb).

func queueCtx(email string) context.Context {
	return oidcauth.ContextWithEmail(context.WithValue(context.Background(), inboxTestEmailCtxKey{}, email), email)
}

func queueIDs(d *gardenhub.QueueDoc) string {
	var ids []string
	for _, r := range d.Rows {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, " ")
}

// The Decisions tab is /decisions: the viewer's own by default, everyone's with all, the viewer's marked "asked of you".
func TestGardenQueueDecisions(t *testing.T) {
	srv := mkInboxServer(t, decisionsScopeFake(), decisionsScopeLookup())
	q := gardenQueues{srv}

	d, err := q.Queue(queueCtx("alice@example.com"), gardenhub.QueueDecisions, false)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Scoped || d.All || d.Note != "" {
		t.Errorf("mine: scoped %v all %v note %q", d.Scoped, d.All, d.Note)
	}
	if ids := queueIDs(d); !strings.Contains(ids, "gd-da1") || !strings.Contains(ids, "gd-da2") || strings.Contains(ids, "gd-db1") {
		t.Errorf("mine rows %q, want alice's two only", ids)
	}
	for _, r := range d.Rows {
		if !r.NeedsYou || r.Why != "asked of you" || r.Kind != "decision" || r.Since == 0 || !strings.HasPrefix(r.Text, "Pick something") {
			t.Errorf("row %+v", r)
		}
	}

	d, err = q.Queue(queueCtx("alice@example.com"), gardenhub.QueueDecisions, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 3 || !d.All {
		t.Fatalf("all: %d rows (all %v), want 3", len(d.Rows), d.All)
	}
	for _, r := range d.Rows {
		if r.NeedsYou != (r.ID != "gd-db1") {
			t.Errorf("all: %s needs_you %v", r.ID, r.NeedsYou)
		}
	}

	// a viewer with no operator identity sees every open decision, none marked theirs, and is told why
	d, err = q.Queue(queueCtx("nobody@example.com"), gardenhub.QueueDecisions, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 3 || d.Scoped || !d.All || d.Note == "" {
		t.Errorf("no identity: %d rows scoped %v all %v note %q", len(d.Rows), d.Scoped, d.All, d.Note)
	}
}

// The Escalations tab is /escalations: the owner's listed and needing the viewer, the desk's and the operators' folded
// into the In hand line, or listed with all.
func TestGardenQueueEscalations(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{escalationsResp: []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-own", "Merge it", "gitlab_mr_merge", "internal", "open", at, map[string]string{"context": "please"})},
		{Issue: makeEscBead("gd-desk", "Desk one", "aws_ec2", "internal", "open", at.Add(time.Hour), map[string]string{"audience": "desk"})},
	}}
	q := gardenQueues{newTestServerForEsc(t, daemon)}

	d, err := q.Queue(queueCtx("alice@example.com"), gardenhub.QueueEscalations, false)
	if err != nil {
		t.Fatal(err)
	}
	if queueIDs(d) != "gd-own" || d.InHand == "" {
		t.Fatalf("owner view: rows %q in hand %q", queueIDs(d), d.InHand)
	}
	r := d.Rows[0]
	if !r.NeedsYou || r.Why != "for the owner" || r.Operation != "gitlab_mr_merge" || r.Text != "please" || r.Since != at.Unix() || r.Audience != "owner" {
		t.Errorf("owner row %+v", r)
	}

	d, err = q.Queue(queueCtx("alice@example.com"), gardenhub.QueueEscalations, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 2 || d.InHand != "" {
		t.Fatalf("all: rows %q in hand %q", queueIDs(d), d.InHand)
	}
	for _, r := range d.Rows {
		if r.NeedsYou != (r.ID == "gd-own") {
			t.Errorf("all: %s needs_you %v", r.ID, r.NeedsYou)
		}
	}
}

// An approved escalation is still in_progress until its executor closes it, so the daemon's open + in_progress list
// carries it: the tab says its state and offers it to nobody (no needs_you, so no Approve box), while a pending one is
// the owner's to answer (gd-yuOG6cX6VI).
func TestGardenQueueEscalationsApprovedIsNotAnswerable(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{escalationsResp: []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-pending", "Merge it", "gitlab_mr_merge", "internal", "open", at, nil)},
		{Issue: makeEscBead("gd-approved", "Merged already", "gitlab_mr_merge", "internal", "in_progress", at.Add(time.Hour),
			map[string]string{"approved_by": "owner@example.com", "approved_at": "2026-10-01T13:00:00Z", "approval_method": "web_ui"})},
	}}
	q := gardenQueues{newTestServerForEsc(t, daemon)}
	d, err := q.Queue(queueCtx("alice@example.com"), gardenhub.QueueEscalations, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]gardenhub.QueueRow{}
	for _, r := range d.Rows {
		rows[r.ID] = r
	}
	if p := rows["gd-pending"]; p.State != "pending" || !p.NeedsYou || p.Why != "for the owner" {
		t.Errorf("pending row %+v", p)
	}
	if a := rows["gd-approved"]; a.State != "approved" || a.NeedsYou || a.Why != "" || a.Status != "in_progress" {
		t.Errorf("approved row %+v", a)
	}
}

// The Inbox tab is the /inbox stream: a pinned row needs the viewer; a viewer with no operator identity gets a note.
func TestGardenQueueInbox(t *testing.T) {
	humanID := int64(100)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-aaa": {ID: "gd-aaa", Title: "normal", Type: "task", Status: "open", Category: "intake", OperatedByID: &humanID, UpdatedAt: time.Now()},
		"gd-zzz": {ID: "gd-zzz", Title: "pinned", Type: "task", Status: "open", Category: "intake", OperatedByID: &humanID, UpdatedAt: time.Now(), Labels: []string{labelInboxPinned, labelInboxWorthy}},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	srv := mkInboxServer(t, daemon, decisionsScopeLookup())
	q := gardenQueues{srv}

	d, err := q.Queue(queueCtx("alice@example.com"), gardenhub.QueueInbox, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]gardenhub.QueueRow{}
	for _, r := range d.Rows {
		got[r.ID] = r
	}
	if p, n := got["gd-zzz"], got["gd-aaa"]; !p.NeedsYou || p.Why != "pinned" || !p.Pinned || !p.Worthy || n.NeedsYou || n.Kind != "intake" {
		t.Errorf("pinned %+v, normal %+v", p, n)
	}
	if d.Scoped || d.All {
		t.Errorf("the inbox has only a mine: scoped %v all %v", d.Scoped, d.All)
	}

	d, err = q.Queue(queueCtx("nobody@example.com"), gardenhub.QueueInbox, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 0 || d.Note == "" {
		t.Errorf("no identity: %d rows, note %q", len(d.Rows), d.Note)
	}
}

// A retiring queue page says "Open it in the garden" to a viewer who reaches /garden (default on), and with the
// redirect flag on it answers 302 to the panel, carrying its query but no other panel's parameter; a scoped principal,
// or a viewer with no garden, keeps the page as it was.
func TestGardenQueuesRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string, garden bool) http.Handler {
		srv := NewServer(decisionsScopeFake(), slog.Default(), "/v", "", "", nil)
		srv.inboxLegacyRoute = true
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		if garden {
			srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		}
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	on := newSrv(t, nil, true)
	for path, want := range map[string]string{
		"/decisions":   `href="/v/garden?queue=decisions"`,
		"/escalations": `href="/v/garden?queue=escalations"`,
		"/inbox":       `href="/v/garden?queue=inbox"`,
	} {
		w := get(on, path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="garden-banner"`) || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d, banner with %s missing", path, w.Code, want)
		}
	}
	if w := get(newSrv(t, nil, false), "/decisions"); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("no /garden wired: no banner")
	}
	if w := get(newSrv(t, map[string]string{gardenQueuesBannerEnv: "false"}, true), "/decisions"); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("banner flag off: no banner")
	}

	redirect := newSrv(t, map[string]string{gardenQueuesRedirectEnv: "true"}, true)
	for path, want := range map[string]string{
		"/decisions?all=1&project=gardenio": "/v/garden?all=1&queue=decisions",
		"/decisions/gd-da1":                 "/v/garden?bead=gd-da1",
		"/escalations":                      "/v/garden?queue=escalations",
		"/inbox":                            "/v/garden?queue=inbox",
		"/inbox/gd-zzz?scope=mine":          "/v/garden?scope=mine&bead=gd-zzz",
	} {
		w := get(redirect, path)
		if w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := get(newSrv(t, map[string]string{gardenQueuesRedirectEnv: "true"}, false), "/decisions"); w.Code != http.StatusOK {
		t.Errorf("redirect flag on without a garden: %d, want the page", w.Code)
	}
}

// gardenPanelURL drops every Workbench panel parameter from the page's query, so the panel it moved to is the one open.
func TestGardenPanelURL(t *testing.T) {
	q := map[string][]string{"project": {"x"}, "bead": {"gd-b"}, "at": {"1,2"}}
	if got := gardenPanelURL("/v", q, "queue", "decisions"); got != "/v/garden?at=1%2C2&queue=decisions" {
		t.Errorf("got %q", got)
	}
	if got := gardenPanelURL("", nil, "bead", "gd-a"); got != "/garden?bead=gd-a" {
		t.Errorf("got %q", got)
	}
}
