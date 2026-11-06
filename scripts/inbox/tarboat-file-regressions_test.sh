#!/usr/bin/env bash
# tarboat-file-regressions_test.sh — Unit tests for the E2 regression-bug filer
# (Test-in-box E2, ka-0000001077, epic ka-0000003645).
#
# The filer reads a tarboat-validate/v1 results doc and runs `kd create
# --type=bug` per failing gate, de-duping against already-open bugs. We drive it
# end-to-end against a STUB `kd` on PATH (no real beads server, no network): the
# stub records `create` invocations to a log and answers `list --json` from a
# fixture so we can exercise the de-dupe path deterministically.
#
# Locks: schema rejection, no-fail no-op, one-bug-per-failing-gate, the gate
# key + calver + assertion message all reach the create call, (gate,calver)
# de-dupe, and that skip/pass gates never file.
#
# Usage:  bash scripts/inbox/tarboat-file-regressions_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/tarboat-file-regressions.sh"

command -v jq >/dev/null 2>&1 || { echo "FATAL: jq required for this test" >&2; exit 2; }

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── stub `kd` ────────────────────────────────────────────────────────────────
# create → append a TAB-joined record of (parent-task, title, gate, calver,
#          labels) to $KD_CREATE_LOG and print a fake id. The (gate,calver)
#          identity is carried as tarboat-gate:/tarboat-calver: LABELS (the real
#          beads server rejects arbitrary typed -f fields), so the stub derives
#          gate/calver from the -l labels — NOT from -f fields.
# list   → emit the JSON in $KD_LIST_FIXTURE (defaults to []).
BIN="$TMP/bin"
mkdir -p "$BIN"
cat > "$BIN/kd" <<'STUB'
#!/usr/bin/env bash
cmd="${1:-}"; shift || true
if [ "$cmd" = create ]; then
  title=""; parent=""; gate=""; calver=""; labels=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --parent-task) parent="$2"; shift 2;;
      --operated-by) shift 2;;
      --type=*|--priority|-d) [ "$1" = -d ] && shift 2 || { [ "$1" = --priority ] && shift 2 || shift; };;
      -l) labels="$labels,$2"
          case "$2" in
            tarboat-gate:*)   gate="${2#tarboat-gate:}";;
            tarboat-calver:*) calver="${2#tarboat-calver:}";;
          esac
          shift 2;;
      -f) shift 2;;
      -*) shift;;
      *) [ -z "$title" ] && title="$1"; shift;;
    esac
  done
  printf '%s\t%s\t%s\t%s\t%s\n' "$parent" "$title" "$gate" "$calver" "${labels#,}" >> "$KD_CREATE_LOG"
  echo "Created gd-STUB$(wc -l < "$KD_CREATE_LOG" | tr -d ' ')"
  exit 0
elif [ "$cmd" = list ]; then
  cat "${KD_LIST_FIXTURE:-/dev/null}" 2>/dev/null || echo '[]'
  exit 0
fi
echo "stub kd: unhandled cmd $cmd" >&2; exit 3
STUB
chmod +x "$BIN/kd"

# A tarboat-validate/v1 doc builder. $1=calver, remaining args are
# "key:status:message" gate specs.
make_doc() {
  local calver="$1"; shift
  local gates="" g key status msg
  for g in "$@"; do
    key="${g%%:*}"; local rest="${g#*:}"; status="${rest%%:*}"; msg="${rest#*:}"
    [ -n "$gates" ] && gates="$gates,"
    gates="$gates$(jq -nc --arg k "$key" --arg s "$status" --arg m "$msg" \
      '{n:1,key:$k,name:("gate "+$k),status:$s,duration_seconds:3,message:$m}')"
  done
  jq -nc --arg c "$calver" --argjson gates "[$gates]" \
    '{schema:"tarboat-validate/v1",tarboat:"/srv/tarboat",namespace:"gardenio",
      release:"gardenio",calver:$c,ip:"10.0.0.9",started_at:"t0",finished_at:"t1",
      gates_run:($gates|length),gates_passed:0,gates_failed:0,gates_skipped:0,
      overall:"fail",release_grade:false,exit_code:1,gates:$gates}'
}

