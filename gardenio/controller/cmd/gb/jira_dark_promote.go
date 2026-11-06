package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
)

// jira_dark_promote.go implements Stage-3 TRICKLE of R7 JIRA Dark Mode
// (ka-0000005212, master epic ka-0000005560): the operator-gated promotion of a
// dark resolution bead (`source:jira-dark`) into a REAL upstream Jira ticket.
//
// Trust-boundary shape (research ka-0000004670 §4): the Jira WRITE token lives
// in exactly one place — an operator-side, separately-scoped, create-only
// executor — never in a crew/agent pod (the dark-Jira write-token fence,
// ka-0000005242, strips it out). So the promotion is a two-move handshake:
//
//  1. An agent files an operator ESCALATION (`gb escalation create
//     --operation-type jira_promote --close-on-execute
//     --requesting-task <this task>`; no --execution-target, so the escalation
//     policy routes it to the operator-side executor, gd-iNgsblfqCx). Its
//     execution_script — emitted verbatim by `gb jira-dark promote-script` —
//     POSTs /rest/api/2/issue with the create-only token, captures the real
//     KEY, and calls `gb jira-dark stamp`.
//  2. `gb jira-dark stamp <dark-bead> <KEY>` is the ONLY self-stamp primitive:
//     it idempotently adopts the dark bead AS the real Jira mirror by writing
//     jira_key + promoted_to_jira_key + jira_url and adding source:jira +
//     jira:<KEY>. Once stamped, the one-way ingestion poller DEDUPS onto this
//     bead (server-side `Labels:["jira:"+KEY]` check) instead of creating a
//     duplicate mirror — the dark bead cleanly BECOMES the mirror.
//
// PEEL-ON-DEMAND: promotion is low-volume, curated, human-reviewed — one bead
// at a time, behind the escalation approval gate. The self-stamp is written to
// be exactly-once: the promoted_to_jira_key idempotency guard makes a replayed
// or re-approved escalation a no-op rather than a second upstream ticket.

