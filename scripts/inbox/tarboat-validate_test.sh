#!/usr/bin/env bash
# tarboat-validate_test.sh — Unit tests for the gate machinery + staging gate
# in tarboat-validate.sh (Tarboat T2/G2.3, ka-0000000094).
#
# The harness as a whole runs a real bootstrap + on-box probes, so it can't run
# end-to-end in CI. But its accounting logic (a failed gate increments
# GATES_FAILED → exit 1 / NOSHIP; an all-pass run → exit 0 / SHIP) and the
# staging gate's "missing asset ⇒ fail" contract ARE pure and worth locking.
#
# We extract the helper/gate scaffolding plus gate_staging from the script and
# exercise them against synthetic staging dirs. No root, no k8s, no network.
#
# Usage:  bash scripts/inbox/tarboat-validate_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/tarboat-validate.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# Build a harness exposing the gate machinery (ok/bad/info, gate(), counters,
# json_str, emit_results) and gate_staging, all extracted verbatim from the SUT.
HARNESS="$TMP/h.sh"
{
  # ok/bad/info: stdout, but bad() MUST set LAST_DIAG (the SUT relies on it to
  # capture the failing assertion into the per-gate results), so mirror that.
  echo 'ok(){ printf "OK %s\n" "$*"; }; bad(){ LAST_DIAG="$*"; printf "BAD %s\n" "$*"; }; info(){ printf ". %s\n" "$*"; }'
  # Counters + per-gate accumulation state the gate() machinery reads/writes.
  echo 'GATES_RUN=0; GATES_FAILED=0; GATES_PASSED=0; GATES_SKIPPED=0'
  echo 'GATE_KEY=""; GATE_SKIPPED=0; LAST_DIAG=""; GATE_JSON=()'
  echo 'TARBOAT=""; NAMESPACE=""; RELEASE=""; CALVER=""; IP=""; STARTED_AT="t0"; RESULTS_JSON=""; RESULTS_EMITTED=0'
  # Box shape governs the two shape-specific gates (staging skips on byo). The
  # SUT resolves this at top level; tests set it explicitly per case. Default
  # tarball so the existing gate_staging cases keep exercising the real check.
  echo 'BOX_SHAPE="tarball"; BYO_REPO_DIR="/opt/gardenio/repo"'
  # json_str() body verbatim (the escaper the results doc depends on).
  awk '/^json_str\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # Pure functional-gate classifiers verbatim (login / inference / beads-count).
  awk '/^classify_login\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_inference\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_beads_count\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_beads_data\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # Persistence & state classifiers verbatim (ka-0000004199).
  awk '/^classify_persistence\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_cache\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_snapshot\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # Helm deployment classifiers verbatim (ka-0000003227).
  awk '/^classify_chart_lint\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_helm_upgrade\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_helm_rollback\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # Networking classifiers verbatim (ka-0000006092).
  awk '/^classify_routing\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_lb\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^classify_dns\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # Harness-bug fix helpers verbatim (ka-0000000669): robust nslookup answer
  # parsing (gate_dns) + live-release discovery (gate_helm_upgrade/rollback).
  awk '/^parse_nslookup_answer\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^helm_list_lines\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  awk '/^pick_release\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # gate() body verbatim.
  awk '/^gate\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # emit_results() body verbatim (the machine-readable results emitter).
  awk '/^emit_results\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # gate_staging() body verbatim.
  awk '/^gate_staging\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
  # gate_acceptance() body verbatim (for the bounded-timeout contract).
  awk '/^gate_acceptance\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$SUT"
} > "$HARNESS"

grep -q 'gate()' "$HARNESS"          || { echo "FATAL: gate() not extracted" >&2; exit 2; }
grep -q 'gate_staging()' "$HARNESS"  || { echo "FATAL: gate_staging() not extracted" >&2; exit 2; }
grep -q 'json_str()' "$HARNESS"      || { echo "FATAL: json_str() not extracted" >&2; exit 2; }
grep -q 'emit_results()' "$HARNESS"  || { echo "FATAL: emit_results() not extracted" >&2; exit 2; }
grep -q 'classify_login()' "$HARNESS"        || { echo "FATAL: classify_login() not extracted" >&2; exit 2; }
grep -q 'classify_inference()' "$HARNESS"    || { echo "FATAL: classify_inference() not extracted" >&2; exit 2; }
grep -q 'classify_beads_count()' "$HARNESS"  || { echo "FATAL: classify_beads_count() not extracted" >&2; exit 2; }
grep -q 'classify_beads_data()' "$HARNESS"   || { echo "FATAL: classify_beads_data() not extracted" >&2; exit 2; }
grep -q 'classify_persistence()' "$HARNESS"  || { echo "FATAL: classify_persistence() not extracted" >&2; exit 2; }
grep -q 'classify_cache()' "$HARNESS"        || { echo "FATAL: classify_cache() not extracted" >&2; exit 2; }
grep -q 'classify_snapshot()' "$HARNESS"     || { echo "FATAL: classify_snapshot() not extracted" >&2; exit 2; }
grep -q 'classify_chart_lint()' "$HARNESS"     || { echo "FATAL: classify_chart_lint() not extracted" >&2; exit 2; }
grep -q 'classify_helm_upgrade()' "$HARNESS"   || { echo "FATAL: classify_helm_upgrade() not extracted" >&2; exit 2; }
grep -q 'classify_helm_rollback()' "$HARNESS"  || { echo "FATAL: classify_helm_rollback() not extracted" >&2; exit 2; }
grep -q 'classify_routing()' "$HARNESS"      || { echo "FATAL: classify_routing() not extracted" >&2; exit 2; }
grep -q 'classify_lb()' "$HARNESS"           || { echo "FATAL: classify_lb() not extracted" >&2; exit 2; }
grep -q 'classify_dns()' "$HARNESS"          || { echo "FATAL: classify_dns() not extracted" >&2; exit 2; }
grep -q 'parse_nslookup_answer()' "$HARNESS" || { echo "FATAL: parse_nslookup_answer() not extracted" >&2; exit 2; }
grep -q 'helm_list_lines()' "$HARNESS"       || { echo "FATAL: helm_list_lines() not extracted" >&2; exit 2; }
grep -q 'pick_release()' "$HARNESS"          || { echo "FATAL: pick_release() not extracted" >&2; exit 2; }

