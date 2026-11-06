package bridge

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- fake ------------------------------------------------------------------

// fakeCatchupDaemon is a tiny in-memory MentionCatchupDaemon for the watchdog
// tests. It stores beads by ID and serves ListBeadsFiltered with the small
// subset of predicates the watchdog uses (type, status, field equality),
// records every SpawnAgent + AddComment call, and lets a test inject list/spawn
// errors. Mirrors the fake-per-suite style of escalation_reaper_test.go.
type fakeCatchupDaemon struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	spawns   []catchupSpawn
	comments []catchupComment
	// listErr, when non-nil, makes the NEXT ListBeadsFiltered whose query
	// targets the given type return the error (keyed by first type). Used to
	// drive the fail-safe branches.
	listErrByType map[string]error
	spawnErr      error
	// nextAgentID is the id returned by SpawnAgent; incremented per call.
	nextAgentID int
	// autoLinkSpawnedAgent, when true, makes SpawnAgent also insert a live agent
	// bead linked to the task (task_id set, no terminal agent_state) so the
	// idempotency path (next sweep sees a live agent) can be exercised.
	autoLinkSpawnedAgent bool
}

type catchupSpawn struct {
	AgentName string
	Project   string
	TaskID    string
	Role      string
	Prompt    string
	Extra     map[string]string
}

type catchupComment struct {
	BeadID string
	Author string
	Text   string
}

func newFakeCatchupDaemon() *fakeCatchupDaemon {
	return &fakeCatchupDaemon{
		beads:         map[string]*beadsapi.BeadDetail{},
		listErrByType: map[string]error{},
	}
}

func (f *fakeCatchupDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(q.Types) > 0 {
		if err := f.listErrByType[q.Types[0]]; err != nil {
			return nil, err
		}
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if !typeIn(b.Type, q.Types) {
			continue
		}
		if len(q.Statuses) > 0 && !statusIn(b.Status, q.Statuses) {
			continue
		}
		if !fieldsMatch(b.Fields, q.Fields) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeCatchupDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	var extra map[string]string
	if len(extraFields) > 0 {
		extra = extraFields[0]
	}
	f.spawns = append(f.spawns, catchupSpawn{
		AgentName: agentName,
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		Prompt:    customPrompt,
		Extra:     extra,
	})
	f.nextAgentID++
	id := "agent-spawned-" + string(rune('a'+f.nextAgentID-1))
	if f.autoLinkSpawnedAgent {
		f.beads[id] = &beadsapi.BeadDetail{
			ID:     id,
			Title:  agentName,
			Type:   "agent",
			Status: "open",
			Fields: map[string]string{"agent": agentName, "task_id": taskID},
		}
	}
	return id, nil
}

func (f *fakeCatchupDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, catchupComment{BeadID: beadID, Author: author, Text: text})
	return nil
}

// seedTask inserts a task bead; seedAgent inserts an agent bead.
func (f *fakeCatchupDaemon) seed(b *beadsapi.BeadDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beads[b.ID] = b
}

// fakeCatchupGitLab is a tiny MentionCatchupGitLab for the terminal-MR guard
// tests. It serves GetMergeRequestByPath from an in-memory map keyed by IID and
// can inject a fetch error. Mirrors the fake-per-suite style above.
type fakeCatchupGitLab struct {
	mu        sync.Mutex
	stateByID map[int]string // mrIID → state (opened/closed/merged/locked)
	notFound  map[int]bool   // mrIID → return (nil, nil)
	err       error          // when non-nil, every fetch errors
	calls     int
}

func newFakeCatchupGitLab() *fakeCatchupGitLab {
	return &fakeCatchupGitLab{stateByID: map[int]string{}, notFound: map[int]bool{}}
}

func (g *fakeCatchupGitLab) GetMergeRequestByPath(_ context.Context, projectPath string, mrIID int) (*GitLabMR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.err != nil {
		return nil, g.err
	}
	if g.notFound[mrIID] {
		return nil, nil
	}
	state, ok := g.stateByID[mrIID]
	if !ok {
		state = "opened"
	}
	return &GitLabMR{IID: mrIID, State: state, WebURL: projectPath}, nil
}

