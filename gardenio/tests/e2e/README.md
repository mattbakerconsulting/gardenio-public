# gardenio E2E Tests

Integration tests for the gardenio/kbeads stack.

## Prerequisites

- `kubectl` context pointing at the target cluster
- `gb` binary from `~/gardenio/controller` (orchestration: bus emit, decision, gate)
- `kd` binary from `~/kbeads` (CRUD: create, close, list, show)
- `jq` and `python3` installed
- A gardenio namespace deployed (default `gardenio-e2e`; CI leases `gardenio-e2e-0`/`-1` via `scripts/lane.sh`)

## gVisor Freeze/Thaw Tests (`test-gvisor-freeze-thaw.sh`) — opt-in

Automated runsc checkpoint/restore (freeze/thaw) state-fidelity test for the
Phase 2 gVisor PoC (bead `ka-0000001895`, epic `ka-0000002167`). Reuses the
freeze/thaw mechanism from the landed PoC harness (MR !3464; design records in
[`gardenio/docs/gvisor-runsc-cr/`](../../docs/gvisor-runsc-cr/README.md), node scripts in
[`scripts/gvisor-node/`](scripts/gvisor-node/)) and the runsc `release-20260525.0` pattern.

**Opt-in / gated.** This module targets the dedicated `gvisor-poc` namespace and
node pool — NOT the per-run e2e namespace the rest of the suite
runs against. The standard e2e cluster's default pool has no gVisor, so the test
**self-SKIPs (exit 0)** unless `GVISOR_E2E=1` is set AND the substrate is present
(runsc RuntimeClass + create-pods in ns `gvisor-poc`). It is therefore inert in
the normal `gardenio-e2e` shard fan-out and runs only via an explicit
`GVISOR_E2E=1 ONLY=gvisor-freeze-thaw` invocation; no CI job sets `GVISOR_E2E`.

### Quick run (against a gVisor-capable cluster):

```bash
GVISOR_E2E=1 ./tests/e2e/scripts/test-gvisor-freeze-thaw.sh
# keep the probe pod for debugging:
GVISOR_E2E=1 KEEP_PROBE=1 ./tests/e2e/scripts/test-gvisor-freeze-thaw.sh
```

### What it proves:

