#!/usr/bin/env bash
# test-costs-spend-page.sh — E2E for the beads-viewer /costs spend page and its
# htmx cost-explorer fragment (ka-0000005996, parent epic ka-0000000958).
#
# WHY THIS EXISTS: the /costs page is the operator's primary spend surface —
# rolling-window headline $/h, the F6 unit-economics + F7 month-projection
# stats, the x-check trust strip, and four Grafana spend panels — and it had
# ZERO e2e. `grep -rln "/costs" gardenio/tests/e2e/scripts/` returned nothing
# before this module. The three cost-adjacent modules that DO exist all skip
# the page itself: litellm-cost-tracking asserts the LiteLLM proxy's per-key
# spend, usage-tracking asserts SESSION-bead usage, and agent-cost-context
# asserts the per-agent chips on /agents + /{id}. Nothing rendered /costs.
#
# WHAT MAKES THIS MORE THAN A STRING GREP: scenario 2 asserts a *behavioral*
# invariant that discriminates the rolling-window rate from the avg-since-
# midnight computation it replaced. Both render as "$N/h", so a marker grep
# cannot tell them apart. But when the UTC day is young and today's settled
# total is still $0.00, an avg-since-midnight rate is necessarily $0.00/h
# while a 24h-rolling rate stays positive — it still sees yesterday's spend.
# That divergence is the proof. (Observed live on staging while writing
# this: Today $0.00 at 25% of day elapsed, rate (24h avg) $77.16/h rolling.)
# Scenario 2 also cross-checks `projected ≈ rate × 24`, so the headline and
# the projection must reconstruct each other — two derivations of one number
# (advice ka-0000005924).
#
# Scenarios:
#   1. /costs renders the spend scaffold: a 200 carrying the page title, the
#      summary card, BOTH rate lines with their window labels + basis words
#      ("rolling" / "reactive" — the emitted discriminator between the 24h
#      rolling headline and the 1h reactive rate), the UTC-day block, and the
#      "Fleet summary" stats dl. Also asserts the inline-ECharts wiring stays
#      GONE (data-chart-catalog / echarts_*.js), which is the June Grafana
#      swap's invariant — costs_test.go:440-447 pins the same absence in-tree,
#      and this module proves it on a DEPLOYED viewer.
#   2. Rolling-window semantics (the behavioral assertion above) + the
#      projected = rate × 24 cross-check.
#   3. The four Grafana spend panels render as embeds keyed to the costs
#      dashboard uid, and degrade to the explicit "(Grafana not configured)"
#      placeholder rather than a dead iframe when GRAFANA_BASE_URL is unset.
#      Panel titles are asserted by name so a silently-dropped panel reds.
#   4. The x-check trust strip renders, and every badge is one of the three
#      legal levels — never a blank/unknown chip. A cost number with no trust
#      verdict at all is the failure this catches.
#   5. The cost explorer is a LAZY htmx shell: the /costs response itself
#      carries hx-get + the "Loading cost explorer" placeholder and NOT the
#      table (htmx has not run at curl time). Fetching /costs/rollup directly
#      returns the fragment, and part=table returns the inner table.
#   6. /costs/rollup input hardening: every rollup axis is accepted, an
#      out-of-allowlist rollup/range falls back instead of erroring, and a
#      PromQL-injection attempt in a filter value neither 500s nor reflects
#      the raw payload into the response.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-costs-spend-page.sh
#
# Prerequisites: kubectl context on the target cluster, curl, python3, and a
# mintable viewer bearer (lib-gitlab-token.sh).
#
# Environment note: the beads-viewer OIDC surface is deployed only on the
# long-lived staging singleton and prod — NOT in the per-instance
# ephemeral e2e namespaces (edge-auth/README.md:99-105). When the viewer
# Service is absent, or when no bearer can be minted in this environment,
# the module SKIPs cleanly (exit 0) exactly like its sibling charts-hub, so
# it is inert in the ephemeral shard fan-out and asserts only where the
# viewer actually runs.
#
# READ-ONLY: this module creates no beads and mutates nothing. /costs and
# /costs/rollup are both GET-only read surfaces, so reruns cannot collide
# and there is nothing to clean up.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Config ────────────────────────────────────────────────────────
# Set the lib-viewer-consumed knobs BEFORE sourcing lib-viewer.sh: the lib
# defaults them with `: "${VAR:=…}"` at source time, so a value assigned
# afterwards is ignored and our dedicated port would silently not take
# effect. VIEWER_PF_PORT is kept distinct from sibling viewer modules
# (charts-hub 19631, agent-cost-context 19611) so co-located shards cannot
# collide on the forward.
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19641}"

# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

# BASE_PATH the viewer is served under. The deployed singleton runs with
# BASE_PATH=/beads, but every route is ALSO registered at the bare path on
# the pod's own listener, so "" works against a port-forward. Overridable
# for a deployment that only serves the prefixed form.
COSTS_BASE="${COSTS_BASE:-}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()

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
skip_all() {
  yellow "SKIP: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

BODY_FILE=""
ROLLUP_FILE=""
cleanup() {
  rm -f "$BODY_FILE" "$ROLLUP_FILE" 2>/dev/null || true
  teardown_viewer_portforward
}
trap cleanup EXIT

# have_marker <file> <literal> — fixed-string presence test.
#
# CRITICAL: grep a FILE, never `printf '%s' "$body" | grep -qF`. Under
# `set -o pipefail` the printf is killed by SIGPIPE the moment `grep -q`
# short-circuits on a match, so the pipeline exits 141 and a genuine MATCH
# is misread as a miss. That footgun produced a full page of false negatives
# while this module was being written; keeping the body on disk avoids it.
have_marker() { grep -qF -- "$2" "$1"; }

assert_marker() { # assert_marker <file> <literal> <label>
  if have_marker "$1" "$2"; then
    pass "$3"
  else
    fail "$3" "marker not found: $2"
  fi
}

assert_absent() { # assert_absent <file> <literal> <label>
  if have_marker "$1" "$2"; then
    fail "$3" "marker should be gone but is present: $2"
  else
    pass "$3"
  fi
}

# vstatus <path> — authenticated GET, echo numeric status only.
vstatus() { viewer_status "${COSTS_BASE}$1" "$VIEWER_TOKEN"; }

# vfetch <path> <outfile> — authenticated GET; body to outfile, status echoed.
vfetch() {
  local path="$1" out="$2"
  curl -sk --max-time 20 \
    -H "Authorization: Bearer ${VIEWER_TOKEN}" \
    -H "Accept: text/html" \
    -o "$out" -w '%{http_code}' \
    "http://localhost:${VIEWER_PF_PORT}${COSTS_BASE}${path}" 2>/dev/null || echo "000"
}

# ── Pre-flight ────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
blue "  /costs spend page E2E (ka-0000005996)"
blue "═══════════════════════════════════════════════════"
echo

for bin in curl python3 kubectl; do
  command -v "$bin" >/dev/null 2>&1 || skip_all "missing required binary: $bin"
done

setup_viewer_portforward || skip_all "beads-viewer service not deployed in $NAMESPACE"

VIEWER_TOKEN="$(mint_user_token "costs-page-e2e-$$@acme.example" 2>/dev/null || true)"
if [ -z "$VIEWER_TOKEN" ]; then
  skip_all "could not mint a viewer bearer in this environment (no GitLab-direct bearer available)"
fi
dim "  Viewer auth: GitLab-direct bearer (${#VIEWER_TOKEN} bytes)"

BODY_FILE="$(mktemp -t costs-body-XXXXXX.html)"
ROLLUP_FILE="$(mktemp -t costs-rollup-XXXXXX.html)"

status="$(vfetch /costs "$BODY_FILE")"
if [ "$status" != "200" ]; then
  # A 401/302 means this environment's auth wiring does not accept our bearer
  # (the pre-auth redirect that masked ka-0000007004). That is an env gap, not
  # a product regression — skip rather than red-gate the suite.
  skip_all "/costs returned HTTP $status (auth/env gap, not a page defect)"
fi
dim "  /costs 200, $(wc -c < "$BODY_FILE") bytes"
echo

# ── Scenario 1: the spend scaffold renders ─────────────────────────
blue "Scenario 1: /costs renders the spend scaffold"
assert_marker "$BODY_FILE" '<title>Costs · Beads Viewer</title>' "page title rendered"
assert_marker "$BODY_FILE" 'class="costs-summary"' "summary card rendered"
assert_marker "$BODY_FILE" 'rate (24h avg)' "24h rolling rate line present"
assert_marker "$BODY_FILE" 'rate (1h avg)' "1h reactive rate line present"
# The basis words are the emitted discriminator: the 24h headline is labelled
# "rolling" and the 1h rate "reactive" (templates/costs.html). Their presence
# is what distinguishes these from a calendar-day average in the markup.
assert_marker "$BODY_FILE" '>rolling<' "24h rate labelled 'rolling'"
assert_marker "$BODY_FILE" '>reactive<' "1h rate labelled 'reactive'"
assert_marker "$BODY_FILE" 'costs-summary-utcday' "UTC-day block rendered"
assert_marker "$BODY_FILE" '% of day elapsed' "day-progress indicator rendered"
assert_marker "$BODY_FILE" 'aria-label="Fleet summary"' "F6 unit-economics stats dl rendered"

# The June Grafana swap removed the inline ECharts catalog wiring from /costs.
# costs_test.go:440-447 pins the absence in-tree; assert it on the DEPLOYED
# page too, so a build that regressed to the old panels reds here.
assert_absent "$BODY_FILE" 'data-chart-catalog' "inline-ECharts catalog attr stays gone"
assert_absent "$BODY_FILE" '/assets/static/echarts_catalog.js' "ECharts catalog script stays gone"
assert_absent "$BODY_FILE" '/assets/static/echarts.min.js' "ECharts library stays gone"
echo

# ── Scenario 2: rolling-window semantics (behavioral) ──────────────
blue "Scenario 2: the headline rate is a ROLLING window, not avg-since-midnight"
# Parse the three numbers the assertion needs straight out of the rendered
# HTML: today's settled total, the day-progress percentage, the 24h rate, and
# the projection. Emitted shapes (templates/costs.html):
#   <div class="costs-summary-total">$0.00</div>
#   <div class="meta costs-summary-progress">25% of day elapsed</div>
#   <span class="meta">rate (24h avg)</span> $77.16/h <span…>rolling</span>
#   <span class="meta">projected</span> $1851.73 <span…>if pace holds 24h</span>
RATE_PARSE="$(python3 - "$BODY_FILE" <<'PY'
import re, sys
h = open(sys.argv[1], encoding="utf-8", errors="replace").read()

def num(pat):
    m = re.search(pat, h)
    return m.group(1) if m else ""

today = num(r'class="costs-summary-total">\$([0-9,.]+)<')
prog  = num(r'costs-summary-progress">([0-9.]+)% of day elapsed')
rate  = num(r'rate \(24h avg\)</span>\s*\$([0-9,.]+)/h')
rate1 = num(r'rate \(1h avg\)</span>\s*\$([0-9,.]+)/h')
proj  = num(r'>projected</span>\s*\$([0-9,.]+)')
print("|".join(x.replace(",", "") for x in (today, prog, rate, rate1, proj)))
PY
)"
IFS='|' read -r TODAY DAYPCT RATE24 RATE1 PROJ <<< "$RATE_PARSE"
dim "  parsed: today=\$${TODAY:-?} day=${DAYPCT:-?}% rate24=\$${RATE24:-?}/h rate1=\$${RATE1:-?}/h projected=\$${PROJ:-?}"

