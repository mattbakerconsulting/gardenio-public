// Package workflowhost holds the re-entrancy logic for a formula step that
// hosts a Claude Code dynamic-workflow run ("workflow-hosting step").
//
// Dynamic-workflow resume — Workflow({scriptPath, resumeFromRunId}) — is
// SAME-SESSION only: the runtime replays the unchanged prefix of agent()
// calls from a journal on local disk. A pod restart (OOM/eviction/redeploy)
// loses both the local journal and the in-memory runId, while the step bead
// stays in_progress. Resuming across a restart therefore requires persisting
// three things on the step bead and restoring the journal off-pod BEFORE
// invoking resume:
//
//   - workflow_run_id      — the runId from the first Workflow({scriptPath})
//     call; the FIRST-CLASS resume handle. Persisted immediately after the
//     run is created, before the first agent() settles.
//   - workflow_script_path — repo-relative path to the orchestration script;
//     persisted at pour/expand time so it survives a crash before the run
//     even starts.
//   - workflow_s3_uri      — the s3://…/<runId>/ prefix where the journal is
//     persisted; used to rehydrate the journal to local disk before resume.
//
// To stop two pods replaying one run (a gardener or stale-claim reaper may
// re-dispatch a long-stale in_progress step — advice ka-0000007726), the step
// also carries a workflow_lease (pod id + heartbeat). A re-entering pod must
// hold the lease before it restores + resumes.
//
// This package is pure logic only — it has no beadsapi or I/O dependency so it
// can be exercised exhaustively in unit tests. The beads read/write and the
// s3-resume pull are wired in cmd/gb/workflow.go on top of these decisions.
//
// Design: gardenio/docs/research/dynamic-workflows-x-formulas-design.md §4.
package workflowhost

import (
	"encoding/json"
	"fmt"
	"time"
)

// Step-bead field names for workflow-hosting re-entrancy state. These are the
// canonical keys; cmd/gb and any other consumer must read/write through these
// constants so the names never drift.
const (
	FieldRunID      = "workflow_run_id"
	FieldScriptPath = "workflow_script_path"
	FieldS3URI      = "workflow_s3_uri"
	FieldLease      = "workflow_lease"
)

// DefaultLeaseTTL is how long a lease stays valid without a heartbeat refresh.
// A lease whose heartbeat is older than this is considered stale and may be
// taken over by a re-entering pod. It is intentionally tighter than the
// gardener >1h reclaim window (advice ka-0000007726): a live host refreshes its
// heartbeat far more often, so a 15-minute gap reliably means the holder is
// gone. The gardener reclaim path should honor a NON-stale lease (i.e. not
// reclaim a step whose lease heartbeat is fresh).
const DefaultLeaseTTL = 15 * time.Minute

// Lease is the anti-double-dispatch lease, stored JSON-encoded in FieldLease.
type Lease struct {
	PodID       string `json:"pod_id"`
	HeartbeatTS string `json:"heartbeat_ts"` // RFC3339, UTC
	RunID       string `json:"run_id,omitempty"`
}

// NewLease builds a lease stamped with now (UTC, RFC3339).
func NewLease(podID, runID string, now time.Time) Lease {
	return Lease{
		PodID:       podID,
		RunID:       runID,
		HeartbeatTS: now.UTC().Format(time.RFC3339),
	}
}

// Heartbeat parses the lease heartbeat timestamp.
func (l Lease) Heartbeat() (time.Time, error) {
	if l.HeartbeatTS == "" {
		return time.Time{}, fmt.Errorf("lease has no heartbeat_ts")
	}
	return time.Parse(time.RFC3339, l.HeartbeatTS)
}

// IsStale reports whether the lease heartbeat is older than ttl relative to
// now. A lease with an unparseable or missing heartbeat is treated as stale —
// a malformed lease must never be able to block a re-entering pod forever.
func (l Lease) IsStale(now time.Time, ttl time.Duration) bool {
	hb, err := l.Heartbeat()
	if err != nil {
		return true
	}
	return now.UTC().Sub(hb.UTC()) > ttl
}

// MarshalLease encodes a lease to the JSON string stored in FieldLease.
func MarshalLease(l Lease) (string, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("marshalling lease: %w", err)
	}
	return string(b), nil
}

