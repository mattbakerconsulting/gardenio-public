package main

// workflow.go — `gb workflow`: re-entrancy control for a formula step that
// hosts a Claude Code dynamic-workflow run.
//
// Dynamic-workflow resume is SAME-SESSION only; a pod restart loses the local
// journal and the in-memory runId. These subcommands persist the resume handles
// on the step bead and hold a lease so two pods never replay one run. The
// journal itself is restored to local disk by coop (`coop --resume` ->
// restore_workflow_dir), which the entrypoint runs before `gb workflow
// rehydrate`; rehydrate then only gates on the lease and emits the resume plan.
// See gardenio/docs/research/dynamic-workflows-x-formulas-design.md §4 and the
// pure decision logic in internal/workflowhost.
//
// Typical use from a workflow-hosting pod:
//
//	# at pour/expand time (before the run starts):
//	gb workflow persist <step> --script-path workflows/sweep.js
//	# immediately after Workflow({scriptPath}) returns a runId (runId FIRST):
//	gb workflow persist <step> --run-id wf_abc --s3-uri s3://bucket/wf_abc/
//	# on pod restart, before invoking Workflow({resumeFromRunId}):
//	gb workflow rehydrate <step>

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gardenio/controller/internal/workflowhost"

	"github.com/spf13/cobra"
)

var (
	wfRunID      string
	wfScriptPath string
	wfS3URI      string
	wfPodID      string
	wfLeaseTTL   time.Duration
	wfForce      bool
)

// journalRestore is the hook that makes the persisted workflow journal present
// on local disk before `resumeFromRunId` replays it. It is a package var so
// tests can stub it and assert the resume ordering.
//
// The default is a NO-OP: coop already owns workflow-journal persist+restore
// end-to-end. `restore_workflow_dir` (coop/crates/cli/src/s3.rs) repopulates the
// Claude companion dir (`<config>/projects/<project>/<uuid>/` — the per-run
// `agent-*.jsonl` + run-state) from the session's `workflow/` S3 prefix, and it
// runs on the `coop --resume` path (run.rs), which the entrypoint executes
// BEFORE `gb workflow rehydrate`. So by the time rehydrate emits its resume plan
// the journal is already on disk — no separate pull is needed.
//
// The prior default shelled out to an `s3-resume pull` binary that was never
// built in-repo (Phase-6 design spike ka-0000002347, B1): any real invocation
// with a persisted runId died at `exec: "s3-resume": not found`, turning a
// resumable run into a hard error. The Phase-6 design doc §4.3/§4.4 no longer
// references that binary.
var journalRestore = func(ctx context.Context, s3URI string) error {
	// coop --resume (run.rs -> restore_workflow_dir) already restored the
	// companion dir from the workflow/ S3 prefix before this command ran.
	return nil
}

var workflowCmd = &cobra.Command{
	Use:     "workflow <command>",
	Short:   "Workflow-hosting step re-entrancy (persist / rehydrate / lease)",
	GroupID: "session",
}

var workflowPersistCmd = &cobra.Command{
	Use:   "persist <step-bead-id>",
	Short: "Persist workflow re-entrancy handles on a step bead",
	Long: `Persist one or more of the workflow re-entrancy handles on a step bead.

Set --script-path at pour/expand time (it survives a crash before the run
starts). Persist --run-id IMMEDIATELY after Workflow({scriptPath}) returns —
the runId is the first-class resume handle, so it must be written before the
first agent() settles. Persist --s3-uri once the journal prefix is known.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowPersist,
}

var workflowRehydrateCmd = &cobra.Command{
	Use:   "rehydrate <step-bead-id>",
	Short: "Acquire the lease, restore the journal, and emit a resume plan",
	Long: `Re-entry protocol for a workflow-hosting step after a pod restart.

In order:
  1. Acquire the workflow lease — REFUSE (non-zero exit) if another live pod
     holds it, preventing two pods from replaying one run.
  2. Confirm the journal is present: coop (coop --resume -> restore_workflow_dir)
     already restored it from the workflow/ S3 prefix earlier in the entrypoint,
     so the default is a no-op (without the journal, even a correct runId yields
     a cold start).
  3. Emit a resume plan: the agent then invokes
     Workflow({scriptPath, resumeFromRunId}).

When no runId is persisted, emits a cold-start plan (the un-resumable
create-run -> persist-runId window) while still holding the lease.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowRehydrate,
}

var workflowLeaseCmd = &cobra.Command{
	Use:   "lease <command>",
	Short: "Manage the workflow anti-double-dispatch lease",
}

var workflowLeaseAcquireCmd = &cobra.Command{
	Use:   "acquire <step-bead-id>",
	Short: "Acquire (or refresh) the lease; refuse if a live pod holds it",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkflowLeaseAcquire,
}

var workflowLeaseRefreshCmd = &cobra.Command{
	Use:   "refresh <step-bead-id>",
	Short: "Refresh the lease heartbeat (alias of acquire for the holding pod)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkflowLeaseAcquire,
}

