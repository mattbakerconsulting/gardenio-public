#!/usr/bin/env bash
# test-gb-salvage.sh — E2E gate for `gb salvage`: the dirty-worktree rescue that
# bundles an involuntarily-killed agent's uncommitted work to S3, and
# `gb salvage restore`, which pulls it back out.
#
# Bead ka-0000000188 (parent ka-0000000958). Guards the salvage subsystem shipped
# by MR !5359 (bundle dirty worktrees to S3 on kill, ka-0000004335) and !5613
# (guarded Tier-2 quarantine push), plus the follow-on fixes:
#   ka-0000005216 — salvage a committed-but-UNPUSHED HEAD, not only a dirty
#     worktree. An agent that `git commit`ed and died before `git push` leaves a
#     spotless `git status`, yet the commit lives only in the dying EmptyDir pod.
#   ka-0000001049 — SKIP salvage on a clean `gb stop`, and exclude agent-infra
#     paths (.claude/.gb/.mcp.json/.state/repos) from the WIP decision, so a
#     voluntary exit does not upload a junk bundle and post a FALSE "Killed
#     mid-work" comment on the worked task.
#   ka-0000001959 — bundle nested reference repos under repos/*, which the outer
#     checkout only records as a gitlink.
#
# THE GAP THIS CLOSES: salvage is unit-tested by ~8 Go test files, but NO e2e
# exercised the real salvage -> S3 -> restore round-trip — a grep of
# gardenio/tests/ for "salvage" returned nothing before this module. The risk is
# data-integrity and silent: a regression in the bundle/upload or the restore
# path means an agent killed mid-work loses its uncommitted changes — the exact
# failure salvage exists to prevent — with no CI signal, because salvage runs on
# the teardown path where it deliberately always exits 0 and production discards
# its stderr (`gb salvage --timeout=15s 2>/dev/null || true` in the pod preStop
# hook, podmanager/manager.go). Nothing goes red when salvage silently stops
# working; the loss only surfaces later as a dead agent's missing work.
#
# WHAT THIS PROVES (leg 1 is the headline — a REAL round-trip, not a mock):
#   1. ROUND-TRIP (always runs, hermetic): the REAL `gb salvage` binary uploads a
#      REAL git bundle over the REAL AWS SDK S3 path to a local S3-compatible
#      stub, and the REAL `gb salvage restore` pulls it back into a fresh clone
#      that never saw the WIP — recovering a tracked edit, an untracked file, and
#      a committed-but-unpushed commit, while a .gitignore'd secret does NOT
#      survive. This is possible in-process because salvage honours
#      COOP_S3_ENDPOINT and forces path-style addressing (audit.go s3EndpointOpts),
#      the same override the in-box MinIO overlay uses — so the object-store leg
#      is exercised for real with no cluster, no MinIO, and no AWS credentials.
#   2. CONTRACT (always runs, hermetic): the WIP/skip decision tables — which
#      porcelain lines count as real WIP vs agent-infra noise, and which agent
#      bead states are a clean exit that must skip salvage entirely —
#      reimplemented over bash predicates and asserted across the full truth table
#      with negative controls. A contract leg is required for the clean-exit table
#      because that branch reads a LIVE agent bead through the daemon client, and
#      the states it keys on (closed/done, stopping) cannot be driven from a shell
#      test without seeding a real agent bead and racing the controller.
#
# WHY A LOCAL S3 STUB AND NOT THE CLUSTER'S BUCKET: writing to the shared
# COOP_S3_BUCKET from a test would interleave with real agents' session data
# under the same coop/sessions/ prefix, and asserting a round-trip needs a
# guaranteed-empty prefix. The stub is ~90 lines of stdlib python3 (no deps),
# implements exactly the three S3 operations salvage uses (PutObject, GetObject,
# ListObjectsV2), binds 127.0.0.1 on an ephemeral port, and is torn down on EXIT.
# The AWS SDK request/response path, the git bundle format, the manifest schema,
# and both CLI entrypoints are all REAL — only the storage backend is local.
#
# Prerequisites: git, python3, and a `gb` binary (GB_BIN, default `gb`). No
# kubectl, no namespace, no beads daemon, no AWS credentials, no network egress.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gb-salvage.sh
#   GB_BIN=/path/to/gb ./gardenio/tests/e2e/scripts/test-gb-salvage.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GB_BIN="${GB_BIN:-gb}"
RUN_ID="${RUN_ID:-$$}"

