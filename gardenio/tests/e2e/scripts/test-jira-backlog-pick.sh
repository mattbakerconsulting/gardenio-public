#!/usr/bin/env bash
# test-jira-backlog-pick.sh — `gb jira backlog-pick` CLI E2E
# (task gd-LQcsEs2WQT, coverage-backfill epic ka-0000000958).
#
# Closes a ZERO-coverage gap (HEAD-verified 2026-08-06, both suites grepped per
# ka-0000000850): the backlog-mode ticket picker `gb jira backlog-pick`
# (cmd/gb/jira_backlog_pick.go → bridge.BacklogPick) had NO e2e assertion in
# either the shell suite or the Playwright suite. Literals with 0 hits before
# this module: `backlog-pick`, `backlog_pick`, `BacklogPick`, `ParseWaveKeys`,
# and the emitted output tokens `wave %s | pick`, `skipped (gardenio-triaged)`,
# `MISSING (no Jira issue`.
#
# The pure selector SelectFromIssues IS unit-tested (jira_backlog_pick_test.go).
# What had NO coverage is the live CLI wiring only reachable through the binary:
#   - the `key IN (...)` JQL BATCHING (backlogPickJQLBatch=50) — a wave larger
#     than one batch must be walked COMPLETELY, never silently truncated. This
#     is the exact failure the bead names: a partial fetch would drop tickets
#     from the fetch, mislabel them MISSING, and either re-triage a ticket
#     (double spend — $2.84 p50 / $8.67 p90 per ticket) or under-report the wave.
#   - the LIVE gardenio-triaged label dedup read back from Jira at pick time
#     (issueHasTriagedSentinel over the SearchIssues response), including the
#     invariant that a REFINEMENT label (gardenio-triaged-sev-1) is NOT the bare
#     sentinel and must NOT be skipped.
#   - the read-only contract: the picker ISSUES ONLY GET /search/jql and NEVER
#     writes Jira (ReadOnly:true on the client) — proven by a stub that records
#     and 405s every write method.
#
# ── Why a hermetic stub rig, not a live Jira ──────────────────────────────────
#
# Two hard constraints — identical in spirit to test-grafana-users-cli /
# test-jira-reply-mirror-link:
#
#   1. The picker requires direct JIRA read creds (JIRA_BASE_URL / JIRA_EMAIL /
#      JIRA_API_TOKEN), present only on project=monorepo pods (advice
#      ka-0000005529). The per-run e2e namespaces do NOT carry
#      them, and pointing the picker at the real acme.atlassian.net from a
#      shard would couple a HARD gate to live Jira state (labels + created on 88
#      real FSM tickets churn), which is neither hermetic nor deterministic.
#   2. The logic under test is entirely in the `gb` binary + the bridge picker.
#      Driving it needs only a Jira that answers GET /rest/api/3/search/jql with
#      an {"issues":[...]} page whose issues carry labels + created — trivially
#      stubbable, and the EXACT shape SearchIssuesPage decodes (jira.go).
#
# So this module stands up ONE throwaway HTTP stub (Python stdlib, always
# present in the e2e shard — e2e-system-deps.lock ships python3) that speaks the
# Jira JQL search endpoint against an in-memory issue table, and drives the REAL
# `gb` binary against it via the `--keys` path (which needs NO beads daemon).
# The stub RECORDS every search request (batch introspection via GET /__reqs)
# and records + REFUSES every write method (GET /__writes) so the read-only
# contract and the JQL batching are asserted end-to-end, not inferred.
#
# The fixture issue shapes (created timestamps, the gardenio-triaged sentinel and
# the gardenio-triaged-sev-1 refinement label) MIRROR the hand-authored fixtures
# in the sibling unit test jira_backlog_pick_test.go — the same shape the GFO
# outbox stamps and the real Jira search returns (labels[] + created RFC3339).
#
# It is hermetic: ZERO pods, ZERO shared-daemon writes, ZERO live Jira — so it
# is green today and is a HARD gate. It does NOT use the shard's beads
# port-forward tunnel at all (the --keys path never talks to the daemon).
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  positive     a wave with UNTRIAGED keys yields the expected pick — the
#                   oldest un-triaged key (by created), with the full partition
#                   (untriaged / skipped / missing) correct, in BOTH --json and
#                   the human one-liner (`wave B | pick FSM-360 | ...`).
#   2  ordering     oldest-first is a real invariant, not input echo: the SAME
#                   keys in REVERSE input order still pick the oldest, and the
#                   untriaged list is created-ascending — a pick that mirrored
#                   input order would go red here.
#   3  all-triaged  the NEGATIVE that pins the invariant: a wave where EVERY key
#                   already carries gardenio-triaged yields `pick (none)` /
#                   pick:"" — NOT a spurious pick. This is the "false wave-
#                   drained vs. double-spend" failure the bead is about.
#   4  refinement   a key carrying ONLY gardenio-triaged-sev-1 (a refinement
#                   label, NOT the bare sentinel) must be PICKED, not skipped —
#                   the dedup keys on the exact sentinel, so a substring match
#                   here would wrongly drain the wave.
#   5  missing      a key with NO Jira issue lands in MISSING (surfaced), not
#                   picked and not silently dropped — a wave-authoring typo must
#                   be visible.
#   6  batching     a 60-key wave (> the 50-key JQL batch) is fetched in TWO
#                   key-scoped searches covering ALL 60 distinct keys with NO
#                   truncation, and STILL picks the oldest across the batch
#                   boundary. Zero writes recorded — the read-only contract.
#   7  bad-input    no --keys and no --wave-bead is a HARD error (exit non-zero),
#                   never a silent empty pick that a gardener would read as
#                   "drained".
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The `gb` binary + python3 are present in every e2e shard container (the shard
# `use:` list includes build-gb; e2e-system-deps.lock ships python3). If either
# is somehow absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an
# absent binary must never red the suite — mirroring the grafana-users-cli /
# jira-reply-mirror-link skip_all("loud") contract. When present, every leg is a
# HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-jira-backlog-pick.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/jira-backlog-pick.${RUN_ID}.XXXXXX")"
GB_BIN=""
GB_TEMP=""          # non-empty only when we built a throwaway binary to clean up
PIDS=()

PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 (a missing binary must NOT red the suite) with a LOUD banner
# so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP jira-backlog-pick (LOUD): $why"
  red " This run did NOT assert 'gb jira backlog-pick' (oldest-first pick, the"
  red " live gardenio-triaged dedup, the refinement-label non-skip, MISSING"
  red " surfacing, the >50-key JQL batching, and the read-only contract). A"
  red " green suite here proves NOTHING about that command. The e2e shard is"
  red " built with the gb binary (build-gb task) + python3 (e2e-system-deps.lock)"
  red " — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
  # Only remove a gb we built ourselves; never a PATH-installed one.
  [ -n "$GB_TEMP" ] && rm -f "$GB_TEMP" 2>/dev/null || true
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# free_port asks the kernel for an unused TCP port by binding :0 and reading it
# back — the same race-tolerant helper the sibling gb-binary rigs use.
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PY
}

