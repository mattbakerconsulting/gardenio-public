package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/deathreport"
	"gardenio/controller/internal/deathtranscript"

	"github.com/spf13/cobra"
)

// gb session death-report — the SUMMARIZER stage of the auto-forensics pipeline
// (epic ka-0000005447, task ka-0000001381). Given an involuntarily-dead agent's
// session transcript plus its close_reason, it renders a "why did this agent
// die" report: the last N turns, the agent's last words + close_reason quoted,
// a Stop-hook-cadence-vs-cooldown-math diff, and a death-family classification.
//
// The forensic logic is a pure library (internal/deathreport); this command is
// only the I/O shell. It resolves the transcript in priority order:
//
//  1. an explicit transcript arg (local path or s3://… URI), else
//  2. --bead <dead-agent-bead-id> → that bead's OWN S3 recording, mapped via
//     internal/deathtranscript (the keystone auto-forensics wire, ka-0000005669):
//     COOP_S3_SESSION_KEY=<bead-id> means the bead IS the session key, so a
//     forensics agent spawned on a death can pull the DEAD agent's recording
//     without knowing its pod, spawn UUID, or local log, else
//  3. the most recent LOCAL session log (developer convenience).
//
// Step 2 is what makes `gb session death-report --bead <deadBead>` an end-to-end
// forensics command: without it, --bead only pulled close_reason and the report
// fell back to the *invoking* agent's own local log — the wrong recording.
var (
	deathReportLastN       int
	deathReportBead        string
	deathReportCloseReason string
	deathReportBaseCadence int
	deathReportNoS3        bool
)

var sessionDeathReportCmd = &cobra.Command{
	Use:   "death-report [transcript]",
	Short: "Summarize why an involuntarily-dead agent died (last N turns, last words, cadence vs cooldown, death family)",
	Long: `Build a "why did this agent die" forensic report for an involuntarily-dead agent.

Reads a Claude Code session JSONL transcript and renders:

  - the last N turns leading up to the involuntary exit (N=10 by default),
  - the agent's LAST WORDS (final assistant text) and its close_reason, quoted,
  - a link to the FULL transcript (the s3://… recording it was distilled from),
  - a diff of the observed Stop-hook cadence against the stop-gate cooldown math,
    pinpointing the block whose timing the cooldown backoff would force-exit on,
  - a death-FAMILY classification (stop-gate cooldown / escape-hatch /
    rate-limited / context-cancelled / oom-kill / pod-evict / reaper / timeout /
    idle-out / graceful-stop / upstream-api-error / unknown).

TRANSCRIPT RESOLUTION (first that applies):
  1. an explicit [transcript] arg — a local path or an s3://… URI;
  2. --bead <dead-agent-bead-id> — pulls THAT bead's own recording from the coop
     session bucket (COOP_S3_BUCKET), the dead agent bead id being the S3 session
     key. This is the auto-forensics path: a forensics agent spawned on an
     involuntary death names the dead bead and gets its recording. Requires
     COOP_S3_BUCKET; pass --no-s3 to opt out and fall back to (3);
  3. the most recent LOCAL session log.

The close_reason drives the death-family classification. Provide it directly
with --close-reason, or pass --bead <agent-bead-id> to pull it (and the agent
name) from the daemon. When neither is given the report rests on the transcript
alone and classifies as unknown.

The cadence diff is computed against the SAME cooldown math the live stop-gate
uses (deathreport.CooldownWindowSecs, shared with cmd/gb/hook.go), so the window
it shows is byte-for-byte the window that decided the death. Pass --base-cooldown
to reconstruct a pod that ran with a non-default STOP_GATE_COOLDOWN_SECS.

Examples:
  gb session death-report --bead ka-abc123                  # pull ka-abc123's S3 recording + classify
  gb session death-report                                   # most recent local log
  gb session death-report s3://bucket/coop/sessions/ka-X/…  # a specific S3 recording
  gb session death-report --bead ka-abc123 log.jsonl        # local log, classify off the dead bead
  gb session death-report --close-reason 'stop_gate_cooldown_involuntary: …' log.jsonl`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionDeathReport,
}