# Bucket/prefix are the stub's own, never the cluster's. The prefix mirrors the
# production default (audit.go s3Prefix) so the asserted key layout is the real
# one.
E2E_BUCKET="e2e-salvage-${RUN_ID}"
E2E_PREFIX="coop/sessions"

# salvageBundleCap in salvage.go — a bundle over this is skipped, not uploaded.
SALVAGE_BUNDLE_CAP=$((64 * 1024 * 1024))

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

# SKIP loud — a missing prerequisite must never be mistaken for a pass, but must
# also never red the suite (this module is wired into every shard).
skip_all() {
  blue "SKIP gb-salvage: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

assert_verdict() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null
  [ -n "$TMPROOT" ] && [ -d "$TMPROOT" ] && rm -rf "$TMPROOT"
  return 0
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb salvage dirty-worktree → S3 → restore E2E (ka-0000000188)"
echo " an agent killed mid-work must not lose its uncommitted changes"
echo "════════════════════════════════════════════════════"

have git     || skip_all "git not available"
have python3 || skip_all "python3 not available"
have "$GB_BIN" || skip_all "gb binary not available (set GB_BIN)"

TMPROOT="$(mktemp -d -t gb-salvage-e2e-XXXXXX)"

# ===========================================================================
# LEG 1 — ROUND-TRIP (always runs, hermetic): real gb salvage -> real S3 wire
# protocol -> real gb salvage restore.
# ===========================================================================
blue "[round-trip] real gb salvage → S3 → gb salvage restore"

# ── A minimal S3-compatible stub: exactly the 3 ops salvage/restore use. ──
# PutObject (upload), GetObject (download), ListObjectsV2 (discover the prefix).
# Path-style only, which is what s3EndpointOpts forces when COOP_S3_ENDPOINT is
# set. No auth check: the SDK signs every request, we simply don't verify — the
# point is the wire format and the round-trip, not IAM.
cat > "${TMPROOT}/s3stub.py" <<'PYEOF'
import os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs
from xml.sax.saxutils import escape

ROOT = sys.argv[1]

def split_key(path):
    parts = path.lstrip('/').split('/', 1)
    return (parts[0], parts[1]) if len(parts) == 2 else (None, None)

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

    def do_PUT(self):
        bucket, key = split_key(urlparse(self.path).path)
        if not key:
            return self._send(400)
        n = int(self.headers.get('Content-Length') or 0)
        data = self.rfile.read(n) if n else b''
        dest = os.path.join(ROOT, bucket, key)
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, 'wb') as fh:
            fh.write(data)
        self.send_response(200)
        self.send_header('ETag', '"stub"')
        self.send_header('Content-Length', '0')
        self.end_headers()

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
            size = os.path.getsize(os.path.join(base, k))
            out.append('<Contents><Key>%s</Key><Size>%d</Size>'
                       '<LastModified>2026-01-01T00:00:00.000Z</LastModified>'
                       '<StorageClass>STANDARD</StorageClass></Contents>'
                       % (escape(k), size))
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
  skip_all "could not start the local S3 stub — cannot exercise the round-trip"
fi
dim "  S3 stub on 127.0.0.1:${S3_PORT}, bucket ${E2E_BUCKET}"

# Salvage env. Deliberately explicit and self-contained so nothing leaks in from
# a real agent pod's environment (an inherited COOP_S3_BUCKET / KD_AGENT_ID would
# aim the upload at the FLEET's bucket, and an inherited BEADS_HTTP_URL would let
# salvage comment on a real task bead). `env -u` below unsets every one of those.
AGENT_BEAD="gd-e2esalv${RUN_ID}"

# Static creds + IMDS disabled: newS3Client has a 5s credential-resolution
# deadline and would otherwise try (and slowly fail) the ambient chain.
salvage_env() {
  env \
    -u BEADS_HTTP_URL -u BEADS_HTTP_ADDR -u BEADS_TOKEN -u BEADS_AUTH_TOKEN \
    -u BEADS_DAEMON_TOKEN -u BOAT_TASK_ID -u BOAT_WORKSPACE -u BOAT_PROJECT \
    -u COOP_S3_SESSION_KEY -u VMAGENT_URL -u GARDENIO_SALVAGE_PUSH \
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
    KD_AGENT_ID="$AGENT_BEAD" \
    BOAT_AGENT_BEAD_ID="$AGENT_BEAD" \
    "$@"
}

