package main

// gb hook — Claude Code agent hook subcommands.
//
// Replaces the shell scripts that implement Claude Code hook behaviour:
//   - check-mail.sh + drain-queue.sh  →  gb hook check-mail
//   - prime.sh                         →  gb hook prime
//   - stop-gate.sh                     →  gb hook stop-gate
//
// The stop-gate command itself, its cooldown/session-debounce logic, and its
// bead-close/wrap-up fallback and live-child-process guard helpers live in
// hook_stopgate.go, hook_beadclose.go, and hook_stopgate_proc.go respectively
// (split out of this file to stay under quench's cloc cap).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"gardenio/clicreds/avareauth"
	"github.com/spf13/cobra"
)

var hookCmd = &cobra.Command{
	Use:     "hook",
	Short:   "Agent hook subcommands (replaces shell hook scripts)",
	GroupID: "orchestration",
	// Reject unknown subcommands without dumping help to stdout. Claude Code
	// hooks pipe a hook command's stdout into the session as a system-reminder,
	// so a stale `.claude/settings.local.json` referencing a non-existent
	// subcommand (e.g. `gb hook hippo-rehydrate`) would otherwise inject the
	// usage banner once per stale entry and waste thousands of tokens. With
	// SilenceUsage + SilenceErrors the failure is mute on stdout — the
	// `2>/dev/null || true` in the hook config swallows the stderr error and
	// non-zero exit.
	SilenceUsage: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return fmt.Errorf("unknown subcommand %q for %q", args[0], cmd.CommandPath())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Bare `gb hook` (no subcommand) — print usage to stderr so it never
		// leaks into a session via stdout-piped hook output.
		fmt.Fprintln(os.Stderr, cmd.UsageString())
		return nil
	},
}

// ── gb hook check-mail ────────────────────────────────────────────────────

// checkMailSeenFile tracks which mail bead IDs have already been injected as an
// inbox reminder, so `gb hook check-mail` (wired on UserPromptSubmit, i.e. every
// operator turn) re-injects ONLY mail that is NEW since the last check rather
// than the full open inbox on every turn (ka-0000007688). Unread-but-already-seen
// mail stays open in the daemon but is not re-announced — the agent still finds
// it via `kd list --type mail`, and each new message is nudged exactly once.
//
// Pod-scoped /tmp EmptyDir, like the stop-gate scratch files (ka-0000007830): it
// survives coop/claude restarts inside gb init's loop but a full pod replacement
// wipes it, so a fresh pod re-announces the open inbox once — the desired
// first-turn behavior. Variable (not const) so tests can override it.
var checkMailSeenFile = "/tmp/check-mail-seen"

var hookCheckMailCmd = &cobra.Command{
	Use:   "check-mail",
	Short: "Inject NEW unread mail as a system-reminder (replaces check-mail.sh)",
	RunE: func(cmd *cobra.Command, args []string) error {
		me := resolveMailActor()
		if me == "" || me == "unknown" {
			return nil
		}

		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"mail", "message"},
			Statuses: []string{"open"},
			Assignee: me,
			Sort:     "-created_at",
			Limit:    20,
		})
		// On a query error leave the seen state untouched — a transient daemon
		// blip must not wipe it and cause a full re-announce next turn.
		if err != nil || result == nil {
			return nil
		}

		// De-dup against what we already injected this pod-session: emit only
		// mail whose ID we have not announced before. Persist the CURRENT open
		// set (not the cumulative union) so the seen file stays bounded to the
		// query window and naturally forgets IDs of mail that got closed —
		// including the empty-inbox case, which records an empty set.
		seen := loadSeenMailIDs()
		var fresh []*beadsapi.BeadDetail
		currentIDs := make([]string, 0, len(result.Beads))
		for _, b := range result.Beads {
			if b == nil {
				continue
			}
			currentIDs = append(currentIDs, b.ID)
			if !seen[b.ID] {
				fresh = append(fresh, b)
			}
		}
		// Record the current open set regardless of whether anything is new, so
		// a message that stays open is never re-announced on a later turn.
		saveSeenMailIDs(currentIDs)

		if len(fresh) == 0 {
			return nil
		}

		var sb strings.Builder
		sb.WriteString("## Inbox\n\n")
		for _, b := range fresh {
			sender := senderFromLabels(b.Labels)
			sb.WriteString(fmt.Sprintf("- %s | %s | %s\n", b.ID, b.Title, sender))
		}
		fmt.Printf("<system-reminder>\n%s</system-reminder>\n", sb.String())
		return nil
	},
}

