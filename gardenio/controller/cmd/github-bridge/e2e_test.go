package main

// Hermetic end-to-end test: a fixture repository's life as GitHub would tell it,
// delivered as signed webhooks through the production receiver and handler chain
// (newBeadSink, the same wiring run() uses) into an in-memory beads daemon.
// Nothing here touches the network, GitHub, or a cluster.
//
// It asserts the three places a GitHub event must land for parity with
// gitlab-bridge: the bead fields, the agent's nudge (mail bead) and the agent's
// Slack thread (the squawk-shaped message beads slack-bridge relays).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const (
	e2eAgent = "crew-pr-events"
	e2eRepo  = "joyousaversion/gardenio"
)

// memDaemon is a stateful in-memory bridgeDaemon. Unlike the per-handler
// fakes it keeps beads, deps and idempotency keys, so a later event sees what
// an earlier one wrote, the way it does against kbeads.
type memDaemon struct {
	mu    sync.Mutex
	seq   int
	beads map[string]*beadsapi.BeadDetail
	order []string
	deps  []memDep
	keys  map[string]string // idempotency key -> bead id

	// The agent-wake writes (ciwake_test.go).
	comments []memComment
	spawns   []memSpawn
	spawnErr error
}

type memDep struct{ from, to, typ string }

func newMemDaemon() *memDaemon {
	return &memDaemon{beads: map[string]*beadsapi.BeadDetail{}, keys: map[string]string{}}
}

