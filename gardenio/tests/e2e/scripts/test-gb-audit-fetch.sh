#!/usr/bin/env bash
# test-gb-audit-fetch.sh — E2E gate for `gb audit fetch`: the forensic session-log
# download an operator runs to pull an agent's archived transcripts out of S3.
#
# Bead ka-0000004272 (parent ka-0000000958). `gb audit` had ZERO e2e coverage of
# any kind — 0 of the ~190 test-*.sh scripts touched it — so two recently-merged
# behaviors shipped UNGUARDED at the CLI level (merged ab1d5b6a2, branch
# fix/gd-RKoO5RuTgy-audit-fetch-bare-bead-id):
#
#   1. `gb audit fetch <bare-bead-id>` expands to EVERY session archived under
#      that bead, newest-first, and prints a
#      `note: <id> resolved to N session(s): ...` line so the local layout is
#      never a surprise (audit.go RunE :407-427 + resolveFetchTargets :679).
#   2. A fetch that downloads NOTHING now EXITS NON-ZERO instead of exiting 0
#      with an empty directory (audit.go :433, ka-0000002531). This is the
#      headline: the files ARE the output, so a silent empty result reads as
#      "this agent archived nothing" to a forensic operator debugging an
#      incident — the exact wrong conclusion.
#
# THE GAP THIS CLOSES: audit.go has a Go unit test for the PURE RESOLVER
# (audit_test.go TestResolveFetchTargets) but the RunE — the expansion loop, the
# `note:` line, the `sessions` key in --json, and the exit-code guard at :433 —
# is completely untested. That is exactly the half where a regression is
# invisible: `gb audit` is a human-driven CLI with no alerting, so nothing goes
# red if the exit code silently flips back to 0. This module drives the REAL `gb`
# binary end-to-end against a local S3 stub and asserts the RunE contract.
#
# WHAT THIS PROVES (leg 3 is the headline — the zero-coverage exit-code guard):
#   1. BARE-BEAD EXPANSION (hermetic): seed two sessions under one bead
#      (<prefix>/gd-BEAD/uuid-a/... and .../uuid-b/...). `gb audit fetch gd-BEAD
#      --json` exits 0, reports count>=4, lists BOTH sessions in the `sessions`
#      key NEWEST-FIRST, prints the `note: ... resolved to 2 session(s)` line,
#      and both local session dirs land on disk with their transcripts.
#   2. COMPOUND PASSTHROUGH (hermetic): `gb audit fetch gd-BEAD/uuid-a` fetches
#      ONLY uuid-a — a fully-qualified "<bead>/<coop-uuid>" id is NOT re-expanded.
#   3. EMPTY-FETCH NON-ZERO EXIT (hermetic, THE HEADLINE): `gb audit fetch
#      gd-JJGaaGEzew` EXITS NON-ZERO and writes nothing — the guard at
#      audit.go:433 that had zero coverage. If a regression flips this back to
#      exit 0, an operator sees an empty dir and concludes the agent archived
#      nothing; only this assertion catches it.
#
# WHY A LOCAL S3 STUB AND NOT THE CLUSTER'S BUCKET: asserting the resolver's
# newest-first ordering and the empty-fetch guard needs a fixed, isolated
# keyspace we fully control; the shared COOP_S3_BUCKET carries real agents'
# session data under the same coop/sessions/ prefix and would make both the
# ordering and the "nothing under this bead" assertions non-deterministic. The
# stub is ~60 lines of stdlib python3 (no deps), implements exactly the two S3
# operations resolveFetchTargets/fetchSession use (ListObjectsV2 with a prefix,
# emitting <Key>/<LastModified>; and GetObject), binds 127.0.0.1 on an ephemeral
# port, and is torn down on EXIT. It reports each object's REAL mtime as its
# <LastModified>, so the newest-first ordering is exercised for real rather than
# hardcoded. The AWS SDK request/response path and the CLI entrypoint are REAL —
# only the storage backend is local. This mirrors the proven stub in
# test-gb-salvage.sh:189-222 (same keyspace, same two ops).
#
# Prerequisites: python3 and a `gb` binary (GB_BIN, default `gb`). No kubectl, no
# namespace, no beads daemon, no AWS credentials, no network egress — the root
# `gb` PersistentPreRunE builds a lazy daemon client (beadsapi.New never dials),
# and `gb audit fetch` never issues a daemon request, so the whole flow is
# hermetic just like test-gb-salvage.sh.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gb-audit-fetch.sh
#   GB_BIN=/path/to/gb ./gardenio/tests/e2e/scripts/test-gb-audit-fetch.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GB_BIN="${GB_BIN:-gb}"
RUN_ID="${RUN_ID:-$$}"