// loadSeenMailIDs reads the set of already-announced mail bead IDs from
// checkMailSeenFile. A missing/unreadable file yields an empty set, so the
// caller safely announces the full open inbox once (the first-turn / fresh-pod
// behavior).
func loadSeenMailIDs() map[string]bool {
	seen := map[string]bool{}
	data, err := os.ReadFile(checkMailSeenFile)
	if err != nil {
		return seen
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			seen[id] = true
		}
	}
	return seen
}

// saveSeenMailIDs writes the given mail bead IDs (the current open set) to
// checkMailSeenFile, one per line. Best-effort: a write failure only means the
// next turn re-announces, so errors are logged to stderr and swallowed rather
// than blocking the hook.
func saveSeenMailIDs(ids []string) {
	var body string
	if len(ids) > 0 {
		body = strings.Join(ids, "\n") + "\n"
	}
	if err := os.WriteFile(checkMailSeenFile, []byte(body), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gb hook check-mail: failed to persist seen mail IDs: %v\n", err)
	}
}

// ── gb hook prime ─────────────────────────────────────────────────────────

// injectQueueFile is the path to the JSONL inject queue drained by PostToolUse.
// Variable so tests can override.
var injectQueueFile = "/tmp/inject-queue.jsonl"

var primeEnqueue bool

var hookPrimeCmd = &cobra.Command{
	Use:   "prime",
	Short: "Output workflow context as system-reminder (replaces prime.sh)",
	Long: `Outputs agent workflow context wrapped in a system-reminder tag.

With --enqueue, also writes the prime output to the inject queue
(/tmp/inject-queue.jsonl) for post-compaction recovery. The PostToolUse
hook (drain-queue.sh) drains the queue on the next tool use, re-injecting
fresh context AFTER compaction completes. This avoids generating prime
output twice.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID := resolvePrimeAgentFromEnv(actor)

		// Capture prime output to a buffer so we can route it to exactly one
		// channel without calling outputPrimeForHook twice.
		var buf strings.Builder
		outputPrimeForHook(&buf, agentID)
		primeContent := buf.String()

		// Route the prime body to a SINGLE channel to avoid injecting the same
		// 2–15 KB twice around every compaction (ka-0000007688):
		//
		//   - PreCompact (--enqueue): enqueue ONLY. drain-queue.sh re-emits the
		//     queued content on the next PostToolUse — AFTER compaction
		//     completes — which is the whole point of the queue. Also printing
		//     it to stdout here injected it into the pre-compaction context that
		//     is about to be discarded, so the agent saw the identical block
		//     twice per compaction (once pre-, once post-). Enqueue-only kills
		//     that duplication.
		//   - SessionStart (no --enqueue): print to stdout for immediate use;
		//     there is no queue drain in that path.
		//
		// Enqueue is best-effort: on a queue-write failure fall back to stdout
		// so the content is not lost entirely across the compaction.
		if primeEnqueue {
			if primeContent != "" {
				if err := enqueuePrimeContent(primeContent); err != nil {
					fmt.Fprintf(os.Stderr, "gb hook prime: enqueue failed, falling back to stdout: %v\n", err)
					fmt.Print(primeContent)
				}
			}
		} else {
			fmt.Print(primeContent)
		}

		// Show this agent's assignment bead (BOAT_AGENT_BEAD_ID set by controller).
		beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
		if beadID == "" {
			beadID, _ = resolveAgentID("")
		}
		if beadID != "" {
			// Oversized field values (notably `prompt:`, which IS the whole
			// agent prompt already delivered as the spawn prompt) are elided —
			// see hook_assignment.go. Printed to stdout only; primeContent
			// above is what gets enqueued for post-compaction recovery, so this
			// block never rode that path.
			if body := renderAssignmentBlock(cmd.Context(), beadID); body != "" {
				fmt.Printf("<system-reminder>\n## Assignment\n\n%s</system-reminder>\n", body)
			}
		}
		// Warn if agent has no claimed work and no open decision.
		outputClaimReminder(cmd.Context(), actor)
		return nil
	},
}

// enqueuePrimeContent appends prime content to the inject queue as a JSONL entry.
func enqueuePrimeContent(content string) error {
	entry := map[string]string{"content": content}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshalling inject queue entry: %w", err)
	}
	data = append(data, '\n')

	f, err := os.OpenFile(injectQueueFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening inject queue: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing inject queue: %w", err)
	}
	return nil
}

// ── gb hook load-state ────────────────────────────────────────────────────

var hookLoadStateCmd = &cobra.Command{
	Use:   "load-state",
	Short: "Inject saved learned state as system-reminder (called by PreCompact hook)",
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := readState()
		if err != nil {
			fmt.Fprintf(os.Stderr, "gb hook load-state: %v\n", err)
			return nil
		}
		if len(state) == 0 {
			return nil
		}

		fmt.Printf("<system-reminder>\n%s</system-reminder>\n", formatStateForInjection(state))
		return nil
	},
}

// ── gb hook workspace-check ───────────────────────────────────────────────

var hookWorkspaceCheckCmd = &cobra.Command{
	Use:   "workspace-check",
	Short: "Warn if working directory is not inside a per-bead worktree",
	Long: `Checks whether the agent's current working directory is inside a
per-bead worktree (under ../worktrees/<bead-id>/, a sibling of the primary
checkout). If not, emits a system-reminder so the agent knows to run
'gb workspace setup <bead-id>'.

Exits 0 always — non-blocking by design.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, _ := os.Getwd()
		// Check if cwd is inside a per-bead worktree. Worktrees now live at
		// the sibling location <project>/worktrees/<bead-id> (bug
		// ka-0000007049); still accept the legacy nested .beads/worktrees/
		// path for one release so in-flight pods on the old layout are not
		// wrongly warned.
		slashCwd := filepath.ToSlash(cwd)
		if strings.Contains(slashCwd, "/worktrees/") || strings.Contains(slashCwd, "/.beads/worktrees/") {
			return nil
		}
		// Not in a worktree — emit a reminder.
		fmt.Printf("<system-reminder>[workspace] Working directory is not a per-bead worktree.\n" +
			"Run 'gb workspace setup <bead-id>' to create an isolated worktree before making changes.</system-reminder>\n")
		return nil
	},
}

