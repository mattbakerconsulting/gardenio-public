package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon is an in-process stand-in for the beads daemon that RECORDS the
// writes the E-PROV forward-provenance paths make (UpsertMergeRequestBead,
// AddDependency, UpdateBeadFields, AddComment, CreateBead) instead of persisting
// them to a real daemon. It satisfies every bridge interface the probe wires:
//
//   - GitLabBeadClient        (GitLabSync host-scope + webhook merge path)
//   - UpstreamAcceptanceBeadClient (FW-3 acceptance reconciler)
//   - GitLabUpstreamBeadClient (FW-2 backstop poller)
//   - mrBacklinkDaemon        (FW-6 peel-backlink reconciler; its interface is
//     package-private but its methods are exported, so a
//     package-main type structurally satisfies it — the
//     same way beadsapi.Client does in cmd/gitlab-bridge)
//
// Because every write is a RECORDED CALL against an in-memory store and no real
// type=agent bead is ever created, no controller/reconciler anywhere observes it,
// so the probe exercises the full provenance logic with ZERO pods and ZERO shared
// daemon state — the trigger-probe / jira-conv-probe precedent.
//
// It intentionally mirrors the semantics of mockGitLabDaemon in
// gitlab_sync_test.go: UpsertMergeRequestBead keys on mr_url, preserves an
// existing MR bead's labels (so a pre-seeded upstream-mirror bead keeps its
// upstream:true marker across the close), and stamps mr_outcome on close.
type mockDaemon struct {
	mu sync.Mutex

	// beads is the bead store keyed by ID (task, merge-request, dark-work, …).
	// ListBeadsFiltered resolves label-AND queries (jira:<key>, etc.) by scanning
	// it directly, so no secondary label index is needed.
	beads map[string]*beadsapi.BeadDetail

	// mrUpserts records every UpsertMergeRequestBead call (with the resolved id).
	mrUpserts []mrUpsertRecord
	// deps records every AddDependency call (produces / triggered-by edges).
	deps []depRecord
	// fieldUpdates records every UpdateBeadFields call (acceptance stamps + backlinks).
	fieldUpdates []fieldUpdateRecord
	// comments records every AddComment call (acceptance notes).
	comments []commentRecord
	// creates records every CreateBead call (unlinked-MR beads, etc.).
	creates []createRecord
	// closed records every CloseBead call — used to prove the reconciler NEVER
	// closes a peel-request or a dark work bead.
	closed map[string]bool
	// listOpenMRLabels records the label passed to every ListOpenMergeRequestBeads
	// call so the backstop-poller leg can assert the upstream:true scope.
	listOpenMRLabels []string

	nextMRBeadID int
}

type mrUpsertRecord struct {
	Fields beadsapi.MergeRequestBeadFields
	Closed bool
	BeadID string
}

type depRecord struct {
	BeadID      string
	DependsOnID string
	DepType     string
	CreatedBy   string
}

type fieldUpdateRecord struct {
	BeadID string
	Fields map[string]string
}

type commentRecord struct {
	BeadID string
	Author string
	Text   string
}

type createRecord struct {
	ID     string
	Title  string
	Type   string
	Labels []string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		beads:  map[string]*beadsapi.BeadDetail{},
		closed: map[string]bool{},
	}
}

// ── seeding helpers ───────────────────────────────────────────────────────────

// seedBead inserts a bead into the store.
func (m *mockDaemon) seedBead(b *beadsapi.BeadDetail) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	m.beads[b.ID] = b
}

// ── recorded-call accessors ───────────────────────────────────────────────────

func (m *mockDaemon) getMRUpserts() []mrUpsertRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.mrUpserts)
}

func (m *mockDaemon) getDeps() []depRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.deps)
}

func (m *mockDaemon) updatesFor(beadID string) []fieldUpdateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []fieldUpdateRecord
	for _, u := range m.fieldUpdates {
		if u.BeadID == beadID {
			out = append(out, u)
		}
	}
	return out
}

func (m *mockDaemon) commentsFor(beadID string) []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []commentRecord
	for _, c := range m.comments {
		if c.BeadID == beadID {
			out = append(out, c)
		}
	}
	return out
}

// beadField returns the current value of a bead field (after any UpdateBeadFields
// / upsert writes reflected into the store), or "".
func (m *mockDaemon) beadField(beadID, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.beads[beadID]; b != nil {
		return b.Fields[key]
	}
	return ""
}

func (m *mockDaemon) beadStatus(beadID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.beads[beadID]; b != nil {
		return b.Status
	}
	return ""
}

func (m *mockDaemon) wasClosed(beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[beadID]
}

func (m *mockDaemon) listOpenMRScopes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.listOpenMRLabels)
}

// ── GitLabBeadClient ──────────────────────────────────────────────────────────