# Bucket/prefix are the stub's own, never the cluster's. The prefix mirrors the
# production default (audit.go s3Prefix) so the asserted key layout is the real
# one.
E2E_BUCKET="e2e-audit-${RUN_ID}"
E2E_PREFIX="coop/sessions"
BEAD="gd-audite2e${RUN_ID}"

PASS=0
FAIL=0
ERRORS=()
STUB_PID=""
TMPROOT=""

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
have() { command -v "$1" >/dev/null 2>&1; }

assert_verdict() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

# SKIP loud — a missing prerequisite must never be mistaken for a pass, but must
# also never red the suite (this module is wired into every shard).
skip_all() {
  blue "SKIP gb-audit-fetch: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null
  [ -n "$TMPROOT" ] && [ -d "$TMPROOT" ] && rm -rf "$TMPROOT"
  return 0
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb audit fetch bare-bead expansion + empty-fetch guard E2E (ka-0000004272)"
echo " a forensic operator must never get exit 0 + an empty dir for a bead"
echo " that archived nothing"
echo "════════════════════════════════════════════════════"

have python3 || skip_all "python3 not available"
have "$GB_BIN" || skip_all "gb binary not available (set GB_BIN)"

TMPROOT="$(mktemp -d -t gb-audit-e2e-XXXXXX)"

# ── A minimal S3-compatible stub: exactly the 2 ops audit fetch uses. ──
# ListObjectsV2 (resolveFetchTargets + fetchSession discovery) and GetObject
# (downloadFile). Path-style only, which is what s3EndpointOpts forces when
# COOP_S3_ENDPOINT is set. No auth check: the SDK signs every request, we simply
# don't verify — the point is the wire format and the resolve/fetch behavior, not
# IAM. Crucially, each object's <LastModified> is its REAL on-disk mtime, so
# resolveFetchTargets' newest-first sort is exercised against genuine timestamps
# rather than a hardcoded constant.
cat > "${TMPROOT}/s3stub.py" <<'PYEOF'
import os, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs
from xml.sax.saxutils import escape

ROOT = sys.argv[1]

def split_key(path):
    parts = path.lstrip('/').split('/', 1)
    return (parts[0], parts[1]) if len(parts) == 2 else (None, None)

def iso(mtime):
    return time.strftime('%Y-%m-%dT%H:%M:%S.000Z', time.gmtime(mtime))

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass  # keep the test output clean

    def _send(self, code, body=b'', ctype=None):
        self.send_response(code)
        if ctype:
            self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        if query.get('list-type') == ['2']:
            return self._list(parsed, query)
        bucket, key = split_key(parsed.path)
        src = os.path.join(ROOT, bucket, key) if key else None
        if not src or not os.path.isfile(src):
            return self._send(404)
        with open(src, 'rb') as fh:
            self._send(200, fh.read(), 'application/octet-stream')

    def _list(self, parsed, query):
        bucket = parsed.path.lstrip('/').split('/')[0]
        prefix = (query.get('prefix') or [''])[0]
        base = os.path.join(ROOT, bucket)
        keys = []
        for dirpath, _, filenames in os.walk(base):
            for fn in filenames:
                rel = os.path.relpath(os.path.join(dirpath, fn), base).replace(os.sep, '/')
                if rel.startswith(prefix):
                    keys.append(rel)
        out = ['<?xml version="1.0" encoding="UTF-8"?>',
               '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">',
               '<Name>%s</Name><Prefix>%s</Prefix>' % (escape(bucket), escape(prefix)),
               '<KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys>' % len(keys),
               '<IsTruncated>false</IsTruncated>']
        for k in sorted(keys):
            full = os.path.join(base, k)
            out.append('<Contents><Key>%s</Key><Size>%d</Size>'
                       '<LastModified>%s</LastModified>'
                       '<StorageClass>STANDARD</StorageClass></Contents>'
                       % (escape(k), os.path.getsize(full), iso(os.path.getmtime(full))))
        out.append('</ListBucketResult>')
        self._send(200, ''.join(out).encode(), 'application/xml')

server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PYEOF

S3ROOT="${TMPROOT}/s3"
mkdir -p "${S3ROOT}/${E2E_BUCKET}"
STUB_PORT_FILE="${TMPROOT}/stub.port"
python3 "${TMPROOT}/s3stub.py" "$S3ROOT" >"$STUB_PORT_FILE" 2>"${TMPROOT}/stub.log" &
STUB_PID=$!

# Wait for the stub to report its ephemeral port (bounded — never hang a shard).
S3_PORT=""
for _ in $(seq 1 50); do
  S3_PORT="$(head -1 "$STUB_PORT_FILE" 2>/dev/null | tr -dc '0-9')"
  [ -n "$S3_PORT" ] && break
  sleep 0.2
done
if [ -z "$S3_PORT" ]; then
  fail "S3 stub failed to start" "$(head -5 "${TMPROOT}/stub.log" 2>/dev/null)"
  skip_all "could not start the local S3 stub — cannot exercise gb audit fetch"
fi
dim "  S3 stub on 127.0.0.1:${S3_PORT}, bucket ${E2E_BUCKET}"

# audit env. Deliberately explicit and self-contained so nothing leaks in from a
# real agent pod's environment (an inherited COOP_S3_BUCKET would aim the fetch
# at the FLEET's bucket). Static creds + IMDS disabled: newS3Client has a short
# credential-resolution deadline and would otherwise try (and slowly fail) the
# ambient chain. BEADS_* are unset so the root PersistentPreRunE builds its lazy
# client against a harmless default and `gb audit fetch` never dials it.
audit_env() {
  env \
    -u BEADS_HTTP_URL -u BEADS_HTTP_ADDR -u BEADS_TOKEN -u BEADS_AUTH_TOKEN \
    -u BEADS_DAEMON_TOKEN -u GARDENIO_TOKEN -u KBEADS_TOKEN \
    -u AWS_WEB_IDENTITY_TOKEN_FILE -u AWS_ROLE_ARN -u AWS_PROFILE \
    -u AWS_SESSION_TOKEN -u AWS_CONTAINER_CREDENTIALS_FULL_URI \
    -u AWS_CONTAINER_CREDENTIALS_RELATIVE_URI \
    COOP_S3_ENDPOINT="http://127.0.0.1:${S3_PORT}" \
    COOP_S3_BUCKET="$E2E_BUCKET" \
    COOP_S3_PREFIX="$E2E_PREFIX" \
    COOP_S3_REGION="us-east-1" \
    AWS_ACCESS_KEY_ID="e2e" \
    AWS_SECRET_ACCESS_KEY="e2e" \
    AWS_EC2_METADATA_DISABLED="true" \
    "$@"
}

# ── Fixture: two sessions archived under one bead ──
# Layout mirrors coop's bead-namespaced archival (audit.go listSessions):
#   <prefix>/<bead>/<coop-uuid>/meta.json
#   <prefix>/<bead>/<coop-uuid>/transcripts/<n>.jsonl
# uuid-a is written FIRST, then uuid-b with a strictly-later mtime, so the
# resolver's newest-first ordering must place uuid-b ahead of uuid-a — asserted
# below against the stub's real-mtime <LastModified>.
seed_session() {
  local uuid="$1"
  local d="${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${BEAD}/${uuid}"
  mkdir -p "${d}/transcripts"
  printf '{"session":"%s","bead":"%s"}\n' "$uuid" "$BEAD" > "${d}/meta.json"
  printf '{"t":0,"session":"%s"}\n' "$uuid" > "${d}/transcripts/0.jsonl"
  printf '{"t":1,"session":"%s"}\n' "$uuid" > "${d}/transcripts/1.jsonl"
}
seed_session "uuid-a"
# Force uuid-b's objects to be strictly NEWER than uuid-a's so the newest-first
# sort has a deterministic, observable answer (touch, not wall-clock sleep).
seed_session "uuid-b"
find "${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${BEAD}/uuid-b" -type f \
  -exec touch -d '2026-02-01T00:00:00Z' {} +
find "${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${BEAD}/uuid-a" -type f \
  -exec touch -d '2026-01-01T00:00:00Z' {} +

# Small JSON field reader (stdlib python3, no jq dependency).
jget() { python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2],''))" "$1" "$2" 2>/dev/null; }

