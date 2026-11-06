package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// Harness precedence: agent > role > project > controller default > claude
// (gd-6SezxhSEEk). Mirrors the model-override tests.

// newHarnessCfg builds a config with an anthropic account ("gardenio") and an
// openai account ("codex-acct"), with optional project/role/default harness.
func newHarnessCfg(defaultHarness, projectHarness, roleHarness string) *config.Config {
	return &config.Config{
		DefaultHarness:         defaultHarness,
		AgentDefaultFableModel: "claude-fable-5-oauth",
		AgentDefaultOpusModel:  "claude-opus-5-oauth",
		AgentDefaultHaikuModel: "anthropic/claude-haiku-4-5",
		ClaudeModel:            "global-claude-model",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio", Harness: projectHarness},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Harness: roleHarness},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {
				Provider: "anthropic", BaseURL: "http://litellm:4000", Credential: "secret:gardenio-oauth",
				Model: "claude-account-model", LiteLLMKey: "sk-key", Labels: []string{"project:gardenio"},
			},
			"codex-acct": {
				Provider: "openai", Credential: "secret:gardenio-codex-auth",
				Model: "gpt-5-codex", Labels: []string{"project:gardenio"},
			},
			"gemini-acct": {
				Provider: "gemini", Credential: "secret:gardenio-gemini-key#api-key",
				Model: "gemini-2.5-pro", Labels: []string{"project:gardenio"},
			},
		},
	}
}

// pinRoleAccount pins an account on the "crew" role. After #394 a
// provider=openai account is reachable only through a role pin: account spread
// and the credentialed count skip openai accounts, and the project-pin branch
// resolves no account.
func pinRoleAccount(cfg *config.Config, account string) *config.Config {
	e := cfg.RoleCache["crew"]
	e.Account = account
	cfg.RoleCache["crew"] = e
	return cfg
}

// onlyAccount trims the pool to one account so account spread is deterministic.
func onlyAccount(cfg *config.Config, name string) *config.Config {
	cfg.AccountCache = map[string]config.AccountCacheEntry{name: cfg.AccountCache[name]}
	cfg.ProjectCache["gardenio"] = config.ProjectCacheEntry{Account: name, Harness: cfg.ProjectCache["gardenio"].Harness}
	return cfg
}

func TestResolveHarness_Precedence(t *testing.T) {
	tests := []struct {
		name                      string
		def, project, role, agent string
		want                      string
		wantErr                   string
	}{
		{"nothing set defaults to claude", "", "", "", "", "claude", ""},
		{"controller default", "codex", "", "", "", "codex", ""},
		{"project beats default", "claude", "codex", "", "", "codex", ""},
		{"role beats project", "", "claude", "codex", "", "codex", ""},
		{"role claude beats project codex", "", "codex", "claude", "", "claude", ""},
		{"agent beats role", "", "", "codex", "claude", "claude", ""},
		{"agent beats everything", "claude", "claude", "claude", "codex", "codex", ""},
		{"whitespace is trimmed", "", "", " codex ", "", "codex", ""},
		{"gemini via role", "", "", "gemini", "", "gemini", ""},
		{"gemini via agent", "", "", "", "gemini", "gemini", ""},
		{"bad role value", "", "", "gpt", "", "", `role "crew": unknown harness "gpt"`},
		{"bad project value", "", "Codex", "", "", "", `project "gardenio": unknown harness "Codex"`},
		{"bad agent value", "", "", "", "aider", "", `agent bead: unknown harness "aider"`},
		{"bad controller default", "bogus", "", "", "", "", `controller default: unknown harness "bogus"`},
		{"bad project value shadowed by role", "", "bogus", "codex", "", "codex", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newHarnessCfg(tc.def, tc.project, tc.role)
			got, err := resolveHarness(cfg, "gardenio", "crew", tc.agent)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveHarness = %q, want %q", got, tc.want)
			}
		})
	}
}

// For a multi-role agent the first role with a harness wins.
func TestResolveHarness_MultiRoleFirstMatch(t *testing.T) {
	cfg := newHarnessCfg("", "", "")
	cfg.RoleCache = map[string]config.RoleCacheEntry{
		"thread": {},
		"crew":   {Harness: "codex"},
	}
	got, err := resolveHarness(cfg, "gardenio", "thread,crew", "")
	if err != nil || got != "codex" {
		t.Fatalf("resolveHarness = %q, %v; want codex", got, err)
	}
}

