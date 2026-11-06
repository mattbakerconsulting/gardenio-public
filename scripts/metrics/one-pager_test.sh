#!/usr/bin/env bash
# one-pager_test.sh — self-test for scripts/metrics/one-pager.sh (gd-UFdSDSsdvt).
#
# Hermetic: fixture JSON in, numbers out. The compute half runs on a --from-dir
# fixture whose every number is worked by hand below; the fetch half runs
# against stub kd and gh on PATH to pin paging, the PR dedupe at day
# boundaries and the refusal at GitHub search's 1000-result cap. No network,
# no daemon.
#
# Usage:  bash scripts/metrics/one-pager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/one-pager.sh"
command -v jq >/dev/null 2>&1 || { echo "FATAL: jq is required" >&2; exit 1; }

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# expect_line <name> <output> <exact line>
expect_line() {
  if grep -qxF -- "$3" <<<"$2"; then pass "$1"; else fail "$1" "no line '$3' in output"; fi
}

# ── compute: fixture in, numbers out ─────────────────────────────────────────
F="$TMP/fixture"
mkdir -p "$F"

# 14 PRs over a 7-day window: 2 per day.
jq -n '[range(1; 15) | {number: ., mergedAt: "2026-10-03T00:00:00Z"}]' >"$F/prs.json"

cat >"$F/roles.json" <<'EOF'
[{"role":"crew","model":"claude-opus-5-5-oauth"},
 {"role":"thread","model":"claude-fable-5-1-oauth"},
 {"role":"short-title-groomer","model":"anthropic/claude-haiku-4-5"},
 {"role":"herder","model":""}]
EOF

# s1 crew   1M input                    opus  4.00
# s2 crew   100k output + 1M cache read opus  2.00 + 0.20 = 2.20
# s3 thread 20k output + 100k cache write fable 1.00 + 100k x 10 x 2 / 1M = 3.00
# s4 herder 250k input, role has no model -> opus 1.00
# recorded 10.20. Crew mean 3.10 fills s5 (retired, no counts): extrapolated
# 13.30. s6 is live (not ended, not costed); s7's role has no recorded session.
cat >"$F/sessions.json" <<'EOF'
[{"id":"s1","role":"crew","status":"completed","input_tokens":1000000,"output_tokens":0,"cache_write_tokens":0,"cache_read_tokens":0},
 {"id":"s2","role":"crew","status":"completed","input_tokens":0,"output_tokens":100000,"cache_write_tokens":0,"cache_read_tokens":1000000},
 {"id":"s3","role":"thread","status":"completed","input_tokens":0,"output_tokens":20000,"cache_write_tokens":100000,"cache_read_tokens":0},
 {"id":"s4","role":"herder","status":"crashed","input_tokens":250000,"output_tokens":0,"cache_write_tokens":0,"cache_read_tokens":0},
 {"id":"s5","role":"crew","status":"retired","input_tokens":null,"output_tokens":null,"cache_write_tokens":null,"cache_read_tokens":null},
 {"id":"s6","role":"crew","status":"active","input_tokens":null,"output_tokens":null,"cache_write_tokens":null,"cache_read_tokens":null},
 {"id":"s7","role":"reviewer","status":"retired","input_tokens":null,"output_tokens":null,"cache_write_tokens":null,"cache_read_tokens":null}]
EOF

# Latencies 30 min, 2 h, 1 h -> median 1 h; d3 unanswered; d4 is an e2e
# fixture and drops out. 4 real decisions in 1 week.
cat >"$F/decisions.json" <<'EOF'
[{"id":"d1","created_at":"2026-10-01T00:00:00.123456Z","created_by":"laptop","responded_at":"2026-10-01T00:30:00Z"},
 {"id":"d2","created_at":"2026-10-02T00:00:00Z","created_by":"crew-x","responded_at":"2026-10-02T02:00:00Z"},
 {"id":"d3","created_at":"2026-10-02T00:00:00Z","created_by":"crew-y","responded_at":null},
 {"id":"d4","created_at":"2026-10-02T00:00:00Z","created_by":"e2e-v2-agent","responded_at":"2026-10-02T00:00:01Z"},
 {"id":"d5","created_at":"2026-10-03T00:00:00Z","created_by":"thread","responded_at":"2026-10-03T01:00:00Z"}]
