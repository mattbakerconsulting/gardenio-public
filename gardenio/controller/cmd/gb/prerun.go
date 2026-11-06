package main

// prerun.go implements the per-role "prerun" subsystem (ka-0000007894).
//
// A prerun is a small set of read-only boot commands whose OUTPUT is
// pre-executed at coop session start and injected into the cached system
// prompt as a `## Prerun results` section — so an agent (gardeners and
// plan-gardeners first) does NOT have to spend its first turn running
// `kd show <assigned>` + `gb news` and paying for the tool-call + result +
// the boot-reasoning that surrounds it.
//
// Measured motivation (ka-0000007971): gardeners/plan-gardeners spend 45–78% of
// their first turns on boot ritual; every one of 11 sampled pods ran
// `kd show <assigned>` and `gb news` in turns 1–6. Crew doing real code work
// spent only 8–9%, which is why prerun is layered PER ROLE (config beads scoped
// role:gardener / role:plan-gardener) rather than applied globally.
//
// Design contract (all from ka-0000007971 / ka-0000007894):
//   - Injection rides the system-prompt file (writeSystemPromptFile /
//     --append-system-prompt-file), NOT the SessionStart hook. The hook channel
//     truncates at ~2 KB and spills the rest to a file the agent must spend a
//     tool call to read — which would spend exactly the budget prerun saves. The
//     system-prompt file is uncapped and cache-friendly.
//   - Read-only commands ONLY. A prerun re-fires on every resume, so a mutating
//     command (kd create / kd claim) on a crashed-and-resumed agent would mint
//     duplicate beads or orphans. Mutating verbs are rejected at resolve time.
//   - Fail-open. A non-zero exit, a timeout, or empty output omits that one
//     section and still launches the agent (mirrors writeSystemPromptFile's
//     best-effort contract).
//   - Per-entry byte cap. A pathological command output is truncated before it
//     is appended so it can't blow the prime size budget.
//   - Every entry declares what it RETRACTS (an init_prompts.go section and/or
//     an advice bead id), removed in the SAME change — injection without
//     retraction is pure additive cost (the ka-0000002045 trap).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// prerunListOnly, when set, prints the resolved prerun entries (without
// executing their commands) instead of the rendered results section.
var prerunListOnly bool

// prerunCmd renders the `## Prerun results` section this agent's role would get
// injected into its system prompt — the same output buildSystemPromptBody
// injects. Useful for operators and E2E to verify what a gardener/plan-gardener
// sees at boot without spawning a pod.
var prerunCmd = &cobra.Command{
	Use:   "prerun",
	Short: "Render the per-role prerun results section injected into the system prompt",
	Long: `Resolves this agent's role prerun config beads, executes their read-only
boot commands, and prints the resulting '## Prerun results' section — exactly
what buildSystemPromptBody injects into the cached system prompt at coop
session start.

Role is taken from BOAT_ROLE; the assigned task bead from BOAT_TASK_ID.

  gb prerun            # execute + render the section (what the agent sees)
  gb prerun --list     # list the resolved entries without executing them`,
	GroupID: "session",
	RunE:    runPrerun,
}

func init() {
	prerunCmd.Flags().BoolVar(&prerunListOnly, "list", false, "list resolved prerun entries without executing them")
}

func runPrerun(cmd *cobra.Command, args []string) error {
	role := os.Getenv("BOAT_ROLE")
	if prerunListOnly {
		entries := resolvePrerunEntries(context.Background(), daemon, role, resolvedAssignedBead())
		if len(entries) == 0 {
			fmt.Fprintf(os.Stdout, "(no prerun entries for role %q)\n", role)
			return nil
		}
		for _, e := range entries {
			fmt.Fprintf(os.Stdout, "- %s: %s\n", e.ID, e.Cmd)
		}
		return nil
	}
	outputPrerunResults(os.Stdout, daemon, role)
	return nil
}

// prerunCategory is the config-bead category name (see config_registry.go).
const prerunCategory = "prerun"

// prerunDefaultTimeout bounds a single prerun command. `gb news` is the pole at
// ~1.2 s (ka-0000007971); this leaves generous headroom while still guaranteeing
// a hung command cannot stall agent boot.
const prerunDefaultTimeout = 5 * time.Second

// prerunDefaultMaxBytes caps a single command's captured output before it is
// appended to the system prompt. Keeps a pathological output from blowing the
// prime size budget (prime_size_budget_test.go). `kd show` / `gb news` land
// well under this (~426 / ~257 tokens ≈ <2 KB each).
const prerunDefaultMaxBytes = 8000

// prerunTotalMaxBytes caps the whole rendered `## Prerun results` section as a
// backstop against many entries each near their per-entry cap.
const prerunTotalMaxBytes = 24000

