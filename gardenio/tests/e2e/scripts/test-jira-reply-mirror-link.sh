#!/usr/bin/env bash
# test-jira-reply-mirror-link.sh — `gb jira reply` → ticket-mirror triggered-by
# link E2E (task ka-0000006808, coverage-backfill epic ka-0000000958).
#
# Closes the gap MR !6704 left open. That MR taught `gb jira reply` to stamp a
# `triggered-by` edge FROM the outbound reply message bead TO the ticket's
# Jira-mirror bead (label `jira:<KEY>`), so the outbound half of a Jira
# conversation is traversable from the ticket node in beads-viewer — the
# outbound analog of the bridge's inbound writeMirrorTriggeredBy. The behavior
# lives in cmd/gb/jira.go: recordJiraReplyBead() → linkJiraReplyToMirror()
# (jira.go:552), which runs CLI-SIDE, AFTER the bridge returns HTTP 200.
#
# ── Why this is a NEW module and not a leg of test-jira-conversational.sh ──────
#
# test-jira-conversational.sh's reply leg drives bridge.NewJiraReplyAPI — the
# BRIDGE handler (internal/bridge/jira_reply.go handlePostReply). That handler
# posts the Jira comment and returns; it does NO mirror-linking (no CreateBead,
# no AddDependency). The mirror edge is written by the `gb` CLI in a DIFFERENT
# package (cmd/gb, package main) once it has the bridge's 200. So the edge is
# structurally unreachable from the conversational probe — extending the probe's
# in-process mock daemon to record AddDependency would record ZERO reply→mirror
# edges, because the code that writes them never runs in the bridge. The genuine
# gate for a CLI-side behavior is the REAL `gb` binary driven end-to-end — the
# exact precedent of test-grafana-render-cli.sh and test-gb-salvage.sh (real
# `gb` binary + Python-stdlib stubs, hermetic, no cluster).
#
# ── Why a hermetic stub rig, not a live-daemon round-trip ─────────────────────
#
#   1. Asserting "EXACTLY ONE edge" needs a daemon whose dependency set starts
#      empty and is observed in full — the shared e2e daemon is neither empty nor
#      cheaply isolable (`kd serve` needs Postgres). A stub records every write.
#   2. `gb jira reply` FIRST POSTs the bridge /api/jira/reply and ONLY on 200
#      calls recordJiraReplyBead → linkJiraReplyToMirror. So the rig needs a stub
#      bridge (200) AND a stub daemon; both are trivially stdlib-stubbable.
#   3. The mirror lookup is a label-AND query (labels=jira:<KEY>) — a stub that
#      serves the mirror for the RIGHT label and empty for a wrong/absent one
#      exercises both the positive link and the negative "no mirror" degrade.
#
# The stub daemon RECORDS every POST /v1/beads (create), GET /v1/beads?labels=…
# (mirror lookup), POST /v1/beads/{id}/dependencies (edge) and POST
# …/{id}/close, and serves back exactly what linkJiraReplyToMirror expects. The
# REAL production code path — recordJiraReplyBead's create→lineage→link→close
# sequence and linkJiraReplyToMirror's label resolution + AddDependency — runs
# unmodified in the compiled `gb`.
#
# ── Lineage isolation (why the edge COUNT can be asserted exactly) ────────────
#
# recordJiraReplyBead also calls writeSlackReplyLineage (slack.go:224 →
# writeReplyLineage), which stamps triggered-by edges to the agent bead
# (BOAT_AGENT_BEAD_ID/KD_AGENT_ID), the trigger bead (BOAT_TRIGGER_BEAD_ID), or a
# human-squawk parent (SLACK_USER_ID / git email → operator alias bead). We
# neutralize ALL of those inputs (empty env + nulled git identity + no operator
# alias bead in the stub) so the ONLY triggered-by edge any reply can produce is
# the mirror edge — making "exactly one edge to the mirror" (positive) and "zero
# edges" (negative) exact, not approximate.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  link        `gb jira reply` for a ticket WITH a synced mirror bead:
#                  the mirror lookup queries labels containing `jira:<KEY>`, and
#                  EXACTLY ONE `triggered-by` edge is written FROM the created
#                  reply message bead TO the `jira:<KEY>`-labelled mirror bead.
#                  The reply message bead is also created (type=message, label
#                  jira-reply) and closed. [MR !6704 headline behavior]
#   2  no-mirror   negative control — `gb jira reply` for a ticket with NO synced
#                  mirror (the label lookup returns empty): the reply STILL
#                  succeeds, the message bead is created AND closed, and ZERO
#                  triggered-by edges are written. Without this leg, leg 1 cannot
#                  distinguish "links to the right mirror" from "links a mirror
#                  edge unconditionally".
#   3  schema      type:message provenance-field assertion (task ka-0000005455 leg
#                  c; the FIFTH instance of the producer-vs-schema drift class,
#                  ka-0000000661). The stub daemon "records only type/labels/title
#                  and never validates fields" — so a schema regression that 400s
#                  jira_key/visibility on the REAL daemon (dropping the provenance
#                  bead silently, because recordJiraReplyBead is best-effort) is
#                  invisible to this module. This leg closes that: it reads back the
#                  reply message bead's ACTUAL create `fields` from the journal and
#                  asserts (a) jira_key + visibility are present and non-empty (the
#                  provenance the bead carries), AND (b) EVERY field the producer
#                  emits is DECLARED in the live type:message schema extracted from
#                  kbeads/internal/server/config.go — so a producer that adds an
#                  undeclared field OR a schema that drops jira_key/visibility reds
#                  the leg (the real daemon would 400 that create). Skips CLEANLY
#                  (loud, never reds) only if config.go is absent from the checkout.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The `gb` binary + python3 are present in every e2e shard container (the shard
# `use:` list includes build-gb; e2e-system-deps.lock ships python3). If either
# is somehow absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an
# absent binary must never red the suite — mirroring the grafana-render-cli /
# gb-salvage skip_all("loud") contract. When present, every leg is a HARD
# assertion. It does NOT use the shard's beads port-forward tunnel at all.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-jira-reply-mirror-link.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/jira-reply-mirror-link.${RUN_ID}.XXXXXX")"
GB_BIN="${GB_BIN:-}"   # honor an explicit override; else resolved below
GB_TEMP=""          # non-empty only when we built a throwaway binary to clean up
PIDS=()

