#!/usr/bin/env bash
# pg-scratch — a throwaway Postgres 16 with pgvector for one agent's tests
# (gd-ELlwvze8G3). The agent image installs it as /usr/local/bin/pg-scratch.
#
#   eval "$(pg-scratch start)"  initdb a fresh cluster under ${TMPDIR:-/tmp},
#                               start it on 127.0.0.1 and a free port, create
#                               an empty database with the vector extension,
#                               and print the export line for
#                               LANDS_CONFORMANCE_DATABASE_URL.
#   pg-scratch env              print that export line again (a Bash tool call
#                               is a fresh shell, so the variable does not
#                               survive into the next call).
#   pg-scratch stop             stop the server and delete its directory.
#
# Nothing starts it by default: pods have a 3Gi limit (gd-QLhMjEsBsR), so the
# server is small (shared_buffers=32MB, max_connections=20, fsync off) and only
# runs between start and stop. One server per state file; start refuses while
# one is live, so a database a test has already migrated is never handed out as
# empty. Only stdout carries the export line; progress and errors go to stderr.
#
# Env: PG_SCRATCH_BINDIR  server binaries (default /usr/lib/postgresql/16/bin)
#      PG_SCRATCH_STATE   file naming the live server's directory
#                         (default ${TMPDIR:-/tmp}/pg-scratch.current)

set -euo pipefail

BINDIR="${PG_SCRATCH_BINDIR:-/usr/lib/postgresql/16/bin}"
STATE="${PG_SCRATCH_STATE:-${TMPDIR:-/tmp}/pg-scratch.current}"
MARKER=PG_SCRATCH
DBNAME=scratch

die() { echo "pg-scratch: $*" >&2; exit 1; }

usage() {
  # OK: help text naming the caller's eval of start's own output, not an eval here
  echo "usage: eval \"\$(pg-scratch start)\" | pg-scratch env | pg-scratch stop" >&2
  exit 2
}

# running DIR — true when DIR's server answers pg_ctl status.
running() {
  [ -x "$BINDIR/pg_ctl" ] && "$BINDIR/pg_ctl" -D "$1/data" status >/dev/null 2>&1
}

# remove DIR — stop DIR's server and delete DIR, but only a directory start
# made (it holds the marker file); anything else is left alone.
remove() {
  local dir="$1"
  [ -d "$dir" ] || return 0
  [ -f "$dir/$MARKER" ] || die "refusing to delete $dir: it is not a pg-scratch directory"
  if running "$dir"; then
    "$BINDIR/pg_ctl" -D "$dir/data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$dir"
}

free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

cmd_start() {
  [ "$(id -u)" != 0 ] || die "run as the agent user, not root (initdb refuses root)"
  local bin
  for bin in initdb postgres pg_ctl; do
    [ -x "$BINDIR/$bin" ] ||
      die "no Postgres server in $BINDIR (the agent image installs postgresql-16 and postgresql-16-pgvector)"
  done
  command -v psql >/dev/null || die "psql is not on PATH"
  command -v python3 >/dev/null || die "python3 is not on PATH (it picks the free port)"

  if [ -f "$STATE" ]; then
    local old
    old="$(cat "$STATE")"
    if [ -d "$old" ] && running "$old"; then
      die "a scratch server is already running in $old; run pg-scratch stop first"
    fi
    remove "$old"
    rm -f "$STATE"
  fi

  local dir
  dir="$(mktemp -d "${TMPDIR:-/tmp}/pg-scratch.XXXXXX")"
  touch "$dir/$MARKER"
  # A failed start leaves nothing behind; the trap reads the global.
  STARTING_DIR="$dir"
  trap 'remove "$STARTING_DIR"; rm -f "$STATE"' EXIT
  echo "$dir" >"$STATE"

  "$BINDIR/initdb" -D "$dir/data" -U postgres -A trust -E UTF8 --locale=C --no-sync \
    >"$dir/initdb.log" 2>&1 || { cat "$dir/initdb.log" >&2; die "initdb failed"; }
  # No unix socket: a socket path under a long TMPDIR passes the 107-byte limit.
  cat >>"$dir/data/postgresql.conf" <<'EOF'
listen_addresses = '127.0.0.1'
unix_socket_directories = ''
shared_buffers = 32MB
max_connections = 20
fsync = off
synchronous_commit = off
full_page_writes = off
EOF

  local port="" attempt
  for attempt in 1 2 3; do
    port="$(free_port)"
    if "$BINDIR/pg_ctl" -D "$dir/data" -l "$dir/server.log" -o "-p $port" -w -t 60 start >/dev/null 2>&1; then
      break
    fi
    port=""
    echo "pg-scratch: start on port attempt $attempt failed, retrying" >&2
  done
  [ -n "$port" ] || { cat "$dir/server.log" >&2 2>/dev/null || true; die "the server did not start"; }

  local admin="postgres://postgres@127.0.0.1:$port/postgres?sslmode=disable"
  local url="postgres://postgres@127.0.0.1:$port/$DBNAME?sslmode=disable"
  psql "$admin" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $DBNAME" >&2 ||
    die "CREATE DATABASE failed"
  psql "$url" -v ON_ERROR_STOP=1 -qc 'CREATE EXTENSION vector' >&2 ||
    die "CREATE EXTENSION vector failed (is postgresql-16-pgvector installed?)"

  echo "export LANDS_CONFORMANCE_DATABASE_URL='$url'" >"$dir/env"
  trap - EXIT
  echo "pg-scratch: Postgres 16 + pgvector on 127.0.0.1:$port in $dir; pg-scratch stop when done" >&2
  cat "$dir/env"
}

cmd_env() {
  # OK: hint text naming the caller's eval of start's own output, not an eval here
  [ -f "$STATE" ] || die "no scratch server; run: eval \"\$(pg-scratch start)\""
  local dir
  dir="$(cat "$STATE")"
  if [ ! -f "$dir/env" ] || ! running "$dir"; then
    die "the scratch server in $dir is not running; run pg-scratch stop, then start"
  fi
  cat "$dir/env"
}

cmd_stop() {
  if [ ! -f "$STATE" ]; then
    echo "pg-scratch: no scratch server to stop" >&2
    return 0
  fi
  local dir
  dir="$(cat "$STATE")"
  rm -f "$STATE"
  remove "$dir"
  echo "pg-scratch: stopped and deleted $dir" >&2
}

[ $# -eq 1 ] || usage
case "$1" in
  start) cmd_start ;;
  env) cmd_env ;;
  stop) cmd_stop ;;
  *) usage ;;
esac