# A fully-populated staging dir that gate_staging should accept.
good_staging() {
  local d="$1"
  mkdir -p "$d/bin" "$d/manifests" "$d/charts" "$d/images" "$d/repo/scripts"
  for f in bin/k3s bin/k3s-install.sh bin/helm bin/crane bin/zot \
           manifests/cert-manager.yaml manifests/servicemonitor-crd.yaml \
           charts/keycloak-operator-1.32.0.tgz; do
    echo x > "$d/$f"
  done
  echo x > "$d/images/gardenio-bundle-9999.oci.tar.zst"
  echo 'echo hi' > "$d/repo/scripts/bootstrap-ec2.sh"; chmod +x "$d/repo/scripts/bootstrap-ec2.sh"
  echo "x  MANIFEST" > "$d/MANIFEST"
  ( cd "$d" && : > SHA256SUMS && for g in MANIFEST bin/k3s; do sha256sum "$g" >> SHA256SUMS; done )
}

# 1. gate machinery: a passing gate increments RUN, not FAILED; exit logic SHIP.
out="$(bash -c "source '$HARNESS'; gate ok true; echo run=\$GATES_RUN failed=\$GATES_FAILED")"
if grep -q "run=1 failed=0" <<<"$out"; then pass "passing gate: run=1 failed=0"; else fail "passing gate" "$out"; fi

# 2. gate machinery: a failing gate increments FAILED.
out="$(bash -c "source '$HARNESS'; gate boom false; echo run=\$GATES_RUN failed=\$GATES_FAILED")"
if grep -q "run=1 failed=1" <<<"$out"; then pass "failing gate: run=1 failed=1"; else fail "failing gate" "$out"; fi

# 3. gate_staging PASSES on a fully-populated, checksum-consistent dir.
GD="$TMP/good"; good_staging "$GD"
if bash -c "source '$HARNESS'; TARBOAT='$GD'; REPO_DIR='$GD/repo'; gate_staging" >/dev/null 2>&1; then
  pass "gate_staging passes on a complete staging dir"
else
  fail "gate_staging complete" "expected pass"
fi

# 4. gate_staging FAILS when an offline asset is missing.
BD="$TMP/bad"; good_staging "$BD"; rm -f "$BD/bin/helm"
# checksum file no longer needs helm; keep SHA256SUMS consistent (it only lists MANIFEST+bin/k3s)
if bash -c "source '$HARNESS'; TARBOAT='$BD'; REPO_DIR='$BD/repo'; gate_staging" >/dev/null 2>&1; then
  fail "gate_staging missing-asset" "expected fail"
else
  pass "gate_staging fails when an offline asset is missing"
fi

# 5. gate_staging FAILS when SHA256SUMS is absent (integrity floor).
ND="$TMP/nosum"; good_staging "$ND"; rm -f "$ND/SHA256SUMS"
if bash -c "source '$HARNESS'; TARBOAT='$ND'; REPO_DIR='$ND/repo'; gate_staging" >/dev/null 2>&1; then
  fail "gate_staging no-sha256sums" "expected fail"
else
  pass "gate_staging fails without SHA256SUMS"
fi

# 6. gate_staging FAILS when the repo tree isn't staged (G1.2 contract).
RD="$TMP/norepo"; good_staging "$RD"; rm -f "$RD/repo/scripts/bootstrap-ec2.sh"
if bash -c "source '$HARNESS'; TARBOAT='$RD'; REPO_DIR='$RD/repo'; gate_staging" >/dev/null 2>&1; then
  fail "gate_staging no-repo" "expected fail"
else
  pass "gate_staging fails without the staged repo tree"
fi

# 6b. gate_staging SKIPS (not fails) on a BYO box — no tarball to stage-check.
#     This is the fix for the mis-filed staging regression ka-0000001460: a BYO
#     internet-clone box has no $TARBOAT/MANIFEST, so the gate must record a skip
#     (GATE_SKIPPED=1, run not release-grade) rather than false-fail. We run it
#     against an EMPTY $TARBOAT (no MANIFEST) with BOX_SHAPE=byo and assert the
#     skip flag is set and no bad() diagnostic about a missing MANIFEST fired.
BYO_TARBOAT="$TMP/byo-empty"; mkdir -p "$BYO_TARBOAT"
out="$(bash -c "source '$HARNESS'; BOX_SHAPE=byo; TARBOAT='$BYO_TARBOAT'; REPO_DIR='/opt/gardenio/repo'; gate_staging; echo skipped=\$GATE_SKIPPED diag=\$LAST_DIAG")"
if grep -q "skipped=1" <<<"$out" && ! grep -qi "no MANIFEST" <<<"$out"; then
  pass "gate_staging skips on a BYO box (no false MANIFEST failure)"
else
  fail "gate_staging byo-skip" "$out"
fi

# 6c. gate_staging STILL FAILS on a tarball box with no MANIFEST (byo skip must
#     not leak into the real tarball path — a genuinely-missing tarball is a
#     loud failure, never a silent skip).
out="$(bash -c "source '$HARNESS'; BOX_SHAPE=tarball; TARBOAT='$BYO_TARBOAT'; REPO_DIR='$BYO_TARBOAT/repo'; gate_staging; echo rc=\$? skipped=\$GATE_SKIPPED" 2>&1)"
if grep -q "skipped=0" <<<"$out" && grep -qi "no MANIFEST" <<<"$out"; then
  pass "gate_staging still fails loudly on a tarball box missing its MANIFEST"
else
  fail "gate_staging tarball-still-fails" "$out"
fi