func (g *fakeCatchupGitLab) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (f *fakeCatchupDaemon) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawns)
}

func typeIn(t string, set []string) bool {
	if len(set) == 0 {
		return true
	}
	return slices.Contains(set, t)
}

func statusIn(s string, set []string) bool {
	return slices.Contains(set, s)
}

func fieldsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// --- helpers ---------------------------------------------------------------

func newTestWatchdog(daemon MentionCatchupDaemon, nowFn func() time.Time) *MentionCatchupWatchdog {
	w := NewMentionCatchupWatchdog(MentionCatchupConfig{
		Daemon:  daemon,
		Logger:  slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		Grace:   10 * time.Minute,
		Project: "gardenio",
	})
	if nowFn != nil {
		w.now = nowFn
	}
	return w
}

// newTestWatchdogGL builds a watchdog wired to a GitLab fake so the terminal-MR
// guard path is exercised.
func newTestWatchdogGL(daemon MentionCatchupDaemon, gitlab MentionCatchupGitLab, nowFn func() time.Time) *MentionCatchupWatchdog {
	w := NewMentionCatchupWatchdog(MentionCatchupConfig{
		Daemon:  daemon,
		GitLab:  gitlab,
		Logger:  slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		Grace:   10 * time.Minute,
		Project: "gardenio",
	})
	if nowFn != nil {
		w.now = nowFn
	}
	return w
}

// orphanTask builds a well-formed orphaned gitlab-mention task bead created
// `age` before `now`.
func orphanTask(id string, now time.Time, age time.Duration) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     "GitLab mention on MR !1160: fix the thing",
		Type:      "task",
		Status:    "open",
		Labels:    []string{"project:gardenio", "gitlab-mention"},
		CreatedAt: now.Add(-age),
		Fields: map[string]string{
			"spawn_source":            "gitlab-mention",
			"mr_url":                  "https://gitlab.com/acme/platform/monorepo/-/merge_requests/1160",
			"gitlab_mr_iid":           "1160",
			"mention_author":          "patrick",
			"mention_instruction":     "please address the review comment on foo.go",
			"gitlab_mr_source_branch": "feat/foo",
		},
	}
}

// --- tests -----------------------------------------------------------------

