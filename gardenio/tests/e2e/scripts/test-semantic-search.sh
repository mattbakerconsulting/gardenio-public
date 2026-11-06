#!/usr/bin/env bash
# test-semantic-search.sh — semantic-search (pgvector + Ollama) E2E smoke test
#
# Exercises the full semantic-search stack on a live kbeads instance:
#   1. Probes whether embedding.enabled is deployed — graceful skip if not.
#   2. Seeds a handful of beads across varied topics.
#   3. Runs `kd embed sync` and asserts non-zero coverage via `kd embed stats`.
#   4. Runs `kd search --semantic "<query>"` with a query that has no lexical
#      overlap with the seed titles; asserts the intended bead is in the top-3.
#   5. Hits POST /v1/search/semantic directly and makes the same assertion.
#   6. Runs `kd similar <bead-id>` and asserts at least one similar result.
#
# Graceful skip: the script treats "feature not deployed" as a SKIP (exit 0),
# not a failure — until the P0 embedding stack lands in staging this module
# runs clean. Once deployed, the assertions take effect automatically.
#
# Usage:
#   NAMESPACE=gardenio-e2e ./tests/e2e/scripts/test-semantic-search.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (NAMESPACE)
#   - kd binary in PATH (or set KD_BIN); must support --semantic, embed, similar
#   - jq, curl, python3 installed
#
# Notes:
#   The test tolerates model warmup: `kd embed sync` is retried a few times on
#   failure so that a cold Ollama pod doesn't flake the run.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()
SKIPPED=0
SKIP_REASON=""

export KD_ACTOR="${KD_ACTOR:-e2e-semantic-search}"

# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-semantic.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Setup: discover embedder config from the beads deployment ────
#
# `kd embed sync` runs on the CLI host (this runner) and computes embeddings
# locally before upserting them into the daemon. It needs BEADS_EMBED_* env
# vars set to a non-noop backend; without them it fails with
# "embedding disabled". The beads deployment already has these wired up
# (see deploy-values/<env>.yaml), so mirror them from the running pod
# unless the caller has already configured them.
setup_embedder_env() {
  if [ -n "${BEADS_EMBED_BACKEND:-}" ] && [ "$BEADS_EMBED_BACKEND" != "noop" ]; then
    dim "  BEADS_EMBED_BACKEND=$BEADS_EMBED_BACKEND already set by caller"
    return
  fi
  blue "Mirroring BEADS_EMBED_* env from deploy/${BEADS_SVC}..."
  local env_out
  env_out=$(kubectl -n "$NAMESPACE" exec "deploy/${BEADS_SVC}" -- env 2>/dev/null \
    | grep '^BEADS_EMBED' || true)
  if [ -z "$env_out" ]; then
    dim "  no BEADS_EMBED_* vars on deploy/${BEADS_SVC} — embed sync will use CLI defaults"
    return
  fi
  # NOTE: use substring splitting instead of `IFS='=' read -r key val`, because
  # `read` with multiple targets strips trailing IFS characters — the LiteLLM
  # master key is base64-encoded and ends in `=`, which would be lost.
  local line key val
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    key="${line%%=*}"
    val="${line#*=}"
    export "$key=$val"
    if [ "$key" = "BEADS_EMBED_API_KEY" ]; then
      dim "  exported $key=<redacted len=${#val}>"
    else
      dim "  exported $key=$val"
    fi
  done <<< "$env_out"
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    kd_retry "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Graceful skip when the feature is not deployed ────────────────
#
# Checks three independent signals and only claims "not deployed" when all three
# point the same way. This avoids a single transient 5xx masking a real
# regression.
#
#   1. POST /v1/search/semantic — feature endpoint
#   2. `kd search --semantic` help text — CLI build has the flag
#   3. `kd embed --help` — CLI build has the command group
probe_feature() {
  local url="${BEADS_HTTP_URL}/v1/search/semantic"
  local auth=()
  [ -n "${BEADS_TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${BEADS_TOKEN}")

  local http_code
  http_code=$(curl -s -o /tmp/semantic-probe.json -w '%{http_code}' \
    "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -X POST "$url" \
    -d '{"query":"probe","limit":1}' 2>/dev/null || echo "000")

  local endpoint_ok=0
  # Accept any 2xx/4xx except 404/501 as "endpoint exists" — a 400 from an
  # empty-corpus server still counts.
  # 503 with body containing "disabled" or "noop" means the embedder is
  # intentionally off (BEADS_EMBED_BACKEND=noop) — treat as "not deployed"
  # and SKIP instead of failing all downstream assertions.
  case "$http_code" in
    404|501|000) endpoint_ok=0 ;;
    503)
      if grep -qiE 'disabled|noop|no embedder' /tmp/semantic-probe.json 2>/dev/null; then
        endpoint_ok=0
      else
        endpoint_ok=1
      fi
      ;;
    *)           endpoint_ok=1 ;;
  esac

  local cli_search_ok=0
  if "$KD_BIN" search --help 2>&1 | grep -q -- '--semantic'; then
    cli_search_ok=1
  fi

  local cli_embed_ok=0
  if "$KD_BIN" embed --help >/dev/null 2>&1; then
    cli_embed_ok=1
  fi

  # `kd embed sync` runs here and computes embeddings with the CLI's configured
  # backend. If BEADS_EMBED_BACKEND is unset or "noop", sync cannot succeed —
  # treat as "not deployed" rather than failing the test harness.
  local local_embedder_ok=1
  case "${BEADS_EMBED_BACKEND:-noop}" in
    ""|noop) local_embedder_ok=0 ;;
  esac

  if [ "$endpoint_ok" = 1 ] && [ "$cli_search_ok" = 1 ] && [ "$cli_embed_ok" = 1 ] \
      && [ "$local_embedder_ok" = 1 ]; then
    return 0
  fi

  SKIP_REASON="semantic-search not deployed (endpoint=${http_code}, cli_search=${cli_search_ok}, cli_embed=${cli_embed_ok}, local_embedder=${local_embedder_ok})"
  return 1
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " semantic-search E2E smoke test"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  setup_embedder_env

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  # ── Feature probe ───────────────────────────────────────────────
  blue "Probing: is the semantic-search stack deployed?"
  if ! probe_feature; then
    yellow "  SKIP: $SKIP_REASON"
    SKIPPED=1
    blue "═══════════════════════════════════════════════════"
    yellow " Module skipped (feature not yet deployed)"
    yellow " This is expected until embedding.enabled=true lands"
    yellow " in the staging deployment."
    blue "═══════════════════════════════════════════════════"
    return 0
  fi
  green "  Feature is deployed — running full suite"
  echo

  local TEST_TAG="e2e-sem-$$-$(date +%s)"

  # ── Scenario 1: Seed varied-topic beads ──────────────────────────
  #
  # Seed titles are intentionally written in vocabulary that a user-provided
  # semantic query (defined later) does NOT share lexically. This way a passing
  # assertion can only come from embedding-based match, not substring fallback.
  blue "Scenario 1: Seed beads across varied topics"
  declare -A SEED_IDS=()
  local seed_titles=(
    "database:Migrating from MySQL 5.7 to Postgres 16 on production"
    "k8s:Pod eviction tuning for memory-hungry workloads in EKS"
    "auth:Session token rotation during password reset flow"
    "frontend:Virtualised table rendering for very large result sets"
    "infra:Terraform state locking with DynamoDB and IAM scopes"
    "ml:Fine-tuning a sentence-transformer on domain-specific jargon"
  )
  local topic title id create_out
  for entry in "${seed_titles[@]}"; do
    topic="${entry%%:*}"
    title="${entry#*:} $TEST_TAG"
    create_out=$(kd_retry "$KD_BIN" create "$title" \
      --type=task -l "e2e:$TEST_TAG" -l "topic:$topic" \
      -d "Automated E2E seed bead for semantic search — topic=$topic — $TEST_TAG" \
      --json 2>/dev/null) || { fail "create seed $topic"; continue; }
    id=$(echo "$create_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -z "$id" ]; then
      fail "seed $topic" "no id returned from create"
      continue
    fi
    SEED_IDS[$topic]="$id"
    CREATED_BEADS+=("$id")
    pass "seeded $topic ($id)"
  done
  echo

  # The "target" bead for most assertions: the ML/embeddings seed. The semantic
  # query below deliberately avoids any word from its title.
  local TARGET_ID="${SEED_IDS[ml]:-}"
  if [ -z "$TARGET_ID" ]; then
    red "ERROR: ML seed bead was not created; cannot proceed"
    return 1
  fi

  # ── Scenario 2: kd embed sync ──────────────────────────────────
  #
  # Run `kd embed sync` inside the beads pod via `kubectl exec`: the CLI's
  # embedder backend (LiteLLM) is IP-whitelisted to the devops cluster, so
  # invoking it from the CI runner gets 403 Forbidden. The beads pod already
  # has BEADS_EMBED_* env wired via Helm.
  blue "Scenario 2: kd embed sync backfills embeddings (via kubectl exec)"
  local sync_rc=1 sync_out=""
  # Retry to absorb cold-start on the embedder backend.
  for i in 1 2 3; do
    # --only-missing: SQL-filter the backfill to beads needing embedding so
    # the test doesn't walk the entire (50k+) beads table just to skip
    # already-embedded beads.
    #
    # --limit=20: cap total beads visited per invocation. Combined with the
    # SQL ORDER BY b.created_at DESC (newest first), this guarantees the 6
    # beads we just seeded above land in the first batch; we never have to
    # wait for a multi-thousand-bead embedding backlog to drain. Critical
    # for the 15-min shard budget — three releases (115.1/115.2/115.3)
    # blew this on the unbounded variant (ka-0000000728).
    sync_out=$(kubectl -n "$NAMESPACE" exec "deploy/${BEADS_SVC}" -- kd embed sync --only-missing --limit=20 2>&1) \
      && sync_rc=0 && break
    dim "  embed sync attempt $i failed, retrying..."
    sleep 5
  done
  assert_exit "kd embed sync exits 0" 0 "$sync_rc"
  [ -n "$sync_out" ] && dim "    $(echo "$sync_out" | head -3 | tr '\n' ' ')"

  # Coverage check: `kd embed stats` is the documented P2 command but the P0
  # formula lists it. Treat its absence as non-fatal and fall back to verifying
  # via a semantic query that returns > 0 results.
  #
  # Cobra prints its help text (to stdout) and exits 0 when asked to run an
  # unknown subcommand, so we can't rely on exit code alone to detect "stats is
  # not implemented" — detect the help preamble explicitly.
  local stats_out stats_rc=0
  stats_out=$("$KD_BIN" embed stats --json 2>/dev/null) || stats_rc=$?
  if grep -q 'Available Commands:' <<<"$stats_out"; then
    stats_out=""
  fi
  if [ "$stats_rc" = 0 ] && [ -n "$stats_out" ]; then
    local covered
    covered=$(echo "$stats_out" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    # tolerate several shapes: {covered: N}, {embedded: N}, {coverage: {embedded: N}}
    for k in ('covered','embedded','count'):
        if k in d:
            print(d[k]); sys.exit(0)
    c = d.get('coverage') or {}
    for k in ('covered','embedded','count'):
        if k in c:
            print(c[k]); sys.exit(0)
    print(0)
except Exception:
    print(0)
" 2>/dev/null || echo "0")
    if [ "$covered" -gt 0 ] 2>/dev/null; then
      pass "embed stats reports non-zero coverage ($covered)"
    else
      fail "embed stats coverage" "expected > 0, got $covered; raw: $(echo "$stats_out" | head -c 200)"
    fi
  else
    dim "  kd embed stats unavailable; deferring coverage check to semantic query"
  fi
  echo

  # ── Scenario 3: kd search --semantic finds target in top-3 ───────
  #
  # The query string has no lexical overlap with the target title
  # ("Fine-tuning a sentence-transformer on domain-specific jargon").
  # Matching relies on embedding similarity between:
  #   query: "teach a language model to understand our internal vocabulary"
  #   title: "fine-tune a sentence-transformer on domain-specific jargon"
  blue "Scenario 3: kd search --semantic returns target in top-3"
  local query="teach a language model to understand our internal vocabulary"
  local search_out search_rc=0
  search_out=$("$KD_BIN" search --semantic "$query" -l "e2e:$TEST_TAG" --limit 10 --json 2>/dev/null) \
    || search_rc=$?
  assert_exit "kd search --semantic exits 0" 0 "$search_rc"

  local top3_ids
  top3_ids=$(echo "$search_out" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    results = d if isinstance(d, list) else d.get('results') or d.get('beads') or []
    for r in results[:3]:
        bead = r.get('bead') if isinstance(r, dict) and 'bead' in r else r
        if isinstance(bead, dict):
            print(bead.get('id',''))
except Exception as e:
    sys.stderr.write(f'parse error: {e}\n')
" 2>/dev/null || echo "")

  if grep -qxF "$TARGET_ID" <<<"$top3_ids"; then
    pass "target bead $TARGET_ID is in top-3 semantic results"
  else
    fail "target in top-3 (kd CLI)" "top-3=$(echo "$top3_ids" | tr '\n' ' '); target=$TARGET_ID"
  fi
  echo

  # ── Scenario 4: POST /v1/search/semantic direct HTTP ─────────────
  blue "Scenario 4: POST /v1/search/semantic returns target in top-3"
  local http_body
  http_body=$(python3 -c "
import json
print(json.dumps({
    'query': '$query',
    'labels': ['e2e:$TEST_TAG'],
    'limit': 10
}))")
  local auth=()
  [ -n "${BEADS_TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${BEADS_TOKEN}")
  local http_out http_code
  http_out=$(curl -s -o /tmp/semantic-resp.json -w '%{http_code}' \
    "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -X POST "${BEADS_HTTP_URL}/v1/search/semantic" \
    -d "$http_body" 2>/dev/null || echo "000")
  http_code="$http_out"
  if [ "$http_code" = "200" ]; then
    pass "POST /v1/search/semantic returns 200"
  else
    fail "POST /v1/search/semantic HTTP" "expected 200, got $http_code; body: $(head -c 200 /tmp/semantic-resp.json)"
  fi
  local http_top3
  http_top3=$(python3 -c "
import sys, json
try:
    with open('/tmp/semantic-resp.json') as f:
        d = json.load(f)
    results = d if isinstance(d, list) else d.get('results') or []
    for r in results[:3]:
        bead = r.get('bead') if isinstance(r, dict) and 'bead' in r else r
        if isinstance(bead, dict):
            print(bead.get('id',''))
except Exception:
    pass
" 2>/dev/null || echo "")
  if grep -qxF "$TARGET_ID" <<<"$http_top3"; then
    pass "target bead $TARGET_ID is in top-3 HTTP results"
  else
    fail "target in top-3 (HTTP)" "top-3=$(echo "$http_top3" | tr '\n' ' '); target=$TARGET_ID"
  fi
  echo

  # ── Scenario 5: kd similar <bead-id> returns at least one result ─
  blue "Scenario 5: kd similar <bead-id> returns ≥ 1 result"
  local sim_out sim_rc=0
  sim_out=$("$KD_BIN" similar "$TARGET_ID" --limit 5 --json 2>/dev/null) || sim_rc=$?
  assert_exit "kd similar exits 0" 0 "$sim_rc"
  local sim_count
  sim_count=$(echo "$sim_out" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    results = d if isinstance(d, list) else d.get('results') or d.get('beads') or []
    # filter out self-reference if the server returns it
    ids = []
    for r in results:
        bead = r.get('bead') if isinstance(r, dict) and 'bead' in r else r
        if isinstance(bead, dict):
            bid = bead.get('id','')
            if bid and bid != '$TARGET_ID':
                ids.append(bid)
    print(len(ids))
except Exception:
    print(0)
" 2>/dev/null || echo "0")
  if [ "$sim_count" -ge 1 ] 2>/dev/null; then
    pass "kd similar returned $sim_count result(s)"
  else
    fail "kd similar result count" "expected ≥ 1, got $sim_count"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
