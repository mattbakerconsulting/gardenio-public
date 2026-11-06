package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/podmanager"
)

func codexAcct() config.AccountCacheEntry {
	return config.AccountCacheEntry{
		Provider:    "openai",
		Credential:  "secret:gardenio-codex-auth#auth.json",
		BillingType: "subscription",
		Model:       "gpt-5-codex",
		Status:      "active",
		Labels:      []string{"project:gardenio"},
	}
}

func TestCodexSessionSecretName(t *testing.T) {
	tests := []struct {
		cred, want string
		wantErr    bool
	}{
		{"secret:gardenio-codex-auth#auth.json", "gardenio-codex-auth-session", false},
		{"secret:gardenio-codex-auth", "gardenio-codex-auth-session", false},
		{"secret:gardenio-codex-auth-work#auth.json", "gardenio-codex-auth-work-session", false},
		{"secret:gardenio-codex-auth#token", "", true},
		{"env:OPENAI_API_KEY", "", true},
		{"coopmux:codex", "", true},
		{"secret:#auth.json", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := codexSessionSecretName(tt.cred)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("codexSessionSecretName(%q) = (%q, %v), want (%q, err=%v)", tt.cred, got, err, tt.want, tt.wantErr)
		}
	}
}

// A role-pinned provider=openai account mounts the -session Secret and sets
// CODEX_HOME, and emits nothing Anthropic/LiteLLM/Claude-OAuth shaped.
func TestApplyAccountConfig_CodexSubscription(t *testing.T) {
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"codex-crew": {Account: "gardenio-codex"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-codex": codexAcct()},
		LiteLLMURL:   "http://litellm.example",
		ClaudeModel:  "claude-opus",
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("unexpected SpawnBlocked: %s", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "gardenio-codex" {
		t.Errorf("ResolvedAccount = %q", spec.ResolvedAccount)
	}
	if spec.CodexAuthSecret != "gardenio-codex-auth-session" {
		t.Errorf("CodexAuthSecret = %q, want the derived -session Secret, never the refresh-token Secret", spec.CodexAuthSecret)
	}
	if spec.Env["CODEX_HOME"] != "/home/agent/.codex" {
		t.Errorf("CODEX_HOME = %q", spec.Env["CODEX_HOME"])
	}
	if spec.Env["CODEX_MODEL"] != "gpt-5-codex" {
		t.Errorf("CODEX_MODEL = %q", spec.Env["CODEX_MODEL"])
	}
	for k := range spec.Env {
		if strings.HasPrefix(k, "ANTHROPIC_") || k == "CLAUDE_MODEL" || k == "CLAUDE_CODE_OAUTH_TOKEN" {
			t.Errorf("env %s must not be set for a Codex account", k)
		}
	}
	if len(spec.SecretEnv) != 0 {
		t.Errorf("SecretEnv = %+v, want none (no CLAUDE_CODE_OAUTH_TOKEN)", spec.SecretEnv)
	}
	for _, name := range referencedSecretNamesForTest(*spec) {
		if name == "gardenio-codex-auth" {
			t.Errorf("the refresh-token Secret %q must never be referenced by an agent pod", name)
		}
	}
}

func referencedSecretNamesForTest(spec podmanager.AgentPodSpec) []string {
	names := []string{spec.CodexAuthSecret}
	for _, se := range spec.SecretEnv {
		names = append(names, se.SecretName)
	}
	return names
}

func TestApplyAccountConfig_CodexMalformedCredentialBlocksSpawn(t *testing.T) {
	acct := codexAcct()
	acct.Credential = "env:OPENAI_API_KEY"
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"codex-crew": {Account: "gardenio-codex"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-codex": acct},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if !spec.SpawnBlocked || !strings.Contains(spec.SpawnBlockReason, "gardenio-codex") {
		t.Errorf("want SpawnBlocked naming the account, got blocked=%v reason=%q", spec.SpawnBlocked, spec.SpawnBlockReason)
	}
	if spec.CodexAuthSecret != "" {
		t.Errorf("CodexAuthSecret = %q, want empty", spec.CodexAuthSecret)
	}
}

// The Claude spread pool must never offer an openai account, and a project that
// only has a Codex account must not look "credentialed but exhausted" (which
// would block every Claude spawn).
func TestSpread_ExcludesOpenAIAccounts(t *testing.T) {
	claude := config.AccountCacheEntry{Provider: "anthropic", Credential: "secret:gardenio-claude-oauth-a", Status: "active", Labels: []string{"project:gardenio"}}
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{
		"gardenio-codex": codexAcct(),
		"claude-a":       claude,
	}}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	for i := 0; i < 10; i++ {
		if got := selectAccount(cfg, "gardenio", spec, map[string]int{"claude-a": 50}); got != "claude-a" {
			t.Fatalf("selectAccount = %q, want claude-a (openai excluded even when heavily outloaded)", got)
		}
	}

	onlyCodex := &config.Config{AccountCache: map[string]config.AccountCacheEntry{"gardenio-codex": codexAcct()}}
	if n := countCredentialedProjectAccounts(onlyCodex, "gardenio", ""); n != 0 {
		t.Errorf("countCredentialedProjectAccounts = %d, want 0 for an openai-only project", n)
	}
	spec2 := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(onlyCodex, spec2)
	if spec2.SpawnBlocked {
		t.Errorf("a Claude spawn in a codex-only project must not be SpawnBlocked: %s", spec2.SpawnBlockReason)
	}
}

// An exhausted role-pinned Codex account must not spill onto the Claude pool.
func TestApplyAccountConfig_CodexPinDoesNotSpillToClaude(t *testing.T) {
	codex := codexAcct()
	codex.Status = "exhausted"
	claude := config.AccountCacheEntry{Provider: "anthropic", Credential: "secret:gardenio-claude-oauth-a", Status: "active", Labels: []string{"project:gardenio"}}
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"codex-crew": {Account: "gardenio-codex"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-codex": codex, "claude-a": claude},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.ResolvedAccount == "claude-a" {
		t.Fatal("exhausted Codex pin spilled onto a Claude account")
	}
	if !spec.SpawnBlocked {
		t.Error("exhausted Codex pin should block the spawn, not spill")
	}
}

func TestSessionSpendReconciler_SkipsSubscriptionAccounts(t *testing.T) {
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"codex-crew": {Account: "gardenio-codex"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-codex": codexAcct()},
	}
	if !resolveSpendAccountIsOpenAI(cfg, "gardenio", "codex-crew") {
		t.Fatal("codex-crew resolves to an openai account")
	}
	if resolveSpendAccountIsOpenAI(cfg, "gardenio", "crew") {
		t.Fatal("crew has no pin and must not be treated as openai")
	}

	// daemon is nil: any LiteLLM/bead call would panic, so reaching the end
	// proves captureBaseline and attemptReconcile returned before touching either.
	r := &sessionSpendReconciler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		skipSession: func(project, role string) bool {
			return resolveSpendAccountIsOpenAI(cfg, project, role)
		},
	}
	r.captureBaseline(context.Background(), "gd-1", "agent-1", "gardenio", "codex-crew")
	bead := &beadsapi.BeadDetail{ID: "gd-1", Fields: map[string]string{"project": "gardenio", "role": "codex-crew"}}
	if !r.attemptReconcile(context.Background(), bead, "agent-1", 0) {
		t.Error("attemptReconcile must report landed (terminal) for a subscription account so nothing is retried")
	}
}