# wait_http polls a URL until it answers (any HTTP status) or times out.
wait_http() {
  local url="$1" tries="${2:-50}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null --max-time 1 "$url" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# jq_py reads a value out of a JSON file via python3 (no jq dependency). The
# second arg is a tiny python expression over `d` (the parsed object).
jq_py() {
  python3 - "$1" "$2" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(""); sys.exit(0)
try:
    print(eval(sys.argv[2]))
except Exception:
    print("")
PY
}

echo "════════════════════════════════════════════════════"
echo " gb jira backlog-pick CLI E2E (gd-LQcsEs2WQT)"
echo " positive · ordering · all-triaged · refinement · missing · batching · bad-input"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the gb binary ────────────────────────────────────────
# Prefer a binary pre-built + installed onto PATH. Fall
# back to an in-shell workspace build for local dev. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip.
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"

# build_gb_from_source builds cmd/gb into a throwaway binary and sets GB_BIN /
# GB_TEMP, or returns non-zero on build failure. cmd/gb imports the local
# clicreds module via go.work (replace => ../clicreds), so it MUST build in
# workspace mode (GOWORK on) — a GOWORK=off build FATALs on the missing
# replacement. Pin GOWORK to the repo-root go.work explicitly.
build_gb_from_source() {
  GB_TEMP="$(mktemp "/tmp/gb.${RUN_ID}.XXXXXX")"
  GB_BIN="$GB_TEMP"
  local build_log="/tmp/gb-build-${RUN_ID}.log"
  if ( GOWORK="$(pwd)/go.work" CGO_ENABLED=0 go build -o "$GB_BIN" ./gardenio/controller/cmd/gb/ ) >"$build_log" 2>&1; then
    return 0
  fi
  BUILD_TAIL="$(tail -8 "$build_log" 2>/dev/null)"
  return 1
}

# gb_has_backlog_pick probes whether the resolved binary actually carries the
# subcommand under test. A PATH-installed gb can predate the backlog-pick feature
# (jira_backlog_pick.go), in which case every leg would hard-fail on "unknown
# flag: --keys" — a false red for a binary that simply lacks the surface. We
# treat an absent subcommand the same way we treat an absent binary: prefer a
# source rebuild, else skip loudly.
#
# NOTE: `gb jira backlog-pick --help` is NOT a reliable probe — the parent `gb
# jira` command has no Run, so an OLD binary treats the unknown `backlog-pick`
# arg as a no-op and prints `gb jira` help with exit 0. Instead grep the parent's
# command listing for the subcommand name, which only appears when it is
# registered (init() → jiraCmd.AddCommand(jiraBacklogPickCmd)).
gb_has_backlog_pick() {
  "$1" jira --help 2>&1 | grep -q 'backlog-pick'
}

if command -v gb >/dev/null 2>&1; then
  GB_BIN="$(command -v gb)"
  if gb_has_backlog_pick "$GB_BIN"; then
    pass "using pre-built gb ($GB_BIN)"
  elif command -v go >/dev/null 2>&1 && [ -d "$CONTROLLER_DIR/cmd/gb" ] && command -v python3 >/dev/null 2>&1; then
    # PATH gb predates `jira backlog-pick`; rebuild from the checked-out source.
    dim "  PATH gb ($GB_BIN) lacks 'jira backlog-pick' — rebuilding from source"
    blue "[build] cmd/gb (from source, workspace mode)"
    if build_gb_from_source && gb_has_backlog_pick "$GB_BIN"; then
      pass "cmd/gb built from source (PATH gb was stale)"
    else
      skip_all "PATH gb lacks 'jira backlog-pick' and a source rebuild did not produce it (build tail: ${BUILD_TAIL:-n/a})"
    fi
  else
    skip_all "PATH gb ($GB_BIN) lacks 'jira backlog-pick' and no go toolchain/source to rebuild it"
  fi
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no gb on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gb" ] \
    || skip_all "no gb on PATH and cmd/gb missing under $CONTROLLER_DIR"
  command -v python3 >/dev/null 2>&1 \
    || skip_all "python3 absent — cannot stand up the stub Jira server"
  blue "[build] cmd/gb (from source, workspace mode)"
  if build_gb_from_source; then
    pass "cmd/gb built from source"
  else
    fail "build gb" "go build failed: ${BUILD_TAIL:-n/a}"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (gb build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi

command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot stand up the stub Jira server"
echo

# ── 2. Stand up the stub Jira JQL-search server ─────────────────────
# One stateful HTTP server that speaks the single endpoint the picker drives:
#   GET   /rest/api/3/search/jql?jql=key IN (...)&fields=...  → {"issues":[...]}
# plus two test-only control endpoints:
#   GET   /__reqs    → {"count":N,"reqs":[{"n":<keys-in-batch>,"keys":[...]}]}
#   GET   /__writes  → {"count":N,"writes":[{"method":..,"path":..}]}
# and it 405s + RECORDS every write method (POST/PUT/DELETE) so the read-only
# contract is proven, not assumed.
#
# The issue table mirrors the unit-test fixtures (jira_backlog_pick_test.go):
# gardenio-triaged is the bare sentinel that dedups; gardenio-triaged-sev-1 is a
# refinement label that must NOT dedup; FSM-999 is deliberately absent → MISSING.
STUB_PY="$WORKDIR/jira_search_stub.py"
cat > "$STUB_PY" <<'PY'
# Throwaway stateful stub of the Jira JQL-search endpoint for the
# gb-jira-backlog-pick E2E. Serves an in-memory issue table keyed by FSM key;
# a `key IN ("FSM-360",...)` JQL returns exactly the named keys that exist.
# Records every search (for batch introspection) and REFUSES + records every
# write method (to prove the picker is read-only). argv[1] = port.
import http.server, json, re, sys, urllib.parse as up

# Fixture issues. created timestamps chosen so oldest-first ordering is
# UNAMBIGUOUS and does NOT follow numeric-key order (so the ordering leg is a
# real test): FSM-361 is the oldest date but is triaged; among untriaged,
# FSM-360 (Feb) is older than FSM-395 (Jun).
ISSUES = {
    "FSM-360": {"key": "FSM-360", "fields": {
        "labels": [], "created": "2026-02-01T10:00:00.000+0000",
        "summary": "oldest untriaged", "status": {"name": "Open"}}},
    "FSM-361": {"key": "FSM-361", "fields": {
        "labels": ["gardenio-triaged"], "created": "2026-01-01T10:00:00.000+0000",
        "summary": "already triaged (bare sentinel, and oldest date)",
        "status": {"name": "Done"}}},
    "FSM-395": {"key": "FSM-395", "fields": {
        "labels": [], "created": "2026-06-15T10:00:00.000+0000",
        "summary": "newer untriaged", "status": {"name": "Open"}}},
    # Refinement-only label: gardenio-triaged-sev-1 is NOT the bare sentinel, so
    # this key must be PICKED, never skipped (issueHasTriagedSentinel matches the
    # exact sentinel only).
    "FSM-400": {"key": "FSM-400", "fields": {
        "labels": ["gardenio-triaged-sev-1", "sev-1"],
        "created": "2026-03-01T00:00:00.000+0000",
        "summary": "refinement label only — must NOT dedup",
        "status": {"name": "Open"}}},
    # FSM-999 deliberately ABSENT → MISSING.
}
# Batch fixture: 60 untriaged keys FSM-100..FSM-159, created strictly ascending
# so FSM-100 is unambiguously the oldest across the > 50-key batch boundary.
for _i in range(60):
    _k = "FSM-%d" % (100 + _i)
    ISSUES[_k] = {"key": _k, "fields": {
        "labels": [], "created": "2026-01-01T00:%02d:00.000+0000" % _i,
        "summary": _k, "status": {"name": "Open"}}}

STATE = {"reqs": [], "writes": []}
KEYRE = re.compile(r'"(FSM-\d+)"')

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _record_write(self):
        STATE["writes"].append({"method": self.command, "path": self.path})
        # A picker that ever tried to write is a bug — fail the write loudly.
        return self._json(405, {"message": "read-only stub: writes forbidden"})

    def do_GET(self):
        u = up.urlparse(self.path)
        q = up.parse_qs(u.query)
        if u.path == "/rest/api/3/search/jql":
            jql = q.get("jql", [""])[0]
            wanted = KEYRE.findall(jql)
            STATE["reqs"].append({"n": len(wanted), "keys": wanted})
            issues = [ISSUES[k] for k in wanted if k in ISSUES]
            return self._json(200, {"issues": issues, "nextPageToken": ""})
        if u.path == "/__reqs":
            return self._json(200, {"count": len(STATE["reqs"]), "reqs": STATE["reqs"]})
        if u.path == "/__writes":
            return self._json(200, {"count": len(STATE["writes"]), "writes": STATE["writes"]})
        if u.path == "/__reset":
            STATE["reqs"] = []
            STATE["writes"] = []
            return self._json(200, {"ok": True})
        return self._json(404, {"message": "not found " + u.path})

    def do_POST(self):   return self._record_write()
    def do_PUT(self):    return self._record_write()
    def do_DELETE(self): return self._record_write()

port = int(sys.argv[1])
http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

STUB_PORT="$(free_port)"
python3 "$STUB_PY" "$STUB_PORT" & PIDS+=($!)
STUB_URL="http://127.0.0.1:${STUB_PORT}"

if ! wait_http "${STUB_URL}/__reqs"; then
  skip_all "stub Jira server failed to come up (python3 http.server unavailable?)"
fi
pass "stub Jira JQL-search server up ($STUB_URL)"
echo

# The picker builds its live JIRA client from these three env vars. The stub is
# auth-disabled; point every var at the stub and give it a throwaway token.
export JIRA_BASE_URL="$STUB_URL"
export JIRA_EMAIL="e2e-backlog-pick@example.com"
export JIRA_API_TOKEN="stub-token-not-a-secret"

# Convenience wrappers around the real binary + stub introspection.
gb_pick() { "$GB_BIN" jira backlog-pick "$@"; }
reset_stub()  { curl -s -o /dev/null "${STUB_URL}/__reset"; }
req_count()   { curl -s "${STUB_URL}/__reqs"   | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])' 2>/dev/null || echo -1; }
write_count() { curl -s "${STUB_URL}/__writes" | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])' 2>/dev/null || echo -1; }

