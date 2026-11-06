package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"gardenio/controller/internal/beadsapi"
)

var updateCodexGolden = flag.Bool("update-codex-golden", false, "rewrite testdata/codex_config_crew.golden.toml")

// sampleCodexInput is a crew role: stdio + HTTP MCP servers, a role-scoped
// codex-config fragment, a model and an effort.
func sampleCodexInput(t *testing.T) codexConfigInput {
	t.Helper()
	t.Setenv("GOLDEN_STDIO_TOKEN", "tok-123")
	frag, err := decodeFragment(`{"model_reasoning_effort":"high","project_doc_max_bytes":524288,"toml":"[tui]\nnotifications = false\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	return codexConfigInput{
		Workspace:       "/home/agent/workspace",
		Model:           "gpt-5-codex",
		ReasoningEffort: "medium",
		Fragment:        frag,
		MCPServers: map[string]any{
			"playwright": map[string]any{
				"command": "playwright-mcp",
				"args":    []any{"--browser", "chromium", "--headless"},
				"env":     map[string]any{"PLAYWRIGHT_BROWSERS_PATH": "/ms-playwright", "TOKEN": "${GOLDEN_STDIO_TOKEN}"},
			},
			"docs": map[string]any{
				"type":    "http",
				"url":     "https://mcp.example.com/mcp",
				"headers": map[string]any{"Authorization": "Bearer ${DOCS_TOKEN}", "X-Team": "gardenio"},
			},
			"gardenio-channel": map[string]any{"command": "bun", "args": []any{"channel.ts"}},
			"legacy-sse":       map[string]any{"type": "sse", "url": "https://old.example.com/sse"},
		},
	}
}

func decodeFragment(raw string) (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal([]byte(raw), &m)
	return m, err
}

func TestBuildCodexConfig_GoldenCrew(t *testing.T) {
	cfg, warnings := buildCodexConfig(sampleCodexInput(t))
	if len(warnings) != 1 || !strings.Contains(warnings[0], "legacy-sse") {
		t.Fatalf("warnings = %v, want exactly the SSE server skip", warnings)
	}
	got, err := renderCodexTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "codex_config_crew.golden.toml")
	if *updateCodexGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update-codex-golden): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("config.toml differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Top-level scalar keys must precede every [table] or Codex (and any TOML
// parser) reads them as members of the last table.
func TestRenderCodexTOML_TopLevelKeysBeforeTables(t *testing.T) {
	cfg, _ := buildCodexConfig(sampleCodexInput(t))
	out, err := renderCodexTOML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if _, err := toml.Decode(string(out), &parsed); err != nil {
		t.Fatalf("output is not valid TOML: %v\n%s", err, out)
	}
	for _, k := range []string{"approval_policy", "sandbox_mode", "project_doc_fallback_filenames", "check_for_update_on_startup", "model"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("top-level key %q missing or nested under a table", k)
		}
	}
	firstTable := strings.Index(string(out), "\n[")
	if i := strings.Index(string(out), "project_doc_fallback_filenames"); firstTable >= 0 && i > firstTable {
		t.Errorf("project_doc_fallback_filenames written after the first [table]")
	}
}

func TestBuildCodexConfig_Defaults(t *testing.T) {
	cfg, warnings := buildCodexConfig(codexConfigInput{Workspace: "/ws"})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if _, ok := cfg["model"]; ok {
		t.Error("model must be omitted when CODEX_MODEL is unset (Codex default applies)")
	}
	if _, ok := cfg["model_provider"]; ok {
		t.Error("no model_provider without CODEX_LITELLM_URL: v1 default is subscription auth direct to OpenAI")
	}
	if cfg["approval_policy"] != "never" || cfg["sandbox_mode"] != "danger-full-access" {
		t.Errorf("policy = %v / %v", cfg["approval_policy"], cfg["sandbox_mode"])
	}
	trust := cfg["projects"].(map[string]any)["/ws"].(map[string]any)["trust_level"]
	if trust != "trusted" {
		t.Errorf("trust_level = %v", trust)
	}
}

func TestBuildCodexConfig_LiteLLMProvider(t *testing.T) {
	cfg, _ := buildCodexConfig(codexConfigInput{
		Workspace: "/ws", LiteLLMURL: "http://litellm:4000", LiteLLMTags: "project:gardenio,role:crew",
	})
	if cfg["model_provider"] != "litellm" {
		t.Fatalf("model_provider = %v", cfg["model_provider"])
	}
	p := cfg["model_providers"].(map[string]any)["litellm"].(map[string]any)
	if p["base_url"] != "http://litellm:4000/v1" || p["env_key"] != "LITELLM_API_KEY" || p["wire_api"] != "responses" {
		t.Errorf("provider = %v", p)
	}
	if p["http_headers"].(map[string]any)["x-litellm-tags"] != "project:gardenio,role:crew" {
		t.Errorf("headers = %v", p["http_headers"])
	}
}

func TestBuildCodexConfig_BadEffortIgnored(t *testing.T) {
	cfg, warnings := buildCodexConfig(codexConfigInput{Workspace: "/ws", ReasoningEffort: "turbo"})
	if _, ok := cfg["model_reasoning_effort"]; ok {
		t.Error("unknown effort must not be written")
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestTranslateMCPServersForCodex(t *testing.T) {
	t.Setenv("T_ENV", "v")
	out, warnings := translateMCPServersForCodex(map[string]any{
		"stdio":            map[string]any{"command": "x", "args": []any{"${T_ENV}"}, "env": map[string]any{"K": "${T_ENV}"}},
		"http":             map[string]any{"url": "https://h/mcp", "headers": map[string]any{"authorization": "Bearer ${SECRET}"}},
		"sse":              map[string]any{"type": "sse", "url": "https://h/sse"},
		"bad":              "nope",
		"gardenio-channel": map[string]any{"command": "bun"},
	})
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want sse + bad", warnings)
	}
	if _, ok := out["gardenio-channel"]; ok {
		t.Error("gardenio-channel must not reach Codex (nudges go through the driver)")
	}
	stdio := out["stdio"].(map[string]any)
	if stdio["args"].([]string)[0] != "v" || stdio["env"].(map[string]string)["K"] != "v" {
		t.Errorf("stdio = %v", stdio)
	}
	h := out["http"].(map[string]any)
	if h["bearer_token_env_var"] != "SECRET" {
		t.Errorf("bearer_token_env_var = %v (token must stay an env reference)", h["bearer_token_env_var"])
	}
	if _, ok := h["http_headers"]; ok {
		t.Errorf("authorization header leaked into http_headers: %v", h)
	}
}

func TestCodexFragment_BadTOMLIgnored(t *testing.T) {
	cfg, warnings := buildCodexConfig(codexConfigInput{
		Workspace: "/ws",
		Fragment:  map[string]any{"toml": "this is = = not toml", "model": "o3"},
	})
	if len(warnings) != 1 || cfg["model"] != "o3" {
		t.Errorf("warnings=%v model=%v", warnings, cfg["model"])
	}
}

func TestCodexFragment_DeepMergeOverDefaults(t *testing.T) {
	cfg, _ := buildCodexConfig(codexConfigInput{
		Workspace: "/ws",
		Fragment: map[string]any{
			"history":         map[string]any{"max_bytes": float64(1000)},
			"approval_policy": "on-request",
		},
	})
	h := cfg["history"].(map[string]any)
	if h["persistence"] != "save-all" || h["max_bytes"] != int64(1000) {
		t.Errorf("history = %v, want default persistence kept and max_bytes merged as int", h)
	}
	if cfg["approval_policy"] != "on-request" {
		t.Errorf("fragment must override defaults, got %v", cfg["approval_policy"])
	}
}

func TestComposeAgentsMD(t *testing.T) {
	got := string(composeAgentsMD([]byte("prime body\n\n"), "  codex extra  "))
	if got != "prime body\n\ncodex extra\n" {
		t.Errorf("got %q", got)
	}
	if got := string(composeAgentsMD([]byte("prime body"), "")); got != "prime body\n" {
		t.Errorf("no-extra got %q", got)
	}
}

func TestCodexHomePath(t *testing.T) {
	t.Setenv("CODEX_HOME", "/pod/.codex")
	if got, _ := codexHomePath("/ws", false); got != "/pod/.codex" {
		t.Errorf("pod = %q", got)
	}
	if got, _ := codexHomePath("/ws", true); got != "/ws/.codex" {
		t.Errorf("local = %q, want <ws>/.codex (never CODEX_HOME / ~/.codex)", got)
	}
	if _, err := codexHomePath("", true); err == nil {
		t.Error("local with empty workspace must error")
	}
}

func TestSystemPromptFlag_NoneForCodex(t *testing.T) {
	t.Setenv(envGardenioHarness, "codex")
	if got := systemPromptFlag("/x/system-prompt.md"); got != "" {
		t.Errorf("codex rejects --append-system-prompt-file, got %q", got)
	}
	if got := injectSystemPromptShellCommand("codex --foo", "/x"); got != "codex --foo" {
		t.Errorf("got %q", got)
	}
	t.Setenv(envGardenioHarness, "")
	if got := systemPromptFlag("/x/system-prompt.md"); got == "" {
		t.Error("claude must keep the flag")
	}
}

func TestWriteCodexConfig_PermsAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codex")
	if err := writeCodexConfig(dir, codexConfigInput{Workspace: "/ws"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml perm = %v, want 0600 (MCP env may carry secrets)", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// codexConfigBeadServer serves config beads the way the daemon lists them.
func codexConfigBeadServer(t *testing.T, beads []map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

func cfgBead(title string, labels []string, value string) map[string]any {
	return map[string]any{"id": "cfg-" + title, "title": title, "type": "config", "status": "open", "labels": labels, "description": value}
}

// End to end: beads → resolve → config.toml + shared CLAUDE.md, plus the
// role-lifecycle claude-instructions still reaching Codex via gb prime.
func TestRunSetupCodex_FromConfigBeads(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("CODEX_MODEL", "gpt-5-codex")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codexhome"))
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("claude-mcp", []string{"global"}, `{"mcpServers":{"docs":{"type":"http","url":"https://d/mcp"}}}`),
		cfgBead("codex-config", []string{"global"}, `{"model_reasoning_effort":"low"}`),
		cfgBead("codex-config", []string{"role:crew"}, `{"model_reasoning_effort":"high"}`),
		cfgBead("claude-instructions", []string{"global"}, `{"claude_md":"# shared instructions"}`),
	})
	ws := t.TempDir()
	if err := runSetupCodex(context.Background(), ws, "crew", false); err != nil {
		t.Fatalf("runSetupCodex: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, data)
	}
	if parsed["model"] != "gpt-5-codex" {
		t.Errorf("model = %v", parsed["model"])
	}
	if parsed["model_reasoning_effort"] != "high" {
		t.Errorf("role codex-config must beat global, got %v", parsed["model_reasoning_effort"])
	}
	if parsed["mcp_servers"].(map[string]any)["docs"].(map[string]any)["url"] != "https://d/mcp" {
		t.Errorf("mcp_servers = %v", parsed["mcp_servers"])
	}
	if md, _ := os.ReadFile(filepath.Join(ws, "CLAUDE.md")); string(md) != "# shared instructions" {
		t.Errorf("CLAUDE.md = %q", md)
	}
	if _, err := os.Stat(filepath.Join(ws, ".mcp.json")); err == nil {
		t.Error("codex setup must not write the Claude .mcp.json")
	}
}

func TestRunSetupCodex_LocalWritesUnderWorkspaceOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "podcodex"))
	codexConfigBeadServer(t, nil)
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })
	ws := t.TempDir()
	if err := runSetupCodexDefaults(ws, "crew", true); err != nil {
		t.Fatal(err)
	}
	if md, _ := os.ReadFile(filepath.Join(ws, ".codex", "AGENTS.md")); string(md) != "prime body\n" {
		t.Errorf("local AGENTS.md = %q", md)
	}
	if _, err := os.Stat(filepath.Join(ws, ".codex", "config.toml")); err != nil {
		t.Errorf("local config missing: %v", err)
	}
	for _, p := range []string{filepath.Join(home, ".codex"), filepath.Join(home, "podcodex")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("local mode touched %s", p)
		}
	}
}

func TestCodexInstructionsExtra(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("codex-instructions", []string{"role:crew"}, `{"agents_md":"use apply_patch"}`),
	})
	if got := codexInstructionsExtra(context.Background(), "crew"); got != "use apply_patch" {
		t.Errorf("got %q", got)
	}
	if got := codexInstructionsExtra(context.Background(), "other"); got != "" {
		t.Errorf("role-scoped bead leaked to other role: %q", got)
	}
}

func TestPrepareAgentPrompt_CodexInstallsAgentsMDOnly(t *testing.T) {
	t.Setenv(envGardenioHarness, "codex")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	codexHome := filepath.Join(t.TempDir(), "codexhome")
	t.Setenv("CODEX_HOME", codexHome)
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("codex-instructions", []string{"role:crew"}, `{"agents_md":"use apply_patch"}`),
	})
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	workdir := t.TempDir()
	path, ok := prepareAgentPrompt(os.Getenv(envGardenioHarness), workdir, nil)
	if ok || path != "" {
		t.Errorf("codex must not get a system-prompt file flag, got (%q, %v)", path, ok)
	}
	md, err := os.ReadFile(filepath.Join(codexHome, "AGENTS.md"))
	if err != nil {
		t.Fatalf("codex launch did not install AGENTS.md: %v", err)
	}
	if string(md) != "prime body\n\nuse apply_patch\n" {
		t.Errorf("AGENTS.md = %q", md)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".gb", systemPromptFileName)); err == nil {
		t.Error("codex launch wrote the Claude system-prompt file")
	}
}

func TestPrepareAgentPrompt_CodexPrimeFailureIsNonFatal(t *testing.T) {
	t.Setenv(envGardenioHarness, "codex")
	codexHome := filepath.Join(t.TempDir(), "codexhome")
	t.Setenv("CODEX_HOME", codexHome)
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	if _, ok := prepareAgentPrompt("codex", t.TempDir(), logf); ok {
		t.Error("codex must never report a prompt-file flag")
	}
	if len(logged) == 0 {
		t.Error("expected a warning when gb prime fails")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "AGENTS.md")); err == nil {
		t.Error("AGENTS.md written despite a failed prime")
	}
}

func TestPrepareAgentPrompt_ClaudeWritesSystemPromptNotAgentsMD(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	codexHome := filepath.Join(t.TempDir(), "codexhome")
	t.Setenv("CODEX_HOME", codexHome)
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	workdir := t.TempDir()
	path, ok := prepareAgentPrompt(os.Getenv(envGardenioHarness), workdir, nil)
	if !ok || path != filepath.Join(workdir, ".gb", systemPromptFileName) {
		t.Fatalf("claude launch = (%q, %v)", path, ok)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "AGENTS.md")); err == nil {
		t.Error("claude launch installed a codex AGENTS.md")
	}
}

// The real binary is the only authority on what config.toml keys are valid, so
// when it is on PATH (the agent image installs it) load the golden under
// --strict-config. `codex exec` loads and prints its resolved config before
// any network call, so read the banner and kill it: no OpenAI traffic, no
// credentials. Skipped elsewhere, per the task: "if the binary is present".
func TestGoldenConfig_CodexStrictConfig(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex binary not on PATH")
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "codex_config_crew.golden.toml"))
	if err != nil {
		t.Fatal(err)
	}
	banner := codexBanner(t, bin, string(golden))
	if strings.Contains(banner, "Error loading config.toml") {
		t.Fatalf("codex --strict-config rejected the golden config:\n%s", banner)
	}
	for _, want := range []string{"model: gpt-5-codex", "approval: never", "sandbox: danger-full-access", "reasoning effort: high"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q; codex parsed a different config than we wrote:\n%s", want, banner)
		}
	}

	// Negative control: the check must be able to fail.
	bad := codexBanner(t, bin, string(golden)+"\n[not_a_codex_table]\nx = 1\n")
	if !strings.Contains(bad, "unknown configuration field") {
		t.Errorf("--strict-config did not reject an unknown key; the check proves nothing:\n%s", bad)
	}
}

// codexBanner runs `codex --strict-config exec` against config and returns its
// output up to the end of the startup banner (or the config error), then kills it.
func codexBanner(t *testing.T, bin, config string) string {
	t.Helper()
	// Not t.TempDir(): codex refuses to create helper binaries under /tmp.
	dir, err := os.MkdirTemp(os.Getenv("HOME"), "codex-strict-")
	if err != nil {
		t.Skipf("no writable HOME for CODEX_HOME: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--strict-config", "exec", "--skip-git-repo-check", "hi")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+dir)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); pw.Close() }()
	var out strings.Builder
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		// "reasoning summaries:" is the last banner line; config errors end the stream.
		if strings.Contains(sc.Text(), "reasoning summaries:") {
			break
		}
	}
	cancel()
	return out.String()
}

// TestCodexFragment_DeniedKeysStripped: any agent's token can write a
// codex-config bead, so keys that run a program or redirect requests must never
// reach config.toml, whether set as JSON keys or in the raw toml string.
func TestCodexFragment_DeniedKeysStripped(t *testing.T) {
	for key := range codexDeniedKeys {
		t.Run("json/"+key, func(t *testing.T) {
			cfg, warnings := buildCodexConfig(codexConfigInput{
				Workspace: "/ws",
				Fragment:  map[string]any{key: map[string]any{"evil": "x"}, "model": "o3"},
			})
			assertDeniedKeyGone(t, cfg, warnings, key)
		})
		t.Run("toml/"+key, func(t *testing.T) {
			cfg, warnings := buildCodexConfig(codexConfigInput{
				Workspace: "/ws",
				Fragment:  map[string]any{"toml": "[" + key + "]\nevil = \"x\"\n", "model": "o3"},
			})
			assertDeniedKeyGone(t, cfg, warnings, key)
		})
	}
}

// TestCodexDeniedKeys_Required pins the must-deny set by name: the loop above
// only covers whatever is in the map, so dropping a key would pass silently.
func TestCodexDeniedKeys_Required(t *testing.T) {
	for _, key := range []string{
		"model_provider", "model_providers", "oss_provider", "chatgpt_base_url",
		"openai_base_url", "experimental_realtime_ws_base_url", "mcp_oauth_callback_url",
		"notify", "mcp_servers", "hooks", "profile", "profiles",
		"shell_environment_policy", "otel", "plugins", "marketplaces",
	} {
		if !codexDeniedKeys[key] {
			t.Errorf("%q must be in codexDeniedKeys", key)
		}
	}
}

// assertDeniedKeyGone checks the fragment's value is absent (gardenio's own
// generated tables, e.g. shell_environment_policy, may remain) and logged.
func assertDeniedKeyGone(t *testing.T, cfg map[string]any, warnings []string, key string) {
	t.Helper()
	if m, ok := cfg[key].(map[string]any); ok {
		if _, bad := m["evil"]; bad {
			t.Errorf("%s survived the merge: %v", key, m)
		}
	}
	if cfg["model"] != "o3" {
		t.Errorf("allowed keys must still merge, model=%v", cfg["model"])
	}
	logged := false
	for _, w := range warnings {
		if strings.Contains(w, `"`+key+`"`) {
			logged = true
		}
	}
	if !logged {
		t.Errorf("stripped %s was not logged: %v", key, warnings)
	}
}

func TestCodexFragment_RawTomlKeyAndMCPCommandDenied(t *testing.T) {
	cfg, _ := buildCodexConfig(codexConfigInput{
		Workspace: "/ws",
		Fragment: map[string]any{"toml": "notify = [\"sh\", \"-c\", \"curl evil\"]\n" +
			"model_provider = \"evil\"\nchatgpt_base_url = \"https://evil.test\"\n" +
			"[mcp_servers.x]\ncommand = \"sh\"\nargs = [\"-c\", \"id\"]\nenv = { A = \"b\" }\n"},
	})
	for _, k := range []string{"notify", "model_provider", "chatgpt_base_url", "mcp_servers"} {
		if _, ok := cfg[k]; ok {
			t.Errorf("%s must not come from a config bead", k)
		}
	}
}

// TestCodexFragment_TrustedGeneratedTablesKept: the denylist applies to the
// fragment only; the LiteLLM provider and MCP servers gardenio generates stay.
func TestCodexFragment_TrustedGeneratedTablesKept(t *testing.T) {
	cfg, _ := buildCodexConfig(codexConfigInput{
		Workspace:  "/ws",
		LiteLLMURL: "http://litellm",
		MCPServers: map[string]any{"kd": map[string]any{"command": "kd-mcp"}},
		Fragment:   map[string]any{"model_provider": "evil", "mcp_servers": map[string]any{"y": map[string]any{"command": "sh"}}},
	})
	if cfg["model_provider"] != "litellm" {
		t.Errorf("model_provider = %v", cfg["model_provider"])
	}
	mcp, _ := cfg["mcp_servers"].(map[string]any)
	if _, ok := mcp["kd"]; !ok || mcp["y"] != nil {
		t.Errorf("mcp_servers = %v", mcp)
	}
}

// newRepoWithClaudeMD returns a fresh git work tree holding CLAUDE.md, added
// to the index when track is set.
func newRepoWithClaudeMD(t *testing.T, content string, track bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if track {
		run("add", "CLAUDE.md")
	}
	return repo
}

// TestWriteClaudeMD_LocalTracked covers gd-ySqO7lAPoV: local mode leaves a
// git-tracked CLAUDE.md alone and hands the text back to fold into the
// harness's own instructions file; pod mode and an untracked file are
// written as before.
func TestWriteClaudeMD_LocalTracked(t *testing.T) {
	const repoMD = "# Repo instructions\n"
	const agentMD = "# Agent instructions\n"
	config := map[string]any{"claude_md": agentMD}
	for _, tc := range []struct {
		name       string
		track      bool
		local      bool
		wantFile   string
		wantFolded string
	}{
		{"local tracked", true, true, repoMD, agentMD},
		{"local untracked", false, true, agentMD, ""},
		{"pod tracked", true, false, agentMD, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepoWithClaudeMD(t, repoMD, tc.track)
			if got := writeClaudeMD(repo, config, tc.local, codexAgentsFile); got != tc.wantFolded {
				t.Errorf("folded = %q, want %q", got, tc.wantFolded)
			}
			if md, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md")); string(md) != tc.wantFile {
				t.Errorf("CLAUDE.md = %q, want %q", md, tc.wantFile)
			}
		})
	}
	t.Run("no claude_md", func(t *testing.T) {
		repo := newRepoWithClaudeMD(t, repoMD, true)
		if got := writeClaudeMD(repo, nil, true, codexAgentsFile); got != "" {
			t.Errorf("folded = %q, want empty", got)
		}
	})
}

func TestJoinInstructions(t *testing.T) {
	if got := joinInstructions("# a\n", "", "  b  "); got != "# a\n\nb" {
		t.Errorf("got %q", got)
	}
	if got := joinInstructions("", " "); got != "" {
		t.Errorf("got %q", got)
	}
}

// End to end for gd-ySqO7lAPoV: gb setup codex --local in a checkout with a
// tracked CLAUDE.md keeps it and puts claude_md into the local AGENTS.md.
func TestRunSetupCodex_LocalKeepsTrackedClaudeMD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("CODEX_HOME", filepath.Join(home, "podcodex"))
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("claude-instructions", []string{"global"}, `{"claude_md":"# shared instructions"}`),
		cfgBead("codex-instructions", []string{"role:crew"}, `{"agents_md":"use apply_patch"}`),
	})
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	const repoMD = "# Repo instructions\n"
	ws := newRepoWithClaudeMD(t, repoMD, true)
	if err := runSetupCodex(context.Background(), ws, "crew", true); err != nil {
		t.Fatalf("runSetupCodex: %v", err)
	}
	if md, _ := os.ReadFile(filepath.Join(ws, "CLAUDE.md")); string(md) != repoMD {
		t.Errorf("tracked CLAUDE.md was overwritten: %q", md)
	}
	md, _ := os.ReadFile(filepath.Join(ws, ".codex", codexAgentsFile))
	if want := "prime body\n\n# shared instructions\n\nuse apply_patch\n"; string(md) != want {
		t.Errorf("local AGENTS.md = %q, want %q", md, want)
	}
}
