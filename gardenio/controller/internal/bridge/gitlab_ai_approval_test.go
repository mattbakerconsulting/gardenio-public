package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// mockAIApprovalGitLab is a fake AIApprovalGitLab for enforcer tests.
type mockAIApprovalGitLab struct {
	mu sync.Mutex
	// existing maps "project!iid" → rules already on the MR.
	existing map[string][]GitLabMRApprovalRule
	// created records every CreateMergeRequestApprovalRule call.
	created   []createdRule
	listErr   error
	createErr error
}

type createdRule struct {
	Project string
	IID     int
	Spec    MRApprovalRuleSpec
}

func newMockAIApprovalGitLab() *mockAIApprovalGitLab {
	return &mockAIApprovalGitLab{existing: make(map[string][]GitLabMRApprovalRule)}
}

func (m *mockAIApprovalGitLab) key(project string, iid int) string {
	return project + "!" + strconv.Itoa(iid)
}

func (m *mockAIApprovalGitLab) ListMergeRequestApprovalRules(_ context.Context, project string, iid int) ([]GitLabMRApprovalRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.existing[m.key(project, iid)], nil
}

func (m *mockAIApprovalGitLab) CreateMergeRequestApprovalRule(_ context.Context, project string, iid int, spec MRApprovalRuleSpec) (*GitLabMRApprovalRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.created = append(m.created, createdRule{Project: project, IID: iid, Spec: spec})
	rule := GitLabMRApprovalRule{ID: len(m.created), Name: spec.Name, ApprovalsRequired: spec.ApprovalsRequired}
	m.existing[m.key(project, iid)] = append(m.existing[m.key(project, iid)], rule)
	return &rule, nil
}

func baseConfig() AIApprovalConfig {
	return AIApprovalConfig{
		Enabled:           true,
		BotUsernames:      []string{"gardenio-bot"},
		ApprovalsRequired: 1,
		ReviewerGroupIDs:  []int{42},
	}
}

func TestNewAIApprovalEnforcer_DisabledReturnsNil(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = false
	if e := NewAIApprovalEnforcer(newMockAIApprovalGitLab(), cfg, slog.Default()); e != nil {
		t.Fatal("expected nil enforcer when disabled")
	}
}

func TestNewAIApprovalEnforcer_NoBotsReturnsNil(t *testing.T) {
	cfg := baseConfig()
	cfg.BotUsernames = nil
	if e := NewAIApprovalEnforcer(newMockAIApprovalGitLab(), cfg, slog.Default()); e != nil {
		t.Fatal("expected nil enforcer when no bot usernames configured")
	}
}

func TestNewAIApprovalEnforcer_DefaultsRuleNameAndCount(t *testing.T) {
	cfg := baseConfig()
	cfg.RuleName = ""
	cfg.ApprovalsRequired = 0
	e := NewAIApprovalEnforcer(newMockAIApprovalGitLab(), cfg, slog.Default())
	if e == nil {
		t.Fatal("expected non-nil enforcer")
	}
	if e.cfg.RuleName != DefaultAIApprovalRuleName {
		t.Errorf("rule name = %q, want default %q", e.cfg.RuleName, DefaultAIApprovalRuleName)
	}
	if e.cfg.ApprovalsRequired != 1 {
		t.Errorf("approvals required = %d, want clamped to 1", e.cfg.ApprovalsRequired)
	}
}

func TestEnsureRule_AppliesToAIAuthor(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err != nil {
		t.Fatalf("EnsureRule: %v", err)
	}
	if len(gl.created) != 1 {
		t.Fatalf("expected 1 rule created, got %d", len(gl.created))
	}
	got := gl.created[0]
	if got.Spec.Name != DefaultAIApprovalRuleName {
		t.Errorf("rule name = %q, want %q", got.Spec.Name, DefaultAIApprovalRuleName)
	}
	if got.Spec.ApprovalsRequired != 1 {
		t.Errorf("approvals required = %d, want 1", got.Spec.ApprovalsRequired)
	}
	if len(got.Spec.GroupIDs) != 1 || got.Spec.GroupIDs[0] != 42 {
		t.Errorf("group ids = %v, want [42]", got.Spec.GroupIDs)
	}
}

func TestEnsureRule_SkipsNonAIAuthor(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	if err := e.EnsureRule(context.Background(), "org/repo", 7, "alice-human"); err != nil {
		t.Fatalf("EnsureRule: %v", err)
	}
	if len(gl.created) != 0 {
		t.Fatalf("expected no rule for human author, got %d", len(gl.created))
	}
}

