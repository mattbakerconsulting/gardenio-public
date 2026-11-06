#!/usr/bin/env bash
# test-airgap-image-distribution.sh — E2E gate for the P4.3 air-gap image-
# distribution knobs: global.airgap and the chart wiring it drives.
#
# Bead ka-0000007260 (parent ka-0000000958). Feature commit f6745c05 (P4.3 close
# 4 air-gap blockers). Source: gardenio/docs/research/inbox-image-distribution.md
# (ka-0000005069, ka-0000007177).
#
# ── THE GAP THIS CLOSES ───────────────────────────────────────────────────────
#
# Gardenio-in-a-Box (the portable single-EC2 product) pulls ZERO images from any
# external registry at runtime — the in-box zot registry + containerd mirror
# serve every pull. Four chart knobs make that safe, all flipped by
# `global.airgap` (values-inbox.yaml sets airgap=true):
#
#   1 AGENT_PULL_POLICY=IfNotPresent  reuse a seeded agent image with no registry
#                                     round-trip on every spawn.
#   2 DIND_IMAGE override             pin a DinD ref the in-box mirror resolves
#                                     (the DinD image was a hardcoded Go constant
#                                     with no override path before P4.3).
#   3 IMAGE_PULL_SECRETS dropped      the in-box registry needs no pull creds; a
#                                     stale secret would trigger a pull against
#                                     the DEAD GitLab registry.
#   4 latestMode FORCED off           THE HEADLINE. latestMode floats the "latest"
#                                     tag, forces ImagePullPolicy: Always, AND
#                                     bypasses the pinned digest — any one of which
#                                     SILENTLY DEFEATS a digest-pinned air-gap
#                                     bundle. global.airgap hard-disables latestMode
#                                     everywhere image refs resolve
#                                     (gardenio.latestMode helper), so a pinned
#                                     digest SURVIVES.
#
# That runtime shipped with Go UNIT coverage only
# (podmanager/airgap_overrides_test.go pins buildContainer/buildDindSidecar once
# the override is ALREADY set on the manager struct; helm/gardenio/tests/
# box_auth_profile_test.go covers a different, edge-auth, profile). A grep of
# gardenio/tests/e2e for airgap / AGENT_PULL_POLICY / DIND_IMAGE returned empty
# (test-sidecar-injection.sh only incidentally asserts the DEFAULT docker:dind).
# So the RENDER→WIRING contract — that the chart actually EMITS the right env
# under global.airgap, DROPS the pull secret, and PRESERVES a pinned digest — was
# asserted against nothing. A regression in _helpers.tpl or controller/
# deployment.yaml (e.g. a latestMode helper that stops consulting airgap) would
# ship a box that silently floats `latest` over its pinned bundle, and no unit
# test would catch it because none of them render the chart.
#
# ── WHY A HERMETIC HELM-RENDER PROBE (no cluster) ─────────────────────────────
#
# global.airgap is NEVER true in any e2e namespace — the ephemeral + staging
# singletons run the cloud profile (airgap=false), and ONLY the portable Gasbox
# product flips it. So there is no live deployment to assert against; the knobs
# live entirely at Helm RENDER time (env vars on the controller Deployment; the
# imageRef/imageTag/imagePullPolicy helpers). This module therefore renders the
# REAL chart with `helm template` and asserts the emitted manifests directly —
# the same from-source/hermetic shape as test-edge-auth-box-auth.sh
# (ka-0000005667): it needs no kubectl, no beads, no tunnel, and runs GREEN in
# every shard container that carries the `helm` binary + the chart on disk.
#
# ── Legs (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   1 pull-policy-airgap    airgap=true  → controller Deployment env
#                           AGENT_PULL_POLICY=IfNotPresent.
#   2 pull-policy-control    airgap=false → NO AGENT_PULL_POLICY env (KNOWN-GOOD
#                           CONTROL: the controller keeps its PullAlways default,
#                           proving leg 1's value is airgap-driven, not hardcoded).
#   3 dind-override          agents.dindImage set → controller Deployment env
#                           DIND_IMAGE carries the override verbatim.
#   4 pull-secret-dropped    airgap=true + imagePullSecrets set → NO
#                           IMAGE_PULL_SECRETS env on the controller Deployment.
#   5 pull-secret-control    airgap=false + SAME imagePullSecrets → IMAGE_PULL_SECRETS
#                           IS emitted (KNOWN-GOOD CONTROL: proves leg 4's absence
#                           is the airgap drop, not a secret that was never set).
#   6 digest-survives-airgap HEADLINE. airgap=true + latestMode=true + pinned
#                           digest → the beads image ref KEEPS its @sha256 digest
#                           (airgap forced latestMode off).
#   7 digest-bypassed-control KNOWN-GOOD CONTROL. airgap=false + latestMode=true +
#                           SAME digest → the beads image ref is `:latest` with NO
#                           digest (latestMode silently defeated the pin). This is
#                           the exact failure global.airgap prevents; asserting it
#                           reproduces here proves leg 6's digest-survival is real
#                           coverage and not a chart that never floats latest.
#
# ── ADDITIONAL SEAM: DEFAULT_ACCOUNT removed (legs 8+9, ka-0000007959) ─────────
# A controller-Deployment render gate hosted here because this is the only
# helm-render module that renders templates/controller/deployment.yaml (its
# render_controller/env_value/env_present helpers apply verbatim). The
# agents.defaultAccount → DEFAULT_ACCOUNT env seam and the cfg.DefaultAccount
# terminal fallback it fed were REMOVED in account-routing.md §7 Phase 3
# (ka-0000007959): the value was unset in prod and Phase 0 telemetry proved the
# layer never won. These legs now assert the removal is complete — the chart must
# never emit DEFAULT_ACCOUNT, even when a stray agents.defaultAccount is passed:
#   8 default-account-set-noop  agents.defaultAccount=<probe> → controller
#                           Deployment emits NO DEFAULT_ACCOUNT env (the value key
#                           is gone from values.yaml and the deployment block is
#                           deleted, so a stray override is inert).
#   9 default-account-control   agents.defaultAccount unset → NO DEFAULT_ACCOUNT
#                           env — the ordinary render is clean.
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The `helm` binary + the chart at gardenio/helm/gardenio are present in every e2e
# shard container. If `helm` is absent (or the chart cannot be located) the
# module SKIPS CLEANLY (exit 0) with a LOUD banner — an absent toolchain must
# never red the suite, mirroring the edge-auth-box-auth skip contract. When both
# are present, every leg is a HARD assertion.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> helm/gardenio
CHART_DIR="${AIRGAP_CHART_DIR:-}"
if [ -z "$CHART_DIR" ]; then
  # Resolve the chart relative to this script; empty (skip-clean) if it moved.
  CHART_DIR="$(cd "$SCRIPT_DIR/../../../helm/gardenio" 2>/dev/null && pwd)" || CHART_DIR=""
