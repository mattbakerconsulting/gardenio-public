package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon is an in-process stand-in for the beads daemon that RECORDS the
// writes the grafana-bridge sync + read-back paths make (CreateBead,
// UpdateBeadFields, AddDependency, AddLabel, AddComment) instead of persisting
// them to a real daemon. It satisfies bridge.DashboardDaemon — the ONLY daemon
// surface the DashboardStore (and thus GrafanaSync + GrafanaReadback) touches —
// so the probe exercises the full sync + conflict-policy logic with ZERO pods
// and ZERO shared daemon state, following the cmd/gitlab-prov-probe /
// cmd/jira-conv-probe precedent.
//
// It intentionally mirrors the semantics of fakeDashboardDaemon in
// grafana_sync_test.go (a same-package test fake, hence not importable here):
// CreateBead mints ka-fakeN ids and parses the Fields JSON into a string map;
// AddDependency indexes by the depends-on target so GetReverseDependencies (the
// child→parent model-doc lookup) resolves; UpdateBeadFields merges into the
// live bead so a later GetBead observes the stamped provenance.
type mockDaemon struct {
	mu sync.Mutex

	// beads is the bead store keyed by ID (dashboard, model/conflict docs,
	// decision beads). ListBeadsFiltered scans it directly for FindByUID.
	beads map[string]*beadsapi.BeadDetail
	// deps is the forward dependency index keyed by the depends-on target, so
	// GetReverseDependencies(parent) returns the rows whose DependsOnID == parent
	// (the child→parent parent-child edges the model/conflict docs carry).
	deps map[string][]beadsapi.Dependency
	// comments records AddComment texts per bead — the provenance notes the
	// conflict-policy branches post.
	comments map[string][]commentRecord
	// created records every CreateBead call (dashboard beads, model/conflict
	// docs, decision beads) for leg assertions.
	created []createRecord

	nextID int
}

type commentRecord struct {
	Author string
	Text   string
}

type createRecord struct {
	ID     string
	Title  string
	Type   string
	Kind   string
	Labels []string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		deps:     map[string][]beadsapi.Dependency{},
		comments: map[string][]commentRecord{},
	}
}

// ── recorded-call accessors ──────────────────────────────────────────────────

func (m *mockDaemon) beadField(beadID, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.beads[beadID]; b != nil {
		return b.Fields[key]
	}
	return ""
}

func (m *mockDaemon) commentsFor(beadID string) []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.comments[beadID])
}

// createdOfType returns every CreateBead call whose Type matches t.
func (m *mockDaemon) createdOfType(t string) []createRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []createRecord
	for _, c := range m.created {
		if c.Type == t {
			out = append(out, c)
		}
	}
	return out
}

// hasConflictArchive reports whether any doc bead carries the conflict-archive
// label (the losing-side model preserved for audit).
func (m *mockDaemon) hasConflictArchive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Type != "doc" {
			continue
		}
		if slices.Contains(b.Labels, "grafana:dashboard-conflict") {
			return true
		}
	}
	return false
}

// depsInto returns the forward dependency rows pointing at dependsOnID (i.e.
// GetReverseDependencies(dependsOnID)) — used to assert the blocks edge a manual
// conflict decision adds.
func (m *mockDaemon) depsInto(dependsOnID string) []beadsapi.Dependency {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.deps[dependsOnID])
}

// ── bridge.DashboardDaemon ────────────────────────────────────────────────────

func (m *mockDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("ka-fake%d", m.nextID)
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		var raw map[string]any
		_ = json.Unmarshal(req.Fields, &raw)
		for k, v := range raw {
			fields[k] = fmt.Sprintf("%v", v)
		}
	}
	status := req.Status
	if status == "" {
		status = "open"
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Kind:   req.Kind,
		Status: status,
		Labels: slices.Clone(req.Labels),
		Fields: fields,
	}
	m.created = append(m.created, createRecord{
		ID: id, Title: req.Title, Type: req.Type, Kind: req.Kind, Labels: slices.Clone(req.Labels),
	})
	return id, nil
}

func (m *mockDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	cp := *b
	cp.Fields = maps.Clone(b.Fields)
	cp.Labels = slices.Clone(b.Labels)
	return &cp, nil
}

func (m *mockDaemon) UpdateBeadDescription(_ context.Context, id, desc string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[id]; ok {
		b.Description = desc
	}
	return nil
}

func (m *mockDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return fmt.Errorf("not found: %s", id)
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	maps.Copy(b.Fields, fields)
	return nil
}

func (m *mockDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Index by the depends-on target so GetReverseDependencies (rows where
	// depends_on_id == queried id) finds it — the model/conflict docs are linked
	// child→parent, so the doc is a reverse dependency of the dashboard.
	m.deps[dependsOnID] = append(m.deps[dependsOnID], beadsapi.Dependency{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	return nil
}

func (m *mockDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.deps[beadID]), nil
}

func (m *mockDaemon) AddLabel(_ context.Context, beadID, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		b.Labels = append(b.Labels, label)
	}
	return nil
}

func (m *mockDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments[beadID] = append(m.comments[beadID], commentRecord{Author: author, Text: text})
	return nil
}

func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		match := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}
