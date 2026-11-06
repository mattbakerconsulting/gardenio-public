package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupAdviceAuditDaemon points the global daemon at a fake server serving a
// small advice corpus with a known per-archetype cost profile, and resets
// adviceAuditCmd flag state between subtests.
func setupAdviceAuditDaemon(t *testing.T) {
	t.Helper()

	type wireBead struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Type        string            `json:"type"`
		Status      string            `json:"status"`
		Labels      []string          `json:"labels"`
		Fields      map[string]string `json:"fields,omitempty"`
	}
	wire := []wireBead{
		{ID: "gd-glob", Title: "global rule", Description: strings.Repeat("g", 400), Type: "advice", Status: "open", Labels: []string{"global"}},
		// Opted-in project broadcast — since Phase 3 (ka-0000007790) a bare
		// bead without scope:project-broadcast is denied and would not count.
		{ID: "gd-bare", Title: "opted-in project broadcast", Description: strings.Repeat("p", 1000), Type: "advice", Status: "open", Labels: []string{"project:gardenio", "scope:project-broadcast"}},
		{ID: "gd-capt", Title: "gardener-only rule", Description: strings.Repeat("c", 2000), Type: "advice", Status: "open", Labels: []string{"g0:project:gardenio", "g0:role:gardener"}},
		{ID: "gd-hippo", Title: "hippo mirror", Description: strings.Repeat("h", 5000), Type: "advice", Status: "open", Labels: []string{"project:gardenio", "source:hippo-memory"}},
	}
	// The archetype grid now derives from the real fleet (agent beads), not the
	// advice corpus (ka-0000002153). This fleet ran exactly one archetype —
	// gardenio/gardener — so the audit measures that one, identically to before.
	agentWire := []wireBead{
		{ID: "gd-ag-capt", Title: "gardener agent", Type: "agent", Status: "closed", Fields: map[string]string{"project": "gardenio", "role": "gardener"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		beads := wire
		if strings.Contains(r.URL.Query().Get("type"), "agent") {
			beads = agentWire
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	adviceAuditCmd.SetContext(context.Background())
	t.Cleanup(func() {
		_ = adviceAuditCmd.Flags().Set("project", "")
		_ = adviceAuditCmd.Flags().Set("role", "")
		_ = adviceAuditCmd.Flags().Set("top", "0")
		_ = adviceAuditCmd.Flags().Set("budget-tokens", "18000")
		_ = adviceAuditCmd.Flags().Set("strict", "false")
		_ = adviceAuditCmd.Flags().Set("by-bead", "false")
		jsonOutput = false
	})
}

// TestAdviceAuditCmd_TableOutput exercises the default table path: the derived
// archetype (gardenio/gardener — the only project×role pair in the corpus) is
// audited through the real pipeline; the hippo mirror never counts.
func TestAdviceAuditCmd_TableOutput(t *testing.T) {
	setupAdviceAuditDaemon(t)

	out := string(captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	}))

	if !strings.Contains(out, "gardenio") || !strings.Contains(out, "gardener") {
		t.Errorf("audit table missing derived gardenio/gardener archetype:\n%s", out)
	}
	// glob(400) + bare(1000) + capt(2000) = 3400 bytes; hippo excluded.
	if !strings.Contains(out, "3400") {
		t.Errorf("audit table missing expected 3400-byte total (hippo mirror must not count):\n%s", out)
	}
	if !strings.Contains(out, "archetypes audited") {
		t.Errorf("audit table missing summary footer:\n%s", out)
	}
}

// TestAdviceAuditCmd_JSON asserts the machine-readable path carries matched
// count, byte/token totals, scope breakdown, and per-item detail.
func TestAdviceAuditCmd_JSON(t *testing.T) {
	setupAdviceAuditDaemon(t)
	jsonOutput = true

	out := captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	})

	var results []struct {
		Project string `json:"project"`
		Role    string `json:"role"`
		Matched int    `json:"matched"`
		Bytes   int    `json:"bytes"`
		Tokens  int    `json:"tokens"`
		ByScope map[string]struct {
			Beads int `json:"beads"`
			Bytes int `json:"bytes"`
		} `json:"by_scope"`
		Items []struct {
			ID    string `json:"id"`
			Bytes int    `json:"bytes"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out)
	}
	if len(results) != 1 {
		t.Fatalf("got %d archetypes, want 1 (gardenio/gardener): %s", len(results), out)
	}
	r := results[0]
	if r.Project != "gardenio" || r.Role != "gardener" {
		t.Errorf("archetype = %s/%s, want gardenio/gardener", r.Project, r.Role)
	}
	if r.Matched != 3 || r.Bytes != 3400 || r.Tokens != 850 {
		t.Errorf("matched=%d bytes=%d tokens=%d, want 3/3400/850", r.Matched, r.Bytes, r.Tokens)
	}
	if len(r.Items) != 3 || r.Items[0].ID != "gd-capt" {
		t.Errorf("items missing or unsorted: %+v", r.Items)
	}
	for _, it := range r.Items {
		if it.ID == "gd-hippo" {
			t.Errorf("hippo mirror leaked into audit items")
		}
	}
	if st := r.ByScope["project"]; st.Beads != 1 || st.Bytes != 1000 {
		t.Errorf("by_scope[project] = %+v, want 1/1000", st)
	}
}

// TestAdviceAuditCmd_ExplicitArchetype asserts --project/--role name an exact
// archetype even when no bead carries that role — the bare-project broadcast
// still reaches it, which is exactly the leak the audit measures.
func TestAdviceAuditCmd_ExplicitArchetype(t *testing.T) {
	setupAdviceAuditDaemon(t)
	jsonOutput = true
	if err := adviceAuditCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatal(err)
	}
	if err := adviceAuditCmd.Flags().Set("role", "thread"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	})

	var results []struct {
		Role    string `json:"role"`
		Matched int    `json:"matched"`
		Bytes   int    `json:"bytes"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out)
	}
	if len(results) != 1 || results[0].Role != "thread" {
		t.Fatalf("explicit archetype not audited: %s", out)
	}
	// A thread agent gets global (400) + the bare-project broadcast (1000)
	// but NOT the gardener-gated bead.
	if results[0].Matched != 2 || results[0].Bytes != 1400 {
		t.Errorf("gardenio/thread matched=%d bytes=%d, want 2/1400 (global + bare-project broadcast)",
			results[0].Matched, results[0].Bytes)
	}
}