EOF

# 6 scored runs, 3 with progress_score 0; one run left no score.
cat >"$F/captains.json" <<'EOF'
[{"id":"a1","role":"plan-gardener","progress_score":0},
 {"id":"a2","role":"plan-gardener","progress_score":1},
 {"id":"a3","role":"plan-gardener","progress_score":2},
 {"id":"a4","role":"plan-gardener","progress_score":0},
 {"id":"a5","role":"release-captain","progress_score":0},
 {"id":"a6","role":"release-captain","progress_score":null},
 {"id":"a7","role":"plan-captain","progress_score":2}]
EOF

WINDOW=(--since 2026-10-01T00:00:00Z --until 2026-10-08T00:00:00Z)
out="$(bash "$SCRIPT_UNDER_TEST" --from-dir "$F" --repo example-org/example-repo "${WINDOW[@]}" 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then pass "fixture run exits 0"; else fail "fixture run exits 0" "rc=$rc: $out"; fi

expect_line "window line" "$out" "- Window: 2026-10-01T00:00:00Z .. 2026-10-08T00:00:00Z (7 days)"
expect_line "merged PRs" "$out" \
  "| Merged PRs | 14 | gh pr list --state merged on example-org/example-repo, mergedAt in window |"
expect_line "PRs per day" "$out" "| PRs merged per day | 2 | merged PRs / 7 days |"
expect_line "recorded cost" "$out" \
  "| Token cost at list price, recorded | \$10.20 | token counts on the 4 of 6 ended session beads (66.7%; project:gardenio, created in window) that carry them, x Anthropic list price; not LiteLLM spend |"
expect_line "extrapolated cost" "$out" \
  "| Token cost at list price, extrapolated | \$13.30 | recorded, plus each role mean recorded cost per session x its 2 ended sessions with no token counts |"
expect_line "cost per PR" "$out" \
  "| Cost per merged PR | \$0.73 recorded, \$0.95 extrapolated | token cost / merged PRs |"
expect_line "decisions per week" "$out" \
  "| Decisions per week | 4 | 4 decision beads created in window, fleet-wide, e2e fixtures excluded / 1 weeks |"
expect_line "median latency" "$out" \
  "| Median decision latency | 1 h (n=3) | responded_at - created_at on responded decision beads |"
expect_line "captain no-op rate" "$out" \
  "| Captain no-op rate | 50% (3/6) | captain runs (plan-gardener, plan-captain, release-captain; project:gardenio) created in window whose wrap-up progress_score is 0 (plan-captain 0/1, plan-gardener 2/4, release-captain 1/1) |"
expect_line "fable row" "$out" "| Fable 5.1 | 1 | 0 | 20000 | 100000 | 0 | \$3.00 |"
expect_line "opus row (role without model priced as opus)" "$out" \
  "| Opus 5.5 | 3 | 1250000 | 100000 | 0 | 1000000 | \$7.20 |"
expect_line "coverage note" "$out" \
  "- 2 ended session beads carry no token counts (mostly closed as retired by the session reaper) and 1 are still live; 1 of the ended ones belong to a role with no recorded session to extrapolate from."
expect_line "unscored captain note" "$out" \
  "- 1 captain runs have no progress_score and are left out of the no-op rate."
if grep -q '^- Commit: ' <<<"$out"; then pass "prints the commit"; else fail "prints the commit" "no Commit line"; fi

# 5-minute TTL: s3's cache write is 100k x 10 x 1.25 / 1M = 1.25, so 9.45.
out5="$(bash "$SCRIPT_UNDER_TEST" --from-dir "$F" --cache-write-ttl 5m "${WINDOW[@]}" 2>&1)"
if grep -qF "| Token cost at list price, recorded | \$9.45 |" <<<"$out5"; then
  pass "5m TTL prices cache writes at 1.25x"
else
  fail "5m TTL prices cache writes at 1.25x" "$(grep 'recorded' <<<"$out5")"
fi

