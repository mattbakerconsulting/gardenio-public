// Command gb operator-refusal reports a kbeads write that was refused
// because no human operator could be attributed to the request. The agent
// harness invokes this command after intercepting kd's EX_USAGE-family exit
// code 64 so the refusal is loudly surfaced via Slack (gb squawk) and
// captured durably as a type=bug bead under the agent's spawn lineage.
//
// The contract surface is:
//
//   gb operator-refusal report \
//     --source <header_missing|header_non_human|fallback_agent_only|chain_root_emailless|unknown> \
//     --command "kd comment add ka-XYZ" \
//     [--agent <agent-name>] \
//     [--message "extra context"]
//
// All fields are sanitized — no headers, tokens, or request bodies are
// emitted. The agent name defaults to the BOAT_AGENT env var so a thin
// shell wrapper around kd does not need to thread the agent identity
// itself. See ka-0000006731.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// validRefusalSources is the closed set of resolver-source labels the
// kbeads daemon may emit on a refused write. We accept the catch-all
// "unknown" sentinel as well so unclassified refusals still produce a
// bug bead (with the label preserved verbatim).
var validRefusalSources = map[string]struct{}{
	"header_missing":       {},
	"header_non_human":     {},
	"fallback_agent_only":  {},
	"chain_root_emailless": {},
	"unknown":              {},
}

var (
	refusalReportSource  string
	refusalReportCommand string
	refusalReportAgent   string
	refusalReportMessage string
)

var operatorRefusalCmd = &cobra.Command{
	Use:     "operator-refusal",
	Short:   "Report a kbeads write refused for missing human operator",
	GroupID: "orchestration",
}

var operatorRefusalReportCmd = &cobra.Command{
	Use:   "report",
	Short: "File a bug bead and squawk for a missing-operator refusal",
	Long: `Report a kbeads write that was refused because the daemon could not
attribute a human operator to the request.

This command:
  1. Posts a 'gb squawk' so the operator sees the refusal in Slack
  2. Files a type=bug bead under the agent's spawn lineage with the
     sanitized refusal context (resolver source label, agent name,
     failing kd command summary — NO tokens, headers, or request bodies)
  3. Exits non-zero so the cycle is marked failed

The agent harness invokes this command after intercepting kd's
EX_USAGE-family exit code 64. See ka-0000006731.`,
	RunE: runOperatorRefusalReport,
}

func init() {
	operatorRefusalReportCmd.Flags().StringVar(&refusalReportSource, "source", "", "resolver source label (header_missing|header_non_human|fallback_agent_only|chain_root_emailless|unknown)")
	operatorRefusalReportCmd.Flags().StringVar(&refusalReportCommand, "command", "", "sanitized command summary (e.g. 'kd comment add ka-XYZ')")
	operatorRefusalReportCmd.Flags().StringVar(&refusalReportAgent, "agent", defaultRefusalAgent(), "agent name (defaults to BOAT_AGENT)")
	operatorRefusalReportCmd.Flags().StringVar(&refusalReportMessage, "message", "", "optional extra context (no secrets)")
	_ = operatorRefusalReportCmd.MarkFlagRequired("source")
	_ = operatorRefusalReportCmd.MarkFlagRequired("command")

	operatorRefusalCmd.AddCommand(operatorRefusalReportCmd)
	rootCmd.AddCommand(operatorRefusalCmd)
}

func defaultRefusalAgent() string {
	if s := os.Getenv("BOAT_AGENT"); s != "" {
		return s
	}
	if s := os.Getenv("HOSTNAME"); s != "" {
		return s
	}
	return ""
}

// sanitizeRefusalSource ensures the source label is one of the known refusal
// values; unknown inputs fall back to "unknown" so the bead still gets a
// safe, queryable label. Returns the canonicalized value.
func sanitizeRefusalSource(s string) string {
	s = strings.TrimSpace(s)
	if _, ok := validRefusalSources[s]; ok {
		return s
	}
	return "unknown"
}

// sanitizeRefusalCommand keeps only the kd subcommand path and any positional
// bead IDs; it strips flags and free-form values so a description or comment
// body that happened to slip into argv cannot leak into the bug bead.
// Length-capped at 200 chars to keep the bead title within bead title limits.
func sanitizeRefusalCommand(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "kd <unknown>"
	}
	// Strip flags by truncating at the first "--" or " -" token.
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "-") {
			break
		}
		out = append(out, f)
	}
	cmd := strings.Join(out, " ")
	if cmd == "" {
		cmd = fields[0]
	}
	if len(cmd) > 200 {
		cmd = cmd[:197] + "..."
	}
	return cmd
}

