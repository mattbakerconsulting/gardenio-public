package main

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeMRLinkDaemon is an in-memory mrLinkDaemon for the auto-link guard tests.
type fakeMRLinkDaemon struct {
	beads      map[string]*beadsapi.BeadDetail
	deps       map[string][]beadsapi.Dependency // by bead ID (forward deps)
	byAssignee map[string][]*beadsapi.BeadDetail
	mrByURL    map[string]*beadsapi.BeadDetail
	updates    map[string]map[string]string // beadID -> fields set
	updateErr  error
}

func newFakeMRLinkDaemon() *fakeMRLinkDaemon {
	return &fakeMRLinkDaemon{
		beads:      map[string]*beadsapi.BeadDetail{},
		deps:       map[string][]beadsapi.Dependency{},
		byAssignee: map[string][]*beadsapi.BeadDetail{},
		mrByURL:    map[string]*beadsapi.BeadDetail{},
		updates:    map[string]map[string]string{},
	}
}

func (f *fakeMRLinkDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	b, ok := f.beads[id]
	if !ok {
		return nil, nil
	}
	return b, nil
}

func (f *fakeMRLinkDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return f.deps[id], nil
}

func (f *fakeMRLinkDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: f.byAssignee[q.Assignee]}, nil
}

func (f *fakeMRLinkDaemon) MRBeadByURL(_ context.Context, url string) (*beadsapi.BeadDetail, error) {
	return f.mrByURL[url], nil
}

func (f *fakeMRLinkDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.updates[id] == nil {
		f.updates[id] = map[string]string{}
	}
	maps.Copy(f.updates[id], fields)
	// Reflect onto the in-memory bead so subsequent reads see the change.
	if b := f.beads[id]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, fields)
	}
	return nil
}

const testMRURL = "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/916"

func agentBead(id, name string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Kind: "config", Type: "agent", Fields: map[string]string{"agent": name}}
}

func taskBead(id string, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Kind: "issue", Type: "task", Fields: map[string]string{}, UpdatedAt: updated}
}

// Core case from ka-0000000816: PR url in wrap-up, an assignee-linked task with
// no mr_url, no produces edge → auto-link sets mr_url on the task bead.
func TestAutoLinkWrapupMRs_AssigneeLinkedTask(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	task := taskBead("gd-task1", time.Now())
	f.beads["gd-task1"] = task
	f.byAssignee["thread-xyz"] = []*beadsapi.BeadDetail{task}

	wrapup := &WrapUp{PullRequests: []string{testMRURL}}
	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", wrapup)

	if got := f.updates["gd-task1"]["mr_url"]; got != testMRURL {
		t.Fatalf("expected mr_url set on gd-task1, got %q (updates=%v)", got, f.updates)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Auto-linked") {
		t.Fatalf("expected one Auto-linked message, got %v", msgs)
	}
}

// Produces-graph case: agent --produces--> task (no assignee match).
func TestAutoLinkWrapupMRs_ProducesGraph(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	task := taskBead("gd-task2", time.Now())
	f.beads["gd-task2"] = task
	f.deps["agent-1"] = []beadsapi.Dependency{{BeadID: "agent-1", DependsOnID: "gd-task2", Type: "produces"}}

	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{PullRequests: []string{testMRURL}})
	if f.updates["gd-task2"]["mr_url"] != testMRURL {
		t.Fatalf("expected mr_url set on gd-task2 via produces edge, got updates=%v", f.updates)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %v", msgs)
	}
}

// Already-linked via the canonical MR bead → no update, no message.
func TestAutoLinkWrapupMRs_AlreadyLinkedMRBead(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	task := taskBead("gd-task3", time.Now())
	f.beads["gd-task3"] = task
	f.byAssignee["thread-xyz"] = []*beadsapi.BeadDetail{task}
	f.mrByURL[testMRURL] = &beadsapi.BeadDetail{ID: "gd-mr1", Type: "merge-request"}

	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{PullRequests: []string{testMRURL}})
	if len(f.updates) != 0 {
		t.Fatalf("expected no updates when already linked, got %v", f.updates)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages when already linked, got %v", msgs)
	}
}