var workflowLeaseReleaseCmd = &cobra.Command{
	Use:   "release <step-bead-id>",
	Short: "Release the lease (only the holder, unless --force)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkflowLeaseRelease,
}

func init() {
	workflowPersistCmd.Flags().StringVar(&wfRunID, "run-id", "", "workflow runId (the first-class resume handle)")
	workflowPersistCmd.Flags().StringVar(&wfScriptPath, "script-path", "", "repo-relative orchestration script path")
	workflowPersistCmd.Flags().StringVar(&wfS3URI, "s3-uri", "", "s3://…/<runId>/ journal prefix")

	workflowRehydrateCmd.Flags().StringVar(&wfPodID, "pod-id", "", "this pod's lease identity (default: POD_NAME/HOSTNAME)")
	workflowRehydrateCmd.Flags().DurationVar(&wfLeaseTTL, "lease-ttl", workflowhost.DefaultLeaseTTL, "lease staleness window")

	workflowLeaseAcquireCmd.Flags().StringVar(&wfPodID, "pod-id", "", "this pod's lease identity (default: POD_NAME/HOSTNAME)")
	workflowLeaseAcquireCmd.Flags().DurationVar(&wfLeaseTTL, "lease-ttl", workflowhost.DefaultLeaseTTL, "lease staleness window")
	workflowLeaseRefreshCmd.Flags().StringVar(&wfPodID, "pod-id", "", "this pod's lease identity (default: POD_NAME/HOSTNAME)")
	workflowLeaseRefreshCmd.Flags().DurationVar(&wfLeaseTTL, "lease-ttl", workflowhost.DefaultLeaseTTL, "lease staleness window")
	workflowLeaseReleaseCmd.Flags().StringVar(&wfPodID, "pod-id", "", "this pod's lease identity (default: POD_NAME/HOSTNAME)")
	workflowLeaseReleaseCmd.Flags().BoolVar(&wfForce, "force", false, "release even if held by another pod")

	workflowLeaseCmd.AddCommand(workflowLeaseAcquireCmd, workflowLeaseRefreshCmd, workflowLeaseReleaseCmd)
	workflowCmd.AddCommand(workflowPersistCmd, workflowRehydrateCmd, workflowLeaseCmd)
}

