package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"gopkg.in/yaml.v3"
)

// ── project.go ────────────────────────────────────────────────────────────

func TestDefaultGBProject(t *testing.T) {
	// primeProject is a package-level flag var; isolate it from any prior test.
	oldProject := primeProject
	primeProject = ""
	defer func() { primeProject = oldProject }()

	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	if got := defaultGBProject(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	t.Setenv("BOAT_PROJECT", "boat")
	if got := defaultGBProject(); got != "boat" {
		t.Errorf("BOAT_PROJECT fallback failed: %q", got)
	}
	t.Setenv("KD_PROJECT", "kd")
	if got := defaultGBProject(); got != "kd" {
		t.Errorf("KD_PROJECT precedence failed: %q", got)
	}

	// The --project flag (primeProject) takes precedence over both env vars.
	// This locks the gd-nqKwUv41GO regression where `gb prime --project <proj>`
	// errored with "unknown flag" and produced empty output — freshly-created
	// advice never rendered in the advice-supersession E2E.
	primeProject = "flagproj"
	if got := defaultGBProject(); got != "flagproj" {
		t.Errorf("--project flag precedence failed: got %q, want flagproj", got)
	}
}

// ── main.go ───────────────────────────────────────────────────────────────

func TestDefaultActor(t *testing.T) {
	t.Setenv("KD_ACTOR", "envactor")
	if got := defaultActor(); got != "envactor" {
		t.Errorf("expected envactor, got %q", got)
	}
	// When env empty, falls back to git config/unknown — just ensure non-empty.
	t.Setenv("KD_ACTOR", "")
	if got := defaultActor(); got == "" {
		t.Error("expected non-empty fallback")
	}
}

func TestDefaultProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	if got := defaultProject(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	t.Setenv("BOAT_PROJECT", "boat")
	if got := defaultProject(); got != "boat" {
		t.Errorf("BOAT_PROJECT fallback: %q", got)
	}
	t.Setenv("KD_PROJECT", "kd")
	if got := defaultProject(); got != "kd" {
		t.Errorf("KD_PROJECT precedence: %q", got)
	}
}

func TestDefaultHTTPURL(t *testing.T) {
	t.Setenv("BEADS_HTTP_URL", "")
	t.Setenv("BEADS_HTTP_ADDR", "")
	// No BEADS_HTTP_*: falls back to the configured host's /kd daemon, or ""
	// when no host is configured (requireDaemonURL then refuses to run).
	if got, want := defaultHTTPURL(), daemonURLForHost(resolveGardenioHost()); got != want {
		t.Errorf("default: %q, want %q", got, want)
	}
	t.Setenv("BEADS_HTTP_ADDR", "example.com:9999")
	if got := defaultHTTPURL(); got != "http://example.com:9999" {
		t.Errorf("addr upgrade: %q", got)
	}
	t.Setenv("BEADS_HTTP_ADDR", "http://example.com:9999")
	if got := defaultHTTPURL(); got != "http://example.com:9999" {
		t.Errorf("addr already scheme: %q", got)
	}
	t.Setenv("BEADS_HTTP_URL", "https://beads.example/")
	if got := defaultHTTPURL(); got != "https://beads.example/" {
		t.Errorf("url override: %q", got)
	}
}

// TestResolveGardenioHost verifies the env-aware default-host resolution:
// $GARDENIO_DEFAULT_HOST overrides the build-time default, schemes/slashes are
// stripped, and there is no compiled-in zero-config fallback.
func TestResolveGardenioHost(t *testing.T) {
	t.Setenv("GARDENIO_DEFAULT_HOST", "")
	if got := resolveGardenioHost(); got != "" {
		t.Errorf("zero-config: got %q, want empty (no compiled default)", got)
	}
	t.Setenv("GARDENIO_DEFAULT_HOST", "gardenio.poc.example")
	if got := resolveGardenioHost(); got != "gardenio.poc.example" {
		t.Errorf("env override: got %q", got)
	}
	// Tolerate a scheme prefix and trailing slash in the override.
	t.Setenv("GARDENIO_DEFAULT_HOST", "https://gardenio.poc.example/")
	if got := resolveGardenioHost(); got != "gardenio.poc.example" {
		t.Errorf("scheme/slash strip: got %q", got)
	}
}

// ── agent_identity.go ─────────────────────────────────────────────────────

func TestResolveAgentID(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	if _, err := resolveAgentID(""); err == nil {
		t.Error("expected error when no flag and no env")
	}
	t.Setenv("KD_AGENT_ID", "env-id")
	got, err := resolveAgentID("")
	if err != nil || got != "env-id" {
		t.Errorf("env fallback: %q err=%v", got, err)
	}
	got, err = resolveAgentID("flag-id")
	if err != nil || got != "flag-id" {
		t.Errorf("flag precedence: %q err=%v", got, err)
	}
}

func TestResolveAgentByActor(t *testing.T) {
	// No daemon state required for early exits.
	if got := resolveAgentByActor(context.Background(), ""); got != "" {
		t.Errorf("empty actor: %q", got)
	}
	if got := resolveAgentByActor(context.Background(), "unknown"); got != "" {
		t.Errorf("unknown actor: %q", got)
	}
}

func TestResolveAgentByActor_FoundAndEmpty(t *testing.T) {
	// Server returns one bead.
	srvFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{"id": "gd-AAA", "title": "agent"}},
			"total": 1,
		})
	}))
	defer srvFound.Close()
	swapDaemon(t, srvFound.URL)
	if got := resolveAgentByActor(context.Background(), "alice"); got != "gd-AAA" {
		t.Errorf("expected gd-AAA, got %q", got)
	}

	// Empty result.
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}, "total": 0})
	}))
	defer srvEmpty.Close()
	swapDaemon(t, srvEmpty.URL)
	if got := resolveAgentByActor(context.Background(), "alice"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}

	// Error response.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srvErr.Close()
	swapDaemon(t, srvErr.URL)
	if got := resolveAgentByActor(context.Background(), "alice"); got != "" {
		t.Errorf("expected empty on error, got %q", got)
	}
}