// A claude spec must be untouched: no GARDENIO_HARNESS, model env and alias
// pins intact, ~/.claude state mount.
func TestHarness_ClaudeSpecUnchanged(t *testing.T) {
	cfg := onlyAccount(newHarnessCfg("", "", ""), "gardenio")
	spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if spec.HarnessError != "" {
		t.Fatalf("unexpected HarnessError %q", spec.HarnessError)
	}
	if spec.Harness != "claude" {
		t.Errorf("Harness = %q, want claude", spec.Harness)
	}
	for _, k := range []string{EnvHarness, "CODEX_MODEL"} {
		if _, ok := spec.Env[k]; ok {
			t.Errorf("claude spec must not set %s", k)
		}
	}
	for k, want := range map[string]string{
		"CLAUDE_MODEL":                  "claude-account-model",
		"ANTHROPIC_MODEL":               "claude-account-model",
		"ANTHROPIC_DEFAULT_FABLE_MODEL": "claude-fable-5-oauth",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":  "claude-opus-5-oauth",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": "anthropic/claude-haiku-4-5",
	} {
		if spec.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, spec.Env[k], want)
		}
	}
	if p, sp := podmanager.HarnessStateMount(spec.Harness); p != podmanager.MountClaudeState || sp != podmanager.SubPathClaudeState {
		t.Errorf("state mount = %q/%q, want the claude mount", p, sp)
	}
}

// Golden for a codex role: harness env, CODEX_MODEL from the resolved model, no
// Claude model pins, and the ~/.codex state mount.
func TestHarness_CodexRoleSpec(t *testing.T) {
	cfg := pinRoleAccount(onlyAccount(newHarnessCfg("", "", "codex"), "codex-acct"), "codex-acct")

	for name, build := range map[string]func(*config.Config, map[string]string) podmanager.AgentPodSpec{
		"events": func(c *config.Config, md map[string]string) podmanager.AgentPodSpec {
			return buildAgentPodSpec(c, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: md})
		},
		"reconciler": func(c *config.Config, md map[string]string) podmanager.AgentPodSpec {
			return BuildSpecFromBeadInfo(c, "gardenio", "crew", "crew", "a", md)
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := build(cfg, map[string]string{})
			if spec.HarnessError != "" {
				t.Fatalf("unexpected HarnessError %q", spec.HarnessError)
			}
			if spec.Harness != "codex" || spec.Env[EnvHarness] != "codex" {
				t.Errorf("Harness=%q %s=%q, want codex/codex", spec.Harness, EnvHarness, spec.Env[EnvHarness])
			}
			if got := spec.Env["CODEX_MODEL"]; got != "gpt-5-codex" {
				t.Errorf("CODEX_MODEL = %q, want the account model gpt-5-codex", got)
			}
			for _, k := range []string{
				"CLAUDE_MODEL", "ANTHROPIC_MODEL", EnvModelPinned,
				"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
			} {
				if v, ok := spec.Env[k]; ok {
					t.Errorf("codex spec must not set %s (got %q)", k, v)
				}
			}
			if p, sp := podmanager.HarnessStateMount(spec.Harness); p != "/home/agent/.codex" || sp != ".state/codex" {
				t.Errorf("state mount = %q/%q, want /home/agent/.codex .state/codex", p, sp)
			}
		})
	}
}