# 6d. gate_acceptance BOUNDS the smoke: a hung acceptance-smoke.sh must fail
#     the gate within ACCEPTANCE_TIMEOUT, not eat the sweep's whole CI task
#     budget (ka-0000002999: a parser hang burned the 60m timeout, SIGKILL
#     skipped the teardown trap, and the EC2 box leaked). Fake repo whose
#     acceptance-smoke.sh sleeps forever; 2s budget; must return fail + the
#     TIMED OUT diagnostic well before the sleep would end.
HANG_REPO="$TMP/hang-repo"; mkdir -p "$HANG_REPO/scripts/inbox"
printf '#!/bin/sh\nsleep 300\n' > "$HANG_REPO/scripts/inbox/acceptance-smoke.sh"
chmod +x "$HANG_REPO/scripts/inbox/acceptance-smoke.sh"
out="$(timeout 60 bash -c "source '$HARNESS'; REPO_DIR='$HANG_REPO'; IP=127.0.0.1; NAMESPACE=t; RELEASE=t; ACCEPTANCE_TIMEOUT=2; gate_acceptance; echo rc=\$? diag=\$LAST_DIAG" 2>&1)"
if grep -q "rc=1" <<<"$out" && grep -qi "TIMED OUT" <<<"$out"; then
  pass "gate_acceptance kills a hung smoke and fails loudly (bounded, no sweep-wide hang)"
else
  fail "gate_acceptance hung-smoke timeout" "$out"
fi

# 6e. gate_acceptance still propagates a fast real failure (exit 1 ≠ timeout).
FAILFAST_REPO="$TMP/failfast-repo"; mkdir -p "$FAILFAST_REPO/scripts/inbox"
printf '#!/bin/sh\nexit 1\n' > "$FAILFAST_REPO/scripts/inbox/acceptance-smoke.sh"
chmod +x "$FAILFAST_REPO/scripts/inbox/acceptance-smoke.sh"
out="$(bash -c "source '$HARNESS'; REPO_DIR='$FAILFAST_REPO'; IP=127.0.0.1; NAMESPACE=t; RELEASE=t; gate_acceptance; echo rc=\$? diag=\$LAST_DIAG" 2>&1)"
if grep -q "rc=1" <<<"$out" && ! grep -qi "TIMED OUT" <<<"$out"; then
  pass "gate_acceptance propagates a real smoke failure (not misreported as timeout)"
else
  fail "gate_acceptance real-failure passthrough" "$out"
fi

# 6f. gate_acceptance passes a fast green smoke through untouched.
OK_REPO="$TMP/ok-repo"; mkdir -p "$OK_REPO/scripts/inbox"
printf '#!/bin/sh\nexit 0\n' > "$OK_REPO/scripts/inbox/acceptance-smoke.sh"
chmod +x "$OK_REPO/scripts/inbox/acceptance-smoke.sh"
out="$(bash -c "source '$HARNESS'; REPO_DIR='$OK_REPO'; IP=127.0.0.1; NAMESPACE=t; RELEASE=t; gate_acceptance; echo rc=\$?" 2>&1)"
if grep -q "rc=0" <<<"$out"; then
  pass "gate_acceptance passes a green smoke (timeout wrapper transparent)"
else
  fail "gate_acceptance green passthrough" "$out"
fi

# 7. gate() records a SKIP (GATE_SKIPPED=1) as skip, not pass/fail.
out="$(bash -c "source '$HARNESS'; skipper(){ GATE_SKIPPED=1; return 0; }; gate s skipper; echo run=\$GATES_RUN pass=\$GATES_PASSED fail=\$GATES_FAILED skip=\$GATES_SKIPPED")"
if grep -q "run=1 pass=0 fail=0 skip=1" <<<"$out"; then pass "skipped gate: counted as skip, not pass/fail"; else fail "skipped gate" "$out"; fi

# 8. gate() emits one JSON object per gate, with key/status/duration fields.
out="$(bash -c "source '$HARNESS'; GATE_KEY=staging; gate 'air-gap' true; printf '%s' \"\${GATE_JSON[0]}\"")"
grep -q '"key":"staging"' <<<"$out" && grep -q '"status":"pass"' <<<"$out" \
  && grep -q '"duration_seconds":' <<<"$out" \
  && pass "gate() emits a per-gate JSON object (key+status+duration)" \
  || fail "gate JSON object" "$out"

# 9. A failing gate captures the bad() diagnostic into the JSON "message".
out="$(bash -c "source '$HARNESS'; boom(){ bad 'staged asset X missing'; return 1; }; GATE_KEY=staging; gate 'air-gap' boom; printf '%s' \"\${GATE_JSON[0]}\"")"
if grep -q '"status":"fail"' <<<"$out" && grep -q 'staged asset X missing' <<<"$out"; then
  pass "failing gate captures the assertion into JSON message"
else
  fail "failing gate message" "$out"
fi

# 10. json_str escapes quotes, backslashes, and newlines (valid JSON strings).
out="$(bash -c "source '$HARNESS'; json_str 'a\"b\\c'")"
if [ "$out" = '"a\"b\\c"' ]; then pass "json_str escapes quote+backslash"; else fail "json_str escape" "$out"; fi

# 11. emit_results to a file: valid JSON, release_grade reflects skips.
#     (a) all-pass, no skip → release_grade true.
RJ="$TMP/r1.json"
bash -c "source '$HARNESS'; RESULTS_JSON='$RJ'; GATE_KEY=staging; gate g true >/dev/null; emit_results 0" >/dev/null 2>&1
if command -v python3 >/dev/null 2>&1; then
  if python3 -c "import json,sys; d=json.load(open('$RJ')); sys.exit(0 if d['release_grade'] is True and d['overall']=='pass' and d['gates_skipped']==0 and len(d['gates'])==1 else 1)"; then
    pass "emit_results: all-pass run is valid JSON + release_grade=true"
  else
    fail "emit_results all-pass" "$(cat "$RJ")"
  fi
  #     (b) a skipped gate → green but release_grade false.
  RJ2="$TMP/r2.json"
  bash -c "source '$HARNESS'; RESULTS_JSON='$RJ2'; sk(){ GATE_SKIPPED=1; return 0; }; GATE_KEY=inference; gate g sk >/dev/null; emit_results 0" >/dev/null 2>&1
  if python3 -c "import json,sys; d=json.load(open('$RJ2')); sys.exit(0 if d['release_grade'] is False and d['overall']=='pass' and d['gates_skipped']==1 else 1)"; then
    pass "emit_results: a skipped gate makes release_grade=false despite green"
  else
    fail "emit_results skip-not-release-grade" "$(cat "$RJ2")"
  fi
  #     (c) a failed gate → overall fail, release_grade false.
  RJ3="$TMP/r3.json"
  bash -c "source '$HARNESS'; RESULTS_JSON='$RJ3'; bm(){ bad oops; return 1; }; GATE_KEY=acceptance; gate g bm >/dev/null; emit_results 1" >/dev/null 2>&1
  if python3 -c "import json,sys; d=json.load(open('$RJ3')); sys.exit(0 if d['overall']=='fail' and d['release_grade'] is False and d['gates'][0]['status']=='fail' else 1)"; then
    pass "emit_results: a failed gate → overall=fail, release_grade=false"
  else
    fail "emit_results fail" "$(cat "$RJ3")"
  fi