func TestResolveAgentIDWithFallback(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "env-id")
	got, err := resolveAgentIDWithFallback(context.Background(), "")
	if err != nil || got != "env-id" {
		t.Errorf("env first: %q err=%v", got, err)
	}

	// Flag wins over env.
	got, err = resolveAgentIDWithFallback(context.Background(), "flag-id")
	if err != nil || got != "flag-id" {
		t.Errorf("flag precedence: %q err=%v", got, err)
	}

	// No flag/env: falls back to actor lookup. With default actor unknown/empty,
	// and no daemon, returns error.
	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "unknown"
	t.Cleanup(func() { actor = oldActor })
	if _, err := resolveAgentIDWithFallback(context.Background(), ""); err == nil {
		t.Error("expected error")
	}
}

// ── agent_k8s_lifecycle.go ────────────────────────────────────────────────

func TestAtoiSafe(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", -1},
		{"0", 0},
		{"42", 42},
		{"1234", 1234},
		{"12a", -1},
		{"-5", -1},
	} {
		if got := atoiSafe(tc.in); got != tc.want {
			t.Errorf("atoiSafe(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ── agent_k8s_workspace.go ────────────────────────────────────────────────

func TestRepoHosts(t *testing.T) {
	hosts := repoHosts("gardenio=https://gitlab.com/acme/gardenio:gardenio,kbeads=https://gitlab.com/acme/kbeads")
	sort.Strings(hosts)
	want := []string{"gitlab.com"}
	if len(hosts) != 1 || hosts[0] != want[0] {
		t.Errorf("expected [gitlab.com], got %v", hosts)
	}
	// Mixed hosts.
	hosts = repoHosts("a=https://gitlab.com/x:foo,b=https://github.com/y:bar")
	sort.Strings(hosts)
	if len(hosts) != 2 || hosts[0] != "github.com" || hosts[1] != "gitlab.com" {
		t.Errorf("unexpected: %v", hosts)
	}
	// Malformed entries skipped.
	if got := repoHosts("bogus,=,=https://"); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
	if got := repoHosts(""); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestHomeDir(t *testing.T) {
	t.Setenv("HOME", "/tmp/someuser")
	if got := homeDir(); got != "/tmp/someuser" {
		t.Errorf("expected env home, got %q", got)
	}
	t.Setenv("HOME", "")
	if got := homeDir(); got != "/home/agent" {
		t.Errorf("expected default, got %q", got)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("XYZ_TEST_KEY", "")
	if got := envOr("XYZ_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("fallback: %q", got)
	}
	t.Setenv("XYZ_TEST_KEY", "val")
	if got := envOr("XYZ_TEST_KEY", "fallback"); got != "val" {
		t.Errorf("value: %q", got)
	}
}

func TestWriteDaemonConfig(t *testing.T) {
	dir := t.TempDir()
	if err := writeDaemonConfig(dir, "http://example:9999"); err != nil {
		t.Fatalf("writeDaemonConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "http://example:9999") {
		t.Errorf("unexpected content: %s", data)
	}
}

func TestWriteGitCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := writeGitCredentials("", "", "", nil); err != nil {
		t.Fatalf("empty hosts: %v", err)
	}
	// gitlab host uses gitlab token, others use user/token.
	err := writeGitCredentials("u", "t", "gl-token", []string{"gitlab.com", "github.com"})
	if err != nil {
		// git config step may fail in CI without git binary — tolerate but
		// still verify the file was written.
		t.Logf("git config call returned: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		t.Fatalf("read creds: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "oauth2:gl-token@gitlab.com") {
		t.Errorf("expected gitlab token line, got %s", s)
	}
	if !strings.Contains(s, "u:t@github.com") {
		t.Errorf("expected github user/token line, got %s", s)
	}
}

func TestSetupGhAuthFrom_NoCredsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	configured, err := setupGhAuthFrom(t.TempDir())
	if err != nil {
		t.Fatalf("setupGhAuthFrom: %v", err)
	}
	if configured {
		t.Error("expected no-op (configured=false) when no git-creds file is mounted")
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "gh", "hosts.yml")); !os.IsNotExist(err) {
		t.Errorf("hosts.yml should not exist, stat err = %v", err)
	}
}

func TestSetupGhAuthFrom_WritesHostsYml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	credsDir := t.TempDir()
	githubDir := filepath.Join(credsDir, "github")
	if err := os.MkdirAll(githubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubDir, "token"), []byte("ghp_supersecret"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubDir, "username"), []byte("gardenio-crew"), 0o400); err != nil {
		t.Fatal(err)
	}

	configured, err := setupGhAuthFrom(credsDir)
	if err != nil {
		t.Fatalf("setupGhAuthFrom: %v", err)
	}
	if !configured {
		t.Fatal("expected configured=true when a token file is present")
	}

	hostsPath := filepath.Join(home, ".config", "gh", "hosts.yml")
	data, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatalf("read hosts.yml: %v", err)
	}

	var hosts map[string]struct {
		User        string `yaml:"user"`
		OAuthToken  string `yaml:"oauth_token"`
		GitProtocol string `yaml:"git_protocol"`
	}
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		t.Fatalf("unmarshal hosts.yml: %v", err)
	}
	gh, ok := hosts["github.com"]
	if !ok {
		t.Fatalf("no github.com entry in hosts.yml: %s", data)
	}
	if gh.OAuthToken != "ghp_supersecret" {
		t.Errorf("oauth_token = %q, want ghp_supersecret", gh.OAuthToken)
	}
	if gh.User != "gardenio-crew" {
		t.Errorf("user = %q, want gardenio-crew", gh.User)
	}
	if gh.GitProtocol != "https" {
		t.Errorf("git_protocol = %q, want https", gh.GitProtocol)
	}
}

