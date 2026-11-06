package main

// prime_size_budget_test.go pins the gb prime byte budget — originally the
// regression test for epic ka-0000008080 ("Shrink gb prime output so advice
// reaches Claude's context"), extended in gd-VtgunBw4KK to cover
// project=monorepo (which had grown to ~31KB / 82 advice items, busting the
// preview cap on every monorepo session).
//
// The total budget mirrors the SessionStart hook's preview cap before the
// harness diverts the rest of prime to a file. The advice sub-cap is
// independently enforced because advice is the dominant section and the
// section most likely to bloat unbounded as new beads are filed.
//
// Enforcement is unconditional: the test fails if any role/project pair
// exceeds either budget. Override the underlying advice budgets via
// GB_PRIME_ADVICE_BUDGET_BYTES / GB_PRIME_ADVICE_TITLE_BUDGET_BYTES if you
// need to experiment locally.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const (
	// Operator policy (ka-0000006677): advice gets 20k tokens
	// (~80 KB at 4 bytes/token), everywhere. The total prime cap is sized
	// to comfortably accommodate advice plus the rest of the prime body
	// (workflow context, roster, auto-assign, identity, header/footer).
	primeBudgetTotal  = 102400 // 100 KB — total prime body cap
	primeBudgetAdvice = 81920  // 80 KB — advice section sub-cap (~20k tokens)
)

// budgetBead is a minimal bead JSON for the size-budget test daemon.
type budgetBead struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Kind        string            `json:"kind"`
	Status      string            `json:"status"`
	Labels      []string          `json:"labels"`
	Fields      map[string]string `json:"fields,omitempty"`
}

// budgetCorpus returns a representative advice corpus covering both
// project=gardenio (~18 project items) and project=monorepo (~53 project
// items, mirroring the real catalog audited at task time for gd-VtgunBw4KK).
// Plus 12 global + per-role beads. The advice matcher filters per-agent so
// a gardenio agent only sees gardenio+global+role items and a monorepo agent
// only sees monorepo+global+role items.
func budgetCorpus() []budgetBead {
	beads := make([]budgetBead, 0, 130)
	addItem := func(id, title, body string, labels []string) {
		beads = append(beads, budgetBead{
			ID: id, Title: title, Description: body,
			Type: "advice", Kind: "data", Status: "open",
			Labels: labels,
			Fields: map[string]string{"confidence": "0.7"},
		})
	}

	for i := range 12 {
		addItem(
			fmt.Sprintf("gd-glob%03d", i),
			fmt.Sprintf("Global rule #%d: cross-cutting workflow guidance", i),
			strings.Repeat(fmt.Sprintf("Global body line %d explaining why this rule applies to every agent. ", i), 5),
			[]string{"global"},
		)
	}
	for i := range 18 {
		addItem(
			fmt.Sprintf("gd-proj%03d", i),
			fmt.Sprintf("Gardenio project guideline #%d", i),
			strings.Repeat(fmt.Sprintf("Project-specific guideline %d for the gardenio codebase. ", i), 5),
			[]string{"project:gardenio"},
		)
	}
	// project=monorepo carries a much larger advice catalog (~53 items as of
	// gd-VtgunBw4KK). Sized to match so the test catches monorepo-specific
	// budget regressions, not just gardenio ones.
	for i := range 53 {
		addItem(
			fmt.Sprintf("gd-mono%03d", i),
			fmt.Sprintf("Monorepo project guideline #%d", i),
			strings.Repeat(fmt.Sprintf("Monorepo-specific guideline %d covering CI, MR rules, JIRA, perf, observability. ", i), 5),
			[]string{"project:monorepo"},
		)
	}
	// Per-role bead counts mirror prod's overflow shape: crew gets ~10
	// extra, gardener ~16 extra, crew-e2e ~22 extra.
	for role, n := range map[string]int{"crew": 10, "gardener": 16, "crew-e2e": 22} {
		for i := range n {
			addItem(
				fmt.Sprintf("gd-%s%03d", role, i),
				fmt.Sprintf("Role:%s rule #%d — pattern to follow when running this role", role, i),
				strings.Repeat(fmt.Sprintf("Detailed %s entry %d covering context, why, and the common edge case. ", role, i), 6),
				[]string{"role:" + role},
			)
		}
	}
	return beads
}