// TestCatchup_RespawnsOrphanedTask is the core acceptance test: an open
// gitlab-mention task past the grace window with no linked agent bead is
// respawned exactly once, with the crew role and MR-binding fields carried
// forward, and an audit comment recorded.
func TestCatchup_RespawnsOrphanedTask(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-orphan", now, 20*time.Minute))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected exactly 1 respawn, got %d", got)
	}
	s := d.spawns[0]
	if s.TaskID != "gd-orphan" {
		t.Errorf("respawn TaskID = %q, want gd-orphan", s.TaskID)
	}
	if s.Role != "crew" {
		t.Errorf("respawn Role = %q, want crew", s.Role)
	}
	if s.Project != "gardenio" {
		t.Errorf("respawn Project = %q, want gardenio (from project: label)", s.Project)
	}
	if !strings.HasPrefix(s.AgentName, "mr-1160-") {
		t.Errorf("respawn AgentName = %q, want mr-1160-* (mention agent name shape, iid-scoped)", s.AgentName)
	}
	// Prompt reconstruction must carry the MR + instruction.
	for _, want := range []string{"!1160", "patrick", "please address the review comment", "feat/foo", "gd-orphan"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("respawn prompt missing %q; got:\n%s", want, s.Prompt)
		}
	}
	// Extra fields: spawn_source, single_claim guard, MR binding.
	if s.Extra["spawn_source"] != "gitlab-mention" {
		t.Errorf("extra spawn_source = %q, want gitlab-mention", s.Extra["spawn_source"])
	}
	if s.Extra["single_claim_task"] != "true" {
		t.Errorf("extra single_claim_task = %q, want true (double-spawn guard)", s.Extra["single_claim_task"])
	}
	if s.Extra["gitlab_mr_url"] == "" {
		t.Errorf("extra gitlab_mr_url empty; MR binding not carried forward")
	}
	if s.Extra["mention_task"] != "gd-orphan" {
		t.Errorf("extra mention_task = %q, want gd-orphan", s.Extra["mention_task"])
	}
	// Every Extra key must be a field type:agent DECLARES. The carry-forward
	// loop reads the TASK bead, whose mr_url / mention_author spellings do not
	// exist on type:agent — copying them verbatim 400'd the whole agent create
	// ("mr_url: unknown field") and the watchdog respawned nothing, forever
	// (ka-0000003933). Task-only spellings must be translated, never carried.
	for _, taskOnly := range []string{"mr_url", "mention_author", "mention_instruction"} {
		if _, ok := s.Extra[taskOnly]; ok {
			t.Errorf("extra carries task-only field %q; type:agent does not declare it, so the agent create 400s and no respawn happens", taskOnly)
		}
	}
	if s.Extra["gitlab_username"] != "patrick" {
		t.Errorf("extra gitlab_username = %q, want patrick (identity carried under the agent-bead spelling)", s.Extra["gitlab_username"])
	}
	// Audit comment recorded on the task.
	if len(d.comments) != 1 || d.comments[0].BeadID != "gd-orphan" {
		t.Fatalf("expected 1 audit comment on gd-orphan, got %+v", d.comments)
	}
	if d.comments[0].Author != mentionCatchupActor {
		t.Errorf("audit comment author = %q, want %q", d.comments[0].Author, mentionCatchupActor)
	}
}

// TestCatchup_SkipsWithinGraceWindow: a freshly-created mention task (younger
// than the grace window) is NOT respawned — the webhook path still owns it.
func TestCatchup_SkipsWithinGraceWindow(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-fresh", now, 2*time.Minute)) // younger than 10m grace

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a task inside the grace window, got %d", got)
	}
}

// TestCatchup_SkipsZeroCreatedAt: a task with an unknown (zero) created_at fails
// safe as "too young" and is never respawned on a missing timestamp.
func TestCatchup_SkipsZeroCreatedAt(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	b := orphanTask("gd-notime", now, 20*time.Minute)
	b.CreatedAt = time.Time{} // zero
	d.seed(b)

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a zero-created_at task, got %d", got)
	}
}

// TestCatchup_SkipsWhenLiveAgentPresent: a task whose crew is alive (active,
// non-terminal agent bead linked by task_id) is NEVER respawned — even with the
// assignee cleared (the stale-reaper-reopened case).
func TestCatchup_SkipsWhenLiveAgentPresent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	task := orphanTask("gd-live", now, 20*time.Minute)
	task.Assignee = "" // assignee cleared, but a live agent is on it
	d.seed(task)
	d.seed(&beadsapi.BeadDetail{
		ID:     "gd-live-agent",
		Title:  "mr-1160-abc",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"task_id": "gd-live", "agent_state": "working"},
	})

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns when a live agent is on the task, got %d", got)
	}
}

// TestCatchup_RespawnsWhenLinkedAgentTerminal: a task whose only linked agent
// bead is in a terminal (failed) state IS respawned — a dead agent bead does not
// count as coverage. This is the crashed-crew case the stale-reaper leaves
// behind un-dispatched.
func TestCatchup_RespawnsWhenLinkedAgentTerminal(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-deadcrew", now, 20*time.Minute))
	d.seed(&beadsapi.BeadDetail{
		ID:     "gd-dead-agent",
		Title:  "mr-1160-old",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"task_id": "gd-deadcrew", "agent_state": "failed"},
	})

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn for a task whose linked agent is terminal, got %d", got)
	}
}

// TestCatchup_Idempotent: after a respawn creates a live agent bead, a second
// sweep does NOT respawn again.
func TestCatchup_Idempotent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.autoLinkSpawnedAgent = true
	d.seed(orphanTask("gd-idem", now, 20*time.Minute))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected exactly 1 respawn across two sweeps (idempotent), got %d", got)
	}
}