func TestSetupGhAuthFrom_DefaultUsernameWhenFileAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	credsDir := t.TempDir()
	githubDir := filepath.Join(credsDir, "github")
	if err := os.MkdirAll(githubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(githubDir, "token"), []byte("ghp_supersecret\n"), 0o400); err != nil {
		t.Fatal(err)
	}

	configured, err := setupGhAuthFrom(credsDir)
	if err != nil {
		t.Fatalf("setupGhAuthFrom: %v", err)
	}
	if !configured {
		t.Fatal("expected configured=true")
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("read hosts.yml: %v", err)
	}
	if !strings.Contains(string(data), "x-access-token") {
		t.Errorf("expected default username x-access-token, got %s", data)
	}
}

// ── agent_start.go ────────────────────────────────────────────────────────

func TestContainerName(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"foo", "gb-agent-foo"},
		{"foo bar", "gb-agent-foo-bar"},
		{"foo/bar:baz", "gb-agent-foo-bar-baz"},
	} {
		if got := containerName(tc.in); got != tc.want {
			t.Errorf("containerName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuote_Coverage(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"foo", "'foo'"},
		{"a'b", `'a'\''b'`},
		{"", "''"},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p <= 0 {
		t.Errorf("expected positive port, got %d", p)
	}
}

func TestResolveScenarioPath_PathValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.toml")
	os.WriteFile(path, []byte("x=1"), 0o644)

	// Absolute path
	if got, err := resolveScenarioPath(path, "", dir); err != nil || got != path {
		t.Errorf("abs path: got %q err=%v", got, err)
	}
	// Relative path with /
	if _, err := resolveScenarioPath("sub/missing.toml", "", dir); err == nil {
		t.Error("expected error for missing path")
	}
	// Empty value
	if _, err := resolveScenarioPath("", "", dir); err == nil {
		t.Error("expected error for empty value")
	}
}

func TestResolveScenarioPath_NameLookup(t *testing.T) {
	dir := t.TempDir()
	scenarioDir := filepath.Join(dir, "scenes")
	os.MkdirAll(scenarioDir, 0o755)
	target := filepath.Join(scenarioDir, "demo.toml")
	os.WriteFile(target, []byte("x=1"), 0o644)

	got, err := resolveScenarioPath("demo", "scenes", dir)
	if err != nil {
		t.Fatalf("got err: %v", err)
	}
	if got != target {
		t.Errorf("expected %q, got %q", target, got)
	}

	// Missing scenario.
	if _, err := resolveScenarioPath("nope", "scenes", dir); err == nil {
		t.Error("expected error for missing scenario")
	}
}

// ── agent_start_k8s.go ────────────────────────────────────────────────────

func TestIntEnvOr(t *testing.T) {
	t.Setenv("ZZZ_INT_TEST", "")
	if got := intEnvOr("ZZZ_INT_TEST", 42); got != 42 {
		t.Errorf("fallback: %d", got)
	}
	t.Setenv("ZZZ_INT_TEST", "7")
	if got := intEnvOr("ZZZ_INT_TEST", 42); got != 7 {
		t.Errorf("value: %d", got)
	}
	t.Setenv("ZZZ_INT_TEST", "not-int")
	if got := intEnvOr("ZZZ_INT_TEST", 42); got != 42 {
		t.Errorf("parse fail fallback: %d", got)
	}
}

func TestOrStr(t *testing.T) {
	for _, tc := range []struct {
		s, def, want string
	}{
		{"", "d", "d"},
		{"   ", "d", "d"},
		{"x", "d", "x"},
	} {
		if got := orStr(tc.s, tc.def); got != tc.want {
			t.Errorf("orStr(%q,%q) = %q, want %q", tc.s, tc.def, got, tc.want)
		}
	}
}

func TestRateLimitSleepDuration_NoSentinel(t *testing.T) {
	os.Remove("/tmp/rate_limit_reset")
	if d := rateLimitSleepDuration(); d != 0 {
		t.Errorf("expected 0 for missing sentinel, got %v", d)
	}
}

func TestRateLimitSleepDuration_BadMessage(t *testing.T) {
	const p = "/tmp/rate_limit_reset"
	os.WriteFile(p, []byte("no reset here"), 0o644)
	defer os.Remove(p)
	// Falls back to 30 min.
	if d := rateLimitSleepDuration(); d != 30*time.Minute {
		t.Errorf("expected 30m fallback, got %v", d)
	}
}

// ── audit.go ──────────────────────────────────────────────────────────────

func TestAuditPtrStr(t *testing.T) {
	if ptrStr(nil) != "" {
		t.Error("nil")
	}
	s := "hi"
	if ptrStr(&s) != "hi" {
		t.Error("non-nil")
	}
}

func TestAuditPtrUint32(t *testing.T) {
	if ptrUint32(nil) != "?" {
		t.Error("nil")
	}
	var u uint32 = 42
	if ptrUint32(&u) != "42" {
		t.Error("non-nil")
	}
}

func TestSessionMetaDuration(t *testing.T) {
	m := &sessionMeta{StartedAt: 100}
	if m.duration() != 0 {
		t.Error("nil ended")
	}
	end := uint64(160)
	m.EndedAt = &end
	if got := m.duration(); got != 60*time.Second {
		t.Errorf("duration: %v", got)
	}
}

func TestS3BucketPrefix(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "b")
	if s3Bucket() != "b" {
		t.Error("bucket")
	}
	t.Setenv("COOP_S3_PREFIX", "")
	if s3Prefix() != "coop/sessions" {
		t.Error("default prefix")
	}
	t.Setenv("COOP_S3_PREFIX", "x/y")
	if s3Prefix() != "x/y" {
		t.Error("override prefix")
	}
}