# Synthetic ids the stub daemon mints/serves. The reply bead id is what the stub
# returns from POST /v1/beads; the mirror id is what it serves for the jira:<KEY>
# label lookup. Both are namespaced so a stray real bead can never collide.
REPLY_BEAD="gd-JReplyMirrorE2E"
MIRROR_BEAD="gd-2XuPeMTxGl"
JIRA_KEY_LINK="PE-42042"       # ticket WITH a synced mirror (leg 1)
JIRA_KEY_NOMIRROR="PE-42043"   # ticket with NO synced mirror (leg 2)

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
  red " SKIP jira-reply-mirror-link (LOUD): $why"
  red " This run did NOT assert 'gb jira reply' stamping a triggered-by edge from"
  red " the reply message bead to the ticket's jira:<KEY> mirror bead (MR !6704)."
  red " A green suite here proves NOTHING about that link. The e2e shard is built"
  red " with the gb binary (build-gb task) + python3 (e2e-system-deps.lock) — if"
  red " this fires in CI, that wiring regressed."
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
# back — race-free enough for a single-process rig (we bind the real server
# immediately after) and avoids hard-coded ports that collide across co-located
# shards.
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

echo "════════════════════════════════════════════════════"
echo " gb jira reply → ticket-mirror triggered-by link E2E (ka-0000006808, !6704)"
echo " link · no-mirror (negative control)"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the gb binary ────────────────────────────────────────
# Prefer an explicit GB_BIN override (local dev against a known-fresh binary),
# then a binary pre-built + installed onto PATH. Fall
# back to an in-shell workspace build for local dev. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip.
#
# NOTE: the linkJiraReplyToMirror behavior under test shipped in MR !6704 — a gb
# older than that produces no mirror edge and this module fails loudly (as it
# should). In CI that is never a concern: the build-gb task compiles the CURRENT
# source at pipeline time, so the pre-built gb always carries the behavior. GB_BIN
# is the escape hatch for a laptop whose PATH gb lags the checkout.
if [ -n "${GB_BIN:-}" ] && [ -x "${GB_BIN}" ]; then
  pass "using GB_BIN override ($GB_BIN)"