// TestCatchup_SkipsNonMentionTasks: the watchdog only touches
// spawn_source=gitlab-mention beads even if a non-mention task somehow slips
// through the field filter (belt-and-suspenders re-check).
func TestCatchup_SkipsNonMentionTasks(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	b := orphanTask("gd-other", now, 20*time.Minute)
	b.Fields["spawn_source"] = "schedule" // not a mention
	d.seed(b)

	w := newTestWatchdog(d, func() time.Time { return now })
	// Call evaluateTask directly to bypass the server-side field filter and
	// prove the in-code re-check rejects a foreign bead.
	if w.evaluateTask(context.Background(), b) {
		t.Fatalf("evaluateTask respawned a non-mention task")
	}
	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a non-mention task, got %d", got)
	}
}

// TestCatchup_SkipsE2EFixtures: a fixture-labelled task is never respawned
// against the real spawn path.
func TestCatchup_SkipsE2EFixtures(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	b := orphanTask("gd-e2e", now, 20*time.Minute)
	b.Labels = append(b.Labels, e2eFixtureLabelPrefix+"catchup")
	d.seed(b)

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for an e2e fixture, got %d", got)
	}
}

// TestCatchup_FailSafeOnAgentListError: a liveness-check list error makes the
// watchdog ASSUME the task is covered (no respawn) — under-respawning is the
// safe direction; a false respawn would race a possibly-live crew.
func TestCatchup_FailSafeOnAgentListError(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-listerr", now, 20*time.Minute))
	d.listErrByType["agent"] = errors.New("daemon down")

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns when the agent liveness list errors (fail-safe), got %d", got)
	}
}

// TestCatchup_DryRunDoesNotSpawn: dry-run logs the intent but never spawns or
// comments.
func TestCatchup_DryRunDoesNotSpawn(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-dry", now, 20*time.Minute))

	w := NewMentionCatchupWatchdog(MentionCatchupConfig{
		Daemon:  d,
		Logger:  slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		Grace:   10 * time.Minute,
		Project: "gardenio",
		DryRun:  true,
	})
	w.now = func() time.Time { return now }
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("dry-run should not spawn, got %d spawns", got)
	}
	if len(d.comments) != 0 {
		t.Fatalf("dry-run should not comment, got %d comments", len(d.comments))
	}
}

// TestCatchup_SpawnFailureNoComment: when SpawnAgent errors the watchdog does
// NOT record a success audit comment (the rescue did not happen).
func TestCatchup_SpawnFailureNoComment(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.spawnErr = errors.New("spawn boom")
	d.seed(orphanTask("gd-spawnfail", now, 20*time.Minute))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if len(d.comments) != 0 {
		t.Fatalf("a failed respawn must not record a success audit comment, got %d", len(d.comments))
	}
}

// TestCatchup_DefaultsApplied verifies the constructor's zero-value defaults.
func TestCatchup_DefaultsApplied(t *testing.T) {
	w := NewMentionCatchupWatchdog(MentionCatchupConfig{Daemon: newFakeCatchupDaemon()})
	if w.interval != defaultMentionCatchupInterval {
		t.Errorf("interval = %v, want default %v", w.interval, defaultMentionCatchupInterval)
	}
	if w.grace != defaultMentionCatchupGrace {
		t.Errorf("grace = %v, want default %v", w.grace, defaultMentionCatchupGrace)
	}
	// The grace window must exceed the webhook path's own dedup window so the
	// two never race on the same fresh task.
	if w.grace <= mentionDedupWindow {
		t.Errorf("catchup grace %v must exceed webhook dedup window %v", w.grace, mentionDedupWindow)
	}
}