1. A live agent container (`runtimeClassName=runsc`) is seeded with a verifiable
   state token — a **file** token (`/work/state.bin`) and an **in-memory** token
   (a monotonic counter held in the agent's RAM).
2. `runsc checkpoint` freezes it (images written, container stopped).
3. `runsc restore` thaws it sub-second, the file token survives intact, and the
   agent **RESUMES** — the in-RAM counter climbs *past* its frozen value, proving
   the restored process is the same live process (RAM preserved), not a cold
   restart from zero.
4. **Failure path:** restoring a runsc checkpoint under a non-runsc runtime (the
   node's `runc`) fails with a clear non-zero error and starts no container — a
   corrupt restore surfaces loudly, never silently.

### Mechanism notes:

- Schedules a privileged hostPath+hostPID probe onto the gvisor node and drives
  `runsc run|checkpoint|restore` via `nsenter -t 1 -m -p` into the host init
  namespaces (the gofer fork + netns open require host init context).
- The agent runs as a **harness-owned** runsc container (own bundle/root +
  host bind-mount for `/work`), not a kubelet-managed pod — so the restore is
  deterministic (the PoC's kubelet-managed pod has its emptyDir GC'd on
  out-of-band sandbox stop; PoC report §5).
- runsc `checkpoint`/`restore` complete the operation but **hang on process
  exit** (release-20260525.0); every call is wrapped in `timeout` and success is
  asserted from observed state, not command rc. This also keeps every
  `kubectl exec` short — long-lived exec streams are what wedged an earlier crew.

## gVisor Freeze → Resume-into-Pool ACCEPTANCE GATE (`test-gvisor-resume-into-pool.sh`) — opt-in

The **epic acceptance gate** (bead `ka-0000003295`, epic `ka-0000007532` "gVisor
freeze→thaw RESUME into a prewarmed pool"). The one end-to-end proof the whole
resume-into-pool epic converges on: **freeze an active agent → resume its snapshot
INTO a CLAIMED prewarmed POOL member (not a fresh on-demand Pod B) → assert /work +
RAM + gb/coop rehydrate-ack all hold on that one resumed agent.**

**Opt-in / gated** exactly like the sibling freeze-thaw module: self-SKIPs (exit 0)
unless `GVISOR_E2E=1` (or `RESUME_INTO_POOL_E2E=1`) AND the `gvisor-poc` substrate
is present; SKIPs LOUD (never silently passes) when the flag is set but the
substrate is absent, and hard-fails under `REQUIRE_SUBSTRATE=1`. Runs only via
`GVISOR_E2E=1 ONLY=gvisor-resume-into-pool`. Needs a Go toolchain to build
`cmd/thawepoch-probe`.

### What it proves — for BOTH variants (the task's explicit deliverable):

- **claudeless** — a minimal agent with `WORKING_SET_ENTRIES=0` (no loaded heap):
  the floor case, fastest signal.
- **active-Claude** — a multi-MB loaded heap (`WORKING_SET_ENTRIES=2000`) standing
  in for a live Claude working set, plus an open append-log FD + UDP socket: the
  case the epic's HARD GATE cares about.

Each variant drives a full ladder against the real substrate:

1. **FREEZE** — `runsc checkpoint` an active single-container agent seeded with the
   variant's working set and a `/work/state.bin` file token.
2. **POOL MEMBER + CLAIM** — mint a Ready runsc placeholder in the
   warm-placeholder shape (`podmanager.WarmPlaceholderWorkPath`, the passive sleep
   body) with pool-standby labels, then CLAIM it (stamp the resuming agent, flip
   `gardenio.io/pool-standby` true→false) — a resume must consume a standby, not run
   on an idle member.
3. **RESUME** — `runsc restore` the checkpoint INTO the claimed member's
   kubelet-owned netns (graft-on-thaw).
4. **ASSERT** — (1) `/work` intact (file token byte-identical), (2) RAM intact
   (in-RAM counter climbs PAST the freeze value; active-Claude also asserts the
   loaded heap `repo_loaded == working_set`), (3) gb+coop rehydrate-ack RELEASE
   within `THAW_REHYDRATE_DEADLINE` via the REAL `thawepoch.WaitForAcks` gate
   (`cmd/thawepoch-probe`, keyed to the claimed member's pod-uid) with the armed +
   ack + marker files present on the substrate FS, (4) a fail-close negative
   control (coop silent → gate HOLDS), and (5) the member stays Running across the
   gate window and the pool is observably consumed.

It sits ABOVE its three building-block siblings —
`test-gvisor-freeze-thaw.sh` (state fidelity), `test-resume-latency.sh` (warm-B
restore), `test-gvisor-rehydrate-ack.sh` (ack RELEASE) — asserting the converged
path none of them cover.

### Quick run (against a gVisor-capable cluster):

```bash
GVISOR_E2E=1 ./tests/e2e/scripts/test-gvisor-resume-into-pool.sh
# one variant + keep pods for debug:
GVISOR_E2E=1 ONLY_VARIANT=claudeless KEEP_PODS=1 ./tests/e2e/scripts/test-gvisor-resume-into-pool.sh
```

## gVisor Boot-Wedge Armed-Guard Tests (`test-gvisor-armed-guard.sh`) — opt-in

Boot-wedge armed-watcher liveness guard / freeze fail-safe (bead `ka-0000000584`).
Asserts the **end-to-end fail-safe path** the P0 boot-wedge fix (`ka-0000006667` —
controller `watchersArmed` + `freezethaw_armed_guard_test.go`) guarantees on a
real runsc sandbox. The unit layer asserts the freeze *decision* over a fake
exec; this module asserts the *invariant* on the substrate: an un-armed sandbox
the guard refuses to freeze is **left running** (an un-thawable snapshot — one
with no live rehydration watcher inside it — is never minted), while the
both-armed positive control proceeds to a real checkpoint that persists a
snapshot.

The guard's decision reproduces the controller's exact on-disk contract on the
node — the `thaw-epoch.<consumer>.armed` sentinel filenames (`thawepoch.ArmedName`)
and the `if [ -f <path> ]; then echo FT_ARMED=<consumer>; fi` probe shape
(`watchersArmed`) — the same reimplement-the-shared-protocol-in-bash pattern the
sibling `test-gvisor-rehydration.sh` uses for the marker/ack contract.

**Opt-in / gated** exactly like the freeze-thaw module: self-SKIPs (exit 0)
unless `GVISOR_E2E=1` AND the `gvisor-poc` substrate is present. Runs via
`GVISOR_E2E=1 ONLY=gvisor-armed-guard`.

```bash
GVISOR_E2E=1 ./tests/e2e/scripts/test-gvisor-armed-guard.sh
```

The three legs (refuse-legs run first — they leave the sandbox live; the
positive control runs last because a real checkpoint stops the container):

1. **Fail-safe / boot-wedge:** arm only `coop` (omit `gb`) → guard refuses; assert
   no checkpoint image was minted and the sandbox is still running with its
   in-RAM counter climbing past the pre-gate value.
2. **None-armed:** clear both sentinels → guard refuses again; sandbox still live.
3. **Positive control:** arm both `coop` + `gb` → guard proceeds; `runsc checkpoint`
   mints checkpoint images and the container stops (a real, thawable snapshot).

## gVisor Duplicate / Concurrent-Spawn DEDUP (`test-gvisor-duplicate-concurrent-spawn.sh`)

E2E acceptance for the **DEDUP** decision of the re-nudge mid-THAW design
(`gardenio/docs/renudge-mid-thaw-coordination.md` §DEDUP; decision child
`ka-0000005125`; single-flight impl child `ka-0000005830`; bead `ka-0000007641`,
parent epic `ka-0000004667`). Proves the wake of a frozen agent is **idempotent**:
a 2nd spawn/wake racing the 1st must produce **exactly one `runsc restore` and one
live Pod B** — never the `ka-0000007549` double-Pod-B class.

Two legs, mirroring `test-thaw-reaper-exemption.sh`:

1. **CONTRACT** (always runs — needs only bash): a faithful model of the full
   DEDUP guard chain the design confirms + strengthens, asserted over the truth
   table with negative controls so a future refactor that drops or over-widens a
   guard fails here in every shard:
   - single-threaded controller event loop serializes spawn events;
   - reconciler create-missing exempts a frozen-awaiting-thaw agent (no duplicate
     cold-start);
   - reconciler terminal-pod reaper exempts the in-flight Pod B (`IsThawing`);
   - bridge respawn single-flight collapses concurrent thread replies to one
     spawn event;
   - the manager per-agent single-flight (design change #2): N concurrent `Thaw`
     calls **join** the first → one restore. This last assertion is the
     acceptance signal for `ka-0000005830` — the model captures both the post-fix
     invariant (1 restore) and the pre-fix hazard it closes (N restores).
2. **SUBSTRATE** (opt-in, `GVISOR_E2E=1`): on the `gvisor-poc` node, fire two
   concurrent Pod-B creates under the deterministic `<canonical>-thaw-b` name (the
   shape two racing thaws produce) and assert exactly **one** Pod B object exists
   and **no** duplicate cold-start under the canonical name. API-agnostic (counts
   pod objects, not Go internals), so it neither duplicates nor depends on the
   exact single-flight symbol the impl lands.

**Opt-in / gated** like the sibling gVisor modules: the contract leg runs
everywhere; the substrate leg self-SKIPs unless `GVISOR_E2E=1` AND the substrate
is present. It honors `REQUIRE_SUBSTRATE=1` (the `ka-0000002867` production-
readiness go/no-go gate): an absent substrate is a **NO-GO hard fail** under the
gate, else a **SKIP LOUD** (PoC-substrate flakes never red the release pipeline,
`ka-0000000847`). Runs via
`GVISOR_E2E=1 ONLY=gvisor-duplicate-concurrent-spawn`.

```bash
./tests/e2e/scripts/test-gvisor-duplicate-concurrent-spawn.sh              # contract only
GVISOR_E2E=1 ./tests/e2e/scripts/test-gvisor-duplicate-concurrent-spawn.sh # + substrate
```

## `gb salvage` Dirty-Worktree → S3 → Restore (`test-gb-salvage.sh`)

E2E gate for the dirty-worktree rescue that bundles an involuntarily-killed
agent's uncommitted work to S3 (`ka-0000000188`, parent `ka-0000000958`) — MR
!5359 (`ka-0000004335`), Tier-2 guarded quarantine push !5613, plus the follow-on
fixes `ka-0000005216` (salvage a committed-but-**unpushed** HEAD, not only a dirty
tree), `ka-0000001049` (skip salvage on a clean `gb stop`; exclude agent-infra
paths from the WIP decision) and `ka-0000001959` (bundle nested `repos/*`).

**The gap it closes:** salvage is unit-tested by ~8 Go test files, but no e2e
exercised the real salvage → S3 → restore round-trip. The risk is data-integrity
and **silent**: salvage runs in the pod preStop hook as `gb salvage --timeout=15s
2>/dev/null || true`, so it always exits 0 and its stderr is discarded. If the
bundle/upload or restore path regresses, nothing goes red — the loss only surfaces
later as a dead agent's missing work.

1. **ROUND-TRIP** (always runs, hermetic): the REAL `gb salvage` binary uploads a
   REAL git bundle over the REAL AWS SDK S3 path to a local S3-compatible stub,
   and the REAL `gb salvage restore` pulls it back into a **fresh clone that never
   saw the WIP** — recovering a tracked edit, an untracked file, and a
   committed-but-unpushed commit, while a `.gitignore`d secret does **not**
   survive. This works with no cluster and no AWS credentials because salvage
   honours `COOP_S3_ENDPOINT` and forces path-style addressing (`s3EndpointOpts`),
   the same override the in-box MinIO overlay uses. Only the storage backend is
   local — the SDK wire path, git bundle format, manifest schema, and both CLI
   entrypoints are real. A spotless-tree fixture asserts the `ka-0000005216`
   direction specifically (the dirty-tree case would salvage anyway), and negative
   controls cover an unreachable sink (must still exit 0) and an empty prefix
   (must fail loudly, never report a false "restored").
2. **CONTRACT** (always runs, hermetic): the real-WIP vs agent-infra
   classification and the clean-exit skip table (`salvageIsCleanExit`) asserted
   over the full truth table with negative controls. A contract leg is required
   for clean-exit because that branch reads a **live** agent bead through the
   daemon client, which a shell test cannot drive without seeding a real agent and
   racing the controller.

Fully hermetic — `git` + `python3` + `gb` only, no kubectl/namespace/beads
daemon/AWS credentials/network — so it runs unconditionally in every shard and
self-SKIPs loud if a prerequisite is missing. Validated by mutation testing: a
gitignore bypass, a dropped unpushed-commit rescue, a corrupted S3 key, and a
silently-succeeding empty restore each turn it red.

```bash
./tests/e2e/scripts/test-gb-salvage.sh                    # hermetic, no substrate
GB_BIN=/path/to/gb ./tests/e2e/scripts/test-gb-salvage.sh # against a specific binary
```

## `gb whoami` Capability Introspection (`test-gb-whoami.sh`)

Coverage backfill (bead `ka-0000001750`, parent E2E-gap sprint `ka-0000000958`)
for `gb whoami` — the read-only "what can I actually do?" self-diagnostic merged
by `ka-0000003630` (`feat(gardenio/gb): add gb whoami`, SHA `f827c849a`). It
reports the AWS STS identity + assumable targets, the Kubernetes ServiceAccount +
the `auth can-i` probes that matter, the GitLab user + per-repo access level, and
which Secrets-Manager prefixes are readable.

**The gap it closes:** `grep -rI whoami gardenio/tests/` returned **zero** hits
repo-wide before this module. The 998 lines of net-new AWS/K8s/GitLab/secrets
probing shipped with **unit** coverage only (`whoami_test.go` — the pure parsers
+ the renderer over hand-built structs); nothing exercised the **real** `gb
whoami` binary driving its four external probes and assembling the report. A
regression there is high-blast-radius and **silent**: `gb whoami` is the
agent-facing input to the escalate/do-not-escalate decision that advice
`ka-0000000752` depends on, and it is load-bearing for the boot-time capability
block in agent prompts — yet every probe section is fail-open, so the command
never exits non-zero on a broken probe. Wrong output manufactures exactly the
false-premise escalations that advice exists to prevent, with no CI signal.

**Why PATH-shadowed stubs, not a live cluster:** the same hermetic pattern as
`test-gb-salvage.sh` and `grafana-render-cli` — drive the **real** `gb whoami`
binary, but shadow the four external tools it shells out to (`aws` / `kubectl` /
`glab` / `git`) on a curated two-entry `PATH` with deterministic fixture stubs
that emit **known** identities. The assertions key on the report the real binary
builds from them (its JSON body, its human render, and its `--jq` path), so the
parse → assemble → render chain is exercised for real with no kubectl context, no
namespace, no beads daemon, no AWS credentials, and no network. Crucially this
also lets `--probe` (which normally mints STS credentials via `sts:AssumeRole`)
run **hermetically**: because the real `aws` is shadowed, the probe never leaves
the process — directly honouring the task constraint that `--probe` must never be
a hard gate assuming a real role on a shard whose IRSA role cannot.

Nine legs, each keyed on a **specific** value that would be wrong if the
probe/parse broke (anti-vacuity), never merely "some non-empty string printed":

1. **read-only default** (headline) — `gb whoami --json` surfaces the exact
   fixture AWS ARN + account, the K8s ServiceAccount + namespace **split out of**
   the `system:serviceaccount:<ns>:<name>` username, the GitLab user, and no
   per-section error on a healthy probe.
2. **can-i two-path truth table** — the K8s ServiceAccount auth path is probed
   **independently** of the AWS identity (`get pods`/`read logs` → allowed;
   `create jobs` → denied). This is the exact miss (`ka-0000003844`) that
   motivated the feature — an agent that probed only AWS and wrongly concluded
   kubectl was unavailable — so a parser that always returned "allowed" reds here.
3. **human render** — the default block carries every section header + the same
   derived values, with ✓/✗ `can-i` marks.
4. **derivations** — GitLab access-level 30 → `"Developer"` (via group), the repo
   path resolved from `BOAT_PROJECTS`, and a set of secret names → the correct
   per-prefix aggregation (`gardenio/prod`:2, `gardenio/aws-poc`:1, total 3).
5. **`--jq` / `--json` parity** — `gb whoami --jq '.aws.caller_identity.Arn'`
   returns the single field, matching the JSON body.
6. **fail-open** — a denied `aws` + a 401 `glab` + an **absent** `kubectl` each
   annotate **only** their own section; `gb whoami` still exits 0 and the healthy
   sections still resolve (the independence + never-fail contract).
7. **offline / no-daemon** — with `BEADS_HTTP_ADDR` on a dead port and a bogus
   token, `gb whoami` still runs (it overrides the root `PersistentPreRunE` so
   introspection works with the daemon down — mirrors `gb version`).
8. **`--probe`** (hermetic) — with the `aws` stub implementing `sts:AssumeRole`
   deterministically, `--probe` promotes assumable targets to `allowed`/`denied`
   without minting any real STS credential.
9. **not-hardcoded / causation control** (AC #2) — a **second**, distinct
   identity fixture yields a report that tracks it; a constant-returning
   regression would make run 1 == run 2 and red this leg.

Verified by mutation testing: a `can-i`-always-yes stub reds leg 2/3, a non-SA
`whoami` username reds legs 1/3/6, an over-permissive `assume-role` stub reds leg
8, a fail-open regression that blanks a sibling section reds leg 6, a truncated
secret list reds leg 4, and a constant-ARN `aws` stub reds legs 1/5/7/9.

Fully **hermetic** — `gb` + `python3` only — so it runs unconditionally in every
shard and **SKIPs loud** (exit 0) if `gb`/`python3` is missing or the `gb` binary
predates the `whoami` subcommand (safe to wire in ahead of the rollout).

```bash
./tests/e2e/scripts/test-gb-whoami.sh                    # hermetic, no substrate
GB_BIN=/path/to/gb ./tests/e2e/scripts/test-gb-whoami.sh # against a specific binary
```

## lambda_microvm Execution Backend (`test-microvm-execution-target.sh`)

E2E gate for the **`lambda_microvm` agent execution backend** — the agent runtime that
landed *and* was prod-cutover'd inside 2026-07-17..07-31 (`ka-0000006937`, parent
`ka-0000000958`, anchor epic `ka-0000005010`).

**The gap it closes:** ~30 Go unit test files cover this surface at the UNIT level
(`internal/microvm/*_test.go`, `poolmanager/microvm_*_test.go`,
`reconciler_microvm_test.go`, `execution_target_resolve_test.go`) — but a grep of
`gardenio/tests` for `microvm|lambda_microvm|RunMicrovm` returned **nothing**
microvm-specific (the only `execution_target` hits are the *unrelated* escalation field,
whose values are `local|service_account`), and `run-shard.sh` wired no microvm module.
Note the existing `gb-stop-suspend` module tests the gVisor/runsc `checkpoint
--leave-running` FREEZE path (`ka-0000000910`) — a **different** execution backend — and
the PoC "C7 live E2E gate" (`gd-JMbObkFnHI`) was a one-off real-AWS smoke blocked on
cross-account trust, never a repeatable module. So a regression in `execution_target`
routing silently sends spawns to the wrong backend, and a suspend/resume-reconcile
regression leaks standby MicroVMs (real continuous AWS billing) or drops resumes.

**Why a CONTRACT leg:** the backend is default-OFF everywhere
(`agents.microvm.enabled=false`) and, when on, needs a **cross-account AWS launcher role**
(acct `345678901234`) plus real MicroVM spend. The staging e2e namespace sets no
`MICROVM_*` env, so `microvmLifecycle.Available()` is false and no MicroVM can be launched
from the suite — launch/suspend/resume/GC are *structurally* unobservable in CI. Both
existing "E2E validation" artifacts for this surface
(`docs/microvm-suspend-resume-e2e-validation.md`, `docs/microvm-warmpool-e2e-validation.md`)
are hand-run **operator runbooks** whose own §Preconditions/§Status sections record that the
live run was never executed — they are not automated regression nets.

Two legs, mirroring `test-stale-reaper-grace-reconcile.sh`:

1. **CONTRACT** (always runs — needs only bash, no AWS, ~0.1s): eleven decision tables (76
   assertions) reimplemented over bash predicates and asserted over the full truth table with
   negative controls and the `<`/`>` boundaries:
   - **execution_target resolution** (`resolveExecutionTarget`): layered
     agent > role > project > default, first-matching-role-wins for multi-role, and
     unknown/mis-cased → `kubernetes` (the can't-strand-an-agent guarantee).
   - **spawn fail-closed** (`handleNonK8sSpawn`): a microvm-targeted spawn with the backend
     unavailable **fails**, never falls through to a pod; `probe` fails closed too.
   - **suspend-on-stop eligibility** (`isMicrovmSuspendEligible`) — master switch +
     `lambda_microvm` + *full* Slack thread binding; a partial binding must terminate
     (suspending it would strand a billing VM nothing can wake).
   - **spawn/teardown twin-predicate lockstep** — `isMicrovmSuspendEligible` (in-VM signal)
     vs `microvmSuspendOnStop` (controller teardown) must agree on every input, or a
     `gb stop` either loses its resume target or leaves an unresumable billing zombie.
   - **resume-on-message routing** (`handleMicrovmResume`'s three guards): resume the SAME
     VM in place; every miss degrades to a cold respawn rather than dropping the message.
   - **AWS-state liveness** (`statusFromState`): `SUSPENDED`/`SUSPENDING` are **Live**, not
     Terminal — the *structural root* of "standby ≠ leak"; unknown states are neither.
   - **`agent_state` vocabulary completeness** — the builtin `type:agent` enum must admit
     every state the microvm lifecycle writes. This table **found a real latent bug**
     (`ka-0000000585`, fixed in the same change): the builtin enum omitted `resuming` and
     `resume_failed`, so on any daemon without a stored `type:agent` override (a fresh
     gasbox/GiB, a fresh e2e namespace — `seed-types-and-users.sh` does not seed
     `type:agent`) those field writes were rejected and, because both call sites only log,
     silently never landed. Prod was masked by an override typing the field as a plain string.
   - **GC standby≠leak + max-standby-age cap** and the **warm-pool cost opt-in** +
     **IdlePolicy AWS-min-60 floor** (`ka-0000005755`: a `0` there 400-blocked *every*
     prod launch).
2. **SUBSTRATE** (opt-in, `MICROVM_SUBSTRATE_E2E=1`): the one live-observable direction on a
   cluster with no MicroVM backend — seed a real agent bead forcing
   `execution_target=lambda_microvm` and assert the controller marks it
   `agent_state=failed` (with a diagnosable `failure_reason`, `ka-0000006530`) and creates
   **NO pod**. SKIPs LOUD by default.

```bash
./gardenio/tests/e2e/scripts/test-microvm-execution-target.sh                          # contract only
MICROVM_SUBSTRATE_E2E=1 ./gardenio/tests/e2e/scripts/test-microvm-execution-target.sh  # + substrate
```

## type:agent `agent_state` Enum (`test-agent-state-enum.sh`)

Backfills the ZERO **live-daemon** coverage of the three `type:agent` `agent_state`
enum values merge `568969352` added (`ka-0000000270`): `queued`,
`model_rate_limited`, `stop_requested`. All three controller call sites
(reconciler `markQueued`, in-pod `init_agent.go` model throttle, `RequestAgentStop`)
only **log** a PATCH error, so a rejected enum value silently never lands — the
third recurrence of the silent-PATCH-drop class (`ka-0000000585` →
`ka-0000006367` → this).

The only prior "coverage" was `test-microvm-execution-target.sh`'s
`state_accepted()` — a shell loop over a **hardcoded** `BUILTIN_AGENT_STATES`
literal that asserts a string against itself and passes with kbeads uninstalled.
This module instead drives the **live** daemon:

1. **Schema** — the daemon's *resolved* `type:agent` config lists each of the
   three values in the `agent_state` enum (`kd fields agent --json`).
2. **Write + read-back** — on an **OPEN**, `project=e2e-test`-fenced `type=agent`
   bead, `kd update -f agent_state=<v>` for each value, then read it BACK
   (`kd show --json | .fields.agent_state`). The read-back is load-bearing: a
   log-and-swallowed rejection still exits 0.
3. **Negative control** — `agent_state=totally_not_a_state` MUST be REJECTED
   (nonzero exit AND value unchanged), so the leg can never degrade into "the
   enum stopped validating and accepts anything."

Notes:
- The bead is kept **OPEN** across the writes (closed beads silently drop
  `agent_state` — it is a `transientClosedBeadFieldKeys`), and the
  `project=e2e-test` fence keeps the reconciler from cold-starting a pod.
- **Self-SKIPs LOUD** when the resolved `agent_state` is a plain `string` — a
  store `type:agent` override masking the builtin enum (the prod shape). The
  e2e daemon does not seed `type:agent`, so the builtin enum is authoritative
  there. The builtin-enum invariant itself is unit-pinned by
  `TestAgentSchema_AgentStateEnum` (`kbeads/internal/server/config_test.go`).

```bash
./gardenio/tests/e2e/scripts/test-agent-state-enum.sh
```

## Stale-Reaper Grace-Gate + Run-Instance Reconcile (`test-stale-reaper-grace-reconcile.sh`)

E2E gate for the two stale-reaper fixes that closed the live incident where
gardeners were killed mid-loop and crew mid-wrap-up (`gd-lbGpeO0u1R`, parent
`ka-0000000958`):

- **`7a44b0b5` / `ka-0000000018` — grace-gated orphan reap.** The reaper's
  working/empty-state orphan branch used to reap a live, actively-reporting pod the
  **instant** its source task closed — zero grace, no busy-check (100% of gardenio-ns
  reaps over a 2h window fired on `agent_state=working` pods whose `updated_at` was
  2-14s old). The fix gates the orphan branch on a grace window measured against
  `updated_at` (the busy/idle proxy: `SyncAll` bumps it every cycle while busy,
  skips it when coop reports idle): crew `orphanReapAfter=5m`, gardener
  `gardenerOrphanReapAfter=15m`. A busy pod keeps a fresh `updated_at` and is never
  orphan-reaped.
- **`977d6f2b` / `ka-0000000889` — stranded run-instance reconcile.** A
  schedule/cron run-instance task (`source:schedule` / `source:multi` /
  `source:schedule-mirror`, or a `*_fire_key` field) left `in_progress` by a gone
  assignee is single-shot and never retried — it piles up as a zombie "idle crew"
  bead. The reaper **closes** it once the assignee is gone: immediately if reaped
  this pass, else after a `runInstanceReconcileMinAge=15m` grace (guarding the
  just-claimed / agent-bead-not-yet-surfaced race).

**The gap it closes:** `test-stale-reaper.sh` only inspects controller logs/metrics
and asserts the reaper is enabled + "logged a pass". It never asserts the grace-gate
truth table (busy-within-grace NOT reaped; idle-past-grace IS) nor the run-instance
reconcile truth table. This module asserts both decision tables directly.

Two legs, mirroring `test-thaw-reaper-exemption.sh`:

1. **CONTRACT** (always runs — needs only bash): the two reaper decision tables
   reimplemented over bash predicates and asserted over the full truth table with
   negative controls (both the reap/close AND leave-alone directions, plus the
   `>`/`<` grace boundaries). A **contract** leg — not a pure live test — is required
   because the 5-15m grace windows exceed the per-shard wall-clock budget and the
   live daemon does not accept an `updated_at` override, so the past-grace directions
   are unobservable live within budget. The Go unit tests (`stalereaper_test.go`) are
   the in-tree companion; this pins the same logic at the e2e layer.
2. **SUBSTRATE** (opt-in, `REAPER_SUBSTRATE_E2E=1`): seed a REAL stranded
   run-instance (`in_progress`, ghost/gone assignee) freshly — inside the 15m grace —
   and assert the live controller reaper **leaves it alone** across a pass (the
   non-destructive within-grace direction the grace guard exists to enforce). A
   run-instance is a `type=task` bead, so seeding it can never trigger a reconciler
   pod cold-start. SKIPs loud by default.

```bash
./tests/e2e/scripts/test-stale-reaper-grace-reconcile.sh                       # contract only
REAPER_SUBSTRATE_E2E=1 ./tests/e2e/scripts/test-stale-reaper-grace-reconcile.sh # + substrate
```

## Escalation Auto-Reaper (`test-escalation-auto-reaper.sh`)

E2E gate for the **escalation auto-reaper** — the sweep that auto-closes an
escalation whose ask a human already satisfied **out-of-band** (bead
`ka-0000006112`, parent `ka-0000000958`). Guards three merged changes:

- **`011b9e33b` / `ka-0000004105` — the MR-witness reaper.** An operator merges
  an approve/merge ask's MR in the GitLab UI; no reverse event reaches the bead,
  so the escalation sits open at P0 forever. Age-ordering sorts those dead
  entries to the **top** of the operator queue (~15% phantom rate, measured
  2026-07-26), training the operator to distrust the whole queue.
- **`00b86ed3c` / `ka-0000007203` — the orchestration-layer witness.** Reaps on a
  signal the operator left on the **bead** (a terminal `result_status` on a
  still-open escalation, or a human-operator resolve-comment), covering the
  apply/deploy/mint asks no MR state can attest to.
- **`ccfc8411a` / `ka-0000002563` — the MR-landed review flag**, the complement: a
  **non**-reapable merge-then-apply ask whose MR has landed is flipped to
  `needs-review` and **never** closed, because the apply step may still stand.

**The gap it closes:** the five escalation modules (`-crud`, `-approval`,
`-execution`, `-v2`, `-slack-notify`) exercise create/approve/execute/resolve and
never assert auto-close; a grep of `gardenio/tests` for
`reap|mr_landed|needs-review` returns only the unrelated **agent-pod**
stale-reaper. Both failure directions hurt: under-reaping re-nudges operators
with dead asks, over-reaping silently closes a live P0.

**Why a from-source probe, not a live-bridge leg** (three independent reasons,
each sufficient):

1. The reaper runs inside **gitlab-bridge, which is `enabled: false`** in the
   ephemeral e2e namespace — there is no sweep
   process to observe.
2. The reaper **hard-skips e2e fixtures**: `evaluateBead`'s first guard is
   `isE2EEscalation(createdBy, labels)`, matching any `e2e-` actor prefix — and
   the suite's `KD_ACTOR` defaults to `e2e-test`. A suite-created escalation could
   never be reaped, by design.
3. gitlab-bridge exposes only `/healthz`, `/readyz`, `/webhook` and a separate
   `:9092` `/metrics` — no sweep-trigger endpoint — and the sweep interval is 10m.

So this follows the `gitlab-dark-provenance` precedent: `cmd/escalation-reap-probe`
drives the **real** `EscalationReaper` (the same `NewEscalationReaper`/`Run` path
gitlab-bridge runs in prod) against a stub GitLab `httptest` server and a mock
beads daemon that records every write. It asserts production code, not a bash
reimplementation. Hermetic — zero pods, zero shared-daemon writes — so it is a
**hard gate in every shard**, skipping loud only if the Go toolchain/source is
absent.

Eight legs: `merge-verb-reap` (merged ⇒ closed with a full audit trail; declined
⇒ reaped as moot with `approved_by=out-of-band`; still-`opened` ⇒ left alone),
`apply-verb-never-reaped` (**the load-bearing guard** — apply / no-`action` (the
real `ka-0000001987` shape) / mint asks are never reaped *even with a merged MR*;
the verb, not MR state, is the discriminator), `approve-only-approvals` (reads the
approvals endpoint, never MR state, since `ka-0000000844` requires its MR to stay
draft; unapproved + Community-Edition 404 stay open), `orchestration-witness`
(`success` reaps / `failure` does **not**; a human directive reaps while the same
words from an **agent** do not; zero GitLab calls), `e2e-fixture-skip` (with a
non-fixture control proving the sweep ran), `review-flag-never-closes` (one-shot
latch across sweeps), `idempotent-sweep`, and `host-scope-and-dryrun` (foreign-host
ref skipped before any API call; dry-run checks but writes nothing).

```bash
./tests/e2e/scripts/test-escalation-auto-reaper.sh   # hermetic; no cluster needed
```

## Dark-pod Jira write-token fence — network leg (`test-dark-jira-fence.sh`)

Coverage backfill (bead `ka-0000007263`, parent E2E-gap sprint `ka-0000000958`)
for the fence merged in MR `!6594` (bead `ka-0000005242`, R7 epic `ka-0000004670`).

R7 proved the "a dark agent cannot touch real upstream Jira" guarantee was **not**
structural: the privileged Jira **write** token could reach a dark pod via
unguarded plaintext-env injection (project/role `EnvVars` + `EnvOverrides`), which
— unlike the prefix-gated secrets path — had no name filtering. The fix is defense
in depth: **pod layer** (`scrubDarkForbiddenEnv` strips `JIRA_API_TOKEN` whichever
layer set it), **network layer** (the `dark-agent-egress` NetworkPolicy denies a
dark pod egress to Atlassian), **client layer** (`GARDENIO_DARK=true` ⇒ read-only
`JiraClient`, writes fail closed).

**The gap it closes:** the pod layer has unit coverage
(`podspec_dark_jira_test.go`), the client layer has unit coverage
(`jira_readonly_test.go`), and the chart **template** has render coverage
(`agent_external_keycloak_egress_test.go`, `landgate_test.go`) — but nothing asserted the **deployed** objects
fence the **real** Jira address. A render test proves the template emits CIDRs; it
cannot prove those CIDRs still cover `acme.atlassian.net`, nor that some
**other** policy re-grants what this one subtracts.

**Why set arithmetic, not a live `connect()`:** the in-shard suite spawns no real
agent pods (the gVisor suite that does runs out-of-band), and a connect probe is
ambiguous in the wrong direction — a timeout is indistinguishable from Jira being
down or a CNI hiccup, so the "secure" result is exactly what a broken environment
produces. Instead the module resolves the real Jira host and proves containment
against the deployed `ipBlock.except` lists.

Five legs, each a hard sub-gate:

1. **policies-present** — both halves of the post-fence split are deployed
   (`<release>-agent-internet-egress` + `<release>-dark-agent-egress`).
2. **selector-split** — the internet policy selects `gardenio.io/agent` Exists +
   `gardenio.io/dark` DoesNotExist; the dark policy selects `gardenio.io/dark`
   Exists. If the internet policy stops excluding dark pods, they regain unfenced
   internet through the union.
3. **union-exclusivity** — the structural leg. NetworkPolicy is **allow-union**, so
   the fence holds only if `dark-agent-egress` is the **only** egress policy
   granting a dark pod a broad `0.0.0.0/0` allow. Simulates label-selector matching
   over every egress policy in the namespace against a synthetic dark pod. A new or
   re-broadened policy silently re-opens Atlassian while every other leg stays
   green — this is the only leg that catches it.
4. **jira-fenced** — every IP `acme.atlassian.net` resolves to is **contained**
   in the dark policy's `except` list.
5. **nondark-control** — those same IPs are **not** excepted by the non-dark policy.
   Without this, leg 4 would also pass under a global egress blackout; it is what
   keeps the fence assertion from going vacuous.

Verified by mutation testing — each simulated regression is caught by the named
leg: emptied Atlassian list → leg 4; stale CIDR after an Atlassian re-IP → leg 4;
internet policy stops excluding dark pods → legs 2+3; a new catch-all egress policy
→ leg 3 only; Atlassian deny leaking into the non-dark policy → leg 5.

Pure `kubectl` + set arithmetic: no pod spawn, no daemon writes, fast and
deterministic. **SKIPs loud** (exit 0) on a namespace deployed before the fence
(shipped `2026.217.x`) — a present but **malformed** policy is a hard failure.

```bash
NAMESPACE=gardenio-e2e ./tests/e2e/scripts/test-dark-jira-fence.sh
# RELEASE defaults to $NAMESPACE; override when they differ (e.g. prod):
NAMESPACE=gardenio-system RELEASE=gardenio ./tests/e2e/scripts/test-dark-jira-fence.sh
JIRA_FENCE_HOST=other.atlassian.net ./tests/e2e/scripts/test-dark-jira-fence.sh
```

> The **pod-layer** (env-scrub) leg is deliberately **not** asserted here. In
> the e2e namespace `jiraBridge.enabled=false`, there is no Jira secret in the
> namespace, and no project/role bead injects `JIRA_API_TOKEN` — so "a dark pod
> lacks the token" passes **even with the scrub deleted**. Making it honest
> requires seeding a project bead whose `EnvVars` carry the token (the exact R7
> plaintext path) **plus** a non-dark control pod that keeps it: two real pod
> spawns, too heavy to gate a shard on. `podspec_dark_jira_test.go` covers that
> layer by construction.

## Project-level runtime_class inheritance (`test-project-runtime-class.sh`)

Coverage backfill (bead `ka-0000001145`, parent E2E-gap sprint `ka-0000000958`)
for the capability merged in commit `e0de71cb` (task `ka-0000006969`, epic
`ka-0000007417`): a `type=project` bead's **`runtime_class`** field pins **every**
agent in that project to that Kubernetes RuntimeClass (e.g. `runsc` → gVisor
freeze/thaw), with precedence **global < project < role < agent**. The field is
parsed by `beadsapi.ListProjectBeads`, loaded into the controller's
`ProjectCache` by `refreshProjectCache`, and applied to the pod by
`applyProjectDefaults` (`podspec.go`).

The merged change shipped with **unit** coverage only
(`podspec_gvisor_ns_test.go` asserts precedence over a hand-built cache map);
no e2e drove the real project-bead → parse → cache → pod chain, and the three
sibling gVisor e2e modules (`gvisor-substrate-ready`, `gb-stop-suspend`,
`gvisor-duplicate-concurrent-spawn`) all set `runtime_class` on **agent** beads
or raw pod YAML — none on a **project** bead. This module closes that gap.

Two legs, mirroring `test-gvisor-duplicate-concurrent-spawn.sh`:

1. **CONTRACT** (always runs — needs only bash): a faithful model of the
   `runtime_class` precedence resolution the two spec builders implement
   (`applyProjectDefaults` → `applyRoleDefaults` → agent-metadata override) plus
   the `routeGVisorNamespace` decision. Walks the full `global<project<role<agent`
   truth table **with negative controls** (a project pin does not leak onto a
   project that never set the field; a non-`runsc` project class does not route
   to the gVisor namespace; an explicit namespace pin wins), so a refactor that
   drops or reorders a precedence layer fails here, in every shard, with no
   substrate.
2. **SUBSTRATE** (opt-in, `GVISOR_E2E=1`): seed a **real** ephemeral
   `type=project` bead with `runtime_class=runsc` (NO per-agent/role tag), seed a
   plain agent bead in it, let the **controller** load the project into its cache
   and spawn the pod, then assert `.spec.runtimeClassName == runsc` **and** the
   pod landed in the gVisor namespace — purely from project inheritance.
   **Causation / no-leak control** on the same substrate: a plain agent in a
   **second** ephemeral project that sets **no** `runtime_class` field does *not*
   pin `runsc` and stays in the release namespace — proving the project field is
   both *sufficient* (the headline) and *necessary* (this control), ruling out a
   cluster that spuriously routes everything to gVisor. Asserts on the
   **observed** pod spec via `kubectl`, never Go internals. (The agent/role
   *override* precedence is proven by the contract leg + the merged unit tests,
   not the substrate leg: K8s admits a `runtimeClassName` only if that
   RuntimeClass exists, and the `gvisor-poc` substrate defines exactly one —
   `runsc` — so a non-`runsc` override can't be scheduled there deterministically.)

**Opt-in / gated** like the sibling gVisor modules: the contract leg runs
everywhere; the substrate leg self-SKIPs unless `GVISOR_E2E=1` (or
`PROJECT_RUNTIME_CLASS_E2E=1`) AND the `gvisor-poc` substrate + a controller
watching the seeded beads are present. It honors `REQUIRE_SUBSTRATE=1` (the
`ka-0000002867` production-readiness go/no-go gate): an absent substrate is a
**NO-GO hard fail** under the gate, else a **SKIP LOUD** (PoC-substrate flakes
never red the release pipeline, `ka-0000000847`). Runs via
`GVISOR_E2E=1 ONLY=project-runtime-class`.

```bash
./tests/e2e/scripts/test-project-runtime-class.sh              # contract only
GVISOR_E2E=1 ./tests/e2e/scripts/test-project-runtime-class.sh # + substrate
```

## Gate System Tests (`test-gate-system.sh`)

Tests `gb bus emit --hook=Stop` gate enforcement from the `bd-pe028` epic.

**Requires** `gb` binary (from `~/gardenio/controller`) and a kbeads server with gate system support.
The target namespace must be running kbeads at commit `8c92e4e` or later.

### Quick run (port-forward auto-setup):

```bash
GB_BIN=/tmp/gb KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-gate-system.sh
```

### With explicit daemon URL:

```bash
GB_BIN=/tmp/gb KD_BIN=/tmp/kd \
BEADS_HTTP_URL=http://localhost:19090 \
  ./tests/e2e/scripts/test-gate-system.sh
```

### Scenarios covered:

1. **Decision gate blocks Stop** — no decision offered → exit 2
2. **Decision created, not responded** → Stop still blocks → exit 2
3. **Decision closed (responded)** → gate satisfied → Stop allowed → exit 0
4. **No agent identity** → fails open → exit 0
5. **Dirty git tree** → commit-push soft warning → exit 0 with `<system-reminder>`
6. **Gate status transitions** — pending → satisfied → pending via `gb gate status/mark/clear`

### Claudeless scenarios (`claudeless/`)

TOML scenarios for claudeless-based full session lifecycle tests.
These simulate a complete Claude Code session without API calls, testing hook
behavior, gate enforcement, and agent lifecycle patterns.

Available scenarios:
- `gate-decision-flow.toml` — Decision gate enforcement: create decision → respond → exit
- `gate-no-identity.toml` — No agent identity → fails open (exit 0)
- `stop-gate-cooldown.toml` — Stop gate exponential backoff, yield marker, escape hatch
- `mail-nudge-delivery.toml` — Mail delivery pipeline: check-mail → inject queue → drain
- `agent-crew-lifecycle.toml` — Full crew lifecycle: ready → claim → work → close → done
- `local-lifecycle.toml` — `gb agent start --local` startup → claim → work → close → stop
- `local-prime-fetch.toml` — Prime/check-mail hooks work for a local (non-K8s) agent
- `local-yield-close.toml` — Local agent creates decision → yields → runner resolves → close
- `local-sigterm.toml` — Local agent idles; runner sends SIGTERM; graceful deregistration
- `local-orphan-cleanup.toml` — Stale local registry entry reaped on next invocation
- `gardener-gardener-start-idempotent.toml` — Second `gb gardener start` returns existing schedule (T9)
- `gardener-mention-entrypoint.toml` — `@gardenio <epic>` → `gb gardener start` round-trips to §5.1 schedule shape (T9)
- `gardener-conditions-fan-in.toml` — Gardener triggers fan in across {cron, bead_event, decision_resolved, manual} with throttle=60s (T9)
- `gardener-stop-no-progress.toml` — Gardener `stop.loop_end` carries `no_progress_for: {runs:3}` (T9)
- `gardener-stop-wall-clock.toml` — `--loop-wall-clock` round-trips into `stop.loop_end.wall_clock` (default 168h, override 72h) (T9)
- `gardener-progress-prime.toml` — `gb prime --for <agent>` renders the "## Loop Progress" block when the agent's `schedule_id` resolves to a gardener with `in_prime=true` (T9)
- `gardener-resume-escalate-metrics.toml` — the terminal_goal master gate (bead ka-0000005353): a premature max_runs stop with resume budget remaining RE-ARMS the gardener (open, `resumes_without_progress` bumped, run_budget refilled); with the budget spent it ESCALATES (gardener-stuck-escalate label + close); the escalate probe's in-process `/metrics` is scraped to assert the gardener's `gardener_resume_total{action="escalate",reason="max_runs"}` series; and `gb gardener start` stamps a resolved `slack_user_id` into the DEFAULT stop blob for a human slack: operator while a sentinel operator stays ownerless. Driven via `trigger-probe --source=gardener` (a single-gardener probe of the resume-or-escalate gate — NOT a fleet-wide EvaluateSource) (gd-H25qwM9YZt)
- `trigger-source-jira-fires.toml` — trigger:jira JQL match → notify action → audit bead `outcome=allow` (driven via the `trigger-probe` test binary against a stub JIRA HTTP server) (ka-0000003167)
- `trigger-source-gitlab-fires.toml` — trigger:gitlab fires for BOTH event shapes (merge_request + pipeline) → audit bead `outcome=allow` (driven via `trigger-probe` against a stub GitLab HTTP server) (ka-0000003167)
- `trigger-source-account-fires.toml` — trigger:account spend-threshold crossed by an account bead → notify action → audit bead `outcome=allow` (driven via `trigger-probe`) (ka-0000003167)
- `trigger-source-slack-reaction-fires.toml` — trigger:slack-reaction buffered reaction_added event → notify action → audit bead `outcome=allow` (driven via `trigger-probe`) (ka-0000003167)
- `advice-systemprompt-truncation.toml` — F9 integration proof: under a tight advice budget, `gb prime --for <agent> --system-prompt` (the subset fed to `claude --append-system-prompt-file`) stays under the 100 KB ceiling and carries the VISIBLE truncation footer — the truncated advice actually reaches the model's turn-1 context instead of being silently dropped. Gated more cheaply by `test-advice-budget.sh` Scenario 13; this is the wired-into-the-session integration proof (ka-0000003643)

The `trigger-source-{jira,gitlab,account,slack-reaction}-fires.toml` scenarios drive
sources that the standalone `trigger-engine` does not register (they need an external
JiraClient/GitLabClient HTTP API, an in-process reaction buffer, or account beads). They
use the test-only `gardenio/controller/cmd/trigger-probe` binary — built from source in the
e2e shard — which wires the REAL source + REAL
`TriggerEngine` + REAL `beadsapi.Client` against a stub HTTP server / buffered event and
fires exactly one `EvaluateSource` cycle. Each scenario auto-skips when `go`, the sources,
or the daemon env vars are unavailable.

`gardener-resume-escalate-metrics.toml` also uses `trigger-probe`, but through its
`--source=gardener` mode rather than a `TriggerSource`. The terminal_goal resume-or-escalate
gate runs inside `evalOne`'s `gardenerPreFireCheck`, which the fleet-wide `EvaluateSource`
fires against EVERY live gardener — so the probe instead drives
`TriggerEngine.ProbeGardenerResumeOrEscalate` (a test-only seam, sibling of `SeedSeen`)
against exactly ONE named gardener trigger bead, then optionally serves the in-process
default prometheus registry on `--metrics-addr` so the scenario can scrape
`gardener_resume_total` exactly as it would from the real gardener-firing bridge.

```bash
# Run a single claudeless scenario
claudeless --scenario tests/e2e/claudeless/gate-decision-flow.toml \
  -p "Run all test steps"

# Run all claudeless scenarios via the e2e module
./tests/e2e/scripts/test-claudeless.sh

# Run the local-mode scenarios via `gb agent start --local`
./tests/e2e/scripts/test-local-mode.sh
```

Claudeless is installed in the `registry.gitlab.com/acme/platform/gardenio/agent:latest` image.

### Local-mode runner (`test-local-mode.sh`)

Sibling of `test-claudeless.sh`. For each `local-*.toml` scenario the runner
invokes `gb agent start --local --test-scenario=<name>`, seeds any required
side-state (pre-assigned task for `local-lifecycle`/`local-yield-close`,
out-of-band decision resolution for `local-yield-close`, SIGTERM delivery
for `local-sigterm`), and asserts that the agent bead moved through
register → close. Emits SKIP when `gb`, `kd`, `coop`, or `claudeless` are
not present so it can be wired into CI before every pod image ships those
tools. CI wiring (`run-shard.sh`) is tracked separately
in A10 (ka-0000004098).

## Local runner scenarios

Four scripts cover the e2e cases in §11 of the local-agents spec
(`docs/superpowers/specs/2026-10-11-local-agents-escalations-design.md`; plan
gd-fwFefkDP4H Task 18, gd-jW7KJ7ZdNy). They share `scripts/lib-local-runner.sh`,
which has the `test-local-mode.sh` preamble plus runner helpers: it mints an
invite, redeems it with `gb runner register` into a private `GARDENIO_HOME`, and
starts `gb runner join --command "claudeless --scenario ..."` in its own
process group. Each script prints a PASS/FAIL/SKIP summary and exits non-zero
on any FAIL. Every negative assertion has a positive control: the same request
from an identity that is allowed, which must succeed.

| Script | Spec | Scenario | What it proves |
|---|---|---|---|
| `test-local-runner-mail.sh` | (a) | `images/agent/scenarios/mail-reader.toml` (mock pod) | A pod agent gets a nudge, a decision answer and a kill as mail. Its own subscriber stamps `delivered_at` and the session closes the mail. The agent bead has `pod_name` and no `coop_url` |
| `test-local-runner-lifecycle.sh` | (b), (e) | `claudeless/local-runner-mail.toml` | `register` writes 0600 files, with no token in `runner.json`, and the invite works only once. A placed task shows in `gb ready` on the runner and nowhere else. `join` makes a `laptop`/`local` agent that claims the task and squawks. When the join process group is SIGSTOPped, the agent pauses, its task stays assigned and new mail waits. SIGCONT resumes the agent and the waiting mail is delivered. `leave` marks the runner away |
| `test-local-runner-escalation.sh` | (c), (d) | `claudeless/local-runner-escalation.toml` | A `--execution-target local --target-runner` escalation: a non-owner's approve gets a 403 ("only its owner may approve"), the owner's approve goes through, `executor` is the joined agent, there is exactly one `control:escalation-approved` mail, and the agent reports success. **(d), local to cloud, is M2 (spec §12) and always prints a SKIP line** |
| `test-local-runner-listener.sh` | (f) | — | The daemon returns 401 for a non-runner bearer that carries `X-Gardenio-Listener: runner`, and 200 for the same request without the header. On the public listener only the runner routes are reachable: a gr_ token works, the shared token and missing bearers get Traefik's 404, `register` reaches the daemon, and `/v1/admin` and approve have no route |

What they need:

- All four: `kd`, `gb` and `jq`, and a namespace from `NAMESPACE` (port-forwarded) or
  `BEADS_HTTP_URL`. The lifecycle and escalation scripts also need `coop`
  and `claudeless`. None of them is in the `HERMETIC_ONLY` subset; each is a
  `run_module` in `run-shard.sh` (`local-runner-mail`, `-lifecycle`,
  `-escalation`, `-listener`).
- **An admin token** for the lifecycle, escalation and listener-ingress
  scripts, because `type=runner_invite` needs admin rights. Without one, the
  lifecycle and escalation scripts skip and write the run-shard skip sentinel.
- (a) needs an agent image that includes `mail-reader.toml`, plus the
  slack-bridge, which sends decision answers; with no `<namespace>-slack-bridge`
  Service (the lane, until slackmock runs there) that assert prints a SKIP line.
  The kill mail needs a credential the daemon lets author `control:` mail (an
  operator, or a fleet in `KBEADS_CONTROL_MAIL_FLEETS`; a pod agent has no
  runner token): put it in `E2E_CONTROL_MAIL_TOKEN_FILE`. Without that file the
  script tries the ambient token and skips the kill asserts if it is refused.
- (c) needs approval-capable tokens: an OIDC token from an allowlisted
  browser or Slack client. A kd token or the shared token is refused just for
  being one. `E2E_RUNNER_OWNER` names the runner's owner (default
  `BEADS_OPERATED_BY`). Put the tokens in files:
  `E2E_OWNER_APPROVER_TOKEN_FILE` for that owner and
  `E2E_STRANGER_APPROVER_TOKEN_FILE` for any other operator. Without the
  owner's file the script tries the ambient token and skips if it is refused.
  Without the stranger's file the non-owner check prints a SKIP line.
- (e) can take up to 90 s for the runner to count as away, plus up to 5 min
  for the next stale-reaper pass, in each direction. `LR_AWAY_TIMEOUT`
  defaults to 480 s.
- (f): set `RUNNER_PUBLIC_URL` to an install with
  `beads.ingress.runnerRoute.enabled=true`, or the k3s public edge. Without it
  only the daemon half runs and the ingress half prints a SKIP line.
  `E2E_RUNNER_HOME` lets you use a runner that is already registered instead
  of minting one. Tokens go to curl through a 0600 header file and are never
  printed.

## Onboarding formulas (`test-onboarding-formulas.sh`)

Pours each formula in `gardenio/formulas/onboarding/` (`register-project`,
`add-local-agent`, `first-task`, `enable-slack`) with `kd formula pour <file>`
and has a claudeless local agent work the molecule up to its first gate.
For each formula:

1. Every declared step is poured, by count and by name.
2. Every gate step carries `gate_type`. A decision gate pours as `type=decision`
   and a manual gate as `type=task`. No other step is gated.
3. The open, unblocked steps right after the pour are the expected ones. When
   that is the sign-in gate (`register-project`, `add-local-agent`), the runner
   closes it, standing in for the person.
4. `gb agent start --local --test-scenario=onboarding-<formula>` runs the
   agent. It claims each ready task step, runs the command the step names,
   comments the output on the step, closes it, and stops. Its tool calls go
   through `scripts/onboarding-agent-step.sh`, which refuses to claim a gate or
   a step that `gb ready --plan <molecule>` does not list.
5. The agent's steps are closed, with the agent as assignee. The gate it stops
   at (`pick-account`, `pick-role`, `pick-task`, `install-app`) is still open,
   keeps its `gate_type`, and is now the only ready step. Every later step is
   still open.

The expected steps, gates and stop points are pinned in `formula_spec()` in
the script, not read back from the formula files. If a step is removed or a
gate dropped, the suite fails. The suite ends by pouring two edited copies of
`first-task.json`, one missing a step and one with its decision gate dropped,
and checks that both fail. When you change a formula on purpose, update
`formula_spec()` in the same PR. To try edited copies, set
`ONBOARD_FORMULA_DIR=<dir>`.

The daemon does not refuse `kd close` on a step whose blockers are still open.
A gate holds only because agents take their work from `gb ready`, which is why
check 2 pins every gate.

Each run uses a throwaway `project:e2e-onboard-<pid>-<epoch>` label. Every
poured bead is closed and deleted on exit. A run takes about two minutes.

**Where it runs.** It is the `onboarding-formulas` module in `run-shard.sh`,
next to `local-mode`, so it runs on a cluster shard. It skips without `gb`,
`kd`, `coop` and `claudeless`. No `.github/workflows/e2e.yml` job runs it. The
only job there with a daemon is `viewer`, and that job builds neither `coop`
nor `claudeless`.

**By hand against `kd serve`.** You need `gb`, `kd`, `coop`, `claudeless` and
`python3` on PATH. Start Postgres with pgvector, then a daemon with no auth and
no NATS (the `viewer` job's recipe, `docs/ci-e2e-postgres-recipe.md`). From the
repo root:

```bash
# Postgres 16 with pgvector, e.g.
docker run -d --name beads-pg -p 5432:5432 \
  -e POSTGRES_USER=beads -e POSTGRES_PASSWORD=beads -e POSTGRES_DB=beads \
  pgvector/pgvector:pg16
PGPASSWORD=beads psql -h localhost -U beads -d beads \
  -c 'CREATE EXTENSION IF NOT EXISTS vector;'

# kd serve: no BEADS_AUTH_TOKEN (dev mode) and no BEADS_NATS_URL (it fails
# closed against an unreachable NATS)
env -u BEADS_AUTH_TOKEN -u BEADS_NATS_URL \
  BEADS_DATABASE_URL='postgres://beads:beads@localhost:5432/beads?sslmode=disable' \
  BEADS_HTTP_ADDR=:18080 BEADS_GRPC_ADDR=:19090 BEADS_METRICS_ADDR=:19091 \
  BEADS_RATE_LIMIT_ENABLED=false kd serve >/tmp/kd-serve.log 2>&1 &
until curl -sf http://localhost:18080/v1/health/live >/dev/null; do sleep 1; done

BEADS_HTTP_URL=http://localhost:18080 \
  ./gardenio/tests/e2e/scripts/test-onboarding-formulas.sh
```

Inside a gardenio agent pod, the environment points `kd` and `gb` at the
fleet daemon and carries its tokens. Run both `kd serve` and the suite under
`env -i PATH="$PATH" HOME="$HOME" …` with only the variables above, so nothing
reaches the fleet daemon. A pod has no Docker daemon and no sudo. There, run
`apt-get download postgresql-16 postgresql-16-pgvector libllvm17t64` (with
`-o Dir::State::Lists=<dir> -o Dir::Cache=<dir>` after an `apt-get update`
into the same dirs), unpack each package with `dpkg -x <deb> <root>`, and run
`<root>/usr/lib/postgresql/16/bin/initdb` and `pg_ctl` with
`LD_LIBRARY_PATH=<root>/usr/lib/x86_64-linux-gnu`. Pass `-k ''` to `pg_ctl`:
a deep scratch path is too long for the Unix socket.

## Decisions/Yield Tests (`test-decisions-yield.sh`)

Tests `gb decision` CRUD and `gb yield` blocking behavior.

**Requires** `gb` binary and a kbeads server with decision/yield support.

### Quick run (port-forward auto-setup):

```bash
GB_BIN=/tmp/gb KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-decisions-yield.sh
```

### With explicit daemon URL:

```bash
GB_BIN=/tmp/gb KD_BIN=/tmp/kd \
BEADS_HTTP_URL=http://localhost:19091 \
  ./tests/e2e/scripts/test-decisions-yield.sh
```

### Scenarios covered:

1. **Decision create** — `gb decision create --no-wait --json` returns ID, exit 0
2. **Decision list** — created decision appears in `gb decision list --json`
3. **Decision show** — shows prompt, options, status=open
4. **Decision respond** — `gb decision respond <id> --select=a` closes it; chosen=a
5. **Yield unblocks on decision close** — background `gb yield --timeout=15s`, respond to decision, yield exits 0 with "resolved"
6. **Yield unblocks on mail** — background yield, `gb mail send`, yield exits 0 with "Mail received"
7. **Yield timeout** — `gb yield --timeout=3s` with no events, exits 0 with "Yield timed out"

## Stack Health Tests (`test-stack-health.sh`)

Verifies the gardenio infrastructure is healthy before running functional tests.

### Scenarios covered:

1. **No CrashLoopBackOff pods** — Scans namespace for pods stuck in CrashLoopBackOff
2. **Core deployments ready** — beads, controller, nats have ready replicas
3. **Beads HTTP health** — `/healthz` endpoint returns 200
4. **PostgreSQL service exists** — PostgreSQL service is discoverable
5. **NATS service exists** — NATS service is discoverable
6. **Coopmux deployment (optional)** — coopmux Deployment ready when present (`coopmux.enabled=false` in minimal namespaces). The deep `/api/v1/health` probe lives in `test-migration-coop-reachable.sh`.

Stack-health runs first in the E2E pipeline. If infrastructure is broken, there's no point running functional tests.

## Migration-Acceptance Smoke (`MIGRATION_ACCEPTANCE=1`)

P4.4 fresh-prod cutover gate (epic `ka-0000003481`). Three codified post-cutover
checks that exercise the real post-auth data path and agent→MR loop the earlier
pre-auth-only checks missed (the `ka-0000007004` 502):

- `test-migration-postauth-viewer.sh` (`ka-0000008176`) — authenticated viewer
  GET → **200 with real daemon-backed body**, not the pre-auth `303`.
- `test-migration-coop-reachable.sh` (`ka-0000004091`) — coopmux
  `/api/v1/health` healthy behind the edge.
- `test-migration-agent-opens-mr.sh` (`ka-0000002701`) — a real agent opens an
  MR end-to-end (opt-in `AGENT_MR_E2E=1`).

Run the named subset with `MIGRATION_ACCEPTANCE=1 NAMESPACE=<ns>
./scripts/run-shard.sh` (point `NAMESPACE` at prod to run it there). Full runbook:
[`gardenio/docs/migration-acceptance-smoke.md`](../../docs/migration-acceptance-smoke.md).

## Dynamic-Workflow Resume Tests (`test-dynworkflow-resume.sh`)

Regression guard for the core resilience property behind running pilots as
dynamic workflows (gd-yJnnThsTeF, parent ka-0000006835): a gardener-shaped
agent can lose its pod **mid dynamic-workflow run** and the next pod resumes
via `resumeFromRunId` — completed `agent()` calls return cached results, only
the unfinished tail re-runs — instead of restarting the whole workflow. Builds
on the companion-dir S3 sync (ka-0000000659).

Two layers, both assert via journal/run-id state (never wall-clock):

- **Layer A — companion-dir journal contract (hermetic):** builds the exact
  Claude companion-dir layout (`<proj>/<uuid>/agent-*.jsonl` +
  `runs/<run-id>.json`, the sibling of the transcript `.jsonl`) on a temp FS
  with a partial run (agent-1 done, run-state `in_progress`), then asserts the
  resume decision: same run-id survives, completed-prefix journals are cached
  byte-identically, a fresh start would discard them, and the resumed tail
  runs only the unfinished `agent()` calls. Runs anywhere; no cluster needed.
- **Layer B — bead lifecycle + optional live-pod check:** an agent bead plus a
  workflow-run session chain (session 1 crashes mid-run with exit 137,
  `resumed=false` → session 2 `resumed=true`, same PVC host, same run-id via
  `resume_from_run_id`). When a live cluster is available, an optional check
  greps the agent pod for coop's `restored workflow companion dir` log line
  and inspects the PVC companion dir; it skips cleanly otherwise.

### Quick run:

```bash
# Layer A runs standalone; Layer B skips gracefully when kd is unreachable.
KD_BIN=/tmp/kd GB_BIN=/tmp/gb \
  ./tests/e2e/scripts/test-dynworkflow-resume.sh
```

### Scenarios covered:

1. **A1–A6** — companion-dir derivation, partial run journal, run-id
   continuity, cached completed-prefix, fresh-start discards state,
   unfinished-tail-only re-run.
2. **B1–B5** — gardener-shaped agent + crashed→resumed session pair sharing one
   workflow run-id.
3. **B6** — optional live-pod verification (coop restore log / PVC journal).

## gVisor-Namespace Sweep Tests (`test-gvisor-ns-sweep.sh`)

Regression guard for the controller treating the PSA-privileged gVisor
namespace (`gardenio-gvisor`) as first-class alongside the primary release
namespace (gd-obdTf3fIcu, backfilling ka-0000007922 + ka-0000008076).

A prewarmed-pool standby or any runsc/gVisor agent (`role=gvisor-poc`) routes
its pod to `cfg.GVisorNamespace` via `routeGVisorNamespace`, not the primary
namespace. Two commits taught the controller to mirror/sweep that second
namespace: `9862042a` (secretreconciler mirrors every managed ExternalSecret
into the gVisor ns, so a runsc pod no longer stalls on "secret(s) not yet
synced in namespace gardenio-gvisor") and `554fb95e` (reconciler / stale-reaper /
pvc-gc reach the gVisor ns for orphan cleanup, drift recreation, stale-pod
deletion, and workspace-PVC GC — a clean no-op when the gVisor ns is unset).

The four single-namespace E2E modules (`test-secret-reconciler.sh`,
`test-reconciler.sh`, `test-stale-reaper.sh`, `test-pvc-gc-ttl.sh`, all
`NAMESPACE=gardenio-e2e`) assert none of the gVisor-ns paths, so a regression
dropping the mirror/sweep for the second namespace would sail through the gate
unseen. Like `test-pvc-gc-ttl.sh`, this is a hermetic `go test` wrapper — it
does **not** require a live cluster; it shells out to the package unit tests
that exercise each subsystem against a fake clientset / httptest daemon so the
multi-namespace behaviour is visible in the E2E gate. (The real gVisor
freeze/thaw pod lifecycle is exercised separately by the opt-in
`test-gvisor-*.sh` suite under `GVISOR_E2E=1`.)

### Quick run (no cluster needed):

```bash
./tests/e2e/scripts/test-gvisor-ns-sweep.sh
```

### Scenarios covered (one subsystem per step, each with its opt-in guard):

1. **secretreconciler** — a managed ExternalSecret is created in BOTH the
   primary and gVisor namespaces; created only in the primary ns when the
   mirror is not enabled.
2. **reconciler** — a gVisor-ns pod is counted present (no duplicate create),
   a gVisor-ns orphan is deleted in its own namespace, Autodestruct sweeps
   both namespaces, and the gVisor ns is invisible when unset.
3. **stale-reaper** — a stale runsc pod is deleted from the gVisor ns
   (a non-runsc pod stays in the primary ns), a pinned namespace wins, and
   routing is opt-in.
4. **pvc-gc** — an orphan workspace PVC in the gVisor ns is GC'd, a PVC still
   mounted by a pod in its own namespace is not, and the sweep is opt-in.

## Beads CRUD Tests (`test-beads-crud.sh`)

Tests kd CLI CRUD operations against a live kbeads server.

**Requires** `kd` binary and a running kbeads server.

### Quick run:

```bash
KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-beads-crud.sh
```

### Scenarios covered:

1. **Create** — `kd create` returns a bead ID, exit 0
2. **Show** — `kd show` returns correct title, type, status
3. **Update** — `kd update -d` modifies description
4. **List** — `kd list` includes the created bead
5. **Close** — `kd close` exits 0
6. **Verify closed** — `kd show` returns status=closed

## Auto-Close Cascade Tests (`test-auto-close-cascade.sh`)

Closes the coverage gap on the kbeads daemon's auto-close cascade
(`kbeads/internal/server/auto_close.go`, `afterBeadClosed` — ka-0000003089).

The cascade was unit-tested only: `auto_close_test.go` is 44 tests against an
in-memory `mockStore`, so the SQL `ParentSubtreeID` / reverse-dep enumeration it
is built on is stubbed out, and the bridge-writes-merge-state → daemon-reacts
seam is never crossed. The neighbouring e2e modules stop short too —
`test-epics-children.sh` scenario 5 closes its epic **manually** (so last-child
auto-close never fires; that assertion passes identically with the cascade
deleted), and `test-mr-merge-flow.sh` covers only the jira-bridge Review
transition.

Every regression here is **silent by construction**: the cascade is best-effort
and never fails the originating close, so a broken cascade still returns HTTP 200
and simply stops transitioning work — merged work stops closing and churns in
`gb ready` forever, or an epic is hard-closed out from under a running gardener.

**Requires** `kd` binary, `python3`, and a reachable kbeads daemon
(`BEADS_HTTP_URL`, or port-forward access to `svc/${NAMESPACE}-beads`).

### Quick run:

```bash
BEADS_HTTP_URL=http://localhost:8080 \
  ./tests/e2e/scripts/test-auto-close-cascade.sh
```

### Scenarios covered:

1. **Last-child epic drain** — epic stays OPEN at 1-of-2 children closed, then
   auto-closes when the **last** child closes.
2. **Merged-MR leaf close** — closing a merged `type=merge-request` bead closes
   the leaf that `produces` it; an **unmerged** MR bead must not.
3. **Composed cascade** — one MR-bead close closes the leaf **and** drains the
   leaf's parent epic in a single propagation (the seam no unit test crosses).
4. **Decision-close park** — a `blocks`-gated bead parks to `blocked` (not
   closed); a `parent-child` neighbour of the same decision is left alone.
5. **Live-gardener defer** — a drained epic with an open, non-disabled
   trigger/schedule descendant must **not** close (bug ka-0000001525); disabling
   and closing that gardener then releases the drain.

### Conventions this module relies on:

- Every positive assertion keys on `closed_by=system:auto-close` (or
  `system:decision-gate`), not merely on terminal status — so a bead closed by
  the cleanup trap, the harness, or the test's own `kd close` can never
  masquerade as a cascade-driven transition.
- Every scenario carries a **negative control**, so the module fails when the
  cascade over-fires, not only when it under-fires. Both directions are
  mutation-verified: dropping the `produces` edge reds scenario 2, and
  pre-disabling the gardener fixture reds scenario 5.
- Fixtures are created with `--no-parent` (the daemon otherwise auto-parents new
  beads to the creating agent's task, splicing fixtures into a live epic's
  subtree) and **deleted** rather than closed at teardown, so teardown cannot
  fire the very cascade under test.
- The scenario-5 trigger fixture is inert by four independent mechanisms: no
  `trigger:*` source label (every `LoadTriggers` filters on one, so no source can
  enumerate it), a `fixture:e2e:` label (`isE2EFixture`), `action=assert` (spawns
  no agent, and is rejected at the first gate by both *mutating* gardener sweeps),
  and no `gardener` field (`isGardenerTrigger` false). Do not "simplify" that
  fixture's fields — see the comment block above it.

## Thread-Task Idle-Hint Demotion (`test-threadtask-idle-hint.sh`)

Module name: `threadtask-idle-hint` (bead `ka-0000007208`, parent
`ka-0000000958`; flag flip `ka-0000000287`). LIVE-DAEMON gate for the thread-task
auto-closer's **idle-hint demotion** path — the death-rule inversion that is the
auto-closer's only behaviour since gd-Zhcpn9IZVl: it never closes a wish
(`internal/bridge/thread_task_autoclose.go`). The flag that once selected it was
retired in gd-ARcfDN4zrh.

**The gap it closes:** MR `7d35eb2ce` (`ka-0000000287`) did TWO things — it added
a 9-scenario **Go unit** suite (`thread_task_autoclose_demoted_test.go`, over a
`mockDaemon`) AND it flipped `slackBridge.slack.threadTaskHintInsteadOfClose:
true` in the staging values, inverting the LIVE behaviour of the
auto-closer in the **exact** namespace this suite runs against: an idle wish now
gets an actionable **hint** and stays OPEN instead of being closed. A selector
grep for `gardenio-idle-hint` / `idle-hint` / `HintInsteadOfClose` /
`threadTaskHint` returned **zero** hits across `tests/e2e/` and
`tests/beads-viewer-e2e/`, and the "auto-close" shell modules
(`test-auto-close-cascade.sh` = the parent-child close **cascade**,
`test-stale-reaper.sh` = the stale-**agent** reaper) cover different subsystems —
a `thread_task` grep inside them is empty. So the demoted world had **no** e2e
assertion at all.

**Why a live-daemon `go:build e2e` test, not a shell reimplementation or the
mock unit suite:** the demotion logic lives in the **bridge**
(`ThreadTaskAutoCloser`), driven by a 30m close window + a 15m backfill sweep
with **no** admin/sweep-trigger endpoint — the same "far past a shard's
wall-clock budget" constraint that makes `escalation-auto-reaper` a probe. A
shell test can neither wait out the window nor reach `runCheck`/`sweepOnce` (both
unexported). And per the sibling escalation `/result` live-daemon backfill
(`58054fe9f`: *"a mock re-implementing the daemon is not evidence the daemon does
it"*), the mock unit suite — while it covers all 9 demoted scenarios — cannot
attest three **real daemon semantics** this path depends on, each empirically
confirmed against the live staging daemon while authoring the test:

1. A comment write does **not** bump the bead's `updated_at` — so
   `alreadyHintedThisEpisode`'s activity-relative dedupe (compare the hint's
   `created_at` against `max(bead.UpdatedAt, newest non-hint comment)`) is
   deterministic. A mock that bumped `updated_at` on comment would mask a dedupe
   regression.
2. A write with no operator resolves **400 `operator_refusal`** — so
   `autoCloseOperatorContext`'s per-call attribution is the only thing keeping
   the `AddComment` write from fail-closing (the client-level `SetSystemOperator`
   default was removed, `ka-0000006293`).
3. The live `type=task` schema **rejects** the `triage_reason` field (400
   "unknown field"), so `markNeedsTriage`'s field write silently no-ops — yet the
   wish still stays OPEN + `needs-triage` **label** + triage **comment** (the
   durable signals the sweep keys off). This is the "assert the shape prod emits"
   lesson (`ka-0000001521`): the label + comment are load-bearing, the field is
   not present on this deployment.

So the module wraps a `go test -tags e2e -run
TestThreadTaskIdleHintDemotionLiveDaemonE2E` (invisible to `go build ./...` /
`go test ./...` / lint — none pass `-tags e2e`) that drives the **real**
`ThreadTaskAutoCloser.runCheck` (the same event-path code the bridge runs in
prod) against a **real** `beadsapi.Client` pointed at the live daemon.

**Event path only (hermetic on a shared daemon):** `runCheck` is scoped to a
SINGLE thread bead id — the one each sub-test seeds under a unique synthetic
`slack-thread:{channel}:{ts}` label — so it touches ONLY that bead and keys only
on that id (never ambient prod state). The backfill `sweepOnce`, which lists
EVERY open `source:slack` task (`Limit 1000`) and would hint on ambient prod
wishes, is deliberately **not** driven here; its dedupe / positive-evidence legs
stay in the mock unit suite where they can run safely.

Five legs, one per task contract point:

1. **demotion** — a done agent → exactly ONE idle-hint COMMENT and the wish stays
   `status=open` (the death-rule inversion).
2. **marker + affordance** — the hint ends with the `[gardenio-idle-hint]` marker
   on its own trailing line and carries the ⏹️ (`:black_square_for_stop:`)
   in-place close affordance; asserted on the MARKER, not the prose (which is
   allowed to evolve).
3. **dedupe** — two `runCheck` passes in one idle episode → exactly ONE hint.
4. **fresh episode** — a human reply newer than the hint opens a new episode → the
   next check re-hints (activity-relative, not a permanent gag).
5. **failed carve-out** — a FAILED agent → wish stays OPEN + `needs-triage` label +
   triage comment, NEVER idle-hinted.

Mutation-verified: driving the failed leg with a DONE agent reds leg 5 (a done
agent hints, earning no triage label/comment).

**Skip-clean contract:** SKIPs CLEANLY (exit 0) with a LOUD banner when the Go
toolchain / controller source is absent OR the beads daemon is unreachable — a
missing prerequisite must never be read as a pass. When present, every leg is a
HARD assertion. Registered beside `thread-respawn-resume` in `run-shard.sh`.

```bash
BEADS_HTTP_URL=http://localhost:19090 \
  ./gardenio/tests/e2e/scripts/test-threadtask-idle-hint.sh
# or standalone (self port-forwards NAMESPACE=gardenio-e2e svc/<ns>-beads):
NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-threadtask-idle-hint.sh
```

## Agent Cost + Context-Size Tests (`test-agent-cost-context.sh`)

Closes the per-agent cost + context-size coverage gap (gd-L8DNvIHRJ2).
Complements the SESSION-bead usage module (`test-usage-tracking.sh`),
the LiteLLM proxy spend module (`test-litellm-cost-tracking.sh`), and the
fleet aggregate `/metrics` module (`test-fleet-metrics.sh`) — none of which
assert the **per-agent bead rollup** or the **context-size chip**.

Data path under test: `statusreporter.reportLiveUsage` mirrors coop's live
context window + running cost estimate onto the agent bead as
`context_tokens` / `live_cost_usd`; `reconcile_session_spend.go` folds each
closed session's spend into the authoritative `total_cost_usd`; and
beads-viewer `buildAgentUsageView` (`cmd/beads-viewer/agent_usage.go`) turns
those fields into the chips rendered by `agent_usage_chips.html` on the
`/agents` rows and the `/{id}` bead-show card.

**Requires** `kd` binary and a live kbeads server. Viewer scenarios SKIP
cleanly when the `beads-viewer` service is absent or its OIDC auth is
unavailable in the target env (exit 0).

### Quick run:

```bash
NAMESPACE=gardenio-e2e KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-agent-cost-context.sh
```

### Scenarios covered:

1. **Schema persistence** — `type:agent` accepts + persists `context_tokens`,
   `live_cost_usd`, `total_cost_usd` on an agent bead
2. **`/agents` chips (live)** — a `working` agent renders the humanized
   context label (`128.0k`), the `ctx` marker, and a cost chip
3. **Bead-show chips (live)** — `/{id}` card renders the context chip and the
   composed live cost (`total_cost_usd` + `live_cost_usd` = `$1.92`)
4. **Cost composition** — the cost chip renders even when `total_cost_usd=0`
   but `live_cost_usd>0` (live estimate counts)
5. **Closed semantics** — a closed agent suppresses the context chip and shows
   ONLY `total_cost_usd` (`$2.00`); the stale `live_cost_usd` is NOT
   double-counted (no `$2.99`)

## Session Spend Attribution Tests (`test-session-spend-attribution.sh`)

Closes the session spend attribution coverage gap (gd-NU1IbgLk5v): the
per-session pipeline shipped end-to-end (F2/E3 counter, `/spend/tags`
close-time reconciliation, coop cost-free usage NATS event) with strong UNIT
coverage, but nothing wired the pieces together as the controller runs them.
It also closes the claudemore proxy-transparency gap (ka-0000005743, A1
ka-0000007533) via its `proxy-transparency` leg — the only spend coverage
anywhere that attaches a `proxy_url` to an account and proves the spend read
bypasses it. This is the fifth cost/attribution module and a distinct,
non-overlapping surface from its four siblings:

- `test-fleet-metrics.sh` — the kbeads `/metrics` fleet aggregate, NOT the
  controller `:9092` `gardenio_agent_spend_usd_total`.
- `test-litellm-cost-tracking.sh` — the LiteLLM proxy `/key/info` + `/spend/logs`
  and the `spend_sync` account-bead `shadow_*` fields.
- `test-litellm-fallbacks.sh` — the DEPLOYED router's
  `router_settings.fallbacks` arms (ka-0000003835). Reads the live
  `/get/config/callbacks`, asserts the crew-e2e `claude-opus-4-8-oauth ->
  claude-opus-5-oauth` spill (ka-0000007458) is present, nested under
  `router_settings` (a top-level `fallbacks:` is silently ignored,
  ka-0000005911), and every primary+target resolves to a live model group.
  Distinct from the chart-render unit test `litellm_router_alias_test.go`,
  which renders DEFAULT values and cannot see a stale-deploy / overlay drop of
  the arm.
- `test-agent-cost-context.sh` — the beads-viewer cost/context **chips**; it
  writes `total_cost_usd` **by hand** via `kd` and never runs the close
  reconciliation.
- `test-usage-tracking.sh` — asserts `gb session analyze` **text**, never the
  coop usage NATS payload.

Unlike a live-cluster loop (a claudeless mock agent makes zero real LiteLLM
spend, so a live spawn→close never advances the counter, and the three halves
span three packages), this module — exactly like `test-pvc-gc-ttl.sh` — wraps
targeted `go test -run` against the REAL production code, exercised as an
integration flow over real HTTP (a `promhttp` `/metrics` scrape, a `beadsapi`
daemon round-trip) and a real embedded NATS server. It is a hermetic HARD gate
(no pods) and SKIPs LOUD only when the Go toolchain / controller source is
genuinely absent.

> **Topology note:** the task bead's "Proposed e2e" describes a NATS→counter
> forwarder (`spend_forwarder.go`) that was **deleted** in `9add5d50c`
> (ka-0000000717) — coop never emits `response_cost`, so it dropped 100% of
> events. The counter is now driven **solely** by `reconcileAtClose` at session
> close. This module asserts the live path, not the removed one.

### Quick run:

```bash
./tests/e2e/scripts/test-session-spend-attribution.sh
```

### Legs (each a HARD sub-gate):

1. **counter-reconcile** (`cmd/controller`, `session_spend_e2e_test.go`) —
   spawn→close→scrape: `captureBaseline` stashes the `/spend/tags` baseline
   ONTO the agent bead (born-at-0 counter series); `reconcileAtClose` reads that
   daemon-PERSISTED baseline BACK and settles the delta, writing
   `total_cost_usd` / `litellm_spend_close` / `litellm_spend_first_baseline`
   onto the bead (the **controller** writes it from the live `/spend/tags`
   delta — NOT a hand-set `kd` field). `gardenio_agent_spend_usd_total`, scraped
   over a REAL `promhttp` `/metrics` endpoint, advances by exactly the settled
   delta; a zero-cost close does NOT advance it; a respawner ACCUMULATES across
   sessions.
2. **usage-model** (`internal/bridge`, `hookstream_usage_contract_test.go`) —
   coop's EXACT usage payload (`coop.events.messages.<actor>.usage`), published
   through a REAL embedded NATS server + the REAL `HookStream` subscription,
   decodes with `model` present, renders the usage chunk, and carries NO cost (a
   stray `response_cost` never surfaces; the consumer struct has no cost field)
   — the strictly-cost-free contract (ka-0000002983).
3. **proxy-transparency** (`cmd/controller`, `session_spend_proxy_e2e_test.go`) —
   claudemore proxy transparency (ka-0000005743 / A1, ka-0000007533). Runs the
   SAME spawn→close→served-counter flow TWICE — once with NO `proxy_url`, once
   with a claudemore `proxy_url` attached to the **same account** — and asserts
   the settled fields (`total_cost_usd` / `litellm_spend_close` /
   `litellm_spend_first_baseline` / baseline) and the served counter are
   **byte-identical**, and a claudemore trap server is contacted **zero** times.
   The base URL each leg reads is resolved by the REAL production function
   `resolveSpendReadBaseURLForAccount` (which returns `base_url`, never
   `proxy_url`), so a "unify the read onto `proxy_url`" regression routes the
   proxied leg's read at the trap and reddens it. **This is the only spend module
   that attaches a proxy to an account** — the other four assert spend but never
   proxy, so the regression is invisible to them (born-vacuous, advice
   ka-0000001521). The mutation proof (route the read through `proxy_url`) reds
   this leg while every unproxied spend test stays green.

## Controller `:9092` Registry Tests (`test-controller-metrics.sh`)

Module name: `controller-metrics` (gd-ckUXI6EOld, epic ka-0000000958). Closes the
gap that **no e2e scraped the controller's own `:9092` Prometheus registry** —
`test-reconciler.sh` asserts only the controller's LOG-JSON `sync_runs`/`sync_errors`
(`kubectl logs | grep`), and `test-fleet-metrics.sh` asserts only the kbeads fleet
aggregate `gardenio_beads_*` on the DAEMON's `:9091` (a different process + registry).
Four controller-emitted families (commits `c806c368`, `3b6ec38e`) shipped with zero
e2e scrape coverage. This module port-forwards `svc/${NAMESPACE}-agents :9092` and
asserts them:

- **Tier 1 (HARD, always-on):** `gardenio_reconcile_total{controller,result}` — the
  reconcile loop's own counter runs continuously, so this series exists on EVERY live
  controller regardless of agent flow. Also asserts the `/metrics` endpoint returns
  valid Prometheus text. Reproduces the bead's acceptance #3 verbatim
  (`curl controller:9092/metrics | grep gardenio_reconcile_total`).
- **Tier 2 (after a spawn/stop cycle):** the three EVENT-DRIVEN families. The module
  drives a **spend-safe** seed→close cycle — it POSTs an agent bead carrying
  `image=registry.invalid/...` (the controller's SSE watcher maps `created`→AgentSpawn
  and `closed`→AgentDone; neither needs a Running pod, and the never-resolving image
  guarantees any pod can only `ImagePullBackOff` ⇒ **zero model spend**). It then
  asserts `gardenio_agent_lifecycle_events_total{event,env,project,role,session_id}`
  (event=spawned + despawned for our project) and
  `gardenio_agent_session_duration_seconds{project,role,outcome}` (outcome=done) appear
  with their documented labels. `gardenio_agent_pod_phase_transitions_total{phase_from,
  phase_to,project,role}` is asserted OBSERVATIONALLY — a witnessed Pending→Running
  transition needs a pod that actually starts, which the spend-safe drive deliberately
  never does, so a warm namespace asserts its label shape and a cold one legitimately
  has none.

Never flakes the release gate: Tier 1 is a hard gate; a Tier-2 family that the drive
does not surface within budget (cold/slow controller, SSE lag) is a non-fatal PENDING
by default and a hard FAIL only under `CONTROLLER_METRICS_STRICT=1` (same convention as
`test-pool-assign-attribution`'s `POOL_ATTRIBUTION_STRICT`; flip it in `run-shard.sh`
once a live run confirms the families populate reliably). Self-SKIPs (exit 0) when
kubectl/cluster access or the controller Service is absent. Hashes to shard 1 alongside
the warm-pool/controller modules.

### Quick run:

```bash
NAMESPACE=gardenio-e2e CONTROLLER_SVC=gardenio-e2e-agents \
  ./tests/e2e/scripts/test-controller-metrics.sh
```

## Controller Idle/Active Metrics (`test-idle-active-metrics.sh`)

Module name: `idle-active-metrics` (ka-0000003235, epic ka-0000000958). Closes the
HEAD-verified gap that **no e2e scraped the two Phase-0 idle/active gauges** that
`internal/reconciler/idle_metrics.go` emits (MR `6072815ec`, ka-0000006778) — coverage
was only the in-process classifier unit test (`idle_metrics_test.go`). The two series,
whose whole purpose is to be scraped into the idle-capacity/cost dashboard
(`docs/idle-active-measurement.md`):

- `gardenio_agents_by_activity{project,activity="idle"|"active"}` (gauge) — the idle:active
  ratio.
- `gardenio_agent_reserved_memory_bytes{project,activity}` (gauge) — the headline
  idle-memory-waste number the freeze/thaw substrate must beat.

A unit test over the classifier cannot catch a registration miss, a renamed label, a
namespace/subsystem typo, or the collector never being wired into the reconcile loop —
every one of which silently yields an **empty dashboard** rather than a failing test.
This module port-forwards `svc/${NAMESPACE}-agents :9092` and asserts against the served
registry. Distinct surface from `test-controller-metrics.sh` (the reconcile_total /
lifecycle / session_duration families on the SAME `:9092`) and `test-fleet-metrics.sh`
(kbeads `:9091`).

- **Feature-deployed gate:** the two gauges are NEW. A controller image built before the
  MR serves a valid `/metrics` with NEITHER family — so total absence of BOTH is treated
  as "feature not deployed here" and the module SKIPs (exit 0), the same convention
  `test-semantic-search.sh` uses to be safe to wire into CI before the stack ships.
  VERIFIED: a 5-day-old staging image exposes neither; the release image
  `2026.217.8` (prod `gardenio-system` and the fresh tag namespace) exposes both. Once
  EITHER family is emitted, every Tier-1 assertion below is HARD.
- **Tier 1 (HARD, always-on):** both gauges present + TYPEd `gauge` + carrying the
  documented `{project,activity}` labels with `activity ∈ {active,idle}` (the only enum
  `classifyActivity` emits). Plus two structural invariants that hold on any warm
  controller regardless of our drive: **pre-seed symmetry** (every project with an
  `active` series also has an `idle` one — the pre-seed `bucketFor` guarantees a
  well-defined idle/active division) and **value-join** (`reserved>0 ⇒ count>0` on the
  identical `{project,activity}`, and any non-zero reserved series is `≥ 1 MiB` so it is a
  byte count, not an agent/page count).
- **Tier 2 (after a spend-safe drive):** seeds an agent bead carrying
  `image=registry.invalid/...` (never-resolving TLD ⇒ ImagePullBackOff ⇒ **zero model
  spend**) for a throwaway project and asserts OUR project lands in
  `gardenio_agents_by_activity` with BOTH activity buckets pre-seeded (active=1 from the
  working seed) — the acceptance "activity label takes both idle and active values". It
  then asserts `reserved_memory>0` for our project and that it MOVES with a second seed —
  but this needs the controller to actually CREATE the (never-starting) pod, which a busy
  e2e namespace may DEFER when its per-project pod cap is saturated (VERIFIED: the tag
  namespace deferred our pod at `limit:4, active:4`). So the reserved-memory drive is a
  non-fatal PENDING by default and a hard FAIL only under `IDLE_METRICS_STRICT=1` (same
  convention as `controller-metrics`' `CONTROLLER_METRICS_STRICT`; flip it in
  `run-shard.sh` once a live run confirms it lands reliably). The `agents_by_activity`
  drive has no such dependency — it is driven from the desired-bead map, no pod required.

Never flakes the release gate: Tier 1 asserts only PROPERTIES true on any warm controller
that ships the feature; the pod-cap-dependent reserved-memory drive is PENDING unless
strict. Self-SKIPs (exit 0) when kubectl/cluster access or the controller Service is
absent, or when the feature is not deployed. Pinned to shard 2 beside `controller-metrics`
(same `:9092` scrape idiom).

### Quick run:

```bash
NAMESPACE=gardenio-e2e CONTROLLER_SVC=gardenio-e2e-agents BEADS_SVC=gardenio-e2e-beads \
  ./tests/e2e/scripts/test-idle-active-metrics.sh
```

## Semantic Search Tests (`test-semantic-search.sh`)

Exercises the pgvector + Ollama embedding stack end-to-end against a live kbeads
deployment that has `embedding.enabled=true`.

**Requires** `kd` binary built from a commit that ships `kd search --semantic`,
`kd embed sync`, and `kd similar`, plus a kbeads deployment with the Ollama
embedding sidecar.

When the feature is not yet deployed (endpoint missing or CLI flag absent) the
module emits SKIP and exits 0 — it is safe to wire into CI before the stack
lands; assertions activate automatically once the deployment flips on.

### Quick run:

```bash
NAMESPACE=gardenio-e2e KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-semantic-search.sh
```

### Scenarios covered:

1. **Feature probe** — POST `/v1/search/semantic` reachable, CLI has
   `--semantic` and `embed` — otherwise SKIP
2. **Seed varied-topic beads** — 6 beads across db / k8s / auth / frontend /
   infra / ML domains, lexically disjoint from the semantic query
3. **`kd embed sync`** — backfills embeddings; tolerates Ollama cold-start
   via retry
4. **Coverage** — `kd embed stats` reports non-zero coverage (best-effort:
   skipped with a note if the command is absent)
5. **`kd search --semantic`** — target bead is in top-3 for a query with no
   lexical overlap with the seed title
6. **POST `/v1/search/semantic`** — same assertion via raw HTTP
7. **`kd similar <bead-id>`** — returns ≥ 1 result other than the input bead

## Trigger System Tests (`test-triggers.sh`)

Tests `gb trigger` CRUD lifecycle against a live kbeads server.

**Requires** `gb` and `kd` binaries and a running kbeads server.

### Quick run:

```bash
GB_BIN=/tmp/gb KD_BIN=/tmp/kd \
  ./tests/e2e/scripts/test-triggers.sh
```

### Scenarios covered:

1. **Create webhook trigger** — with conditions, cooldown, max_concurrent
2. **Create JIRA trigger** — with JQL query and project
3. **Create GitLab pipeline trigger** — with project, status, ref filters
4. **Create GitLab MR trigger** — with state, target-branch, label filters
5. **List all triggers** — all created triggers appear
6. **List by source** — `--source webhook` filters correctly
7. **Disable trigger** — `gb trigger disable` sets enabled=false
8. **Enable trigger** — `gb trigger enable` restores enabled=true
9. **Verify bead fields** — type=trigger, kind=data, source-specific fields
10. **action=spawn_agent** — role and allowed_roles stored correctly
11. **action=apply_formula** — formula_id and vars stored correctly
12. **Close lifecycle** — closed trigger removed from active list

## Hermetic scripts (not wired into `run-shard.sh`)

Eleven scripts in `tests/e2e/scripts/` are intentionally **not** registered
as `run_module` calls in `run-shard.sh`. They are pure linters/unit tests
or standalone repros — no kubectl, no beads daemon, no network — and CI
runs them separately (or they ship as ad-hoc developer tools):

- `test-lib-retry.sh` — unit test for `lib-retry.sh`.
- `test-lib-gvisor-node.sh` — cluster-free unit test for `lib-gvisor-node.sh`
  (the gVisor-poc substrate warm-node gate + bounded signature-keyed retry,
  decision ka-0000000122). Run in CI by `checks.yml` script-selftests.
- `test-lib-gvisor-forensics.sh` — cluster-free unit test for
  `lib-gvisor-forensics.sh` (Rig R2, the freeze→thaw canary forensic
  auto-capture harness, bead ka-0000006948). Run in CI by `checks.yml`
  script-selftests.
- `test-shard-isolation.sh` — partition-invariant lint for `run-shard.sh`
  (described below).
- `test-shard-budget-guard.sh` — hermetic unit test for the budget-aware
  in-shard retry guard in `run-shard.sh` (ka-0000003834). Lifts
  `shard_seconds_remaining()`/`retry_fits_budget()` and drives them across
  the wall-clock budget boundary (no kubectl, no beads daemon). Run in CI by
  `checks.yml` script-selftests.
- `test-claudeless-scenario-isolated.sh` — standalone claudeless
  lifecycle repro (ka-0000002034); drives claudeless+coop in a local mock
  PTY (no kubectl, no beads daemon, no namespace).
- `test-claudeless-multi-nudge-isolated.sh` — companion multi-nudge
  diagnostic for the claudeless eventual-consistency path (ka-0000002034);
  standalone (no kubectl, no beads daemon).
- `test-concurrency-isolation.sh` — static `$NAMESPACE`-hygiene lint over
  the other `test-*.sh` scripts (concurrency-safety analogue of
  `test-shard-isolation.sh`; no kubectl, no beads daemon).
- `test-outbox-shape-selftest.sh` — hermetic, credless self-test of the
  1:1 GBFSM outbox CREATE-shape (ka-0000003147); sources
  `lib-outbox-shape.sh`, no kubectl/beads. Run in CI by `checks.yml`
  script-selftests.
- `test-lib-outbox-rig.sh` — cluster-free, credless unit test for
  `lib-outbox-rig.sh`'s `outbox_rig_guard` (ka-0000003147); no kubectl/beads.
  Run in CI by `checks.yml` script-selftests.
- `test-terraform-prewarm-cache.sh` — sanity check that the agent-image
  terraform provider prewarm (`images/agent/install/terraform-providers.sh`
  → `/opt/terraform-plugin-cache`) populates a non-empty, CONSUMABLE plugin
  cache (ka-0000002834); no kubectl/beads. `--contract-only` asserts the
  provider/version lockstep with `versions.env` credlessly on bare ubuntu;
  the full populate+consume proof (cache non-empty at the pinned versions, and
  terraform serving a provider "from the shared cache directory") runs against
  a freshly-built cache. No CI job runs it today (39s, pulls providers over the
  network).

The E2E direction gardener's drift heuristic
(`gardenio/docs/e2e-direction.md`) excludes all eleven from the script count.
Note the drift command globs `tests/e2e/scripts/test-*.sh` **and**
`tests/e2e/scripts/verify-*.sh` **and** `tests/e2e/edge-auth/test-*.sh`,
because `run-shard.sh` wires the `edge-auth` module from the `edge-auth/`
directory and the `outbox-rules-json` module from
`scripts/verify-outbox-rules-json.sh`. If you add another hermetic script,
update that doc, the run-shard.sh header, and this section together.

### Out-of-band cluster scripts (not wired, but not hermetic)

Six additional scripts are real cluster/proxy E2Es — they DO talk to
kubectl/beads or a live proxy — but are dispatched by a separate harness rather
than a `run_module` call here, so they are also excluded from the drift count:

- `test-gvisor-substrate-ready.sh`, `test-gvisor-freeze-interrupt.sh`,
  `test-gvisor-renudge-thaw.sh`, `test-gvisor-abort-races-completion.sh` —
  the gVisor suite. Run by hand, gated on `GVISOR_E2E=1` plus the
  `gvisor-poc` substrate node, and so cannot run in the generic sharded
  suite. No CI job dispatches them today.
- `test-pf-supervisor.sh` — port-forward supervisor check, run via the
  `test-pf-supervisor` target in `gardenio/Makefile`.
- `test-litellm-budget-cap.sh` — deliberate-runaway acceptance for the per-CI
  scoped LiteLLM virtual key (ka-0000000823, R4-COST-1/SEC-1). Needs a real
  LiteLLM proxy + `LITELLM_MASTER_KEY` and spends real (tiny) money, so it is
  run on demand against a live proxy, never in the credless sharded suite.

These are documented in the `run-shard.sh` header out-of-band block and
subtracted by the `e2e-direction.md` drift command. Wire one INTO a shard
or add another out-of-band cluster script → update all three places.

## Shard isolation invariant (`test-shard-isolation.sh`)

`run-shard.sh` partitions the canonical module list across `TOTAL_SHARDS`
parallel runners (CI fixes `TOTAL_SHARDS=4`). Heavy modules are pinned
explicitly; the long tail hashes via `cksum(name) % TOTAL_SHARDS`. The
critical correctness property is:

> **Every test module is assigned to exactly one shard.**

This matters because each module's real-time uploader writes to S3 under a
`COOP_S3_SESSION_KEY` derived from the agent bead ID. If two parallel shards
ran the same module concurrently, two uploaders would collide on the same S3
prefix and corrupt the resumable session output (see ka-0000003664 gap G9 →
gd-UHoecPuYe0).

`test-shard-isolation.sh` is a hermetic lint that:

1. Discovers every `run_module "<name>"` registration in `run-shard.sh`.
2. Asserts no module name is registered twice.
3. **Asserts exclusion-list completeness** (ka-0000004758): every
   `scripts/test-*.sh` on disk is either invoked by a `run_module` call OR
   named in the `run-shard.sh` header exclusion block (the bullets between
   the `shard-isolation-lint: exclusion-list BEGIN`/`END` sentinels). It also
   fails on a **stale** exclusion entry (a name in the block that no longer
   exists on disk) and on an inconsistency (a script both wired and listed as
   "not wired"). This replaces the old hand-maintained prose arithmetic — the
   guard that once let two hermetic scripts go 18 days undetected — with a
   mechanical check, so e2e wiring drift can no longer recur silently.
4. Lifts `shard_for()` from `run-shard.sh` and evaluates it in isolation —
   no kubectl, no beads daemon, no network.
5. For `TOTAL_SHARDS ∈ {4, 5, 8, 16}`, verifies every module maps to a
   shard in `[0, TOTAL_SHARDS)` and the union across all shards covers the
   full module set with no duplicates.

The minimum supported value is `TOTAL_SHARDS=4` because the explicit
`shard_for()` case statement pins heavy modules to shards 0..3.

```bash
./gardenio/tests/e2e/scripts/test-shard-isolation.sh
```

This lint runs in CI in `checks.yml` script-selftests, so a new unaccounted script or a stale
exclusion entry reds the introducing MR. Run it locally before any refactor
of `run-shard.sh`'s sharding logic, and any time a test module is added or a
hermetic/out-of-band script is added or removed.

## CI/CD

E2E tests run via `.github/workflows/e2e.yml` (manual dispatch, on main or a
branch, a nightly cron, and `agent/e2e-ci/**` pushes; the `e2e` PR label door
was removed by gd-19ECZxvFQw, as pull requests run the light tier only). The `shell-hermetic` job runs `run-shard.sh` with
`HERMETIC_ONLY=1` directly: there is no automatic retry and no flaky-module
quarantine (the old Captain wrapper that provided both was removed, gd-CgCBtAK7KQ),
so a flaky module fails the run until someone re-runs it, e.g. with
`ONLY=<module>[,<module>...]`.

Run locally with `make e2e` (requires port-forward or `BEADS_HTTP_URL` set).

## Deploying a gardenio-e2e namespace

```bash
helm upgrade --install gardenio-e2e helm/gardenio/ -n gardenio-e2e --create-namespace \
  --values helm/gardenio/values.yaml
```

CI uses `scripts/lane.sh install <ns>` against the leased pool namespaces
instead (see `lane/README.md`).

Port-forward for local testing:
```bash
kubectl -n gardenio-e2e port-forward svc/gardenio-e2e-beads 19090:8080
# Then: BEADS_HTTP_URL=http://localhost:19090 kd list
```