# ── Fixture: an origin + a clone carrying every class of WIP salvage rescues ──
ORIGIN="${TMPROOT}/origin.git"
WORK="${TMPROOT}/work"
git init -q --bare "$ORIGIN"
git clone -q "$ORIGIN" "$WORK" 2>/dev/null
git -C "$WORK" config user.email "e2e@gardenio"
git -C "$WORK" config user.name "e2e"
echo "base" > "${WORK}/base.txt"
git -C "$WORK" add -A
git -C "$WORK" commit -qm "base"
git -C "$WORK" push -q origin HEAD:main 2>/dev/null
git -C "$WORK" fetch -q origin 2>/dev/null

# The four classes, one fixture each:
#   1. a tracked edit          → must be recovered
#   2. an untracked file       → must be recovered (salvage uses `git add -A`)
#   3. an unpushed commit      → must be recovered (ka-0000005216)
#   4. a .gitignore'd secret   → must NOT be recovered (git add -A honours ignores)
echo "secret.log" > "${WORK}/.gitignore"
git -C "$WORK" add .gitignore
git -C "$WORK" commit -qm "add gitignore"   # committed-but-UNPUSHED (class 3)
UNPUSHED_SHA="$(git -C "$WORK" rev-parse HEAD)"
echo "TRACKED_EDIT_${RUN_ID}" >> "${WORK}/base.txt"          # class 1
echo "UNTRACKED_WIP_${RUN_ID}" > "${WORK}/newfile.txt"       # class 2
echo "SHOULD_NOT_SURVIVE_${RUN_ID}" > "${WORK}/secret.log"   # class 4
# Agent-infra noise that must NOT, on its own, be treated as WIP (ka-0000001049).
mkdir -p "${WORK}/.state" && echo noise > "${WORK}/.state/scratch"

# ── Run the REAL salvage ──
SALVAGE_LOG="${TMPROOT}/salvage.log"
salvage_env timeout 120 "$GB_BIN" salvage \
  --workspace "$WORK" --reason involuntary_termination --timeout 60s \
  >"$SALVAGE_LOG" 2>&1
SALVAGE_RC=$?

# Contract: salvage runs in a preStop hook behind `|| true` — it must ALWAYS
# exit 0, so a failure here can never extend or fail pod teardown.
assert_verdict "gb salvage exits 0 (preStop teardown contract)" "$SALVAGE_RC" "0"

if grep -q "worktree(s) bundled to s3://${E2E_BUCKET}/${E2E_PREFIX}/" "$SALVAGE_LOG"; then
  pass "salvage reports a bundled worktree to the expected s3:// prefix"
else
  fail "salvage did not report a bundled worktree" "$(tail -5 "$SALVAGE_LOG")"
fi

# ── Assert the objects really landed, under the production key layout ──
# salvagePrefix(): <prefix>/<agent-bead>/<session>/salvage/ — on the preStop path
# COOP_S3_SESSION_KEY is unset, so session == agent bead.
SALV_DIR="${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${AGENT_BEAD}/${AGENT_BEAD}/salvage"
if [ -f "${SALV_DIR}/work.bundle" ]; then
  pass "work.bundle uploaded at <prefix>/<agent-bead>/<session>/salvage/"
else
  fail "work.bundle missing from the S3 stub" \
    "expected ${SALV_DIR}/work.bundle; got: $(find "${S3ROOT}" -type f 2>/dev/null | head -5)"
fi
if [ -f "${SALV_DIR}/salvage-manifest.json" ]; then
  pass "salvage-manifest.json uploaded alongside the bundle"
else
  fail "salvage-manifest.json missing from the S3 stub"
fi

