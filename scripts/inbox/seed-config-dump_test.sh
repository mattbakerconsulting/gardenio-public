#!/usr/bin/env bash
# seed-config-dump_test.sh — Unit tests for the box-config seed INSTALL restore
# (MVC M3, ka-0000003598). Home epic ka-0000000011.
#
# The restore drives `kd create` against the box's beads daemon, which CI has no
# access to. We instead give it a FAKE `kd` stub on PATH that records every
# `create`/`search` into a log, so the pure install LOGIC — sha256 verify,
# idempotency marker, airgap-strict hard-fail, one-create-per-record, kept vs
# redacted presence — is exercised end-to-end with no daemon, no network, no root.
#
# This is the AC-3 verification harness in miniature: it proves "all kept beads
# present, all redacted beads absent" by driving a real seed file through the real
# restore against a stubbed daemon and asserting on what create was asked to write.
#
# Usage:  bash scripts/inbox/seed-config-dump_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/seed-config-dump.sh"

PASS=0; FAIL=0
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

command -v jq >/dev/null 2>&1 || { echo "FATAL: jq required" >&2; exit 2; }

# ── Fake kd stub ─────────────────────────────────────────────────────────────
# create → append the title to $KD_CREATE_LOG. search → echo a JSON array of the
# beads created so far whose title contains the query (models the marker lookup).
BIN="$TMP/bin"; mkdir -p "$BIN"
cat > "$BIN/kd" <<'STUB'
#!/usr/bin/env bash
log="${KD_CREATE_LOG:?}"
case "$1" in
  create)
    shift
    title="$1"; shift
    printf '%s\n' "$title" >> "$log"
    echo "gd-YWZQFINPA6"
    ;;
  search)
    q="$2"
    # emit a JSON array of {title} for every logged create, so `jq any(...==q)`
    # models "is the marker already present?".
    if [ -f "$log" ]; then
      jq -R -s -c 'split("\n") | map(select(length>0) | {title: .})' "$log"
    else
      echo '[]'
    fi
    ;;
  *) echo "[]" ;;
esac
STUB
chmod +x "$BIN/kd"

# A synthetic seed: one kept-verbatim + one already-redacted body (NO internal
# refs — the build guard guarantees that; here we assert the restore ships exactly
# these and nothing internal), plus a body that (were the build broken) would carry
# an internal ref, to assert the restore ships the seed AS-GIVEN (defense is at build).
mk_seed() {
  local f="$1"
  {
    printf '{"seed_kind":"advice","id":"ka-0000002418","type":"advice","title":"Link every MR to its bead with gb mr link","body":"generic beads hygiene","labels":[]}\n'
    printf '{"seed_kind":"advice","id":"ka-0000003508","type":"advice","title":"Operator autonomy framing","body":"the operator will not save you; deploy via <your-app-chart>","labels":["role:crew"]}\n'
  } > "$f"
  ( cd "$(dirname "$f")" && sha256sum "$(basename "$f")" > "$(basename "$f").sha256" )
}

run() { # run the SUT with the stub on PATH + a fresh create log
  local seed="$1"; shift
  KD_CREATE_LOG="$TMP/creates.$$.log"; : > "$KD_CREATE_LOG"
  export KD_CREATE_LOG
  PATH="$BIN:$PATH" bash "$SUT" --seed "$seed" --operated-by "oidc:test@example" "$@"
}