// The bulk coopmux sync reads a Claude-style "token" key; a Codex account has
// none (its auth.json is pushed by the keeper), so the sync must skip it without
// touching the Secret API.
func TestSyncAccountsToCoopmux_SkipsCodexAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{
				"id": "a1", "title": "gardenio-codex", "type": "account", "status": "open",
				"labels": []string{"project:gardenio"},
				"fields": map[string]any{"provider": "openai", "credential": "secret:gardenio-codex-auth#auth.json"},
			}},
			"total": 1,
		})
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	if !syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test") {
		t.Error("expected true: nothing to sync is not a failure")
	}
	if n := len(k8s.Actions()); n != 0 {
		t.Errorf("sync made %d Secret API calls for a Codex account, want 0: %v", n, k8s.Actions())
	}
}

// The bulk sync's stale-account cleanup deletes every coopmux account that is not
// in the beads daemon's pushed set. The keeper-owned Codex account is pushed by the
// keeper, not the bulk sync, so the cleanup must leave it alone.
func TestSyncAccountsToCoopmux_CleanupKeepsCodexAccounts(t *testing.T) {
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{"id": "a1", "title": "gardenio-codex", "type": "account", "status": "open",
					"fields": map[string]any{"provider": "openai", "credential": "secret:gardenio-codex-auth#auth.json"}},
				{"id": "a2", "title": "claude-a", "type": "account", "status": "open",
					"fields": map[string]any{"provider": "claude", "credential": "secret:claude-a"}},
			},
			"total": 2,
		})
	}))
	t.Cleanup(daemonSrv.Close)

	var mu sync.Mutex
	var deleted []string
	muxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/credentials/status"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]coopmux.AccountStatus{{Name: "gardenio-codex"}, {Name: "claude-a"}, {Name: "stale"}})
		}
	}))
	t.Cleanup(muxSrv.Close)

	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "claude-a", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("sk-ant-oat-x")},
	})
	notifier := &credSyncNotifier{logger: discardLogger()}
	mux := coopmux.New(coopmux.Config{URL: muxSrv.URL})
	if !syncAccountsToCoopmux(context.Background(), discardLogger(), newClient(t, daemonSrv), k8s, "ns", mux, notifier, "", "test") {
		t.Fatal("sync failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/stale") {
		t.Fatalf("cleanup deleted %v; want only the stale account", deleted)
	}
}
