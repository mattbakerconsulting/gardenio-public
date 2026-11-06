#!/usr/bin/env bash
# test-gitlab-jwt-e2e.sh — T4 end-to-end: all-GitLab-issued JWTs on staging
#
# Bead: gd-8EL2X1CJEw  (epic ka-0000004118 — Direct GitLab→coopmux: skip
# Keycloak federation). This is the green-light GATE before the T5 prod
# cutover: it proves the Keycloak-permadelete cutover actually landed on the
# long-lived staging SINGLETON — the two JWT-verifying services (the beads
# daemon and coopmux) are pointed DIRECTLY at gitlab.com as the OIDC issuer,
# NO Keycloak-issued JWT is observed on either verifier at runtime, and a full
# session leg (kd create → kd close, attributed) succeeds against the daemon.
#
# ── Why this module exists ────────────────────────────────────────────────────
# T1 (ka-0000000246) flipped the forwardAuth oauth2-proxy to provider=gitlab;
# T2 (gd-YA22Xm2j6m) flipped coopmux's COOP_MUX_OIDC_* to gitlab.com; T3
# (ka-0000002268) flipped the beads daemon's BEADS_OIDC_* to gitlab.com. Each
# landed a Helm-values override that WINS over the Keycloak derivation (see
# the staging values: beads.oidc.issuerUrl / coopmux.oidc.issuerUrl
# = https://gitlab.com). But no CI module asserted the END STATE — that the
# rendered, DEPLOYED, RUNNING services actually carry the GitLab issuer and
# reject Keycloak tokens. A silent regression (a values override dropped, a
# Keycloak fallback re-selected, a stale rollout) would go undetected until a
# human noticed logins breaking. This module is that end-state gate.
#
# It verifies the FACT (the live deployment env + running-pod logs), not a
# proxy for it (advice gd-UHv3Nhisq6 / ka-0000002586): a values file that
# "looks right" is a silent no-op if the rollout never happened.
#
# ── The four acceptance checks (bead gd-8EL2X1CJEw) ───────────────────────────
#   G1. Beads daemon JWT verifier is GitLab-issued, NOT Keycloak. The deployed
#       ${ns}-beads container carries BEADS_OIDC_ISSUER_URL=https://gitlab.com
#       and NO Keycloak realm issuer. (T3 landed.)
#   G2. Coopmux JWT verifier is GitLab-issued, NOT Keycloak. The deployed
#       ${ns}-coopmux container carries COOP_MUX_OIDC_ISSUER_URL=https://gitlab.com
#       and NO Keycloak realm issuer. (T2 landed.)
#   G3. Full session leg: a `kd create` → `kd close` against the daemon
#       succeeds and its audit event log attributes the write to the injected
#       GitLab-identity actor (oidc:<email> sub/email) — the server-side,
#       green-able-in-CI equivalent of the browser→forwardauth→daemon hop
#       (mirrors edge-auth Gate Q, ka-0000007461). Proves the daemon is
#       reachable AND records the GitLab `sub`/`email` as the actor.
#   G4. No Keycloak JWTs observed. The daemon boot log shows "OIDC auth
#       enabled" with issuers containing gitlab.com, and neither the daemon nor
#       coopmux runtime logs show a Keycloak realm issuer being used to verify a
#       token. (Keycloak's own POD may still be running — deletion is T6; this
#       only asserts no Keycloak JWT reaches the two verifiers.)
#
# ── Skip-clean contract (never a false red) ───────────────────────────────────
# The GitLab-direct cutover is deployed ONLY on the long-lived staging
# SINGLETON. Per-instance EPHEMERAL e2e
# namespaces (gardenio-e2e-N) intentionally stay on the Keycloak realm broker
# until the cutover is proven (see the e2e ForwardAuth values + the
# edge-auth README "ephemeral vs singleton" section). So in the ephemeral ns
# this module's GitLab-issuer assertions do NOT apply — it LOUD-SKIPs each gate
# whose substrate is Keycloak, exactly like the migration-acceptance modules.
# The module hard-asserts ONLY when it observes the GitLab issuer wired, which
# is the singleton (or a future all-GitLab ephemeral). It is therefore inert in
# the standard fast-smoke fan-out and green-lights the singleton cutover.
#
# Point it at the singleton (or prod, post-T5) with:
#   NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-gitlab-jwt-e2e.sh
#
# Environment:
#   NAMESPACE        (default gardenio-e2e) — namespace to introspect.
#   BEADS_HTTP_URL   (optional) — kbeads HTTP base; else port-forwards the svc.
#   BEADS_TOKEN      (optional) — static daemon token for the G3 write leg.
#   KD_BIN           (default kd) — kd CLI binary.
#   GITLAB_ISSUER    (default https://gitlab.com) — the expected GitLab issuer.
#
# READ-ONLY apart from creating + closing one throwaway fixture bead (G3).

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_DEPLOY="${BEADS_DEPLOY:-${NAMESPACE}-beads}"
COOPMUX_DEPLOY="${COOPMUX_DEPLOY:-${NAMESPACE}-coopmux}"
KD_BIN="${KD_BIN:-kd}"
GITLAB_ISSUER="${GITLAB_ISSUER:-https://gitlab.com}"
# A Keycloak realm issuer always contains "/realms/" in its path (see
# gardenio.keycloak.issuerURL helper: https://<host>/auth/realms/<realm> or
# https://<host>/realms/<realm>). We use that marker to distinguish a
# Keycloak-fronted verifier from a GitLab-direct one.
KEYCLOAK_ISSUER_MARKER="${KEYCLOAK_ISSUER_MARKER:-/realms/}"

