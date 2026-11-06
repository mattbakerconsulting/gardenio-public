package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGeminiGolden = flag.Bool("update-gemini-golden", false, "rewrite testdata/gemini_settings_crew.golden.json")

// sampleGeminiInput is a crew role: stdio + streamable-HTTP + SSE MCP servers,
// a role-scoped gemini-config fragment and a pinned model. Hooks are the
// translated built-in defaults, so the golden also pins the hook translation.
func sampleGeminiInput(t *testing.T) geminiSettingsInput {
	t.Helper()
	t.Setenv("GOLDEN_STDIO_TOKEN", "tok-123")
	t.Setenv("DOCS_TOKEN", "docs-456")
	for _, k := range []string{"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"} {
		t.Setenv(k, "")
	}
	frag, err := decodeFragment(`{"model":{"maxSessionTurns":50},"ui":{"hideBanner":true},"hooks":{"BeforeTool":[{"hooks":[{"type":"command","command":"evil"}]}]},"telemetry":{"enabled":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, _, _, _ := buildGeminiHooks(harnessHooksSource(nil))
	return geminiSettingsInput{
		Model:    "gemini-2.5-pro",
		Fragment: frag,
		Hooks:    doc,
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
			"bad":              map[string]any{"type": "websocket", "url": "wss://x"},
		},
	}
}

func TestBuildGeminiSettings_GoldenCrew(t *testing.T) {
	cfg, warnings := buildGeminiSettings(sampleGeminiInput(t))
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{`"hooks"`, `"telemetry"`, `mcp server "bad"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
	if len(warnings) != 3 {
		t.Errorf("warnings = %v, want exactly: hooks and telemetry stripped, websocket server skipped", warnings)
	}
	got, err := renderGeminiSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "gemini_settings_crew.golden.json")
	if *updateGeminiGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update-gemini-golden): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("settings.json differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The three first-run gates of spike R7 must always be cleared, whatever the
// fragment says: without selectedType the CLI exits 41, and statistics/update
// checks phone home.
func TestBuildGeminiSettings_PinnedGatesSurviveFragment(t *testing.T) {
	frag := map[string]any{
		"security": map[string]any{"auth": map[string]any{"selectedType": "oauth-personal"}},
		"general":  map[string]any{"enableAutoUpdate": true},
		"privacy":  map[string]any{"usageStatisticsEnabled": true},
		"proxy":    "http://evil",
		"tools":    map[string]any{"discoveryCommand": "evil"},
	}
	cfg, warnings := buildGeminiSettings(geminiSettingsInput{Fragment: frag})
	if len(warnings) != 5 {
		t.Errorf("warnings = %v, want all five fragment keys stripped", warnings)
	}
	raw, _ := json.Marshal(cfg)
	var got struct {
		Security struct{ Auth struct{ SelectedType string } }
		General  struct{ EnableAutoUpdate, EnableAutoUpdateNotification bool }
		Privacy  struct{ UsageStatisticsEnabled bool }
		Proxy    any
		Tools    any
		Model    any
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Security.Auth.SelectedType != "gemini-api-key" || got.General.EnableAutoUpdate ||
		got.General.EnableAutoUpdateNotification || got.Privacy.UsageStatisticsEnabled {
		t.Errorf("pinned gates changed: %s", raw)
	}
	if got.Proxy != nil || got.Tools != nil {
		t.Errorf("denied keys leaked: %s", raw)
	}
	if got.Model != nil {
		t.Errorf("model.name set without GEMINI_MODEL: %s", raw)
	}
}

func TestTranslateMCPServersForGemini(t *testing.T) {
	t.Setenv("T", "secret")
	out, warnings := translateMCPServersForGemini(map[string]any{
		"stdio":            map[string]any{"command": "srv", "args": []any{"--k", "${T}"}, "env": map[string]any{"A": "${T}"}},
		"http":             map[string]any{"type": "http", "url": "https://h/mcp", "headers": map[string]any{"Authorization": "Bearer ${T}"}},
		"sse":              map[string]any{"type": "sse", "url": "https://h/sse"},
		"gardenio-channel": map[string]any{"command": "bun"},
		"junk":             "not an object",
	})
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"junk"`) {
		t.Errorf("warnings = %v", warnings)
	}
	if _, ok := out["gardenio-channel"]; ok {
		t.Error("gardenio-channel must be dropped silently")
	}
	for name, srv := range out {
		if srv.(map[string]any)["trust"] != true {
			t.Errorf("%s: not trusted (a per-tool prompt would stall an approval-less turn)", name)
		}
	}
	if h := out["http"].(map[string]any); h["httpUrl"] != "https://h/mcp" || h["url"] != nil ||
		h["headers"].(map[string]string)["Authorization"] != "Bearer secret" {
		t.Errorf("http server = %v", h)
	}
	if s := out["sse"].(map[string]any); s["url"] != "https://h/sse" || s["httpUrl"] != nil {
		t.Errorf("sse server = %v", s)
	}
	if s := out["stdio"].(map[string]any); s["args"].([]string)[1] != "secret" || s["env"].(map[string]string)["A"] != "secret" {
		t.Errorf("stdio server = %v", s)
	}
}

func TestWriteGeminiSettingsAndContextMD(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	t.Setenv("GEMINI_MODEL", "")
	in := geminiSettingsInputFromEnv()
	in.Model = "gemini-2.5-pro"
	if err := writeGeminiSettings(home, in, defaultHookSettings()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(home, geminiSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("settings.json mode = %v, want 0600 (MCP env may carry secrets)", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(home, geminiSettingsFile))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings.json is not JSON: %v", err)
	}
	if doc["model"].(map[string]any)["name"] != "gemini-2.5-pro" {
		t.Errorf("model = %v", doc["model"])
	}

	if err := writeGeminiContextMD(home, []byte("# prime\n\n"), "extra"); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(home, geminiContextFile))
	if string(md) != "# prime\n\nextra\n" {
		t.Errorf("GEMINI.md = %q", md)
	}
}

func TestGeminiHomePath(t *testing.T) {
	t.Setenv("HOME", "/home/agent")
	t.Setenv("GEMINI_CLI_HOME", "")
	if got, _ := geminiHomePath("", false); got != "/home/agent/.gemini" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("GEMINI_CLI_HOME", "/data")
	if got, _ := geminiHomePath("", false); got != "/data/.gemini" {
		t.Errorf("GEMINI_CLI_HOME = %q", got)
	}
	if got, _ := geminiHomePath("/ws", true); got != "/ws/.gemini" {
		t.Errorf("local = %q", got)
	}
	if _, err := geminiHomePath("", true); err == nil {
		t.Error("local mode with no workspace must fail")
	}
}

func TestRunSetupGeminiDefaults_LocalConfinesWrites(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("GEMINI_MODEL", "gemini-2.5-flash")
	stubPrime := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("PRIME"), nil }
	defer func() { runPrimeSystemPrompt = stubPrime }()

	if err := runSetupGeminiDefaults(ws, "crew", true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{geminiSettingsFile, geminiContextFile} {
		if _, err := os.Stat(filepath.Join(ws, ".gemini", f)); err != nil {
			t.Errorf("local mode did not write %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("local mode touched the operator's ~/.gemini")
	}
	md, _ := os.ReadFile(filepath.Join(ws, ".gemini", geminiContextFile))
	if strings.TrimSpace(string(md)) != "PRIME" {
		t.Errorf("GEMINI.md = %q", md)
	}
}

func TestSetupGeminiSettings_MockModeWritesEmptySettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	setupGeminiSettings(t.TempDir(), "crew", true)
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", geminiSettingsFile))
	if err != nil || strings.TrimSpace(string(raw)) != "{}" {
		t.Errorf("mock-mode settings = %q, %v", raw, err)
	}
}

func TestBuildGeminiSettings_OAuthSelectsOauthPersonal(t *testing.T) {
	cfg, _ := buildGeminiSettings(geminiSettingsInput{OAuth: true})
	got := cfg["security"].(map[string]any)["auth"].(map[string]any)["selectedType"]
	if got != "oauth-personal" {
		t.Errorf("selectedType = %v, want oauth-personal", got)
	}
	cfg, _ = buildGeminiSettings(geminiSettingsInput{})
	got = cfg["security"].(map[string]any)["auth"].(map[string]any)["selectedType"]
	if got != "gemini-api-key" {
		t.Errorf("default selectedType = %v, want gemini-api-key", got)
	}
}

func TestGeminiSettingsInputFromEnv_AuthMode(t *testing.T) {
	t.Setenv("GEMINI_AUTH_MODE", "oauth")
	if !geminiSettingsInputFromEnv().OAuth {
		t.Error("GEMINI_AUTH_MODE=oauth should select OAuth")
	}
	t.Setenv("GEMINI_AUTH_MODE", "")
	if geminiSettingsInputFromEnv().OAuth {
		t.Error("unset GEMINI_AUTH_MODE must stay on the API key")
	}
}

func TestWriteGeminiSettings_OAuthInstallsLoginWritable(t *testing.T) {
	authDir := t.TempDir()
	login := `{"access_token":"a","refresh_token":"r","expiry_date":1}`
	if err := os.WriteFile(filepath.Join(authDir, "oauth_creds.json"), []byte(login), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARDENIO_GEMINI_AUTH_DIR", authDir)
	home := filepath.Join(t.TempDir(), ".gemini")
	if err := writeGeminiSettings(home, geminiSettingsInput{OAuth: true}, harnessHooksSource(nil)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(home, "oauth_creds.json")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != login {
		t.Fatalf("installed login = %q, %v", got, err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Errorf("oauth_creds.json mode = %v, want 0600 (the CLI rewrites it on refresh)", fi.Mode().Perm())
	}
}

func TestWriteGeminiSettings_OAuthWithoutMountedLoginFails(t *testing.T) {
	t.Setenv("GARDENIO_GEMINI_AUTH_DIR", t.TempDir())
	home := filepath.Join(t.TempDir(), ".gemini")
	err := writeGeminiSettings(home, geminiSettingsInput{OAuth: true}, harnessHooksSource(nil))
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("err = %v, want a 'not mounted' error", err)
	}
}

func TestWriteGeminiSettings_APIKeyDoesNotTouchLogin(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	if err := writeGeminiSettings(home, geminiSettingsInput{}, harnessHooksSource(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "oauth_creds.json")); !os.IsNotExist(err) {
		t.Errorf("API-key pod must not get an oauth_creds.json (stat err = %v)", err)
	}
}

// End to end for gd-ySqO7lAPoV: gb setup gemini --local in a checkout with a
// tracked CLAUDE.md keeps it and puts claude_md into the local GEMINI.md.
func TestRunSetupGemini_LocalKeepsTrackedClaudeMD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("BOAT_PROJECT", "gardenio")
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("claude-instructions", []string{"global"}, `{"claude_md":"# shared instructions"}`),
	})
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("PRIME\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	const repoMD = "# Repo instructions\n"
	ws := newRepoWithClaudeMD(t, repoMD, true)
	if err := runSetupGemini(context.Background(), ws, "crew", true); err != nil {
		t.Fatalf("runSetupGemini: %v", err)
	}
	if md, _ := os.ReadFile(filepath.Join(ws, "CLAUDE.md")); string(md) != repoMD {
		t.Errorf("tracked CLAUDE.md was overwritten: %q", md)
	}
	md, _ := os.ReadFile(filepath.Join(ws, ".gemini", geminiContextFile))
	if want := "PRIME\n\n# shared instructions\n"; string(md) != want {
		t.Errorf("local GEMINI.md = %q, want %q", md, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("local mode touched the operator's ~/.gemini")
	}
}

// gd-LOACIS8ZOA: gemini-config and gemini-instructions must be registered
// categories, or ResolveConfigBeads returns nil for them and the beads are
// silently ignored.
func TestResolveConfigBeads_GeminiCategories(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("gemini-config", []string{"global"}, `{"ui":{"hideBanner":true}}`),
		cfgBead("gemini-instructions", []string{"role:crew"}, `{"gemini_md":"use the shell tool"}`),
	})
	subs := buildSubscriptions("crew")
	cfg, n, err := ResolveConfigBeads(context.Background(), daemon, "gemini-config", subs)
	if err != nil || n != 1 {
		t.Fatalf("gemini-config: n=%d err=%v", n, err)
	}
	if ui, _ := cfg["ui"].(map[string]any); ui["hideBanner"] != true {
		t.Errorf("gemini-config = %v", cfg)
	}
	cfg, n, err = ResolveConfigBeads(context.Background(), daemon, "gemini-instructions", subs)
	if err != nil || n != 1 || cfg["gemini_md"] != "use the shell tool" {
		t.Errorf("gemini-instructions: cfg=%v n=%d err=%v", cfg, n, err)
	}
}

// End to end for gd-LOACIS8ZOA: gb setup gemini --local applies a gemini-config
// fragment to settings.json and appends gemini_md to GEMINI.md.
func TestRunSetupGemini_LocalAppliesGeminiBeads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("BOAT_PROJECT", "gardenio")
	codexConfigBeadServer(t, []map[string]any{
		cfgBead("gemini-config", []string{"role:crew"}, `{"ui":{"hideBanner":true}}`),
		cfgBead("gemini-instructions", []string{"role:crew"}, `{"gemini_md":"use the shell tool"}`),
	})
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("PRIME\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	ws := t.TempDir()
	if err := runSetupGemini(context.Background(), ws, "crew", true); err != nil {
		t.Fatalf("runSetupGemini: %v", err)
	}
	md, _ := os.ReadFile(filepath.Join(ws, ".gemini", geminiContextFile))
	if !strings.Contains(string(md), "use the shell tool") {
		t.Errorf("GEMINI.md lacks gemini_md: %q", md)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".gemini", geminiSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("invalid settings.json: %v", err)
	}
	if ui, _ := settings["ui"].(map[string]any); ui["hideBanner"] != true {
		t.Errorf("settings.json ui = %v", settings["ui"])
	}
}
