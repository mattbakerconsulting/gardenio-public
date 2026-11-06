package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// extractNatsServerConf pulls the nats-server.conf payload out of the
// rendered ConfigMap YAML. The config body is the indented block under the
// "nats-server.conf: |" key.
func extractNatsServerConf(t *testing.T, rendered string) string {
	t.Helper()
	idx := strings.Index(rendered, "nats-server.conf: |")
	if idx < 0 {
		t.Fatal("rendered statefulset.yaml has no nats-server.conf key")
	}
	rest := rendered[idx:]
	nl := strings.Index(rest, "\n")
	rest = rest[nl+1:]

	var b strings.Builder
	for _, line := range strings.Split(rest, "\n") {
		// The block scalar ends at the first line that is not blank and not
		// indented by the block's 4-space prefix (next YAML key or doc end).
		if line == "" {
			b.WriteString("\n")
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		b.WriteString(strings.TrimPrefix(line, "    "))
		b.WriteString("\n")
	}
	return b.String()
}

// TestNatsServerConfParsesWithRealParser feeds the chart's rendered
// subject-isolation nats-server.conf through the REAL nats-server config
// parser (the same code path the container runs at startup). This is the
// test that was missing when the original config shipped per-user `token`
// fields — invalid server config crashlooped the NATS pod on the first
// armed deploy and failed release 2026.199.16 at staging (ka-0000000002).
// Any chart change that renders config nats-server rejects fails here, at
// render time, instead of at deploy time.
func TestNatsServerConfParsesWithRealParser(t *testing.T) {
	out := helmTemplate(t, "templates/nats/statefulset.yaml",
		"nats.enabled=true",
		"nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true",
	)
	conf := extractNatsServerConf(t, out)

	// The container resolves $SERVER_NAME / $NATS_*_TOKEN via NATS config
	// env-var substitution. Provide the same variables here.
	t.Setenv("SERVER_NAME", "test-nats-0")
	t.Setenv("NATS_INFRA_TOKEN", "infra-secret")
	t.Setenv("NATS_AGENT_TOKEN", "agent-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "nats-server.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("nats-server rejects the rendered config (this is exactly the "+
			"class of failure that crashlooped the pod in release 2026.199.16):\n%v\n--- config ---\n%s", err, conf)
	}

	// The parse must yield the two-user authorization the chart promises.
	if len(opts.Users) != 2 {
		t.Fatalf("parsed %d users, want 2 (infra + agent)", len(opts.Users))
	}
	users := map[string]*natsserver.User{}
	for _, u := range opts.Users {
		users[u.Username] = u
	}
	infra, ok := users["infra"]
	if !ok {
		t.Fatal("no infra user parsed")
	}
	if infra.Password != "infra-secret" {
		t.Errorf("infra password = %q, want the env-substituted secret", infra.Password)
	}
	agent, ok := users["agent"]
	if !ok {
		t.Fatal("no agent user parsed")
	}
	if agent.Password != "agent-secret" {
		t.Errorf("agent password = %q, want the env-substituted secret", agent.Password)
	}
	if agent.Permissions == nil || agent.Permissions.Publish == nil {
		t.Fatal("agent user parsed without publish permissions")
	}
	pubAllow := strings.Join(agent.Permissions.Publish.Allow, ",")
	for _, subj := range []string{"coop.events.>", "coop.mux.session.>", "hooks.>", "_INBOX.>"} {
		if !strings.Contains(pubAllow, subj) {
			t.Errorf("agent publish allow-list missing %q (got %s)", subj, pubAllow)
		}
	}
}

// TestNatsServerConfLegacyTokenParses covers the legacy single-token branch
// with the same real-parser gate.
func TestNatsServerConfLegacyTokenParses(t *testing.T) {
	out := helmTemplate(t, "templates/nats/statefulset.yaml",
		"nats.enabled=true",
		"nats.auth.enabled=true",
		"nats.auth.token=legacy-secret",
	)
	conf := extractNatsServerConf(t, out)
	t.Setenv("SERVER_NAME", "test-nats-0")

	dir := t.TempDir()
	path := filepath.Join(dir, "nats-server.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("nats-server rejects the legacy-token config:\n%v\n--- config ---\n%s", err, conf)
	}
	if opts.Authorization != "legacy-secret" {
		t.Errorf("authorization token = %q, want the values-provided token", opts.Authorization)
	}
}

// TestNatsServerConfNoPerUserTokenField pins the syntax-level regression: a
// users[] entry must never carry a `token` field — nats-server only supports
// user/password/nkey there and hard-fails at startup on anything else.
func TestNatsServerConfNoPerUserTokenField(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/nats/statefulset.yaml",
		"nats.enabled=true",
		"nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true",
	)
	conf := extractNatsServerConf(t, out)
	// A `token:` key inside the authorization users block is the exact
	// "unknown field" crashloop from ka-0000000002.
	if regexp.MustCompile(`(?m)^\s+token:`).MatchString(conf) {
		t.Error("rendered nats-server.conf has a per-user `token:` field — " +
			"users[] entries support only user/password/nkey (ka-0000000002)")
	}
}