# ===========================================================================
# LEG 1 — BARE-BEAD EXPANSION: fetch every session under a bead, newest-first.
# ===========================================================================
blue "[expansion] gb audit fetch <bare-bead-id> → every session, newest-first"

DL1="${TMPROOT}/dl1"
L1_OUT="${TMPROOT}/leg1.out"
L1_ERR="${TMPROOT}/leg1.err"
audit_env timeout 60 "$GB_BIN" audit fetch "$BEAD" --output "$DL1" --json \
  >"$L1_OUT" 2>"$L1_ERR"
L1_RC=$?

assert_verdict "bare-bead fetch exits 0" "$L1_RC" "0"

# The RunE prints a note naming how many sessions the bare bead expanded to —
# the "no surprise" contract (audit.go :416). Grep the stderr for it.
if grep -q "note: ${BEAD} resolved to 2 session(s)" "$L1_ERR"; then
  pass "prints the 'resolved to 2 session(s)' expansion note"
else
  fail "expansion note missing/wrong" "$(grep '^note:' "$L1_ERR" || tail -3 "$L1_ERR")"
fi

# --json count: 2 sessions × (meta.json + 2 transcripts) = 6 files. The task's
# floor is count>=4; assert the exact 6 so a silent under-fetch is also caught.
L1_COUNT="$(jget "$L1_OUT" count)"
if [ "${L1_COUNT:-0}" -ge 4 ]; then
  pass "reports count>=4 files fetched (got ${L1_COUNT})"