// prerunEntry is one prerun command declaration, decoded from a prerun config
// bead's JSON list.
type prerunEntry struct {
	// ID is a stable identifier used to de-duplicate across layers: a
	// more-specific layer (role) overriding an entry with the same ID replaces
	// the less-specific one instead of running both.
	ID string `json:"id"`

	// Cmd is the shell command line executed via `sh -c`. MUST be read-only —
	// see prerunMutatingVerbs. `{{.AssignedBead}}` is substituted with the
	// agent's assigned task bead id (BOAT_TASK_ID) before execution; an entry
	// that references it is skipped when no bead is assigned.
	Cmd string `json:"cmd"`

	// Heading is the human-readable label rendered as a `### <heading>`
	// sub-section above the command output. Defaults to Cmd if empty.
	Heading string `json:"heading"`

	// TimeoutMS overrides prerunDefaultTimeout for this entry (milliseconds).
	TimeoutMS int `json:"timeout_ms,omitempty"`

	// MaxBytes overrides prerunDefaultMaxBytes for this entry.
	MaxBytes int `json:"max_bytes,omitempty"`

	// RetractsSections names init_prompts.go / prime-body sections this entry
	// obsoletes (documentation + a machine-checkable guard that retraction and
	// injection ship together). Not consumed at runtime; asserted by tests.
	RetractsSections []string `json:"retracts_sections,omitempty"`

	// ReplacesAdvice names advice bead ids this entry obsoletes. Documentation
	// for the same guard; advice suppression itself is handled by the existing
	// supersession filter.
	ReplacesAdvice []string `json:"replaces_advice,omitempty"`
}

// prerunAssignedBeadPlaceholder is substituted in an entry's Cmd with the
// agent's assigned task bead id. An entry referencing it is skipped when no
// bead is assigned (a warm-pool pod before assignment, or a work-seeking role).
const prerunAssignedBeadPlaceholder = "{{.AssignedBead}}"

// prerunMutatingVerbs are command fragments that indicate a state mutation. A
// prerun re-fires on every resume, so any of these disqualifies an entry — it
// is skipped (fail-open) rather than executed. Matched as whitespace-delimited
// tokens so `kd show` is never mistaken for `kd create`.
var prerunMutatingVerbs = []string{
	"kd create", "kd claim", "kd close", "kd update", "kd comment",
	"kd dep", "kd label", "kd delete",
	"gb spawn", "gb start", "gb decision", "gb escalation", "gb squawk",
	"gb stop", "gb yield", "gb mail", "gb done", "gb mr",
}

// resolvedAssignedBead returns the agent's assigned task bead id, or "".
// BOAT_TASK_ID is the canonical pre-assignment carrier (agent_start.go); on a
// warm-pool pod it is only hydrated at assignment, so an empty value correctly
// signals "no assigned bead yet".
func resolvedAssignedBead() string {
	return os.Getenv("BOAT_TASK_ID")
}

// isReadOnlyPrerunCmd reports whether cmd is safe to run as a prerun (i.e.
// contains no known mutating verb). Conservative: an unrecognized command is
// treated as read-only, because the config beads are operator-authored and the
// verb list covers every state-mutating kd/gb verb.
func isReadOnlyPrerunCmd(cmd string) bool {
	// Normalize internal whitespace so "kd   create" still matches "kd create".
	norm := strings.Join(strings.Fields(cmd), " ")
	// Guard each pipeline/&&/; segment independently so a read-only lead
	// command can't smuggle a mutating one after a separator.
	for _, seg := range splitShellSegments(norm) {
		seg = strings.TrimSpace(seg)
		for _, verb := range prerunMutatingVerbs {
			if seg == verb || strings.HasPrefix(seg, verb+" ") {
				return false
			}
		}
	}
	return true
}

// splitShellSegments splits a command line on the shell separators that start a
// new command (| & ; ) so each segment can be verb-checked independently.
func splitShellSegments(cmd string) []string {
	seps := []string{"&&", "||", "|", ";", "&"}
	segs := []string{cmd}
	for _, sep := range seps {
		var next []string
		for _, s := range segs {
			next = append(next, strings.Split(s, sep)...)
		}
		segs = next
	}
	return segs
}

