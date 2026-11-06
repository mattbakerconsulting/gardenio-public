package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// grantsGolden is kbeads' mirror of its machine capability table (the chart
// tests cross-check the same file). The grafana-bridge authenticates as
// machine:gardenio-grafana-bridge with gardenio-machine-config-seed, whose
// create/update on config beads is typed to config_titles.
//
// This test reads that file by relative path, so CI must run it when only
// kbeads/** changes (.github/workflows/ci.yml, controller path gate).
const grantsGolden = "../../../../kbeads/internal/server/testdata/machine_grants.golden.json"

// seededTitles is the set of config titles EnsureGrafanaBridgeConfigs writes.
func seededTitles(t *testing.T) map[string]bool {
	t.Helper()
	all := configs()
	seeded := map[string]bool{}
	for _, key := range GrafanaBridgeConfigKeys() {
		if _, ok := all[key]; !ok {
			t.Fatalf("GrafanaBridgeConfigKeys lists %q, which configs() does not define", key)
		}
		title, _ := parseEntryKey(key)
		seeded[title] = true
	}
	return seeded
}

// TestMachineConfigSeedTitles_MatchBridgeSeed pins the seeded set and the
// kbeads config-seed grant to each other, exactly: a seeded title the grant
// lacks is a 403 (EnsureGrafanaBridgeConfigs would only warn), and a granted
// title that is not seeded is a privilege the bridge never needs.
func TestMachineConfigSeedTitles_MatchBridgeSeed(t *testing.T) {
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
	for _, e := range golden["gardenio-machine-config-seed"].ConfigTitles {
		if strings.HasSuffix(e, "*") {
			t.Errorf("config-seed grants the prefix %q; list exact titles only", e)
		}
		granted[e] = true
	}
	seeded := seededTitles(t)
	if len(seeded) == 0 || len(granted) == 0 {
		t.Fatalf("empty set: seeded=%v granted=%v", seeded, granted)
	}
	for title := range seeded {
		if !granted[title] {
			t.Errorf("EnsureGrafanaBridgeConfigs seeds %q but gardenio-machine-config-seed does not grant it; extend machineConfigSeedTitles in kbeads/internal/server/machine_grants.go and regenerate the golden", title)
		}
	}
	for title := range granted {
		if !seeded[title] {
			t.Errorf("gardenio-machine-config-seed grants %q but grafana-bridge does not seed it; remove it from machineConfigSeedTitles", title)
		}
	}
}

// grafana-bridge must never seed what reaches every agent pod, whatever the
// grant says: prerun runs via `sh -c`, context/claude-instructions are role
// system prompts, nudge-prompts is every agent's nudge text.
func TestGrafanaBridgeConfigKeys_NeverSeedAgentFacingConfig(t *testing.T) {
	forbidden := []string{
		"prerun", "context", "claude-instructions", "nudge-prompts",
		"claude-hooks", "claude-settings", "claude-mcp", "auth",
	}
	seeded := seededTitles(t)
	for _, f := range forbidden {
		if seeded[f] {
			t.Errorf("grafana-bridge seeds %q, which its machine grant must never cover", f)
		}
	}
	for title := range seeded {
		if !strings.HasPrefix(title, "type:") && !strings.HasPrefix(title, "view:") {
			t.Errorf("grafana-bridge seeds non type/view title %q", title)
		}
	}
}

// EnsureGrafanaBridgeConfigs writes only the owned entries: no other bridge's
// types, views, prompts or prerun reach the daemon through it.
func TestEnsureGrafanaBridgeConfigs_WritesOnlyOwnedEntries(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureGrafanaBridgeConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("EnsureGrafanaBridgeConfigs: %v", err)
	}
	if len(setter.beads) != 1 || setter.beads[0].Title != "type:dashboard" {
		t.Fatalf("beads = %+v; want exactly type:dashboard", setter.beads)
	}
}

// failingConfigSetter fails every write.
type failingConfigSetter struct{ mockConfigSetter }

func (f *failingConfigSetter) CreateBead(context.Context, beadsapi.CreateBeadRequest) (string, error) {
	return "", errors.New("403 forbidden")
}