// Already-linked via an mr_url field on the candidate task → no update.
func TestAutoLinkWrapupMRs_AlreadyLinkedField(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	task := taskBead("gd-task4", time.Now())
	task.Fields["mr_url"] = testMRURL
	f.beads["gd-task4"] = task
	f.byAssignee["thread-xyz"] = []*beadsapi.BeadDetail{task}

	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{PullRequests: []string{testMRURL}})
	if len(f.updates) != 0 || len(msgs) != 0 {
		t.Fatalf("expected no-op when task already carries mr_url, updates=%v msgs=%v", f.updates, msgs)
	}
}

// No candidate bead to attach to → advisory message, no update.
func TestAutoLinkWrapupMRs_NoCandidate(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")

	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{PullRequests: []string{testMRURL}})
	if len(f.updates) != 0 {
		t.Fatalf("expected no updates with no candidate, got %v", f.updates)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "gb mr link") {
		t.Fatalf("expected advisory message pointing at `gb mr link`, got %v", msgs)
	}
}

// Non-GitLab-MR urls (GitHub PR, plain link) are ignored entirely.
func TestAutoLinkWrapupMRs_NonGitLabIgnored(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	task := taskBead("gd-task5", time.Now())
	f.beads["gd-task5"] = task
	f.byAssignee["thread-xyz"] = []*beadsapi.BeadDetail{task}

	wrapup := &WrapUp{PullRequests: []string{
		"https://github.com/owner/repo/pull/7",
		"https://example.com/not-an-mr",
	}}
	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", wrapup)
	if len(f.updates) != 0 || len(msgs) != 0 {
		t.Fatalf("expected non-GitLab urls ignored, updates=%v msgs=%v", f.updates, msgs)
	}
}

// Two distinct MR urls land on two distinct candidate beads (most-recent first).
func TestAutoLinkWrapupMRs_TwoURLsTwoBeads(t *testing.T) {
	f := newFakeMRLinkDaemon()
	f.beads["agent-1"] = agentBead("agent-1", "thread-xyz")
	older := taskBead("gd-old", time.Now().Add(-time.Hour))
	newer := taskBead("gd-new", time.Now())
	f.beads["gd-old"] = older
	f.beads["gd-new"] = newer
	f.byAssignee["thread-xyz"] = []*beadsapi.BeadDetail{older, newer}

	url2 := "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/917"
	msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{PullRequests: []string{testMRURL, url2}})
	if len(msgs) != 2 {
		t.Fatalf("expected two link messages, got %v", msgs)
	}
	// Newest bead gets the first url.
	if f.updates["gd-new"]["mr_url"] != testMRURL {
		t.Errorf("expected newest bead gd-new to get first url, got %v", f.updates["gd-new"])
	}
	if f.updates["gd-old"]["mr_url"] != url2 {
		t.Errorf("expected older bead gd-old to get second url, got %v", f.updates["gd-old"])
	}
}

// Nil/empty wrap-ups and nil daemon are safe no-ops.
func TestAutoLinkWrapupMRs_Empty(t *testing.T) {
	if msgs := autoLinkWrapupMRs(context.Background(), nil, "agent-1", &WrapUp{PullRequests: []string{testMRURL}}); msgs != nil {
		t.Errorf("nil daemon should be a no-op, got %v", msgs)
	}
	f := newFakeMRLinkDaemon()
	if msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", nil); msgs != nil {
		t.Errorf("nil wrapup should be a no-op, got %v", msgs)
	}
	if msgs := autoLinkWrapupMRs(context.Background(), f, "agent-1", &WrapUp{}); msgs != nil {
		t.Errorf("empty pull_requests should be a no-op, got %v", msgs)
	}
}
