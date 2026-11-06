#!/usr/bin/env bash
# test-migration-agent-opens-mr.sh — P4.4 migration acceptance smoke:
# a REAL agent spawned on the new prod cluster creates a merge request in the
# gardenio repo end-to-end.
#
# Bead: ka-0000002701  (epic ka-0000003481 → Phase-4 migration ka-0000002205)
#
# WHY this is the deepest check: it exercises the FULL post-cutover loop that no
# other smoke test touches — controller schedules a pod → agent image boots →
# coopmux brokers the session → the agent talks to the kbeads daemon (claim a
# bead) → pushes a branch to GitLab → `glab mr create` → `gb mr link` writes the
# MR back onto the task bead. If any hop in controller + coopmux + daemon +
# GitLab is mis-wired after the cut, this fails where the shallower checks pass.
#
# HOW success is detected: we poll the pre-assigned TASK bead's `mr_url` field.
# `gb mr link <bead> <url>` sets that field synchronously via the daemon
# (controller/cmd/gb/mr.go), so a non-empty, well-formed GitLab MR URL on the
# task bead is a race-free, bridge-independent proof the agent completed the
# loop. (The gitlab-bridge's produces edge is a stronger signal but requires the
# bridge deployed, which it is not in staging — so mr_url is the primary gate.)
#
# SAFETY / opt-in: spawning a REAL Claude agent costs minutes of wall-clock and
# API tokens and requires a GitLab push token, so this module is OFF by default.
# It runs ONLY when AGENT_MR_E2E=1. Otherwise it SKIPS LOUDLY. It must NEVER be
# added to the fast main-push SMOKE subset. The spawned agent is instructed to
# work a THROWAWAY branch and open the MR as a Draft — it never pushes to main
# and never merges. This is a normal agent→MR flow, not an infra/data write, so
# it needs no operator decision (program §8).
#
# Self-SKIPs LOUDLY when: AGENT_MR_E2E != 1, the gb binary is absent, the
# controller Deployment is absent (no pod scheduler → no real agent), or no
# GitLab push token is configured for the target project.
#
# Runnable against prod.gardenio.acme.example by pointing kubectl at the prod cluster,
# exporting NAMESPACE=<prod ns> and AGENT_MR_E2E=1 during a cutover-acceptance
# window (see gardenio/docs/migration-acceptance-smoke.md).
#
# Usage:
#   AGENT_MR_E2E=1 NAMESPACE=gardenio-e2e \
#     ./gardenio/tests/e2e/scripts/test-migration-agent-opens-mr.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh" 2>/dev/null || true

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19832}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
# The project + repo the spawned agent should open its MR against.
MR_PROJECT="${MR_PROJECT:-gardenio}"
MR_AGENT_ROLE="${MR_AGENT_ROLE:-crew}"
# How long to wait for the agent to complete claim→branch→push→MR→mr link.
# A real Claude agent doing a trivial no-op MR typically finishes in a few
# minutes; default 20 min with generous headroom.
MR_WAIT_SECS="${MR_WAIT_SECS:-1200}"
MR_POLL_INTERVAL="${MR_POLL_INTERVAL:-20}"
PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()
SPAWNED_AGENTS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  # Best-effort: ask each spawned agent to stop and close the beads we created.
  for a in "${SPAWNED_AGENTS[@]:-}"; do
    [ -n "$a" ] && "$KD_BIN" update "$a" -f "stop_requested=\"true\"" >/dev/null 2>&1 || true
    [ -n "$a" ] && "$KD_BIN" close "$a" >/dev/null 2>&1 || true
  done
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/migration-agent-mr-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