fi
HELM_BIN="${HELM_BIN:-helm}"
# A syntactically-valid but obviously-fake digest so the ref is unambiguous in
# assertions and can never collide with a real published digest.
PIN_DIGEST="sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
# A distinctive default-account probe (legs 8+9, ka-0000007959). Passed as a
# stray agents.defaultAccount override to prove the removed seam is inert (the
# chart must not resurrect a DEFAULT_ACCOUNT env from it).
DEFAULT_ACCOUNT_PROBE="e2e-defaultaccount-probe"
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

# render_controller_env <airgap> <extra-set...> — render ONLY the controller
# (agents) Deployment. agents.enabled=true is required or the whole Deployment is
# gated out (.Values.agents.enabled defaults false for cloud installs).
render_controller() {
  local airgap="$1"; shift
  "$HELM_BIN" template gb "$CHART_DIR" \
    --set "global.airgap=${airgap}" \
    --set agents.enabled=true \
    "$@" \
    -s templates/controller/deployment.yaml 2>/dev/null
}

# render_beads <airgap> <latestMode> — render ONLY the beads Deployment (it
# resolves its image via the gardenio.imageRef helper, the digest-preservation
# surface) with a pinned digest.
render_beads() {
  local airgap="$1" latest="$2"
  "$HELM_BIN" template gb "$CHART_DIR" \
    --set "global.airgap=${airgap}" \
    --set "global.latestMode=${latest}" \
    --set "beads.image.digest=${PIN_DIGEST}" \
    -s templates/beads/deployment.yaml 2>/dev/null
}

