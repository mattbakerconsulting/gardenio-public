#!/bin/bash
# One-time e2e fixture seeding: test user beads (+ shard-operator user beads).
#
# Runs once per e2e dispatch (shared across all shards) so that test-user POSTs
# don't create duplicate user beads. User-bead seeding uses upsert-by-label so
# duplicates aren't created on re-run.
#
# ── Why this script no longer PUTs type:* schemas (ka-0000003677) ─────────────
# It used to PUT hand-maintained full schemas for type:{account,advice,
# escalation,formula,memory,molecule,project,trigger,user}. Every one of those
# types is AUTHORITATIVE in the kbeads daemon builtin (kbeads/internal/server/
# config.go `builtinConfigs`), so the daemon already compiles the correct schema
# in when NO stored config exists (resolveTypeConfig / listConfigsWithBuiltins).
#
# A stored PUT here does NOT add anything the daemon lacks — it can only SHADOW
# the builtin and DRIFT behind it. resolveTypeConfig merges builtin fields that
# are missing from a stored config, but it does NOT reconcile a stored field
# whose `required`/`type`/enum constraint is WEAKER than the builtin's (the store
# row wins there), and listConfigsWithBuiltins / GetConfig / `kd fields` return
# the stored row verbatim with no merge at all. So a drifted stored schema makes
# the ephemeral e2e namespace test a shape prod does NOT run — false-green in
# both directions (a passing round-trip that never exercised the real field, or
# a real regression the weakened constraint hides). This is the exact shadowing
# that masked ka-0000000585 in prod. Verified 2026-08-06: the old PUTs had
# drifted — type:trigger omitted pipeline_ref_pattern (shipped 74cd09f0e),
# type:user omitted status_message (shipped bb8645d16), type:account had dropped
# `required` on provider/credential and typed max_budget as string not float.
#
# By NOT PUTting these, e2e exercises exactly the builtin schema prod runs. The
# durable guard that keeps this true — TestE2ESeedTypes_NoBuiltinShadow in
# kbeads/internal/server/config_seed_shadow_test.go — fails CI if any type:*
# schema PUT is re-added to this script for a type the builtin owns (unless the
# stored schema is a proven superset of the builtin). Add a bead type to the
# daemon builtin instead; do not resurrect a hand-maintained PUT here.

set -euo pipefail

: "${BEADS_HTTP_URL:?BEADS_HTTP_URL must be set}"
: "${BEADS_TOKEN:?BEADS_TOKEN must be set}"

# ── Test user beads (idempotent: skip if a prior run already seeded them) ──
echo "Seeding test user beads..."
# Query existing user beads by the shared label; only create if absent.
EXISTING=$(curl -sf --max-time 30 \
  -H "Authorization: Bearer $BEADS_TOKEN" \
  "$BEADS_HTTP_URL/v1/beads?type=user&labels=e2e:escalation-approval" 2>/dev/null \
  | python3 -c 'import sys,json
try:
  d=json.load(sys.stdin)
  beads=d.get("beads",d if isinstance(d,list) else [])
  print(len(beads))
except Exception:
  print(0)' 2>/dev/null || echo 0)

if [ "${EXISTING:-0}" -ge 2 ]; then
  echo "Test user beads already seeded ($EXISTING found) — skipping"
else
  E2E_USER_OUT=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $BEADS_TOKEN" \
    -d '{"title":"E2E Test User","type":"user","labels":["e2e:escalation-approval"],"fields":{"email":"e2e-test@gardenio.dev","display_name":"E2E Test User","capabilities":"[\"approve:escalation\"]"}}' 2>/dev/null || echo "{}")
  echo "Test user (capable) seeded: $(echo "$E2E_USER_OUT" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id","unknown"))' 2>/dev/null || echo 'unknown')"

  E2E_NOCAP_OUT=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $BEADS_TOKEN" \
    -d '{"title":"E2E No-Cap User","type":"user","labels":["e2e:escalation-approval"],"fields":{"email":"e2e-nocap@gardenio.dev","display_name":"E2E No-Cap User","capabilities":"[\"gardenio-viewer\"]"}}' 2>/dev/null || echo "{}")
  echo "Test user (no-cap) seeded: $(echo "$E2E_NOCAP_OUT" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id","unknown"))' 2>/dev/null || echo 'unknown')"
fi

# ── Per-shard operator user beads (gd-6WRsSTbszR) ─────────────────────────────
# run-shard.sh:193 exports BEADS_OPERATED_BY=oidc:e2e-shard-${N}@example.com so
# OIDC creates from inside e2e tests are attributed to a synthetic per-shard
# identity. Project-scoped types (type=schedule, type=trigger, ...) enforce
# write_policy.project_scoped, which calls lookupSpawnPermissions() to find a
# user-type config bead matching the operator's email. Without a seeded user
# bead, perms==nil → "project-scoped write denied: not authorized" and the
# test prints "could not create <type> bead" (the real error is swallowed by
# `2>/dev/null` on the kd create). See gardenio.e2e.scheduler module Scenarios
# 1 & 4 for the affected paths.
echo "Seeding shard-operator user beads..."
EXISTING_SHARD=$(curl -sf --max-time 30 \
  -H "Authorization: Bearer $BEADS_TOKEN" \
  "$BEADS_HTTP_URL/v1/beads?type=user&labels=e2e:shard-operator" 2>/dev/null \
  | python3 -c 'import sys,json
try:
  d=json.load(sys.stdin)
  beads=d.get("beads",d if isinstance(d,list) else [])
  print(len(beads))
except Exception:
  print(0)' 2>/dev/null || echo 0)

if [ "${EXISTING_SHARD:-0}" -ge 4 ]; then
  echo "Shard-operator user beads already seeded ($EXISTING_SHARD found) — skipping"
else
  for SHARD in 0 1 2 3; do
    EMAIL="e2e-shard-${SHARD}@example.com"
    SHARD_OUT=$(curl -sf --max-time 30 -X POST "$BEADS_HTTP_URL/v1/beads" \
      -H 'Content-Type: application/json' \
      -H "Authorization: Bearer $BEADS_TOKEN" \
      -d "{\"title\":\"E2E Shard $SHARD Operator\",\"type\":\"user\",\"labels\":[\"e2e:shard-operator\"],\"fields\":{\"email\":\"$EMAIL\",\"display_name\":\"E2E Shard $SHARD Operator\",\"allowed_projects\":\"*\"}}" 2>/dev/null || echo "{}")
    echo "Shard $SHARD operator seeded: $(echo "$SHARD_OUT" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id","unknown"))' 2>/dev/null || echo 'unknown')"
  done
fi

echo "Seeding complete"
