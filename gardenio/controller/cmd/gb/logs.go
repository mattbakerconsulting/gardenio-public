// Command `gb logs <component>` — the agent-reachable service-log surface
// (design ka-0000005579 option A, task ka-0000005209).
//
// # What it is
//
// A bounded, allowlisted, read-only LogsQL query over the already-live in-chart
// VictoriaLogs store (gardenio-victoria-logs:9428 /select/logsql/query). It is
// the READ counterpart to the pod-log shippers, which STREAM gardenio-pod slog
// JSON into that same store. All the query governance lives in the reusable
// internal/vlogs client (component allowlist, mandatory bounded --since, result
// cap, grep→exact-phrase translation); this file is the thin CLI adapter.
//
// # Why it preserves the SA lockdown
//
// The gardenio-agent SA is deliberately locked out of the k8s pod-log API
// (ka-0000001752): `kubectl auth can-i get pods/log` = no, and this verb does
// NOT change that. It reads the log STORE over HTTP — never Kubernetes — so no
// pod-log RBAC is granted to the agent. This mirrors the sanctioned "agent
// reaches an in-cluster observability service directly by injected env URL"
// pattern already used by `gb grafana` (GRAFANA_URL) and the vmagent metric push
// (VMAGENT_URL).
//
// # Audit
//
// Every invocation emits a structured audit line to stderr (actor / operated_by
// / component / window / grep / query / result_count). Because gb runs inside
// the agent's coop session, that line is captured in the session transcript that
// `gb audit` surfaces from S3 — the who/what/when record design §5 requires.
//
// # Endpoint resolution
//
// The VictoriaLogs base URL is release-relative (prod: gardenio-victoria-logs;
// e2e namespaces differ), so the chart injects it as
// GARDENIO_LOGS_URL into agent pods. Resolution order: --url flag →
// GARDENIO_LOGS_URL → GARDENIO_VICTORIALOGS_URL → the well-known prod Service name.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/jqout"
	"gardenio/controller/internal/vlogs"

	"github.com/spf13/cobra"
)

// defaultLogsEndpoint is the last-resort VictoriaLogs base URL when no env var
// or flag is set — the prod release Service name. In-cluster the chart injects
// GARDENIO_LOGS_URL (release-relative), which takes precedence.
const defaultLogsEndpoint = "http://gardenio-victoria-logs:9428"

var logsCmd = &cobra.Command{
	Use:     "logs <component>",
	Short:   "Read bounded, redacted service logs for a gardenio component",
	GroupID: "orchestration",
	Long: `Query a gardenio component's service logs from the in-chart VictoriaLogs store.

This is a bounded, read-only LogsQL query — NOT the Kubernetes pod-log API. The
gardenio-agent ServiceAccount stays locked out of pods/log (see ka-0000001752);
this verb reads the log STORE over HTTP, so no pod-log RBAC is granted.

A component is only queryable if a shipper is streaming its slog JSON to
VictoriaLogs (off by default per component). Queryable components:

  ` + strings.Join(vlogs.Components(), ", ") + `

The query is bounded server-side: --since is clamped to [1m, 24h], --limit is
capped, and --grep is an exact-phrase filter (never raw LogsQL). Every call is
audited to the session transcript (surfaced by ` + "`gb audit`" + `).

  gb logs controller --since 15m
  gb logs slack-bridge --since 1h --grep "decision resolved"
  gb logs trigger-engine --since 30m --limit 50 --json`,
	Args: cobra.ExactArgs(1),
	// No kbeads round-trip: this verb reads VictoriaLogs directly, like
	// `gb grafana`/`gb whoami`. Override the root PersistentPreRunE (which dials
	// the beads daemon) but keep the --jq compile contract so `gb logs … --jq`
	// works through the shared printJSON choke-point.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		if jqFilter != "" {
			jsonOutput = true
			if compiledJQ == nil {
				f, err := jqout.Parse(jqFilter)
				if err != nil {
					return err
				}
				compiledJQ = f
			}
		}
		return nil
	},
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().Duration("since", vlogs.DefaultSince, "look-back window (clamped to [1m, 24h])")
	logsCmd.Flags().String("grep", "", "keep only lines containing this exact phrase")
	logsCmd.Flags().Int("limit", vlogs.DefaultLimit, "max lines to return (capped server-side)")
	logsCmd.Flags().String("url", "", "VictoriaLogs base URL override (default: $GARDENIO_LOGS_URL, else the in-cluster Service)")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	component := args[0]
	if !vlogs.IsAllowed(component) {
		return fmt.Errorf("component %q is not queryable; allowed: %s",
			component, strings.Join(vlogs.Components(), ", "))
	}

	since, _ := cmd.Flags().GetDuration("since")
	grep, _ := cmd.Flags().GetString("grep")
	limit, _ := cmd.Flags().GetInt("limit")
	urlFlag, _ := cmd.Flags().GetString("url")

	endpoint := resolveLogsEndpoint(urlFlag)
	opts := vlogs.QueryOptions{Component: component, Since: since, Grep: grep, Limit: limit}

	// Build the exact LogsQL up front so it can be both audited and (on failure)
	// surfaced — it names precisely what was read.
	query, err := vlogs.BuildLogsQL(opts)
	if err != nil {
		return err
	}

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 35*time.Second)
	defer cancel()

	client := vlogs.New(endpoint, nil)
	lines, qErr := client.Query(ctx, opts)

	// Audit EVERY call (success or failure) to the session transcript. Emitted to
	// stderr as structured slog JSON so it never pollutes stdout / --json output.
	auditLog := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	auditLog.Info("gb-logs query",
		"actor", actor,
		"operated_by", operatedBy,
		"component", component,
		"since", since.String(),
		"grep", grep,
		"limit", limit,
		"query", query,
		"result_count", len(lines),
		"err", logsErrString(qErr),
	)
	if qErr != nil {
		return qErr
	}

	if jsonOutput {
		records := make([]map[string]any, 0, len(lines))
		for _, l := range lines {
			records = append(records, l.Raw)
		}
		printJSON(records)
		return nil
	}

	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "no %s log lines in the last %s%s\n",
			component, since.String(), grepSuffix(grep))
		return nil
	}
	for _, l := range lines {
		fmt.Println(formatLine(l))
	}
	return nil
}

// resolveLogsEndpoint picks the VictoriaLogs base URL: --url flag, then the
// chart-injected GARDENIO_LOGS_URL, then GARDENIO_VICTORIALOGS_URL, then the
// well-known prod Service name.
func resolveLogsEndpoint(urlFlag string) string {
	if urlFlag != "" {
		return urlFlag
	}
	for _, k := range []string{"GARDENIO_LOGS_URL", "GARDENIO_VICTORIALOGS_URL"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return defaultLogsEndpoint
}

// formatLine renders one record as a human-readable line: time, level, source,
// then the message. Missing fields are simply omitted.
func formatLine(l vlogs.Line) string {
	var parts []string
	if l.Time != "" {
		parts = append(parts, l.Time)
	}
	if lvl, ok := l.Raw["level"].(string); ok && lvl != "" {
		parts = append(parts, lvl)
	}
	if l.Source != "" {
		parts = append(parts, "["+l.Source+"]")
	}
	if l.Msg != "" {
		parts = append(parts, l.Msg)
	}
	if len(parts) == 0 {
		// No stamped fields — fall back to the raw record so nothing is lost.
		b, _ := json.Marshal(l.Raw)
		return string(b)
	}
	return strings.Join(parts, " ")
}

func grepSuffix(grep string) string {
	if grep == "" {
		return ""
	}
	return fmt.Sprintf(" matching %q", grep)
}

func logsErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
