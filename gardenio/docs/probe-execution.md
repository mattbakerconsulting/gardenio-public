# Probe execution target (`execution_target=probe`)

**Plan:** Probe pods (epic `ka-0000000544`). **Impl task:** `ka-0000002769`.

A **probe** is a one-off *scripted pre-check* that runs a shell script in a bare
Kubernetes pod and reports its result purely by **exit code** — exit `0` closes
the bead **done**, non-zero marks it **failed**. It is the cheap "did anything
change?" primitive meant to replace a full **no-op gardener inference**: instead
of spawning a 2 CPU / 8 Gi coop agent pod that reasons for ~9 turns only to
conclude "nothing to do", a probe runs a few `git ls-remote` + bead reads in a
50m / 128Mi pod and exits.

This is the **primitive**, shipped **unwired** — no trigger fires it. A gardener
lane opts in explicitly (that gating is a later increment of the plan, tracked
under the scope decision `ka-0000001284`).

## Table of contents

- [How it differs from an agent pod](#how-it-differs-from-an-agent-pod)
- [The `type=script` bead](#the-typescript-bead)
- [Running a probe](#running-a-probe)
- [Where it plugs into the controller](#where-it-plugs-into-the-controller)
- [Example](#example)

## How it differs from an agent pod

`execution_target` selects the runtime backend that realizes a unit of work
(`internal/podmanager/manager.go`). There are now three:

| target | backend | shape |
|---|---|---|
| `kubernetes` (default, or empty) | `CreateAgentPod` | full coop agent pod (3 probed ports, entrypoint launches coop, 2 CPU / 8 Gi) |
| `lambda_microvm` | AWS Lambda MicroVM | epic `ka-0000005010` |
| `probe` | `BuildProbePod` | bare `/bin/sh -lc "$script"` pod, 50m / 128Mi, no coop, no OAuth |

`probe` is deliberately **NOT** a Kubernetes target
(`podmanager.IsKubernetesTarget` returns `false` for it) even though it realizes
as a pod: it is a *different* pod, so the controller spawn door routes it away
from `CreateAgentPod`. A probe pod:

- runs `/bin/sh -lc "$script"` — the agent image entrypoint (which would launch
  coop) is overridden, so none of the coop ports are ever served;
- carries **no** readiness/liveness/startup probes (a plain script never serves
  those ports — a probe on a never-listening port would fail the pod);
- carries **no** `CLAUDE_CODE_OAUTH_TOKEN` and **no** `ANTHROPIC_*` env — it runs
  a script, not Claude;
- runs `RestartPolicy: Never` with a bounded `activeDeadlineSeconds` (default
  120s), so it runs exactly once to a terminal phase;
- is labelled `gardenio.io/probe=true` (**not** `gardenio.io/agent`), so the
  reconciler's desired-vs-actual agent sweep leaves it alone — the CLI owns its
  lifecycle, exactly as the poolmanager owns warm placeholders.

It reuses the fleet **agent image** only so the script has the standard
toolchain (`git`, `jq`, `kd`, `gb`, `glab`) on `PATH`.

## The `type=script` bead

A probe's payload is a `type=script` bead (`kind=data`, declared in
`kbeads/internal/server/config.go`):

| field | type | meaning |
|---|---|---|
| `script` | string, **required** | shell body run as `/bin/sh -lc "$script"` |
| `activity_deadline_seconds` | integer | pod `activeDeadlineSeconds` (default 120) |
| `exit_code` | integer | stamped by the runner at completion |
| `close_reason` | string | stamped on a failed probe (`probe exit N: …`) |
| `execution_target` | string | conventionally `probe` |
| `project` | string | owning project → the probe pod's `gardenio.io/project` label |

## Running a probe

```sh
gb probe run <script-bead-id>
```

`gb probe run` reads the `type=script` bead, builds the probe pod
(`podmanager.BuildProbePod`), creates it in the agent namespace, waits for it to
reach a terminal phase, reads the probe container's exit code, and closes the
bead:

- **exit 0** → `agent_state=done`, bead closed, `exit_code=0`;
- **non-zero** (or the deadline / an unschedulable pod) → `agent_state=failed`,
  `close_reason` set, bead closed, `exit_code=N`;
- `gb`'s own process exit code mirrors the probe, so a caller can branch on it.

A harness failure (couldn't create/observe the pod, or a timeout) is *not*
treated as a probe failure — the bead is left open for a retry and the error is
surfaced, so a broken probe never silently closes the bead.

Flags: `--image` (defaults to `$COOP_IMAGE` / `$GARDENIO_AGENT_IMAGE`),
`--namespace` (defaults to `$POD_NAMESPACE` / `$NAMESPACE` / `gardenio`),
`--timeout`, `--poll-interval`, `--keep-pod`.

## Where it plugs into the controller

- **Schema** — `type:script` + the `probe` value are recognised in
  `kbeads/internal/server/config.go`; `resolveExecutionTarget`
  (`cmd/controller/podspec.go`) normalises `probe` as a known target.
- **Spawn door** — an *agent* bead that resolves to `probe` fails closed in
  `handleNonK8sSpawn` (`cmd/controller/main.go`) with a pointer to `gb probe
  run`, because a probe is CLI-driven, not spawned through the async agent door.
- **Exit-code mapping** — `probeAgentState` (`cmd/gb/probe.go`) mirrors
  `statusreporter.PhaseToAgentState`: `PodSucceeded`→done, `PodFailed`→failed.

## Example

Create a probe that fails when `main` has moved since a recorded SHA (the shape
of the release-owner "should I even run?" pre-check), then run it:

```sh
# 1. Create the script bead.
kd create "probe: has main moved since last release?" \
  --type script -l project:gardenio \
  -f script='LAST=$(kd show ka-LASTREL --json | jq -r .fields.released_sha);
             HEAD=$(git ls-remote origin main | cut -f1);
             test "$LAST" = "$HEAD"'   # exit 0 = unchanged (no-op); exit 1 = moved
# → prints the new bead id, e.g. ka-0000001713

# 2. Run it. Exit 0 closes it done ("nothing changed, skip the gardener");
#    exit 1 closes it failed ("main moved — a real run is warranted").
gb probe run ka-0000001713
```

The unit tests exercising this flow live in
`gardenio/controller/cmd/gb/probe_test.go`,
`gardenio/controller/internal/podmanager/probe_test.go`, and
`kbeads/internal/server/script_type_test.go`.