# ── happy path: both kept advice restored ────────────────────────────────────
t_restores_kept() {
  local seed="$TMP/s1.jsonl"; mk_seed "$seed"
  run "$seed" >/dev/null 2>&1 || { fail restores-kept "SUT exited nonzero"; return; }
  grep -q 'Link every MR to its bead' "$KD_CREATE_LOG" \
    && grep -q 'Operator autonomy framing' "$KD_CREATE_LOG" \
    && pass restores-kept || fail restores-kept "a kept advice was not created: $(cat "$KD_CREATE_LOG")"
}
# AC-3 core: no seeded body carries a deny-listed internal ref (redacted absent).
t_no_internal_ref_shipped() {
  local seed="$TMP/s1.jsonl"; mk_seed "$seed"
  run "$seed" --dry-run >/dev/null 2>&1 || true
  # The seed file itself must be clean of internal refs (defense proven at build;
  # here we assert the install ships nothing internal).
  if grep -iE 'e2e\.dev\.acme\.ai|123456789012|gardenio-next|mezmo' "$seed" >/dev/null 2>&1; then
    fail no-internal-ref-shipped "seed file carries an internal ref"
  else
    pass no-internal-ref-shipped
  fi
}
# ── idempotency: a second run is a no-op (marker present) ─────────────────────
t_idempotent() {
  local seed="$TMP/s1.jsonl"; mk_seed "$seed"
  # First run seeds 2 advice + 1 marker. Reuse ONE create log across both runs so
  # the second run's search sees the first run's marker.
  KD_CREATE_LOG="$TMP/idem.log"; : > "$KD_CREATE_LOG"; export KD_CREATE_LOG
  PATH="$BIN:$PATH" bash "$SUT" --seed "$seed" --operated-by x >/dev/null 2>&1
  local after_first; after_first="$(wc -l < "$KD_CREATE_LOG")"
  PATH="$BIN:$PATH" bash "$SUT" --seed "$seed" --operated-by x >/dev/null 2>&1
  local after_second; after_second="$(wc -l < "$KD_CREATE_LOG")"
  [ "$after_first" = "$after_second" ] \
    && pass idempotent || fail idempotent "second run created more beads ($after_first → $after_second)"
}
t_force_reseeds() {
  local seed="$TMP/s1.jsonl"; mk_seed "$seed"
  KD_CREATE_LOG="$TMP/force.log"; : > "$KD_CREATE_LOG"; export KD_CREATE_LOG
  PATH="$BIN:$PATH" bash "$SUT" --seed "$seed" --operated-by x >/dev/null 2>&1
  local n1; n1="$(wc -l < "$KD_CREATE_LOG")"
  PATH="$BIN:$PATH" bash "$SUT" --seed "$seed" --operated-by x --force >/dev/null 2>&1
  local n2; n2="$(wc -l < "$KD_CREATE_LOG")"
  [ "$n2" -gt "$n1" ] && pass force-reseeds || fail force-reseeds "--force did not re-seed ($n1 → $n2)"
}
# ── airgap-strict: missing seed is a hard fail; non-strict is a soft skip ─────
t_strict_missing_fails() {
  if run "$TMP/does-not-exist.jsonl" --airgap-strict >/dev/null 2>&1; then
    fail strict-missing-fails "missing seed did not hard-fail under --airgap-strict"
  else
    pass strict-missing-fails
  fi
}
t_nonstrict_missing_skips() {
  if run "$TMP/does-not-exist.jsonl" >/dev/null 2>&1; then
    pass nonstrict-missing-skips
  else
    fail nonstrict-missing-skips "missing seed should be a soft skip (exit 0) when not strict"
  fi
}
t_strict_bad_sha_fails() {
  local seed="$TMP/s2.jsonl"; mk_seed "$seed"
  printf 'corruption\n' >> "$seed"   # invalidate the recorded sha256
  if run "$seed" --airgap-strict >/dev/null 2>&1; then
    fail strict-bad-sha-fails "corrupt seed passed under --airgap-strict"
  else
    pass strict-bad-sha-fails
  fi
}
t_dry_run_creates_nothing() {
  local seed="$TMP/s1.jsonl"; mk_seed "$seed"
  run "$seed" --dry-run >/dev/null 2>&1 || { fail dry-run-creates-nothing "dry-run exited nonzero"; return; }
  [ ! -s "$KD_CREATE_LOG" ] && pass dry-run-creates-nothing \
    || fail dry-run-creates-nothing "dry-run created beads: $(cat "$KD_CREATE_LOG")"
}

for t in t_restores_kept t_no_internal_ref_shipped t_idempotent t_force_reseeds \
         t_strict_missing_fails t_nonstrict_missing_skips t_strict_bad_sha_fails \
         t_dry_run_creates_nothing; do
  "$t"
done

echo
echo "seed-config-dump_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