// resolveWorkflowPodID resolves this pod's lease identity. Precedence:
// --pod-id flag, then POD_NAME, then HOSTNAME, then os.Hostname().
func resolveWorkflowPodID(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	for _, k := range []string{"POD_NAME", "HOSTNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-pod"
}

// readWorkflowState reads the persisted re-entrancy state off a step bead.
func readWorkflowState(ctx context.Context, stepID string) (workflowhost.State, error) {
	bead, err := daemon.GetBead(ctx, stepID)
	if err != nil {
		return workflowhost.State{}, fmt.Errorf("reading step bead %s: %w", stepID, err)
	}
	if bead == nil {
		return workflowhost.State{}, fmt.Errorf("step bead %s not found", stepID)
	}
	lease, err := workflowhost.ParseLease(bead.Fields[workflowhost.FieldLease])
	if err != nil {
		return workflowhost.State{}, err
	}
	return workflowhost.State{
		RunID:      bead.Fields[workflowhost.FieldRunID],
		ScriptPath: bead.Fields[workflowhost.FieldScriptPath],
		S3URI:      bead.Fields[workflowhost.FieldS3URI],
		Lease:      lease,
	}, nil
}

// claimLease writes our lease onto the step bead, then read-back-verifies that
// our write won. With no server-side compare-and-set the read-back only narrows
// the TOCTOU window (a concurrent pod could still interleave) — see design §4.5
// — but it catches the common last-writer-wins loss.
func claimLease(ctx context.Context, stepID, podID, runID string, now time.Time) error {
	lease := workflowhost.NewLease(podID, runID, now)
	ljson, err := workflowhost.MarshalLease(lease)
	if err != nil {
		return err
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, stepID, map[string]any{workflowhost.FieldLease: ljson}); err != nil {
		return fmt.Errorf("writing lease: %w", err)
	}
	if back, err := daemon.GetBead(ctx, stepID); err == nil && back != nil {
		if cur, perr := workflowhost.ParseLease(back.Fields[workflowhost.FieldLease]); perr == nil && cur != nil && cur.PodID != podID {
			return fmt.Errorf("lost lease race to pod %q after write", cur.PodID)
		}
	}
	return nil
}

func runWorkflowPersist(cmd *cobra.Command, args []string) error {
	stepID := args[0]
	fields := map[string]any{}
	if wfRunID != "" {
		fields[workflowhost.FieldRunID] = wfRunID
	}
	if wfScriptPath != "" {
		fields[workflowhost.FieldScriptPath] = wfScriptPath
	}
	if wfS3URI != "" {
		fields[workflowhost.FieldS3URI] = wfS3URI
	}
	if len(fields) == 0 {
		return fmt.Errorf("nothing to persist: set at least one of --run-id / --script-path / --s3-uri")
	}
	if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), stepID, fields); err != nil {
		return fmt.Errorf("persisting workflow fields on %s: %w", stepID, err)
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"step":   stepID,
			"fields": fields,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "persisted on %s:", stepID)
	for k, v := range fields {
		fmt.Fprintf(cmd.OutOrStdout(), " %s=%v", k, v)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

// rehydrateResult is the machine-readable plan emitted by `gb workflow rehydrate`.
type rehydrateResult struct {
	Step            string `json:"step"`
	PodID           string `json:"pod_id"`
	Mode            string `json:"mode"` // "resume" | "cold-start"
	Resumable       bool   `json:"resumable"`
	RunID           string `json:"run_id,omitempty"`
	ScriptPath      string `json:"script_path,omitempty"`
	S3URI           string `json:"s3_uri,omitempty"`
	JournalRestored bool   `json:"journal_restored"`
	Reason          string `json:"reason"`
}

func runWorkflowRehydrate(cmd *cobra.Command, args []string) error {
	stepID := args[0]
	podID := resolveWorkflowPodID(wfPodID)
	ctx := cmd.Context()
	now := time.Now().UTC()

	state, err := readWorkflowState(ctx, stepID)
	if err != nil {
		return err
	}

	// 1. Lease gate FIRST — anti-double-dispatch.
	dec := workflowhost.EvaluateLease(state.Lease, podID, now, wfLeaseTTL)
	if !dec.Acquired {
		return fmt.Errorf("workflow rehydrate refused: %s", dec.Reason)
	}
	// 2. Claim the lease before doing any restore/resume work.
	if err := claimLease(ctx, stepID, podID, state.RunID, now); err != nil {
		return fmt.Errorf("acquiring lease on %s: %w", stepID, err)
	}

	// 3. Decide resume vs cold start.
	rd := workflowhost.DecideResume(state)
	res := rehydrateResult{
		Step:       stepID,
		PodID:      podID,
		Resumable:  rd.Resumable,
		RunID:      rd.RunID,
		ScriptPath: rd.ScriptPath,
		S3URI:      rd.S3URI,
		Reason:     rd.Reason,
	}

	if !rd.Resumable {
		res.Mode = "cold-start"
		return emitRehydrate(cmd, res)
	}

	// 4. Ensure the journal is present before resume (ordering guarantee).
	// The default journalRestore is a no-op — coop --resume already restored
	// the companion dir earlier in the entrypoint; the hook stays so tests can
	// assert the ordering and an operator can override it if ever needed.
	if rd.NeedJournalRestore {
		if err := journalRestore(ctx, rd.S3URI); err != nil {
			return fmt.Errorf("ensuring journal present from %s before resume: %w", rd.S3URI, err)
		}
		res.JournalRestored = true
	}
	res.Mode = "resume"
	return emitRehydrate(cmd, res)
}

func emitRehydrate(cmd *cobra.Command, res rehydrateResult) error {
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}
	if res.Mode == "resume" {
		fmt.Fprintf(cmd.OutOrStdout(),
			"resume %s: journal_restored=%v — invoke Workflow({scriptPath: %q, resumeFromRunId: %q})\n",
			res.Step, res.JournalRestored, res.ScriptPath, res.RunID)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "cold-start %s: %s\n", res.Step, res.Reason)
	}
	return nil
}

func runWorkflowLeaseAcquire(cmd *cobra.Command, args []string) error {
	stepID := args[0]
	podID := resolveWorkflowPodID(wfPodID)
	ctx := cmd.Context()
	now := time.Now().UTC()

	state, err := readWorkflowState(ctx, stepID)
	if err != nil {
		return err
	}
	dec := workflowhost.EvaluateLease(state.Lease, podID, now, wfLeaseTTL)
	if !dec.Acquired {
		return fmt.Errorf("lease not acquired: %s", dec.Reason)
	}
	if err := claimLease(ctx, stepID, podID, state.RunID, now); err != nil {
		return fmt.Errorf("acquiring lease on %s: %w", stepID, err)
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"step": stepID, "pod_id": podID, "acquired": true, "reason": dec.Reason,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "lease acquired on %s by %s (%s)\n", stepID, podID, dec.Reason)
	return nil
}

func runWorkflowLeaseRelease(cmd *cobra.Command, args []string) error {
	stepID := args[0]
	podID := resolveWorkflowPodID(wfPodID)
	ctx := cmd.Context()

	state, err := readWorkflowState(ctx, stepID)
	if err != nil {
		return err
	}
	if state.Lease != nil && state.Lease.PodID != podID && !wfForce {
		return fmt.Errorf("lease held by pod %q, not %q — use --force to override", state.Lease.PodID, podID)
	}
	tombstone, err := workflowhost.ReleasedLease()
	if err != nil {
		return err
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, stepID, map[string]any{workflowhost.FieldLease: tombstone}); err != nil {
		return fmt.Errorf("releasing lease on %s: %w", stepID, err)
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"step": stepID, "pod_id": podID, "released": true,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "lease released on %s by %s\n", stepID, podID)
	return nil
}