# ── Leg 1: positive (untriaged + skipped + missing, --json + human) ─
blue "[leg positive] wave of untriaged/triaged/missing → pick oldest untriaged"
reset_stub
P1="$WORKDIR/positive.json"
if gb_pick --keys "FSM-360,FSM-361,FSM-395,FSM-999" --wave B --json >"$P1" 2>"$WORKDIR/positive.err"; then
  pick="$(jq_py "$P1" 'd["pick"]')"
  untr="$(jq_py "$P1" '",".join(d["untriaged"])')"
  skip="$(jq_py "$P1" '",".join(d["skipped"])')"
  miss="$(jq_py "$P1" '",".join(d["missing"])')"
  wave="$(jq_py "$P1" 'd["wave"]')"
  # Human one-liner must carry the pick + partition tokens the gardener logs.
  H1="$WORKDIR/positive.human"
  gb_pick --keys "FSM-360,FSM-361,FSM-395,FSM-999" --wave B >"$H1" 2>/dev/null
  if [ "$pick" = "FSM-360" ] && [ "$untr" = "FSM-360,FSM-395" ] \
     && [ "$skip" = "FSM-361" ] && [ "$miss" = "FSM-999" ] && [ "$wave" = "B" ] \
     && grep -q 'wave B | pick FSM-360' "$H1" \
     && grep -q 'skipped (gardenio-triaged): FSM-361' "$H1" \
     && grep -q 'MISSING (no Jira issue' "$H1"; then
    pass "leg positive — pick=FSM-360, untriaged=[FSM-360,FSM-395], skipped=[FSM-361], missing=[FSM-999], human one-liner correct"
  else
    fail "leg positive" "pick=$pick untriaged=[$untr] skipped=[$skip] missing=[$miss] wave=$wave / human=$(tr '\n' '|' <"$H1")"
  fi