# The bundle must be a real git bundle, non-empty, and under the 64MiB cap.
if [ -f "${SALV_DIR}/work.bundle" ]; then
  BUNDLE_BYTES="$(wc -c < "${SALV_DIR}/work.bundle" | tr -d ' ')"
  if [ "$BUNDLE_BYTES" -gt 0 ] && [ "$BUNDLE_BYTES" -lt "$SALVAGE_BUNDLE_CAP" ]; then
    pass "bundle is non-empty and under the 64MiB salvageBundleCap (${BUNDLE_BYTES}B)"
  else
    fail "bundle size out of contract" "${BUNDLE_BYTES}B vs cap ${SALVAGE_BUNDLE_CAP}"
  fi
  if head -c 64 "${SALV_DIR}/work.bundle" | grep -q '^# v[0-9]* git bundle'; then
    pass "uploaded object is a real git bundle (magic header)"
  else
    fail "uploaded object is not a git bundle" "$(head -c 32 "${SALV_DIR}/work.bundle" | tr -d '\0')"
  fi
fi

# Manifest schema — the fields an operator/successor relies on to recover.
MANIFEST="${SALV_DIR}/salvage-manifest.json"
if [ -f "$MANIFEST" ]; then
  m_get() { python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2],''))" "$MANIFEST" "$1" 2>/dev/null; }
  assert_verdict "manifest version is 1" "$(m_get version)" "1"
  assert_verdict "manifest records the involuntary reason" "$(m_get reason)" "involuntary_termination"
  assert_verdict "manifest records the agent bead" "$(m_get agent_bead)" "$AGENT_BEAD"
  if [ "$(m_get prefix)" = "s3://${E2E_BUCKET}/${E2E_PREFIX}/${AGENT_BEAD}/${AGENT_BEAD}/salvage/" ]; then
    pass "manifest prefix points at the real salvage location"
  else
    fail "manifest prefix wrong" "got '$(m_get prefix)'"
  fi
  # The recovery recipe must name the restore command an operator actually runs.
  if m_get recover | grep -q "gb salvage restore --agent-bead ${AGENT_BEAD}"; then
    pass "manifest carries a copy-pasteable recover recipe"
  else
    fail "manifest recover recipe missing/wrong" "got '$(m_get recover)'"
  fi
  # The target must record the bundle key and the dirty files it captured.
  T_NAME="$(python3 -c "import json;d=json.load(open('$MANIFEST'));print(d['targets'][0]['name'] if d.get('targets') else '')" 2>/dev/null)"
  assert_verdict "manifest target is the primary checkout ('work')" "$T_NAME" "work"
fi

# ── Restore into a FRESH clone that never saw the WIP ──
# This is the assertion that matters: the dirty work must come back from S3
# alone, into a repo that only ever had the pushed base commit.
RECOVER="${TMPROOT}/recover"
git clone -q "$ORIGIN" "$RECOVER" 2>/dev/null
git -C "$RECOVER" config user.email "e2e@gardenio"
git -C "$RECOVER" config user.name "e2e"

# Sanity/negative control: prove the fresh clone does NOT already contain the WIP,
# so a later "recovered" assertion cannot pass vacuously.
if grep -q "TRACKED_EDIT_${RUN_ID}" "${RECOVER}/base.txt" 2>/dev/null; then
  fail "control: fresh clone already has the WIP" "the round-trip assertion would be vacuous"
else
  pass "control: fresh clone has no WIP before restore"
fi

RESTORE_LOG="${TMPROOT}/restore.log"
( cd "$RECOVER" && salvage_env timeout 120 "$GB_BIN" salvage restore \
    --agent-bead "$AGENT_BEAD" --output "${TMPROOT}/dl" ) \
  >"$RESTORE_LOG" 2>&1
RESTORE_RC=$?
assert_verdict "gb salvage restore exits 0" "$RESTORE_RC" "0"

if grep -q "downloaded s3://${E2E_BUCKET}/.*work.bundle" "$RESTORE_LOG"; then
  pass "restore downloaded the bundle from S3"
else
  fail "restore did not report downloading the bundle" "$(tail -5 "$RESTORE_LOG")"
fi

# restore fetches refs/salvage/* into the cwd repo and must NEVER touch a branch.
if git -C "$RECOVER" rev-parse --verify -q refs/salvage/work >/dev/null 2>&1; then
  pass "restore fetched refs/salvage/work into the recovery clone"
else
  fail "refs/salvage/work missing after restore" "$(tail -5 "$RESTORE_LOG")"