func (d *memDaemon) add(b *beadsapi.BeadDetail) {
	if b.ID == "" {
		d.seq++
		b.ID = fmt.Sprintf("gd-mem%04d", d.seq)
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	d.beads[b.ID] = b
	d.order = append(d.order, b.ID)
}

func (d *memDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.beads[id]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (d *memDaemon) ListTaskBeadsForMR(_ context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, id := range d.order {
		if b := d.beads[id]; workBeadTypes[b.Type] && b.Fields["mr_url"] == mrURL {
			out = append(out, b)
		}
	}
	return out, nil
}

func (d *memDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res := &beadsapi.ListBeadsResult{}
	for _, id := range d.order {
		b := d.beads[id]
		if len(q.Types) > 0 && b.Type != q.Types[0] {
			continue
		}
		match := true
		for k, v := range q.Fields {
			match = match && b.Fields[k] == v
		}
		if match {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func (d *memDaemon) UpdateBeadFields(_ context.Context, id string, f map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.beads[id]
	if !ok {
		return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
	}
	for k, v := range f {
		b.Fields[k] = v
	}
	return nil
}

// UpsertMergeRequestBead mirrors beadsapi's contract for the fields this test
// reads: one type=merge-request bead per mr_url, forge stamped when set, closed
// on a terminal state with mr_outcome.
func (d *memDaemon) UpsertMergeRequestBead(_ context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var mr *beadsapi.BeadDetail
	for _, id := range d.order {
		if b := d.beads[id]; b.Type == "merge-request" && b.Fields["mr_url"] == f.MRURL {
			mr = b
		}
	}
	if mr == nil {
		mr = &beadsapi.BeadDetail{Type: "merge-request", Status: "open"}
		d.add(mr)
	}
	mr.Title = f.Title
	if closed {
		mr.Status = "closed"
	} else {
		mr.Status = "open"
	}
	for k, v := range map[string]string{
		"mr_url": f.MRURL, "mr_iid": f.MRIID, "mr_state": f.MRState, "mr_outcome": f.MROutcome,
		"mr_project_path": f.ProjectPath, "mr_source_branch": f.SourceBranch,
		"mr_target_branch": f.TargetBranch, "forge": f.Forge, "mr_author": f.Author,
	} {
		if v != "" {
			mr.Fields[k] = v
		}
	}
	return mr.ID, nil
}

func (d *memDaemon) AddDependency(_ context.Context, from, to, typ, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.deps {
		if e == (memDep{from, to, typ}) {
			return fmt.Errorf("duplicate dependency %s %s %s", from, typ, to)
		}
	}
	d.deps = append(d.deps, memDep{from, to, typ})
	return nil
}

func (d *memDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if req.IdempotencyKey != "" {
		if id, ok := d.keys[req.IdempotencyKey]; ok {
			return id, nil
		}
	}
	b := &beadsapi.BeadDetail{
		Type: req.Type, Title: req.Title, Description: req.Description,
		Assignee: req.Assignee, Labels: req.Labels, Priority: req.Priority, Status: "open",
	}
	if len(req.Fields) > 0 {
		if err := json.Unmarshal(req.Fields, &b.Fields); err != nil {
			return "", err
		}
	}
	d.add(b)
	if req.IdempotencyKey != "" {
		d.keys[req.IdempotencyKey] = b.ID
	}
	return b.ID, nil
}

func (d *memDaemon) CloseBead(_ context.Context, id string, _ map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.beads[id]; ok {
		b.Status = "closed"
	}
	return nil
}

func (d *memDaemon) MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.order {
		if b := d.beads[id]; b.Type == "merge-request" && b.Fields["mr_url"] == mrURL {
			return b, nil
		}
	}
	return nil, nil
}

func (d *memDaemon) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, e := range d.deps {
		if e.typ == "produces" && e.to == mrBeadID {
			out = append(out, d.beads[e.from])
		}
	}
	return out, nil
}

// ofType returns the beads of a type, oldest first.
func (d *memDaemon) ofType(typ string) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, id := range d.order {
		if b := d.beads[id]; b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

var _ bridgeDaemon = (*memDaemon)(nil)

// threadLines are the texts slack-bridge would post into the agent's thread.
func (d *memDaemon) threadLines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, b := range d.ofType("message") {
		if b.Status != "closed" {
			t.Errorf("thread line %s left open: slack-bridge relays closed squawks", b.ID)
		}
		if b.Fields["source_agent"] != e2eAgent {
			t.Errorf("thread line source_agent = %q, want %q", b.Fields["source_agent"], e2eAgent)
		}
		out = append(out, b.Fields["text"])
	}
	return out
}

// TestE2E_FixtureRepoPullRequestLifecycle walks one PR from opened to merged:
// failing then passing CI and a changes-requested review in between.
func TestE2E_FixtureRepoPullRequestLifecycle(t *testing.T) {
	d := newMemDaemon()
	d.add(&beadsapi.BeadDetail{ID: taskID, Type: "task", Status: "in_progress", Assignee: e2eAgent, Title: "PR events"})
	d.add(&beadsapi.BeadDetail{
		ID: "gd-agent0001", Type: "agent", Status: "in_progress", Title: e2eAgent,
		Fields: map[string]string{"agent": e2eAgent, "task_id": taskID},
	})

	h := newWebhookHandler(webhookConfig{
		Secret: testSecret,
		Sink:   newBeadSink(logSink{logger: quiet()}, d, nil, nil, quiet()),
		Logger: quiet(),
		Replay: newReplayCache(3600e9, 100, nil),
	})
	n := 0
	send := func(event, fixtureName string, wantCode int) {
		t.Helper()
		n++
		body := fixture(t, fixtureName)
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerSignature, sign(testSecret, body))
		req.Header.Set(headerDelivery, "e2e-"+strconv.Itoa(n))
		req.Header.Set(headerEvent, event)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != wantCode {
			t.Fatalf("%s %s = %d, want %d: %s", event, fixtureName, rec.Code, wantCode, rec.Body.String())
		}
	}
	task := func() *beadsapi.BeadDetail { b, _ := d.GetBead(context.Background(), taskID); return b }
	mr := func() *beadsapi.BeadDetail {
		t.Helper()
		b, _ := d.MRBeadByURL(context.Background(), ghPRURL)
		if b == nil {
			t.Fatal("no merge-request bead for the PR")
		}
		return b
	}

	// 1. PR opened on the agent's branch: the MR bead appears, the task is
	// linked to it, and the thread hears about it.
	send("pull_request", "pull_request_opened", http.StatusAccepted)
	if f := mr().Fields; f["forge"] != "github" || f["mr_state"] != "opened" || f["mr_iid"] != "42" ||
		f["mr_project_path"] != e2eRepo || f["mr_source_branch"] != "agent/launch/"+taskID {
		t.Errorf("MR bead fields after open: %v", f)
	}
	if f := task().Fields; f["mr_url"] != ghPRURL || f["forge"] != "github" {
		t.Errorf("task bead not linked to the PR: %v", f)
	}
	wantDeps := []memDep{{taskID, mr().ID, "produces"}, {mr().ID, taskID, "triggered-by"}}
	if len(d.deps) != 2 || d.deps[0] != wantDeps[0] || d.deps[1] != wantDeps[1] {
		t.Errorf("deps = %+v, want %+v", d.deps, wantDeps)
	}

	// 2. CI fails.
	send("check_suite", "check_suite_completed_failure", http.StatusAccepted)
	if f := mr().Fields; f["mr_pipeline_status"] != "failed" || !strings.Contains(f["mr_pipeline_url"], "/commit/9fceb02") {
		t.Errorf("pipeline fields after failure: %v", f)
	}

	// 3. A human requests changes: the review flag lands on the task and the
	// agent is nudged with the review text.
	send("pull_request_review", "review_changes_requested", http.StatusAccepted)
	if task().Fields[reviewFlagField] != "true" {
		t.Errorf("%s not set on the task after a changes-requested review", reviewFlagField)
	}
	mails := d.ofType("mail")
	if len(mails) != 1 || mails[0].Assignee != e2eAgent {
		t.Fatalf("mails after review = %+v, want one to %s", mails, e2eAgent)
	}
	if !strings.Contains(mails[0].Title, "alice requested changes on PR #42") ||
		!strings.Contains(mails[0].Description, "nil <bead> case") {
		t.Errorf("review nudge lost the reviewer or the ask:\n%s\n%s", mails[0].Title, mails[0].Description)
	}
	if mails[0].Priority != 2 || strings.Contains(strings.Join(mails[0].Labels, ","), "interrupt") {
		t.Errorf("review nudge must inform, not interrupt: priority=%d labels=%v", mails[0].Priority, mails[0].Labels)
	}

	// 4. CI goes green after the fix.
	send("check_suite", "check_suite_completed_success", http.StatusAccepted)
	if got := mr().Fields["mr_pipeline_status"]; got != "success" {
		t.Errorf("mr_pipeline_status = %q, want success", got)
	}

	// 5. Merge: MR bead closes with the outcome, the agent is told.
	send("pull_request", "pull_request_closed_merged", http.StatusAccepted)
	if m := mr(); m.Status != "closed" || m.Fields["mr_state"] != "merged" || m.Fields["mr_outcome"] != "merged" {
		t.Errorf("MR bead after merge: status=%s fields=%v", m.Status, m.Fields)
	}
	mails = d.ofType("mail")
	if len(mails) != 2 || !strings.Contains(mails[1].Title, "merged") {
		t.Fatalf("mails after merge = %d, want the merge notice second: %+v", len(mails), mails)
	}

	// 6. GitHub redelivers the merge (same GUID): acknowledged, nothing new.
	beadsBefore := len(d.order)
	n-- // reuse the previous delivery id
	send("pull_request", "pull_request_closed_merged", http.StatusOK)
	if len(d.order) != beadsBefore {
		t.Errorf("redelivery created %d bead(s)", len(d.order)-beadsBefore)
	}

	// The thread read, in order, as the story of the PR.
	lines := d.threadLines(t)
	wantInOrder := []string{"PR opened", "failed on PR #42", "requested changes", "passed on PR #42", "PR merged"}
	if len(lines) != len(wantInOrder) {
		t.Fatalf("thread lines = %d, want %d:\n%s", len(lines), len(wantInOrder), strings.Join(lines, "\n"))
	}
	for i, want := range wantInOrder {
		if !strings.Contains(lines[i], want) {
			t.Errorf("thread line %d = %q, want it to contain %q", i, lines[i], want)
		}
	}
}