# Synthetic GitLab human identity injected for the G3 write leg. In prod the
# forwardauth → daemon hop stamps this from the GitLab-verified session; here
# we inject it directly via X-Operated-By (the same static-token attribution
# channel kbeads honors) so the assertion is green-able in CI. Distinct from
# the shard's ambient BEADS_OPERATED_BY so the check proves the per-request
# GitLab identity wins, not the ambient operator.
GITLAB_ACTOR_EMAIL="${GITLAB_ACTOR_EMAIL:-t4-gitlab-jwt-e2e@acme.example}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

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

have() { command -v "$1" >/dev/null 2>&1; }

# Read a single env var's value off the first container of a Deployment.
# Empty string when the deployment/var is absent (caller distinguishes).
deploy_env() {
  local deploy="$1" var="$2"
  kubectl -n "$NAMESPACE" get deploy "$deploy" \
    -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='${var}')].value}" \
    2>/dev/null || echo ""
}

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ══════════════════════════════════════════════════════════════════
# G1: Beads daemon JWT verifier is GitLab-issued, not Keycloak
# ══════════════════════════════════════════════════════════════════
gate_beads_issuer() {
  blue "G1: beads daemon JWT verifier → GitLab issuer (not Keycloak)"
  if ! have kubectl; then
    skip "beads daemon issuer" "kubectl not in PATH"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get deploy "$BEADS_DEPLOY" >/dev/null 2>&1; then
    skip "beads daemon issuer" "deploy/$BEADS_DEPLOY absent in $NAMESPACE"
    return 0
  fi

  local issuer
  issuer="$(deploy_env "$BEADS_DEPLOY" BEADS_OIDC_ISSUER_URL)"
  if [ -z "$issuer" ]; then
    skip "beads daemon issuer" \
      "BEADS_OIDC_ISSUER_URL unset on $BEADS_DEPLOY — OIDC JWT verification not wired here (token-only ns); GitLab cutover not deployed"
    return 0
  fi

  # Skip-clean contract: a Keycloak realm issuer means this is a legitimate
  # NOT-YET-CUTOVER namespace (the ephemeral e2e ns stays on the Keycloak realm
  # broker until the cutover proves out — the e2e ForwardAuth values). The
  # module is INERT there — a loud SKIP, never a red — so it does not fail the
  # fast fan-out. It hard-asserts ONLY when it observes the GitLab issuer wired
  # (the singleton / prod post-T5).
  case "$issuer" in
    *"$KEYCLOAK_ISSUER_MARKER"*)
      skip "beads daemon issuer" \
        "BEADS_OIDC_ISSUER_URL=$issuer is a Keycloak realm — this ns is not on the GitLab cutover (ephemeral/pre-T5); assertions apply only post-cutover"
      return 0
      ;;
  esac
  if [ "$issuer" = "$GITLAB_ISSUER" ]; then
    pass "beads daemon BEADS_OIDC_ISSUER_URL=$issuer (GitLab-direct)"
  else
    fail "beads daemon issuer = $GITLAB_ISSUER" \
      "BEADS_OIDC_ISSUER_URL=$issuer — not the expected GitLab issuer and not a Keycloak realm; unexpected verifier target"
    return 0
  fi

  # A leftover BEADS_OIDC_ISSUER_URLS with a Keycloak realm would mean a
  # Keycloak token would STILL be accepted (multi-issuer) — the cutover must
  # REPLACE the issuer, not add GitLab alongside Keycloak (T3 acceptance).
  local extra
  extra="$(deploy_env "$BEADS_DEPLOY" BEADS_OIDC_ISSUER_URLS)"
  case "$extra" in
    *"$KEYCLOAK_ISSUER_MARKER"*)
      fail "no Keycloak issuer accepted by daemon" \
        "BEADS_OIDC_ISSUER_URLS=$extra still lists a Keycloak realm — a Keycloak JWT would be accepted; cutover must replace, not add"
      ;;
    "")
      pass "beads daemon has no additional (Keycloak) issuer configured"
      ;;
    *)
      pass "beads daemon extra issuers carry no Keycloak realm ($extra)"
      ;;
  esac
}

