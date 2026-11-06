package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// beginPrimeBeadCache arms the daemon client's request-scoped GetBead memo for
// the duration of one prime render, and returns the disarm function to defer.
//
// A `gb prime` renders ~a dozen sections, and many of them independently
// re-fetch the SAME two beads: the agent's own bead is read by
// lookupOperatorUserBead, isAutoAssignEnabled, isMemoryEligible,
// outputScheduledLoopContext, outputProgressBlock, outputPriorWorkOnBead,
// outputPriorWrapupOnResume AND inside advice-subscription enrichment
// (EnrichAgentSubscriptions, a different package) — and the operator user bead
// is read twice (outputOperatorIdentity + outputUserPreferences). Every one
// returns the identical ~3.5 KB document, so all but the first are pure
// redundant re-reads (the exact redundant-re-fetch class this token-efficiency
// work targets — ka-0000002397).
//
// The memo lives on the daemon client (beadsapi.Client) rather than in this
// package so it also covers GetBead calls made by the advice package during the
// same render, with no cross-package signature churn. It is disarmed on return
// so it never outlives the render, and it is a no-op for any client that never
// arms it (the controller, bridges). daemon may be nil in a few unit tests.
func beginPrimeBeadCache() func() {
	if daemon == nil {
		return func() {}
	}
	daemon.BeginGetBeadCache()
	return daemon.EndGetBeadCache
}

// lookupOperatorUserBead resolves the spawning user's user bead via the agent
// bead's user_bead_id field (set during thread spawn). Returns nil when the
// agent has no linked user bead or any lookup fails — callers treat that as a
// silent no-op.
func lookupOperatorUserBead() *beadsapi.BeadDetail {
	agentBeadID := os.Getenv("KD_AGENT_ID")
	if agentBeadID == "" {
		return nil
	}

	ctx := context.Background()

	agentBead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		return nil
	}

	userBeadID := agentBead.Fields["user_bead_id"]
	if userBeadID == "" {
		return nil
	}

	userBead, err := daemon.GetBead(ctx, userBeadID)
	if err != nil {
		return nil
	}
	return userBead
}

// outputOperatorIdentity emits a one-line "## Operator" section naming the human
// who spawned this agent and, when known, their pronouns — so the agent refers
// to and addresses them correctly from its very first turn, without an operator
// round-trip. No-op when there is no linked user bead. Pronouns are read
// straight from the user bead's `pronouns` field, so backfilling a user's
// pronouns propagates to every agent they spawn with no further plumbing.
func outputOperatorIdentity(w io.Writer) {
	userBead := lookupOperatorUserBead()
	if userBead == nil {
		return
	}

	name := strings.TrimSpace(userBead.Fields["display_name"])
	if name == "" {
		name = strings.TrimSpace(userBead.Title)
	}
	if name == "" {
		return
	}

	pronouns := strings.TrimSpace(userBead.Fields["pronouns"])

	fmt.Fprintln(w, "## Operator")
	if pronouns != "" {
		fmt.Fprintf(w, "You were spawned by **%s** (%s). Use these pronouns when referring to or addressing them.\n\n", name, pronouns)
	} else {
		fmt.Fprintf(w, "You were spawned by **%s**.\n\n", name)
	}
}

// outputUserPreferences fetches the spawning user's preferences from their user
// bead and outputs them as a markdown section. The agent bead's user_bead_id
// field (set during thread spawn) links to the user bead. No-op if the agent
// has no linked user bead or the user has no preferences.
func outputUserPreferences(w io.Writer) {
	userBead := lookupOperatorUserBead()
	if userBead == nil {
		return
	}
	userBeadID := userBead.ID

	raw := userBead.Fields["preferences"]
	if raw == "" || raw == "null" {
		return
	}

	var prefs map[string]string
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil || len(prefs) == 0 {
		return
	}

	// Output sorted preferences.
	keys := make([]string, 0, len(prefs))
	for k := range prefs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintln(w, "## User Preferences")
	fmt.Fprintf(w, "_From user bead %s (%s)_\n\n", userBeadID, userBead.Title)
	for _, k := range keys {
		fmt.Fprintf(w, "- **%s**: %s\n", k, prefs[k])
	}
	fmt.Fprintln(w)
}