// A failed write must be logged as a warning only, never as "ensured".
func TestEnsureConfigs_FailedWriteIsNotLoggedAsEnsured(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	err := EnsureGrafanaBridgeConfigs(context.Background(), &failingConfigSetter{}, logger)
	// gd-pj5hT3V1la: a seed whose writes all failed must say so to its
	// caller (it used to return nil), naming the key, so a bridge can retry
	// it once its machine credential is there.
	if err == nil {
		t.Fatal("EnsureConfigs must return the failed writes")
	}
	if !strings.Contains(err.Error(), "config type:dashboard:") {
		t.Fatalf("error must name the failed key: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "failed to ensure config bead") {
		t.Fatal("expected a Warn for the failed write")
	}
	if strings.Contains(out, "ensured config bead") {
		t.Fatalf("a failed write was logged as ensured:\n%s", out)
	}
}

// authUnavailableSetter fails every daemon call the way a machine-principal
// client does before its first token: the error wraps
// beadsapi.ErrMachineAuthUnavailable.
type authUnavailableSetter struct{}

func (authUnavailableSetter) CreateBead(context.Context, beadsapi.CreateBeadRequest) (string, error) {
	return "", fmt.Errorf("POST /beads: %w: machine token fetch failed", beadsapi.ErrMachineAuthUnavailable)
}

func (authUnavailableSetter) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, fmt.Errorf("GET /beads: %w: machine token fetch failed", beadsapi.ErrMachineAuthUnavailable)
}

func (authUnavailableSetter) UpdateBeadDescription(context.Context, string, string) error {
	return fmt.Errorf("PATCH /beads: %w: machine token fetch failed", beadsapi.ErrMachineAuthUnavailable)
}

// The joined seed error keeps the auth_unavailable classification, which is
// what the grafana-bridge startup retry keys on (gd-pj5hT3V1la).
func TestEnsureConfigs_AuthUnavailableSurvivesTheJoin(t *testing.T) {
	err := EnsureGrafanaBridgeConfigs(context.Background(), authUnavailableSetter{}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !beadsapi.IsAuthUnavailable(err) {
		t.Fatalf("joined seed error must still be auth_unavailable: %v", err)
	}
}

// controllerSeededTitles is the set of config titles EnsureConfigs writes.
func controllerSeededTitles() map[string]bool {
	seeded := map[string]bool{}
	for key := range configs() {
		title, _ := parseEntryKey(key)
		seeded[title] = true
	}
	return seeded
}

// TestMachineControllerConfigSeedTitles_MatchSeed pins what the controller's
// EnsureConfigs seeds and the kbeads gardenio-machine-controller-config-seed
// grant to each other, exactly (gd-BDirD4F7Iq): a seeded title the grant lacks
// is a 403 on the controller's machine principal (EnsureConfigs would only
// warn), and a granted title configs() does not define is a privilege the
// controller never needs.
func TestMachineControllerConfigSeedTitles_MatchSeed(t *testing.T) {
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
	for _, e := range golden["gardenio-machine-controller-config-seed"].ConfigTitles {
		if strings.HasSuffix(e, "*") {
			t.Errorf("controller-config-seed grants the prefix %q; list exact titles only", e)
		}
		granted[e] = true
	}
	seeded := controllerSeededTitles()
	if len(seeded) == 0 || len(granted) == 0 {
		t.Fatalf("empty set: seeded=%d granted=%d", len(seeded), len(granted))
	}
	for title := range seeded {
		if !granted[title] {
			t.Errorf("EnsureConfigs seeds %q but gardenio-machine-controller-config-seed does not grant it; extend machineControllerConfigSeedTitles in kbeads/internal/server/machine_grants.go and regenerate the golden", title)
		}
	}
	for title := range granted {
		if !seeded[title] {
			t.Errorf("gardenio-machine-controller-config-seed grants %q but configs() does not define it; remove it from machineControllerConfigSeedTitles", title)
		}
	}
	// Credential routing and the agent-facing configs the controller does not
	// seed stay out of reach, whatever configs() grows into.
	for _, forbidden := range []string{"auth", "claude-hooks", "claude-settings", "claude-mcp"} {
		if granted[forbidden] {
			t.Errorf("controller-config-seed must never grant %q", forbidden)
		}
	}
}