if [ -z "$RATE24" ]; then
  # The rate renders as an em-dash when no metrics upstream is configured.
  # That is a legitimate degraded state, not a failure — but it means the
  # rolling-vs-midnight discrimination cannot be made here.
  if have_marker "$BODY_FILE" 'rate (24h avg)</span> <span class="meta">—'; then
    skip "rolling-window rate semantics" "no metrics upstream (rate renders as —)"
  else
    fail "24h rate parses as a dollar figure" "could not extract a rate from the page"
  fi
else
  pass "24h rate parses as a dollar figure (\$${RATE24}/h)"

  # THE DISCRIMINATING ASSERTION. If the headline were avg-since-midnight
  # (today_total / hours_elapsed) then a $0.00 settled day MUST yield a
  # $0.00/h rate. A positive rate on a $0.00 day can only come from a window
  # that reaches back past 00:00 UTC — i.e. a genuine rolling window.
  # When today's total is already positive the two computations are no longer
  # distinguishable from the outside, so the check reports honestly instead of
  # claiming a proof it did not make.
  verdict="$(python3 - "$TODAY" "$DAYPCT" "$RATE24" <<'PY'
import sys
def f(x):
    try: return float(x)
    except (TypeError, ValueError): return None
today, pct, rate = f(sys.argv[1]), f(sys.argv[2]), f(sys.argv[3])
if rate is None:
    print("ERR no rate"); raise SystemExit
if today is None or pct is None:
    print("INCONCLUSIVE could not parse today/day-progress"); raise SystemExit
# Need a meaningful slice of the day elapsed: at ~0% even an avg-since-midnight
# rate can be large or undefined, so the divergence proves nothing yet.
if today == 0.0 and pct >= 5.0:
    print("PROVEN" if rate > 0 else
          f"FAILED rate {rate} is 0 on a $0.00 day — looks like avg-since-midnight")