var primeCmd = &cobra.Command{
	Use:   "prime",
	Short: "Output AI-optimized workflow context",
	Long: `Output essential workflow context in AI-optimized markdown format.

Outputs 5 sections:
1. Workflow context — session close protocol, core rules, essential commands
2. Advice — scoped advice beads matching agent subscriptions
3. Jack awareness — active/expired infrastructure jacks
4. Agent roster — live agents with tasks, idle times, crash state
5. Auto-assign — assigns highest-priority ready task if agent is idle

Agent identity is resolved from KD_ACTOR or KD_AGENT_ID env vars,
or the --for flag.

Examples:
  gb prime
  gb prime --for beads/crew/test-agent
  gb prime --no-advice
  gb prime --json
  gb prime --size                             # single-line byte summary for this agent
  gb prime --size --by-source                 # per-(project,role) boot-context token breakdown by source
  gb prime --size --by-source --project gardenio --role crew --csv`,
	GroupID: "session",
	RunE:    runPrime,
}

var (
	primeForAgent     string
	primeNoAdvice     bool
	primeSizeOnly     bool
	primeSystemPrompt bool
	primeProject      string
	primeBySource     bool
	primeBySourceRole string
	primeBySourceCSV  bool
)

func init() {
	primeCmd.Flags().StringVar(&primeForAgent, "for", "", "agent ID to inject matching advice for")
	primeCmd.Flags().BoolVar(&primeNoAdvice, "no-advice", false, "suppress advice output")
	primeCmd.Flags().BoolVar(&primeSizeOnly, "size", false, "report total/advice byte sizes to stdout instead of the full prime body")
	primeCmd.Flags().BoolVar(&primeSystemPrompt, "system-prompt", false, "emit rules-only subset (identity + workflow + advice + wrap-up) suitable for claude --append-system-prompt-file; omits live state (roster, jacks, auto-assign)")
	primeCmd.Flags().StringVar(&primeProject, "project", "", "project context to prime for (overrides $KD_PROJECT / $BOAT_PROJECT); scopes the identity header, project advice, and auto-assign")
	primeCmd.Flags().BoolVar(&primeBySource, "by-source", false, "with --size: emit a per-(project,role)-archetype boot-context token breakdown by source (system_prompt/claude_md/advice/role_nudge/tool_descriptions/mcp) instead of the single-agent summary line")
	primeCmd.Flags().StringVar(&primeBySourceRole, "role", "", "with --size --by-source: measure only this role's archetypes")
	primeCmd.Flags().BoolVar(&primeBySourceCSV, "csv", false, "with --size --by-source: emit CSV instead of an aligned table")
}

func runPrime(cmd *cobra.Command, args []string) error {
	return runPrimeTo(os.Stdout, os.Stderr)
}

// primeSizeReport summarizes the byte cost of a single prime invocation.
// Captured so the runtime can log it (mezmo) and tests can assert on it.
type primeSizeReport struct {
	Role        string
	Project     string
	TotalBytes  int
	AdviceBytes int
	AdviceItems int
}

func (r primeSizeReport) logLine() string {
	return fmt.Sprintf("[prime] role=%s project=%s total_bytes=%d advice_bytes=%d advice_items=%d\n",
		r.Role, r.Project, r.TotalBytes, r.AdviceBytes, r.AdviceItems)
}