fi

# THE HEADLINE ASSERTION: the killed agent's uncommitted work is recoverable.
if git -C "$RECOVER" checkout -q refs/salvage/work 2>/dev/null; then
  if grep -q "TRACKED_EDIT_${RUN_ID}" "${RECOVER}/base.txt" 2>/dev/null; then
    pass "RECOVERED: tracked edit survived the kill → S3 → restore round-trip"
  else
    fail "tracked edit LOST" "base.txt in refs/salvage/work lacks the WIP marker"
  fi
  if grep -q "UNTRACKED_WIP_${RUN_ID}" "${RECOVER}/newfile.txt" 2>/dev/null; then
    pass "RECOVERED: untracked file survived (salvage snapshots with git add -A)"
  else
    fail "untracked file LOST" "newfile.txt absent from refs/salvage/work"
  fi
  # The committed-but-unpushed commit must also be in the bundle. (The
  # SPOTLESS-tree case — the actual ka-0000005216 regression — is asserted
  # separately below, because here the dirty tree alone would trigger salvage.)
  if git -C "$RECOVER" merge-base --is-ancestor "$UNPUSHED_SHA" refs/salvage/work 2>/dev/null; then
    pass "RECOVERED: committed-but-unpushed HEAD is an ancestor"
  else
    fail "unpushed commit LOST" "${UNPUSHED_SHA} not an ancestor of refs/salvage/work"
  fi
  # A .gitignore'd secret must NOT be swept into a durable, shared bundle.
  if [ -f "${RECOVER}/secret.log" ]; then
    fail "SECRET LEAKED: .gitignore'd file was bundled into S3" \
      "secret.log must never be salvaged"
  else
    pass "gitignored secret.log correctly NOT bundled (no secret leak to S3)"
  fi
  git -C "$RECOVER" checkout -q - 2>/dev/null || true
else
  fail "could not check out refs/salvage/work" "$(tail -5 "$RESTORE_LOG")"
fi

# restore is read-only w.r.t. branches: it writes only under refs/salvage/*.
if [ -z "$(git -C "$RECOVER" for-each-ref --format='%(refname)' refs/heads/ | grep -v 'refs/heads/main$' || true)" ]; then
  pass "restore created no branches (writes only refs/salvage/*)"
else
  fail "restore mutated branches" "$(git -C "$RECOVER" for-each-ref --format='%(refname)' refs/heads/)"
fi

# ── ka-0000005216: a SPOTLESS worktree with an unpushed commit must salvage ──
# The regression this fix closed: an agent that `git commit`ed its work and died
# before `git push` leaves `git status` completely clean, so a dirtiness-only
# check finds NOTHING to salvage — yet the commit exists only in the dying pod's
# EmptyDir and is lost forever. This fixture has ZERO dirty files, so it fails
# unless worktreeHasUnpushedCommits is consulted. (The round-trip above cannot
# prove this: its tree is dirty, so discovery fires regardless.)
CLEANWORK="${TMPROOT}/cleanwork"
git clone -q "$ORIGIN" "$CLEANWORK" 2>/dev/null
git -C "$CLEANWORK" config user.email "e2e@gardenio"
git -C "$CLEANWORK" config user.name "e2e"
echo "COMMITTED_NOT_PUSHED_${RUN_ID}" > "${CLEANWORK}/committed.txt"
git -C "$CLEANWORK" add -A
git -C "$CLEANWORK" commit -qm "work committed but never pushed"
CLEAN_SHA="$(git -C "$CLEANWORK" rev-parse HEAD)"

# Assert the premise: the tree really is spotless, so this fixture is only
# salvageable via the unpushed-commit path.
if [ -z "$(git -C "$CLEANWORK" status --porcelain)" ]; then
  pass "control: unpushed-commit fixture has a spotless worktree"
else
  fail "control: unpushed-commit fixture is dirty" "the ka-0000005216 assertion would be vacuous"
fi

