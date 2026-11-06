package main

import (
	"net/http"
	"slices"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// decisionsScopeFake builds a fakeDaemon pre-loaded with open decision beads
// addressed to two different operators (id 100 = alice, id 101 = bob). Paired
// with inboxFilteringDaemon (which honours q.OperatedByID + q.Types), this lets
// the /decisions owner-scope tests assert the end-to-end filtering behaviour.
func decisionsScopeFake() *inboxFilteringDaemon {
	aliceID := int64(100)
	bobID := int64(101)
	mkDecision := func(id, title string, opID *int64) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:           id,
			Title:        title,
			Type:         "decision",
			Status:       "open",
			Category:     "action",
			OperatedByID: opID,
			Fields: map[string]string{
				"prompt": "Pick something for " + title,
			},
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now(),
		}
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-da1": mkDecision("gd-da1", "alice decision A", &aliceID),
		"gd-da2": mkDecision("gd-da2", "alice decision B", &aliceID),
		"gd-db1": mkDecision("gd-db1", "bob decision", &bobID),
	}
	return &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}}
}

func decisionsScopeLookup() *inboxFakeLookup {
	return &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 100, kind: "human", found: true},
		},
	}
}

// TestHandleDecisions_DefaultScopesToViewer verifies that a logged-in viewer
// sees only the decisions addressed to them, and that the handler issued the
// operated_by_id predicate for the viewer's operator id.
func TestHandleDecisions_DefaultScopesToViewer(t *testing.T) {
	daemon := decisionsScopeFake()
	mux := mkInboxMux(t, daemon, decisionsScopeLookup())

	req := inboxRequest(http.MethodGet, "/decisions", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-da1") || !strings.Contains(body, "gd-da2") {
		t.Errorf("expected alice's decisions (gd-da1/gd-da2) in body\n%s", body)
	}
	if strings.Contains(body, "gd-db1") {
		t.Errorf("leaked bob's decision (gd-db1) into alice's default view\n%s", body)
	}
	if !strings.Contains(body, "Show all") {
		t.Errorf("expected a Show all toggle when scope is active\n%s", body)
	}

	var sawScopedQuery bool
	for _, q := range daemon.fakeDaemon.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "decision" && q.OperatedByID == 100 {
			sawScopedQuery = true
			break
		}
	}
	if !sawScopedQuery {
		t.Errorf("expected a decision query with OperatedByID=100; got %+v", daemon.fakeDaemon.listAllQ)
	}
}

// TestHandleDecisions_ShowAllDropsScope verifies that ?all=1 drops the operator
// predicate and renders the full pending queue.
func TestHandleDecisions_ShowAllDropsScope(t *testing.T) {
	daemon := decisionsScopeFake()
	mux := mkInboxMux(t, daemon, decisionsScopeLookup())

	req := inboxRequest(http.MethodGet, "/decisions?all=1", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, id := range []string{"gd-da1", "gd-da2", "gd-db1"} {
		if !strings.Contains(body, id) {
			t.Errorf("expected all decisions in show-all view, missing %s\n%s", id, body)
		}
	}
	if !strings.Contains(body, "Show mine") {
		t.Errorf("expected a Show mine toggle in show-all view\n%s", body)
	}

	for _, q := range daemon.fakeDaemon.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "decision" && q.OperatedByID != 0 {
			t.Errorf("show-all should not scope by operator, got OperatedByID=%d", q.OperatedByID)
		}
	}
}

// TestHandleDecisions_UnresolvableViewerShowsAll verifies that an anonymous /
// unprovisioned viewer (no resolver match) falls back to the full queue and the
// scope toggle is hidden.
func TestHandleDecisions_UnresolvableViewerShowsAll(t *testing.T) {
	daemon := decisionsScopeFake()
	mux := mkInboxMux(t, daemon, decisionsScopeLookup())

	// "nobody@example.com" is not in the lookup → resolver returns not-found.
	req := inboxRequest(http.MethodGet, "/decisions", "nobody@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, id := range []string{"gd-da1", "gd-da2", "gd-db1"} {
		if !strings.Contains(body, id) {
			t.Errorf("unresolvable viewer should see the full queue, missing %s\n%s", id, body)
		}
	}
	if strings.Contains(body, "Show all") || strings.Contains(body, "Show mine") {
		t.Errorf("scope toggle should be hidden for an unresolvable viewer\n%s", body)
	}
	for _, q := range daemon.fakeDaemon.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "decision" && q.OperatedByID != 0 {
			t.Errorf("unresolvable viewer should not scope by operator, got OperatedByID=%d", q.OperatedByID)
		}
	}
}

// TestHandleDecisions_EmptyMineNudgesShowAll verifies that when the viewer owns
// no open decisions, the empty state points them at Show all.
func TestHandleDecisions_EmptyMineNudgesShowAll(t *testing.T) {
	// Lookup resolves alice to an operator id that owns no decisions (200).
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"carol@example.com": {id: 200, kind: "human", found: true},
		},
	}
	daemon := decisionsScopeFake()
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/decisions", "carol@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No open decisions assigned to you") {
		t.Errorf("expected scoped empty-state copy\n%s", body)
	}
	if !strings.Contains(body, "all=1") {
		t.Errorf("expected scoped empty state to link to Show all\n%s", body)
	}
}

// TestHandleDecisions_MineIncludesLandsApprovalAddressedToViewer: the Lands
// Gate files its approval decision as a machine principal and names the
// approver only in fields.operated_by, so the OperatedByID scope alone hid it
// from the approver (2026-11-05, gd-SlkrBguRxA). The "mine" view adds it; a
// lands decision addressed to someone else stays out.
func TestHandleDecisions_MineIncludesLandsApprovalAddressedToViewer(t *testing.T) {
	daemon := decisionsScopeFake()
	machineID := int64(25)
	for id, approver := range map[string]string{"gd-land1": "oidc:Alice@example.com", "gd-land2": "oidc:bob@example.com"} {
		daemon.fakeDaemon.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: "Plot application " + id, Type: "decision", Status: "open", Category: "action",
			OperatedByID: &machineID, Labels: []string{landsIntakeLabel},
			Fields:    map[string]string{"prompt": "Plot application " + id, "operated_by": approver},
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	srv := mkInboxServer(t, daemon, decisionsScopeLookup())
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	w := httptest.NewRecorder()
	edgeAuth(mux).ServeHTTP(w, inboxRequest(http.MethodGet, "/decisions", "alice@example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-land1") {
		t.Errorf("the lands approval addressed to alice is missing from her default view\n%s", body)
	}
	if strings.Contains(body, "gd-land2") || strings.Contains(body, "gd-db1") {
		t.Errorf("a decision addressed to someone else leaked into alice's view\n%s", body)
	}
	if !strings.Contains(body, "gd-da1") {
		t.Errorf("alice's own decisions dropped\n%s", body)
	}

	// The garden Queues panel's decisions tab reads the same "mine" scope.
	ctx := oidcauth.ContextWithEmail(inboxRequest(http.MethodGet, "/", "alice@example.com").Context(), "alice@example.com")
	doc, err := srv.decisionsQueue(ctx, false)
	if err != nil {
		t.Fatalf("decisionsQueue: %v", err)
	}
	var ids []string
	for _, row := range doc.Rows {
		ids = append(ids, row.ID)
	}
	if !slices.Contains(ids, "gd-land1") || slices.Contains(ids, "gd-land2") {
		t.Errorf("garden queue mine rows = %v, want gd-land1 and not gd-land2", ids)
	}
}