// runPrimeTo writes prime output to stdout and a single-line size summary to
// stderr. Pulled out of runPrime so tests can capture both streams without
// mucking with os.Stdout/os.Stderr.
func runPrimeTo(stdout, stderr io.Writer) error {
	agentID := resolvePrimeAgentIdentity(nil)
	role := os.Getenv("BOAT_ROLE")
	project := defaultGBProject()

	// Prewarmed agent standby: while a prewarmed pod is still parked in the
	// idle pool, output a lightweight standby message instead of full workflow
	// context. Once the pool manager assigns work (standbyAssignedSentinel
	// present), fall through to normal priming so the assigned thread/adhoc
	// agent gets its real workflow context — BOAT_AGENT_STATE stays "prewarmed"
	// in the pod env forever, so it cannot gate this on its own (ka-0000004623).
	if isUnassignedPrewarmed() {
		var buf bytes.Buffer
		outputPrewarmedStandby(&buf, agentID)
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			return err
		}
		report := primeSizeReport{Role: role, Project: project, TotalBytes: buf.Len()}
		fmt.Fprint(stderr, report.logLine())
		return nil
	}

	if primeSystemPrompt {
		// --system-prompt: emit only the rules-relevant subset, suitable for
		// `claude --append-system-prompt-file`. Skips live state (roster,
		// jacks, auto-assign) that changes every cycle and would invalidate
		// the system-prompt cache for no benefit.
		var buf bytes.Buffer
		buildSystemPromptBody(&buf, agentID, role)
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			return err
		}
		report := primeSizeReport{Role: role, Project: project, TotalBytes: buf.Len()}
		fmt.Fprint(stderr, report.logLine())
		return nil
	}

	body, advice, sectionSizes := buildPrimeBody(agentID, role)

	// Push the context-composition-at-start gauge (ka-0000007153). Best-effort:
	// silently skipped when VMAGENT_URL is unset (dev shells, tests).
	pushPrimeContextMetrics(context.Background(), role, sectionSizes)

	if primeSizeOnly {
		// --size --by-source: decompose the boot context into its six injection
		// surfaces for EVERY (project,role) archetype, not just this agent — the
		// P1 "Snapshot boot-context by source" breakdown (ka-0000004541). See
		// prime_size_by_source.go.
		if primeBySource {
			return runPrimeBySource(stdout)
		}
		// --size: report only the size summary to stdout (machine-readable).
		// Useful for ad-hoc checks ("is gb prime over budget right now?")
		// without dumping the full body.
		report := primeSizeReport{Role: role, Project: project, TotalBytes: body.Len(), AdviceBytes: advice.Len(), AdviceItems: countAdviceItems(advice.Bytes())}
		fmt.Fprint(stdout, report.logLine())
		return nil
	}

	if _, err := stdout.Write(body.Bytes()); err != nil {
		return err
	}

	report := primeSizeReport{Role: role, Project: project, TotalBytes: body.Len(), AdviceBytes: advice.Len(), AdviceItems: countAdviceItems(advice.Bytes())}
	fmt.Fprint(stderr, report.logLine())
	return nil
}