run_filer() {  # run_filer <doc-file> [extra args...] ; sets OUT, RC; PATH has stub kd
  OUT="$(PATH="$BIN:$PATH" KD_CREATE_LOG="$TMP/create.log" \
         KD_LIST_FIXTURE="${KD_LIST_FIXTURE:-}" bash "$SUT" "$@" 2>&1)" && RC=0 || RC=$?
}

# ── 1. invalid JSON → exit 2, files nothing ──────────────────────────────────
: > "$TMP/create.log"
echo 'not json' > "$TMP/bad.json"
KD_LIST_FIXTURE="" run_filer "$TMP/bad.json"
if [ "$RC" = 2 ] && [ ! -s "$TMP/create.log" ]; then pass "invalid JSON rejected (exit 2, no create)"
else fail "invalid JSON rejected" "rc=$RC log=$(cat "$TMP/create.log")"; fi

# ── 2. wrong schema → exit 2 ─────────────────────────────────────────────────
: > "$TMP/create.log"
echo '{"schema":"something/v9","gates":[]}' > "$TMP/wrong.json"
KD_LIST_FIXTURE="" run_filer "$TMP/wrong.json"
if [ "$RC" = 2 ] && grep -q "unsupported schema" <<<"$OUT"; then pass "wrong schema rejected"
else fail "wrong schema rejected" "rc=$RC out=$OUT"; fi

# ── 3. all-pass doc → exit 0, files nothing ──────────────────────────────────
: > "$TMP/create.log"
make_doc "2026.161.1" "staging:pass:" "bootstrap:pass:" > "$TMP/green.json"
KD_LIST_FIXTURE="" run_filer "$TMP/green.json"
if [ "$RC" = 0 ] && [ ! -s "$TMP/create.log" ]; then pass "all-pass doc files no bugs"
else fail "all-pass doc files no bugs" "rc=$RC log=$(cat "$TMP/create.log")"; fi

# ── 4. one failing gate → one bug, carrying gate+calver+assertion ────────────
: > "$TMP/create.log"
make_doc "2026.161.2" "staging:pass:" "inference:fail:LiteLLM round-trip returned 503" \
  > "$TMP/onefail.json"
KD_LIST_FIXTURE="" run_filer "$TMP/onefail.json"
created="$(wc -l < "$TMP/create.log" | tr -d ' ')"
rec="$(cat "$TMP/create.log")"
if [ "$RC" = 0 ] && [ "$created" = 1 ] \
   && grep -q $'\tinference\t2026.161.2\t' <<<"$rec" \
   && grep -q "ka-0000003645" <<<"$rec" \
   && grep -qi "inference" <<<"$rec"; then
  pass "single failing gate → one bug parented to epic with gate+calver"
else fail "single failing gate → one bug" "rc=$RC created=$created rec=$rec"; fi

# 4b. the failing assertion message reaches `kd create -d`
:> "$TMP/create.log"
DESC_BIN="$TMP/binDesc"; mkdir -p "$DESC_BIN"
cat > "$DESC_BIN/kd" <<'STUB'
#!/usr/bin/env bash
[ "${1:-}" = create ] && { shift; while [ "$#" -gt 0 ]; do [ "$1" = -d ] && { echo "$2" >> "$KD_DESC_LOG"; shift 2; continue; }; shift; done; echo "Created gd-STUBD"; exit 0; }
[ "${1:-}" = list ] && { echo '[]'; exit 0; }
exit 3
STUB
chmod +x "$DESC_BIN/kd"
OUT="$(PATH="$DESC_BIN:$PATH" KD_DESC_LOG="$TMP/desc.log" bash "$SUT" "$TMP/onefail.json" 2>&1)" && rc=0 || rc=$?
if [ "$rc" = 0 ] && grep -q "LiteLLM round-trip returned 503" "$TMP/desc.log"; then
  pass "failing assertion message reaches bug description"