# bead_field <bead-id> <field> — read a bead field via the daemon (retry-safe).
bead_field() {
  local val
  val=$("$KD_BIN" show "$1" --json 2>/dev/null | jq -r ".fields.\"$2\" // empty" 2>/dev/null || true)
  echo "$val"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " migration-agent-opens-mr smoke (ka-0000002701)"
  blue " namespace=$NAMESPACE project=$MR_PROJECT"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Opt-in gate: real agent spawns are expensive — off by default ──
  if [ "${AGENT_MR_E2E:-}" != "1" ]; then
    skip "agent-opens-MR smoke" "AGENT_MR_E2E != 1 (real Claude agent spawn is opt-in; set AGENT_MR_E2E=1 for a cutover-acceptance run)"
    summary
    return 0
  fi

  # ── Precondition: gb binary present ─────────────────────────────
  if ! command -v "$GB_BIN" >/dev/null 2>&1; then
    skip "agent-opens-MR smoke" "gb binary not found in PATH"
    summary
    return 0
  fi

  # ── Precondition: controller present (else no pod scheduler) ────
  if ! kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-controller" >/dev/null 2>&1 \
     && ! kubectl -n "$NAMESPACE" get deploy -l app.kubernetes.io/component=controller >/dev/null 2>&1; then
    skip "agent-opens-MR smoke" "controller Deployment absent in $NAMESPACE — no pod scheduler to run a real agent"
    summary
    return 0
  fi

  setup_beads_portforward

  # ── Precondition: a GitLab push token is configured for the project ─
  # The agent pod gets GITLAB_TOKEN from the project bead's `secrets` field
  # (podmanager injects it; initclone writes the oauth2 credential line). If
  # the project has none, `git push` / `glab mr create` fail — skip rather
  # than spawn an agent that cannot possibly open an MR.
  local proj_secrets
  proj_secrets=$("$KD_BIN" show "project:${MR_PROJECT}" --json 2>/dev/null \
    | jq -r '.fields.secrets // empty' 2>/dev/null || true)
  if [ -z "$proj_secrets" ] || ! grep -qiE 'gitlab|glab|GL_TOKEN|GITLAB_TOKEN' <<<"$proj_secrets"; then
    # Not fatal on its own — some deployments wire the token cluster-wide — but
    # warn loudly so a "no MR ever appeared" outcome is diagnosable.
    yellow "  NOTE: project:${MR_PROJECT} secrets field does not obviously carry a GitLab token; the spawned agent may be unable to push. Proceeding (token may be cluster-wide)."
  fi

  # ── Scenario 1: create a scoped task for the agent to fulfill ───
  blue "Scenario 1: create a throwaway task bead for the agent"
  local tag task_title task_id create_out
  tag="mrsmoke-$(date +%s)-$$"
  task_title="P4.4 cutover smoke: agent-opens-MR probe ${tag}"
  create_out=$("$KD_BIN" create "$task_title" --type=task \
    -l "project:${MR_PROJECT}" -l migration-smoke \
    -d "MIGRATION ACCEPTANCE SMOKE (ka-0000002701). A spawned agent must prove the post-cutover controller+coopmux+daemon+GitLab loop by opening a trivial Draft MR. Marker: ${tag}." \
    --json 2>/dev/null || echo "{}")
  task_id=$(echo "$create_out" | jq -r '.id // empty' 2>/dev/null || true)
  if [ -z "$task_id" ]; then
    task_id=$(echo "$create_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -n1)
  fi
  if [ -z "$task_id" ]; then
    fail "create probe task" "could not create task bead"
    summary
    return 0
  fi
  CREATED_BEADS+=("$task_id")
  pass "probe task created ($task_id)"
  echo

  # ── Scenario 2: spawn a real agent pre-assigned to the task ─────
  blue "Scenario 2: spawn a real agent to open the MR end-to-end"
  # Tight, self-contained prompt: trivial no-op change on a throwaway branch,
  # Draft MR, gb mr link back to the task, then gb stop. Never touch main.
  local agent_prompt
  agent_prompt=$(cat <<PROMPT
You are a P4.4 migration-acceptance smoke agent. Your ONLY job is to prove the
end-to-end agent→MR loop on this cluster. Do exactly this and nothing else:
1. kd claim ${task_id}
2. From an up-to-date main, create a branch: smoke/${tag}
3. Make ONE trivial, safe, no-op change (e.g. append a line to a file under
   gardenio/tests/e2e/smoke-probe/ or touch a scratch file). Do NOT modify any
   production code, CI, or config.
4. Commit, push the branch, and open a DRAFT merge request with 'glab mr create
   --draft --title "P4.4 cutover smoke ${tag}" --description "Automated migration
   acceptance probe (ka-0000002701). Safe to close." '. Never push to or merge into main.
5. Run: gb mr link ${task_id} <the MR url>
6. gb squawk a one-line confirmation, then gb stop. Do NOT merge the MR.
PROMPT
)
  # NB: `gb spawn` has NO --project flag — it infers the project from the task
  # bead's project: label (set at creation above) or BOAT_PROJECT. Passing an
  # unsupported flag would error (fail-when-healthy), so we set BOAT_PROJECT and
  # rely on the task label (review finding, ka-0000003481).
  local spawn_out agent_bead
  spawn_out=$(BOAT_PROJECT="$MR_PROJECT" "$GB_BIN" spawn --role "$MR_AGENT_ROLE" \
    --task "$task_id" --prompt "$agent_prompt" 2>&1 || echo "")
  agent_bead=$(printf '%s' "$spawn_out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -n1)
  if [ -z "$agent_bead" ]; then
    fail "spawn agent" "gb spawn produced no agent bead id (output: $(printf '%s' "$spawn_out" | head -c 200))"
    summary
    return 0
  fi
  SPAWNED_AGENTS+=("$agent_bead")
  pass "agent spawned ($agent_bead) pre-assigned to $task_id"
  echo

  # ── Scenario 3: poll the task bead for a real MR url ────────────
  blue "Scenario 3: agent opens an MR (poll task mr_url, up to ${MR_WAIT_SECS}s)"
  local deadline mr_url=""
  deadline=$(( $(date +%s) + MR_WAIT_SECS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    mr_url=$(bead_field "$task_id" "mr_url")
    if [ -n "$mr_url" ]; then
      break
    fi
    # Surface agent liveness while we wait so a stuck spawn is diagnosable.
    local astate
    astate=$(bead_field "$agent_bead" "agent_state")
    dim "  waiting… agent_state=${astate:-?} ($(( (deadline - $(date +%s)) ))s left)"
    sleep "$MR_POLL_INTERVAL"
  done

  if [ -z "$mr_url" ]; then
    fail "agent opened an MR" \
      "task $task_id has no mr_url after ${MR_WAIT_SECS}s — the agent did not complete claim→branch→push→glab mr create→gb mr link. Check the agent pod (gardenio.io/agent=…) logs."
  elif grep -qE '/(-/)?merge_requests/[0-9]+' <<<"$mr_url"; then
    pass "agent opened a real MR: $mr_url"
  else
    fail "agent opened an MR" "mr_url set but not a well-formed GitLab MR URL: '$mr_url'"
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ migration-agent-opens-mr: $PASS passed, $SKIP skipped"
  else
    red " ✗ migration-agent-opens-mr: $FAIL/$total failed, $SKIP skipped"
    for e in "${ERRORS[@]}"; do red "   - $e"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