# env_value <manifest> <ENV_NAME> — echo the .value of a container env entry, or
# empty if the env var is absent. Parses the rendered YAML with awk (the env
# block is `- name: X` / `  value: Y` pairs); trims surrounding quotes.
env_value() {
  local manifest="$1" want="$2"
  printf '%s\n' "$manifest" | awk -v want="$want" '
    /- name:/   { n=$3 }
    n==want && /value:/ {
      sub(/^[[:space:]]*value:[[:space:]]*/, "")
      gsub(/^"|"$/, "")
      print
      exit
    }'
}

# env_present <manifest> <ENV_NAME> — return 0 iff an env entry with that name
# exists (regardless of value).
env_present() {
  grep -qE "^[[:space:]]*- name: $2$" <<<"$1"
}

# beads_image <manifest> — echo the beads container image ref (first `image:`
# line under the beads Deployment), quotes trimmed.
beads_image() {
  printf '%s\n' "$1" | awk '
    /image:/ {
      sub(/^[[:space:]]*image:[[:space:]]*/, "")
      gsub(/^"|"$/, "")
      print
      exit
    }'
}

# ── Preflight: skip cleanly if helm or the chart is unavailable ──────
blue "╔══════════════════════════════════════════════════════╗"
blue "║  Air-gap image-distribution E2E (P4.3, ka-0000007260) ║"
blue "╚══════════════════════════════════════════════════════╝"
echo ""

if ! command -v "$HELM_BIN" >/dev/null 2>&1; then
  blue "SKIP: '$HELM_BIN' not found on PATH."
  blue "  The air-gap knobs live at Helm render time; without helm there is"
  blue "  nothing to render. Skipping cleanly (exit 0) — an absent toolchain"
  blue "  must never red the suite (see edge-auth-box-auth skip contract)."
  exit 0
fi
if [ -z "$CHART_DIR" ] || [ ! -f "$CHART_DIR/Chart.yaml" ]; then
  blue "SKIP: gardenio helm chart not found (looked at '${CHART_DIR:-<unresolved>}')."
  blue "  Skipping cleanly (exit 0)."
  exit 0
fi
dim "  helm:  $(command -v "$HELM_BIN")  ($("$HELM_BIN" version --short 2>/dev/null || echo '?'))"
dim "  chart: $CHART_DIR"
echo ""

# Render the manifests once per scenario, up front, so a render error surfaces
# as a hard failure (not a silently-empty assertion).
blue "Rendering chart scenarios..."
CTRL_AIRGAP=$(render_controller true \
  --set agents.dindImage='airgap-registry.local/docker:dind-pinned' \
  --set 'imagePullSecrets[0].name=stale-gitlab-regcred')
CTRL_CLOUD=$(render_controller false \
  --set agents.dindImage='airgap-registry.local/docker:dind-pinned' \
  --set 'imagePullSecrets[0].name=stale-gitlab-regcred')
BEADS_AIRGAP=$(render_beads true true)
BEADS_CLOUD=$(render_beads false true)
# Default-account removal scenarios (legs 8+9, ka-0000007959). airgap is
# irrelevant to this knob, so render under the cloud profile. Since the value key
# and the deployment block are deleted, a stray override is inert and the
# ordinary render is clean — both must emit NO DEFAULT_ACCOUNT env.
CTRL_DA_SET=$(render_controller false --set "agents.defaultAccount=${DEFAULT_ACCOUNT_PROBE}")
CTRL_DA_EMPTY=$(render_controller false)