// jiraKeyPattern matches a real upstream Jira issue key (PROJECT-NNN). It is
// deliberately strict — a promotion must never stamp a synthetic dark key
// (`DEVOPS-D1`) or a malformed value onto the sync-eligible jira_key field.
var jiraKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[1-9][0-9]*$`)

// darkPromotionStamp is the deterministic, idempotent set of writes that adopt a
// promoted dark bead as a real Jira mirror. It is computed by planPromotionStamp
// (pure — no I/O) so the exact field/label delta is unit-testable.
type darkPromotionStamp struct {
	// Fields is the typed field patch (jira_key, promoted_to_jira_key, and
	// optionally jira_url / promoted_by).
	Fields map[string]any
	// AddLabels are the labels to add — source:jira (so the poller's
	// source:jira-scoped catch-up/self-heal adopts the bead) and jira:<KEY>
	// (the server-side dedup key createBeadFromIssue honors).
	AddLabels []string
	// NoOp is true when the bead already carries promoted_to_jira_key == KEY:
	// a safe idempotent replay with nothing to write.
	NoOp bool
}

// planPromotionStamp computes the idempotent stamp promoting a dark bead to
// jiraKey. It is the SOLE author of the jira_key + promoted_to_jira_key fields
// and the jira:<KEY>/source:jira labels on a dark bead.
//
// Guards (all fail-closed):
//   - jiraKey must be a real PROJECT-NNN key (never a synthetic dark key).
//   - the bead must carry source:jira-dark (promotion is only for dark
//     resolutions — refuse to stamp anything else).
//   - promoted_to_jira_key idempotency: already == jiraKey ⇒ NoOp; already a
//     DIFFERENT key ⇒ refuse (a re-approved escalation must not mint a second
//     upstream ticket).
//   - a pre-existing, different jira_key (a native mirror, or a collision) ⇒
//     refuse to overwrite.
func planPromotionStamp(bead *beadsapi.BeadDetail, jiraKey, baseURL, promotedBy string) (darkPromotionStamp, error) {
	if bead == nil {
		return darkPromotionStamp{}, fmt.Errorf("nil bead")
	}
	jiraKey = strings.TrimSpace(jiraKey)
	if !jiraKeyPattern.MatchString(jiraKey) {
		return darkPromotionStamp{}, fmt.Errorf("%q is not a valid upstream Jira issue key (want PROJECT-NNN)", jiraKey)
	}
	if !slices.Contains(bead.Labels, "source:jira-dark") {
		return darkPromotionStamp{}, fmt.Errorf("bead %s is not a dark resolution (missing source:jira-dark label) — refusing to promote", bead.ID)
	}

	// Idempotency on the promotion marker (exactly-once create).
	if prev := strings.TrimSpace(bead.Fields["promoted_to_jira_key"]); prev != "" {
		if prev == jiraKey {
			return darkPromotionStamp{NoOp: true}, nil
		}
		return darkPromotionStamp{}, fmt.Errorf("bead %s already promoted to %s — refusing to re-stamp with %s", bead.ID, prev, jiraKey)
	}
	// A pre-existing jira_key with no promotion marker means this bead is (or
	// collides with) a natively-ingested mirror; never overwrite it.
	if prev := strings.TrimSpace(bead.Fields["jira_key"]); prev != "" && prev != jiraKey {
		return darkPromotionStamp{}, fmt.Errorf("bead %s already bound to jira_key %s — refusing to overwrite with %s", bead.ID, prev, jiraKey)
	}

	fields := map[string]any{
		"jira_key":             jiraKey,
		"promoted_to_jira_key": jiraKey,
	}
	if promotedBy != "" {
		fields["promoted_by"] = promotedBy
	}
	if baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/"); baseURL != "" {
		fields["jira_url"] = baseURL + "/browse/" + jiraKey
	}

	var add []string
	if !slices.Contains(bead.Labels, "source:jira") {
		add = append(add, "source:jira")
	}
	if lbl := "jira:" + jiraKey; !slices.Contains(bead.Labels, lbl) {
		add = append(add, lbl)
	}

	return darkPromotionStamp{Fields: fields, AddLabels: add}, nil
}

// buildDarkPromotionScript emits the exact operator-side execution_script for a
// jira_promote escalation: it POSTs /rest/api/2/issue with the create-only token
// (advice ka-0000001317), captures the KEY, and self-stamps via `gb jira-dark
// stamp`. Emitting a single vetted script — rather than each agent hand-rolling
// curl — is the whole point of the trickle surface: this is the ONLY place the
// write token is used, so the call must be correct and idempotent by
// construction. `set -euo pipefail` + the promoted_to_jira_key pre-check make a
// replayed run a no-op instead of a second upstream ticket.
//
// The description is embedded via a QUOTED heredoc so arbitrary prose (backticks,
// $, quotes) survives verbatim — never command-substituted.
func buildDarkPromotionScript(beadID, project, summary, issuetype, description string) string {
	if strings.TrimSpace(issuetype) == "" {
		issuetype = "Task"
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("set -euo pipefail\n\n")
	b.WriteString("# jira_promote execution_script — R7 Stage-3 TRICKLE (ka-0000005212).\n")
	b.WriteString("# Runs ONLY on the operator-side create-only executor that holds the\n")
	b.WriteString("# separately-scoped Jira write token. Never runs in a crew pod.\n")
	b.WriteString(": \"${JIRA_BASE_URL:?set JIRA_BASE_URL}\" \"${JIRA_EMAIL:?set JIRA_EMAIL}\" \"${JIRA_API_TOKEN:?set JIRA_API_TOKEN}\"\n\n")
	fmt.Fprintf(&b, "BEAD=%s\n", shellSingleQuote(beadID))
	fmt.Fprintf(&b, "PROJECT=%s\n", shellSingleQuote(project))
	fmt.Fprintf(&b, "SUMMARY=%s\n", shellSingleQuote(summary))
	fmt.Fprintf(&b, "ISSUETYPE=%s\n", shellSingleQuote(issuetype))
	b.WriteString("read -r -d '' DESCRIPTION <<'GBJIRADARKDESC' || true\n")
	b.WriteString(description)
	if !strings.HasSuffix(description, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("GBJIRADARKDESC\n\n")
	b.WriteString("# Idempotency: never mint a second upstream ticket for a promoted bead.\n")
	b.WriteString("existing=$(kd show \"$BEAD\" --json | jq -r '.fields.promoted_to_jira_key // \"\"')\n")
	b.WriteString("if [ -n \"$existing\" ]; then\n")
	b.WriteString("  echo \"already promoted to $existing — nothing to do\"; exit 0\n")
	b.WriteString("fi\n\n")
	b.WriteString("payload=$(jq -n --arg p \"$PROJECT\" --arg s \"$SUMMARY\" --arg t \"$ISSUETYPE\" --arg d \"$DESCRIPTION\" \\\n")
	b.WriteString("  '{fields:{project:{key:$p},summary:$s,description:$d,issuetype:{name:$t}}}')\n")
	b.WriteString("KEY=$(curl -sS -u \"$JIRA_EMAIL:$JIRA_API_TOKEN\" \\\n")
	b.WriteString("  \"$JIRA_BASE_URL/rest/api/2/issue\" \\\n")
	b.WriteString("  -H 'Content-Type: application/json' -d \"$payload\" | jq -r .key)\n")
	b.WriteString("if [ -z \"$KEY\" ] || [ \"$KEY\" = \"null\" ]; then\n")
	b.WriteString("  echo \"jira create failed — no key returned\"; exit 1\n")
	b.WriteString("fi\n\n")
	b.WriteString("# Self-stamp: adopt the dark bead AS the real mirror (idempotent).\n")
	b.WriteString("gb jira-dark stamp \"$BEAD\" \"$KEY\"\n")
	b.WriteString("echo \"promoted $BEAD -> $KEY\"\n")
	return b.String()
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes,
// so it is safe as a literal bash assignment value.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var jiraDarkCmd = &cobra.Command{
	Use:     "jira-dark",
	Short:   "Dark-Jira trickle-up: operator-gated promotion of a dark resolution to a real Jira ticket",
	GroupID: "orchestration",
	Long: `Dark-Jira trickle-up (R7 Stage-3 TRICKLE, ka-0000005212).

Promote a curated dark resolution bead (source:jira-dark) into a REAL upstream
Jira ticket. This is peel-on-demand: low-volume, human-reviewed, one bead at a
time, behind an operator escalation approval gate.

Flow:
  1. gb jira-dark promote-script <dark-bead> --project PE --summary "..."
       → emits the create-only execution_script for the escalation.
  2. gb escalation create --operation-type jira_promote \
       --requesting-task <task> --close-on-execute --script-file <script> ...
       → operator approves; the script runs on the create-only executor.
  3. The script POSTs /rest/api/2/issue, gets KEY, then calls:
       gb jira-dark stamp <dark-bead> <KEY>
     which idempotently adopts the bead as the real mirror.`,
}

var (
	jiraDarkStampBaseURL string
	jiraDarkStampBy      string
)

var jiraDarkStampCmd = &cobra.Command{
	Use:   "stamp <dark-bead-id> <JIRA-KEY>",
	Short: "Idempotently adopt a promoted dark bead as its real Jira mirror",
	Long: `Stamp a dark resolution bead with the real Jira KEY it was promoted to.

The SOLE self-stamp primitive of the trickle-up flow: writes jira_key +
promoted_to_jira_key (+ jira_url) and adds the source:jira and jira:<KEY> labels
so the one-way ingestion poller DEDUPS onto this bead instead of creating a
duplicate mirror. Idempotent: re-running with the same KEY is a no-op; running
with a different KEY (or on a bead already bound to another jira_key) is refused
so a replayed/re-approved escalation can never mint a second upstream ticket.

--base-url defaults to $JIRA_BASE_URL, --by to $BEADS_OPERATED_BY.`,
	Args: cobra.ExactArgs(2),
	RunE: runJiraDarkStamp,
}

func runJiraDarkStamp(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	jiraKey := args[1]
	if daemon == nil {
		return fmt.Errorf("no beads daemon configured")
	}

	baseURL := jiraDarkStampBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("JIRA_BASE_URL")
	}
	promotedBy := jiraDarkStampBy
	if promotedBy == "" {
		promotedBy = os.Getenv("BEADS_OPERATED_BY")
	}

	bead, err := daemon.GetBead(cmd.Context(), beadID)
	if err != nil {
		return fmt.Errorf("reading bead %s: %w", beadID, err)
	}
	if bead == nil {
		return fmt.Errorf("bead %s not found", beadID)
	}

	stamp, err := planPromotionStamp(bead, jiraKey, baseURL, promotedBy)
	if err != nil {
		return err
	}
	if stamp.NoOp {
		fmt.Fprintf(os.Stdout, "already promoted %s -> %s (no-op)\n", beadID, strings.TrimSpace(jiraKey))
		return nil
	}

	if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), beadID, stamp.Fields); err != nil {
		return fmt.Errorf("stamping promotion fields on %s: %w", beadID, err)
	}
	for _, lbl := range stamp.AddLabels {
		if err := daemon.AddLabel(cmd.Context(), beadID, lbl); err != nil {
			return fmt.Errorf("adding label %q to %s: %w", lbl, beadID, err)
		}
	}
	fmt.Fprintf(os.Stdout, "promoted %s -> %s\n", beadID, strings.TrimSpace(jiraKey))
	return nil
}