// TestCatchup_ProjectFallback: a task with no project: label falls back to the
// configured default project.
func TestCatchup_ProjectFallback(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	b := orphanTask("gd-noproj", now, 20*time.Minute)
	b.Labels = []string{"gitlab-mention"} // no project: label
	d.seed(b)

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn, got %d", got)
	}
	if d.spawns[0].Project != "gardenio" {
		t.Errorf("respawn Project = %q, want gardenio (default fallback)", d.spawns[0].Project)
	}
}

// --- parked-handle guard (ka-0000000411) -----------------------------------

// parkHandle builds an open escalation/decision bead whose requesting_task
// points back at the given mention task — the live re-entry handle a crew files
// when its work is done but the close is gated on an operator action.
func parkHandle(id, handleType, status, requestingTask string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Approve + merge !2261 (apikit Kafka producer awaits delivery ack)",
		Type:   handleType,
		Status: status,
		Fields: map[string]string{
			"requesting_task": requestingTask,
			"operation_type":  "gitlab_mr_merge",
		},
	}
}

// TestCatchup_SkipsTaskParkedBehindOpenEscalation is the core regression test
// for ka-0000000411: a mention task whose work is done but un-closable (the fix
// awaits one human MR approval) is parked behind an OPEN close-on-execute merge
// escalation with requesting_task set. The watchdog must NOT respawn — the
// escalation is a live re-entry handle that auto-respawns the filing crew on
// merge. This is the exact 7x-respawn storm on !2261 the bug reports.
func TestCatchup_SkipsTaskParkedBehindOpenEscalation(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-parked", now, 30*time.Minute)) // well past grace, no live agent
	d.seed(parkHandle("gd-esc", "escalation", "open", "gd-parked"))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a task parked behind an open escalation, got %d", got)
	}
	if len(d.comments) != 0 {
		t.Fatalf("a parked task must not get a respawn audit comment, got %d", len(d.comments))
	}
}

// TestCatchup_SkipsTaskParkedBehindOpenDecision: the same skip holds for an open
// DECISION (the other handle type filed with --requesting-task).
func TestCatchup_SkipsTaskParkedBehindOpenDecision(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-parked-dec", now, 30*time.Minute))
	d.seed(parkHandle("gd-dec", "decision", "open", "gd-parked-dec"))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a task parked behind an open decision, got %d", got)
	}
}

// TestCatchup_SkipsTaskParkedBehindBlockedHandle: a handle in a non-open pending
// state (blocked/deferred) still parks the task — it is still awaiting an
// operator, matching the stop-gate parkHandleOpen allow-list.
func TestCatchup_SkipsTaskParkedBehindBlockedHandle(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-parked-blk", now, 30*time.Minute))
	d.seed(parkHandle("gd-esc-blk", "escalation", "blocked", "gd-parked-blk"))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a task parked behind a blocked handle, got %d", got)
	}
}

// TestCatchup_RespawnsWhenParkHandleClosed: a CLOSED (resolved) escalation no
// longer parks the task — resolving it already fired its own auto-respawn, and
// if the task is still open+orphaned afterwards it is genuinely stranded and
// should be rescued. The closed handle must not grant a permanent exemption.
func TestCatchup_RespawnsWhenParkHandleClosed(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-wasparked", now, 30*time.Minute))
	d.seed(parkHandle("gd-esc-closed", "escalation", "closed", "gd-wasparked"))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn for a task whose park handle is closed, got %d", got)
	}
}

// TestCatchup_RespawnsWhenHandleTargetsDifferentTask: an open escalation whose
// requesting_task points at a DIFFERENT task must not park this one — the guard
// only skips a task with a handle pointing back at itself.
func TestCatchup_RespawnsWhenHandleTargetsDifferentTask(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-mine", now, 30*time.Minute))
	// An open handle exists, but it parks some OTHER task.
	d.seed(parkHandle("gd-esc-other", "escalation", "open", "gd-someone-else"))

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn — the open handle targets a different task, got %d", got)
	}
}