else
  echo "SKIP: python3 unavailable — emit_results JSON-validity assertions skipped"
fi

# ── Functional "it actually works" classifiers ───────────────────────────────
# These are the pure judges behind gate_login / gate_inference (--inference-live)
# / gate_beads_data. They classify text handed to them (no kubectl/curl/jq), so
# the operator's "prove a fresh box ACTUALLY works" bar (decision ka-0000006524)
# is locked here without a box.

# classify_login: a usable Keycloak Sign-in page → ok / 0.
out="$(bash -c "source '$HARNESS'; classify_login '<form id=\"kc-form-login\" action=\"/realms/x/login-actions/authenticate\"><input name=\"password\"></form>'")"
[ "$out" = ok ] && pass "classify_login: kc-form-login page → ok" || fail "classify_login ok-form" "$out"

out="$(bash -c "source '$HARNESS'; classify_login 'totally unrelated 200 body with name=\"password\" field'")"
[ "$out" = ok ] && pass "classify_login: any password form → ok" || fail "classify_login password-field" "$out"

# classify_login: the headline gasbox breakage (invalid_scope) → broken / 1.
if bash -c "source '$HARNESS'; classify_login 'error: invalid_scope: Invalid scopes: openid email profile groups'" >/dev/null; then
  fail "classify_login invalid_scope" "expected non-zero"
else
  out="$(bash -c "source '$HARNESS'; classify_login 'error: invalid_scope'" || true)"
  [ "$out" = broken ] && pass "classify_login: invalid_scope → broken (the gasbox headline breakage)" || fail "classify_login invalid_scope verdict" "$out"
fi

# classify_login: a body with neither a form nor an error → no-form / 1.
if bash -c "source '$HARNESS'; classify_login '<html>nothing useful here</html>'" >/dev/null; then
  fail "classify_login no-form" "expected non-zero"
else
  pass "classify_login: no form, no error → non-zero (login not proven usable)"
fi

# classify_inference: HTTP 200 + real assistant message → ok / 0.
out="$(bash -c "source '$HARNESS'; classify_inference 200 '{\"id\":\"msg_014ZFvp\",\"type\":\"message\",\"content\":[{\"text\":\"PONG\"}]}'")"
[ "$out" = ok ] && pass "classify_inference: 200 + msg_ assistant message → ok" || fail "classify_inference ok" "$out"

# classify_inference: 200 but an error envelope → error-body / 1 (a Service-up
# probe stays green here; this catches the real-call-fails-anyway case).
if bash -c "source '$HARNESS'; classify_inference 200 '{\"error\":{\"message\":\"litellm upstream\"}}'" >/dev/null; then
  fail "classify_inference 200-error" "expected non-zero"
else
  out="$(bash -c "source '$HARNESS'; classify_inference 200 '{\"error\":{}}'" || true)"
  [ "$out" = error-body ] && pass "classify_inference: 200 + error envelope → error-body (round-trip failed)" || fail "classify_inference error verdict" "$out"
fi

# classify_inference: a non-200 (e.g. netpol-blocked 000/5xx) → http-<status> / 1.
if bash -c "source '$HARNESS'; classify_inference 000 ''" >/dev/null; then
  fail "classify_inference http-000" "expected non-zero"
else
  out="$(bash -c "source '$HARNESS'; classify_inference 000 ''" || true)"
  [ "$out" = http-000 ] && pass "classify_inference: http 000 (coopmux<->agent netpol blocked) → non-zero" || fail "classify_inference http-000 verdict" "$out"
fi

# classify_beads_count: a positive count → echoes it / 0.
out="$(bash -c "source '$HARNESS'; classify_beads_count '176'")"
[ "$out" = 176 ] && pass "classify_beads_count: 176 → ok (echoes count)" || fail "classify_beads_count positive" "$out"

# classify_beads_count: zero rows → 0 / 1 (daemon up but unseeded/broken path).
if bash -c "source '$HARNESS'; classify_beads_count '0'" >/dev/null; then
  fail "classify_beads_count zero" "expected non-zero"
else
  pass "classify_beads_count: 0 rows → non-zero (beads path broken/unseeded)"
fi

# classify_beads_count: empty / unreachable psql output → no-count / 1.
if bash -c "source '$HARNESS'; classify_beads_count ''" >/dev/null; then
  fail "classify_beads_count empty" "expected non-zero"
else
  out="$(bash -c "source '$HARNESS'; classify_beads_count ''" || true)"
  [ "$out" = no-count ] && pass "classify_beads_count: empty psql output → no-count (postgres unreachable)" || fail "classify_beads_count empty verdict" "$out"
fi

# classify_beads_data — the seeder-aware daemon<->postgres verdict (ka-0000005066).
# Args: <count-verdict> <daemon-ready> <seeder-present>.

# Populated table: any positive count → pass, echo the count (regardless of the
# other inputs — data present is unambiguous).
out="$(bash -c "source '$HARNESS'; classify_beads_data '176' ready seeder")"
[ "$out" = 176 ] && pass "classify_beads_data: 176 rows → pass (echoes count)" || fail "classify_beads_data populated" "$out"