// A gemini role (with GEMINI_HARNESS_ENABLED) gets the harness env, GEMINI_MODEL
// from the account model, the workspace-trust pin and the ~/.gemini state mount;
// none of the Claude model pins survive (ADR 0005 phase 2).
func TestHarness_GeminiRoleSpec(t *testing.T) {
	cfg := pinRoleAccount(onlyAccount(newHarnessCfg("", "", "gemini"), "gemini-acct"), "gemini-acct")
	cfg.GeminiHarnessEnabled = true

	for name, build := range map[string]func(*config.Config, map[string]string) podmanager.AgentPodSpec{
		"events": func(c *config.Config, md map[string]string) podmanager.AgentPodSpec {
			return buildAgentPodSpec(c, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: md})
		},
		"reconciler": func(c *config.Config, md map[string]string) podmanager.AgentPodSpec {
			return BuildSpecFromBeadInfo(c, "gardenio", "crew", "crew", "a", md)
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := build(cfg, map[string]string{})
			if spec.HarnessError != "" {
				t.Fatalf("unexpected HarnessError %q", spec.HarnessError)
			}
			if spec.Harness != "gemini" || spec.Env[EnvHarness] != "gemini" {
				t.Errorf("Harness=%q %s=%q, want gemini/gemini", spec.Harness, EnvHarness, spec.Env[EnvHarness])
			}
			if got := spec.Env["GEMINI_MODEL"]; got != "gemini-2.5-pro" {
				t.Errorf("GEMINI_MODEL = %q, want the account model gemini-2.5-pro", got)
			}
			if got := spec.Env["GEMINI_CLI_TRUST_WORKSPACE"]; got != "true" {
				t.Errorf("GEMINI_CLI_TRUST_WORKSPACE = %q, want true (untrusted workspace exits 55)", got)
			}
			for _, k := range []string{
				"CLAUDE_MODEL", "ANTHROPIC_MODEL", "CODEX_MODEL", EnvModelPinned,
				"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
			} {
				if v, ok := spec.Env[k]; ok {
					t.Errorf("gemini spec must not set %s (got %q)", k, v)
				}
			}
			if p, sp := podmanager.HarnessStateMount(spec.Harness); p != "/home/agent/.gemini" || sp != ".state/gemini" {
				t.Errorf("state mount = %q/%q, want /home/agent/.gemini .state/gemini", p, sp)
			}
		})
	}
}

// A role/agent model override flows into CODEX_MODEL, and the agent model wins.
func TestHarness_CodexModelOverrides(t *testing.T) {
	cfg := onlyAccount(newHarnessCfg("", "", "codex"), "codex-acct")
	cfg.RoleCache["crew"] = config.RoleCacheEntry{Harness: "codex", Model: "role-model", Account: "codex-acct"}

	spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if spec.Env["CODEX_MODEL"] != "role-model" {
		t.Errorf("CODEX_MODEL = %q, want role-model", spec.Env["CODEX_MODEL"])
	}
	spec = buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{"model": "agent-model"}})
	if spec.Env["CODEX_MODEL"] != "agent-model" {
		t.Errorf("CODEX_MODEL = %q, want agent-model", spec.Env["CODEX_MODEL"])
	}
}

// The agent bead's harness metadata overrides the role.
func TestHarness_AgentMetadataOverridesRole(t *testing.T) {
	cfg := pinRoleAccount(onlyAccount(newHarnessCfg("", "", "claude"), "codex-acct"), "codex-acct")
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "a", map[string]string{"harness": "codex"})
	if spec.Harness != "codex" || spec.HarnessError != "" {
		t.Errorf("Harness=%q err=%q, want codex", spec.Harness, spec.HarnessError)
	}
}

// Provider/harness mismatches and unknown values set HarnessError, and the spawn
// guard turns that into a permanent, escalation-free, readable refusal.
func TestHarness_ProviderMismatchRefused(t *testing.T) {
	tests := []struct {
		name, account, role, wantInErr string
	}{
		{"codex on anthropic account", "gardenio", "codex", `needs an account with provider "openai"`},
		{"claude on openai account", "codex-acct", "claude", `cannot use account "codex-acct"`},
		{"unknown value", "gardenio", "gpt", `unknown harness "gpt"`},
		{"codex with no account resolved", "", "codex", `needs an account with provider`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := onlyAccount(newHarnessCfg("", "", tc.role), tc.account)
			if tc.account == "codex-acct" {
				pinRoleAccount(cfg, tc.account) // openai accounts resolve only via a role pin
			}
			spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
			if !strings.Contains(spec.HarnessError, tc.wantInErr) {
				t.Fatalf("HarnessError = %q, want containing %q", spec.HarnessError, tc.wantInErr)
			}
			r := evaluateSpawnCredentials(context.Background(), nil, slog.Default(), spec)
			if !r.refuse || !r.permanent || !r.harness {
				t.Errorf("refusal = %+v, want refuse+permanent+harness", r)
			}
			if !strings.Contains(r.reason, tc.wantInErr) {
				t.Errorf("reason = %q, want containing %q", r.reason, tc.wantInErr)
			}
		})
	}
}