// TestCatchup_FailSafeOnParkHandleListError: a park-handle list error makes the
// watchdog ASSUME the task is parked (no respawn). Under-respawning for one tick
// is the safe direction — the whole point of this guard is to STOP a redundant
// respawn storm, so a transient daemon hiccup must fail toward "don't respawn".
func TestCatchup_FailSafeOnParkHandleListError(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-parkerr", now, 30*time.Minute))
	d.listErrByType["escalation"] = errors.New("daemon down")

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns when the park-handle list errors (fail-safe), got %d", got)
	}
}

// TestCatchup_ParkHandleCheckedBeforeGitLab: a parked task must be skipped
// BEFORE the terminal-MR guard runs, so the watchdog never spends a GitLab fetch
// on a task it will skip anyway (parked tasks are the common re-fire case).
func TestCatchup_ParkHandleCheckedBeforeGitLab(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-parked-gl", now, 30*time.Minute))
	d.seed(parkHandle("gd-esc-gl", "escalation", "open", "gd-parked-gl"))
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "opened"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a parked task, got %d", got)
	}
	if gl.callCount() != 0 {
		t.Fatalf("park-handle guard must short-circuit before the GitLab fetch, got %d calls", gl.callCount())
	}
}

// --- terminal-MR guard (ka-0000007740) -------------------------------------

// TestCatchup_SkipsMergedMR: a task whose linked MR is MERGED on GitLab is NOT
// respawned — the work is settled; re-dispatching crew onto it is wasted spend
// and posts confusing comments on a finished MR.
func TestCatchup_SkipsMergedMR(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-merged", now, 20*time.Minute)) // MR IID 1160
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "merged"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a merged MR, got %d", got)
	}
	if len(d.comments) != 0 {
		t.Fatalf("a skipped (merged) MR must not record an audit comment, got %d", len(d.comments))
	}
	if gl.callCount() == 0 {
		t.Fatalf("expected the guard to query GitLab for MR state")
	}
}

// TestCatchup_SkipsClosedMR: a task whose linked MR is CLOSED (not merged, e.g.
// the site-deployment !914 case) is likewise NOT respawned.
func TestCatchup_SkipsClosedMR(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-closed", now, 20*time.Minute))
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "closed"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a closed MR, got %d", got)
	}
}

// TestCatchup_RespawnsOpenMR: the load-bearing case — a task whose linked MR is
// still OPEN IS respawned. These are the real, silently-dropped human review
// asks the watchdog exists to rescue; the terminal-MR guard must never swallow
// them. This is why an age ceiling would be WRONG: most orphans are months old
// yet sit on open MRs.
func TestCatchup_RespawnsOpenMR(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-open", now, 90*24*time.Hour)) // 90 days old, still open
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "opened"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn for an open MR (months-old but live ask), got %d", got)
	}
}

// TestCatchup_RespawnsLockedMR: a `locked` MR is still an active discussion an
// unaddressed mention should reach; only merged/closed are terminal, so a locked
// MR respawns.
func TestCatchup_RespawnsLockedMR(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-locked", now, 20*time.Minute))
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "locked"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn for a locked MR (not terminal), got %d", got)
	}
}

// TestCatchup_FailsOpenOnGitLabError: a GitLab fetch error must NOT skip the
// respawn — a transient hiccup dropping a real ask is the worse failure. The
// guard fails OPEN (respawn proceeds); the next sweep re-checks.
func TestCatchup_FailsOpenOnGitLabError(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-glerr", now, 20*time.Minute))
	gl := newFakeCatchupGitLab()
	gl.err = errors.New("gitlab 503")

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn when the MR-state fetch errors (fail-open), got %d", got)
	}
}

// TestCatchup_FailsOpenOnMRNotFound: a (nil, nil) MR lookup (MR not found /
// deleted project) fails open — respawn proceeds rather than silently dropping.
func TestCatchup_FailsOpenOnMRNotFound(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-notfound", now, 20*time.Minute))
	gl := newFakeCatchupGitLab()
	gl.notFound[1160] = true

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn when the MR is not found (fail-open), got %d", got)
	}
}