func TestEnsureRule_Idempotent(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	for i := range 3 {
		if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err != nil {
			t.Fatalf("EnsureRule iter %d: %v", i, err)
		}
	}
	if len(gl.created) != 1 {
		t.Fatalf("expected exactly 1 rule across re-runs, got %d", len(gl.created))
	}
}

func TestEnsureRule_IdempotentCaseInsensitive(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	gl.existing[gl.key("org/repo", 7)] = []GitLabMRApprovalRule{
		{ID: 99, Name: "ai-generated CHANGE review", ApprovalsRequired: 1},
	}
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err != nil {
		t.Fatalf("EnsureRule: %v", err)
	}
	if len(gl.created) != 0 {
		t.Fatalf("expected no new rule when name matches case-insensitively, got %d", len(gl.created))
	}
}

func TestEnsureRule_SkipsWhenNoReviewerConfigured(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	cfg := baseConfig()
	cfg.ReviewerGroupIDs = nil
	cfg.ReviewerUserIDs = nil
	e := NewAIApprovalEnforcer(gl, cfg, slog.Default())

	if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err != nil {
		t.Fatalf("EnsureRule: %v", err)
	}
	if len(gl.created) != 0 {
		t.Fatalf("expected no rule when no reviewers configured, got %d", len(gl.created))
	}
}

func TestEnsureRule_NilEnforcerNoOp(t *testing.T) {
	var e *AIApprovalEnforcer
	if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err != nil {
		t.Fatalf("nil enforcer should be a no-op, got %v", err)
	}
}

func TestEnsureRule_PropagatesListError(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	gl.listErr = errors.New("boom")
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	if err := e.EnsureRule(context.Background(), "org/repo", 7, "gardenio-bot"); err == nil {
		t.Fatal("expected error to propagate from list")
	}
}

func TestEnsureRule_SkipsInvalidTarget(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	e := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	if err := e.EnsureRule(context.Background(), "", 0, "gardenio-bot"); err != nil {
		t.Fatalf("EnsureRule: %v", err)
	}
	if len(gl.created) != 0 {
		t.Fatalf("expected no rule for invalid target, got %d", len(gl.created))
	}
}

// TestGitLabWebhookHandler_AIApprovalOnOpen pins that an MR `open` event from a
// bot author drives the enforcer to create the approval rule, and that a human
// author's open event does not.
func TestGitLabWebhookHandler_AIApprovalOnOpen(t *testing.T) {
	gl := newMockAIApprovalGitLab()
	enforcer := NewAIApprovalEnforcer(gl, baseConfig(), slog.Default())

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newMockGitLabDaemon(),
		WebhookSecret: "test-secret",
		AIApproval:    enforcer,
		Logger:        slog.Default(),
	})

	openEvent := func(author string, iid int) []byte {
		body, _ := json.Marshal(map[string]any{
			"object_kind": "merge_request",
			"user":        map[string]any{"username": author},
			"project":     map[string]any{"path_with_namespace": "org/repo"},
			"object_attributes": map[string]any{
				"iid":               iid,
				"state":             "opened",
				"action":            "open",
				"url":               "https://gitlab.com/org/repo/-/merge_requests/" + strconv.Itoa(iid),
				"target_project_id": 99,
			},
		})
		return body
	}

	post := func(body []byte) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Token", "test-secret")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	if code := post(openEvent("gardenio-bot", 10)); code != 200 {
		t.Fatalf("bot open: expected 200, got %d", code)
	}
	if len(gl.created) != 1 {
		t.Fatalf("bot open: expected 1 rule created, got %d", len(gl.created))
	}
	if gl.created[0].IID != 10 {
		t.Errorf("rule applied to wrong MR iid %d, want 10", gl.created[0].IID)
	}

	if code := post(openEvent("alice-human", 11)); code != 200 {
		t.Fatalf("human open: expected 200, got %d", code)
	}
	if len(gl.created) != 1 {
		t.Fatalf("human open must not create a rule: total created = %d, want 1", len(gl.created))
	}
}

func TestParseIntCSV(t *testing.T) {
	cases := map[string][]int{
		"":            nil,
		"42":          {42},
		"1,2,3":       {1, 2, 3},
		" 1 , 2 ,foo": {1, 2},
		",,":          nil,
	}
	for in, want := range cases {
		got := ParseIntCSV(in)
		if len(got) != len(want) {
			t.Errorf("ParseIntCSV(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ParseIntCSV(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
