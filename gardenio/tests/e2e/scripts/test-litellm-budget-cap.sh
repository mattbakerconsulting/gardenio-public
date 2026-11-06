#!/usr/bin/env bash
# test-litellm-budget-cap.sh — deliberate-runaway acceptance for the per-CI
# scoped LiteLLM virtual key (R4-COST-1 / R4-SEC-1; task ka-0000000823, epic
# ka-0000000278; design gardenio/docs/research/claudemore-r4-derisking.md).
#
# What it proves: a virtual key minted with a tiny max_budget is HARD-REFUSED by
# LiteLLM (HTTP 429, "budget" error) once cumulative spend crosses the cap — i.e.
# the cap is enforced in-run at the proxy, not merely observed by the lagging
# 5-min spend reconcile. This is the acceptance the bead names: "a deliberate
# runaway test confirms litellm 429s at the cap."
#
# Lifecycle mirrored: mint (as at namespace-create) -> drive a runaway ->
# assert 429 -> delete (as at reap). The mint/delete verbs are the new
# `gb litellm-key` write path (gardenio/controller/cmd/gb/litellm_key.go).
#
# Usage:
#   LITELLM_URL=http://gardenio-litellm:4000 \
#   LITELLM_MASTER_KEY=sk-master-... \
#   ./tests/e2e/scripts/test-litellm-budget-cap.sh [--model claude-haiku-4-5] [--budget 0.05]
#
# Prerequisites:
#   - gb binary in PATH (or set GB_BIN) — provides `gb litellm-key`
#   - curl + jq installed
#   - LITELLM_URL + LITELLM_MASTER_KEY reachable (in-cluster proxy or devops proxy)
#   - The chosen --model must be routable on the target proxy.
#
# Exit 0 = the key 429'd at the cap and was cleaned up. Non-zero = the cap did
# not engage (a COST guardrail regression) or setup/teardown failed.

set -uo pipefail

GB_BIN="${GB_BIN:-gb}"
MODEL="claude-haiku-4-5"
# A budget small enough that a handful of real turns crosses it, keeping the
# runaway cheap. LiteLLM budgets are API-equivalent dollars.
BUDGET="0.05"
# Hard cap on runaway iterations so a broken budget fails fast (as a loud error)
# instead of spending real money in an unbounded loop.
MAX_CALLS="${MAX_CALLS:-40}"

while [ $# -gt 0 ]; do
  case "$1" in
    --model)  MODEL="$2"; shift 2 ;;
    --budget) BUDGET="$2"; shift 2 ;;
    --max-calls) MAX_CALLS="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

: "${LITELLM_URL:?LITELLM_URL is required}"
: "${LITELLM_MASTER_KEY:?LITELLM_MASTER_KEY is required}"

BASE="${LITELLM_URL%/}"
ALIAS="ci-budget-cap-$$-$(date +%s)"
VKEY=""

# shellcheck disable=SC2317  # invoked indirectly via `trap ... EXIT`
cleanup() {
  # Delete-at-reap: always revoke the key we minted, even on failure, so a
  # deliberate-runaway test never leaks a live (if capped) credential.
  if [ -n "$VKEY" ]; then
    echo "=== cleanup: deleting minted key ${VKEY: -6} ==="
    # LITELLM_URL / LITELLM_MASTER_KEY are read from the env by `gb litellm-key`
    # (the flag defaults) — do NOT pass the master key on argv, where it would be
    # visible in `ps`.
    "$GB_BIN" litellm-key delete --key "$VKEY" --ignore-missing || \
      echo "WARN: key delete failed — revoke manually: gb litellm-key delete --key $VKEY"
  fi
}
trap cleanup EXIT

echo "=== Minting capped key: alias=$ALIAS max_budget=\$$BUDGET model=$MODEL ==="
# `gb litellm-key mint` prints the raw sk-... key on stdout; the summary goes to
# stderr, so command-substitution captures only the key. The proxy URL + master
# key come from LITELLM_URL / LITELLM_MASTER_KEY in the env (the flag defaults),
# so the master key is never placed on argv where `ps` could read it. We only
# override --litellm-url so a trailing-slash-normalized $BASE is used.
VKEY="$(LITELLM_URL="$BASE" "$GB_BIN" litellm-key mint \
  --alias "$ALIAS" \
  --max-budget "$BUDGET" \
  --models "$MODEL")"

if [ -z "$VKEY" ] || [ "${VKEY#sk-}" = "$VKEY" ]; then
  echo "FAIL: mint did not return an sk-... key (got: '${VKEY:0:12}...')" >&2
  exit 1
fi
echo "Minted ${VKEY: -6}"

echo "=== Driving a deliberate runaway (max $MAX_CALLS calls) until 429 ==="
GOT_429=0
CALLS=0
for i in $(seq 1 "$MAX_CALLS"); do
  CALLS="$i"
  HTTP_CODE="$(curl -s -o /tmp/lc-body.$$ -w '%{http_code}' \
    -X POST "$BASE/v1/messages" \
    -H "Authorization: Bearer $VKEY" \
    -H "content-type: application/json" \
    -H "anthropic-version: 2023-06-01" \
    -d "{\"model\":\"$MODEL\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"Write a long detailed essay about the number $i. Be exhaustive.\"}]}")"

  if [ "$HTTP_CODE" = "429" ]; then
    # Confirm it is a BUDGET 429, not a generic rate-limit 429.
    BODY="$(cat /tmp/lc-body.$$ 2>/dev/null || true)"
    echo "Call $i -> 429. Body: $(echo "$BODY" | head -c 300)"
    if grep -qiE 'budget|exceeded' <<<"$BODY"; then
      GOT_429=1
      break
    fi
    echo "WARN: 429 but not budget-shaped — retrying to distinguish rate-limit from budget"
    sleep 2
    continue
  fi
  echo "Call $i -> HTTP $HTTP_CODE"
done
rm -f /tmp/lc-body.$$

# Show the final key state (spend vs budget) for the record.
echo "=== Final key state ==="
LITELLM_URL="$BASE" "$GB_BIN" litellm-key info --key "$VKEY" || true

if [ "$GOT_429" -eq 1 ]; then
  echo "PASS: LiteLLM 429'd the capped key at the budget after $CALLS call(s) (R4-COST-1/SEC-1)."
  exit 0
fi

echo "FAIL: drove $CALLS calls without a budget 429 — the max_budget cap did NOT engage." >&2
echo "      This is a COST guardrail regression (R4-COST-1). Check the key carries max_budget:" >&2
echo "      gb litellm-key info --key $VKEY" >&2
exit 1