else
  fail "count too low" "got '${L1_COUNT}', want >=4"
fi
assert_verdict "reports the exact file count (2 meta + 4 transcript)" "${L1_COUNT:-0}" "6"

# The `sessions` key must contain BOTH sessions...
SESS_JSON="$(python3 -c "import json;print(' '.join(json.load(open('$L1_OUT')).get('sessions',[])))" 2>/dev/null)"
if grep -q "${BEAD}/uuid-a" <<<"$SESS_JSON" \
   && grep -q "${BEAD}/uuid-b" <<<"$SESS_JSON"; then
  pass "--json 'sessions' lists BOTH sessions (${SESS_JSON})"
else
  fail "--json 'sessions' missing a session" "got '${SESS_JSON}'"
fi

# ...NEWEST-FIRST: uuid-b (mtime 2026-02) must precede uuid-a (mtime 2026-01).
FIRST_SESS="$(python3 -c "import json;s=json.load(open('$L1_OUT')).get('sessions',[]);print(s[0] if s else '')" 2>/dev/null)"
assert_verdict "newest session (uuid-b) is listed FIRST" "$FIRST_SESS" "${BEAD}/uuid-b"

# Both session dirs must actually exist on disk with their transcripts.
if [ -f "${DL1}/${BEAD}/uuid-a/meta.json" ] && [ -f "${DL1}/${BEAD}/uuid-a/0.jsonl" ]; then
  pass "uuid-a landed on disk (meta.json + transcripts)"
else
  fail "uuid-a not fully on disk" "$(find "${DL1}" -type f 2>/dev/null | head)"
fi
if [ -f "${DL1}/${BEAD}/uuid-b/meta.json" ] && [ -f "${DL1}/${BEAD}/uuid-b/0.jsonl" ]; then
  pass "uuid-b landed on disk (meta.json + transcripts)"