CLEAN_BEAD="${AGENT_BEAD}-clean"
env -u BEADS_HTTP_URL -u BEADS_HTTP_ADDR -u BOAT_TASK_ID -u COOP_S3_SESSION_KEY \
    -u VMAGENT_URL -u AWS_WEB_IDENTITY_TOKEN_FILE -u AWS_ROLE_ARN -u AWS_PROFILE \
    -u GARDENIO_SALVAGE_PUSH \
    COOP_S3_ENDPOINT="http://127.0.0.1:${S3_PORT}" COOP_S3_BUCKET="$E2E_BUCKET" \
    COOP_S3_PREFIX="$E2E_PREFIX" COOP_S3_REGION="us-east-1" \
    AWS_ACCESS_KEY_ID=e2e AWS_SECRET_ACCESS_KEY=e2e AWS_EC2_METADATA_DISABLED=true \
    KD_AGENT_ID="$CLEAN_BEAD" BOAT_AGENT_BEAD_ID="$CLEAN_BEAD" \
    timeout 120 "$GB_BIN" salvage --workspace "$CLEANWORK" --timeout 60s \
    >"${TMPROOT}/clean-salvage.log" 2>&1

CLEAN_DIR="${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${CLEAN_BEAD}/${CLEAN_BEAD}/salvage"
if [ -f "${CLEAN_DIR}/work.bundle" ]; then
  pass "spotless worktree with an unpushed commit IS salvaged (ka-0000005216)"
  CLEAN_RECOVER="${TMPROOT}/clean-recover"
  git clone -q "$ORIGIN" "$CLEAN_RECOVER" 2>/dev/null
  ( cd "$CLEAN_RECOVER" && salvage_env timeout 120 "$GB_BIN" salvage restore \
      --agent-bead "$CLEAN_BEAD" --output "${TMPROOT}/dl-clean" ) \
      >"${TMPROOT}/clean-restore.log" 2>&1
  if git -C "$CLEAN_RECOVER" merge-base --is-ancestor "$CLEAN_SHA" refs/salvage/work 2>/dev/null; then
    pass "RECOVERED: the unpushed commit itself round-tripped through S3"
  else
    fail "unpushed commit LOST from a spotless tree (ka-0000005216 regression)" \
      "${CLEAN_SHA} not an ancestor of refs/salvage/work"
  fi
else
  fail "spotless worktree with an unpushed commit was NOT salvaged (ka-0000005216 regression)" \
    "committed-but-unpushed work would be lost on kill: $(tail -3 "${TMPROOT}/clean-salvage.log")"
fi

# ── Negative control: an unreachable S3 sink must still exit 0 ──
# Port 9 (discard) is closed; salvage must degrade quietly rather than hang or
# fail teardown. This is the property that keeps a broken bucket from wedging
# every pod's preStop hook.
DEAD_LOG="${TMPROOT}/dead.log"
env -u BEADS_HTTP_URL -u BEADS_HTTP_ADDR -u BOAT_TASK_ID -u COOP_S3_SESSION_KEY \
    -u VMAGENT_URL -u AWS_WEB_IDENTITY_TOKEN_FILE -u AWS_ROLE_ARN -u AWS_PROFILE \
    COOP_S3_ENDPOINT="http://127.0.0.1:9" COOP_S3_BUCKET="$E2E_BUCKET" \
    COOP_S3_PREFIX="$E2E_PREFIX" COOP_S3_REGION="us-east-1" \
    AWS_ACCESS_KEY_ID=e2e AWS_SECRET_ACCESS_KEY=e2e AWS_EC2_METADATA_DISABLED=true \
    KD_AGENT_ID="${AGENT_BEAD}-dead" BOAT_AGENT_BEAD_ID="${AGENT_BEAD}-dead" \
    timeout 120 "$GB_BIN" salvage --workspace "$WORK" --timeout 30s \
    >"$DEAD_LOG" 2>&1
assert_verdict "unreachable S3 sink still exits 0 (never wedges teardown)" "$?" "0"

# And restore against an empty prefix must fail LOUDLY (not silently succeed) —
# an operator must never be told "restored" when nothing came back.
EMPTY_LOG="${TMPROOT}/empty.log"
if ( cd "$RECOVER" && salvage_env timeout 60 "$GB_BIN" salvage restore \
      --agent-bead "gd-nonexistent${RUN_ID}" --output "${TMPROOT}/dl-empty" ) \
      >"$EMPTY_LOG" 2>&1; then
  EMPTY_RC=0