func (m *mockDaemon) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type != "" && b.Type != "task" {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (m *mockDaemon) ListTaskBeadsAll(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	return m.ListTaskBeads(ctx)
}

func (m *mockDaemon) ListTaskBeadsForMR(ctx context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	if mrURL == "" {
		return nil, nil
	}
	all, _ := m.ListTaskBeads(ctx)
	var out []*beadsapi.BeadDetail
	for _, b := range all {
		if b.Fields["mr_url"] == mrURL {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *mockDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *mockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(fields))
	maps.Copy(cp, fields)
	m.fieldUpdates = append(m.fieldUpdates, fieldUpdateRecord{BeadID: beadID, Fields: cp})
	if b := m.beads[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, cp)
	}
	return nil
}

func (m *mockDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, commentRecord{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (m *mockDaemon) UpsertMergeRequestBead(_ context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Find an existing MR bead by mr_url (keyed identity — mirrors the real
	// client + gitlab_sync_test.go's mock). A pre-seeded upstream-mirror bead is
	// found here so its labels (upstream:true) survive the close.
	var existing *beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type == "merge-request" && b.Fields["mr_url"] == f.MRURL {
			existing = b
			break
		}
	}
	var beadID string
	if existing == nil {
		m.nextMRBeadID++
		beadID = fmt.Sprintf("mr-bead-%d", m.nextMRBeadID)
		fields := map[string]string{}
		mergeMRFields(fields, f)
		status := "open"
		if closed {
			status = "closed"
			if _, ok := fields["mr_outcome"]; !ok {
				fields["mr_outcome"] = inferOutcome(f.MRState)
			}
		}
		m.beads[beadID] = &beadsapi.BeadDetail{
			ID:     beadID,
			Title:  f.Title,
			Type:   "merge-request",
			Status: status,
			Labels: slices.Clone(f.Labels),
			Fields: fields,
		}
	} else {
		beadID = existing.ID
		mergeMRFields(existing.Fields, f)
		if closed && existing.Status != "closed" {
			existing.Status = "closed"
			if _, ok := existing.Fields["mr_outcome"]; !ok {
				existing.Fields["mr_outcome"] = inferOutcome(f.MRState)
			}
		}
	}
	m.mrUpserts = append(m.mrUpserts, mrUpsertRecord{Fields: f, Closed: closed, BeadID: beadID})
	return beadID, nil
}

func (m *mockDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps = append(m.deps, depRecord{BeadID: beadID, DependsOnID: dependsOnID, DepType: depType, CreatedBy: createdBy})
	return nil
}

// MRBeadForTask walks recorded produces(task→MR) deps to the type=merge-request
// bead, mirroring beadsapi.Client.MRBeadForTask.
func (m *mockDaemon) MRBeadForTask(_ context.Context, taskBeadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deps {
		if d.BeadID != taskBeadID || d.DepType != "produces" {
			continue
		}
		if b := m.beads[d.DependsOnID]; b != nil && b.Type == "merge-request" {
			return b, nil
		}
	}
	return nil, nil
}

// ── UpstreamAcceptanceBeadClient ──────────────────────────────────────────────

func (m *mockDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.beads[beadID]; b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("bead %q not found", beadID)
}

// ProducingTaskBeadsForMR walks the reverse produces edge from an MR bead to the
// task bead(s) that produced it — for an upstream-mirror bead these are the
// peel-request beads. Mirrors beadsapi.Client.ProducingTaskBeadsForMR.
func (m *mockDaemon) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, d := range m.deps {
		// produces(task→MR): the MR bead is the DependsOnID; the producer is BeadID.
		if d.DepType == "produces" && d.DependsOnID == mrBeadID {
			if b := m.beads[d.BeadID]; b != nil && !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// ── GitLabUpstreamBeadClient ──────────────────────────────────────────────────

func (m *mockDaemon) ListOpenMergeRequestBeads(_ context.Context, label string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listOpenMRLabels = append(m.listOpenMRLabels, label)
	var out []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type != "merge-request" {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if label != "" && !slices.Contains(b.Labels, label) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// ── mrBacklinkDaemon (FW-6) ───────────────────────────────────────────────────

// MRBeadByURL returns the canonical type=merge-request bead for a URL, or nil.
func (m *mockDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Type == "merge-request" && b.Fields["mr_url"] == mrURL {
			return b, nil
		}
	}
	return nil, nil
}

func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, b := range m.beads {
		if !matchesQuery(b, q) {
			continue
		}
		if !seen[b.ID] {
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (m *mockDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextMRBeadID++
	id := fmt.Sprintf("bd-created-%d", m.nextMRBeadID)
	m.creates = append(m.creates, createRecord{ID: id, Title: req.Title, Type: req.Type, Labels: req.Labels})
	return id, nil
}

func (m *mockDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed[beadID] = true
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// inferOutcome maps a terminal GitLab MR state into the mr_outcome enum, matching
// beadsapi.inferOutcome / bridge.inferUpstreamOutcome.
func inferOutcome(state string) string {
	if state == "merged" {
		return "merged"
	}
	return "closed"
}

// mergeMRFields projects MergeRequestBeadFields into a bead fields map, matching
// beadsapi.mrFieldsMap closely enough for the probe's assertions (mr_url,
// mr_state, mr_outcome, mr_merged, source/target branch, title, iid).
func mergeMRFields(out map[string]string, f beadsapi.MergeRequestBeadFields) {
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("mr_url", f.MRURL)
	set("mr_iid", f.MRIID)
	set("gitlab_mr_iid", f.MRIID)
	set("project_path", f.ProjectPath)
	set("source_branch", f.SourceBranch)
	set("target_branch", f.TargetBranch)
	set("mr_state", f.MRState)
	set("mr_outcome", f.MROutcome)
	set("mr_title", f.Title)
	set("author", f.Author)
	if out["operated_by"] == "" {
		set("operated_by", f.OperatedBy)
	}
	switch f.MRState {
	case "merged":
		out["mr_merged"] = "true"
	case "closed":
		out["mr_merged"] = "false"
	}
}

// matchesQuery applies the subset of ListBeadsQuery filters the FW-6 backlink
// path uses (types + labels-AND). Enough for bestActionableForJiraKey.
func matchesQuery(b *beadsapi.BeadDetail, q beadsapi.ListBeadsQuery) bool {
	if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
		return false
	}
	for _, want := range q.Labels {
		if !slices.Contains(b.Labels, want) {
			return false
		}
	}
	return true
}