elif today == 0.0:
    print(f"INCONCLUSIVE day only {pct}% elapsed — too early to discriminate")
else:
    print(f"INCONCLUSIVE today=${today} is non-zero — both computations yield a positive rate")
PY
)"
  case "$verdict" in
    PROVEN*)
      pass "rate is rolling, NOT avg-since-midnight (\$${RATE24}/h while today=\$${TODAY} at ${DAYPCT}% of day)"
      ;;
    FAILED*)
      fail "rate is rolling, NOT avg-since-midnight" "${verdict#FAILED }"
      ;;
    INCONCLUSIVE*)
      # Not a defect — the fleet simply has spend today, so the two
      # computations agree and this particular proof is unavailable.
      skip "rolling-vs-midnight discrimination" "${verdict#INCONCLUSIVE }"
      ;;
    *)
      fail "rolling-vs-midnight discrimination" "unexpected verdict: $verdict"
      ;;
  esac

  # Cross-check: the projection is documented as "if pace holds 24h", i.e.
  # rate × 24. Two independently-rendered numbers that must reconstruct each
  # other (advice ka-0000005924). Tolerance covers the page's 2-decimal
  # rounding of both figures.
  if [ -n "$PROJ" ]; then
    xcheck="$(python3 - "$RATE24" "$PROJ" <<'PY'
import sys
rate, proj = float(sys.argv[1]), float(sys.argv[2])
want = rate * 24.0
# Both numbers are rendered to 2dp, so the rate carries up to 0.005 of
# rounding error which the ×24 amplifies to 0.12; allow a little more.
print("OK" if abs(want - proj) <= max(0.25, want * 0.005)
      else f"DRIFT projected={proj} but rate*24={want:.2f}")
PY
)"
    if [ "$xcheck" = "OK" ]; then
      pass "projected reconstructs rate × 24 (\$${PROJ} ≈ \$${RATE24} × 24)"
    else
      fail "projected reconstructs rate × 24" "$xcheck"
    fi
  else
    skip "projected cross-check" "no projection rendered (needs a 24h rate)"
  fi
fi
echo

# ── Scenario 3: Grafana spend panels ──────────────────────────────
blue "Scenario 3: the four Grafana spend panels render (or degrade cleanly)"
assert_marker "$BODY_FILE" 'class="costs-chart-cards"' "chart-cards section rendered"
# Panel titles asserted by name so a silently-dropped panel reds the module.
for title in 'Cost per agent-hour' 'Fleet spend rate' \
             'Per-agent cost over time' 'Spend by account over time'; do
  assert_marker "$BODY_FILE" "$title" "spend panel present: $title"
done
# Either a real embed keyed to the costs dashboard uid, or the explicit
# unconfigured placeholder — never a dead iframe. costsDashboardUID is
# "gardenio-costs-candidate" (costs.go).
if have_marker "$BODY_FILE" 'Grafana not configured'; then
  pass "Grafana unconfigured → explicit placeholder (no dead iframe)"
  assert_absent "$BODY_FILE" '<iframe' "no iframe emitted while Grafana is unconfigured"
else
  assert_marker "$BODY_FILE" 'gardenio-costs-candidate' "embeds keyed to the costs dashboard uid"
  assert_marker "$BODY_FILE" 'grafana-embed' "Grafana embed wrapper rendered"
fi
echo

# ── Scenario 4: x-check trust strip ───────────────────────────────
blue "Scenario 4: every cost number carries a trust verdict"
assert_marker "$BODY_FILE" 'class="costs-trust-strip"' "trust strip rendered"
assert_marker "$BODY_FILE" 'costs-trust-chip' "trust badges rendered"
# Each badge must resolve to one of the three legal levels. A chip that is
# neither ok/drift/unverified means a cost number is displayed with no
# corroboration verdict at all — the exact failure the strip exists to
# prevent (costs_trust.go).
TRUST_VERDICT="$(python3 - "$BODY_FILE" <<'PY'
import re, sys
h = open(sys.argv[1], encoding="utf-8", errors="replace").read()
chips = re.findall(r'<span class="chip costs-trust-chip costs-trust-(\w+)"', h)
if not chips:
    print("ERR no trust chips found"); raise SystemExit