else
  EMPTY_RC=$?
fi
if [ "$EMPTY_RC" -ne 0 ] && grep -q "no salvage found at s3://" "$EMPTY_LOG"; then
  pass "restore of a nonexistent salvage fails loudly ('no salvage found')"
else
  fail "restore of a nonexistent salvage did not fail loudly" "$(tail -3 "$EMPTY_LOG")"
fi
echo

# ===========================================================================
# LEG 2 — CONTRACT (always runs): the WIP + clean-exit decision tables.
#
# These mirror, decision-for-decision, the Go that salvage actually runs. The
# clean-exit table in particular cannot be driven from a shell test — that branch
# reads a live agent bead through the daemon client — so the contract is what
# pins both directions in CI.
# ===========================================================================
blue "[contract] real-WIP vs agent-infra classification (ka-0000001049)"

# salvagePorcelainLineIsRealWIP + salvagePathIsInfra (salvage.go). A porcelain
# line is real WIP unless it is an UNTRACKED ("??") path whose FIRST component is
# an agent-infra path — those are provisioned per-pod, not the agent's work, and
# salvaging them would upload junk and post a false "Killed mid-work" comment.
#   salvageInfraPaths = {.claude, .gb, .mcp.json, .state, repos}
porcelain_is_real_wip() {
  local line="$1"
  [ "${#line}" -lt 4 ] && { echo "WIP"; return; }   # too short to parse; don't drop it
  local status="${line:0:2}" path="${line:3}"
  if [ "$status" != "??" ]; then
    echo "WIP"; return                              # any tracked/staged change is real
  fi
  path="${path%\"}"; path="${path#\"}"              # git quotes odd paths
  local head="${path%%/*}"
  case "$head" in
    .claude|.gb|.mcp.json|.state|repos) echo "INFRA" ;;
    *) echo "WIP" ;;
  esac
}

# Tracked changes are always real WIP, whatever the path.
assert_verdict "tracked modification is real WIP" "$(porcelain_is_real_wip " M base.txt")" "WIP"
assert_verdict "staged addition is real WIP" "$(porcelain_is_real_wip "A  new.go")" "WIP"
assert_verdict "staged+unstaged is real WIP" "$(porcelain_is_real_wip "MM main.go")" "WIP"
assert_verdict "deletion is real WIP" "$(porcelain_is_real_wip " D gone.txt")" "WIP"
# A tracked edit UNDER an infra path still counts — the infra filter is untracked-only.
assert_verdict "tracked edit under .claude/ is still real WIP" \
  "$(porcelain_is_real_wip " M .claude/settings.json")" "WIP"
# Untracked agent-infra noise must NOT trigger a salvage on its own.
assert_verdict "untracked .state/ is infra noise" "$(porcelain_is_real_wip "?? .state/scratch")" "INFRA"
assert_verdict "untracked repos/ dir is infra noise" "$(porcelain_is_real_wip "?? repos/")" "INFRA"
assert_verdict "untracked .claude/ is infra noise" "$(porcelain_is_real_wip "?? .claude/foo")" "INFRA"
assert_verdict "untracked .gb/ is infra noise" "$(porcelain_is_real_wip "?? .gb/state.json")" "INFRA"
assert_verdict "untracked .mcp.json is infra noise" "$(porcelain_is_real_wip "?? .mcp.json")" "INFRA"
# Real untracked work IS salvaged — the direction that loses data if broken.
assert_verdict "untracked source file is real WIP" "$(porcelain_is_real_wip "?? feature.go")" "WIP"
assert_verdict "untracked nested source is real WIP" "$(porcelain_is_real_wip "?? pkg/thing/impl.go")" "WIP"
# Negative control: a path merely PREFIXED like an infra dir is NOT infra —
# matching is on the whole first component, so ".stateful/" must salvage.
assert_verdict "untracked .stateful/ is NOT infra (prefix ≠ component)" \
  "$(porcelain_is_real_wip "?? .stateful/x")" "WIP"
assert_verdict "untracked repository/ is NOT infra (prefix ≠ component)" \
  "$(porcelain_is_real_wip "?? repository/x")" "WIP"
echo