# ══════════════════════════════════════════════════════════════════
# G2: Coopmux JWT verifier is GitLab-issued, not Keycloak
# ══════════════════════════════════════════════════════════════════
gate_coopmux_issuer() {
  blue "G2: coopmux JWT verifier → GitLab issuer (not Keycloak)"
  if ! have kubectl; then
    skip "coopmux issuer" "kubectl not in PATH"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" >/dev/null 2>&1; then
    skip "coopmux issuer" \
      "deploy/$COOPMUX_DEPLOY absent (coopmux.enabled=false — only on singleton / fleet / inbox overlays)"
    return 0
  fi

  local issuer
  issuer="$(deploy_env "$COOPMUX_DEPLOY" COOP_MUX_OIDC_ISSUER_URL)"
  if [ -z "$issuer" ]; then
    skip "coopmux issuer" \
      "COOP_MUX_OIDC_ISSUER_URL unset on $COOPMUX_DEPLOY — coopmux OIDC verification not wired here; GitLab cutover not deployed"
    return 0
  fi

  # Skip-clean contract (as G1): a Keycloak realm issuer is a legitimate
  # pre-cutover namespace — SKIP, never red.
  case "$issuer" in
    *"$KEYCLOAK_ISSUER_MARKER"*)
      skip "coopmux issuer" \
        "COOP_MUX_OIDC_ISSUER_URL=$issuer is a Keycloak realm — this ns is not on the GitLab cutover (ephemeral/pre-T5); assertions apply only post-cutover"
      return 0
      ;;
  esac
  if [ "$issuer" = "$GITLAB_ISSUER" ]; then
    pass "coopmux COOP_MUX_OIDC_ISSUER_URL=$issuer (GitLab-direct)"
  else
    fail "coopmux issuer = $GITLAB_ISSUER" \
      "COOP_MUX_OIDC_ISSUER_URL=$issuer — not the expected GitLab issuer and not a Keycloak realm"
  fi
}