func init() {
	hookCmd.AddCommand(hookCheckMailCmd)
	hookCmd.AddCommand(hookPrimeCmd)
	hookPrimeCmd.Flags().BoolVar(&primeEnqueue, "enqueue", false,
		"also enqueue prime output to inject queue for post-compaction recovery")
	hookCmd.AddCommand(hookStopGateCmd)
	hookCmd.AddCommand(hookLoadStateCmd)
	hookCmd.AddCommand(hookWorkspaceCheckCmd)
}

// emitHookWithRetry calls daemon.EmitHook with increasing backoff on transient
// errors. Returns an error only after all retries are exhausted.
//
// A land door serves no hook route (gd-2iqSr9ANOH), so its 404 is not
// transient, and the decision gate means nothing to a tenant's laptop agent
// (gd-jHjROHhaqz): there it sends GET /v1/me, which the door records as the
// agent's presence, and allows.
func emitHookWithRetry(ctx context.Context, req beadsapi.EmitHookRequest) (*beadsapi.EmitHookResponse, error) {
	if landDoor() != "" {
		_, _ = daemon.Me(ctx)
		return &beadsapi.EmitHookResponse{}, nil
	}
	backoffs := []time.Duration{5 * time.Second, 30 * time.Second, 1 * time.Minute, 5 * time.Minute}
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		resp, err := daemon.EmitHook(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// A re-auth bounce (expired AVA cookie → 3xx to the Cloudflare-walled
		// IdP) is deterministic, not transient — retrying just hammers the wall
		// with 5 attempts of Cloudflare HTML. Bail immediately so the caller can
		// fail open with a clean "run gb login" line.
		if errors.Is(err, avareauth.ErrReauthRequired) {
			return nil, err
		}
		if attempt < len(backoffs) {
			fmt.Fprintf(os.Stderr, "gb hook: EmitHook failed (attempt %d/%d), retrying in %s: %v\n",
				attempt+1, len(backoffs)+1, backoffs[attempt], err)
			select {
			case <-time.After(backoffs[attempt]):
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			}
		}
	}
	return nil, lastErr
}

// outputClaimReminder checks if the agent has any in-progress claimed work or
// open decisions. If not, emits a system-reminder nudging them to claim a bead
// before starting work.
func outputClaimReminder(ctx context.Context, agentName string) {
	if agentName == "" {
		return
	}

	// Check for any in-progress work claimed by this agent.
	task, err := daemon.ListAssignedTask(ctx, agentName)
	if err == nil && task != nil {
		return // Agent already has claimed work — no reminder needed.
	}

	// Check for any open decisions this agent is waiting on.
	decisions, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Assignee: agentName,
		Limit:    1,
	})
	if err == nil && len(decisions.Beads) > 0 {
		return // Agent has a pending decision — already engaged.
	}

	fmt.Printf("<system-reminder>\nNo claimed work found. Run `gb ready` to see available beads, then `kd claim <id>` to claim one before starting work.\n</system-reminder>\n")
}
