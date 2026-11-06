#!/usr/bin/env bash
# test-skip-visibility-lint.sh — hermetic gate for the e2e skip-visibility
# protocol (ka-0000000989).
#
# THE BUG THIS GUARDS AGAINST (green-by-skip):
#   run_module() in run-shard.sh records ANY module `exit 0` as MODULE PASSED.
#   Every module that self-skips (prerequisite binary absent, opt-in unset, …)
#   does so with `exit 0` — which is load-bearing, because the gvisor suite and
#   other modules are also run DIRECTLY, where a non-zero exit
#   IS a failure. So a self-skip was previously INDISTINGUISHABLE from a real
#   pass: 0 assertions ran, yet the module counted toward the green tally the
#   release gate rests on. The confirmed live instance was test-claudemore-hooks
#   (a HARD shard-0 gate) skipping on EVERY CI run because no `claude` binary is
#   ever installed — asserting nothing while prod deploy is E2E-gated.
#
# THE FIX (ka-0000000989): run_module hands each module a per-invocation sentinel
# path via E2E_SKIP_SENTINEL; the module's skip helpers write their reason there
# right before `exit 0`; run_module classifies exit-0-with-sentinel as a distinct
# SKIPPED outcome (JSONL status:skip, JUnit <skipped/>, MODULE SKIPPED: stdout).
#
# WHAT THIS LINT ENFORCES (so the false-green class cannot silently return):
#   A. run-shard.sh still implements the protocol end-to-end (exports the
#      sentinel, classifies it, emits the MODULE SKIPPED line + status:skip row).
#   B. EVERY skip-family function (name matches skip*, plus the shared
#      gvisor_require_substrate_guard) that has a clean `exit 0` writes the
#      sentinel first — i.e. no new green-by-skip site can be added uninstrumented.
#   C. A functional check: run_module (lifted from run-shard.sh) actually records
#      a sentinel-writing exit-0 module as SKIPPED, a plain exit-0 as PASSED, and
#      an exit-1 as FAILED.
#
# Hermetic: no kubectl, no beads daemon, no network. It only static-scans the
# scripts and drives the lifted run_module against fake modules with stubbed
# port-forward/curl helpers (same lift technique as test-shard-budget-guard.sh).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_SHARD="${SCRIPT_DIR}/run-shard.sh"

PASS=0
FAIL=0
ERRORS=()
pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

if [ ! -f "$RUN_SHARD" ]; then
  echo "FAIL: run-shard.sh not found at $RUN_SHARD" >&2
  exit 1
fi

echo "════════════════════════════════════════════════════"
echo " skip-visibility protocol lint (ka-0000000989)"
echo "════════════════════════════════════════════════════"

# ── Part A: run-shard.sh implements the protocol ────────────────────
echo
echo "A. run-shard.sh wires the skip-sentinel protocol"
if grep -q 'export E2E_SKIP_SENTINEL=' "$RUN_SHARD"; then
  pass "run_module exports E2E_SKIP_SENTINEL"
else
  fail "run_module does not export E2E_SKIP_SENTINEL — the sentinel is never handed to modules"
fi
if grep -q 'MODULE SKIPPED:' "$RUN_SHARD"; then
  pass "run_module emits a 'MODULE SKIPPED:' line (the per-module verdict in the shard log)"
else
  fail "run_module never prints 'MODULE SKIPPED:' — skips are not surfaced in the log"
fi
# The JSONL emit is echo-escaped in the source (\"status\":\"skip\"), so match
# the status token tolerant of the backslash-escaping.
if grep -Eq 'status\\?":\\?"skip\\?"' "$RUN_SHARD"; then
  pass "run_module records a status:skip JSONL row (JUnit <skipped/>, not a pass)"
else
  fail "run_module never records status:skip — a skip would still be counted as a pass"
fi