else
  fail "uuid-b not fully on disk" "$(find "${DL1}" -type f 2>/dev/null | head)"
fi
echo

# ===========================================================================
# LEG 2 — COMPOUND PASSTHROUGH: a fully-qualified "<bead>/<coop-uuid>" id is NOT
# re-expanded — it fetches ONLY that one session.
# ===========================================================================
blue "[passthrough] gb audit fetch <bead>/<uuid> → only that session"

DL2="${TMPROOT}/dl2"
L2_OUT="${TMPROOT}/leg2.out"
L2_ERR="${TMPROOT}/leg2.err"
audit_env timeout 60 "$GB_BIN" audit fetch "${BEAD}/uuid-a" --output "$DL2" --json \
  >"$L2_OUT" 2>"$L2_ERR"
L2_RC=$?

assert_verdict "compound-id fetch exits 0" "$L2_RC" "0"

# Only uuid-a — exactly one session, exactly its 3 files.
L2_SESS="$(python3 -c "import json;print(' '.join(json.load(open('$L2_OUT')).get('sessions',[])))" 2>/dev/null)"
assert_verdict "fetches ONLY the requested session" "$L2_SESS" "${BEAD}/uuid-a"
assert_verdict "compound fetch pulls exactly that session's 3 files" "$(jget "$L2_OUT" count)" "3"

# uuid-b must NOT have been fetched by a compound-id request.
if [ -e "${DL2}/${BEAD}/uuid-b" ]; then
  fail "compound id wrongly expanded" "uuid-b appeared under $DL2"
else
  pass "compound id did NOT re-expand (uuid-b absent)"
fi
# A compound id resolving to itself must NOT print an expansion note.
if grep -q "resolved to" "$L2_ERR"; then
  fail "compound id printed a spurious expansion note" "$(grep 'resolved to' "$L2_ERR")"
else
  pass "compound id prints no expansion note (resolves to itself)"
fi
echo

# ===========================================================================
# LEG 3 — EMPTY-FETCH NON-ZERO EXIT (THE HEADLINE, audit.go:433, ka-0000002531).
# The one behavior with zero coverage today: a fetch that downloads NOTHING must
# EXIT NON-ZERO and write nothing — never exit 0 with an empty dir, which a
# forensic operator would read as "this agent archived nothing."
# ===========================================================================
blue "[empty-guard] gb audit fetch <bead-with-no-sessions> → non-zero exit, nothing written"

DL3="${TMPROOT}/dl3"
L3_OUT="${TMPROOT}/leg3.out"
L3_ERR="${TMPROOT}/leg3.err"
if audit_env timeout 60 "$GB_BIN" audit fetch "gd-JJGaaGEzew${RUN_ID}" \
     --output "$DL3" --json >"$L3_OUT" 2>"$L3_ERR"; then
  L3_RC=0
else
  L3_RC=$?
fi

if [ "$L3_RC" -ne 0 ]; then
  pass "empty fetch EXITS NON-ZERO (rc=${L3_RC}) — never a silent exit-0 empty dir"
else
  fail "empty fetch exited 0 (regression: audit.go:433 guard flipped)" \
    "an operator would read the empty dir as 'agent archived nothing'"
fi

# It must say WHY it failed — the guard's diagnostic, not a bare non-zero.
if grep -q "no files downloaded" "$L3_ERR"; then
  pass "empty fetch reports 'no files downloaded' on stderr"
else
  fail "empty fetch gave no 'no files downloaded' diagnostic" "$(tail -3 "$L3_ERR")"
fi

# And it must have written NOTHING — no transcript files under the output dir.
# (The dir itself may be created; the contract is that no session data lands.)
DL3_FILES="$(find "$DL3" -type f 2>/dev/null | wc -l | tr -d ' ')"
assert_verdict "empty fetch writes zero files" "$DL3_FILES" "0"
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " gb-audit-fetch Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gb-audit-fetch assertions passed — bare-bead expansion + empty-fetch guard hold."
exit 0