// buildPrimeBody renders the full prime body and a sidecar buffer holding only
// the advice section. The advice sidecar lets callers measure the advice cost
// in isolation — important because advice is the dominant section and the one
// budget-tightening work is targeting (epic ka-0000008080).
//
// The returned primeSectionSizes maps each rendered section to its byte size,
// measured as the body-buffer growth around the section's render call. It
// feeds the gardenio_prime_hook_bytes{source,...} gauge (ka-0000007153) so
// operators can see the composition of the gb-prime hook slice at agent start;
// sections that render nothing are omitted. (This is only the ~7k gb-prime
// slice, not the full ~70-90k boot — see gardenio_session_boot_context_tokens.)
func buildPrimeBody(agentID, role string) (body, adviceOnly *bytes.Buffer, sizes primeSectionSizes) {
	body = &bytes.Buffer{}
	adviceOnly = &bytes.Buffer{}
	sizes = primeSectionSizes{}

	// Arm the daemon client's request-scoped bead memo so the ~a-dozen sections
	// below (and advice enrichment) share one GET per distinct bead instead of
	// each re-fetching the agent/user bead (ka-0000002397). See beginPrimeBeadCache.
	defer beginPrimeBeadCache()()

	// section runs render and records the body-buffer growth under source.
	// Repeated sources accumulate (e.g. advice + its role diff).
	section := func(source string, render func()) {
		start := body.Len()
		render()
		if d := body.Len() - start; d > 0 {
			sizes[source] += d
		}
	}

	// 0. Identity announcement.
	section("identity", func() { outputIdentity(body, agentID, role) })

	// 0a. Operator identity + pronouns (if spawned by a known user).
	// 0b. User preferences (if spawned by a known user).
	section("operator", func() {
		outputOperatorIdentity(body)
		outputUserPreferences(body)
	})

	// 1. Workflow context.
	section("workflow", func() { outputWorkflowContext(body, role) })

	// 1b. Scheduled-loop context. Renders only when the agent bead has
	// schedule_id set; for non-scheduled agents this is a silent no-op.
	// The block overrides the default gardener "stop every cycle" habit —
	// see proposals/ephemeral-schedules-gardener-primitive.md §2 — so it
	// must appear before advice to prime the agent's lifecycle expectations
	// up-front.
	//
	// 1c. Loop Progress block (live state — only the dynamic prime path,
	// not the cached system-prompt subset). Renders only when the agent's
	// schedule has progress.surface.in_prime: true.
	if agentID != "" {
		section("schedule", func() {
			outputScheduledLoopContext(body, agentID)
			outputProgressBlock(body, agentID)
		})
		// 1d. Prior work on this bead — deliver a predecessor's handoff to a
		// crew agent re-dispatched onto the same task_id (Shape A read half,
		// ka-0000003925). Silent no-op unless the agent carries a task_id AND a
		// prior closed occupant left a handoff/tldr.
		section("prior_work", func() { outputPriorWorkOnBead(body, agentID) })
		// 1e. Your prior turn — surface THIS agent's own archived prior_wrapup
		// after a revive-in-place resume, so a resumed turn continues its own
		// work instead of rediscovering it (ka-0000008136). Distinct from
		// prior_work above (a different occupant's handoff on a shared task_id);
		// silent no-op unless this bead carries an archived prior_wrapup.
		section("prior_wrapup", func() { outputPriorWrapupOnResume(body, agentID) })
		// 1f. Arms duty — an agent holding an armless epic (a plan-gardener:
		// its plan or any phase) is told to choose arms (gd-YvWv4mR4fu). Silent
		// no-op when the held bead is not an epic or already bears arms.
		section("arms", func() { outputArmsDuty(body, agentID, role) })
	}

	// 2. Advice — tee into adviceOnly so callers can measure it separately.
	if !primeNoAdvice && agentID != "" {
		section("advice", func() {
			outputAdvice(io.MultiWriter(body, adviceOnly), agentID, role)
			outputAdviceRoleDiff(body, agentID)
		})
	}

	// 2b. Wrap-up expectations.
	if agentID != "" {
		section("wrapup", func() {
			outputWrapUpExpectations(body, agentID)
		})
	}

	// 3. Jack awareness.
	section("jacks", func() { outputJackSection(body) })

	// 4. Agent roster.
	section("roster", func() { outputRosterSection(body, agentID) })

	// 5. Auto-assign (if agent has no in_progress bead).
	if agentID != "" {
		section("autoassign", func() { outputAutoAssign(body, agentID) })
	}

	// 6. Tldr decision hints — if the agent has an open decision with a
	// tldr option, emit the length budget + example shape up-front so they
	// draft to size.
	section("decision_hints", func() { outputTldrDecisionHints(body, os.Getenv("KD_AGENT_ID")) })

	return body, adviceOnly, sizes
}

// outputTldrDecisionHints scans open decision beads requested by this agent
// and emits a compact length-budget hint whenever an option has
// artifact_type=="tldr". Phase 1 defaults: max_chars=500, min_chars=0,
// warn_at=350. A tone_hint knob can come later (see tldr-defaults, Phase 2).
func outputTldrDecisionHints(w io.Writer, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	ctx := context.Background()

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Sort:     "-created_at",
		Limit:    200,
	})
	if err != nil {
		return
	}

	for _, b := range result.Beads {
		if b.Fields["requesting_agent_bead_id"] != agentBeadID {
			continue
		}
		raw := b.Fields["options"]
		if raw == "" {
			continue
		}
		var opts []map[string]any
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			continue
		}
		for _, opt := range opts {
			at, _ := opt["artifact_type"].(string)
			if at != "tldr" {
				continue
			}
			maxChars, minChars, warnAt := 500, 0, 350
			if params, ok := opt["artifact_params"].(map[string]any); ok {
				if v, ok := params["max_chars"].(float64); ok {
					maxChars = int(v)
				}
				if v, ok := params["min_chars"].(float64); ok {
					minChars = int(v)
				}
				if v, ok := params["warn_at"].(float64); ok {
					warnAt = int(v)
				}
			}
			fmt.Fprintf(w, "\nPending decision %s has a tldr option:\n", b.ID)
			fmt.Fprintf(w, "  Budget: max %d chars (warn %d, min %d).\n", maxChars, warnAt, minChars)
			fmt.Fprintln(w, "  Style: short, one section. Markdown ok; no code fences.")
			fmt.Fprintln(w, "  Lead with the outcome, one line per idea.")
			fmt.Fprintln(w, `  Example shape: "Root cause: X. Fix: Y. Verified: Z."`)
			break // one hint per decision
		}
	}
}