// budgetConfigBeads returns a config bead carrying claude-instructions
// content sized to mirror production workflow context (~1.5KB rendered).
func budgetConfigBeads() []budgetBead {
	cfg := map[string]string{
		"prime_header":       "# Beads Workflow Context\n\nUse `gb prime` to recover context after compaction.\n",
		"session_close":      "# SESSION CLOSE PROTOCOL\n\nBefore saying done: git status, add, commit, push. Work is not done until pushed.\n",
		"core_rules":         "## Core Rules\n\n- Use kd for CRUD, gb for orchestration.\n- Never use TodoWrite or markdown for task tracking.\n- Multi-step work MUST be an epic with child tasks.\n",
		"commands":           "## Commands\n\n- gb ready — find work\n- kd claim — claim a task\n- gb done — finish\n- gb decision — request human input\n",
		"workflows":          "## Workflows\n\nStandard flow: claim → work → commit → push → close.\n",
		"decisions":          "## Decisions\n\nUse gb decision create for human input. Every option must declare an artifact_type.\n",
		"session_resumption": "## Session Resumption\n\nAfter compaction, run gb prime.\n",
		"lifecycle":          "## Lifecycle\n\nClaim before work. Yield when blocked. Stop when done.\n",
		"stop_gate":          "## Stop Gate\n\nBefore stopping: verify commits pushed, tests green, bead closed.\n",
	}
	raw, _ := json.Marshal(cfg)

	// Gardener-scoped prerun bead so the gardener/* budget cases exercise the
	// prerun path with its output COUNTED against the prime budget (ka-0000007894
	// explicitly requires this). Uses deterministic `printf` payloads (~2 KB
	// combined, mirroring the measured `kd show` + `gb news` result sizes) rather
	// than real kd/gb — so the test is hermetic yet the injected bytes are real.
	prerunCfg, _ := json.Marshal(map[string]any{
		"prerun": []map[string]any{
			{"id": "assigned", "cmd": "printf 'assigned-bead-detail %.0s' $(seq 1 60)", "heading": "kd show", "retracts_sections": []string{"lifecycle:read-task-bead"}},
			{"id": "news", "cmd": "printf 'fleet-news-row %.0s' $(seq 1 60)", "heading": "gb news", "retracts_sections": []string{"workflows:gardener-loop-assess-news"}},
		},
	})

	return []budgetBead{
		{
			ID: "gd-cfg-claude-instructions", Title: "claude-instructions",
			Description: string(raw),
			Type:        "config", Kind: "config", Status: "open",
			Labels: []string{"global"},
		},
		{
			ID: "gd-cfg-prerun-gardener", Title: "prerun",
			Description: string(prerunCfg),
			Type:        "config", Kind: "config", Status: "open",
			Labels: []string{"role:gardener"},
		},
	}
}

// startBudgetDaemon stands up an httptest server matching the queries
// runPrime issues against the real beads daemon. Returns a teardown.
func startBudgetDaemon(t *testing.T, advice, configs []budgetBead) func() {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Roster — empty.
		if r.URL.Path == "/v1/agents/roster" {
			_ = json.NewEncoder(w).Encode(map[string]any{"actors": []any{}, "unclaimed_tasks": []any{}})
			return
		}

		// GetBead by id — 404 (no agent/user bead lookups exercised).
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.URL.Path != "/v1/beads/" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// ListBeadsFiltered — dispatch by type.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			typ := r.URL.Query().Get("type")
			var out []budgetBead
			switch typ {
			case "advice":
				out = advice
			case "config":
				out = configs
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": out, "total": len(out)})
			return
		}

		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}))

	old := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("new beadsapi client: %v", err)
	}
	daemon = c
	return func() {
		daemon = old
		srv.Close()
	}
}

