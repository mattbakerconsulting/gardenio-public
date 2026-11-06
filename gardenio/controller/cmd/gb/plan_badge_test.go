package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
)

// newBadgeTestCmd returns a bare cobra.Command with a context, so
// generatePlanBadge and the upload have one to derive from, without wiring the
// whole root command tree.
func newBadgeTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}

// resetBadgeFlags restores the package-level flag vars between subtests so one
// case cannot leak state into another.
func resetBadgeFlags() {
	planBadgeTier0 = false
	planBadgeStyle = badge.StyleLineArtBold
	planBadgeModel = ""
	planBadgeBaseURL = ""
	planBadgePNG = false
	planBadgeSize = badge.DefaultRasterSize
	planBadgeOut = ""
	planBadgeUpload = false
	planBadgeNoCache = false
	planBadgeRefresh = false
}

func TestGeneratePlanBadge_Tier0Forced(t *testing.T) {
	resetBadgeFlags()
	planBadgeTier0 = true
	// Even with a key present, --tier0 must skip the model entirely and
	// return the deterministic badge with no advisory error.
	t.Setenv("BADGE_API_KEY", "should-be-ignored")
	b, err := generatePlanBadge(newBadgeTestCmd(), "gd-abc", "A Plan", "prose", "")
	if err != nil {
		t.Fatalf("--tier0 should never error: %v", err)
	}
	if b.Tier != badge.TierDeterministic {
		t.Fatalf("tier = %q, want tier0", b.Tier)
	}
	if b.SVG != badge.Tier0("gd-abc", "A Plan").SVG {
		t.Error("--tier0 did not return the deterministic badge")
	}
}