// buildSystemPromptBody renders the rules-only subset of prime: identity,
// user preferences, workflow context (stop gate, lifecycle, core rules), advice,
// and wrap-up expectations. Excludes live-state sections (jacks, roster,
// auto-assign) that change frequently and would invalidate the system prompt
// cache without delivering durable rules.
//
// Used by `gb prime --system-prompt` to generate the file passed to
// `claude --append-system-prompt-file`.
func buildSystemPromptBody(w io.Writer, agentID, role string) {
	// Arm the daemon client's request-scoped bead memo (ka-0000002397) so the
	// operator-identity, prior-work, prior-wrapup, scheduled-loop and advice-
	// enrichment reads below share one GET per distinct bead. See beginPrimeBeadCache.
	defer beginPrimeBeadCache()()
	outputIdentity(w, agentID, role)
	// Slack thread context — bake the originating thread into the system
	// prompt for thread-spawned agents. This bypasses the SessionStart
	// hook's 2 KB output cap (the harness saves anything larger to a file
	// and only injects a preview), so the agent reliably sees the full
	// thread on its first turn without having to run `gb slack thread`.
	outputSlackThreadContext(w, systemPromptThreadContextMaxChars)
	// Sibling beads already filed in this thread, so a RESUMED thread agent
	// discovers prior answers/work instead of re-deriving them (ka-0000001160).
	// Rides the same per-session bake as the thread context above: the
	// system-prompt body is regenerated at each coop session start (i.e. each
	// resume), which is exactly when the freshest sibling list is needed.
	outputThreadSiblings(w)
	outputOperatorIdentity(w)
	outputUserPreferences(w)
	outputWorkflowContext(w, role)
	// Per-role prerun results (ka-0000007894): pre-execute this role's read-only
	// boot commands (e.g. `kd show <assigned>` + `gb news` for gardeners) and
	// inject their output here, in the cached system-prompt body, so the agent
	// does not spend its first turn re-running them. No-op for roles with no
	// prerun config bead (the crew default). Rides this uncapped channel rather
	// than the ~2 KB SessionStart hook (ka-0000007971 finding 1).
	outputPrerunResults(w, daemon, role)
	// Scheduled-loop context belongs in the cacheable system-prompt subset:
	// the yield-vs-stop rule is a durable behavior override, not live state.
	if agentID != "" {
		outputScheduledLoopContext(w, agentID)
		// Prior-run handoff for a crew agent re-dispatched onto a shared
		// task_id (Shape A read half, ka-0000003925). The predecessor is
		// already closed at spawn time, so this is a durable fact worth baking
		// into the cacheable system-prompt subset — the agent sees it on its
		// first turn without running `gb prime`. Silent no-op for the common
		// fresh-spawn case (no task_id or no prior occupant).
		outputPriorWorkOnBead(w, agentID)
		// This agent's own archived prior_wrapup after a revive-in-place resume
		// (ka-0000008136). Like the prior-work handoff above it is a durable fact
		// present at spawn time, so it rides the cacheable system-prompt subset;
		// silent no-op unless the bead carries an archived prior_wrapup.
		outputPriorWrapupOnResume(w, agentID)
		// Arms duty for an armless held epic (gd-YvWv4mR4fu). Each gardener run
		// is a fresh session, so baking it here reaches every run, including
		// those of gardeners bound before the playbook carried the duty.
		outputArmsDuty(w, agentID, role)
	}
	if !primeNoAdvice && agentID != "" {
		outputAdvice(w, agentID, role)
		outputAdviceRoleDiff(w, agentID)
	}
	if agentID != "" {
		outputWrapUpExpectations(w, agentID)
	}
	// Salvaged-WIP recovery note (epic ka-0000005916 step B2): when a prior
	// incarnation's WIP was auto-restored into this pod's repo(s) on respawn,
	// tell the agent it exists and how to recover it so it cherry-picks instead
	// of redoing the work. No-op (unset env) for the common fresh-spawn case, so
	// it never touches the system-prompt budget for agents with nothing restored.
	outputSalvageRestoreNote(w)
	// Memory pointer — short ~500-char block for non-memory-eligible agents.
	// Memory-eligible agents (gardeners, scheduled-loop) get the full Claude
	// Code auto-memory preamble injected by the harness itself; this is a
	// no-op for them. See ka-0000006599.
	outputMemoryPointer(w, agentID, role)
}