// ParseLease decodes a lease from the JSON string stored in FieldLease. An
// empty string — or a tombstone lease with no pod id (see ReleasedLease) —
// yields (nil, nil): no lease is held. The tombstone form exists because the
// beads server rejects clearing a custom field to "", so release writes an
// empty-pod marker instead of deleting the field.
func ParseLease(s string) (*Lease, error) {
	if s == "" {
		return nil, nil
	}
	var l Lease
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return nil, fmt.Errorf("parsing lease %q: %w", s, err)
	}
	if l.PodID == "" {
		return nil, nil // tombstone / released lease — no holder
	}
	return &l, nil
}

// ReleasedLease is the tombstone value written to FieldLease on release. It is
// a non-empty JSON string (the server rejects ""), but ParseLease reads it back
// as "no lease held".
func ReleasedLease() (string, error) {
	return MarshalLease(Lease{})
}

// State is the persisted re-entrancy state read off a step bead.
type State struct {
	RunID      string
	ScriptPath string
	S3URI      string
	Lease      *Lease // nil when no lease is held
}

// ResumeDecision describes what a re-entering host should do for the workflow.
type ResumeDecision struct {
	// Resumable is true when a runId is persisted, so the host can call
	// Workflow({scriptPath, resumeFromRunId}) instead of starting cold.
	Resumable bool
	// NeedJournalRestore is true when an s3 journal prefix is persisted and
	// must be pulled to local disk BEFORE resume. Without the journal, even a
	// correct runId yields a cold start (no prefix to replay).
	NeedJournalRestore bool
	RunID              string
	ScriptPath         string
	S3URI              string
	// Reason is a human-readable explanation, primarily for the cold-start
	// case (the un-resumable create-run→persist-runId window).
	Reason string
}

// DecideResume turns persisted State into a resume decision. The ordering
// guarantee — restore journal before resume — is encoded as NeedJournalRestore
// being meaningful only when Resumable is true; cmd/gb must perform the restore
// first and only then act on the resume.
func DecideResume(s State) ResumeDecision {
	if s.RunID == "" {
		// No runId was ever persisted: either the run never started, or the
		// pod died inside the create-run→persist-runId window. Start cold.
		return ResumeDecision{
			Resumable:  false,
			ScriptPath: s.ScriptPath,
			Reason:     "no workflow_run_id persisted — cold start (run never created, or crash before runId was persisted)",
		}
	}
	return ResumeDecision{
		Resumable:          true,
		NeedJournalRestore: s.S3URI != "",
		RunID:              s.RunID,
		ScriptPath:         s.ScriptPath,
		S3URI:              s.S3URI,
		Reason:             "workflow_run_id present — restore journal (if any) then Workflow({resumeFromRunId})",
	}
}

// LeaseDecision is the outcome of evaluating whether a pod may take the lease.
type LeaseDecision struct {
	// Acquired is true when podID may hold the lease (free, self-owned, or
	// stale takeover).
	Acquired bool
	// Conflict is the live lease held by another pod when Acquired is false.
	Conflict *Lease
	// Reason is a human-readable explanation of the decision.
	Reason string
}

// EvaluateLease decides whether podID may take the lease given the current
// lease (nil when none), the wall-clock now, and the staleness ttl. This is
// the anti-double-dispatch gate: a pod may resume only when it is granted the
// lease.
//
// A pod is granted the lease when:
//   - no lease is held, OR
//   - it already holds the lease (re-entry / heartbeat refresh by same pod), OR
//   - the existing lease is stale (holder is gone — takeover).
//
// It is REFUSED when a different pod holds a non-stale lease.
func EvaluateLease(current *Lease, podID string, now time.Time, ttl time.Duration) LeaseDecision {
	if current == nil {
		return LeaseDecision{Acquired: true, Reason: "no lease held"}
	}
	if current.PodID == podID {
		return LeaseDecision{Acquired: true, Reason: "lease already held by this pod"}
	}
	if current.IsStale(now, ttl) {
		return LeaseDecision{
			Acquired: true,
			Reason:   fmt.Sprintf("existing lease from pod %q is stale (heartbeat %q older than %s) — taking over", current.PodID, current.HeartbeatTS, ttl),
		}
	}
	return LeaseDecision{
		Acquired: false,
		Conflict: current,
		Reason:   fmt.Sprintf("lease held by live pod %q (heartbeat %q) — refusing to double-dispatch", current.PodID, current.HeartbeatTS),
	}
}