// TestAdviceAuditCmd_TopListsHeaviest asserts --top renders the per-archetype
// heaviest-bead breakdown used to pick SHORTEN/RETARGET targets.
func TestAdviceAuditCmd_TopListsHeaviest(t *testing.T) {
	setupAdviceAuditDaemon(t)
	if err := adviceAuditCmd.Flags().Set("top", "2"); err != nil {
		t.Fatal(err)
	}

	out := string(captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	}))

	if !strings.Contains(out, "top 2 of 3 matched beads") {
		t.Errorf("--top missing per-archetype breakdown header:\n%s", out)
	}
	if !strings.Contains(out, "gd-capt") || !strings.Contains(out, "gd-bare") {
		t.Errorf("--top missing the two heaviest beads:\n%s", out)
	}
	if strings.Contains(out, "gd-glob\t") {
		t.Errorf("--top=2 should not list the third-heaviest bead:\n%s", out)
	}
}

// setupAdviceAuditMultiArchetype points the daemon at a two-archetype fleet
// (gardenio/gardener + gardenio/crew) so the fleet-amplified --by-bead ranking has
// a global bead that reaches BOTH archetypes and role beads that reach one each
// — the reach amplification the per-archetype view cannot express.
func setupAdviceAuditMultiArchetype(t *testing.T) {
	t.Helper()
	type wireBead struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Type        string            `json:"type"`
		Status      string            `json:"status"`
		Labels      []string          `json:"labels"`
		Fields      map[string]string `json:"fields,omitempty"`
	}
	wire := []wireBead{
		// global, 400 B — reaches BOTH archetypes -> fleet 800 B.
		{ID: "gd-glob", Title: "global rule", Description: strings.Repeat("g", 400), Type: "advice", Status: "open", Labels: []string{"global"}},
		// role:crew, 400 B — reaches ONLY crew -> fleet 400 B (same per-copy
		// size as gd-glob, must rank BELOW it purely on reach).
		{ID: "gd-crew", Title: "crew-only rule", Description: strings.Repeat("r", 400), Type: "advice", Status: "open", Labels: []string{"g0:project:gardenio", "g0:role:crew"}},
		// gardener-only, 600 B — reaches ONLY gardener -> fleet 600 B.
		{ID: "gd-capt", Title: "gardener-only rule", Description: strings.Repeat("c", 600), Type: "advice", Status: "open", Labels: []string{"g0:project:gardenio", "g0:role:gardener"}},
	}
	agentWire := []wireBead{
		{ID: "gd-ag-capt", Title: "gardener agent", Type: "agent", Status: "closed", Fields: map[string]string{"project": "gardenio", "role": "gardener"}},
		{ID: "gd-ag-crew", Title: "crew agent", Type: "agent", Status: "closed", Fields: map[string]string{"project": "gardenio", "role": "crew"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		beads := wire
		if strings.Contains(r.URL.Query().Get("type"), "agent") {
			beads = agentWire
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	adviceAuditCmd.SetContext(context.Background())
	t.Cleanup(func() {
		_ = adviceAuditCmd.Flags().Set("project", "")
		_ = adviceAuditCmd.Flags().Set("role", "")
		_ = adviceAuditCmd.Flags().Set("top", "0")
		_ = adviceAuditCmd.Flags().Set("budget-tokens", "18000")
		_ = adviceAuditCmd.Flags().Set("strict", "false")
		_ = adviceAuditCmd.Flags().Set("by-bead", "false")
		jsonOutput = false
	})
}

// TestAdviceAuditCmd_ByBeadJSON is the core fleet-amplification contract at the
// CLI layer: --by-bead --json collapses per-archetype items into one row per
// bead, weighted by reach. The global bead (reach 2) must carry 2× its per-copy
// bytes and outrank an equal-size role bead (reach 1) — the whole reason the
// view exists (ka-0000003599).
func TestAdviceAuditCmd_ByBeadJSON(t *testing.T) {
	setupAdviceAuditMultiArchetype(t)
	jsonOutput = true
	if err := adviceAuditCmd.Flags().Set("by-bead", "true"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	})

	var costs []struct {
		ID          string `json:"id"`
		Scope       string `json:"scope"`
		Bytes       int    `json:"bytes"`
		Reach       int    `json:"reach"`
		FleetBytes  int    `json:"fleet_bytes"`
		FleetTokens int    `json:"fleet_tokens"`
	}
	if err := json.Unmarshal(out, &costs); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out)
	}
	byID := map[string]int{}
	var glob, crew struct {
		reach, fleetBytes int
		scope             string
	}
	for i, c := range costs {
		byID[c.ID] = i
		if c.ID == "gd-glob" {
			glob.reach, glob.fleetBytes, glob.scope = c.Reach, c.FleetBytes, c.Scope
		}
		if c.ID == "gd-crew" {
			crew.reach, crew.fleetBytes = c.Reach, c.FleetBytes
		}
	}
	if len(costs) != 3 {
		t.Fatalf("got %d bead rows, want 3: %s", len(costs), out)
	}
	if glob.reach != 2 || glob.fleetBytes != 800 {
		t.Errorf("gd-glob reach=%d fleetBytes=%d, want 2/800", glob.reach, glob.fleetBytes)
	}
	if glob.scope != "global" {
		t.Errorf("gd-glob scope=%q, want global", glob.scope)
	}
	if crew.reach != 1 || crew.fleetBytes != 400 {
		t.Errorf("gd-crew reach=%d fleetBytes=%d, want 1/400", crew.reach, crew.fleetBytes)
	}
	// The equal-size global must outrank the role bead: it appears earlier.
	if byID["gd-glob"] >= byID["gd-crew"] {
		t.Errorf("gd-glob (fleet 800) must rank above equal-size gd-crew (fleet 400); order=%v", byID)
	}
}