func init() {
	sessionCmd.AddCommand(sessionDeathReportCmd)
	sessionDeathReportCmd.Flags().IntVarP(&deathReportLastN, "last-n", "n", deathreport.DefaultLastN,
		"number of trailing turns to summarize")
	sessionDeathReportCmd.Flags().StringVar(&deathReportBead, "bead", "",
		"dead agent bead ID — pull its close_reason, name, AND its S3 recording")
	sessionDeathReportCmd.Flags().StringVar(&deathReportCloseReason, "close-reason", "",
		"close_reason string to classify (overrides --bead's close_reason)")
	sessionDeathReportCmd.Flags().IntVar(&deathReportBaseCadence, "base-cooldown", 0,
		"stop-gate base cooldown seconds for the cadence diff (default: the production 30s)")
	sessionDeathReportCmd.Flags().BoolVar(&deathReportNoS3, "no-s3", false,
		"do not auto-pull the --bead's S3 recording; use an explicit transcript or the local log")
}

func runSessionDeathReport(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	opts := deathreport.Options{
		AgentBeadID:      deathReportBead,
		CloseReason:      deathReportCloseReason,
		LastN:            deathReportLastN,
		BaseCooldownSecs: deathReportBaseCadence,
	}

	// Pull close_reason + agent name from the dead bead when a bead is given and
	// the operator did not override close_reason directly. The bead is also the
	// S3 session-key carrier for the auto-pull below, so hold onto it.
	var bead *beadsapi.BeadDetail
	if deathReportBead != "" {
		if b, err := daemon.GetBead(ctx, deathReportBead); err == nil && b != nil {
			bead = b
			if opts.CloseReason == "" {
				opts.CloseReason = b.Fields["close_reason"]
			}
			// Carry the bead's fields so the classifier can correct a generic
			// context_cancelled close_reason with the credential-failure evidence
			// that lives in the fields (ka-0000002985 DEFECT 1).
			opts.Fields = b.Fields
			opts.AgentName = deathReportAgentName(b.Fields, b.Assignee, b.Title)
		} else if err != nil {
			// Non-fatal: the transcript still yields a report, just without the
			// bead-derived classification. Warn so the operator knows why the
			// family may read "unknown".
			fmt.Fprintf(os.Stderr, "warning: could not fetch bead %s: %v\n", deathReportBead, err)
		}
	}

	// Keystone auto-forensics path (ka-0000005669): no explicit transcript arg +
	// a --bead + a configured bucket → pull the DEAD bead's own S3 recording. The
	// dead agent bead id is the coop session key (COOP_S3_SESSION_KEY=<bead-id>),
	// so we map bead → latest transcript snapshot directly from the bucket. This
	// is the branch that makes a forensics agent spawned on a death read the DEAD
	// agent's recording rather than its own local log.
	if len(args) == 0 && !deathReportNoS3 && deathReportBead != "" && deathtranscript.BucketFromEnv() != "" {
		// Resolve the session key off the fetched bead when available, else fall
		// back to the bead id itself — COOP_S3_SESSION_KEY=<bead-id> means the id
		// IS the session key, so a failed daemon fetch (nil bead) does not break
		// the S3 pull.
		keyBead := bead
		if keyBead == nil {
			keyBead = &beadsapi.BeadDetail{ID: deathReportBead}
		}
		data, events, url, err := fetchDeadBeadForensics(ctx, keyBead)
		if err == nil && events != nil {
			// Codex (or any event-stream harness): the report is built from coop's
			// harness-neutral event logs, there is no Claude JSONL to parse.
			opts.TranscriptURL = url
			opts.Events = events
			fmt.Fprintf(os.Stderr, "pulled event log: %s\n", url)
			return emitDeathReport(nil, opts, url)
		}
		switch {
		case errors.Is(err, deathtranscript.ErrNoTranscript):
			// Session died before its first compaction snapshot, or S3 upload was
			// off. Fall through to the close_reason-only / local-log report below.
			fmt.Fprintf(os.Stderr, "warning: no S3 transcript archived for %s; reporting from close_reason\n", deathReportBead)
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: could not pull S3 transcript for %s: %v\n", deathReportBead, err)
		default:
			opts.TranscriptURL = url
			fmt.Fprintf(os.Stderr, "pulled transcript: %s\n", url)
			return emitDeathReport(bytes.NewReader(data), opts, url)
		}
	}

	// Resolve the transcript (explicit local path, s3:// URI, or newest local
	// log). resolveSessionLog handles the S3 download + cleanup.
	logPath, cleanup, err := resolveSessionLog(args)
	if err != nil {
		// No transcript is not fatal when we still have a close_reason to
		// classify — emit a close_reason-only report.
		if opts.CloseReason == "" {
			return fmt.Errorf("no transcript and no close_reason: %w", err)
		}
		fmt.Fprintf(os.Stderr, "warning: no transcript available (%v); reporting from close_reason only\n", err)
		return emitDeathReport(nil, opts, "")
	}
	if cleanup != nil {
		defer cleanup()
	}

	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("opening transcript %s: %w", logPath, err)
	}
	defer f.Close()

	// When the transcript came from an s3:// arg, surface it as the full-recording
	// link; a local path is not a durable link so it is left off the report.
	if len(args) == 1 && isS3URI(args[0]) {
		opts.TranscriptURL = args[0]
	}

	return emitDeathReport(f, opts, logPath)
}