// sanitizeRefusalAgent keeps the agent name as-is but trims whitespace and
// bounds the length. Empty agent is rendered as "(unknown agent)" so the
// bug bead is never silently agent-less.
func sanitizeRefusalAgent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(unknown agent)"
	}
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}

// sanitizeRefusalMessage trims and length-caps an optional extra-context
// message; the harness should pass only non-sensitive context here.
func sanitizeRefusalMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1000 {
		s = s[:997] + "..."
	}
	return s
}

func runOperatorRefusalReport(cmd *cobra.Command, args []string) error {
	source := sanitizeRefusalSource(refusalReportSource)
	command := sanitizeRefusalCommand(refusalReportCommand)
	agent := sanitizeRefusalAgent(refusalReportAgent)
	message := sanitizeRefusalMessage(refusalReportMessage)

	title := fmt.Sprintf("kbeads refused %s: %s", agent, command)
	if len(title) > 500 {
		title = title[:497] + "..."
	}

	descBuilder := strings.Builder{}
	descBuilder.WriteString("kbeads daemon refused a write because no human operator could be attributed.\n\n")
	fmt.Fprintf(&descBuilder, "- resolver_source: %s\n", source)
	fmt.Fprintf(&descBuilder, "- agent: %s\n", agent)
	fmt.Fprintf(&descBuilder, "- command: %s\n", command)
	fmt.Fprintf(&descBuilder, "- at: %s\n", time.Now().UTC().Format(time.RFC3339))
	if message != "" {
		descBuilder.WriteString("\n")
		descBuilder.WriteString("Extra context:\n")
		descBuilder.WriteString(message)
		descBuilder.WriteString("\n")
	}
	descBuilder.WriteString("\nFiled automatically by `gb operator-refusal report` after kd CLI exited 64. ")
	descBuilder.WriteString("See ka-0000006731 for the loud-surface contract.\n")

	fields := map[string]string{
		"resolver_source": source,
		"agent":           agent,
		"command":         command,
		"source_agent":    actor,
	}
	if message != "" {
		fields["message"] = message
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling fields: %w", err)
	}

	labels := []string{
		"operator-refusal",
		fmt.Sprintf("refusal:%s", source),
		fmt.Sprintf("agent:%s", agent),
	}
	if project := defaultProject(); project != "" {
		labels = append(labels, fmt.Sprintf("project:%s", project))
	}

	beadID, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Kind:        "issue",
		Description: descBuilder.String(),
		Labels:      labels,
		Fields:      json.RawMessage(fieldsJSON),
		CreatedBy:   actor,
		Priority:    1,
	})
	if err != nil {
		// We are reporting on a refused write — the daemon may refuse THIS
		// call too if the agent's identity is fundamentally broken. Emit the
		// failure to stderr and continue exiting non-zero so the cycle
		// failure surfaces even when the bug bead could not be filed.
		fmt.Fprintf(os.Stderr, "gb operator-refusal report: filing bug bead failed: %v\n", err)
	} else {
		fmt.Printf("filed bug bead %s for operator refusal (source=%s)\n", beadID, source)
	}

	// Post a squawk so the operator sees the refusal in Slack.
	squawkText := fmt.Sprintf("kbeads refused write from %s: resolver_source=%s, command=%s", agent, source, command)
	if len(squawkText) > 500 {
		squawkText = squawkText[:497] + "..."
	}
	squawkFields := map[string]string{
		"source_agent":     actor,
		"text":             squawkText,
		"resolver_source":  source,
		"refusal_command":  command,
		"refusal_agent":    agent,
		"refusal_bug_bead": beadID,
	}
	squawkFieldsJSON, _ := json.Marshal(squawkFields)
	squawkID, sqErr := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
		Title:     squawkText,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"squawk", "operator-refusal", fmt.Sprintf("from:%s", actor)},
		Fields:    json.RawMessage(squawkFieldsJSON),
		CreatedBy: actor,
	})
	if sqErr != nil {
		fmt.Fprintf(os.Stderr, "gb operator-refusal report: squawk failed: %v\n", sqErr)
	} else if cErr := daemon.CloseBead(cmd.Context(), squawkID, nil); cErr != nil {
		fmt.Fprintf(os.Stderr, "gb operator-refusal report: closing squawk bead failed: %v\n", cErr)
	}

	// Exit non-zero so the cycle is marked failed. cobra returning an
	// error triggers os.Exit(1) in main(). We construct a stable sentinel
	// error so callers can tell this is the refusal-report path.
	return fmt.Errorf("operator refusal reported (source=%s)", source)
}