// adviceCountRE matches the "## Advice (N items)" header outputAdvice writes.
var adviceCountRE = regexp.MustCompile(`(?m)^## Advice \((\d+) items\)`)

// countAdviceItems extracts the rendered item count from an advice section
// buffer. Returns 0 when the section is empty (no header was emitted).
func countAdviceItems(advice []byte) int {
	m := adviceCountRE.FindSubmatch(advice)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return n
}

// outputPrewarmedStandby outputs a standby message for prewarmed agents.
// These agents have Claude running but are waiting for work assignment via nudge.
func outputPrewarmedStandby(w io.Writer, agentID string) {
	fmt.Fprint(w, `# Prewarmed Agent — Standby Mode

You are a **prewarmed agent** waiting in the idle pool for work assignment.

**Do NOT** seek work, run `+"`gb ready`"+`, or create beads. Your session is warm and
ready — when a Slack thread mention or operator assigns you, the pool manager
will inject a nudge with your task description.

**What to do:** Wait for a nudge message. It will contain:
- Thread context (Slack channel, thread, description)
- Your assigned task details

When you receive the nudge, treat it as your starting prompt and begin work immediately.
Follow the standard workflow: claim the task, do the work, commit, push, close the bead, `+"`gb done`"+`.
`)
	if agentID != "" {
		fmt.Fprintf(w, "\nAgent: **%s**\n", agentID)
	}
}

// resolvePrimeAgentIdentity resolves the agent identity for prime output.
func resolvePrimeAgentIdentity(cmd *cobra.Command) string {
	if primeForAgent != "" {
		return primeForAgent
	}
	if v := os.Getenv("KD_ACTOR"); v != "" {
		return v
	}
	if v := os.Getenv("KD_AGENT_ID"); v != "" {
		return v
	}
	if actor != "" && actor != "unknown" {
		return actor
	}
	return ""
}

// outputIdentity outputs the agent's identity (name, role, project) so it can
// announce itself at session start. This runs before workflow context so the
// agent knows who it is from the very first line of prime output.
func outputIdentity(w io.Writer, agentID, role string) {
	project := defaultGBProject()

	// Only output if we have meaningful identity info.
	if agentID == "" && role == "" && project == "" {
		return
	}

	fmt.Fprintln(w, "## Your Identity")
	fmt.Fprintln(w)
	if agentID != "" {
		fmt.Fprintf(w, "- **Agent**: %s\n", agentID)
	}
	if role != "" {
		fmt.Fprintf(w, "- **Role**: %s\n", role)
	}
	if project != "" {
		fmt.Fprintf(w, "- **Project**: %s\n", project)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Announce your role and project in your first message to the team (e.g. via `gb squawk`).")
	fmt.Fprintln(w)
}

// outputWorkflowContext fetches workflow context from config beads (claude-instructions
// category), falling back to hardcoded defaults.
func outputWorkflowContext(w io.Writer, role string) {
	ctx := context.Background()

	// Build subscriptions for config resolution.
	// Use buildSubscriptions to properly handle comma-separated BOAT_ROLE
	// (e.g. "gardener,crew" → ["global", "role:gardener", "role:crew"]).
	subs := buildSubscriptions(role)

	merged, count, _ := ResolveConfigBeads(ctx, daemon, "claude-instructions", subs)
	if count > 0 {
		outputConfigSections(w, merged)
		return
	}

	fmt.Fprintln(w, "<!-- no claude-instructions config beads found — run EnsureConfigs or seed config beads -->")
}

// outputConfigSections outputs resolved config bead sections in canonical order.
func outputConfigSections(w io.Writer, config map[string]any) {
	sections := []string{
		"prime_header", "session_close", "core_rules", "commands",
		"workflows", "decisions", "session_resumption", "lifecycle", "stop_gate",
	}
	for _, key := range sections {
		if val, ok := config[key].(string); ok && val != "" {
			fmt.Fprintln(w, val)
		}
	}
}

// outputJackSection fetches active/expired jacks and outputs warnings.
func outputJackSection(w io.Writer) {
	ctx := context.Background()

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"jack"},
		Statuses: []string{"in_progress"},
		Limit:    50,
	})
	if err != nil || len(result.Beads) == 0 {
		return
	}

	now := time.Now()
	type jackSummary struct {
		bead       *beadsapi.BeadDetail
		target     string
		remaining  time.Duration
		expiredAgo time.Duration
		expired    bool
	}

	var jacks []jackSummary
	for _, b := range result.Beads {
		j := jackSummary{bead: b}
		j.target = b.Fields["jack_target"]

		if b.DueAt != "" {
			if dueAt, err := time.Parse(time.RFC3339, b.DueAt); err == nil {
				if now.After(dueAt) {
					j.expired = true
					j.expiredAgo = now.Sub(dueAt)
				} else {
					j.remaining = time.Until(dueAt)
				}
			}
		}

		jacks = append(jacks, j)
	}

	fmt.Fprintf(w, "\n## Active Jacks (%d)\n\n", len(jacks))
	for _, j := range jacks {
		agent := j.bead.Assignee
		if agent == "" {
			agent = j.bead.CreatedBy
		}
		if j.expired {
			fmt.Fprintf(w, "- **EXPIRED** `%s` on `%s` (by %s, expired %s ago) — run `kd jack down %s`\n",
				j.bead.ID, j.target, agent, formatDuration(j.expiredAgo), j.bead.ID)
		} else {
			remaining := "unknown"
			if j.remaining > 0 {
				remaining = formatDuration(j.remaining) + " remaining"
			}
			fmt.Fprintf(w, "- `%s` on `%s` (by %s, %s)\n",
				j.bead.ID, j.target, agent, remaining)
		}
	}
	fmt.Fprintln(w)
}