var (
	jiraDarkPromoteProject   string
	jiraDarkPromoteSummary   string
	jiraDarkPromoteIssueType string
	jiraDarkPromoteDescFile  string
)

var jiraDarkPromoteScriptCmd = &cobra.Command{
	Use:   "promote-script <dark-bead-id>",
	Short: "Emit the create-only execution_script for a jira_promote escalation",
	Long: `Emit the vetted operator-side execution_script that promotes a dark bead to a
real Jira ticket. The script POSTs /rest/api/2/issue with the create-only token
(the ONLY place the write token is used), captures the KEY, and self-stamps via
'gb jira-dark stamp'. It is idempotent (a promoted bead is a no-op).

Feed the output to an escalation:
  gb jira-dark promote-script <bead> --project PE --summary "..." > /tmp/s.sh
  gb escalation create --operation-type jira_promote \
    --requesting-task <task> --close-on-execute --script-file /tmp/s.sh \
    --title "Promote dark resolution <bead> to Jira PE" --context @rationale.md

This command performs an idempotency PRE-CHECK against the live bead and refuses
to emit a script for a bead already promoted (or not a source:jira-dark bead).`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraDarkPromoteScript,
}

func runJiraDarkPromoteScript(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	if jiraDarkPromoteProject == "" {
		return fmt.Errorf("--project is required (upstream Jira project key, e.g. PE)")
	}
	if jiraDarkPromoteSummary == "" {
		return fmt.Errorf("--summary is required")
	}

	description := ""
	if jiraDarkPromoteDescFile != "" {
		data, err := os.ReadFile(jiraDarkPromoteDescFile)
		if err != nil {
			return fmt.Errorf("reading --description-file: %w", err)
		}
		description = string(data)
	}

	// Idempotency + shape pre-check against the live bead so an agent does not
	// draft an escalation for a bead that is already promoted or is not a dark
	// resolution. Best-effort: skip when no daemon is wired.
	if daemon != nil {
		if bead, err := daemon.GetBead(cmd.Context(), beadID); err == nil && bead != nil {
			if err := checkPromotable(bead); err != nil {
				return err
			}
		}
	}

	fmt.Fprint(os.Stdout, buildDarkPromotionScript(beadID, jiraDarkPromoteProject,
		jiraDarkPromoteSummary, jiraDarkPromoteIssueType, description))
	return nil
}