else
  fail "leg positive" "gb exited non-zero: $(tail -3 "$WORKDIR/positive.err" 2>/dev/null)"
fi
echo

# ── Leg 2: ordering (reverse input still picks oldest; not input echo) ──
blue "[leg ordering] reverse input order → still oldest-first (FSM-360 before FSM-395)"
reset_stub
P2="$WORKDIR/ordering.json"
# Reverse the input so a pick that echoed input order would return FSM-395.
if gb_pick --keys "FSM-395,FSM-360" --wave B --json >"$P2" 2>"$WORKDIR/ordering.err"; then
  pick="$(jq_py "$P2" 'd["pick"]')"
  untr="$(jq_py "$P2" '",".join(d["untriaged"])')"
  if [ "$pick" = "FSM-360" ] && [ "$untr" = "FSM-360,FSM-395" ]; then
    pass "leg ordering — reverse input still picks oldest FSM-360, untriaged created-ascending"
  else
    fail "leg ordering" "pick=$pick untriaged=[$untr] (want FSM-360 / FSM-360,FSM-395 — a pick mirroring input order regressed)"
  fi
else
  fail "leg ordering" "gb exited non-zero: $(tail -3 "$WORKDIR/ordering.err" 2>/dev/null)"
fi
echo

# ── Leg 3: all-triaged (the negative — pick (none), NOT a spurious pick) ──
blue "[leg all-triaged] every key already gardenio-triaged → pick (none)"
reset_stub
P3="$WORKDIR/all-triaged.json"
if gb_pick --keys "FSM-361" --wave B --json >"$P3" 2>"$WORKDIR/all-triaged.err"; then
  pick="$(jq_py "$P3" 'd["pick"]')"
  untr="$(jq_py "$P3" 'len(d["untriaged"])')"
  skip="$(jq_py "$P3" '",".join(d["skipped"])')"
  H3="$WORKDIR/all-triaged.human"
  gb_pick --keys "FSM-361" --wave B >"$H3" 2>/dev/null
  if [ "$pick" = "" ] && [ "$untr" = 0 ] && [ "$skip" = "FSM-361" ] \
     && grep -q 'pick (none)' "$H3"; then
    pass "leg all-triaged — pick empty, 0 untriaged, FSM-361 skipped, human reads 'pick (none)'"
  else
    fail "leg all-triaged" "pick='$pick' untriaged=$untr skipped=[$skip] / human=$(tr '\n' '|' <"$H3") (want empty pick / 0 / FSM-361 / 'pick (none)')"
  fi