func TestAuditParseDuration(t *testing.T) {
	d, err := parseDuration("2d")
	if err != nil || d != 48*time.Hour {
		t.Errorf("2d: %v err=%v", d, err)
	}
	d, err = parseDuration("90m")
	if err != nil || d != 90*time.Minute {
		t.Errorf("90m: %v err=%v", d, err)
	}
	if _, err := parseDuration("garbage"); err == nil {
		t.Error("expected parse error")
	}
	if _, err := parseDuration("xd"); err == nil {
		t.Error("expected parse error for xd")
	}
}

func TestReadBatchFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "batch.txt")
	content := "# comment\ngd-AAA\n\ngd-BBB\n# another\ngd-CCC\n"
	os.WriteFile(p, []byte(content), 0o644)

	ids, err := readBatchFile(p)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"gd-AAA", "gd-BBB", "gd-CCC"}
	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}

	// Missing file.
	if _, err := readBatchFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error for missing file")
	}
}

// ── peek.go ───────────────────────────────────────────────────────────────

func TestResolveMuxEnv(t *testing.T) {
	t.Setenv("COOP_MUX_URL", "")
	if _, _, err := resolveMuxEnv(); err == nil {
		t.Error("expected error")
	}
	t.Setenv("COOP_MUX_URL", "http://mux/")
	t.Setenv("COOP_MUX_TOKEN", "tok")
	u, tok, err := resolveMuxEnv()
	if err != nil || u != "http://mux" || tok != "tok" {
		t.Errorf("got %q %q %v", u, tok, err)
	}
}

