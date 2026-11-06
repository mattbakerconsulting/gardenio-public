package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// slackSeededTitles is the set of config titles EnsureSlackBridgeConfigs writes.
func slackSeededTitles(t *testing.T) map[string]bool {
	t.Helper()
	all := configs()
	seeded := map[string]bool{}
	for _, key := range SlackBridgeConfigKeys() {
		if _, ok := all[key]; !ok {
			t.Fatalf("SlackBridgeConfigKeys lists %q, which configs() does not define", key)
		}
		title, _ := parseEntryKey(key)
		seeded[title] = true
	}
	return seeded
}

// TestSlackBridgeConfigSeedTitles_MatchBridgeSeed pins the seeded set and the
// kbeads gardenio-machine-slack-config-seed grant to each other, exactly: a
// seeded title the grant lacks is a 403, and a granted title that is not seeded
// is a privilege the bridge never needs. The golden is kbeads' mirror of its
// capability table (see grantsGolden).
func TestSlackBridgeConfigSeedTitles_MatchBridgeSeed(t *testing.T) {
	b, err := os.ReadFile(grantsGolden)
	if err != nil {
		t.Fatalf("read kbeads grants golden: %v", err)
	}
	var golden map[string]struct {
		ConfigTitles []string `json:"config_titles"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatalf("decode kbeads grants golden: %v", err)
	}
	granted := map[string]bool{}
	for _, e := range golden["gardenio-machine-slack-config-seed"].ConfigTitles {
		if strings.HasSuffix(e, "*") {
			t.Errorf("slack-config-seed grants the prefix %q; list exact titles only", e)
		}
		granted[e] = true
	}
	seeded := slackSeededTitles(t)
	if len(seeded) == 0 || len(granted) == 0 {
		t.Fatalf("empty set: seeded=%v granted=%v", seeded, granted)
	}
	for title := range seeded {
		if !granted[title] {
			t.Errorf("EnsureSlackBridgeConfigs seeds %q but gardenio-machine-slack-config-seed does not grant it; extend machineSlackConfigSeedTitles in kbeads/internal/server/machine_grants.go and regenerate the golden", title)
		}
	}
	for title := range granted {
		if !seeded[title] {
			t.Errorf("gardenio-machine-slack-config-seed grants %q but slack-bridge does not seed it; remove it from machineSlackConfigSeedTitles", title)
		}
	}
}

// slack-bridge must never seed what reaches every agent pod or sets another
// type's write policy, whatever the grant says.
func TestSlackBridgeConfigKeys_NeverSeedAgentFacingOrPolicyConfig(t *testing.T) {
	forbidden := []string{
		"prerun", "context", "claude-instructions", "nudge-prompts",
		"claude-hooks", "claude-settings", "claude-mcp", "auth",
		"type:role", "type:agent", "type:project", "type:user", "type:account",
		"type:trigger", "type:schedule", "type:task", "type:view",
	}
	seeded := slackSeededTitles(t)
	for _, f := range forbidden {
		if seeded[f] {
			t.Errorf("slack-bridge seeds %q, which its machine grant must never cover", f)
		}
	}
	for title := range seeded {
		if !strings.HasPrefix(title, "type:") && !strings.HasPrefix(title, "view:") {
			t.Errorf("slack-bridge seeds non type/view title %q", title)
		}
	}
}

// EnsureSlackBridgeConfigs writes only the owned entries.
func TestEnsureSlackBridgeConfigs_WritesOnlyOwnedEntries(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureSlackBridgeConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("EnsureSlackBridgeConfigs: %v", err)
	}
	got := map[string]bool{}
	for _, b := range setter.beads {
		got[b.Title] = true
	}
	want := slackSeededTitles(t)
	if len(got) != len(want) {
		t.Fatalf("beads = %v; want exactly %v", got, want)
	}
	for title := range want {
		if !got[title] {
			t.Errorf("missing %q in %v", title, got)
		}
	}
}