# THE ROOT-CAUSE CASE: empty table, daemon Ready, NO seeder on the box → PASS.
# This is the false-failure the old gate red'd on a healthy Slack-less BYO box.
if out="$(bash -c "source '$HARNESS'; classify_beads_data '0' ready no-seeder")"; then
  [ "$out" = empty-no-seeder ] && pass "classify_beads_data: 0 rows + daemon Ready + no seeder → pass (empty-no-seeder)" || fail "classify_beads_data empty-no-seeder verdict" "$out"
else
  fail "classify_beads_data empty-no-seeder" "expected exit 0 (pass), got non-zero"
fi

# Empty table but a seeder IS present → real seed regression → fail (unseeded).
if bash -c "source '$HARNESS'; classify_beads_data '0' ready seeder" >/dev/null; then
  fail "classify_beads_data unseeded" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_beads_data '0' ready seeder" || true)"
  [ "$out" = unseeded ] && pass "classify_beads_data: 0 rows + seeder present → fail (unseeded — controller wrote nothing)" || fail "classify_beads_data unseeded verdict" "$out"
fi

# Empty/unreadable AND daemon NOT Ready → the path is genuinely down → fail.
if bash -c "source '$HARNESS'; classify_beads_data '0' not-ready no-seeder" >/dev/null; then
  fail "classify_beads_data path-broken" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_beads_data '0' not-ready no-seeder" || true)"
  [ "$out" = path-broken ] && pass "classify_beads_data: 0 rows + daemon NOT Ready → fail (path-broken)" || fail "classify_beads_data path-broken verdict" "$out"
fi

# no-count (count query itself failed) while daemon Ready → schema/DB anomaly → fail.
if bash -c "source '$HARNESS'; classify_beads_data 'no-count' ready no-seeder" >/dev/null; then
  fail "classify_beads_data no-count" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_beads_data 'no-count' ready no-seeder" || true)"
  [ "$out" = no-count ] && pass "classify_beads_data: no-count + daemon Ready → fail (schema/DB anomaly)" || fail "classify_beads_data no-count verdict" "$out"
fi

# no-count with daemon NOT Ready → path-broken wins (the daemon can't reach pg).
if bash -c "source '$HARNESS'; classify_beads_data 'no-count' not-ready seeder" >/dev/null; then
  fail "classify_beads_data no-count+not-ready" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_beads_data 'no-count' not-ready seeder" || true)"
  [ "$out" = path-broken ] && pass "classify_beads_data: no-count + daemon NOT Ready → fail (path-broken, precedence)" || fail "classify_beads_data no-count+not-ready verdict" "$out"
fi

# The new functional gates flow through the SAME gate() accounting + JSON, so a
# failing functional gate must increment GATES_FAILED and emit a fail object.
out="$(bash -c "source '$HARNESS'; lf(){ bad 'login unusable: invalid_scope'; return 1; }; GATE_KEY=login; gate 'login completes' lf; printf '%s' \"\${GATE_JSON[0]}\"")"
grep -q '"key":"login"' <<<"$out" && grep -q '"status":"fail"' <<<"$out" && grep -q 'invalid_scope' <<<"$out" \
  && pass "functional gate flows through gate(): login fail → fail JSON with diagnostic" \
  || fail "login gate JSON" "$out"

# ── classify_persistence (ka-0000004199: DB persistence gate) ────────────────
# Sentinel read back AND a PVC-backed data dir → durable pass (verdict ok).
out="$(bash -c "source '$HARNESS'; classify_persistence 'tok123' pvc")" \
  && [ "$out" = ok ] && pass "classify_persistence: readback + pvc → ok (durable)" \
  || fail "classify_persistence pvc" "$out"

# Empty readback → the write/read round-trip failed → write-failed (fail).
if bash -c "source '$HARNESS'; classify_persistence '' pvc" >/dev/null; then
  fail "classify_persistence write-failed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_persistence '' pvc" || true)"
  [ "$out" = write-failed ] && pass "classify_persistence: empty readback → fail (write-failed)" || fail "classify_persistence write-failed verdict" "$out"
fi

# Readback ok but emptyDir data dir → round-trips but NOT durable → ephemeral (fail).
if bash -c "source '$HARNESS'; classify_persistence 'tok' emptydir" >/dev/null; then
  fail "classify_persistence ephemeral" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_persistence 'tok' emptydir" || true)"
  [ "$out" = ephemeral ] && pass "classify_persistence: readback + emptyDir → fail (ephemeral, not durable)" || fail "classify_persistence ephemeral verdict" "$out"
fi

# Readback ok but unknown backing → cannot assert durability → unknown-backing (fail).
if bash -c "source '$HARNESS'; classify_persistence 'tok' unknown" >/dev/null; then
  fail "classify_persistence unknown-backing" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_persistence 'tok' ''" || true)"
  [ "$out" = unknown-backing ] && pass "classify_persistence: readback + unknown backing → fail (unknown-backing)" || fail "classify_persistence unknown-backing verdict" "$out"
fi

# ── classify_cache (ka-0000004199: cache-state gate) ─────────────────────────
# No in-chart Redis on the box → cache-state N/A → absent (pass). The optional-
# component false-failure this gate is built to avoid.
out="$(bash -c "source '$HARNESS'; classify_cache '' ''")" \
  && [ "$out" = absent ] && pass "classify_cache: no in-chart Redis → pass (absent, N/A)" \
  || fail "classify_cache absent" "$out"

# Redis present AND SET/GET round-trip returned our value → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_cache present ok")" \
  && [ "$out" = ok ] && pass "classify_cache: present + round-trip ok → pass (ok)" \
  || fail "classify_cache ok" "$out"

# Redis present but round-trip did not return the value → roundtrip-failed (fail).
if bash -c "source '$HARNESS'; classify_cache present bad" >/dev/null; then
  fail "classify_cache roundtrip-failed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_cache present bad" || true)"
  [ "$out" = roundtrip-failed ] && pass "classify_cache: present but round-trip failed → fail (roundtrip-failed)" || fail "classify_cache roundtrip-failed verdict" "$out"