func TestMuxHTTPClient(t *testing.T) {
	c := muxHTTPClient()
	if c == nil {
		t.Fatal("nil client")
	}
	if c.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
}

func TestPeekFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{5, "5s"},
		{65, "1m5s"},
		{3665, "1h1m5s"},
	} {
		if got := peekFormatDuration(tc.in); got != tc.want {
			t.Errorf("peekFormatDuration(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{500, "500 B"},
		{1500, "1.5 KB"},
		{2 * 1024 * 1024, "2.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	} {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	// Under `go test`, stdout is usually a pipe, not a TTY.
	_ = isTerminal()
}

// ── decision.go ───────────────────────────────────────────────────────────

func TestSenderFromLabels(t *testing.T) {
	if got := senderFromLabels([]string{"x", "y"}); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := senderFromLabels([]string{"foo", "from:alice", "bar"}); got != "alice" {
		t.Errorf("expected alice, got %q", got)
	}
	if got := senderFromLabels(nil); got != "" {
		t.Error("nil slice")
	}
}

func TestExtractArtifactType(t *testing.T) {
	opts := `[{"id":"a","artifact_type":"plan"},{"id":"b","artifact_type":"report"},{"id":"c"}]`
	if got := extractArtifactType(opts, "a"); got != "plan" {
		t.Errorf("a: %q", got)
	}
	if got := extractArtifactType(opts, "b"); got != "report" {
		t.Errorf("b: %q", got)
	}
	if got := extractArtifactType(opts, "c"); got != "" {
		t.Errorf("c should be empty: %q", got)
	}
	if got := extractArtifactType(opts, "missing"); got != "" {
		t.Errorf("missing: %q", got)
	}
	if got := extractArtifactType("not json", "a"); got != "" {
		t.Errorf("bad json: %q", got)
	}
}

// ── news.go ───────────────────────────────────────────────────────────────

func TestResolveOptionLabel(t *testing.T) {
	opts := `[{"id":"a","short":"Short A","label":"Label A"},{"id":"b","label":"Label B"},{"id":"c"}]`
	if got := resolveOptionLabel(opts, "a"); got != "Short A" {
		t.Errorf("a short: %q", got)
	}
	if got := resolveOptionLabel(opts, "b"); got != "Label B" {
		t.Errorf("b label: %q", got)
	}
	if got := resolveOptionLabel(opts, "c"); got != "" {
		t.Errorf("c empty: %q", got)
	}
	if got := resolveOptionLabel("", "a"); got != "" {
		t.Errorf("empty json: %q", got)
	}
	if got := resolveOptionLabel(opts, ""); got != "" {
		t.Errorf("empty id: %q", got)
	}
	if got := resolveOptionLabel("not json", "a"); got != "" {
		t.Errorf("bad json: %q", got)
	}
}

func TestPrintNewsBead(t *testing.T) {
	// Exercises formatting; just ensure no panic.
	printNewsBead(&beadsapi.BeadDetail{ID: "gd-X", Title: "hi", Assignee: "bob", Type: "task"})
	printNewsBead(&beadsapi.BeadDetail{ID: "gd-Y", Title: strings.Repeat("a", 100)})
	printNewsBeadWithSpawner(
		&beadsapi.BeadDetail{ID: "gd-Z", Assignee: "carol"},
		map[string]string{"carol": "root"},
	)
}

func TestPrintNewsDecision(t *testing.T) {
	printNewsDecision(&beadsapi.BeadDetail{
		ID:     "gd-D1",
		Title:  "pick one",
		Fields: map[string]string{"prompt": "choose", "chosen": "a", "options": `[{"id":"a","short":"A"}]`, "responded_by": "alice"},
	})
	printNewsDecision(&beadsapi.BeadDetail{
		ID:     "gd-D2",
		Title:  "no chosen",
		Fields: map[string]string{},
	})
	printNewsDecision(&beadsapi.BeadDetail{
		ID:     "gd-D3",
		Title:  strings.Repeat("x", 100),
		Fields: map[string]string{"prompt": strings.Repeat("p", 100), "chosen": strings.Repeat("c", 100), "options": "[]"},
	})
}

func TestPrintNewsReport(t *testing.T) {
	printNewsReport(&beadsapi.BeadDetail{
		ID:        "gd-R1",
		Title:     "report",
		Fields:    map[string]string{"report_type": "plan", "decision_id": "gd-D1"},
		CreatedBy: "bob",
	})
	printNewsReport(&beadsapi.BeadDetail{
		ID:     "gd-R2",
		Title:  strings.Repeat("x", 100),
		Fields: map[string]string{},
	})
}

// ── advice.go: relativeTime + humanDuration + printAdviceList ────────────

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{2 * 24 * time.Hour, "2 days"},
		{5 * time.Hour, "5 hours"},
		{30 * time.Minute, "30 minutes"},
		{5 * time.Second, "5 seconds"},
	} {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(3 * time.Hour)
	past := now.Add(-2 * time.Hour)

	if got := relativeTime(future, now); got != "in 3 hours" {
		t.Errorf("future: %q", got)
	}
	if got := relativeTime(past, now); got != "2 hours ago" {
		t.Errorf("past: %q", got)
	}
}

func TestPrintAdviceList(t *testing.T) {
	// Empty list.
	printAdviceList(nil, 0)
	// Non-empty, exercise truncation path.
	printAdviceList([]*beadsapi.BeadDetail{
		{ID: "gd-A", Title: strings.Repeat("t", 60), Labels: []string{"x", "y"}, Fields: map[string]string{"confidence": "0.9"}},
		{ID: "gd-B", Title: "short", Fields: map[string]string{}},
	}, 2)
}

// ── mail.go: printMail* ───────────────────────────────────────────────────

func TestPrintMail(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:          "gd-M",
		Title:       "Subject",
		Assignee:    "bob",
		Description: "hello",
		Labels:      []string{"from:alice"},
	}
	printMailLine(b)
	printMailDetail(b)

	// No description.
	printMailDetail(&beadsapi.BeadDetail{ID: "gd-N", Title: "no body"})
}

func TestResolveMailActor(t *testing.T) {
	t.Setenv("KD_ACTOR", "gd-alice")
	if got := resolveMailActor(); got != "gd-alice" {
		t.Errorf("env: %q", got)
	}
	t.Setenv("KD_ACTOR", "")
	oldActor := actor
	actor = "fallback-actor"
	t.Cleanup(func() { actor = oldActor })
	if got := resolveMailActor(); got != "fallback-actor" {
		t.Errorf("fallback: %q", got)
	}
}

// ── pool.go ───────────────────────────────────────────────────────────────

func TestResolvePoolProject(t *testing.T) {
	oldPP := poolProject
	t.Cleanup(func() { poolProject = oldPP })

	poolProject = "explicit"
	if got := resolvePoolProject(); got != "explicit" {
		t.Errorf("explicit: %q", got)
	}

	poolProject = ""
	t.Setenv("KD_PROJECT", "kdproj")
	t.Setenv("BOAT_PROJECT", "")
	if got := resolvePoolProject(); got != "kdproj" {
		t.Errorf("fallback: %q", got)
	}
}

// ── setup helpers ─────────────────────────────────────────────────────────

// neutralizeSlackEnv clears the Slack thread-binding env vars for the duration
// of a test so a code path under test does not accidentally treat the process
// as a thread-bound agent and fire a real Slack breadcrumb / interrupted-marker
// PATCH.
//
// closeAgentBeadBestEffort("context_cancelled") posts an "interrupted mid-turn"
// breadcrumb whenever resolveSlackEnv() resolves (SLACK_BRIDGE_URL +
// SLACK_THREAD_CHANNEL + SLACK_THREAD_TS all set). Doing so issues a PATCH on
// the agent bead (markInterruptedBreadcrumbPosted) plus a live HTTP call to the
// bridge. Agent/CI pods often carry SLACK_BRIDGE_URL — and a thread-spawned
// agent carries the thread coords too — so a salvage test that leaves those vars
// ambient reaches out to the network and lands a stray agent-bead PATCH that the
// salvage mock misreads as a task reopen. That is the env-dependent flake in
// gd-95VY2YZ9Sy. Clearing all three here (t.Setenv auto-restores) makes the
// salvage tests hermetic regardless of the ambient environment.
func neutralizeSlackEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SLACK_BRIDGE_URL", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
}

// swapDaemon points the package-global `daemon` at a test server and
// restores it on cleanup.
func swapDaemon(t *testing.T, baseURL string) {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: baseURL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}