// fetchDeadBeadForensics maps a dead agent bead to its coop S3 session and
// returns what the report is built from: for a Claude agent the RAW bytes of its
// most recent transcript snapshot, for an event-stream harness (Codex) the turns
// projected from coop's harness-neutral hook/state event logs (non-nil events,
// nil data). url is the s3:// location of the object the report links as the
// full recording. The harness comes from the bead's `harness` field, else the
// spawn's meta.json; anything unknown takes the Claude path unchanged.
func fetchDeadBeadForensics(ctx context.Context, bead *beadsapi.BeadDetail) (data []byte, events []deathtranscript.Turn, url string, err error) {
	bucket := deathtranscript.BucketFromEnv()
	if bucket == "" {
		return nil, nil, "", fmt.Errorf("COOP_S3_BUCKET is not set")
	}
	sessionKey := deathtranscript.SessionKeyForBead(bead)
	if sessionKey == "" {
		return nil, nil, "", fmt.Errorf("bead has no resolvable session key")
	}

	client, err := newS3Client(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("building S3 client: %w", err)
	}
	store := deathtranscript.NewS3Store(client, bucket)
	fetcher := deathtranscript.NewFetcher(store, deathtranscript.PrefixFromEnv())

	harness := strings.TrimSpace(bead.Fields["harness"])
	if harness == "" {
		harness = fetcher.SessionHarness(ctx, sessionKey)
	}
	if deathtranscript.IsEventStreamHarness(harness) {
		ev, err := fetcher.FetchLatestEvents(ctx, sessionKey)
		if err != nil {
			return nil, nil, "", err
		}
		turns := deathtranscript.ParseEvents(ev.Hook, ev.State)
		if turns == nil {
			turns = []deathtranscript.Turn{} // non-nil: "event path, nothing recorded"
		}
		return nil, turns, fmt.Sprintf("s3://%s/%s", bucket, ev.Key), nil
	}

	data, _, chunkKey, err := fetcher.FetchLatestRaw(ctx, sessionKey)
	if err != nil {
		return nil, nil, "", err
	}
	return data, nil, fmt.Sprintf("s3://%s/%s", bucket, chunkKey), nil
}

// isS3URI reports whether path is an s3://… URI.
func isS3URI(path string) bool {
	return len(path) >= 5 && path[:5] == "s3://"
}

// emitDeathReport builds the report from the (possibly nil) transcript reader
// and renders it as JSON or a table.
func emitDeathReport(transcript io.Reader, opts deathreport.Options, logPath string) error {
	rep, err := deathreport.Build(transcript, opts)
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	if logPath != "" {
		fmt.Printf("Transcript: %s\n\n", logPath)
	}
	rep.RenderText(os.Stdout)
	return nil
}

// deathReportAgentName resolves a human-facing agent name from a bead's fields,
// preferring the explicit `agent` field, then the assignee, then the title.
func deathReportAgentName(fields map[string]string, assignee, title string) string {
	if fields != nil {
		if n := fields["agent"]; n != "" {
			return n
		}
	}
	if assignee != "" {
		return assignee
	}
	return title
}