# Guard: every render must have produced a real controller/beads Deployment.
# An empty render (chart moved, template renamed) would otherwise turn every
# leg into a false PASS-by-absence. Fail LOUDLY instead.
for _pair in "controller(airgap):$CTRL_AIRGAP" "controller(cloud):$CTRL_CLOUD" \
             "controller(defaultAccount-set):$CTRL_DA_SET" \
             "controller(defaultAccount-empty):$CTRL_DA_EMPTY" \
             "beads(airgap):$BEADS_AIRGAP" "beads(cloud):$BEADS_CLOUD"; do
  _label="${_pair%%:*}"; _body="${_pair#*:}"
  if ! grep -q "kind: Deployment" <<<"$_body"; then
    fail "render sanity: $_label produced a Deployment" \
         "helm template returned no Deployment manifest — chart layout drifted?"
  fi
done
if [ "$FAIL" -ne 0 ]; then
  blue "══════════════════════════════════════════════"
  red "  Render sanity failed — aborting before assertions"
  blue "══════════════════════════════════════════════"
  exit 1
fi
green "  ✓ all six scenarios rendered a controller/beads Deployment"
echo ""

# ── Leg 1: pull policy under airgap ─────────────────────────────────
blue "1. AGENT_PULL_POLICY under global.airgap=true..."
APP_AIRGAP=$(env_value "$CTRL_AIRGAP" AGENT_PULL_POLICY)
if [ "$APP_AIRGAP" = "IfNotPresent" ]; then
  pass "airgap=true → AGENT_PULL_POLICY=IfNotPresent"
else
  fail "airgap=true → AGENT_PULL_POLICY=IfNotPresent" \
       "got '${APP_AIRGAP:-<absent>}'"
fi

# ── Leg 2: control — no pull policy override under cloud ─────────────
blue "2. CONTROL: AGENT_PULL_POLICY absent under global.airgap=false..."
if env_present "$CTRL_CLOUD" AGENT_PULL_POLICY; then
  fail "airgap=false → AGENT_PULL_POLICY absent (control)" \
       "env present with value '$(env_value "$CTRL_CLOUD" AGENT_PULL_POLICY)'; expected the controller's PullAlways default (no env)"
else
  pass "airgap=false → no AGENT_PULL_POLICY env (controller keeps PullAlways default)"
fi

# ── Leg 3: DIND_IMAGE override honored ──────────────────────────────
blue "3. DIND_IMAGE override honored..."
DIND=$(env_value "$CTRL_AIRGAP" DIND_IMAGE)
if [ "$DIND" = "airgap-registry.local/docker:dind-pinned" ]; then
  pass "agents.dindImage → DIND_IMAGE=airgap-registry.local/docker:dind-pinned"
else
  fail "DIND_IMAGE override honored" \
       "got '${DIND:-<absent>}', want 'airgap-registry.local/docker:dind-pinned'"
fi

# ── Leg 4: IMAGE_PULL_SECRETS dropped under airgap ──────────────────
blue "4. IMAGE_PULL_SECRETS dropped under global.airgap=true..."
if env_present "$CTRL_AIRGAP" IMAGE_PULL_SECRETS; then
  fail "airgap=true → IMAGE_PULL_SECRETS dropped" \
       "env still present ('$(env_value "$CTRL_AIRGAP" IMAGE_PULL_SECRETS)'); a stale secret would pull against the dead GitLab registry"
else
  pass "airgap=true → IMAGE_PULL_SECRETS env dropped (in-box registry needs no creds)"
fi

# ── Leg 5: control — IMAGE_PULL_SECRETS emitted under cloud ─────────
blue "5. CONTROL: IMAGE_PULL_SECRETS emitted under global.airgap=false..."
IPS_CLOUD=$(env_value "$CTRL_CLOUD" IMAGE_PULL_SECRETS)
if [ "$IPS_CLOUD" = "stale-gitlab-regcred" ]; then
  pass "airgap=false → IMAGE_PULL_SECRETS=stale-gitlab-regcred (proves leg 4 is the airgap drop)"
else
  fail "airgap=false → IMAGE_PULL_SECRETS emitted (control)" \
       "got '${IPS_CLOUD:-<absent>}', want 'stale-gitlab-regcred' — control invalid, leg 4 proves nothing"
fi

# ── Leg 6: HEADLINE — pinned digest survives under airgap ───────────
blue "6. HEADLINE: pinned digest survives (airgap forces latestMode OFF)..."
IMG_AIRGAP=$(beads_image "$BEADS_AIRGAP")
if grep -qF "@$PIN_DIGEST" <<<"$IMG_AIRGAP"; then
  pass "airgap=true + latestMode=true → beads ref KEEPS @sha256 digest"
  dim "    $IMG_AIRGAP"