fi

# ── classify_snapshot (ka-0000004199: snapshot-integrity gate) ───────────────
# A non-empty pg_restore -l TOC count → restorable archive → echoes count (pass).
out="$(bash -c "source '$HARNESS'; classify_snapshot '284'")" \
  && [ "$out" = 284 ] && pass "classify_snapshot: 284 TOC entries → pass (echoes count)" \
  || fail "classify_snapshot populated" "$out"

# Zero TOC entries → pg_dump/pg_restore produced nothing enumerable → empty (fail).
if bash -c "source '$HARNESS'; classify_snapshot '0'" >/dev/null; then
  fail "classify_snapshot empty" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_snapshot '0'" || true)"
  [ "$out" = empty ] && pass "classify_snapshot: 0 TOC entries → fail (empty/corrupt archive)" || fail "classify_snapshot empty verdict" "$out"
fi

# Non-numeric/empty output (pg_dump aborted) → empty (fail).
if bash -c "source '$HARNESS'; classify_snapshot ''" >/dev/null; then
  fail "classify_snapshot no-output" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_snapshot ''" || true)"
  [ "$out" = empty ] && pass "classify_snapshot: empty output → fail (empty)" || fail "classify_snapshot no-output verdict" "$out"
fi

# The new persistence/state gates flow through the SAME gate() accounting + JSON.
out="$(bash -c "source '$HARNESS'; pf(){ bad 'data dir is an emptyDir — NOT durable'; return 1; }; GATE_KEY=db_persistence; gate 'DB persistence' pf; printf '%s' \"\${GATE_JSON[0]}\"")"
grep -q '"key":"db_persistence"' <<<"$out" && grep -q '"status":"fail"' <<<"$out" && grep -q 'emptyDir' <<<"$out" \
  && pass "persistence gate flows through gate(): fail → fail JSON with diagnostic" \
  || fail "db_persistence gate JSON" "$out"

# ── classify_chart_lint (ka-0000003227: chart-lint gate) ─────────────────────
# helm lint summary "0 chart(s) failed" → clean → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_chart_lint '0'")" \
  && [ "$out" = ok ] && pass "classify_chart_lint: 0 failed → pass (ok)" \
  || fail "classify_chart_lint ok" "$out"

# "1 chart(s) failed" → lint errors → failed (fail).
if bash -c "source '$HARNESS'; classify_chart_lint '1'" >/dev/null; then
  fail "classify_chart_lint failed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_chart_lint '1'" || true)"
  [ "$out" = failed ] && pass "classify_chart_lint: 1 failed → fail (failed)" || fail "classify_chart_lint failed verdict" "$out"
fi

# Empty (no parseable summary — lint never ran / chart missing) → lint-unknown (fail).
# Never a silent pass: an unparseable lint must not masquerade as a clean chart.
if bash -c "source '$HARNESS'; classify_chart_lint ''" >/dev/null; then
  fail "classify_chart_lint lint-unknown" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_chart_lint ''" || true)"
  [ "$out" = lint-unknown ] && pass "classify_chart_lint: no summary → fail (lint-unknown, never silent pass)" || fail "classify_chart_lint lint-unknown verdict" "$out"
fi

# ── classify_helm_upgrade (ka-0000003227: helm-upgrade gate) ─────────────────
# Deployed release AND dry-run rendered → upgradeable → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_helm_upgrade deployed ok")" \
  && [ "$out" = ok ] && pass "classify_helm_upgrade: deployed + dry-run ok → pass (ok)" \
  || fail "classify_helm_upgrade ok" "$out"

# No release status (release absent) → no-release (fail).
if bash -c "source '$HARNESS'; classify_helm_upgrade '' ''" >/dev/null; then
  fail "classify_helm_upgrade no-release" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_helm_upgrade '' ''" || true)"
  [ "$out" = no-release ] && pass "classify_helm_upgrade: empty status → fail (no-release)" || fail "classify_helm_upgrade no-release verdict" "$out"
fi

# Release exists but status ≠ deployed (mid-operation) → not-deployed (fail).
if bash -c "source '$HARNESS'; classify_helm_upgrade pending-upgrade ''" >/dev/null; then
  fail "classify_helm_upgrade not-deployed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_helm_upgrade pending-upgrade ''" || true)"
  [ "$out" = not-deployed ] && pass "classify_helm_upgrade: pending-upgrade → fail (not-deployed)" || fail "classify_helm_upgrade not-deployed verdict" "$out"
fi

# Deployed but dry-run did NOT render → chart no longer templates → dryrun-failed (fail).
if bash -c "source '$HARNESS'; classify_helm_upgrade deployed bad" >/dev/null; then
  fail "classify_helm_upgrade dryrun-failed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_helm_upgrade deployed bad" || true)"
  [ "$out" = dryrun-failed ] && pass "classify_helm_upgrade: deployed + dry-run bad → fail (dryrun-failed)" || fail "classify_helm_upgrade dryrun-failed verdict" "$out"
fi

# ── classify_helm_rollback (ka-0000003227: helm-rollback gate) ───────────────
# ≥2 revisions AND latest deployed → a prior target exists → rollback-ready (pass).
out="$(bash -c "source '$HARNESS'; classify_helm_rollback '3' deployed")" \
  && [ "$out" = rollback-ready ] && pass "classify_helm_rollback: 3 revs + deployed → pass (rollback-ready)" \
  || fail "classify_helm_rollback rollback-ready" "$out"

# Exactly 1 deployed revision (fresh install) → machinery present, no target → single-revision (pass).
out="$(bash -c "source '$HARNESS'; classify_helm_rollback '1' deployed")" \
  && [ "$out" = single-revision ] && pass "classify_helm_rollback: 1 rev + deployed → pass (single-revision, fresh)" \
  || fail "classify_helm_rollback single-revision" "$out"

# 0/empty history → no ledger → cannot roll back → no-history (fail).
if bash -c "source '$HARNESS'; classify_helm_rollback '0' ''" >/dev/null; then
  fail "classify_helm_rollback no-history" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_helm_rollback '' ''" || true)"
  [ "$out" = no-history ] && pass "classify_helm_rollback: empty history → fail (no-history)" || fail "classify_helm_rollback no-history verdict" "$out"
