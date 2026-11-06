package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// autoCollectStub serves the routes gb decision report exercises when
// GARDENIO_BUNDLE_AUTOCOLLECT=1 is set:
//
//	GET  /v1/beads/<id>                 → bead fixtures by id
//	GET  /v1/beads/<id>/dependencies    → parent-child deps
//	GET  /v1/beads/<id>/mentioned-by    → reverse-mention edges (children of <id>)
//	POST /v1/beads                      → assigns gd-bead-N + records type
//	POST /v1/beads/<id>/dependencies    → records the edge
//
// The stub seeds bead fixtures for the decision and origin task and lets
// tests read back recorded edges + created beads.
type autoCollectStub struct {
	mu             sync.Mutex
	decisionFields map[string]string
	beads          map[string]map[string]any // pre-seeded beads by id
	deps           map[string][]beadsapi.Dependency
	mentionedBy    map[string][]beadsapi.BeadReferenceSummary

	createCounter int
	createdBeads  []createdBead
	addedDeps     []addedDep
}

type createdBead struct {
	ID    string
	Title string
	Type  string
}

func (s *autoCollectStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			s.mu.Lock()
			deps := s.deps[id]
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/mentioned-by"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/mentioned-by")
			s.mu.Lock()
			refs := s.mentionedBy[id]
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"references": refs})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			var body addDepBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.addedDeps = append(s.addedDeps, addedDep{BeadID: id, DependsOnID: body.DependsOnID, Type: body.Type})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			s.mu.Lock()
			fixture, ok := s.beads[id]
			s.mu.Unlock()
			if !ok {
				// default: a decision-typed bead so the report flow doesn't bail.
				fixture = map[string]any{
					"id":     id,
					"type":   "decision",
					"status": "open",
					"fields": s.decisionFields,
				}
			}
			_ = json.NewEncoder(w).Encode(fixture)

		case r.Method == http.MethodPost && path == "/v1/beads":
			var body beadsapi.CreateBeadRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.createCounter++
			id := newCreatedID(body.Type, s.createCounter)
			s.createdBeads = append(s.createdBeads, createdBead{ID: id, Title: body.Title, Type: body.Type})
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
}

func newCreatedID(beadType string, n int) string {
	switch beadType {
	case "report":
		return reportIDForCounter(n)
	case "bundle":
		return bundleIDForCounter(n)
	}
	return "gd-other-" + itoa(n)
}

func reportIDForCounter(n int) string { return "gd-report-" + itoa(n) }
func bundleIDForCounter(n int) string { return "gd-bundle-" + itoa(n) }

func runAutoCollectReport(t *testing.T, stub *autoCollectStub) *autoCollectStub {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content", "ok")
	_ = cmd.Flags().Set("type", "summary")
	_ = cmd.Flags().Set("format", "markdown")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("content", "")
		_ = cmd.Flags().Set("type", "")
		_ = cmd.Flags().Set("format", "markdown")
	})
	if err := cmd.RunE(cmd, []string{"gd-decision-1"}); err != nil {
		t.Fatalf("decision report: %v", err)
	}
	return stub
}

// stubWithOriginAndDecision returns a stub with gd-decision-1 parented to
// gd-task-orig (the origin task). Tests can extend mentionedBy / deps before
// running the command.
func stubWithOriginAndDecision() *autoCollectStub {
	return &autoCollectStub{
		decisionFields: map[string]string{},
		beads: map[string]map[string]any{
			"gd-decision-1": {
				"id":     "gd-decision-1",
				"type":   "decision",
				"status": "open",
				"fields": map[string]string{},
			},
			"gd-task-orig": {
				"id":     "gd-task-orig",
				"type":   "task",
				"title":  "Origin Task Title",
				"status": "in_progress",
				"fields": map[string]string{},
			},
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-task-orig", Type: "parent-child"},
			},
		},
		mentionedBy: map[string][]beadsapi.BeadReferenceSummary{},
	}
}

// TestDecisionReport_AutoCollect_FlagOff_NoBundleCreated asserts the default
// (env unset) path: the report bead is created but no bundle bead exists.
func TestDecisionReport_AutoCollect_FlagOff_NoBundleCreated(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_AUTOCOLLECT", "")
	stub := stubWithOriginAndDecision()
	runAutoCollectReport(t, stub)

	for _, b := range stub.createdBeads {
		if b.Type == "bundle" {
			t.Fatalf("flag off: no bundle bead expected, got %+v", b)
		}
	}
	for _, d := range stub.addedDeps {
		if strings.HasPrefix(d.DependsOnID, "gd-bundle-") {
			t.Fatalf("flag off: no edge to a bundle expected, got %+v", d)
		}
	}
}