elif command -v gb >/dev/null 2>&1; then
  GB_BIN="$(command -v gb)"
  pass "using pre-built gb ($GB_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no gb on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gb" ] \
    || skip_all "no gb on PATH and cmd/gb missing under $CONTROLLER_DIR"
  command -v python3 >/dev/null 2>&1 \
    || skip_all "python3 absent — cannot stand up the stub daemon/bridge rig"
  blue "[build] cmd/gb (from source, workspace mode)"
  GB_TEMP="$(mktemp "/tmp/gb.${RUN_ID}.XXXXXX")"
  GB_BIN="$GB_TEMP"
  BUILD_LOG="/tmp/gb-build-${RUN_ID}.log"
  # cmd/gb imports the local clicreds module via go.work (replace => ../clicreds),
  # so it MUST build in workspace mode (GOWORK on). Pin GOWORK to the repo-root
  # go.work explicitly, exactly like test-grafana-render-cli.sh.
  if ( GOWORK="$(pwd)/go.work" CGO_ENABLED=0 go build -o "$GB_BIN" ./gardenio/controller/cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/gb built from source"
  else
    fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (gb build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi

command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot stand up the stub daemon/bridge rig"
command -v curl >/dev/null 2>&1 \
  || skip_all "curl absent — cannot poll the stub rig readiness"
echo

# ── 2. Stand up the stub rig ────────────────────────────────────────
# One Python file, two roles (argv[1]): the beads daemon and the jira-bridge.
#
# DAEMON records every write to a JSONL journal ($JOURNAL) so the shell can
# assert on the exact sequence of creates / dependency edges / closes AFTER each
# `gb` run — the same "record then assert" shape the gitlab-prov-probe mock uses
# in-process, here over HTTP against the real binary. It serves the mirror bead
# for GET /v1/beads only when the labels query contains jira:<MIRROR_KEY>; any
# other label query returns an empty set (the "no mirror synced" case).
STUB_PY="$WORKDIR/stubs.py"
cat > "$STUB_PY" <<'PY'
# Throwaway HTTP stubs for the gb-jira-reply-mirror-link E2E. One file, two
# roles (argv[1]): a beads daemon that RECORDS writes + serves a mirror lookup,
# and a jira-bridge that accepts /api/jira/reply with a 200. argv[3] (daemon
# only) is the label key whose jira:<key> lookup returns the mirror bead; any
# other label query returns empty (the "no mirror synced yet" degrade).
import http.server, json, os, sys, urllib.parse as up

JOURNAL   = os.environ["STUB_JOURNAL"]      # daemon append-only write log (JSONL)
REPLY_ID  = os.environ["STUB_REPLY_ID"]     # id returned from POST /v1/beads
MIRROR_ID = os.environ["STUB_MIRROR_ID"]    # id served for the jira:<key> lookup

def record(entry):
    with open(JOURNAL, "a") as fh:
        fh.write(json.dumps(entry) + "\n")

class Daemon(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def _empty(self, code=200):
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        u = up.urlparse(self.path)
        if u.path == "/v1/beads":
            q = up.parse_qs(u.query)
            labels = q.get("labels", [""])[0]
            record({"op": "list", "labels": labels})
            # The mirror is served ONLY for the label key this daemon was armed
            # with (leg 1). A wrong/absent key (leg 2) returns an empty set, the
            # exact "poller not synced" shape linkJiraReplyToMirror degrades on.
            if MIRROR_KEY and ("jira:" + MIRROR_KEY) in labels:
                return self._json(200, {"beads": [{"id": MIRROR_ID, "type": "task",
                                                   "status": "open",
                                                   "title": MIRROR_KEY + " mirror"}],
                                        "total": 1})
            return self._json(200, {"beads": [], "total": 0})
        # Any bead GET (lineage/alias probes) → 404 so nothing links off it.
        return self._json(404, {"message": "not found"})

    def do_POST(self):
        u = up.urlparse(self.path)
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw.decode()) if raw else {}
        except Exception:
            body = {}
        path = u.path
        if path == "/v1/beads":
            record({"op": "create", "type": body.get("type"),
                    "labels": body.get("labels"), "title": body.get("title"),
                    "fields": body.get("fields")})
            # Echo the persisted bead so CreateBeadDetail's toDetail() is happy.
            return self._json(200, {"id": REPLY_ID, "type": body.get("type", "message"),
                                    "status": "open", "title": body.get("title", ""),
                                    "labels": body.get("labels", [])})
        if path.startswith("/v1/beads/") and path.endswith("/dependencies"):
            bead = path[len("/v1/beads/"):-len("/dependencies")]
            record({"op": "dep", "bead": up.unquote(bead),
                    "depends_on_id": body.get("depends_on_id"),
                    "type": body.get("type"), "created_by": body.get("created_by")})
            return self._empty(200)
        if path.startswith("/v1/beads/") and path.endswith("/close"):
            bead = path[len("/v1/beads/"):-len("/close")]
            record({"op": "close", "bead": up.unquote(bead)})
            return self._empty(200)
        # Any other mutating call (labels/fields/comments) — accept, don't record.
        return self._empty(200)

class Bridge(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        u = up.urlparse(self.path)
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw.decode()) if raw else {}
        except Exception:
            body = {}
        if u.path == "/api/jira/reply":
            key = body.get("jira_key", "")
            resp = json.dumps({"ok": True, "jira_key": key}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(resp)))
            self.end_headers()
            self.wfile.write(resp)
            return
        self.send_response(404); self.send_header("Content-Length", "0"); self.end_headers()

ROLES = {"daemon": Daemon, "bridge": Bridge}
role = sys.argv[1]
port = int(sys.argv[2])
# MIRROR_KEY is the label key the daemon serves a mirror for (daemon role only).
MIRROR_KEY = sys.argv[3] if len(sys.argv) > 3 else ""
http.server.HTTPServer(("127.0.0.1", port), ROLES[role]).serve_forever()
PY

JOURNAL="$WORKDIR/daemon.jsonl"
: > "$JOURNAL"

DAEMON_PORT="$(free_port)"
BRIDGE_PORT="$(free_port)"

# The daemon is armed to serve a mirror ONLY for JIRA_KEY_LINK. Leg 2 uses a
# DIFFERENT key, so the same daemon returns empty for it — no second process.
STUB_JOURNAL="$JOURNAL" STUB_REPLY_ID="$REPLY_BEAD" STUB_MIRROR_ID="$MIRROR_BEAD" \
  python3 "$STUB_PY" daemon "$DAEMON_PORT" "$JIRA_KEY_LINK" & PIDS+=($!)
STUB_JOURNAL="$JOURNAL" STUB_REPLY_ID="$REPLY_BEAD" STUB_MIRROR_ID="$MIRROR_BEAD" \
  python3 "$STUB_PY" bridge "$BRIDGE_PORT" & PIDS+=($!)

DAEMON_URL="http://127.0.0.1:${DAEMON_PORT}"
BRIDGE_URL="http://127.0.0.1:${BRIDGE_PORT}"

rig_ok=1
wait_http "${DAEMON_URL}/v1/beads/probe" || rig_ok=0
wait_http "${BRIDGE_URL}/probe"          || rig_ok=0
if [ "$rig_ok" != 1 ]; then
  skip_all "stub daemon/bridge rig failed to come up (python3 http.server unavailable?)"
fi
pass "stub rig up (daemon:$DAEMON_PORT bridge:$BRIDGE_PORT)"
echo

# ── 3. Neutralize every lineage input so the mirror edge is the ONLY edge ──────
# writeReplyLineage (behind recordJiraReplyBead) stamps triggered-by edges to the
# agent bead / trigger bead / human-squawk-parent when those env inputs are set.
# Clearing them all — plus nulling the git identity so gitUserEmail() returns
# nothing and no operator-alias lookup can match — leaves the ticket mirror as
# the sole possible triggered-by target, so the edge COUNT is exact.
export BEADS_HTTP_ADDR="$DAEMON_URL"     # gb daemon target (defaultHTTPURL reads this)
export JIRA_BRIDGE_URL="$BRIDGE_URL"     # gb jira reply bridge target
export BEADS_OPERATED_BY="oidc:jira-reply-mirror-e2e@example.com"  # daemon attribution
export HOME="$WORKDIR"                   # isolate any ~/.gitconfig / credstore
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
unset BOAT_AGENT_BEAD_ID KD_AGENT_ID BOAT_TRIGGER_BEAD_ID 2>/dev/null || true
unset SLACK_USER_ID SLACK_THREAD_CHANNEL SLACK_THREAD_TS 2>/dev/null || true
unset GARDENIO_TOKEN KBEADS_TOKEN BEADS_AUTH_TOKEN BEADS_DAEMON_TOKEN 2>/dev/null || true
unset BEADS_HTTP_URL 2>/dev/null || true   # let BEADS_HTTP_ADDR win, unambiguously

# jq-free JSONL projections over the daemon journal (python3 is guaranteed
# present; jq is not). Each prints a count or list to stdout.
#
# count_op <op>                    → number of journal rows with that op
# count_dep_to <from> <to> <type>  → number of triggered-by edges from→to
# list_labels_for op=list          → the labels string of every list row
count_op() {
  python3 - "$JOURNAL" "$1" <<'PY'
import json, sys
j, op = sys.argv[1], sys.argv[2]
n = 0
for line in open(j):
    line = line.strip()
    if not line: continue
    if json.loads(line).get("op") == op: n += 1
print(n)
PY
}
count_dep() {
  # count_dep <from_bead> <to_bead> <dep_type>
  python3 - "$JOURNAL" "$1" "$2" "$3" <<'PY'
import json, sys
j, frm, to, typ = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
n = 0
for line in open(j):
    line = line.strip()
    if not line: continue
    e = json.loads(line)
    if e.get("op") == "dep" and e.get("bead") == frm \
       and e.get("depends_on_id") == to and e.get("type") == typ:
        n += 1
print(n)
PY
}
count_all_deps() {
  count_op dep
}
list_query_labels() {
  python3 - "$JOURNAL" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line: continue
    e = json.loads(line)
    if e.get("op") == "list":
        print(e.get("labels", ""))
PY
}
close_count_for() {
  python3 - "$JOURNAL" "$1" <<'PY'
import json, sys
j, bead = sys.argv[1], sys.argv[2]
n = 0
for line in open(j):
    line = line.strip()
    if not line: continue
    e = json.loads(line)
    if e.get("op") == "close" and e.get("bead") == bead: n += 1
print(n)
PY
}
# message_create_fields prints the space-separated KEYS of the fields blob on the
# FIRST type=message create in the journal — the exact producer field set
# recordJiraReplyBead marshalled. Empty output means no message create was seen
# (or it carried no fields), which the leg treats as a hard failure.
message_create_fields() {
  python3 - "$JOURNAL" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line: continue
    e = json.loads(line)
    if e.get("op") == "create" and e.get("type") == "message":
        f = e.get("fields") or {}
        if isinstance(f, dict):
            print(" ".join(sorted(f.keys())))
        break
PY
}
# message_field_value prints the VALUE of one field on the first type=message
# create (empty if absent) — used to assert jira_key/visibility are non-empty.
message_field_value() {
  python3 - "$JOURNAL" "$1" <<'PY'
import json, sys
j, key = sys.argv[1], sys.argv[2]
for line in open(j):
    line = line.strip()
    if not line: continue
    e = json.loads(line)
    if e.get("op") == "create" and e.get("type") == "message":
        f = e.get("fields") or {}
        if isinstance(f, dict):
            print(f.get(key, "") or "")
        break
PY
}
# schema_message_fields prints the space-separated field names DECLARED in the
# live type:message builtin schema, parsed straight out of config.go — so the
# assertion is coupled to the REAL schema, not a hand-copied list (a schema edit
# that drops jira_key/visibility changes this set and reds the leg). Prints
# nothing if the block cannot be located (the caller then skips loud).
schema_message_fields() {
  python3 - "$1" <<'PY'
import re, json, sys
try:
    src = open(sys.argv[1]).read()
except OSError:
    sys.exit(0)
m = re.search(r'"type:message":\s*\{Key:\s*"type:message",\s*Value:\s*json\.RawMessage\(`(.*?)`\)\}', src, re.S)
if not m:
    sys.exit(0)
try:
    schema = json.loads(m.group(1))
except Exception:
    sys.exit(0)
print(" ".join(f["name"] for f in schema.get("fields", []) if "name" in f))
PY
}

# ── Leg 1: link — reply to a ticket WITH a synced mirror ───────────────────────
blue "[leg link] gb jira reply (mirror present) → one triggered-by edge to the mirror"
: > "$JOURNAL"
if JIRA_KEY="$JIRA_KEY_LINK" "$GB_BIN" jira reply "On it — investigating now." \
     >"$WORKDIR/link.out" 2>"$WORKDIR/link.err"; then
  created="$(count_op create)"
  closed="$(close_count_for "$REPLY_BEAD")"
  mirror_edges="$(count_dep "$REPLY_BEAD" "$MIRROR_BEAD" triggered-by)"
  total_edges="$(count_all_deps)"
  labels_seen="$(list_query_labels | tr '\n' ' ')"
  if [ "$created" -lt 1 ]; then
    fail "leg link create" "no reply message bead created (want >=1, got $created)"
  elif ! grep -q "jira:${JIRA_KEY_LINK}" <<<"$labels_seen"; then
    fail "leg link lookup" "mirror lookup did not query labels jira:${JIRA_KEY_LINK} (saw: ${labels_seen:-none})"
  elif [ "$mirror_edges" != "1" ]; then
    fail "leg link edge" "want exactly 1 triggered-by ${REPLY_BEAD}->${MIRROR_BEAD}, got $mirror_edges (all deps: $total_edges)"
  elif [ "$total_edges" != "1" ]; then
    fail "leg link edge-isolation" "want exactly 1 triggered-by edge total (only the mirror), got $total_edges — a lineage edge leaked"
  elif [ "$closed" -lt 1 ]; then
    fail "leg link close" "reply message bead $REPLY_BEAD not closed (want >=1, got $closed)"
  else
    pass "leg link — mirror looked up by jira:${JIRA_KEY_LINK}; exactly 1 triggered-by ${REPLY_BEAD}->${MIRROR_BEAD}; bead created+closed"
  fi
else
  fail "leg link" "gb jira reply exited non-zero: $(tail -3 "$WORKDIR/link.err" 2>/dev/null)"
fi
echo

# ── Leg 2: no-mirror — negative control (reply to a ticket with NO mirror) ──────
blue "[leg no-mirror] gb jira reply (no mirror synced) → reply succeeds, ZERO edges"
: > "$JOURNAL"
if JIRA_KEY="$JIRA_KEY_NOMIRROR" "$GB_BIN" jira reply "Following up." \
     >"$WORKDIR/nomirror.out" 2>"$WORKDIR/nomirror.err"; then
  created="$(count_op create)"
  closed="$(close_count_for "$REPLY_BEAD")"
  total_edges="$(count_all_deps)"
  labels_seen="$(list_query_labels | tr '\n' ' ')"
  if [ "$created" -lt 1 ]; then
    fail "leg no-mirror create" "reply message bead not created even with no mirror (want >=1, got $created)"
  elif ! grep -q "jira:${JIRA_KEY_NOMIRROR}" <<<"$labels_seen"; then
    fail "leg no-mirror lookup" "mirror lookup did not query labels jira:${JIRA_KEY_NOMIRROR} (saw: ${labels_seen:-none})"
  elif [ "$total_edges" != "0" ]; then
    fail "leg no-mirror edge" "want ZERO triggered-by edges when no mirror exists, got $total_edges (link is unconditional?)"
  elif [ "$closed" -lt 1 ]; then
    fail "leg no-mirror close" "reply message bead $REPLY_BEAD not closed (want >=1, got $closed)"
  else
    pass "leg no-mirror — reply succeeded, bead created+closed, ZERO triggered-by edges (degrades cleanly)"
  fi
else
  fail "leg no-mirror" "gb jira reply exited non-zero: $(tail -3 "$WORKDIR/nomirror.err" 2>/dev/null)"
fi
echo

# ── Leg 3: schema — type:message provenance-field assertion ────────────────────
# The stub daemon accepts any fields (it "never validates"), so on its own this
# module cannot catch the drift class ka-0000000661 is the fifth instance of: a
# producer marshalling jira_key/visibility that the REAL type:message schema does
# not declare, which 400s the best-effort create and silently drops the
# provenance bead. We close that here by comparing the producer's ACTUAL emitted
# field set (read back from the journal) against the LIVE schema in config.go.
blue "[leg schema] type:message provenance fields declared in the live schema (ka-0000005455 leg c)"
# Resolve config.go relative to this repo checkout. The e2e shard checks out the
# monorepo, so kbeads/internal/server/config.go is present; a stray layout skips.
CONFIG_GO="${CONFIG_GO:-kbeads/internal/server/config.go}"
: > "$JOURNAL"
if JIRA_KEY="$JIRA_KEY_LINK" "$GB_BIN" jira reply "Provenance check — schema fields." \
     >"$WORKDIR/schema.out" 2>"$WORKDIR/schema.err"; then
  emitted="$(message_create_fields)"
  jira_key_val="$(message_field_value jira_key)"
  vis_val="$(message_field_value visibility)"
  declared="$(schema_message_fields "$CONFIG_GO")"
  if [ -z "$emitted" ]; then
    fail "leg schema emitted" "no type=message create with fields recorded — recordJiraReplyBead did not run (out: $(tail -2 "$WORKDIR/schema.out" 2>/dev/null))"
  elif [ -z "$jira_key_val" ] || [ "$jira_key_val" != "$JIRA_KEY_LINK" ]; then
    fail "leg schema jira_key" "message bead jira_key=\"$jira_key_val\", want \"$JIRA_KEY_LINK\" (the provenance binding)"
  elif [ -z "$vis_val" ]; then
    fail "leg schema visibility" "message bead visibility is empty — the provenance bead lost its visibility field"
  elif [ -z "$declared" ]; then
    # config.go absent/unparseable → skip THIS leg loud, do not red the module.
    red "  ⚠ leg schema SKIPPED (loud): could not read type:message schema from $CONFIG_GO —"
    red "    the producer-vs-schema drift assertion did NOT run this cycle."
  else
    # Every field the producer emits MUST be declared, or the real daemon 400s the
    # create ("<key>: unknown field") and drops the provenance bead. This is the
    # exact end-to-end coupling the stub's permissiveness otherwise hides.
    undeclared=""
    for f in $emitted; do
      case " $declared " in
        *" $f "*) : ;;                 # declared — ok
        *)        undeclared="$undeclared $f" ;;
      esac
    done
    # Cross-check the two provenance fields are actually IN the declared set, so a
    # schema that dropped jira_key/visibility reds even though the producer emits
    # them (the real regression ka-0000000661 was).
    prov_missing=""
    for f in jira_key visibility; do
      case " $declared " in
        *" $f "*) : ;;
        *)        prov_missing="$prov_missing $f" ;;
      esac
    done
    if [ -n "$undeclared" ]; then
      fail "leg schema declared" "producer emits field(s)$undeclared not declared in type:message schema — the real daemon 400s this create and drops the provenance bead (emitted: $emitted)"
    elif [ -n "$prov_missing" ]; then
      fail "leg schema provenance" "type:message schema is missing provenance field(s)$prov_missing — a successful gb jira reply would 400 the follow-up create (ka-0000000661)"
    else
      pass "leg schema — message bead carries jira_key=$JIRA_KEY_LINK + visibility=$vis_val; all emitted fields ($emitted) declared in the live type:message schema"
    fi
  fi
else
  fail "leg schema" "gb jira reply exited non-zero: $(tail -3 "$WORKDIR/schema.err" 2>/dev/null)"
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