fi

# Latest revision not deployed (stuck) → not rollback-safe → not-deployed (fail).
if bash -c "source '$HARNESS'; classify_helm_rollback '2' pending-rollback" >/dev/null; then
  fail "classify_helm_rollback not-deployed" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_helm_rollback '2' failed" || true)"
  [ "$out" = not-deployed ] && pass "classify_helm_rollback: latest not deployed → fail (not-deployed)" || fail "classify_helm_rollback not-deployed verdict" "$out"
fi

# The helm deployment gates flow through the SAME gate() accounting + JSON.
out="$(bash -c "source '$HARNESS'; hf(){ bad 'helm lint reported 1 failed chart(s)'; return 1; }; GATE_KEY=chart_lint; gate 'chart lint' hf; printf '%s' \"\${GATE_JSON[0]}\"")"
grep -q '"key":"chart_lint"' <<<"$out" && grep -q '"status":"fail"' <<<"$out" && grep -q 'helm lint' <<<"$out" \
  && pass "chart_lint gate flows through gate(): fail → fail JSON with diagnostic" \
  || fail "chart_lint gate JSON" "$out"

# ── classify_routing (ka-0000006092: routing gate) ───────────────────────────
# Every IngressRoute ref resolved (0 dangling of N checked) → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_routing 0 7")" \
  && [ "$out" = ok ] && pass "classify_routing: 0 dangling of 7 → ok (all routes resolve)" \
  || fail "classify_routing ok" "$out"

# No IngressRoutes to check (edge-owned routing / CRD absent) → no-routes (pass).
out="$(bash -c "source '$HARNESS'; classify_routing 0 0")" \
  && [ "$out" = no-routes ] && pass "classify_routing: 0 checked → pass (no-routes, edge-owned)" \
  || fail "classify_routing no-routes" "$out"

# A dangling ref (route points at a missing Service) → dangling-<N> (fail). This
# is the silent-404 black-hole a "route object exists" check misses.
if bash -c "source '$HARNESS'; classify_routing 2 7" >/dev/null; then
  fail "classify_routing dangling" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_routing 2 7" || true)"
  [ "$out" = dangling-2 ] && pass "classify_routing: 2 dangling → fail (dangling-2, Hosts 404)" || fail "classify_routing dangling verdict" "$out"
fi

# ── classify_lb (ka-0000006092: load-balancing gate) ─────────────────────────
# Service present with >=1 ready endpoint → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_lb present 3")" \
  && [ "$out" = ok ] && pass "classify_lb: present + 3 ready endpoints → ok (live backend)" \
  || fail "classify_lb ok" "$out"

# Service absent → not this gate's failure → absent (pass; skipped upstream).
out="$(bash -c "source '$HARNESS'; classify_lb '' 0")" \
  && [ "$out" = absent ] && pass "classify_lb: absent Service → pass (absent, optional component)" \
  || fail "classify_lb absent" "$out"

# Service present but 0 ready endpoints → no-endpoints (fail). The 503 black-hole
# a Service-exists check sails through.
if bash -c "source '$HARNESS'; classify_lb present 0" >/dev/null; then
  fail "classify_lb no-endpoints" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_lb present 0" || true)"
  [ "$out" = no-endpoints ] && pass "classify_lb: present + 0 ready endpoints → fail (no-endpoints, 503 black-hole)" || fail "classify_lb no-endpoints verdict" "$out"
fi

# ── classify_dns (ka-0000006092: DNS gate) ───────────────────────────────────
# Resolved IP matches the expected ClusterIP → ok (pass).
out="$(bash -c "source '$HARNESS'; classify_dns 10.43.0.10 10.43.0.10")" \
  && [ "$out" = ok ] && pass "classify_dns: resolved == ClusterIP → ok (DNS healthy)" \
  || fail "classify_dns ok" "$out"

# Headless Service (empty expected) but a resolution succeeded → any answer counts → ok.
out="$(bash -c "source '$HARNESS'; classify_dns 10.42.1.5 ''")" \
  && [ "$out" = ok ] && pass "classify_dns: resolved + headless (no expected IP) → ok (any answer counts)" \
  || fail "classify_dns headless" "$out"

# Resolved to the WRONG address → mismatch (fail).
if bash -c "source '$HARNESS'; classify_dns 10.43.9.9 10.43.0.10" >/dev/null; then
  fail "classify_dns mismatch" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_dns 10.43.9.9 10.43.0.10" || true)"
  [ "$out" = mismatch ] && pass "classify_dns: resolved != ClusterIP → fail (mismatch, stale record)" || fail "classify_dns mismatch verdict" "$out"
fi

# No answer at all → unresolved (fail). Cluster DNS is down.
if bash -c "source '$HARNESS'; classify_dns '' 10.43.0.10" >/dev/null; then
  fail "classify_dns unresolved" "expected non-zero (fail)"
else
  out="$(bash -c "source '$HARNESS'; classify_dns '' 10.43.0.10" || true)"
  [ "$out" = unresolved ] && pass "classify_dns: empty resolution → fail (unresolved, CoreDNS down)" || fail "classify_dns unresolved verdict" "$out"
fi

# ── parse_nslookup_answer (ka-0000000669: gate_dns parse bug) ────────────────
# The REGRESSION: the old `awk … | tr -dc '0-9.:'` pipeline stripped newlines, so
# a stray numeric token on stdout (the probe pod name's $$ PID) concatenated onto
# the real ClusterIP — "10.43.184.36" + "45232" -> "10.43.184.3645232" — and the
# gate false-failed a HEALTHY box with a "mismatch". The exact observed stdout was
# an answer Address line followed by the kubectl-run pod-name/status echo.
NSLOOKUP_OK="$(printf 'Server:\t10.43.0.10\nAddress:\t10.43.0.10:53\n\nName:\tgardenio-beads.gardenio.svc.cluster.local\nAddress: 10.43.184.36\n')"
out="$(printf '%s\n' "$NSLOOKUP_OK" | bash -c "source '$HARNESS'; parse_nslookup_answer")"
[ "$out" = "10.43.184.36" ] && pass "parse_nslookup_answer: takes the answer A record, drops the DNS-server header address" || fail "parse_nslookup_answer answer" "$out"

