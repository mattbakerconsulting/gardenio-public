#!/usr/bin/env bash
# pg-scratch_test.sh — tests for pg-scratch.sh (gd-ELlwvze8G3).
#
# The `agent` image is release-only (images.yml drops it on pull_request), so
# these run the helper on a PR. The refusal cases need no server and always
# run. The live cases start a real server and skip when the Postgres 16 server
# or pgvector is absent (CI runners); on an agent pod they run. Point
# PG_SCRATCH_TEST_BINDIR at another .../usr/lib/postgresql/16/bin (say, debs
# unpacked with dpkg -x) to run them anywhere. A few seconds with a server.
#
# Usage:  bash gardenio/images/agent/pg-scratch_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HELPER="$SCRIPT_DIR/pg-scratch.sh"
REAL_BINDIR="${PG_SCRATCH_TEST_BINDIR:-/usr/lib/postgresql/16/bin}"

PASS=0
FAIL=0
TMPROOT="$(mktemp -d)"
trap 'PG_SCRATCH_STATE="$TMPROOT/state" PG_SCRATCH_BINDIR="$REAL_BINDIR" bash "$HELPER" stop >/dev/null 2>&1 || true; rm -rf "$TMPROOT"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# run BINDIR ARGS... — the helper with its state and TMPDIR under TMPROOT;
# stdout to $TMPROOT/out, stderr to $TMPROOT/err, returns its exit code.
run() {
  local bindir="$1"
  shift
  PG_SCRATCH_BINDIR="$bindir" PG_SCRATCH_STATE="$TMPROOT/state" TMPDIR="$TMPROOT/tmp" \
    bash "$HELPER" "$@" >"$TMPROOT/out" 2>"$TMPROOT/err"
}

# expect NAME WANT_RC WANT_STDERR_SUBSTRING BINDIR ARGS...
expect() {
  local name="$1" want_rc="$2" want="$3" rc=0
  shift 3
  run "$@" || rc=$?
  if [ "$rc" != "$want_rc" ]; then
    fail "$name" "exit $rc, want $want_rc; stderr: $(cat "$TMPROOT/err")"
  elif ! grep -qF -- "$want" "$TMPROOT/err"; then
    fail "$name" "stderr lacks '$want': $(cat "$TMPROOT/err")"
  else
    pass "$name"
  fi
}

mkdir -p "$TMPROOT/tmp" "$TMPROOT/empty"

# ── Refusals: no server needed ───────────────────────────────────────
expect "no command" 2 "usage:" "$TMPROOT/empty"
expect "unknown command" 2 "usage:" "$TMPROOT/empty" restart
expect "start without server binaries" 1 "no Postgres server in $TMPROOT/empty" "$TMPROOT/empty" start
if [ -e "$TMPROOT/state" ] || [ -n "$(ls -A "$TMPROOT/tmp")" ]; then
  fail "failed start leaves nothing" "state or tmp dir left: $(ls -A "$TMPROOT" "$TMPROOT/tmp")"
else
  pass "failed start leaves nothing"
fi
expect "stop with nothing running" 0 "no scratch server to stop" "$TMPROOT/empty" stop
expect "env with nothing running" 1 "no scratch server" "$TMPROOT/empty" env

mkdir -p "$TMPROOT/precious"
echo keep >"$TMPROOT/precious/file"
echo "$TMPROOT/precious" >"$TMPROOT/state"
expect "stop never deletes a directory it did not make" 1 "not a pg-scratch directory" "$TMPROOT/empty" stop
if [ -f "$TMPROOT/precious/file" ]; then pass "foreign directory survives"; else fail "foreign directory survives" "deleted"; fi
rm -f "$TMPROOT/state"

# ── Live: a real server ──────────────────────────────────────────────
pg_root="${REAL_BINDIR%/usr/lib/postgresql/16/bin}"
if [ ! -x "$REAL_BINDIR/initdb" ] || [ ! -f "$pg_root/usr/share/postgresql/16/extension/vector.control" ]; then
  echo "SKIP: live cases: no Postgres 16 server + pgvector at $REAL_BINDIR"
elif [ "$(id -u)" = 0 ]; then
  echo "SKIP: live cases: running as root (initdb refuses root)"
else
  if run "$REAL_BINDIR" start; then
    pass "start"
    url_line="$(cat "$TMPROOT/out")"
    if [[ "$url_line" =~ ^export\ LANDS_CONFORMANCE_DATABASE_URL=\'postgres://postgres@127\.0\.0\.1:[0-9]+/scratch\?sslmode=disable\'$ ]]; then
      pass "start prints only the export line"
    else
      fail "start prints only the export line" "stdout: $url_line"
    fi
    eval "$url_line"
    ext="$(psql "$LANDS_CONFORMANCE_DATABASE_URL" -tAc "SELECT extname FROM pg_extension WHERE extname = 'vector'" 2>&1)" || true
    if [ "$ext" = vector ]; then pass "vector extension is created"; else fail "vector extension is created" "$ext"; fi
    tables="$(psql "$LANDS_CONFORMANCE_DATABASE_URL" -tAc "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" 2>&1)" || true
    if [ "$tables" = 0 ]; then pass "database is empty"; else fail "database is empty" "$tables"; fi

    expect "second start refuses while one is live" 1 "already running" "$REAL_BINDIR" start
    run "$REAL_BINDIR" env || true
    if [ "$(cat "$TMPROOT/out")" = "$url_line" ]; then pass "env reprints the export line"; else fail "env reprints the export line" "$(cat "$TMPROOT/out")"; fi

    dir="$(cat "$TMPROOT/state")"
    expect "stop" 0 "stopped and deleted" "$REAL_BINDIR" stop
    if [ -e "$dir" ] || [ -e "$TMPROOT/state" ]; then fail "stop deletes the directory and state" "left: $dir"; else pass "stop deletes the directory and state"; fi
    if psql "$LANDS_CONFORMANCE_DATABASE_URL" -tAc 'SELECT 1' >/dev/null 2>&1; then
      fail "server is down after stop" "still answers"
    else
      pass "server is down after stop"
    fi
  else
    fail "start" "$(cat "$TMPROOT/err")"
  fi
fi

echo "pg-scratch_test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