// checkPromotable fails closed if a bead cannot legitimately be promoted: not a
// dark resolution, or already promoted. Mirrors planPromotionStamp's guards
// without needing the eventual KEY.
func checkPromotable(bead *beadsapi.BeadDetail) error {
	if !slices.Contains(bead.Labels, "source:jira-dark") {
		return fmt.Errorf("bead %s is not a dark resolution (missing source:jira-dark label)", bead.ID)
	}
	if prev := strings.TrimSpace(bead.Fields["promoted_to_jira_key"]); prev != "" {
		return fmt.Errorf("bead %s already promoted to %s — nothing to do", bead.ID, prev)
	}
	return nil
}

func init() {
	jiraDarkStampCmd.Flags().StringVar(&jiraDarkStampBaseURL, "base-url", "", "Jira base URL for jira_url (default $JIRA_BASE_URL)")
	jiraDarkStampCmd.Flags().StringVar(&jiraDarkStampBy, "by", "", "Promoting operator identity for promoted_by (default $BEADS_OPERATED_BY)")

	jiraDarkPromoteScriptCmd.Flags().StringVar(&jiraDarkPromoteProject, "project", "", "Upstream Jira project key, e.g. PE (required)")
	jiraDarkPromoteScriptCmd.Flags().StringVar(&jiraDarkPromoteSummary, "summary", "", "Ticket summary (required)")
	jiraDarkPromoteScriptCmd.Flags().StringVar(&jiraDarkPromoteIssueType, "issuetype", "Task", "Jira issue type name")
	jiraDarkPromoteScriptCmd.Flags().StringVar(&jiraDarkPromoteDescFile, "description-file", "", "File whose contents become the ticket description")

	jiraDarkCmd.AddCommand(jiraDarkStampCmd)
	jiraDarkCmd.AddCommand(jiraDarkPromoteScriptCmd)
	rootCmd.AddCommand(jiraDarkCmd)
}