else
  fail "leg all-triaged" "gb exited non-zero: $(tail -3 "$WORKDIR/all-triaged.err" 2>/dev/null)"
fi
echo

# ── Leg 4: refinement (gardenio-triaged-sev-1 is NOT the sentinel → picked) ──
blue "[leg refinement] key with ONLY gardenio-triaged-sev-1 → PICKED, not skipped"
reset_stub
P4="$WORKDIR/refinement.json"
if gb_pick --keys "FSM-400" --wave R --json >"$P4" 2>"$WORKDIR/refinement.err"; then
  pick="$(jq_py "$P4" 'd["pick"]')"
  skip="$(jq_py "$P4" 'len(d["skipped"] or [])')"
  if [ "$pick" = "FSM-400" ] && [ "$skip" = 0 ]; then
    pass "leg refinement — FSM-400 (gardenio-triaged-sev-1 only) picked, 0 skipped — dedup keys the bare sentinel exactly"
  else
    fail "leg refinement" "pick=$pick skipped=$skip (want FSM-400 / 0 — a substring dedup would wrongly skip it)"
  fi
else
  fail "leg refinement" "gb exited non-zero: $(tail -3 "$WORKDIR/refinement.err" 2>/dev/null)"
fi
echo

# ── Leg 5: missing (no Jira issue → surfaced in MISSING, not picked) ──
blue "[leg missing] a key with no Jira issue → MISSING, not picked, not dropped"
reset_stub
P5="$WORKDIR/missing.json"
if gb_pick --keys "FSM-360,FSM-999" --wave B --json >"$P5" 2>"$WORKDIR/missing.err"; then
  pick="$(jq_py "$P5" 'd["pick"]')"
  miss="$(jq_py "$P5" '",".join(d["missing"])')"
  if [ "$pick" = "FSM-360" ] && [ "$miss" = "FSM-999" ]; then
    pass "leg missing — FSM-999 surfaced in MISSING (not picked, not silently dropped), FSM-360 still picked"
  else
    fail "leg missing" "pick=$pick missing=[$miss] (want FSM-360 / FSM-999)"
  fi