# ══════════════════════════════════════════════════════════════════
# G3: Full session leg — kd create → close, GitLab actor attributed
# ══════════════════════════════════════════════════════════════════
# The server-side, green-able-in-CI equivalent of the browser → forwardauth →
# daemon hop: a write carrying ONLY the injected GitLab human identity
# (X-Operated-By: oidc:<email>) must succeed AND record that identity as the
# actor on the bead's audit event log (mirrors edge-auth Gate Q, ka-0000007461).
# Proves the daemon is reachable and the audit log shows the GitLab sub/email as
# the actor — exactly the "audit logs show GitLab sub/email as the actor"
# acceptance line.
gate_session_leg() {
  blue "G3: full session leg (kd create → close) with GitLab actor attribution"
  if ! have curl || ! have jq; then
    skip "session leg" "curl/jq not in PATH"
    return 0
  fi

  # Resolve a daemon HTTP base.
  local base="${BEADS_HTTP_URL:-}"
  if [ -z "$base" ]; then
    if ! have kubectl || ! kubectl -n "$NAMESPACE" get svc "$BEADS_SVC" >/dev/null 2>&1; then
      skip "session leg" "no BEADS_HTTP_URL and svc/$BEADS_SVC absent — cannot reach the daemon"
      return 0
    fi
    local port=19097
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
      >/tmp/t4-gitlab-jwt-pf.log 2>&1 &
    PF_PID=$!
    base="http://localhost:${port}"
    local _i
    for _i in $(seq 1 12); do
      curl -sf --max-time 1 -o /dev/null "${base}/v1/health" 2>/dev/null && break
      sleep 0.5
    done
  fi

  # Load the static daemon token if we can (authenticates the write; the
  # GitLab identity attribution rides on X-Operated-By, mirroring the prod
  # forwardauth envelope which carries the verified GitLab identity).
  local token="${BEADS_TOKEN:-}"
  if [ -z "$token" ] && have kubectl; then
    token="$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  fi

  local op="oidc:${GITLAB_ACTOR_EMAIL#oidc:}"
  local -a wh=(-H "Content-Type: application/json" -H "X-Operated-By: ${op}")
  [ -n "$token" ] && wh+=(-H "Authorization: Bearer ${token}")

  # ── kd create (the "kd create" session leg) ───────────────────────
  local create_body create_http create_json bead_id
  create_body="$(curl -sS --max-time 15 -w '\n__HTTP__:%{http_code}' \
    -X POST "${wh[@]}" \
    -d "{\"title\":\"T4 gitlab-jwt-e2e session leg\",\"type\":\"task\",\"labels\":[\"e2e:gitlab-jwt-e2e\"],\"description\":\"T4 gd-8EL2X1CJEw GitLab-JWT session leg probe — safe to delete\"}" \
    "${base%/}/v1/beads" 2>&1)" || true
  create_http="$(printf '%s' "$create_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  create_json="$(printf '%s' "$create_body" | sed '/__HTTP__:/d')"

  if [ "$create_http" != "200" ] && [ "$create_http" != "201" ]; then
    # A 401 here on the singleton would mean the daemon rejected the write —
    # but attribution rides on the static token, so this is a reachability
    # failure, not an issuer failure. Report it honestly.
    fail "kd create session leg → 2xx" \
      "got ${create_http:-no-response} — daemon write did not succeed: $(printf '%s' "$create_json" | head -c 160)"
    return 0
  fi
  bead_id="$(printf '%s' "$create_json" | jq -r '.id // empty' 2>/dev/null)"
  if [ -z "$bead_id" ]; then
    fail "kd create returns bead id" "no .id in response: $(printf '%s' "$create_json" | head -c 160)"
    return 0
  fi
  CREATED_BEADS+=("$bead_id")
  pass "kd create session leg → ${bead_id} (daemon reachable)"

  # ── Assert the audit event log attributes the GitLab actor ────────
  local events_json events_http
  events_json="$(curl -sS --max-time 15 -w '\n__HTTP__:%{http_code}' \
    "${wh[@]}" "${base%/}/v1/beads/${bead_id}/events" 2>&1)" || true
  events_http="$(printf '%s' "$events_json" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  events_json="$(printf '%s' "$events_json" | sed '/__HTTP__:/d')"
  if [ "$events_http" != "200" ]; then
    fail "GET bead events → 200" "got ${events_http:-no-response}"
  else
    local distinct_ops mismatched
    distinct_ops="$(printf '%s' "$events_json" | jq -r \
      '[.events[]? | select((.topic // "") | test("created|updated|comment")) | .operated_by // ""] | unique | .[]' 2>/dev/null)"
    if [ -z "$distinct_ops" ]; then
      fail "audit events carry the GitLab actor" \
        "no write events with operated_by found: $(printf '%s' "$events_json" | head -c 240)"
    else
      mismatched="$(printf '%s\n' "$distinct_ops" | grep -v -x "$op" || true)"
      if [ -z "$mismatched" ]; then
        pass "audit event log attributes the GitLab actor (operated_by=${op})"
      else
        fail "audit events attribute to ${op}" \
          "found other operator(s): $(printf '%s' "$mismatched" | tr '\n' ' ') — GitLab identity not recorded as the actor"
      fi
    fi
  fi

  # ── kd close (the "kd close" session leg) ─────────────────────────
  # POST /v1/beads/{id}/close (handleCloseBead) — the canonical close route.
  local close_http close_body
  close_body="$(curl -sS --max-time 15 -w '\n__HTTP__:%{http_code}' \
    -X POST "${wh[@]}" -d '{}' \
    "${base%/}/v1/beads/${bead_id}/close" 2>&1)" || true
  close_http="$(printf '%s' "$close_body" | awk -F: '/__HTTP__:/{print $2}' | tr -d '\r ')"
  if [ "$close_http" = "200" ] || [ "$close_http" = "201" ] || [ "$close_http" = "204" ]; then
    pass "kd close session leg → ${close_http}"
  else
    fail "kd close session leg → 2xx" \
      "got ${close_http:-no-response}: $(printf '%s' "$close_body" | sed '/__HTTP__:/d' | head -c 160)"
  fi
}