// TestPrimeOutputSizeBudget measures gb prime size for each (role, project)
// pair we ship and fails if either the total prime body or the advice
// section busts its byte budget. Covers both project=gardenio (smaller advice
// catalog) and project=monorepo (~53 project advice items as of
// gd-VtgunBw4KK, the task that extended this test) so a monorepo-specific
// regression cannot silently slip past CI.
func TestPrimeOutputSizeBudget(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	cases := []struct {
		name    string
		role    string
		project string
		agent   string
	}{
		{"crew/gardenio", "crew", "gardenio", "gardenio/crews/budget-test-crew"},
		{"gardener/gardenio", "gardener", "gardenio", "gardenio/gardeners/budget-test-gardener"},
		{"crew-e2e/gardenio", "crew-e2e", "gardenio", "gardenio/crew-e2es/budget-test-crewe2e"},
		{"crew/monorepo", "crew", "monorepo", "monorepo/crews/budget-test-crew"},
		{"gardener/monorepo", "gardener", "monorepo", "monorepo/gardeners/budget-test-gardener"},
	}

	// Force a clean flag state — test must not depend on cobra parsing.
	oldFor, oldNoAdvice, oldSize := primeForAgent, primeNoAdvice, primeSizeOnly
	primeNoAdvice = false
	primeSizeOnly = false
	defer func() {
		primeForAgent, primeNoAdvice, primeSizeOnly = oldFor, oldNoAdvice, oldSize
	}()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BOAT_ROLE", c.role)
			t.Setenv("BOAT_PROJECT", c.project)
			t.Setenv("KD_ACTOR", "")
			t.Setenv("KD_AGENT_ID", "")
			t.Setenv("BOAT_AGENT_STATE", "")
			t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
			primeForAgent = c.agent

			var stdout, stderr bytes.Buffer
			if err := runPrimeTo(&stdout, &stderr); err != nil {
				t.Fatalf("runPrimeTo: %v", err)
			}

			total := stdout.Len()
			// Re-derive advice size by recomputing it the same way runPrimeTo
			// does — keeps the test independent of stderr log format churn.
			_, advice, _ := buildPrimeBody(c.agent, c.role)
			adviceBytes := advice.Len()

			t.Logf("BASELINE %s: total=%d bytes advice=%d bytes (budget total=%d advice=%d)",
				c.name, total, adviceBytes, primeBudgetTotal, primeBudgetAdvice)

			report := func(label string, got, budget int) {
				if got <= budget {
					return
				}
				t.Errorf("%s: %s size %d > budget %d (over by %d bytes) — tighten advice budgets or audit project advice catalog (see gd-VtgunBw4KK)",
					c.name, label, got, budget, got-budget)
			}
			report("total prime", total, primeBudgetTotal)
			report("advice section", adviceBytes, primeBudgetAdvice)

			// Sanity: the stderr summary line must always be emitted so it
			// shows up in mezmo logs from SessionStart hooks (gate 3 of
			// gd-r2FnwU2QBc). Don't pin the exact format — just the prefix.
			if !bytes.Contains(stderr.Bytes(), []byte("[prime] role="+c.role)) {
				t.Errorf("%s: stderr missing [prime] size summary; got %q", c.name, stderr.String())
			}
		})
	}
}

// TestSystemPromptBudget_WithPrerun pins that the system-prompt body — the
// channel prerun rides (buildSystemPromptBody / --append-system-prompt-file) —
// stays under the total prime budget for a gardener WITH a prerun config bead
// active, so the injected prerun output is genuinely counted (ka-0000007894).
// buildSystemPromptBody is a superset delivery path (no size flag), so the same
// primeBudgetTotal ceiling applies.
func TestSystemPromptBudget_WithPrerun(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	oldFor, oldNoAdvice := primeForAgent, primeNoAdvice
	primeForAgent = "gardenio/gardeners/budget-test-gardener"
	primeNoAdvice = false
	defer func() { primeForAgent, primeNoAdvice = oldFor, oldNoAdvice }()

	t.Setenv("BOAT_ROLE", "gardener")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TASK_ID", "gd-MISSION1")

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, "gardenio/gardeners/budget-test-gardener", "gardener")
	out := buf.String()

	// Prove prerun actually fired and its output was counted (not a no-op path).
	if !strings.Contains(out, "## Prerun results") {
		t.Fatalf("gardener system-prompt body missing ## Prerun results — prerun did not fire:\n%.500s", out)
	}
	if !strings.Contains(out, "assigned-bead-detail") || !strings.Contains(out, "fleet-news-row") {
		t.Errorf("prerun payloads not injected into system-prompt body")
	}
	if got := buf.Len(); got > primeBudgetTotal {
		t.Errorf("gardener system-prompt body with prerun = %d bytes > budget %d (over by %d)", got, primeBudgetTotal, got-primeBudgetTotal)
	}
}