// TestAdviceAuditCmd_ByBeadTable asserts the human table carries the REACH and
// fleet-token columns and the deduped per-bead footer.
func TestAdviceAuditCmd_ByBeadTable(t *testing.T) {
	setupAdviceAuditMultiArchetype(t)
	if err := adviceAuditCmd.Flags().Set("by-bead", "true"); err != nil {
		t.Fatal(err)
	}

	out := string(captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	}))

	if !strings.Contains(out, "REACH") || !strings.Contains(out, "FLEET-TOKENS") {
		t.Errorf("--by-bead table missing REACH/FLEET-TOKENS columns:\n%s", out)
	}
	// Global bead reaches 2 of 2 archetypes.
	if !strings.Contains(out, "2/2") {
		t.Errorf("--by-bead table missing global bead reach 2/2:\n%s", out)
	}
	if !strings.Contains(out, "matched beads across 2 archetypes") {
		t.Errorf("--by-bead footer missing fleet summary:\n%s", out)
	}
	// One row per bead — gd-glob appears exactly once even though it matches
	// both archetypes (the dedup the per-archetype --top view cannot do).
	if n := strings.Count(out, "gd-glob"); n != 1 {
		t.Errorf("gd-glob should appear exactly once in the per-bead table, got %d:\n%s", n, out)
	}
}