else
  fail "leg missing" "gb exited non-zero: $(tail -3 "$WORKDIR/missing.err" 2>/dev/null)"
fi
echo

# ── Leg 6: batching (> 50 keys → 2 searches, all 60 covered, oldest picked, 0 writes) ──
blue "[leg batching] 60-key wave → 2 key-scoped searches, all 60 covered, oldest picked, ZERO writes"
reset_stub
KEYS60="$(python3 -c 'print(",".join("FSM-%d" % (100+i) for i in range(60)))')"
P6="$WORKDIR/batching.json"
if gb_pick --keys "$KEYS60" --wave BATCH --json >"$P6" 2>"$WORKDIR/batching.err"; then
  pick="$(jq_py "$P6" 'd["pick"]')"
  nuntr="$(jq_py "$P6" 'len(d["untriaged"])')"
  nsearches="$(req_count)"
  writes="$(write_count)"
  # The stub recorded each batch's key count; assert the union covers all 60
  # distinct keys with no truncation (batch sizes 50 + 10).
  covered="$(curl -s "${STUB_URL}/__reqs" | python3 -c 'import json,sys; d=json.load(sys.stdin); ks=[k for r in d["reqs"] for k in r["keys"]]; print(len(set(ks)))' 2>/dev/null || echo -1)"
  batchsizes="$(curl -s "${STUB_URL}/__reqs" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(",".join(str(r["n"]) for r in d["reqs"]))' 2>/dev/null || echo '?')"
  if [ "$pick" = "FSM-100" ] && [ "$nuntr" = 60 ] && [ "$nsearches" = 2 ] \
     && [ "$covered" = 60 ] && [ "$writes" = 0 ] && [ "$batchsizes" = "50,10" ]; then
    pass "leg batching — 60 keys in 2 searches (sizes 50,10), all 60 distinct covered, pick=FSM-100 across the boundary, 0 writes"
  else
    fail "leg batching" "pick=$pick untriaged=$nuntr searches=$nsearches covered=$covered writes=$writes batchsizes=$batchsizes (want FSM-100/60/2/60/0/50,10)"
  fi
else
  fail "leg batching" "gb exited non-zero: $(tail -3 "$WORKDIR/batching.err" 2>/dev/null)"
fi
echo

# ── Leg 7: bad-input (no --keys and no --wave-bead → hard error) ────
blue "[leg bad-input] neither --keys nor --wave-bead → hard error, never a silent empty pick"
reset_stub
if gb_pick --wave B --json >"$WORKDIR/bad.out" 2>"$WORKDIR/bad.err"; then
  fail "leg bad-input" "gb exited 0 with no worklist — a missing --keys/--wave-bead must be a hard error, not a silent 'drained' pick"
else
  if grep -qi 'keys or --wave-bead' "$WORKDIR/bad.err"; then
    pass "leg bad-input — non-zero exit, actionable 'provide the wave's keys via --keys or --wave-bead' error"
  else
    fail "leg bad-input msg" "non-zero exit but error not actionable: $(tail -2 "$WORKDIR/bad.err")"
  fi
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
