package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon is an in-process stand-in for the beads daemon that serves the
// REAL bridge.TriggerEngine + bridge.GitLabTriggerSource the gitlab-pipeline
// ref-pattern gate runs through, WITHOUT touching any shared kbeads daemon.
//
// Why a mock daemon and not the live shard daemon (--http-url): EvaluateSource
// ("gitlab") loads EVERY enabled trigger:gitlab bead from whatever daemon it is
// pointed at and evaluates each against the stub GitLab server. Against the
// shared shard daemon that would (a) fan out to unrelated production gitlab
// triggers and (b) leave audit beads on the shared store — cross-talk the
// hermetic escalation-reap-probe / gitlab-prov-probe precedent exists to avoid.
// Here LoadTriggers sees EXACTLY the one trigger bead we seed, and every audit
// CreateBead is a recorded in-memory call, so the probe drives the full
// evaluation→fireAction→recordTriggerAudit loop with ZERO pods and ZERO shared
// daemon state.
//
// It implements the subset of bridge.TriggerBeadClient the notify-action
// gitlab-pipeline path actually exercises (ListBeadsFiltered for LoadTriggers /
// CatchUpKeys / the pause-gate reads, CreateBead for the audit ledger,
// UpdateBeadFields for stampLastChecked, ListProjectBeads for the gate state).
// Every other interface method is a harmless stub — a notify trigger spawns no
// agent, files no task bead, and walks no dependency graph.
type mockDaemon struct {
	mu sync.Mutex

	// triggers is the set of trigger beads LoadTriggers returns (keyed by ID).
	// Seeded once per leg; the engine reads these to build TriggerDefs.
	triggers map[string]*beadsapi.BeadDetail

	// audits records every CreateBead call the audit ledger makes — the load-
	// bearing observable. Each fired match yields exactly one type=event audit
	// bead (recordTriggerAudit), so len(audits) == number of matches that
	// actually FIRED, and each carries the match_key of the pipeline that fired.
	audits []auditRecord

	// fieldUpdates records UpdateBeadFields calls (stampLastChecked writes
	// last_checked back on the trigger). Not asserted, but reflected into the
	// trigger store so a second eval sees the stamped value.
	fieldUpdates []fieldUpdateRecord

	nextBeadID int
}

// auditRecord is one recorded recordTriggerAudit → CreateBead call.
type auditRecord struct {
	ID       string
	Title    string
	Type     string
	Status   string
	Labels   []string
	Fields   map[string]string
	Outcome  string // fields.outcome ("allow" / "error" / "deny")
	MatchKey string // fields.match_key (the pipeline key that fired)
	Action   string // fields.action
	TrigID   string // fields.trigger_id
}

type fieldUpdateRecord struct {
	BeadID string
	Fields map[string]string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{triggers: map[string]*beadsapi.BeadDetail{}}
}

// ── seeding + accessors ────────────────────────────────────────────────────

// seedTrigger inserts a trigger bead into the store LoadTriggers reads.
func (m *mockDaemon) seedTrigger(b *beadsapi.BeadDetail) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	m.triggers[b.ID] = b
}

// auditsForTrigger returns the audits whose fields.trigger_id == trigID.
func (m *mockDaemon) auditsForTrigger(trigID string) []auditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []auditRecord
	for _, a := range m.audits {
		if a.TrigID == trigID {
			out = append(out, a)
		}
	}
	return out
}

// ── bridge.TriggerBeadClient ───────────────────────────────────────────────

func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// The pause-gate reads (Types=["gate"]) and any non-trigger query return
	// empty so nothing is ever gated/paused in the probe.
	if len(q.Types) == 1 && q.Types[0] == "trigger" {
		var out []*beadsapi.BeadDetail
		for _, b := range m.triggers {
			if beadMatchesQuery(b, q) {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}

	// CatchUpKeys (Kinds=["issue"], Labels=["trigger:<id>"]) — no prior beads
	// exist in the probe, so return empty; that keeps every fresh match unseen.
	return &beadsapi.ListBeadsResult{}, nil
}

func (m *mockDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextBeadID++
	id := fmt.Sprintf("audit-%d", m.nextBeadID)
	fields := decodeFields(req.Fields)
	m.audits = append(m.audits, auditRecord{
		ID:       id,
		Title:    req.Title,
		Type:     req.Type,
		Status:   req.Status,
		Labels:   slices.Clone(req.Labels),
		Fields:   fields,
		Outcome:  fields["outcome"],
		MatchKey: fields["match_key"],
		Action:   fields["action"],
		TrigID:   fields["trigger_id"],
	})
	return id, nil
}

func (m *mockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(fields))
	maps.Copy(cp, fields)
	m.fieldUpdates = append(m.fieldUpdates, fieldUpdateRecord{BeadID: beadID, Fields: cp})
	if b := m.triggers[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, cp)
	}
	return nil
}

func (m *mockDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	// No project gate config in the probe → nothing disabled. An empty map
	// means loadGateState leaves projectDisabled/projectSchedulesDisabled nil,
	// so gate.skip() is always false and every seeded trigger is evaluated.
	return map[string]beadsapi.ProjectInfo{}, nil
}

func (m *mockDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.triggers[beadID]; b != nil {
		return b, nil
	}
	return nil, nil
}

// ── remaining bridge.TriggerBeadClient methods — unused by the notify path ──
// A notify-action gitlab-pipeline trigger spawns no agent, files no task bead,
// and walks no dependency graph, so these are never reached in the probe. They
// exist only to satisfy the interface.

func (m *mockDaemon) UpdateBeadFieldsTyped(context.Context, string, map[string]any) error {
	return nil
}

func (m *mockDaemon) SpawnAgent(context.Context, string, string, string, string, string, ...map[string]string) (string, error) {
	return "", fmt.Errorf("SpawnAgent must not be called by a notify-action trigger")
}

func (m *mockDaemon) AddDependency(context.Context, string, string, string, string) error {
	return nil
}

func (m *mockDaemon) AddComment(context.Context, string, string, string) error { return nil }

func (m *mockDaemon) AddLabel(context.Context, string, string) error { return nil }

func (m *mockDaemon) CloseBead(context.Context, string, map[string]string) error { return nil }

func (m *mockDaemon) UpdateBead(context.Context, string, beadsapi.UpdateBeadRequest) error {
	return nil
}

func (m *mockDaemon) MRBeadForTask(context.Context, string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (m *mockDaemon) GetReverseDependencies(context.Context, string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

func (m *mockDaemon) BatchGetDependencies(context.Context, []string, string) (map[string][]beadsapi.Dependency, error) {
	return map[string][]beadsapi.Dependency{}, nil
}

func (m *mockDaemon) BatchGetBeads(context.Context, []string) (map[string]*beadsapi.BeadDetail, error) {
	return map[string]*beadsapi.BeadDetail{}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

// beadMatchesQuery applies the subset of ListBeadsQuery filters LoadTriggers
// uses: type (already checked by the caller) + labels-AND + status.
func beadMatchesQuery(b *beadsapi.BeadDetail, q beadsapi.ListBeadsQuery) bool {
	if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
		return false
	}
	for _, want := range q.Labels {
		if !slices.Contains(b.Labels, want) {
			return false
		}
	}
	return true
}