else fail "assertion in description" "rc=$rc desc=$(cat "$TMP/desc.log" 2>/dev/null)"; fi

# ── 5. multiple fails + a skip → one bug per FAIL only (skip never files) ─────
: > "$TMP/create.log"
make_doc "2026.161.3" "staging:fail:SHA256SUMS verification failed" \
  "bootstrap:skip:no box reachable" "inference:fail:egress not default-deny" \
  > "$TMP/multi.json"
KD_LIST_FIXTURE="" run_filer "$TMP/multi.json"
created="$(wc -l < "$TMP/create.log" | tr -d ' ')"
if [ "$RC" = 0 ] && [ "$created" = 2 ] \
   && grep -q $'\tstaging\t' "$TMP/create.log" \
   && grep -q $'\tinference\t' "$TMP/create.log" \
   && ! grep -q $'\tbootstrap\t' "$TMP/create.log"; then
  pass "two fails + one skip → exactly two bugs (skip excluded)"
else fail "fails-only filing" "rc=$RC created=$created log=$(cat "$TMP/create.log")"; fi

# ── 6. de-dupe: an open bug for same gate+calver → skip, no new create ────────
: > "$TMP/create.log"
cat > "$TMP/existing.json" <<'EOF'
[ { "id":"gd-OPEN1", "type":"bug", "status":"open",
    "labels": [ "tarboat-regression", "tarboat-gate:inference", "tarboat-calver:2026.161.4" ] } ]
EOF
make_doc "2026.161.4" "inference:fail:still broken" > "$TMP/dup.json"
KD_LIST_FIXTURE="$TMP/existing.json" run_filer "$TMP/dup.json"
if [ "$RC" = 0 ] && [ ! -s "$TMP/create.log" ] && grep -qi "already exists" <<<"$OUT"; then
  pass "de-dupe: open bug for same gate+calver suppresses a new file"
else fail "de-dupe same gate+calver" "rc=$RC log=$(cat "$TMP/create.log") out=$OUT"; fi

# 6b. de-dupe is calver-scoped: same gate, DIFFERENT calver → still files
: > "$TMP/create.log"
make_doc "2026.161.5" "inference:fail:broken on newer box" > "$TMP/dup2.json"
KD_LIST_FIXTURE="$TMP/existing.json" run_filer "$TMP/dup2.json"
if [ "$RC" = 0 ] && [ "$(wc -l < "$TMP/create.log" | tr -d ' ')" = 1 ]; then
  pass "de-dupe is calver-scoped: same gate on a new calver still files"
else fail "de-dupe calver-scoped" "rc=$RC log=$(cat "$TMP/create.log")"; fi

# ── 7. --dry-run never invokes kd create ─────────────────────────────────────
: > "$TMP/create.log"
KD_LIST_FIXTURE="" run_filer --dry-run "$TMP/multi.json"
if [ "$RC" = 0 ] && [ ! -s "$TMP/create.log" ] && grep -q "dry-run" <<<"$OUT"; then
  pass "--dry-run files nothing"
else fail "--dry-run" "rc=$RC log=$(cat "$TMP/create.log") out=$OUT"; fi

# ── 8. stdin ('-') is accepted ───────────────────────────────────────────────
: > "$TMP/create.log"
OUT="$(PATH="$BIN:$PATH" KD_CREATE_LOG="$TMP/create.log" \
       bash "$SUT" - < "$TMP/onefail.json" 2>&1)" && rc=0 || rc=$?
if [ "$rc" = 0 ] && [ "$(wc -l < "$TMP/create.log" | tr -d ' ')" = 1 ]; then
  pass "reads results doc from stdin"
else fail "stdin" "rc=$rc log=$(cat "$TMP/create.log")"; fi

# ── summary ──────────────────────────────────────────────────────────────────
echo
echo "tarboat-file-regressions_test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