// outputRosterSection fetches the live agent roster and outputs it.
func outputRosterSection(w io.Writer, self string) {
	ctx := context.Background()

	roster, err := daemon.GetAgentRoster(ctx, 1800) // 30-min threshold
	if err != nil || roster == nil || len(roster.Actors) == 0 {
		return
	}

	// Partition into active vs stale.
	const staleThresholdSecs = 600 // 10 minutes
	var active, stale []beadsapi.RosterActor
	for _, a := range roster.Actors {
		if a.Reaped {
			stale = append(stale, a)
			continue
		}
		isStopped := a.LastEvent == "Stop" && a.IdleSecs > 60
		if isStopped {
			continue
		}
		if a.IdleSecs > staleThresholdSecs {
			stale = append(stale, a)
		} else {
			active = append(active, a)
		}
	}

	if len(active) == 0 && len(stale) == 0 {
		return
	}

	fmt.Fprintf(w, "\n## Active Agents (%d)\n\n", len(active))
	if self != "" {
		fmt.Fprintf(w, "You are **%s**. Do not pick up other agents' in-progress tasks.\n\n", self)
	}

	for _, a := range active {
		idleStr := formatIdleDur(a.IdleSecs)
		youTag := ""
		if self != "" && a.Actor == self {
			youTag = " <- you"
		}

		if a.TaskID != "" {
			epicStr := ""
			if a.EpicTitle != "" {
				epicStr = fmt.Sprintf(" (epic: %s)", a.EpicTitle)
			}
			fmt.Fprintf(w, "- **%s**%s — working on %s: %s%s (idle %s)\n",
				a.Actor, youTag, a.TaskID, a.TaskTitle, epicStr, idleStr)
		} else {
			activityHint := ""
			if a.ToolName != "" {
				activityHint = fmt.Sprintf(", last: %s", a.ToolName)
			}
			fmt.Fprintf(w, "- **%s**%s — active, no claimed task (idle %s%s)\n",
				a.Actor, youTag, idleStr, activityHint)
		}
	}

	// Show stale agents.
	if len(stale) > 0 {
		var crashed, idle []string
		for _, a := range stale {
			idleStr := formatIdleDur(a.IdleSecs)
			if a.Reaped {
				if a.TaskID != "" {
					crashed = append(crashed, fmt.Sprintf("%s (had %s: %s)", a.Actor, a.TaskID, a.TaskTitle))
				} else {
					crashed = append(crashed, fmt.Sprintf("%s (idle %s)", a.Actor, idleStr))
				}
			} else {
				idle = append(idle, fmt.Sprintf("%s (idle %s)", a.Actor, idleStr))
			}
		}
		if len(crashed) > 0 {
			fmt.Fprintf(w, "\n_Crashed (%d): %s_\n", len(crashed), strings.Join(crashed, ", "))
		}
		if len(idle) > 0 {
			fmt.Fprintf(w, "\n_Stale (%d, likely disconnected): %s_\n", len(idle), strings.Join(idle, ", "))
		}
	}

	// Show unclaimed work.
	if len(roster.UnclaimedTasks) > 0 {
		fmt.Fprintf(w, "\n> **Unclaimed in-progress work** (no assignee — consider claiming):\n")
		for _, t := range roster.UnclaimedTasks {
			fmt.Fprintf(w, ">   - %s [P%d]: %s\n", t.ID, t.Priority, t.Title)
		}
	}

	fmt.Fprintln(w)
}