func TestGeneratePlanBadge_NoKeyFallsBack(t *testing.T) {
	resetBadgeFlags()
	// No BADGE_API_KEY / ANTHROPIC_API_KEY -> disabled generator -> Tier0.
	t.Setenv("BADGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	b, err := generatePlanBadge(newBadgeTestCmd(), "gd-def", "Another Plan", "prose", "")
	if err == nil {
		t.Error("expected advisory error when no model key is set")
	}
	if b.Tier != badge.TierDeterministic {
		t.Fatalf("tier = %q, want tier0 fallback", b.Tier)
	}
	if !strings.Contains(b.SVG, `viewBox="0 0 100 100"`) {
		t.Errorf("fallback badge missing shared viewBox: %s", b.SVG)
	}
}

func TestBadgeCmd_Registered(t *testing.T) {
	// gb badge is a top-level verb; gb plan badge stays as a hidden alias.
	if c, _, err := rootCmd.Find([]string{"badge"}); err != nil || c != badgeCmd || !c.Runnable() {
		t.Errorf("`gb badge` not registered as a runnable root command (got %v, %v)", c, err)
	}
	var found bool
	for _, c := range planCmd.Commands() {
		if c.Name() == "badge" {
			found = true
			if !c.Runnable() || !c.Hidden {
				t.Errorf("plan badge alias runnable=%v hidden=%v, want both", c.Runnable(), c.Hidden)
			}
			if c.Flags().Lookup("tier0") == nil || c.Flags().Lookup("upload") == nil {
				t.Error("plan badge alias is missing the badge flags")
			}
		}
	}
	if !found {
		t.Error("`gb plan badge` alias not registered under planCmd")
	}
}

// setupBadgeLineDaemon installs a test daemon that serves beads by id and each
// bead's dependencies from parents (child id -> parent id, as a parent-child
// edge plus a blocks edge that the walk must ignore).
func setupBadgeLineDaemon(t *testing.T, parents map[string]string, beads ...map[string]any) {
	t.Helper()
	byID := map[string]map[string]any{}
	for _, b := range beads {
		byID[b["id"].(string)] = b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		if id, ok := strings.CutSuffix(path, "/dependencies"); ok {
			deps := []map[string]any{{"bead_id": id, "depends_on_id": "gd-blocker", "type": "blocks"}}
			if p := parents[id]; p != "" {
				deps = append(deps, map[string]any{"bead_id": id, "depends_on_id": p, "type": "parent-child"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})
			return
		}
		if b, ok := byID[path]; ok {
			_ = json.NewEncoder(w).Encode(b)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	installBadgeTestDaemon(t, srv.URL)
}

// installBadgeTestDaemon points the gb daemon client at addr for one test.
func installBadgeTestDaemon(t *testing.T, addr string) {
	t.Helper()
	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

func TestResolveBadgeArms_TaskInheritsPlanArms(t *testing.T) {
	setupBadgeLineDaemon(t,
		map[string]string{"gd-task": "gd-phase", "gd-phase": "gd-plan"},
		map[string]any{"id": "gd-phase", "type": "epic"},
		map[string]any{"id": "gd-plan", "type": "epic", "heraldry": "brown 🏅 . 🖌️ . 🔎"},
	)
	task := &beadsapi.BeadDetail{ID: "gd-task", Type: "task"}
	arms := resolveBadgeArms(context.Background(), task)
	if arms.From != "gd-plan" || beadsapi.HeraldryRun(arms.Heraldry) != "🏅🖌️🔎" {
		t.Fatalf("got %+v, want the plan's arms inherited from gd-plan", arms)
	}
	if !arms.Inherited("gd-task") {
		t.Error("plan arms on a task must be reported inherited")
	}
}

func TestResolveBadgeArms_NoArmedAncestor(t *testing.T) {
	setupBadgeLineDaemon(t,
		map[string]string{"gd-task": "gd-plan"},
		map[string]any{"id": "gd-plan", "type": "epic"},
	)
	arms := resolveBadgeArms(context.Background(), &beadsapi.BeadDetail{ID: "gd-task", Type: "task"})
	if !arms.None() || arms.From != "" {
		t.Fatalf("got %+v, want no arms", arms)
	}
}

func TestResolveBadgeArms_OwnArmsSkipDaemon(t *testing.T) {
	// No daemon is installed for this bead's line: own arms must not read it.
	setupBadgeLineDaemon(t, nil)
	b := &beadsapi.BeadDetail{ID: "gd-bug", Type: "bug", Heraldry: "blue 🐛 . 🔧"}
	arms := resolveBadgeArms(context.Background(), b)
	if arms.From != "gd-bug" || arms.Inherited("gd-bug") {
		t.Fatalf("got %+v, want the bug's own arms", arms)
	}
}

func TestBadgeDefaultModel(t *testing.T) {
	t.Setenv("BADGE_MODEL", "")
	if got := badgeDefaultModel(); got != badge.DefaultModel {
		t.Errorf("unset BADGE_MODEL: got %q, want %q", got, badge.DefaultModel)
	}
	t.Setenv("BADGE_MODEL", "claude-sonnet-5-5")
	if got := badgeDefaultModel(); got != "claude-sonnet-5-5" {
		t.Errorf("BADGE_MODEL set: got %q, want claude-sonnet-5-5", got)
	}
}

// TestGeneratePlanBadge_ArmsReachModel: the arms gb badge resolved are what
// the model is asked to draw, by run and charge name, under --model.
func TestGeneratePlanBadge_ArmsReachModel(t *testing.T) {
	resetBadgeFlags()
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 100 100\"><circle cx=\"50\" cy=\"50\" r=\"40\"/></svg>"}]}`))
	}))
	defer srv.Close()
	t.Setenv("BADGE_API_KEY", "key")
	planBadgeBaseURL = srv.URL
	planBadgeModel = "badge-model-x"

	b, err := generatePlanBadge(newBadgeTestCmd(), "gd-arm", "A Plan", "prose", "🔥🧭📦")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Tier != badge.TierModel {
		t.Fatalf("tier = %q, want tier2", b.Tier)
	}
	if got.Model != "badge-model-x" {
		t.Errorf("model = %q, want badge-model-x", got.Model)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %+v, want one", got.Messages)
	}
	for _, want := range []string{"Arms: 🔥🧭📦", "1. fire", "2. compass", "3. package"} {
		if !strings.Contains(got.Messages[0].Content, want) {
			t.Errorf("prompt missing %q:\n%s", want, got.Messages[0].Content)
		}
	}
}