// TestCatchup_NilGitLabRespawns: with no GitLab client configured the guard is
// inert (fail-open) — the watchdog degrades to its pre-guard behaviour rather
// than dropping every ask. Guards the nil-client branch.
func TestCatchup_NilGitLabRespawns(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-nogl", now, 20*time.Minute))

	// newTestWatchdog wires no GitLab client (w.gitlab == nil).
	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn with a nil GitLab client (guard inert), got %d", got)
	}
}

// TestCatchup_UnparseableMRURLRespawns: a task whose mr_url does not parse
// cannot be verified, so the guard falls through to respawn (defensive: a
// mention task always carries a real mr_url in practice).
func TestCatchup_UnparseableMRURLRespawns(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	b := orphanTask("gd-badurl", now, 20*time.Minute)
	b.Fields["mr_url"] = "not-a-gitlab-url"
	d.seed(b)
	gl := newFakeCatchupGitLab()

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn for an unparseable mr_url (guard falls through), got %d", got)
	}
	if gl.callCount() != 0 {
		t.Fatalf("guard must not call GitLab when the mr_url is unparseable, got %d calls", gl.callCount())
	}
}

// TestCatchup_MergedMRNotFetchedForYoungOrGuarded: the terminal-MR guard runs
// only for a bead that is otherwise an orphan (past grace, no live agent). A
// task inside the grace window is skipped BEFORE the guard, so no GitLab call is
// made — the guard never adds a fetch to a bead that would not respawn anyway.
func TestCatchup_MergedMRNotFetchedWithinGrace(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	d.seed(orphanTask("gd-young", now, 2*time.Minute)) // inside 10m grace
	gl := newFakeCatchupGitLab()
	gl.stateByID[1160] = "merged"

	w := newTestWatchdogGL(d, gl, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns for a young task, got %d", got)
	}
	if gl.callCount() != 0 {
		t.Fatalf("guard must not query GitLab for a bead skipped before the guard, got %d calls", gl.callCount())
	}
}

// TestCatchup_SkipsTaskCoveredByLiveNudgedAgent (Track A3, plan ka-0000004007):
// a mention task delivered by NUDGING a live agent (rather than spawning) has no
// linked agent bead of its own — the webhook stamps nudged_agent instead. While
// that agent is still live, the watchdog must NOT treat the task as an orphan
// and respawn a redundant crew.
func TestCatchup_SkipsTaskCoveredByLiveNudgedAgent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	task := orphanTask("gd-nudged", now, 20*time.Minute)
	task.Fields["nudged_agent"] = "mr-1160-alive"
	d.seed(task)
	// The nudged agent is live — bound by its `agent` name, NOT task_id (a
	// nudged agent's task_id points at ITS original task, not this one).
	d.seed(&beadsapi.BeadDetail{
		ID:     "gd-nudged-agent",
		Title:  "mr-1160-alive",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"agent": "mr-1160-alive", "agent_state": "working"},
	})

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 0 {
		t.Fatalf("expected 0 respawns when the nudged agent is still live, got %d", got)
	}
}

// TestCatchup_RespawnsWhenNudgedAgentTerminal (Track A3): if the nudged agent
// later dies WITHOUT addressing the mention, the guard goes false and the task
// correctly falls through to a fresh dispatch — the guard defers to a covering
// agent, it never permanently drops a request.
func TestCatchup_RespawnsWhenNudgedAgentTerminal(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	d := newFakeCatchupDaemon()
	task := orphanTask("gd-nudged-dead", now, 20*time.Minute)
	task.Fields["nudged_agent"] = "mr-1160-dead"
	d.seed(task)
	d.seed(&beadsapi.BeadDetail{
		ID:     "gd-nudged-dead-agent",
		Title:  "mr-1160-dead",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"agent": "mr-1160-dead", "agent_state": "failed"},
	})

	w := newTestWatchdog(d, func() time.Time { return now })
	w.sweepOnce(context.Background())

	if got := d.spawnCount(); got != 1 {
		t.Fatalf("expected 1 respawn when the nudged agent is terminal, got %d", got)
	}
}