// TestAdviceAuditCmd_ByBeadTopClips asserts --top clips the per-bead ranking to
// N rows (heaviest fleet-cost first).
func TestAdviceAuditCmd_ByBeadTopClips(t *testing.T) {
	setupAdviceAuditMultiArchetype(t)
	if err := adviceAuditCmd.Flags().Set("by-bead", "true"); err != nil {
		t.Fatal(err)
	}
	if err := adviceAuditCmd.Flags().Set("top", "1"); err != nil {
		t.Fatal(err)
	}

	out := string(captureStdout(t, func() error {
		return adviceAuditCmd.RunE(adviceAuditCmd, nil)
	}))

	// Heaviest fleet-cost bead is gd-glob (800 B). --top 1 shows only it.
	if !strings.Contains(out, "gd-glob") {
		t.Errorf("--top 1 should show the heaviest bead gd-glob:\n%s", out)
	}
	if strings.Contains(out, "gd-capt") || strings.Contains(out, "gd-crew") {
		t.Errorf("--top 1 should clip lower-ranked beads:\n%s", out)
	}
	// The footer still reports the FULL matched count, not the clipped view.
	if !strings.Contains(out, "3 matched beads") {
		t.Errorf("--top clip should not shrink the footer's total count:\n%s", out)
	}
}

// TestAdviceAuditCmd_StrictGate asserts --strict turns the BUDGET column into a
// runtime gate: the gardenio/gardener archetype in the fixture costs 850 tokens,
// so a budget below it must make the command exit non-zero (return an error)
// while a budget above it exits 0. Without --strict the command always exits 0
// regardless of over-budget rows — the gap this flag closes (ka-0000006559). The
// diagnostic table still renders in every case; --strict only sets the exit code.
func TestAdviceAuditCmd_StrictGate(t *testing.T) {
	cases := []struct {
		name    string
		strict  bool
		budget  string
		wantErr bool
	}{
		// 850-token archetype vs a 500-token budget: over budget.
		{"strict/over-budget/errors", true, "500", true},
		// 850-token archetype vs a 2000-token budget: under budget.
		{"strict/under-budget/ok", true, "2000", false},
		// Same over-budget corpus but no --strict: advisory only, exits 0.
		{"no-strict/over-budget/exits-zero", false, "500", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupAdviceAuditDaemon(t)
			if err := adviceAuditCmd.Flags().Set("budget-tokens", tc.budget); err != nil {
				t.Fatal(err)
			}
			if tc.strict {
				if err := adviceAuditCmd.Flags().Set("strict", "true"); err != nil {
					t.Fatal(err)
				}
			}

			var runErr error
			out := string(captureStdout(t, func() error {
				runErr = adviceAuditCmd.RunE(adviceAuditCmd, nil)
				return nil
			}))

			if tc.wantErr && runErr == nil {
				t.Errorf("--strict with budget %s: want non-zero exit (error), got nil\n%s", tc.budget, out)
			}
			if !tc.wantErr && runErr != nil {
				t.Errorf("budget %s strict=%v: want exit 0, got error %v\n%s", tc.budget, tc.strict, runErr, out)
			}
			// The diagnostic table renders regardless of the exit code.
			if !strings.Contains(out, "archetypes audited") {
				t.Errorf("audit table missing summary footer even under --strict:\n%s", out)
			}
			if tc.wantErr && !strings.Contains(runErr.Error(), "over the") {
				t.Errorf("strict error should name the budget breach, got %q", runErr.Error())
			}
		})
	}
}