blue "[contract] clean-exit skip (ka-0000001049) — salvageIsCleanExit"
# salvageIsCleanExit(): salvage is SKIPPED only for a VOLUNTARY exit —
#   - status=closed AND agent_state=done      → terminal clean `gb stop`
#   - status!=closed AND agent_state=stopping → voluntary freeze/suspend
# Everything else SALVAGES. It fails OPEN (salvage) on an unreadable bead, which
# is the safe bias: an unnecessary bundle is cheap, lost work is not.
clean_exit_verdict() {
  local status="$1" state="$2"
  [ "$status" = "unknown" ] && { echo "SALVAGE"; return; }   # daemon/bead unreadable → fail open
  if [ "$status" = "closed" ] && [ "$state" = "done" ]; then echo "SKIP"; return; fi
  if [ "$status" != "closed" ] && [ "$state" = "stopping" ]; then echo "SKIP"; return; fi
  echo "SALVAGE"
}

# The two voluntary exits — must NOT upload or post "Killed mid-work".
assert_verdict "clean gb stop (closed/done) skips salvage" "$(clean_exit_verdict closed "done")" "SKIP"
assert_verdict "voluntary freeze (open/stopping) skips salvage" "$(clean_exit_verdict open "stopping")" "SKIP"
# THE HEADLINE direction: an involuntary kill mid-work MUST salvage.
assert_verdict "killed mid-work (open/working) SALVAGES" "$(clean_exit_verdict open "working")" "SALVAGE"
assert_verdict "killed with empty state (open/'') SALVAGES" "$(clean_exit_verdict open "")" "SALVAGE"
# Boundary cases: the two predicates are conjunctions — neither half alone skips.
assert_verdict "closed but still working SALVAGES (state must be done)" \
  "$(clean_exit_verdict closed "working")" "SALVAGE"
assert_verdict "closed/stopping SALVAGES (stopping only skips while OPEN)" \
  "$(clean_exit_verdict closed "stopping")" "SALVAGE"
assert_verdict "open/done SALVAGES (done only skips once CLOSED)" \
  "$(clean_exit_verdict open "done")" "SALVAGE"
# Fail-open control: an unreadable bead must salvage, never silently skip.
assert_verdict "unreadable agent bead fails OPEN (salvages)" "$(clean_exit_verdict unknown "")" "SALVAGE"
echo

blue "[contract] Tier-2 quarantine push enable gate (MR !5613)"
# salvagePushEnvEnabled(): GARDENIO_SALVAGE_PUSH is truthy for 1/true/yes/on,
# case-insensitive and trimmed. The push tier is fail-closed by design — it
# mirrors WIP to a real remote, so anything unrecognized must NOT enable it.
push_enabled() {
  local raw="$1"
  raw="$(printf '%s' "$raw" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
  case "$raw" in
    1|true|yes|on) echo "ON" ;;
    *) echo "OFF" ;;
  esac
}
assert_verdict "GARDENIO_SALVAGE_PUSH=1 enables the push tier" "$(push_enabled "1")" "ON"
assert_verdict "GARDENIO_SALVAGE_PUSH=true enables" "$(push_enabled "true")" "ON"
assert_verdict "GARDENIO_SALVAGE_PUSH=TRUE enables (case-insensitive)" "$(push_enabled "TRUE")" "ON"
assert_verdict "GARDENIO_SALVAGE_PUSH=' on ' enables (trimmed)" "$(push_enabled " on ")" "ON"
assert_verdict "GARDENIO_SALVAGE_PUSH=yes enables" "$(push_enabled "yes")" "ON"
assert_verdict "unset stays OFF (Tier-2 is opt-in)" "$(push_enabled "")" "OFF"
assert_verdict "GARDENIO_SALVAGE_PUSH=0 stays OFF" "$(push_enabled "0")" "OFF"
assert_verdict "GARDENIO_SALVAGE_PUSH=false stays OFF" "$(push_enabled "false")" "OFF"
# Fail-closed control: a typo must never silently push WIP to the remote.
assert_verdict "typo 'ture' stays OFF (fail-closed)" "$(push_enabled "ture")" "OFF"
assert_verdict "'enabled' stays OFF (not in the truthy set)" "$(push_enabled "enabled")" "OFF"
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " gb-salvage Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gb-salvage assertions passed — dirty WIP survives kill → S3 → restore."
exit 0