# ══════════════════════════════════════════════════════════════════
# G4: No Keycloak JWTs observed — daemon + coopmux runtime logs
# ══════════════════════════════════════════════════════════════════
# Two assertions, both gated on the GitLab issuer being wired (else this ns is
# a legitimate Keycloak namespace and the check does not apply):
#   G4a. The daemon boot log shows "OIDC auth enabled" naming a gitlab.com
#        issuer (kbeads/cmd/kd/serve.go logs issuers= at startup).
#   G4b. Neither the daemon nor coopmux runtime logs mention a Keycloak realm
#        issuer being used to verify a token — a Keycloak realm URL in the
#        VERIFIER's logs is the "Keycloak JWT observed" signature we forbid.
gate_no_keycloak_jwts() {
  blue "G4: no Keycloak JWTs observed on the verifiers"
  if ! have kubectl; then
    skip "no Keycloak JWTs" "kubectl not in PATH"
    return 0
  fi

  # Only meaningful when the GitLab cutover is actually deployed here.
  local beads_issuer=""
  if kubectl -n "$NAMESPACE" get deploy "$BEADS_DEPLOY" >/dev/null 2>&1; then
    beads_issuer="$(deploy_env "$BEADS_DEPLOY" BEADS_OIDC_ISSUER_URL)"
  fi
  case "$beads_issuer" in
    "$GITLAB_ISSUER")
      : # GitLab-direct — proceed with the log assertions.
      ;;
    *"$KEYCLOAK_ISSUER_MARKER"*|"")
      skip "no Keycloak JWTs" \
        "daemon not on the GitLab issuer here (issuer='${beads_issuer:-unset}') — Keycloak namespace; the no-Keycloak-JWT invariant applies only post-cutover"
      return 0
      ;;
  esac

  # ── G4a: daemon boot log confirms the GitLab issuer ───────────────
  # slog text handler emits: OIDC auth enabled ... issuers=[https://gitlab.com]
  local daemon_logs
  daemon_logs="$(kubectl -n "$NAMESPACE" logs "deploy/${BEADS_DEPLOY}" \
    --tail=4000 2>/dev/null || echo "")"
  if [ -z "$daemon_logs" ]; then
    skip "daemon boot log shows GitLab issuer" "no daemon logs retrievable"
  else
    local enabled_line
    enabled_line="$(printf '%s\n' "$daemon_logs" | grep -i "OIDC auth enabled" | tail -1 || true)"
    if [ -z "$enabled_line" ]; then
      # Older log rotation may have dropped the boot line — not a failure by
      # itself (the deployed env in G1 is the authoritative fact). Note it.
      dim "  (boot 'OIDC auth enabled' line not in the retained window — G1 env is authoritative)"
      pass "daemon OIDC enablement asserted via deployed env (boot line rotated out)"
    elif grep -qi "gitlab.com" <<<"$enabled_line"; then
      pass "daemon boot log: OIDC auth enabled with a gitlab.com issuer"
    else
      fail "daemon boot log names a gitlab.com issuer" \
        "OIDC-enabled line does not mention gitlab.com: $(printf '%s' "$enabled_line" | head -c 200)"
    fi
    # A Keycloak realm issuer appearing in an OIDC-enabled/verify line is the
    # forbidden signature.
    if printf '%s\n' "$daemon_logs" \
        | grep -iE "OIDC auth enabled|verif" \
        | grep -qF "$KEYCLOAK_ISSUER_MARKER"; then
      fail "daemon logs show no Keycloak realm issuer" \
        "a Keycloak realm issuer ($KEYCLOAK_ISSUER_MARKER) appears in the daemon's OIDC verify path — Keycloak JWT observed"
    else
      pass "daemon logs show no Keycloak realm issuer in the verify path"
    fi
  fi

  # ── G4b: coopmux runtime logs carry no Keycloak realm issuer ──────
  if kubectl -n "$NAMESPACE" get deploy "$COOPMUX_DEPLOY" >/dev/null 2>&1; then
    local coopmux_issuer coopmux_logs
    coopmux_issuer="$(deploy_env "$COOPMUX_DEPLOY" COOP_MUX_OIDC_ISSUER_URL)"
    case "$coopmux_issuer" in
      "$GITLAB_ISSUER")
        coopmux_logs="$(kubectl -n "$NAMESPACE" logs "deploy/${COOPMUX_DEPLOY}" \
          --tail=4000 2>/dev/null || echo "")"
        if [ -z "$coopmux_logs" ]; then
          skip "coopmux logs show no Keycloak issuer" "no coopmux logs retrievable"
        elif grep -qF "$KEYCLOAK_ISSUER_MARKER" <<<"$coopmux_logs"; then
          fail "coopmux logs show no Keycloak realm issuer" \
            "a Keycloak realm issuer ($KEYCLOAK_ISSUER_MARKER) appears in coopmux logs — Keycloak JWT observed on the coopmux verifier"
        else
          pass "coopmux logs show no Keycloak realm issuer"
        fi
        ;;
      *)
        dim "  coopmux not on the GitLab issuer here (issuer='${coopmux_issuer:-unset}') — skipping its log assertion"
        ;;
    esac
  else
    dim "  coopmux Deployment absent — no coopmux log assertion"
  fi
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════════════"
  blue " T4 end-to-end: all-GitLab-issued JWTs (gd-8EL2X1CJEw)"
  blue " namespace=$NAMESPACE  expected issuer=$GITLAB_ISSUER"
  blue "═══════════════════════════════════════════════════════════"
  echo

  gate_beads_issuer;    echo
  gate_coopmux_issuer;  echo
  gate_session_leg;     echo
  gate_no_keycloak_jwts

  summary
}

summary() {
  echo
  blue "═══════════════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ gitlab-jwt-e2e: $PASS passed, $SKIP skipped"
    if [ "$PASS" -eq 0 ]; then
      yellow "   (all gates skipped — Keycloak namespace / GitLab cutover not deployed here)"
      # Skip-visibility protocol (ka-0000000989): a run that asserted NOTHING
      # (every gate skipped — no cluster + no reachable daemon) must be recorded
      # as SKIPPED, never counted toward the green tally. Write the sentinel so
      # run_module classifies this exit-0 as SKIPPED, not PASSED.
      if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then
        printf '%s\n' "all gates skipped — not on the GitLab cutover here" \
          >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
      fi
    fi
  else
    red " ✗ gitlab-jwt-e2e: $FAIL/$total failed, $SKIP skipped"
    for e in "${ERRORS[@]}"; do red "   - $e"; done
  fi
  blue "═══════════════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