else
  fail "HEADLINE: pinned digest survives under airgap" \
       "beads ref '$IMG_AIRGAP' lost its digest — latestMode was NOT forced off, the air-gap bundle pin is silently defeated"
fi

# ── Leg 7: control — latestMode defeats the digest under cloud ──────
blue "7. CONTROL: latestMode defeats the pinned digest under global.airgap=false..."
IMG_CLOUD=$(beads_image "$BEADS_CLOUD")
if grep -qF "@$PIN_DIGEST" <<<"$IMG_CLOUD"; then
  fail "airgap=false + latestMode=true → digest bypassed (control)" \
       "beads ref '$IMG_CLOUD' still carries the digest; latestMode did NOT float latest — control invalid, so leg 6 proves nothing"
elif grep -qE ':latest("|$)' <<<"$IMG_CLOUD"; then
  pass "airgap=false + latestMode=true → beads ref is :latest, digest bypassed (the failure airgap prevents)"
  dim "    $IMG_CLOUD"
else
  fail "airgap=false + latestMode=true → digest bypassed (control)" \
       "beads ref '$IMG_CLOUD' is neither digest-pinned nor :latest — unexpected; control inconclusive"
fi

# ══ DEFAULT_ACCOUNT removal (legs 8+9, ka-0000007959) ══════════════
#
# account-routing.md §7 Phase 3 (ka-0000007959) removed the cluster-wide terminal
# account fallback end to end: the agents.defaultAccount value key (values.yaml),
# the DEFAULT_ACCOUNT env block (deployment.yaml), the os.Getenv read
# (config.go), and cfg.DefaultAccount + its podspec fall-through are all deleted.
# The value was unset in prod and Phase 0 telemetry proved the layer never won;
# an empty spread pool now blocks the spawn (transient, reconciler-retried)
# rather than baking a last-resort credential. These legs assert the removal is
# complete: the chart must NEVER emit a DEFAULT_ACCOUNT env, whether or not a
# stray agents.defaultAccount override is passed. This replaces the former
# helm→env seam legs (ka-0000001890) that this Phase deliberately deletes.

# ── Leg 8: stray agents.defaultAccount override is INERT — no env ──
blue "8. agents.defaultAccount override → controller emits NO DEFAULT_ACCOUNT env (seam removed, ka-0000007959)..."
if env_present "$CTRL_DA_SET" DEFAULT_ACCOUNT; then
  fail "agents.defaultAccount override → DEFAULT_ACCOUNT absent (removed seam)" \
       "env present with value '$(env_value "$CTRL_DA_SET" DEFAULT_ACCOUNT)'; Phase 3 (ka-0000007959) deleted the value key and the deployment.yaml DEFAULT_ACCOUNT block, so a stray override must be inert. A resurrected env means the removal is incomplete."
else
  pass "agents.defaultAccount override is inert → no DEFAULT_ACCOUNT env (value key + deployment block deleted, ka-0000007959)"
fi

# ── Leg 9: ordinary render emits NO DEFAULT_ACCOUNT env ──
blue "9. CONTROL: ordinary render → NO DEFAULT_ACCOUNT env..."
if env_present "$CTRL_DA_EMPTY" DEFAULT_ACCOUNT; then
  fail "ordinary render → DEFAULT_ACCOUNT absent (control)" \
       "env present with value '$(env_value "$CTRL_DA_EMPTY" DEFAULT_ACCOUNT)'; the deployment.yaml DEFAULT_ACCOUNT block was deleted in Phase 3 (ka-0000007959), so a default render must never carry it."
else
  pass "ordinary render → no DEFAULT_ACCOUNT env (deployment block deleted, ka-0000007959)"
fi

# ── Summary ──────────────────────────────────────────────────────
echo ""
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS air-gap image-distribution assertions passed"
else
  red "  $PASS passed, $FAIL failed"
  echo ""
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
fi
blue "══════════════════════════════════════════════"

exit "$FAIL"
