# claudemore instant-rollback runbook

> **claudemore is test-lane only (2026-10-31, gd-Y4ZCGr2m2t, decision
> gd-rZJdGewUse).** The chart no longer ships a claudemore Deployment, Service or
> NetworkPolicy, and there is no claudemore image. The binary
> (`gardenio/controller/cmd/claudemore`) is built from source by the hermetic e2e
> replay modules (`gardenio/tests/e2e/scripts/test-claudemore-*.sh`) and nowhere
> else. Nothing in a cluster should point at claudemore. The procedure below is
> still the fix if an account bead has a stale `proxy_url`, because the
> controller's `proxy_url` resolution is unchanged; the "Rolling back the
> Deployment" section is history.

**Scope:** Phase 1 MVP (ka-0000007035, epic ka-0000005323). This is the
acceptance-A2 reversibility procedure: revert agents off claudemore and back to
litellm-direct within ~60s, with **no code change and no redeploy**.

## TL;DR

Rollback = **clear the `proxy_url` field on the account bead(s).** The next pod
spawn routes `ANTHROPIC_BASE_URL` straight at litellm again. Existing in-flight
pods are unaffected (they keep their baked-in env until they recycle).

## Background — what selects claudemore

claudemore is a thin Anthropic Messages-API reverse-proxy that **fronts (never
replaces) litellm**. Agents are pointed at it purely through an account-bead
field:

- `proxy_url` set → controller bakes `ANTHROPIC_BASE_URL=<proxy_url>` into new
  agent pods (traffic flows agent → claudemore → litellm → Anthropic).
- `proxy_url` empty → controller falls back to the account's `base_url`
  (litellm), then to the controller-level `LITELLM_URL` (legacy behavior).

The resolution order lives in `applyAccountConfig`
(`gardenio/controller/cmd/controller/podspec.go`): `proxy_url` → `base_url` →
`cfg.LiteLLMURL`. Because litellm still sees every request in LIVE mode, spend
tracking, virtual keys, model-alias routing, and OAuth forwarding are unchanged
(the cost fence — R5 synthesis §5.2). **claudemore meters no spend itself.**

## Rollback procedure (~60s)

1. **Identify the account bead(s) with `proxy_url` set:**

   ```bash
   kd list --type=account --json | \
     jq -r '.[] | select(.fields.proxy_url != null and .fields.proxy_url != "") | .id + "  " + .title'
   ```

2. **Clear `proxy_url` on each:**

   ```bash
   kd update <account-bead-id> --unset-field proxy_url
   ```

   (Or set it explicitly empty: `kd update <id> --field proxy_url= --allow-empty`;
   kd update refuses an empty value without `--allow-empty`.)

3. **Confirm the controller picked up the change.** The controller refreshes its
   account cache on the bead-update SSE event and on its periodic resync (well
   under 60s). New pods spawned after the refresh route straight to litellm.

4. **(Optional) Force-recycle live pods** if you need the rollback to take
   effect immediately rather than on natural pod churn:

   ```bash
   kubectl -n gardenio delete pod -l app.kubernetes.io/component=agent --field-selector status.phase=Running
   ```

   Only do this if an active incident requires every pod off claudemore *now*;
   otherwise let pods recycle naturally — `proxy_url` is already cleared for the
   next spawn.

## Verify rollback

- Spawn (or wait for) a fresh agent pod and confirm its env:

  ```bash
  kubectl -n gardenio get pod <agent-pod> -o jsonpath='{.spec.containers[0].env}' | \
    tr ',' '\n' | grep -A1 ANTHROPIC_BASE_URL
  ```

  It should point at the litellm Service (e.g. `http://gardenio-litellm:4000`),
  **not** the claudemore Service.

- litellm `/spend/tags` continues to attribute spend per `agent:<name>` exactly
  as before — there is no spend gap, because claudemore never metered.

## Why this is safe

- **No code change, no image rebuild, no Helm release.** The rollback is a single
  bead-field edit; the controller's existing account-refresh loop does the rest.
- **No spend discontinuity.** litellm is the cost source of truth in both
  states; claudemore is a pass-through in front of it, never a replacement.
- **There is no forward path in-cluster any more.** Setting `proxy_url` to a
  claudemore Service URL needs a claudemore Deployment, which the chart no longer
  ships (gd-Y4ZCGr2m2t). Bringing one back is a new decision, not an edit.

## Rolling back the Deployment too (history)

Until gd-Y4ZCGr2m2t the `claudemore` Deployment was gated behind
`claudemore.enabled` in the Helm chart (default `false`, never on in k3s). That
template, the `claudemore` values block (including `isolation` and `recording`)
and its NetworkPolicies were removed on 2026-10-31, so there is no Deployment to
roll back.