// isAutoAssignEnabled checks whether auto-assignment is enabled for this agent.
// Inheritance: agent bead overrides project bead. Default is DISABLED.
// Agents are ephemeral and should ask for work on startup rather than being
// auto-assigned. Set auto_assign=true on the agent or project bead to opt in.
func isAutoAssignEnabled(ctx context.Context, proj string) bool {
	// 1. Agent bead (highest precedence).
	if agentBeadID := os.Getenv("KD_AGENT_ID"); agentBeadID != "" {
		if agentBead, err := daemon.GetBead(ctx, agentBeadID); err == nil {
			if v, ok := agentBead.Fields["auto_assign"]; ok {
				return v == "true"
			}
		}
	}

	// 2. Project bead.
	if proj != "" {
		result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"project"},
			Statuses: []string{"open", "in_progress", "blocked", "deferred"},
			Limit:    50,
		})
		if err == nil {
			for _, b := range result.Beads {
				name := strings.TrimPrefix(b.Title, "Project: ")
				if name == proj {
					if b.Fields["auto_assign"] == "true" {
						return true
					}
					break
				}
			}
		}
	}

	// 3. Default: disabled (agents pull work, not pushed).
	return false
}

// outputAutoAssign checks if the agent has in_progress beads and auto-assigns
// the highest-priority ready task if idle. Skips if BOAT_TASK_ID is set (the
// agent was spawned with a specific pre-assigned task) or if auto_assign is
// disabled on the agent or project bead.
func outputAutoAssign(w io.Writer, agentID string) {
	if os.Getenv("BOAT_TASK_ID") != "" {
		return
	}

	// Require a project context to prevent cross-project assignment.
	proj := defaultGBProject()
	if proj == "" {
		return // no project context — refuse to auto-assign
	}

	ctx := context.Background()

	// Check auto_assign setting (agent bead overrides project bead).
	if !isAutoAssignEnabled(ctx, proj) {
		return
	}

	// Check if agent already has in_progress work.
	resp, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Assignee: agentID,
		Statuses: []string{"in_progress"},
		Limit:    1,
	})
	if err != nil || len(resp.Beads) > 0 {
		return // agent already has work
	}

	// Fetch ready issue-kind tasks scoped to this agent's project.
	ready, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:   []string{"open"},
		Labels:     []string{"project:" + proj},
		Kinds:      []string{"issue"},
		NoOpenDeps: true,
		Sort:       "priority",
		Limit:      1,
	})
	if err != nil || len(ready.Beads) == 0 {
		return
	}

	// Verify the candidate bead actually belongs to our project.
	// Belt-and-suspenders check: the server filters by label, but we
	// double-check client-side to avoid cross-project assignment.
	task := ready.Beads[0]
	hasProjectLabel := false
	for _, l := range task.Labels {
		if l == "project:"+proj {
			hasProjectLabel = true
			break
		}
	}
	if !hasProjectLabel {
		return
	}

	// Auto-claim.
	inProgress := "in_progress"
	err = daemon.UpdateBead(ctx, task.ID, beadsapi.UpdateBeadRequest{
		Assignee: &agentID,
		Status:   &inProgress,
	})
	if err != nil {
		fmt.Fprintf(w, "\nAuto-claim failed for %s: %v\n", task.ID, err)
		return
	}

	fmt.Fprintf(w, "\nAuto-assigned bead %s: %s\n", task.ID, task.Title)
	fmt.Fprintf(w, "Run `kd show %s` for full details.\n", task.ID)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func formatIdleDur(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", int(secs))
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%ds", int(secs)/60, int(secs)%60)
	}
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	return fmt.Sprintf("%dh%dm", h, m)
}