// TestPrimeAdviceRoleBudgetTiering pins the per-role advice-budget tier
// (gd-1F9DRZzxOm): a cheap mechanical role (isCheapAdviceRole) must render a
// strictly SMALLER advice section than a default role over the identical
// matched catalog — because its lower-priority items spill from full-summary to
// compact title-only lines — while STILL surfacing every advice title (nothing
// is dropped, only abbreviated harder). Uses a global-only corpus so the
// matched item set is identical across roles and the only variable is the
// budget tier.
func TestPrimeAdviceRoleBudgetTiering(t *testing.T) {
	// 40 global advice items, each with a body long enough that its abbreviated
	// render (~title + 500-char summary + pointer) is ~600 bytes. 40 items
	// (~24 KB) fits under the default 60 KB abbreviated cap in full, but busts
	// the 16 KB cheap-role cap so ~a third spill to title-only.
	var corpus []budgetBead
	for i := range 40 {
		corpus = append(corpus, budgetBead{
			ID:    fmt.Sprintf("gd-tier%03d", i),
			Title: fmt.Sprintf("Global tiering rule #%d — a distinct advice title", i),
			Description: strings.Repeat(
				fmt.Sprintf("Body sentence %d explaining the rule, its rationale, and the edge case. ", i), 8),
			Type: "advice", Kind: "data", Status: "open",
			Labels: []string{"global"},
			Fields: map[string]string{"confidence": "0.7"},
		})
	}
	cleanup := startBudgetDaemon(t, corpus, nil)
	defer cleanup()

	// Neutralize any ambient env override so the role tier is what's exercised.
	t.Setenv(primeAdviceBudgetEnv, "")
	t.Setenv(primeAdviceTitleBudgetEnv, "")

	render := func(role string) string {
		var buf bytes.Buffer
		outputAdvice(&buf, "gardenio/probe/tier-test", role)
		return buf.String()
	}

	defaultOut := render("crew") // default budget tier
	cheapOut := render("thread") // cheap mechanical tier
	if !isCheapAdviceRole("thread") {
		t.Fatalf("precondition: 'thread' should be a cheap advice role")
	}
	if isCheapAdviceRole("crew") {
		t.Fatalf("precondition: 'crew' should NOT be a cheap advice role")
	}

	if len(cheapOut) >= len(defaultOut) {
		t.Errorf("cheap-role advice section (%d bytes) should be smaller than default-role (%d bytes)",
			len(cheapOut), len(defaultOut))
	}
	t.Logf("advice bytes: default(crew)=%d cheap(thread)=%d saved=%d",
		len(defaultOut), len(cheapOut), len(defaultOut)-len(cheapOut))

	// Every advice title must still surface in the cheap render (as an
	// abbreviated item or a title-only line) — the tier tightens summaries, it
	// does not drop advice.
	for i := range 40 {
		id := fmt.Sprintf("gd-tier%03d", i)
		if !strings.Contains(cheapOut, id) {
			t.Errorf("cheap-role render dropped advice %s — every title must still surface:\n%.800s", id, cheapOut)
		}
	}
	// And the cheap render must not have hit the hard "N more … not shown"
	// footer: the title-only tier is generous enough to hold all overflow.
	if strings.Contains(cheapOut, "not shown (budget exceeded)") {
		t.Errorf("cheap-role render hit the hard truncation footer; title budget should absorb overflow:\n%.800s", cheapOut)
	}
}

// TestPrimeSizeOnlyFlag verifies --size short-circuits to a stdout summary
// without dumping the full prime body. Useful for ad-hoc budget checks.
func TestPrimeSizeOnlyFlag(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	oldFor, oldNoAdvice, oldSize := primeForAgent, primeNoAdvice, primeSizeOnly
	primeForAgent = "gardenio/crews/budget-test-crew"
	primeNoAdvice = false
	primeSizeOnly = true
	defer func() {
		primeForAgent, primeNoAdvice, primeSizeOnly = oldFor, oldNoAdvice, oldSize
	}()

	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	var stdout, stderr bytes.Buffer
	if err := runPrimeTo(&stdout, &stderr); err != nil {
		t.Fatalf("runPrimeTo: %v", err)
	}

	out := stdout.String()
	if !strings.HasPrefix(out, "[prime] role=crew project=gardenio") {
		t.Errorf("--size stdout should start with [prime] summary; got %q", out)
	}
	if strings.Contains(out, "## Advice") {
		t.Errorf("--size stdout should NOT contain rendered prime body; got %q", out)
	}
}