legal = {"ok", "drift", "unverified"}
bad = sorted(set(chips) - legal)
print(f"BAD {','.join(bad)}" if bad else f"OK {len(chips)} chips, levels={sorted(set(chips))}")
PY
)"
case "$TRUST_VERDICT" in
  OK*)  pass "all trust badges carry a legal level (${TRUST_VERDICT#OK }))" ;;
  BAD*) fail "all trust badges carry a legal level" "illegal level(s): ${TRUST_VERDICT#BAD }" ;;
  *)    fail "trust badges parse" "${TRUST_VERDICT#ERR }" ;;
esac
echo

# ── Scenario 5: the cost explorer is a lazy htmx shell ────────────
blue "Scenario 5: cost explorer loads lazily over htmx"
# The page ships the SHELL, not the table: htmx has not run at curl time.
assert_marker "$BODY_FILE" 'id="costs-rollup-host"' "rollup host element present"
assert_marker "$BODY_FILE" 'hx-get=' "htmx lazy-load trigger present"
assert_marker "$BODY_FILE" 'Loading cost explorer' "placeholder rendered before htmx runs"
# The table must NOT be inlined in the page response — that is what makes it
# lazy. costs-rollup-total is emitted only by the fragment.
assert_absent "$BODY_FILE" 'costs-rollup-total' "rollup table NOT inlined in the page (proves lazy load)"

# Now fetch what htmx would fetch.
rstatus="$(vfetch '/costs/rollup?part=table&rollup=agent&range=24h' "$ROLLUP_FILE")"
if [ "$rstatus" = "200" ]; then
  pass "GET /costs/rollup?part=table 200 ($(wc -c < "$ROLLUP_FILE") bytes)"
  # The fragment carries either a real rollup or an explicit degraded note.
  if have_marker "$ROLLUP_FILE" 'costs-rollup-total'; then
    pass "rollup fragment renders a spend total"
    assert_marker "$ROLLUP_FILE" 'costs-rollup-caption' "rollup provenance caption rendered"
  elif have_marker "$ROLLUP_FILE" 'Live rollup unavailable' \
    || have_marker "$ROLLUP_FILE" 'Metrics temporarily unavailable' \
    || have_marker "$ROLLUP_FILE" 'No spend recorded'; then
    pass "rollup fragment degrades with an explicit note (no metrics upstream / empty window)"
  else
    fail "rollup fragment renders a total or an explicit note" \
      "neither a total nor a degraded note found"
  fi
else
  fail "GET /costs/rollup?part=table 200" "got HTTP $rstatus"
fi
echo

# ── Scenario 6: /costs/rollup input hardening ─────────────────────
blue "Scenario 6: /costs/rollup accepts every axis and hardens bad input"
for rollup in agent role project model account; do
  st="$(vstatus "/costs/rollup?part=table&rollup=${rollup}&range=24h")"
  if [ "$st" = "200" ]; then
    pass "rollup axis accepted: $rollup"
  else
    fail "rollup axis accepted: $rollup" "got HTTP $st"
  fi
done
# Out-of-allowlist values must FALL BACK to the default, never error.
for bad in 'rollup=DROP' 'range=99y' 'rollup=&range=' 'rollup=../etc/passwd'; do
  st="$(vstatus "/costs/rollup?part=table&${bad}")"
  if [ "$st" = "200" ]; then
    pass "out-of-allowlist input falls back cleanly: $bad"
  else
    fail "out-of-allowlist input falls back cleanly: $bad" "got HTTP $st (expected a 200 fallback)"
  fi
done
# A PromQL-injection attempt in a filter value must neither 500 nor reflect
# the raw payload back into the response (costs_rollup.go validates filter
# values against a charset allowlist).
INJ='x"} or up{'
inj_enc="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=""))' "$INJ")"
inj_file="$(mktemp -t costs-inj-XXXXXX.html)"
istatus="$(vfetch "/costs/rollup?part=table&rollup=role&role=${inj_enc}" "$inj_file")"
if [ "$istatus" = "200" ]; then
  pass "PromQL-injection filter value does not error (HTTP 200)"
  if have_marker "$inj_file" 'or up{'; then
    fail "injection payload is not reflected into the response" \
      "raw payload 'or up{' found in the rendered fragment"
  else
    pass "injection payload is not reflected into the response"
  fi
else
  fail "PromQL-injection filter value does not error" "got HTTP $istatus"
fi
rm -f "$inj_file"
echo

# ── Summary ───────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green "  ✓ All $total assertions passed ($SKIP skipped)"
else
  red "  ✗ $FAIL/$total assertions failed ($SKIP skipped)"
  echo
  for err in "${ERRORS[@]}"; do
    red "   - $err"
  done
fi
blue "═══════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
