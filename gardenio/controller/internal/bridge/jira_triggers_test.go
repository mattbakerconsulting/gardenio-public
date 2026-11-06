package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ensurePastLastChecked mutates `b` in place to set `last_checked` to a past
// timestamp when it is absent. This bypasses the engine's runtime-bootstrap
// guard (which suppresses first-evaluation firing for brand-new triggers) so
// unit tests exercise steady-state behavior by default. Tests that specifically
// want to exercise the bootstrap path should explicitly set an empty
// `last_checked` override after creation.
func ensurePastLastChecked(b *beadsapi.BeadDetail) {
	if b == nil || b.Type != "trigger" {
		return
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	if _, ok := b.Fields["last_checked"]; !ok {
		b.Fields["last_checked"] = time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	}
}

// mockTriggerDaemon implements TriggerBeadClient for testing.
type mockTriggerDaemon struct {
	mu                     sync.Mutex
	beads                  []*beadsapi.BeadDetail // returned by ListBeadsFiltered
	createdBeads           []beadsapi.CreateBeadRequest
	createOperators        []string // per-CreateBead OperatorFromContext snapshot
	createSystemOperators  []string // per-CreateBead SystemOperatorFromContext snapshot
	createErr              error    // when set, CreateBead fails with this error
	updatedIDs             []string
	updatedFields          []map[string]string          // parallel to updatedIDs; per-call field map
	updatedTyped           []map[string]any             // per-UpdateBeadFieldsTyped call, raw JSON-typed values
	updatedBeadIDs         []string                     // per-UpdateBead record (wedged-trigger heal)
	updatedBeadReqs        []beadsapi.UpdateBeadRequest // parallel to updatedBeadIDs
	spawnedNames           []string
	spawnedRoles           []string
	spawnedTaskIDs         []string            // per-SpawnAgent taskID snapshot
	spawnedFields          []map[string]string // per-SpawnAgent merged extraFields snapshot
	spawnOperators         []string            // per-SpawnAgent OperatorFromContext snapshot
	spawnErr               error               // when set, SpawnAgent fails with this error
	lastPrompt             string
	projects               map[string]beadsapi.ProjectInfo // returned by ListProjectBeads
	addedDeps              []triggerMockDep                // per-AddDependency record
	commentedIDs           []string                        // per-AddComment record (gardener stop)
	commentSystemOperators []string                        // per-AddComment SystemOperatorFromContext snapshot
	closedIDs              []string                        // per-CloseBead record (gardener stop)
	closedFields           []map[string]string             // parallel to closedIDs; per-CloseBead field map
	addedLabels            []triggerMockLabel              // per-AddLabel record (gardener escalate flag)
	addLabelErr            error                           // when set, AddLabel fails with this error
	updateFieldsErr        error                           // when set, UpdateBeadFields fails with this error
	updateFieldsTypedErr   error                           // when set, UpdateBeadFieldsTyped fails with this error
	// listBeadsHook, when non-nil, is consulted at the start of every
	// ListBeadsFiltered call (under the mock lock) with the 1-based call count
	// and the query. A non-nil error is returned WITHOUT serving any beads, so
	// tests can fail the first N list calls and then succeed — the fake-daemon
	// shape the startup catch-up and heal-list retries are specified against
	// (gd-niQPoeR7ja).
	listBeadsHook  func(n int, q beadsapi.ListBeadsQuery) error
	listBeadsCalls int
	// listBeadsErrForParent, when non-nil, makes ListBeadsFiltered return an
	// error for any query whose Parent is in the map — WITHOUT breaking the
	// engine's own unscoped trigger-listing queries (Parent==""). Used by the
	// gardener block-on-MR fail-safety tests (ka-0000007387) to exercise the
	// subtree-read-error branches (gardenerHasUnmergedTrackedMR /
	// gardenerFrontierFullyWedged) that must fail toward NOT parking / waking.
	listBeadsErrForParent map[string]error
	// mrBeadsByTask maps a producing bead ID to its type=merge-request bead,
	// consulted by MRBeadForTask. Populated by gardener block-on-MR tests
	// (ka-0000007387); nil for every other test, where MRBeadForTask returns nil
	// exactly as the legacy stub did.
	mrBeadsByTask map[string]*beadsapi.BeadDetail
	// mrBeadsByURL maps a canonical MR URL to its type=merge-request mirror bead,
	// consulted by MRBeadByURL. Populated by the specific_mr_merged
	// (--stop-on-mr-merged) tests (ka-0000005598); nil for every other test, where
	// MRBeadByURL returns nil.
	mrBeadsByURL map[string]*beadsapi.BeadDetail
	// parentOf maps a child bead ID to its parent bead ID (parent-child). When
	// non-nil, ListBeadsFiltered honors q.Parent by transitive-subtree membership
	// (ka-0000007387 proactive-wedge test needs a faithful actionable-count of 0
	// under an OPEN epic). Nil for every legacy test, where q.Parent is ignored
	// exactly as before — so the existing dormancy fixtures (which set no
	// parentage) are unaffected.
	parentOf map[string]string
}

// isDescendantOf reports whether child is in the transitive parent-child subtree
// under parent, per the mock's parentOf map. A bead is NOT its own descendant.
func (m *mockTriggerDaemon) isDescendantOf(child, parent string) bool {
	seen := map[string]bool{}
	cur := m.parentOf[child]
	for cur != "" && !seen[cur] {
		if cur == parent {
			return true
		}
		seen[cur] = true
		cur = m.parentOf[cur]
	}
	return false
}

// triggerMockLabel captures a single AddLabel call for assertions in tests that
// care about the real-label escalate flag (gardener-stuck-escalate) landing via
// the label endpoint rather than the rejected fields-blob shape (ka-0000007275).
type triggerMockLabel struct {
	BeadID string
	Label  string
}

// triggerMockDep captures a single AddDependency call for assertions in tests that
// care about the typed edges written by trigger actions (e.g. `triggered-by`
// lineage from the apply_formula action).
type triggerMockDep struct {
	BeadID      string
	DependsOnID string
	DepType     string
	CreatedBy   string
}

func (m *mockTriggerDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listBeadsCalls++
	if m.listBeadsHook != nil {
		if err := m.listBeadsHook(m.listBeadsCalls, q); err != nil {
			return nil, err
		}
	}

	// Targeted subtree-read-error injection (ka-0000007387 fail-safety tests):
	// error ONLY for a query scoped to a specific parent, leaving the engine's
	// unscoped trigger-listing queries (Parent=="") unaffected.
	if q.Parent != "" && m.listBeadsErrForParent != nil {
		if err := m.listBeadsErrForParent[q.Parent]; err != nil {
			return nil, err
		}
	}

	// Auto-inject a past last_checked for every trigger bead so tests don't
	// have to individually opt out of the runtime-bootstrap guard.
	for _, b := range m.beads {
		ensurePastLastChecked(b)
	}

	// Filter beads by type if requested.
	var filtered []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if len(q.Types) > 0 {
			match := false
			for _, t := range q.Types {
				if b.Type == t {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		// Filter by status if requested. Faithful to the daemon so callers
		// that query a specific status (e.g. the schedule source's wedged-
		// trigger heal querying status=in_progress, ka-0000006681) only see
		// matching beads rather than the whole fixture set. A bead with an
		// empty Status matches any status filter (legacy fixtures predate the
		// field).
		if len(q.Statuses) > 0 && b.Status != "" {
			match := false
			for _, st := range q.Statuses {
				if b.Status == st {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		// Filter by labels if requested.
		if len(q.Labels) > 0 {
			match := true
			for _, reqLabel := range q.Labels {
				found := false
				for _, beadLabel := range b.Labels {
					if beadLabel == reqLabel {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		// Filter by custom-field equality if requested (fields->>'key' = 'value').
		// Faithful to the daemon so callers that gate on a field predicate — e.g.
		// the live-concurrency lease querying spawned_by:trigger:<id>
		// (ka-0000003120) — only see matching beads.
		if len(q.Fields) > 0 {
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
		}
		// Filter by transitive parent-child subtree membership, but ONLY when the
		// test opted in by populating parentOf (ka-0000007387). Legacy tests set
		// no parentage, so q.Parent stays ignored for them exactly as before.
		if q.Parent != "" && m.parentOf != nil && !m.isDescendantOf(b.ID, q.Parent) {
			continue
		}
		filtered = append(filtered, b)
	}
	return &beadsapi.ListBeadsResult{Beads: filtered, Total: len(filtered)}, nil
}

func (m *mockTriggerDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return "", m.createErr
	}
	m.createdBeads = append(m.createdBeads, req)
	m.createOperators = append(m.createOperators, beadsapi.OperatorFromContext(ctx))
	m.createSystemOperators = append(m.createSystemOperators, beadsapi.SystemOperatorFromContext(ctx))
	// Return a unique ID per call so molecule + step beads can be
	// distinguished by deps assertions in apply_formula tests.
	id := fmt.Sprintf("gd-trigger-bead-%d", len(m.createdBeads))
	return id, nil
}

func (m *mockTriggerDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateFieldsErr != nil {
		return m.updateFieldsErr
	}
	m.updatedIDs = append(m.updatedIDs, beadID)
	// Defensive copy so callers mutating their input map after the call do
	// not retroactively change recorded test state.
	cp := make(map[string]string, len(fields))
	maps.Copy(cp, fields)
	m.updatedFields = append(m.updatedFields, cp)
	return nil
}

// UpdateBeadFieldsTyped records into the same updatedIDs/updatedFields state as
// the string variant, stringifying values so existing assertions keep working.
// gardenerPostFire routes its run-counter mirror through here (ka-0000007976).
func (m *mockTriggerDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateFieldsTypedErr != nil {
		return m.updateFieldsTypedErr
	}
	m.updatedIDs = append(m.updatedIDs, beadID)
	cp := make(map[string]string, len(fields))
	raw := make(map[string]any, len(fields))
	for k, v := range fields {
		cp[k] = fmt.Sprintf("%v", v)
		raw[k] = v
	}
	m.updatedFields = append(m.updatedFields, cp)
	// Preserve the raw JSON-typed values too, so a test can assert an
	// integer-typed field (runs_spawned) is sent as a number, not a string —
	// the type:trigger schema mismatch that froze RunsSpawned (ka-0000007976).
	m.updatedTyped = append(m.updatedTyped, raw)
	return nil
}

func (m *mockTriggerDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedBeadIDs = append(m.updatedBeadIDs, beadID)
	m.updatedBeadReqs = append(m.updatedBeadReqs, req)
	// Reflect the mutation onto the in-memory bead so a heal that releases a
	// wedged in_progress trigger to open is visible to the subsequent
	// LoadTriggers query in the same call. Priority + Fields are reflected too so
	// a sweep that reads-then-writes a bead's priority (the stale-revival-review
	// escalation, ka-0000005280) sees its own prior write on the next run and is
	// idempotent — faithful to the daemon's read-after-write.
	for _, b := range m.beads {
		if b != nil && b.ID == beadID {
			if req.Status != nil {
				b.Status = *req.Status
			}
			if req.Assignee != nil {
				b.Assignee = *req.Assignee
			}
			if req.Priority != nil {
				b.Priority = *req.Priority
			}
			if len(req.Fields) > 0 {
				if b.Fields == nil {
					b.Fields = map[string]string{}
				}
				for k, v := range req.Fields {
					if v == nil {
						delete(b.Fields, k)
						continue
					}
					b.Fields[k] = fmt.Sprintf("%v", v)
				}
			}
		}
	}
	return nil
}

func (m *mockTriggerDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b != nil && b.ID == beadID {
			return b, nil
		}
	}
	return nil, fmt.Errorf("bead %s not found", beadID)
}

func (m *mockTriggerDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.projects == nil {
		return map[string]beadsapi.ProjectInfo{}, nil
	}
	out := make(map[string]beadsapi.ProjectInfo, len(m.projects))
	for k, v := range m.projects {
		out[k] = v
	}
	return out, nil
}

func (m *mockTriggerDaemon) AddComment(ctx context.Context, beadID, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commentedIDs = append(m.commentedIDs, beadID)
	m.commentSystemOperators = append(m.commentSystemOperators, beadsapi.SystemOperatorFromContext(ctx))
	return nil
}

func (m *mockTriggerDaemon) AddLabel(_ context.Context, beadID, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addLabelErr != nil {
		return m.addLabelErr
	}
	m.addedLabels = append(m.addedLabels, triggerMockLabel{BeadID: beadID, Label: label})
	// Reflect onto the in-memory bead so a subsequent GetBead sees the label,
	// mirroring the daemon.
	for _, b := range m.beads {
		if b != nil && b.ID == beadID && !slices.Contains(b.Labels, label) {
			b.Labels = append(b.Labels, label)
		}
	}
	return nil
}

// labelsAddedFor returns the labels stamped on beadID via AddLabel calls.
func (m *mockTriggerDaemon) labelsAddedFor(beadID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, l := range m.addedLabels {
		if l.BeadID == beadID {
			out = append(out, l.Label)
		}
	}
	return out
}

func (m *mockTriggerDaemon) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closedIDs = append(m.closedIDs, beadID)
	m.closedFields = append(m.closedFields, fields)
	return nil
}

func (m *mockTriggerDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addedDeps = append(m.addedDeps, triggerMockDep{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		DepType:     depType,
		CreatedBy:   createdBy,
	})
	return nil
}

// MRBeadForTask returns the type=merge-request bead produced by taskID when the
// test populated mrBeadsByTask (gardener block-on-MR tests, ka-0000007387);
// otherwise nil, preserving the legacy no-op behavior every other test relies on.
func (m *mockTriggerDaemon) MRBeadForTask(_ context.Context, taskID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mrBeadsByTask == nil {
		return nil, nil
	}
	return m.mrBeadsByTask[taskID], nil
}

// MRBeadByURL resolves a canonical MR URL to its type=merge-request mirror bead.
// It satisfies the gardenerMRURLDaemon capability the specific_mr_merged stop kind
// (--stop-on-mr-merged, ka-0000005598) needs on the live gardener fire path.
// Returns nil when mrBeadsByURL is unpopulated, preserving the legacy behavior
// every other test relies on.
func (m *mockTriggerDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mrBeadsByURL == nil {
		return nil, nil
	}
	return m.mrBeadsByURL[mrURL], nil
}

// GetReverseDependencies returns the recorded dependency edges (via AddDependency)
// whose DependsOnID is beadID — i.e. every bead that depends ON beadID. Used by
// the pouring-stall sweep to find a trigger's `triggered-by` molecules.
func (m *mockTriggerDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []beadsapi.Dependency
	for _, d := range m.addedDeps {
		if d.DependsOnID == beadID {
			out = append(out, beadsapi.Dependency{BeadID: d.BeadID, DependsOnID: d.DependsOnID, Type: d.DepType})
		}
	}
	return out, nil
}

// BatchGetDependencies returns forward or reverse edges for each requested ID
// from the recorded AddDependency edges. direction "reverse" returns edges where
// DependsOnID == id (children of id, for parent-child); "forward" returns edges
// where BeadID == id.
func (m *mockTriggerDaemon) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		for _, d := range m.addedDeps {
			match := d.BeadID == id
			if direction == "reverse" {
				match = d.DependsOnID == id
			}
			if match {
				out[id] = append(out[id], beadsapi.Dependency{BeadID: d.BeadID, DependsOnID: d.DependsOnID, Type: d.DepType})
			}
		}
	}
	return out, nil
}

// BatchGetBeads returns the seeded beads matching the requested IDs.
func (m *mockTriggerDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, b := range m.beads {
		if b != nil && want[b.ID] {
			out[b.ID] = b
		}
	}
	return out, nil
}

func (m *mockTriggerDaemon) SpawnAgent(ctx context.Context, agentName, _, taskID, role, prompt string, extraFields ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spawnErr != nil {
		return "", m.spawnErr
	}
	m.spawnedNames = append(m.spawnedNames, agentName)
	m.spawnedRoles = append(m.spawnedRoles, role)
	m.spawnedTaskIDs = append(m.spawnedTaskIDs, taskID)
	merged := map[string]string{}
	for _, ef := range extraFields {
		maps.Copy(merged, ef)
	}
	m.spawnedFields = append(m.spawnedFields, merged)
	m.spawnOperators = append(m.spawnOperators, beadsapi.OperatorFromContext(ctx))
	m.lastPrompt = prompt
	return "gd-agent-1", nil
}

func newTestJiraServer(issues []JiraIssue) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"issues":     issues,
			"total":      len(issues),
			"maxResults": 50,
			"startAt":    0,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestJiraTriggerEvaluator_SpawnAgent(t *testing.T) {
	// Set up a fake JIRA server returning one issue.
	issues := []JiraIssue{
		{
			Key: "FSM-101",
			Fields: JiraIssueFields{
				Summary: "Customer cannot log in",
				Status:  &JiraNamedRef{Name: "Escalate to Engineering"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-1",
				Type:   "trigger",
				Status: "open",
				Title:  "Triage FSM escalations",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = FSM AND status = 'Escalate to Engineering'",
					"action":         "spawn_agent",
					"role":           "crew",
					"project":        "monorepo",
					"prompt":         "Triage {{jira_key}}: {{jira_summary}}",
					"enabled":        "true",
					"max_concurrent": "3",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Should have created a task bead.
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Title != "[FSM-101] Customer cannot log in" {
		t.Errorf("bead title: %s", daemon.createdBeads[0].Title)
	}

	// Should have spawned an agent.
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedRoles[0] != "crew" {
		t.Errorf("agent role: %s", daemon.spawnedRoles[0])
	}
	if daemon.lastPrompt != "Triage FSM-101: Customer cannot log in" {
		t.Errorf("agent prompt: %s", daemon.lastPrompt)
	}
}

// jiraReporterSpawnDaemon returns a mock trigger daemon with one JQL spawn
// trigger whose author is a sentinel (no human in lineage) — the shape that
// produced the fleet:jira-bridge auto-spawn agents in ka-0000001781.
func jiraReporterSpawnTrigger() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-trigger-reporter",
		Type:   "trigger",
		Status: "open",
		Title:  "Auto-spawn crew on new FSM tickets",
		Labels: []string{"trigger:jira", "project:monorepo"},
		// The trigger bead carries NO human author: created_by is an agent and
		// operated_by is the bridge fleet sentinel — exactly the live prod shape.
		CreatedBy:  "thread-1783455308-220259",
		OperatedBy: "fleet:jira-bridge",
		Fields: map[string]string{
			"jql":            "project = FSM AND created >= -2d",
			"action":         "spawn_agent",
			"role":           "crew",
			"project":        "monorepo",
			"prompt":         "Work {{jira_key}}",
			"enabled":        "true",
			"max_concurrent": "3",
		},
	}
}

// TestJiraTriggerEvaluator_AttributesAutoSpawnToReporter is the ka-0000001781
// regression: a sentinel-authored JQL auto-spawn must attribute the task +
// agent to the ticket REPORTER (resolved to a fleet human via the
// UserDirectory), not fleet:jira-bridge. Without the reporter resolver the
// spawn context carries no human and the daemon auto-attributes fleet.
func TestJiraTriggerEvaluator_AttributesAutoSpawnToReporter(t *testing.T) {
	issues := []JiraIssue{
		{
			Key: "FSM-900",
			Fields: JiraIssueFields{
				Summary:  "Login broken",
				Reporter: &JiraUser{DisplayName: "Jane Doe", AccountID: "acct-jane"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{jiraReporterSpawnTrigger()}}
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
		"acct-jane": {BeadID: "gd-user-jane", SlackID: "U0JANE", Email: "jane@example.com"},
	}}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
		Users:  users,
	})
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	// The spawn context — and thus the agent bead's operated_by — must be the
	// resolved human, not the fleet sentinel.
	if got := daemon.spawnOperators[0]; got != "slack:U0JANE" {
		t.Errorf("spawn operator = %q, want slack:U0JANE (the reporter)", got)
	}
	if got := daemon.spawnedFields[0]["operated_by"]; got != "slack:U0JANE" {
		t.Errorf("agent operated_by = %q, want slack:U0JANE", got)
	}
	// Every bead created during the fire (the task bead AND the trigger-audit
	// bead recordTriggerAudit writes) must be attributed to the same human, so
	// no downstream write dead-ends on a missing operator.
	if len(daemon.createOperators) == 0 {
		t.Fatalf("expected at least one created bead")
	}
	for i, op := range daemon.createOperators {
		if op != "slack:U0JANE" {
			t.Errorf("created-bead[%d] operator = %q, want slack:U0JANE", i, op)
		}
	}
	// The reporter identity keys must be carried on the task bead fields (the
	// first created bead) so the resolution is auditable / re-derivable.
	var fields map[string]string
	if err := json.Unmarshal(daemon.createdBeads[0].Fields, &fields); err != nil {
		t.Fatalf("unmarshalling bead fields: %v", err)
	}
	if fields["jira_reporter_id"] != "acct-jane" {
		t.Errorf("jira_reporter_id = %q, want acct-jane", fields["jira_reporter_id"])
	}
	if fields["operated_by"] != "slack:U0JANE" {
		t.Errorf("bead operated_by field = %q, want slack:U0JANE", fields["operated_by"])
	}
}

// TestJiraTriggerEvaluator_ReporterViaEmail verifies the email lookup key path:
// Jira Cloud user beads carry email (not the opaque accountId), so an
// email-only reporter still resolves to oidc:<email> when there is no SlackID.
func TestJiraTriggerEvaluator_ReporterViaEmail(t *testing.T) {
	issues := []JiraIssue{
		{
			Key: "FSM-901",
			Fields: JiraIssueFields{
				Summary:  "Slow dashboard",
				Reporter: &JiraUser{DisplayName: "Bob", AccountID: "acct-bob", EmailAddress: "bob@acme.example"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{jiraReporterSpawnTrigger()}}
	users := &fakeJiraUserDir{byEmail: map[string]*UserIdentity{
		"bob@acme.example": {BeadID: "gd-user-bob", Email: "bob@acme.example"},
	}}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
		Users:  users,
	})
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnOperators) != 1 || daemon.spawnOperators[0] != "oidc:bob@acme.example" {
		t.Errorf("spawn operators = %v, want [oidc:bob@acme.example]", daemon.spawnOperators)
	}
}

// TestJiraTriggerEvaluator_UnresolvedReporterFallsBackToDefault verifies the
// precedence: when the reporter cannot be mapped to a fleet human, the engine
// still attributes to its DefaultOperator (the ka-0000006023 net) rather than
// stamping a non-human. An unmapped reporter must NOT poison the attribution.
func TestJiraTriggerEvaluator_UnresolvedReporterFallsBackToDefault(t *testing.T) {
	t.Setenv("TRIGGER_ENGINE_DEFAULT_OPERATOR", "oidc:matthew.baker@acme.example")
	issues := []JiraIssue{
		{
			Key: "FSM-902",
			Fields: JiraIssueFields{
				Summary:  "Unknown reporter",
				Reporter: &JiraUser{DisplayName: "Ghost", AccountID: "acct-ghost"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{jiraReporterSpawnTrigger()}}
	// No mapping for acct-ghost — resolver returns "".
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{}}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
		Users:  users,
	})
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnOperators) != 1 || daemon.spawnOperators[0] != "oidc:matthew.baker@acme.example" {
		t.Errorf("spawn operators = %v, want [oidc:matthew.baker@acme.example] (default fallback)", daemon.spawnOperators)
	}
}

// TestJiraTriggerEvaluator_NoUsersUnchanged verifies that when no UserDirectory
// is wired (Users nil), the evaluator behaves exactly as before — no reporter
// resolver is installed and a sentinel-authored trigger flows through to fleet
// auto-attribution (empty spawn operator on the mock).
func TestJiraTriggerEvaluator_NoUsersUnchanged(t *testing.T) {
	issues := []JiraIssue{
		{
			Key: "FSM-903",
			Fields: JiraIssueFields{
				Summary:  "No directory wired",
				Reporter: &JiraUser{DisplayName: "Jane", AccountID: "acct-jane"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{jiraReporterSpawnTrigger()}}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
		// Users deliberately nil.
	})
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	// fleet:jira-bridge is not a human, so ctxWithTriggerOperator places it on
	// ctx but gateSpawnOperator won't stamp it as operated_by (only humans are
	// stamped). The mock records the raw ctx operator, which is the sentinel.
	if got := daemon.spawnedFields[0]["operated_by"]; got != "" {
		t.Errorf("agent operated_by = %q, want empty (no human, unchanged behavior)", got)
	}
}

func TestJiraTriggerEvaluator_Dedup(t *testing.T) {
	issues := []JiraIssue{
		{Key: "PE-501", Fields: JiraIssueFields{Summary: "Bug one"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-2",
				Type:   "trigger",
				Status: "open",
				Title:  "Auto-fix PE bugs",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE AND issuetype = Bug",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// First evaluation: should spawn.
	eval.Evaluate(context.Background())
	// Second evaluation: should NOT spawn (deduped).
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Errorf("expected 1 spawn (dedup), got %d", len(daemon.spawnedNames))
	}
}

func TestJiraTriggerEvaluator_DisabledTrigger(t *testing.T) {
	issues := []JiraIssue{
		{Key: "PE-601", Fields: JiraIssueFields{Summary: "Should not fire"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-3",
				Type:   "trigger",
				Status: "open",
				Title:  "Disabled trigger",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "false",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns for disabled trigger, got %d", len(daemon.spawnedNames))
	}
}

func TestJiraTriggerEvaluator_MaxConcurrent(t *testing.T) {
	issues := []JiraIssue{
		{Key: "PE-701", Fields: JiraIssueFields{Summary: "Bug A"}},
		{Key: "PE-702", Fields: JiraIssueFields{Summary: "Bug B"}},
		{Key: "PE-703", Fields: JiraIssueFields{Summary: "Bug C"}},
		{Key: "PE-704", Fields: JiraIssueFields{Summary: "Bug D"}},
		{Key: "PE-705", Fields: JiraIssueFields{Summary: "Bug E"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-4",
				Type:   "trigger",
				Status: "open",
				Title:  "Limited trigger",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":            "project = PE AND issuetype = Bug",
					"action":         "spawn_agent",
					"project":        "monorepo",
					"enabled":        "true",
					"max_concurrent": "2",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 2 {
		t.Errorf("expected 2 spawns (max_concurrent=2), got %d", len(daemon.spawnedNames))
	}
}

func TestJiraTriggerEvaluator_NotifyAction(t *testing.T) {
	issues := []JiraIssue{
		{Key: "DEVOPS-50", Fields: JiraIssueFields{Summary: "New infra request"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	var (
		mu          sync.Mutex
		notified    bool
		notifyTitle string
		notifyMsg   string
	)

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-5",
				Type:   "trigger",
				Status: "open",
				Title:  "DEVOPS watcher",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = DEVOPS",
					"action":  "notify",
					"prompt":  "New DEVOPS ticket: {{jira_key}} — {{jira_summary}}",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Notifier: func(_ context.Context, title, msg string, _ string) {
			mu.Lock()
			notified = true
			notifyTitle = title
			notifyMsg = msg
			mu.Unlock()
		},
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	mu.Lock()
	defer mu.Unlock()

	if !notified {
		t.Fatal("expected notification")
	}
	if notifyTitle != "DEVOPS watcher" {
		t.Errorf("notify title: %s", notifyTitle)
	}
	if notifyMsg != "New DEVOPS ticket: DEVOPS-50 — New infra request" {
		t.Errorf("notify message: %s", notifyMsg)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	// Should NOT spawn an agent for notify action.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns for notify action, got %d", len(daemon.spawnedNames))
	}
}

func TestJiraTriggerEvaluator_CatchUp(t *testing.T) {
	issues := []JiraIssue{
		{Key: "PE-801", Fields: JiraIssueFields{Summary: "Already tracked"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Trigger bead.
			{
				ID:     "gd-trigger-6",
				Type:   "trigger",
				Status: "open",
				Title:  "PE auto-fix",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = PE",
					"action":  "spawn_agent",
					"project": "monorepo",
					"enabled": "true",
				},
			},
			// Existing task bead created by a previous trigger run.
			{
				ID:     "gd-task-existing",
				Type:   "task",
				Status: "in_progress",
				Title:  "[PE-801] Already tracked",
				Labels: []string{"source:jira", "trigger:gd-trigger-6"},
				Fields: map[string]string{
					"jira_key": "PE-801",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Run catch-up to populate seen set.
	eval.CatchUpTrigger(context.Background(), "gd-trigger-6")

	// Evaluate should not spawn for PE-801 (already seen).
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns after catch-up, got %d", len(daemon.spawnedNames))
	}
}

// TestJiraTriggerEvaluator_WideWindowIsDedupGatedNotWindowGated is the
// gd-KF6cwVD5lc regression: re-arming the live FSM-triage trigger
// (ka-0000005997) widened its JQL from `created >= -2d` to `created >= -14d` so
// a multi-day outage/pause can self-heal instead of permanently dropping
// whatever it missed (the -2d gap that silently accreted the 88-ticket
// pre-go-live backlog, evidence ka-0000003294).
//
// The safety property this guards: a wider window is storm-free because the
// trigger's re-fire suppression is the engine `seen` map (seeded by
// CatchUpKeys from prior trigger-labeled beads), NOT the JQL window. So when a
// widened window re-surfaces already-triaged tickets, every one is
// dedup-suppressed — while a ticket the narrow window had DROPPED (never fired,
// so absent from `seen`) still fires exactly once. This is what makes widening
// the correct durable fix rather than a re-triage-storm hazard.
//
// The test JIRA server does not itself evaluate JQL — it returns whatever the
// widened window would surface — so the assertion isolates the engine behavior
// that actually makes the wider window safe.
func TestJiraTriggerEvaluator_WideWindowIsDedupGatedNotWindowGated(t *testing.T) {
	// The widened `created >= -14d` window surfaces two already-triaged tickets
	// (in `seen` via a prior trigger-labeled bead) plus one ticket the old
	// narrow `-2d` window had dropped during a multi-day gap (absent from
	// `seen`). Only the dropped one must fire.
	issues := []JiraIssue{
		{Key: "FSM-492", Fields: JiraIssueFields{Summary: "Already triaged, re-surfaced by wider window"}},
		{Key: "FSM-493", Fields: JiraIssueFields{Summary: "Already triaged, re-surfaced by wider window"}},
		{Key: "FSM-470", Fields: JiraIssueFields{Summary: "Dropped during a >2d gap — never triaged"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// The live-shaped FSM-triage trigger with the widened window.
			{
				ID:     "ka-0000005997",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM new tickets → fsm-triage agent",
				Labels: []string{"trigger:jira", "project:monorepo"},
				Fields: map[string]string{
					"jql":            "project = FSM AND created >= -14d",
					"action":         "spawn_agent",
					"role":           "fsm-triage",
					"project":        "monorepo",
					"prompt":         "Triage {{jira_key}}: {{jira_summary}}",
					"enabled":        "true",
					"max_concurrent": "2",
				},
			},
			// Prior trigger-labeled beads for the two already-triaged tickets —
			// exactly the CatchUpKeys dedup seed the live engine rehydrates.
			{
				ID:     "gd-task-fsm-492",
				Type:   "task",
				Status: "closed",
				Title:  "[FSM-492] Already triaged",
				Labels: []string{"source:jira", "trigger:ka-0000005997"},
				Fields: map[string]string{"jira_key": "FSM-492"},
			},
			{
				ID:     "gd-task-fsm-493",
				Type:   "task",
				Status: "closed",
				Title:  "[FSM-493] Already triaged",
				Labels: []string{"source:jira", "trigger:ka-0000005997"},
				Fields: map[string]string{"jira_key": "FSM-493"},
			},
			// Note: FSM-470 has NO trigger-labeled bead — it was dropped by the
			// narrow window and never fired, so it is absent from `seen`.
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Rehydrate the dedup seed the way the live engine does on startup, then
	// evaluate the widened window.
	eval.CatchUpTrigger(context.Background(), "ka-0000005997")
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Exactly one spawn: the gap-dropped ticket. The two already-triaged
	// tickets the wider window re-surfaced are dedup-suppressed — no storm.
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected exactly 1 spawn (only the gap-dropped ticket), got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedRoles[0] != "fsm-triage" {
		t.Errorf("spawned role = %q, want fsm-triage", daemon.spawnedRoles[0])
	}
	if want := "Triage FSM-470: Dropped during a >2d gap — never triaged"; daemon.lastPrompt != want {
		t.Errorf("spawn fired for the wrong ticket:\n  got:  %q\n  want: %q", daemon.lastPrompt, want)
	}
}

func TestExpandIssueTemplate(t *testing.T) {
	issue := JiraIssue{
		Key: "FSM-42",
		Fields: JiraIssueFields{
			Summary:  "Login broken for customer",
			Status:   &JiraNamedRef{Name: "Open"},
			Priority: &JiraNamedRef{Name: "High"},
			Reporter: &JiraUser{DisplayName: "Jane Doe"},
			Labels:   []string{"urgent", "auth"},
		},
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			"basic substitution",
			"Fix {{jira_key}}: {{jira_summary}}",
			"Fix FSM-42: Login broken for customer",
		},
		{
			"all fields",
			"{{jira_key}} [{{jira_status}}] ({{jira_priority}}) reported by {{jira_reporter}} — {{jira_labels}}",
			"FSM-42 [Open] (High) reported by Jane Doe — urgent, auth",
		},
		{
			"lowercase key",
			"agent/fix-{{jira_key_lower}}",
			"agent/fix-fsm-42",
		},
		{
			"empty template",
			"",
			"",
		},
		{
			"no placeholders",
			"Static prompt text",
			"Static prompt text",
		},
		{
			"unassigned",
			"Assigned to: {{jira_assignee}}",
			"Assigned to: Unassigned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandIssueTemplate(tt.template, issue)
			if got != tt.expected {
				t.Errorf("expandIssueTemplate():\n  got:  %q\n  want: %q", got, tt.expected)
			}
		})
	}
}

func TestJiraTriggerEvaluator_NoTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient("http://unused"),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Should not panic or error when no triggers exist.
	eval.Evaluate(context.Background())
}

func TestJiraTriggerEvaluator_CreateBeadAction(t *testing.T) {
	issues := []JiraIssue{
		{Key: "FSM-200", Fields: JiraIssueFields{Summary: "New service request"}},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-7",
				Type:   "trigger",
				Status: "open",
				Title:  "FSM importer",
				Labels: []string{"trigger:jira"},
				Fields: map[string]string{
					"jql":     "project = FSM",
					"action":  "create_bead",
					"project": "fsm-triage",
					"enabled": "true",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Should create a bead but NOT spawn.
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns for create_bead action, got %d", len(daemon.spawnedNames))
	}
}

func TestJiraTriggerEvaluator_DevopsSpawnAgent(t *testing.T) {
	// Simulates the exact trigger config created for DEVOPS crew spawning.
	issues := []JiraIssue{
		{
			Key: "DEVOPS-401",
			Fields: JiraIssueFields{
				Summary:   "Set up monitoring for new EKS cluster",
				Status:    &JiraNamedRef{Name: "To Do"},
				IssueType: &JiraNamedRef{Name: "Task"},
				Priority:  &JiraNamedRef{Name: "Medium"},
			},
		},
		{
			Key: "DEVOPS-402",
			Fields: JiraIssueFields{
				Summary:   "Rotate API keys for staging env",
				Status:    &JiraNamedRef{Name: "Ready for Development"},
				IssueType: &JiraNamedRef{Name: "Task"},
				Priority:  &JiraNamedRef{Name: "High"},
			},
		},
	}
	jiraServer := newTestJiraServer(issues)
	defer jiraServer.Close()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-trigger-devops",
				Type:   "trigger",
				Status: "open",
				Title:  "Auto-crew on new DEVOPS ticket",
				Labels: []string{"trigger:jira", "project:gardenio"},
				Fields: map[string]string{
					"jql":              `project = DEVOPS AND status in ("To Do", "Ready for Development") AND assignee is EMPTY`,
					"action":           "spawn_agent",
					"role":             "crew",
					"project":          "gardenio",
					"prompt":           "Triage and work on JIRA ticket {{jira_key}}: {{jira_summary}}. Priority: {{jira_priority}}. Type: {{jira_type}}. Status: {{jira_status}}.",
					"enabled":          "true",
					"max_concurrent":   "3",
					"cooldown_minutes": "5",
				},
			},
		},
	}

	eval := NewJiraTriggerEvaluator(JiraTriggerConfig{
		Jira:   newTestJiraClient(jiraServer.URL),
		Daemon: daemon,
		Logger: slog.Default(),
	})

	eval.Evaluate(context.Background())

	// Check first evaluation results.
	daemon.mu.Lock()
	createdCount := len(daemon.createdBeads)
	bead0Title := ""
	bead1Title := ""
	if createdCount >= 1 {
		bead0Title = daemon.createdBeads[0].Title
	}
	if createdCount >= 2 {
		bead1Title = daemon.createdBeads[1].Title
	}
	spawnCount := len(daemon.spawnedNames)
	roles := append([]string{}, daemon.spawnedRoles...)
	lastPrompt := daemon.lastPrompt
	daemon.mu.Unlock()

	if createdCount != 2 {
		t.Fatalf("expected 2 created beads, got %d", createdCount)
	}
	if bead0Title != "[DEVOPS-401] Set up monitoring for new EKS cluster" {
		t.Errorf("bead[0] title: %s", bead0Title)
	}
	if bead1Title != "[DEVOPS-402] Rotate API keys for staging env" {
		t.Errorf("bead[1] title: %s", bead1Title)
	}
	if spawnCount != 2 {
		t.Fatalf("expected 2 spawned agents, got %d", spawnCount)
	}
	for _, role := range roles {
		if role != "crew" {
			t.Errorf("agent role: %s, expected crew", role)
		}
	}
	if lastPrompt != "Triage and work on JIRA ticket DEVOPS-402: Rotate API keys for staging env. Priority: High. Type: Task. Status: Ready for Development." {
		t.Errorf("last agent prompt: %s", lastPrompt)
	}

	// Verify dedup: second evaluation should not spawn again.
	eval.Evaluate(context.Background())

	daemon.mu.Lock()
	spawnCountAfter := len(daemon.spawnedNames)
	daemon.mu.Unlock()

	if spawnCountAfter != 2 {
		t.Errorf("expected still 2 spawns after dedup, got %d", spawnCountAfter)
	}
}
