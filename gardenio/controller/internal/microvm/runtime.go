package microvm

import (
	"context"
	"time"
)

// AgentRuntime is the runtime-neutral backend boundary for realizing a
// `type=agent` bead (nfd!15 "Gardenio-owned changes #2"). The controller
// assembles ONE runtime-neutral AgentSpec from the existing project/role/task/
// account/model/secret-selection logic and hands it to the AgentRuntime that
// matches the bead's resolved execution_target:
//
//   - execution_target=kubernetes (default) → the existing Kubernetes pod path
//     (podmanager.Manager). NOT reimplemented behind this interface for C4; the
//     pod path keeps its own richer entry points.
//   - execution_target=lambda_microvm → the *Backend in this package.
//
// A single agent creation event selects exactly one backend and must never
// launch both a pod and a MicroVM. Launch/Status/Terminate are the three
// primitives the spawn door (C4) and the reconciler (C5) drive; the MicroVM
// backend translates the same spec into per-run secrets plus RunMicrovm.
type AgentRuntime interface {
	// Launch realizes the agent as exactly one runtime and returns a handle for
	// later Status/Terminate. It must be idempotent on spec.BeadID: a duplicate
	// spawn event for the same bead returns the already-running handle instead of
	// creating a second runtime (the MicroVM backend uses the bead ID as the AWS
	// client token to enforce this at the service). On a failed launch it must
	// leave no orphaned per-run resources (rollback).
	Launch(ctx context.Context, spec AgentSpec) (RuntimeHandle, error)

	// Status reports the current runtime state for a handle, so the reconciler
	// can treat a MicroVM agent as live-by-its-runtime rather than stale-because-
	// no-pod (C5).
	Status(ctx context.Context, handle RuntimeHandle) (RuntimeStatus, error)

	// Terminate tears the runtime down and cleans up its per-run secrets/identity
	// material. It must be idempotent — terminating an already-gone runtime is a
	// success, not an error — so stop/cancel and leak GC (C5) can call it freely.
	Terminate(ctx context.Context, handle RuntimeHandle, reason string) error
}

// AgentSpec is the runtime-neutral description of one agent to launch. It is
// deliberately backend-agnostic: it carries only the identity, credential, and
// environment inputs both the Kubernetes and MicroVM backends need, assembled
// from the same project/role/task/account/model/secret-selection logic used for
// pods (nfd!15 #2 — "do not hard-code gardenio2, one project, or one static
// secret"). The MicroVM backend renders it into the per-run credential document
// (see runtimeDocument) plus the RunMicrovm call.
type AgentSpec struct {
	// BeadID is the agent bead's canonical ID (e.g. "ka-0000008160"). It is the
	// idempotency key (AWS client token), the per-run secret path segment
	// (ziti-microvm-agents/<BeadID>/…), and the resource tag value. Required.
	BeadID string

	// Project, Role, AgentName, TaskID are the correlation identifiers surfaced
	// to the runtime (and its logs/telemetry) so a MicroVM agent is traceable to
	// its bead exactly like a pod is. Only BeadID is required.
	Project   string
	Role      string
	AgentName string
	TaskID    string

	// Environment is the environment map the MicroVM runtime materializes for the
	// agent process — the runtime-neutral equivalent of the pod's env vars
	// (BEADS_HTTP_URL, COOP_* , account/model selection, Git config, correlation
	// IDs, …). Gardenio owns the source selection for every key; this backend
	// only writes it verbatim into the task-scoped runtime document. Never log
	// its values — it may carry tokens.
	Environment map[string]string

	// NATSToken, ClaudeCredentials, SSHPrivateKey are the optional file-oriented
	// credential fields the MicroVM runtime writes to the UID-1000 filesystem
	// with restrictive modes (nfd!15 "Credential delivery contract"). Each is
	// omitted from the runtime document when empty. NATSToken carries the agent
	// NATS user token when the deployment runs NATS auth (ka-0000006995; empty
	// on no-auth deployments, matching pods); ClaudeCredentials and a Git
	// credential (SSHPrivateKey or an env-based helper) are needed for a real
	// agent. These values must never enter Terraform state, image layers,
	// run-hook payloads, or logs — they live only inside the task-scoped secret.
	NATSToken         string
	ClaudeCredentials string
	SSHPrivateKey     string
}

// RuntimeHandle identifies a launched runtime for later Status/Terminate. For
// the MicroVM backend it carries the AWS microvm id plus the per-run secret
// ARNs so Terminate can both stop the VM and revoke the secrets without
// re-deriving them.
type RuntimeHandle struct {
	// BeadID is the agent bead the runtime realizes — the idempotency key and
	// secret-path segment. Always set.
	BeadID string

	// MicrovmID is the AWS MicroVM identifier returned by RunMicrovm (empty until
	// a VM is created — e.g. a launch that failed after the secret was written but
	// before RunMicrovm succeeded, so Terminate can still revoke the secret).
	MicrovmID string

	// AgentSecretARN is the per-run runtime-credential secret ARN
	// (ziti-microvm-agents/<BeadID>/agent-config). Empty when no per-run secret
	// was created. Terminate deletes it.
	AgentSecretARN string

	// IdentitySecretARN is the Ziti identity secret ARN passed to the MicroVM.
	// For the POC this is the shared static identity (config
	// MicrovmStaticIdentitySecretARN), which is NOT per-run and MUST NOT be
	// deleted on Terminate; a future per-run identity (deferred production work)
	// would be revoked here. IsPerRunIdentity distinguishes the two.
	IdentitySecretARN string

	// IsPerRunIdentity is true only when IdentitySecretARN is a task-scoped
	// identity this launch created (and so Terminate must revoke it). False for
	// the shared static POC identity, which outlives any single run.
	IsPerRunIdentity bool

	// StartedAt is the runtime-start timestamp RunMicrovm reported, when known.
	// C5 persists it as runtime_started_at at launch. Zero when the backend did
	// not report a start time.
	StartedAt time.Time
}

// RuntimeStatus is the backend-neutral state of a launched runtime, mapped from
// the MicroVM's AWS state. C5 persists State/StateReason onto the agent bead and
// drives reconciliation off Live/Terminal.
type RuntimeStatus struct {
	// State is the raw backend state string (for the MicroVM backend, the AWS
	// MicrovmState: PENDING/RUNNING/SUSPENDING/SUSPENDED/TERMINATING/TERMINATED).
	State string

	// StateReason is the backend's human-readable reason for the current state,
	// when provided (e.g. a launch/termination cause). Empty when none.
	StateReason string

	// Live is true while the runtime is doing (or could resume) work —
	// PENDING/RUNNING/SUSPENDING/SUSPENDED. The reconciler treats a Live MicroVM
	// agent as healthy-by-its-runtime rather than stale-because-no-pod.
	Live bool

	// Terminal is true once the runtime is gone or going away
	// (TERMINATING/TERMINATED) or the runtime no longer exists at all.
	Terminal bool

	// StartedAt is the backend's runtime-start timestamp when known (the AWS
	// MicrovmState StartedAt). Zero when the runtime never started or the
	// backend does not report it; C5 persists it as runtime_started_at.
	StartedAt time.Time

	// Exists is false when the runtime backend reports the runtime no longer
	// exists at all (e.g. GetMicrovm ResourceNotFound). Distinct from Terminal,
	// which also covers a still-visible-but-terminating runtime. The reconciler
	// uses Exists=false to know a leaked-bead close is unambiguously safe.
	Exists bool
}