// A codex role with no role pin spreads over the project's openai accounts
// (ADR 0004 §4), and fails closed when the project has none.
func TestHarness_CodexSpreadsOverProjectOpenAIAccounts(t *testing.T) {
	cfg := newHarnessCfg("", "", "codex") // claude + openai accounts both labeled project:gardenio, no role pin
	spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if spec.HarnessError != "" {
		t.Fatalf("unexpected HarnessError %q", spec.HarnessError)
	}
	if spec.ResolvedAccount != "codex-acct" || spec.CodexAuthSecret != "gardenio-codex-auth-session" {
		t.Errorf("ResolvedAccount=%q CodexAuthSecret=%q, want codex-acct and its -session Secret", spec.ResolvedAccount, spec.CodexAuthSecret)
	}

	onlyClaude := onlyAccount(newHarnessCfg("", "", "codex"), "gardenio")
	spec = buildAgentPodSpec(onlyClaude, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if !strings.Contains(spec.HarnessError, "needs an account with provider") {
		t.Fatalf("HarnessError = %q, want fail-closed no-openai-account message", spec.HarnessError)
	}
	if spec.CodexAuthSecret != "" {
		t.Errorf("no codex credential expected, got %q", spec.CodexAuthSecret)
	}
	r := evaluateSpawnCredentials(context.Background(), nil, slog.Default(), spec)
	if !r.refuse || !r.permanent || !r.harness {
		t.Errorf("refusal = %+v, want refuse+permanent+harness", r)
	}
}

// harnessRecordingServer is a beads daemon stub that records every request body,
// so a test can assert the agent bead was marked failed with a harness reason.
func harnessRecordingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// The stuck-restart recreate path honours spec.HarnessError (gd-helNySn0WA): a
// misconfigured codex agent must not silently come back as a Claude pod. The
// recreate is refused (no CreateAgentPod) and the bead is marked failed with the
// harness reason.
func TestHandleEvent_StuckRestartRefusesInvalidHarness(t *testing.T) {
	srv, requests := harnessRecordingServer(t)
	mgr := &createCountingManager{}
	cfg := onlyAccount(newHarnessCfg("", "", "codex"), "gardenio") // codex on an anthropic account
	cfg.Namespace = "gardenio"
	event := subscriber.Event{
		Type: subscriber.AgentStuck, Project: "gardenio", Role: "crew",
		AgentName: "gardenio-crew-x1", BeadID: "gd-StuckH",
		Metadata: map[string]string{"reason": "no coop seq advance", "source": "stuck-heuristic"},
	}
	c := newClient(t, srv)
	err := handleEvent(context.Background(), discardLogger(), cfg, event, mgr,
		nil, fake.NewSimpleClientset(), c, c, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("handleEvent err = %v, want nil (refusal is not a retryable error)", err)
	}
	if mgr.created != 0 {
		t.Fatalf("created %d pods, want 0 (stuck-restart must not recreate a misconfigured harness)", mgr.created)
	}
	var marked bool
	for _, req := range requests() {
		if strings.Contains(req, "gd-StuckH") && strings.Contains(req, "invalid agent harness") &&
			strings.Contains(req, `needs an account with provider`) {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("bead gd-StuckH not marked failed with the harness reason; requests: %v", requests())
	}
}

type fakeHarnessStamper struct {
	beadID, harness string
	calls           int
	err             error
}

func (f *fakeHarnessStamper) StampResolvedHarness(_ context.Context, beadID, harness string) error {
	f.calls++
	f.beadID, f.harness = beadID, harness
	return f.err
}

// TestStampResolvedHarness: the resolved harness (ADR 0004 section 4) is
// written to the agent bead; unresolved/errored specs, missing bead ids and a
// failing write are all non-fatal no-ops.
func TestStampResolvedHarness(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	t.Run("writes resolved harness", func(t *testing.T) {
		f := &fakeHarnessStamper{}
		stampResolvedHarness(ctx, logger, f, "bd-1", podmanager.AgentPodSpec{Harness: "codex"})
		if f.calls != 1 || f.beadID != "bd-1" || f.harness != "codex" {
			t.Errorf("got %+v, want one stamp bd-1=codex", f)
		}
	})
	t.Run("claude is stamped too", func(t *testing.T) {
		f := &fakeHarnessStamper{}
		stampResolvedHarness(ctx, logger, f, "bd-1", podmanager.AgentPodSpec{Harness: "claude"})
		if f.calls != 1 || f.harness != "claude" {
			t.Errorf("got %+v, want a claude stamp so consumers never re-resolve", f)
		}
	})
	t.Run("skips", func(t *testing.T) {
		for name, tc := range map[string]struct {
			id   string
			spec podmanager.AgentPodSpec
		}{
			"no bead id":      {"", podmanager.AgentPodSpec{Harness: "codex"}},
			"unresolved":      {"bd-1", podmanager.AgentPodSpec{}},
			"harness errored": {"bd-1", podmanager.AgentPodSpec{Harness: "codex", HarnessError: "bad"}},
		} {
			f := &fakeHarnessStamper{}
			stampResolvedHarness(ctx, logger, f, tc.id, tc.spec)
			if f.calls != 0 {
				t.Errorf("%s: stamped %+v, want no write", name, f)
			}
		}
		stampResolvedHarness(ctx, logger, nil, "bd-1", podmanager.AgentPodSpec{Harness: "codex"}) // nil stamper must not panic
	})
	t.Run("write error is swallowed", func(t *testing.T) {
		f := &fakeHarnessStamper{err: errors.New("daemon down")}
		stampResolvedHarness(ctx, logger, f, "bd-1", podmanager.AgentPodSpec{Harness: "codex"})
		if f.calls != 1 {
			t.Errorf("calls = %d, want the write attempted once", f.calls)
		}
	})
}

// codex on lambda_microvm is refused (gd-jyideibol0): the microvm image and
// credential path are Claude-only. The refusal rides HarnessError so every
// spawn door fails permanently; claude on microvm and codex on a pod are
// untouched.
func TestHarness_CodexOnMicrovmRefused(t *testing.T) {
	const mvm = podmanager.ExecutionTargetLambdaMicroVM
	tests := []struct {
		name, harness, target string
		wantRefused           bool
	}{
		{"codex on microvm", "codex", mvm, true},
		{"codex on kubernetes", "codex", "kubernetes", false},
		{"codex on default target", "codex", "", false},
		{"claude on microvm", "claude", mvm, false},
		{"gemini on microvm", "gemini", mvm, true},
		{"gemini on kubernetes", "gemini", "kubernetes", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := onlyAccount(newHarnessCfg("", "", tc.harness), "gardenio")
			if tc.harness == "codex" {
				cfg = pinRoleAccount(onlyAccount(newHarnessCfg("", "", tc.harness), "codex-acct"), "codex-acct")
			}
			if tc.harness == "gemini" {
				cfg = pinRoleAccount(onlyAccount(newHarnessCfg("", "", tc.harness), "gemini-acct"), "gemini-acct")
				cfg.GeminiHarnessEnabled = true
			}
			e := cfg.RoleCache["crew"]
			e.ExecutionTarget = tc.target
			cfg.RoleCache["crew"] = e

			for name, spec := range map[string]podmanager.AgentPodSpec{
				"event":    buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}}),
				"beadinfo": BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "a", map[string]string{}),
			} {
				if got := spec.HarnessError != ""; got != tc.wantRefused {
					t.Fatalf("%s: HarnessError = %q, refused=%v want %v", name, spec.HarnessError, got, tc.wantRefused)
				}
				if !tc.wantRefused {
					continue
				}
				if !strings.Contains(spec.HarnessError, `not supported on execution target "lambda_microvm"`) {
					t.Errorf("%s: HarnessError = %q, want microvm refusal", name, spec.HarnessError)
				}
				r := evaluateSpawnCredentials(context.Background(), nil, slog.Default(), spec)
				if !r.refuse || !r.permanent || !r.harness {
					t.Errorf("%s: refusal = %+v, want refuse+permanent+harness", name, r)
				}
			}
		})
	}
}