// TestDecisionReport_AutoCollect_FlagOn_CreatesBundleAndLinks asserts that
// with the flag set and no existing bundle on the origin task, a single new
// bundle bead is created, parented to the origin, and the report is linked
// under it.
func TestDecisionReport_AutoCollect_FlagOn_CreatesBundleAndLinks(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_AUTOCOLLECT", "1")
	stub := stubWithOriginAndDecision()
	runAutoCollectReport(t, stub)

	bundleCount := 0
	var bundleID, bundleTitle string
	for _, b := range stub.createdBeads {
		if b.Type == "bundle" {
			bundleCount++
			bundleID = b.ID
			bundleTitle = b.Title
		}
	}
	if bundleCount != 1 {
		t.Fatalf("flag on, no existing bundle: want exactly 1 bundle created, got %d (%+v)", bundleCount, stub.createdBeads)
	}
	if !strings.Contains(bundleTitle, "Origin Task Title") || !strings.Contains(bundleTitle, "Bundle") {
		t.Errorf("bundle title should derive from origin title; got %q", bundleTitle)
	}

	// bundle → origin parent-child
	wantBundleToOrigin := false
	// report → bundle parent-child
	wantReportToBundle := false
	for _, d := range stub.addedDeps {
		if d.Type != "parent-child" {
			continue
		}
		if d.BeadID == bundleID && d.DependsOnID == "gd-task-orig" {
			wantBundleToOrigin = true
		}
		if strings.HasPrefix(d.BeadID, "gd-report-") && d.DependsOnID == bundleID {
			wantReportToBundle = true
		}
	}
	if !wantBundleToOrigin {
		t.Errorf("expected bundle→origin parent-child edge; got %+v", stub.addedDeps)
	}
	if !wantReportToBundle {
		t.Errorf("expected report→bundle parent-child edge; got %+v", stub.addedDeps)
	}
}

// TestDecisionReport_AutoCollect_TwoReports_SameBundle covers the append
// path: a second decision report against the same origin task reuses the
// existing bundle instead of creating a new one.
func TestDecisionReport_AutoCollect_TwoReports_SameBundle(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_AUTOCOLLECT", "1")
	stub := stubWithOriginAndDecision()
	// Pre-seed: an existing bundle on the origin from a prior report.
	parentChild := "parent-child"
	stub.mentionedBy["gd-task-orig"] = []beadsapi.BeadReferenceSummary{
		{
			ID:               "gd-bundle-existing",
			Type:             "bundle",
			Status:           "open",
			RelationshipType: &parentChild,
		},
	}
	stub.beads["gd-bundle-existing"] = map[string]any{
		"id":     "gd-bundle-existing",
		"type":   "bundle",
		"status": "open",
		"fields": map[string]string{"kind": "report"},
	}

	runAutoCollectReport(t, stub)

	for _, b := range stub.createdBeads {
		if b.Type == "bundle" {
			t.Fatalf("expected no new bundle when one already exists; got %+v", b)
		}
	}

	wantReportToExisting := false
	for _, d := range stub.addedDeps {
		if d.Type == "parent-child" && strings.HasPrefix(d.BeadID, "gd-report-") && d.DependsOnID == "gd-bundle-existing" {
			wantReportToExisting = true
		}
	}
	if !wantReportToExisting {
		t.Errorf("expected report→existing-bundle parent-child edge; got %+v", stub.addedDeps)
	}
}

// TestDecisionReport_AutoCollect_FlagOn_StandaloneDecisionNoBundle asserts
// that when the decision has no inferable origin task (operator-created
// standalone decision), no bundle is created even with the flag set.
func TestDecisionReport_AutoCollect_FlagOn_StandaloneDecisionNoBundle(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_AUTOCOLLECT", "1")
	stub := &autoCollectStub{
		decisionFields: map[string]string{},
		beads: map[string]map[string]any{
			"gd-decision-1": {
				"id":     "gd-decision-1",
				"type":   "decision",
				"status": "open",
				"fields": map[string]string{},
			},
		},
		deps:        map[string][]beadsapi.Dependency{},
		mentionedBy: map[string][]beadsapi.BeadReferenceSummary{},
	}
	runAutoCollectReport(t, stub)

	for _, b := range stub.createdBeads {
		if b.Type == "bundle" {
			t.Fatalf("standalone decision: no bundle expected, got %+v", b)
		}
	}
}