# ── Part B: every skip-family exit-0 site writes the sentinel ────────
# A skip-family function: name matches ^[a-z_]*skip[a-z_]* OR is the shared
# gvisor_require_substrate_guard. A "clean exit 0" is a line that is exactly
# `exit 0` inside such a function's body — the skip-as-pass shape. It must be
# immediately preceded by a line that writes E2E_SKIP_SENTINEL, OR (one-liner
# form) sit on the same line as an E2E_SKIP_SENTINEL write.
echo
echo "B. every skip-family exit-0 site writes the sentinel before exiting"
SCAN_FILES=("$SCRIPT_DIR"/test-*.sh "$SCRIPT_DIR"/lib-gvisor-node.sh)
UNINSTRUMENTED=$(
  for f in "${SCAN_FILES[@]}"; do
    [ -f "$f" ] || continue
    awk '
      # multi-line skip-family function opens (body continues on next line)
      /^([a-z_]*skip[a-z_]*|gvisor_require_substrate_guard)\(\)[[:space:]]*\{[[:space:]]*$/ { infn=1; prev=""; next }
      infn && /^\}[[:space:]]*$/ { infn=0; next }
      # one-liner skip-family function on a single line: check the whole line
      /^[a-z_]*skip[a-z_]*\(\)[[:space:]]*\{.*exit 0/ {
        if ($0 !~ /E2E_SKIP_SENTINEL/) print FILENAME " line " NR " (one-liner)"
        next
      }
      # clean exit-0 inside a multi-line skip body
      infn && /^[[:space:]]*exit 0[[:space:]]*$/ {
        if (prev !~ /E2E_SKIP_SENTINEL/) print FILENAME " line " NR
      }
      infn { prev=$0 }
    ' "$f"
  done
)
if [ -z "$UNINSTRUMENTED" ]; then
  pass "every skip-family 'exit 0' is preceded by an E2E_SKIP_SENTINEL write"
else
  fail "these skip-family 'exit 0' sites do NOT write the sentinel (they would be recorded as PASSED, re-introducing green-by-skip):"
  printf '%s\n' "$UNINSTRUMENTED" | sed "s|$SCRIPT_DIR/||; s/^/    /" >&2
fi

# Sanity: there IS at least one instrumented site, so a broken scan pattern
# (matching nothing) cannot vacuously pass Part B.
INSTRUMENTED_COUNT=$(grep -lR 'E2E_SKIP_SENTINEL' "${SCAN_FILES[@]}" 2>/dev/null | wc -l | tr -d ' ')
if [ "${INSTRUMENTED_COUNT:-0}" -ge 10 ]; then
  pass "sentinel writes are present across the suite ($INSTRUMENTED_COUNT files) — scan is not vacuous"
else
  fail "only $INSTRUMENTED_COUNT files write E2E_SKIP_SENTINEL — expected the full skip-family set; the scan or the instrumentation regressed"
fi

# ── Part C: functional — run_module classifies skip/pass/fail ───────
echo
echo "C. run_module classifies a sentinel-writing exit-0 as SKIPPED (functional)"
extract_func() {
  awk -v fn="$1" '$0 ~ "^" fn "\\(\\)[[:space:]]*\\{"{c=1} c{print} c&&/^\}/{exit}' "$RUN_SHARD"
}
JSON_STR_FUNC="$(extract_func json_str)"
BUDGET_WARN_FUNC="$(extract_func shard_budget_warn)"
RUN_MODULE_FUNC="$(extract_func run_module)"
if [ -z "$JSON_STR_FUNC" ] || [ -z "$RUN_MODULE_FUNC" ] || [ -z "$BUDGET_WARN_FUNC" ]; then
  fail "could not lift json_str/shard_budget_warn/run_module from run-shard.sh (function names or brace style changed?)"
else
  # shellcheck disable=SC2294
  eval "$JSON_STR_FUNC"
  # shellcheck disable=SC2294
  eval "$BUDGET_WARN_FUNC"
  # shellcheck disable=SC2294
  eval "$RUN_MODULE_FUNC"
  # Stubs for everything run_module calls that would touch a cluster/network.
  ensure_port_forward() { :; }
  ensure_bridge_port_forward() { :; }
  retry_fits_budget() { return 0; }
  shard_seconds_remaining() { echo 1000; }
  curl() { return 0; }       # pretend both tunnels are healthy
  kd() { return 0; }
  # Globals run_module reads. These are consumed ONLY inside the eval'd
  # run_module / shard_budget_warn bodies, which shellcheck cannot see — hence
  # the SC2034 disables (genuinely used at eval time, not dead).
  # shellcheck disable=SC2034
  BRIDGE_AVAILABLE=0
  # shellcheck disable=SC2034
  E2E_SHARD=0
  TESTS_PASSED=0; TESTS_FAILED=0; TESTS_SKIPPED=0
  # shellcheck disable=SC2034
  SHARD_BUDGET_WARNED=1
  # shellcheck disable=SC2034
  SHARD_BUDGET_SECONDS=1500
  # shellcheck disable=SC2034
  SHARD_BUDGET_WARN_PCT=80
  # shellcheck disable=SC2034
  SHARD_START_TS=$(date +%s)
  RESULTS_JSONL="$(mktemp)"; : > "$RESULTS_JSONL"
  trap 'rm -f "$RESULTS_JSONL"' EXIT

  PASSMOD="$(mktemp)"; printf '#!/usr/bin/env bash\necho ran-real-assertion\nexit 0\n' > "$PASSMOD"
  FAILMOD="$(mktemp)"; printf '#!/usr/bin/env bash\necho boom\nexit 1\n' > "$FAILMOD"
  SKIPMOD="$(mktemp)"; cat > "$SKIPMOD" <<'M'
#!/usr/bin/env bash
set -uo pipefail
skip_clean() { echo "SKIP: $1"; [ -n "${E2E_SKIP_SENTINEL:-}" ] && printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; exit 0; }
skip_clean "prerequisite binary absent"
echo "UNREACHABLE — should have exited in skip_clean"
M

  run_module passmod "$PASSMOD" >/dev/null 2>&1; pm_rc=$?
  run_module skipmod "$SKIPMOD" >/dev/null 2>&1; sm_rc=$?
  run_module failmod "$FAILMOD" >/dev/null 2>&1; fm_rc=$?
  rm -f "$PASSMOD" "$SKIPMOD" "$FAILMOD"

  # Counters
  if [ "$TESTS_PASSED" = 1 ]; then
    pass "a plain exit-0 module is counted PASSED (TESTS_PASSED=1)"
  else
    fail "TESTS_PASSED=$TESTS_PASSED, expected 1"
  fi
  if [ "$TESTS_SKIPPED" = 1 ]; then
    pass "a sentinel-writing exit-0 module is counted SKIPPED (TESTS_SKIPPED=1)"
  else
    fail "TESTS_SKIPPED=$TESTS_SKIPPED, expected 1 — the skip was mis-counted (green-by-skip regression!)"
  fi
  if [ "$TESTS_FAILED" = 1 ]; then
    pass "an exit-1 module is counted FAILED (TESTS_FAILED=1)"
  else
    fail "TESTS_FAILED=$TESTS_FAILED, expected 1"
  fi
  # Return codes: skip and pass both return 0 (must not red a direct dispatch),
  # fail returns non-zero.
  if [ "$pm_rc" = 0 ] && [ "$sm_rc" = 0 ]; then
    pass "skip and pass both return rc=0 (a skip never reds a direct dispatch)"
  else
    fail "expected pass_rc=0 skip_rc=0, got pass_rc=$pm_rc skip_rc=$sm_rc"
  fi
  if [ "$fm_rc" != 0 ]; then
    pass "fail returns rc!=0"
  else
    fail "fail returned rc=0"
  fi

  # JSONL: exactly one row per status, with the right status strings.
  SKIP_ROWS=$(grep -c '"status":"skip"' "$RESULTS_JSONL")
  PASS_ROWS=$(grep -c '"status":"pass"' "$RESULTS_JSONL")
  FAIL_ROWS=$(grep -c '"status":"fail"' "$RESULTS_JSONL")
  if [ "$SKIP_ROWS" = 1 ] && [ "$PASS_ROWS" = 1 ] && [ "$FAIL_ROWS" = 1 ]; then
    pass "results JSONL has exactly one pass, one skip, one fail row"
  else
    fail "results JSONL row counts wrong: pass=$PASS_ROWS skip=$SKIP_ROWS fail=$FAIL_ROWS"
  fi
  # The skip row carries the reason the module wrote.
  if grep '"status":"skip"' "$RESULTS_JSONL" | grep -q 'prerequisite binary absent'; then
    pass "the skip row records the module's skip reason"
  else
    fail "the skip row does not carry the reason string: $(grep '"status":"skip"' "$RESULTS_JSONL")"
  fi
fi

# ── Part E: top-level skip guards write the sentinel too (ka-0000005441) ─
# Part B only sees skips inside a skip-FAMILY function (name matches skip*, or
# gvisor_require_substrate_guard). But a module can green-by-skip with a bare
# TOP-LEVEL guard and no function at all:
#
#     if [ -z "${SLACK_BOT_TOKEN:-}" ]; then
#       echo "SKIP: SLACK_BOT_TOKEN not set — ..."
#       exit 0          # <- 0 assertions ran, recorded as MODULE PASSED
#     fi
#
# That is exactly how slack-cards / slack-dashboard / slack-threads skipped on
# every e2e shard while counting toward the green tally (ka-0000005441).
# This part flags any TOP-LEVEL echo/printf "SKIP…" → `exit 0` pair with no
# intervening E2E_SKIP_SENTINEL write.
#
# Disjoint from Part B by construction: this scan tracks function depth the same
# way Part B does (an opener whose brace ends the line; a lone `}` closes) and
# ONLY inspects depth-0 lines, so a skip guarded inside a skip-family function
# stays Part B's job. It must NOT match:
#   - a plain success `exit 0` with no preceding SKIP echo,
#   - a `SKIP` verdict VALUE returned from a decision-table helper
#     (`echo "SKIP"; return` — a return, not a process exit; and in a function),
#   - a guard that already writes the sentinel before exiting.
echo
echo "E. top-level SKIP guards write the sentinel before 'exit 0' (ka-0000005441)"

# Shared scanner — the SAME awk drives the real-file scan below AND the
# positive/negative control fixtures, so what the lint tests is exactly what it
# enforces (no hand-copied second implementation to drift).
toplevel_skip_scan() {
  awk '
    # function opener (brace at end of line) => descend; lone } => ascend
    /^[a-zA-Z_][a-zA-Z0-9_]*\(\)[[:space:]]*\{[[:space:]]*$/ { depth++; pend=0; next }
    /^\}[[:space:]]*$/                                        { if (depth>0) depth--; pend=0; next }
    depth>0                                                   { next }
    # top level only past here:
    /E2E_SKIP_SENTINEL/                          { pend=0; next }   # already instrumented
    /^[[:space:]]*(echo|printf).*SKIP/           { pend=1; sl=NR; next }
    /^[[:space:]]*exit[[:space:]]+0[[:space:]]*$/ { if (pend) print FILENAME " line " NR " (top-level SKIP echo at line " sl ")"; pend=0; next }
    /^[[:space:]]*(echo|printf)[[:space:]]/      { next }           # keep pending across extra echoes
    /^[[:space:]]*#/                             { next }           # …and comments
    /^[[:space:]]*$/                             { next }           # …and blanks
                                                 { pend=0 }         # any other statement clears it
  ' "$@"
}

# E1: the real suite must be clean. The scanner is heredoc-unaware, so exclude
# THIS lint's own file — its E2/E3 fixtures deliberately embed the bad shape as
# heredoc test data, and the file is not run_module-wired (it runs as its own
# `test-skip-visibility` CI task), so it can never inflate a shard tally.
SELF_BASENAME="$(basename "${BASH_SOURCE[0]}")"
TOPLEVEL_UNINSTRUMENTED=$(
  for f in "${SCAN_FILES[@]}"; do
    [ -f "$f" ] || continue
    [ "$(basename "$f")" = "$SELF_BASENAME" ] && continue
    toplevel_skip_scan "$f"
  done
)
if [ -z "$TOPLEVEL_UNINSTRUMENTED" ]; then
  pass "no top-level 'SKIP…/exit 0' guard skips silently (all write the sentinel)"
else
  fail "these TOP-LEVEL skip guards exit 0 without writing the sentinel (recorded as PASSED — green-by-skip):"
  printf '%s\n' "$TOPLEVEL_UNINSTRUMENTED" | sed "s|$SCRIPT_DIR/||; s/^/    /" >&2
fi

# E2: anti-vacuity positive control — a broken/empty scan pattern must not pass
# E1 vacuously. Feed the scanner a synthetic module with the exact bad shape and
# assert it IS flagged.
E_TMP="$(mktemp -d)"
BADFIX="$E_TMP/test-bad.sh"
cat > "$BADFIX" <<'M'
#!/usr/bin/env bash
set -euo pipefail
if [ -z "${SOME_TOKEN:-}" ]; then
  echo "SKIP: SOME_TOKEN not set — needs creds"
  echo "  Set SOME_TOKEN to run."
  exit 0
fi
echo "real assertion"
M
if [ -n "$(toplevel_skip_scan "$BADFIX")" ]; then
  pass "positive control: a bare top-level SKIP/exit-0 guard IS flagged (scan is not vacuous)"
else
  fail "positive control: the scanner did NOT flag a known green-by-skip guard — the scan pattern is broken"
fi

# E3: anti-over-broad negative controls — none of these three legitimate shapes
# may be flagged.
GOODFIX="$E_TMP/test-good.sh"
cat > "$GOODFIX" <<'M'
#!/usr/bin/env bash
set -euo pipefail
if [ -z "${SOME_TOKEN:-}" ]; then
  echo "SKIP: SOME_TOKEN not set — needs creds"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "SOME_TOKEN not set" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi
  exit 0
fi
echo "real assertion"
M
VERDICTFIX="$E_TMP/test-verdict.sh"
cat > "$VERDICTFIX" <<'M'
#!/usr/bin/env bash
# SKIP returned as a decision-table VALUE (return, not exit) — the adversarial
# near-miss the fix must not break (test-stale-reaper-grace-reconcile et al.).
create_missing_verdict() {
  local state="$1"
  [ "$state" = "failed" ] && { echo "SKIP"; return; }
  echo "RECREATE"
}
create_missing_verdict failed
M
PLAINEXITFIX="$E_TMP/test-plainexit.sh"
cat > "$PLAINEXITFIX" <<'M'
#!/usr/bin/env bash
echo "all assertions passed"
exit 0
M
OVERBROAD=""
for f in "$GOODFIX" "$VERDICTFIX" "$PLAINEXITFIX"; do
  hit="$(toplevel_skip_scan "$f")"
  [ -n "$hit" ] && OVERBROAD="${OVERBROAD}${OVERBROAD:+$'\n'}$hit"
done
if [ -z "$OVERBROAD" ]; then
  pass "negative controls: instrumented guard, verdict-return helper, and plain exit-0 are NOT flagged (scan not over-broad)"
else
  fail "over-broad match — the scanner flagged a legitimate shape:"
  printf '%s\n' "$OVERBROAD" | sed 's/^/    /' >&2
fi
rm -rf "$E_TMP"

# ── Summary ─────────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  for err in "${ERRORS[@]}"; do echo "  - $err"; done
  exit 1
fi
echo "OK: skip-visibility protocol holds — self-skips are recorded distinctly from passes"