// resolvePrerunEntries resolves the prerun config beads for the given role and
// returns the ordered, de-duplicated, safety-filtered entry list to execute.
//
// Resolution reuses the standard config-bead merge chain (ResolveConfigBeads,
// MergeConcatList): global < project < role < agent layers concatenate their
// `prerun` arrays. Entries are then:
//   - de-duplicated by ID, LATER (more-specific) wins — so a role layer can
//     override a global entry by re-declaring the same id;
//   - dropped if the command is not read-only (fail-open safety);
//   - dropped if the command references {{.AssignedBead}} but no bead is
//     assigned (nothing to show).
//
// Returns nil (not an error) when no prerun beads match — the common case for
// roles with no prerun configured.
func resolvePrerunEntries(ctx context.Context, lister configBeadLister, role, assignedBead string) []prerunEntry {
	if lister == nil {
		return nil
	}
	subs := buildSubscriptions(role)
	merged, count, err := ResolveConfigBeads(ctx, lister, prerunCategory, subs)
	if err != nil || count == 0 || merged == nil {
		return nil
	}

	raw, ok := merged[prerunCategory].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}

	// Decode each entry via a JSON round-trip (the merged map holds
	// map[string]any values from the config-bead JSON). De-dup by ID with
	// last-wins: a later (more-specific) layer re-declaring an id replaces the
	// earlier entry in place, preserving the earlier slot's order.
	var entries []prerunEntry
	seen := make(map[string]int) // id -> index in entries
	for _, item := range raw {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var e prerunEntry
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if strings.TrimSpace(e.Cmd) == "" {
			continue
		}
		if e.ID != "" {
			if idx, dup := seen[e.ID]; dup {
				entries[idx] = e
				continue
			}
			seen[e.ID] = len(entries)
		}
		entries = append(entries, e)
	}

	out := make([]prerunEntry, 0, len(entries))
	for _, e := range entries {
		if !isReadOnlyPrerunCmd(e.Cmd) {
			fmt.Fprintf(os.Stderr, "[prerun] skipping non-read-only entry %q (cmd=%q)\n", e.ID, e.Cmd)
			continue
		}
		if strings.Contains(e.Cmd, prerunAssignedBeadPlaceholder) && assignedBead == "" {
			// Nothing to show — no assigned bead yet (warm pool / work-seeking).
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runPrerunCommand executes a single prerun entry and returns its captured,
// byte-capped output. Best-effort: any failure (timeout, non-zero exit, empty
// output) returns ("", false) and the caller omits the section.
func runPrerunCommand(ctx context.Context, e prerunEntry, assignedBead string) (string, bool) {
	cmdLine := strings.ReplaceAll(e.Cmd, prerunAssignedBeadPlaceholder, assignedBead)

	timeout := prerunDefaultTimeout
	if e.TimeoutMS > 0 {
		timeout = time.Duration(e.TimeoutMS) * time.Millisecond
	}
	maxBytes := prerunDefaultMaxBytes
	if e.MaxBytes > 0 {
		maxBytes = e.MaxBytes
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", cmdLine)
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	// Bound how long Run() blocks after the context kills the process: a killed
	// `sh` can leave a child holding the stdout pipe open, which would otherwise
	// stall Wait() until that child exits (defeating the timeout). WaitDelay
	// forces the pipe closed shortly after the kill so a timed-out prerun really
	// returns promptly. Mirrors runCoopSession's WaitDelay usage.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[prerun] entry %q failed (cmd=%q): %v\n", e.ID, cmdLine, err)
		return "", false
	}

	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", false
	}
	if len(out) > maxBytes {
		out = out[:maxBytes] + "\n… [prerun output truncated]"
	}
	return out, true
}

// outputPrerunResults resolves, executes, and renders the `## Prerun results`
// section for the given role into w. It is a no-op (writes nothing) when no
// prerun entries apply or every entry fails — the fail-open contract.
//
// Called by buildSystemPromptBody so the results land in the cached,
// --append-system-prompt-file-delivered body (not the capped hook channel).
func outputPrerunResults(w io.Writer, lister configBeadLister, role string) {
	assignedBead := resolvedAssignedBead()
	entries := resolvePrerunEntries(context.Background(), lister, role, assignedBead)
	if len(entries) == 0 {
		return
	}

	// Render each entry into a scratch buffer first so we emit the section
	// header only if at least one command produced output, and can enforce the
	// whole-section cap.
	var body bytes.Buffer
	rendered := 0
	for _, e := range entries {
		out, ok := runPrerunCommand(context.Background(), e, assignedBead)
		if !ok {
			continue
		}
		heading := e.Heading
		if heading == "" {
			heading = strings.ReplaceAll(e.Cmd, prerunAssignedBeadPlaceholder, assignedBead)
		}
		fmt.Fprintf(&body, "### %s\n\n", heading)
		fmt.Fprintf(&body, "```\n%s\n```\n\n", out)
		rendered++
		if body.Len() > prerunTotalMaxBytes {
			break
		}
	}
	if rendered == 0 {
		return
	}

	fmt.Fprintln(w, "## Prerun results")
	fmt.Fprintln(w, "The commands below were run for you at session start; their output is current as of boot.")
	fmt.Fprintln(w, "You do NOT need to re-run them on your first turn — read the results here and proceed.")
	fmt.Fprintln(w)
	out := body.String()
	if len(out) > prerunTotalMaxBytes {
		out = out[:prerunTotalMaxBytes] + "\n… [prerun section truncated]\n\n"
	}
	fmt.Fprint(w, out)
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
}