# ── argument and input errors ────────────────────────────────────────────────
expect_rc2() {
  local name="$1"; shift
  bash "$SCRIPT_UNDER_TEST" "$@" >/dev/null 2>&1
  local rc=$?
  if [ "$rc" -eq 2 ]; then pass "$name"; else fail "$name" "expected exit 2, got $rc"; fi
}
mkdir -p "$TMP/empty"
expect_rc2 "missing fixture file" --from-dir "$TMP/empty" "${WINDOW[@]}"
expect_rc2 "--days must be a positive integer" --from-dir "$F" --days 0
expect_rc2 "--since after --until" --from-dir "$F" --since 2026-10-08T00:00:00Z --until 2026-10-01T00:00:00Z
expect_rc2 "bad --cache-write-ttl" --from-dir "$F" --cache-write-ttl 2h "${WINDOW[@]}"
expect_rc2 "unknown argument" --frobnicate

# ── fetch: stub kd and gh ────────────────────────────────────────────────────
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/kd" <<'EOF'
#!/usr/bin/env bash
# Two one-bead pages for sessions (paging), empty for everything else.
type="" offset=0
while [ $# -gt 0 ]; do
  case "$1" in -t) type="$2"; shift ;; --offset) offset="$2"; shift ;; esac
  shift
done
if [ "$type" = session ] && [ "$offset" = 0 ]; then
  echo '{"beads":[{"id":"p1","fields":{"role":"crew","status":"completed","input_tokens":1000000,"output_tokens":0}}],"count":1,"total":2,"has_more":true}'
elif [ "$type" = session ] && [ "$offset" = 1 ]; then
  echo '{"beads":[{"id":"p2","fields":{"role":"crew","status":"completed","input_tokens":1000000,"output_tokens":0}}],"count":1,"total":2,"has_more":false}'
else
  echo '{"beads":[],"count":0,"total":0,"has_more":false}'
fi
EOF
cat >"$BIN/gh" <<'EOF'
#!/usr/bin/env bash
# One PR at each end of the searched day, numbered by its merge time, so the
# shared boundary between two days shows up twice.
search=""
while [ $# -gt 0 ]; do [ "$1" = --search ] && search="$2"; shift; done
range="${search#merged:}"
if [ -n "${STUB_GH_CAP:-}" ]; then
  jq -n --arg t "${range%%..*}" '[range(0; 1000) | {number: ., mergedAt: $t}]'
  exit 0
fi
jq -n --arg a "${range%%..*}" --arg b "${range##*..}" \
  '[$a, $b] | map({number: fromdateiso8601, mergedAt: .})'
EOF
chmod +x "$BIN/kd" "$BIN/gh"

FW=(--since 2026-10-01T00:00:00Z --until 2026-10-03T00:00:00Z --repo example-org/example-repo)
fout="$(PATH="$BIN:$PATH" bash "$SCRIPT_UNDER_TEST" "${FW[@]}" 2>&1)"
frc=$?
if [ "$frc" -eq 0 ]; then pass "stubbed fetch exits 0"; else fail "stubbed fetch exits 0" "rc=$frc: $fout"; fi
# Days return {10-01, 10-02} and {10-02, 10-03}: deduped to 3, then 10-03 is
# the window end and drops out.
if grep -qF '| Merged PRs | 2 |' <<<"$fout"; then
  pass "PRs deduped across day boundaries, window end excluded"
else
  fail "PRs deduped across day boundaries, window end excluded" "$(grep 'Merged PRs' <<<"$fout")"
fi
if grep -qF "| Token cost at list price, recorded | \$8.00 | token counts on the 2 of 2 " <<<"$fout"; then
  pass "kd pages followed past has_more"
else
  fail "kd pages followed past has_more" "$(grep 'recorded' <<<"$fout")"
fi

STUB_GH_CAP=1 PATH="$BIN:$PATH" bash "$SCRIPT_UNDER_TEST" "${FW[@]}" >/dev/null 2>"$TMP/cap.err"
crc=$?
if [ "$crc" -eq 2 ] && grep -q "1000-result cap" "$TMP/cap.err"; then
  pass "refuses a day at the gh search cap"
else
  fail "refuses a day at the gh search cap" "rc=$crc: $(cat "$TMP/cap.err")"
fi

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