# The precise failure shape from CI run bd927b53d24c4b: a trailing pod-name token on
# stdout. The robust parser must ignore it (old pipeline merged it into the IP).
NSLOOKUP_NOISE="$(printf 'Server:\t10.43.0.10\nAddress:\t10.43.0.10:53\n\nName:\tgardenio-beads.gardenio.svc.cluster.local\nAddress: 10.43.184.36\ntarboat-dns-probe-45232\n')"
out="$(printf '%s\n' "$NSLOOKUP_NOISE" | bash -c "source '$HARNESS'; parse_nslookup_answer")"
[ "$out" = "10.43.184.36" ] && pass "parse_nslookup_answer: ignores trailing pod-name/PID noise (no newline-strip concat)" || fail "parse_nslookup_answer noise (regression)" "$out"

# A port on the answer Address line (busybox variants) is stripped, not merged.
out="$(printf 'Name:\tx.svc\nAddress: 10.43.184.36:53\n' | bash -c "source '$HARNESS'; parse_nslookup_answer")"
[ "$out" = "10.43.184.36" ] && pass "parse_nslookup_answer: strips :port from answer Address" || fail "parse_nslookup_answer port" "$out"

# NXDOMAIN / no answer section → empty output → classify_dns reads it as unresolved.
out="$(printf 'Server:\t10.43.0.10\nAddress:\t10.43.0.10:53\n\n' | bash -c "source '$HARNESS'; parse_nslookup_answer")"
[ -z "$out" ] && pass "parse_nslookup_answer: no answer section → empty (unresolved)" || fail "parse_nslookup_answer empty" "$out"

# End-to-end: the noisy nslookup output parsed then classified against the true
# ClusterIP must be ok — the whole point of the fix (was a phantom mismatch).
resolved="$(printf '%s\n' "$NSLOOKUP_NOISE" | bash -c "source '$HARNESS'; parse_nslookup_answer")"
out="$(bash -c "source '$HARNESS'; classify_dns '$resolved' 10.43.184.36")" \
  && [ "$out" = ok ] && pass "gate_dns end-to-end: noisy nslookup + true ClusterIP → ok (no phantom mismatch)" \
  || fail "gate_dns end-to-end" "$out"

# ── helm_list_lines + pick_release (ka-0000000669: helm gates release discovery) ─
# helm list -A -o json → "NAME NAMESPACE STATUS CHART" lines, jq-free.
HELM_JSON='[{"name":"gardenio","namespace":"gardenio","revision":"3","status":"deployed","chart":"gardenio-2026.238.7","app_version":"2026.238.7"}]'
out="$(printf '%s' "$HELM_JSON" | bash -c "source '$HARNESS'; helm_list_lines")"
[ "$out" = "gardenio gardenio deployed gardenio-2026.238.7" ] && pass "helm_list_lines: parses helm list -A json to fielded lines" || fail "helm_list_lines" "$out"

# Exact name+namespace match wins (fast path, unchanged behavior).
out="$(printf 'gardenio gardenio deployed gardenio-2026.238.7\n' | bash -c "source '$HARNESS'; pick_release gardenio gardenio")" \
  && [ "$out" = "gardenio gardenio deployed" ] && pass "pick_release: exact name@ns match → chosen" || fail "pick_release exact" "$out"

# THE BUG: workloads healthy but helm recorded the release under a DIFFERENT
# name/namespace than assumed → discover it via the sole gardenio-chart release.
DRIFT="$(printf 'gardenio-inbox byo deployed gardenio-2026.238.7\nkeycloak-operator operators deployed keycloak-operator-1.2.3\n')"
out="$(printf '%s\n' "$DRIFT" | bash -c "source '$HARNESS'; pick_release gardenio gardenio")" \
  && [ "$out" = "gardenio-inbox byo deployed" ] && pass "pick_release: no exact match → sole gardenio-chart release discovered (the real BYO bug)" || fail "pick_release drift discovery" "$out"

# Exactly one release total, non-matching name → that one (best effort).
out="$(printf 'weird-name odd deployed gardenio-2026.238.7\n' | bash -c "source '$HARNESS'; pick_release gardenio gardenio")" \
  && [ "$out" = "weird-name odd deployed" ] && pass "pick_release: sole gardenio-chart release chosen regardless of name" || fail "pick_release sole" "$out"

# No releases at all → return 1, empty (gate then reports no-release as before).
if printf '' | bash -c "source '$HARNESS'; pick_release gardenio gardenio" | grep -q .; then
  fail "pick_release empty" "expected no output"
else
  pass "pick_release: no releases → nothing chosen (gate keeps no-release)"
fi

# Ambiguous (2+ gardenio-chart releases, no exact match) → nothing chosen.
AMBIG="$(printf 'g1 a deployed gardenio-1\ng2 b deployed gardenio-2\n')"
if printf '%s\n' "$AMBIG" | bash -c "source '$HARNESS'; pick_release gardenio gardenio" | grep -q .; then
  fail "pick_release ambiguous" "expected no output when >1 gardenio-chart release and no exact match"
else
  pass "pick_release: multiple gardenio-chart releases, no exact match → nothing chosen (ambiguous)"
fi

# The new networking gates flow through the SAME gate() accounting + JSON.
out="$(bash -c "source '$HARNESS'; nf(){ bad 'IngressRoute foo references Service bar which does not exist'; return 1; }; GATE_KEY=routing; gate 'routing' nf; printf '%s' \"\${GATE_JSON[0]}\"")"
grep -q '"key":"routing"' <<<"$out" && grep -q '"status":"fail"' <<<"$out" && grep -q 'does not exist' <<<"$out" \
  && pass "routing gate flows through gate(): fail → fail JSON with diagnostic" \
  || fail "routing gate JSON" "$out"

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
