#!/usr/bin/env bash
# test-gb-salvage-autorestore.sh — E2E gate for the AUTOMATIC salvage
# auto-restore that fires on an agent RESPAWN (epic ka-0000005916 Track B, step
# B2 ka-0000003478; backfill bead gd-WDgPrzYp2N, parent ka-0000000958).
#
# THE BOUNDARY vs test-gb-salvage.sh (deliberately disjoint — do NOT merge):
#   test-gb-salvage.sh covers the MANUAL round-trip — `gb salvage` → S3 →
#   `gb salvage restore` (the operator/successor runs restore by hand). That is
#   real coverage of a DIFFERENT leg and is left untouched.
#   This module covers the AUTOMATIC hook at init_agent_loop.go:107
#   (autoRestoreSalvageOnRespawn), the leg that actually runs in a production
#   respawn: `gb init` — BEFORE it ever launches coop — lists the agent's S3
#   salvage prefix, fetches every bundle's refs/salvage/* into the freshly
#   re-cloned repo, and writes salvage-restore-note.md into the pod's system
#   prompt so the resumed agent cherry-picks its lost WIP instead of redoing it.
#
# WHY THIS GAP MATTERS (context-preservation across respawn): a crew pod's
# EmptyDir is wiped on delete, so a successor re-clones fresh and — WITHOUT this
# hook — redoes the killed agent's uncommitted work from the restored Claude
# transcript's memory (the patrick-incident loss this epic fixes). The automatic
# leg is the one that closes that loop in prod, yet a grep of gardenio/tests/ for
# "auto-restore" / "salvage-restore-note" returned ZERO before this module.
# Advice gd-ExQfhwI2ob records that S3 session resume has been unreliable in the
# prod namespace, so a silent regression here is a real, quiet data-loss risk.
#
# WHAT THIS PROVES — the REAL production call site, driven hermetically:
#   LEG 1 (AUTO-RESTORE, the headline): a REAL `gb salvage` seeds a bundle to a
#     local S3 stub (as the dying pod's preStop hook does), then the REAL
#     `gb init --mock` runs against a FRESH clone that never saw the WIP. Its
#     autoRestoreSalvageOnRespawn hook — the exact code path at
#     init_agent_loop.go:107, fired ONCE per pod boot before the coop restart
#     loop — is asserted to have (a) fetched refs/salvage/work into the fresh
#     clone, (b) made the killed agent's tracked edit, untracked file, AND
#     committed-but-unpushed commit all recoverable from that ref, (c) left the
#     branch/HEAD untouched, and (d) written salvage-restore-note.md whose
#     CONTENT names the restored ref and the recovery recipe (not merely that a
#     file exists — see ANTI-VACUITY below).
#   LEG 2 (DELIVERY): `gb prime --system-prompt`, given the note path
#     BOAT_SALVAGE_RESTORE_NOTE that the restore exported, emits the note into
#     the system-prompt body (outputSalvageRestoreNote) — so the note actually
#     reaches the resumed agent's first turn.
#   LEG 3 (END-TO-END): a second `gb init --mock` (COOP_MAX_RESTARTS=1) writes
#     the pod's .gb/system-prompt.md and it CONTAINS the note — proving the
#     in-process env hand-off (autoRestore os.Setenv → writeSystemPromptFile →
#     gb prime → --append-system-prompt-file) survives end to end, even with a
#     dead daemon/NATS and no coop binary (the note is written before coop
#     launches).
#   NEGATIVE CONTROL: a respawn whose S3 salvage prefix is EMPTY writes NO note
#     and fetches NO ref — the quiet no-op a fresh first-life spawn must be.
#
# ANTI-VACUITY (restated because it is THE failure mode for this test): if no
# bundle was salvaged, the note is LEGITIMATELY absent — so a test that respawns
# with nothing salvaged and finds no note proves nothing. This module therefore
# CREATES a real salvage bundle FIRST, asserts the fresh clone lacks the WIP
# BEFORE restore (so a later "recovered" assertion cannot pass vacuously), then
# respawns and asserts the note's CONTENT references the salvaged ref.
#
# WHY HERMETIC (no cluster / no model spend), and how the real call site is
# still exercised: the automatic hook lives only inside `gb init`'s restart
# loop (no standalone CLI). We drive the REAL `gb init --mock` binary with a
# fully isolated env (env -i + sandboxed HOME so setupPVC never touches the real
# ~/.claude, a dead daemon/NATS so no junk session bead lands on a shared
# daemon, COOP_MAX_RESTARTS=0 so the loop fires auto-restore then returns
# WITHOUT launching coop). salvage/restore honour COOP_S3_ENDPOINT + path-style
# addressing (audit.go s3EndpointOpts) — the same override the in-box MinIO
# overlay uses — so the S3 leg is real against a ~90-line stdlib-python3 stub,
# with no MinIO, no AWS creds, and no network egress. Zero model spend: --mock
# never starts Claude.
#
# Prerequisites: git, python3, and a `gb` binary (GB_BIN, default `gb`). No
# kubectl, no namespace, no beads daemon, no AWS credentials, no network egress.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gb-salvage-autorestore.sh
#   GB_BIN=/path/to/gb ./gardenio/tests/e2e/scripts/test-gb-salvage-autorestore.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GB_BIN="${GB_BIN:-gb}"
RUN_ID="${RUN_ID:-$$}"

# The stub's own bucket/prefix, never the cluster's. The prefix mirrors the
# production default (audit.go s3Prefix) so the asserted salvage key layout is
# the real one.
E2E_BUCKET="e2e-autorestore-${RUN_ID}"
E2E_PREFIX="coop/sessions"

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

# print_summary prints the pass/fail tally and the collected errors.
print_summary() {
  echo "════════════════════════════════════════════════════"
  echo " gb-salvage-autorestore Summary: $PASS passed, $FAIL failed"
  echo "════════════════════════════════════════════════════"
  if [ "${#ERRORS[@]}" -gt 0 ]; then
    for e in "${ERRORS[@]}"; do red "  - $e"; done
  fi
}

# SKIP loud — a missing prerequisite (no git/python3/gb, stub won't bind) must
# never be mistaken for a pass, but must also never red the suite (this module
# is wired into every shard). Use ONLY for a genuine environment gap, NEVER
# after a fail() — a recorded failure must exit non-zero (see abort_failed).
skip_all() {
  blue "SKIP gb-salvage-autorestore: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# abort_failed ends the run early when a REAL failure has already been recorded
# and every downstream assertion would be vacuous (e.g. the salvage bundle never
# seeded, or auto-restore fetched no ref). Unlike skip_all it prints the summary
# and exits NON-ZERO, so a regression that guts the feature reds the module
# instead of silently short-circuiting to a pass. (A mutation test that disabled
# autoRestoreSalvageOnRespawn proved skip_all-after-fail masked the regression.)
abort_failed() {
  [ -n "${1:-}" ] && dim "  aborting: $1"
  print_summary
  exit 1
}

# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null
  [ -n "$TMPROOT" ] && [ -d "$TMPROOT" ] && rm -rf "$TMPROOT"
  return 0
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " AUTOMATIC salvage auto-restore on respawn E2E (gd-WDgPrzYp2N)"
echo " a respawned agent must recover its salvaged WIP + a restore note,"
echo " without the operator running 'gb salvage restore' by hand"
echo "════════════════════════════════════════════════════"

have git     || skip_all "git not available"
have python3 || skip_all "python3 not available"
have "$GB_BIN" || skip_all "gb binary not available (set GB_BIN)"

TMPROOT="$(mktemp -d -t gb-autorestore-e2e-XXXXXX)"

# ── A minimal S3-compatible stub: exactly the 3 ops salvage/restore use. ──
# PutObject (upload), GetObject (download), ListObjectsV2 (discover the prefix).
# Path-style only, which is what s3EndpointOpts forces when COOP_S3_ENDPOINT is
# set. No auth check: the SDK signs every request, we simply don't verify — the
# point is the wire format and the round-trip, not IAM. (Mirrors the stub in
# test-gb-salvage.sh, kept inline so this module is self-contained.)
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
  skip_all "could not start the local S3 stub — cannot exercise the auto-restore"
fi
dim "  S3 stub on 127.0.0.1:${S3_PORT}, bucket ${E2E_BUCKET}"

# A single sandboxed HOME for every gb invocation. setupPVC (agent_k8s_workspace.
# go) does os.RemoveAll(~/.claude) + symlink; pointing HOME here keeps it away
# from the real agent's state and makes gb init side-effect-free.
SBHOME="${TMPROOT}/home"
mkdir -p "${SBHOME}"

AGENT_BEAD="gd-autorestore${RUN_ID}"

# hermetic_gb runs a gb invocation in a FULLY isolated env — nothing leaks in
# from a real agent pod. env -i wipes the environment; we re-add only PATH +
# sandbox HOME + the S3 stub coordinates + static AWS creds (newS3Client has a
# 5s credential-resolution deadline and would otherwise slowly fail the ambient
# chain) + the agent-bead identity that keys the salvage prefix. A dead
# daemon/NATS (127.0.0.1:1) makes every bead/daemon call fail FAST so no junk
# session bead is written and nothing hangs. Extra env (BOAT_WORKSPACE, restart
# cap, …) is passed as trailing `env` KEY=VAL pairs by the caller.
hermetic_gb() {
  env -i \
    PATH="$PATH" \
    HOME="$SBHOME" \
    COOP_S3_ENDPOINT="http://127.0.0.1:${S3_PORT}" \
    COOP_S3_BUCKET="$E2E_BUCKET" \
    COOP_S3_PREFIX="$E2E_PREFIX" \
    COOP_S3_REGION="us-east-1" \
    AWS_ACCESS_KEY_ID="e2e" \
    AWS_SECRET_ACCESS_KEY="e2e" \
    AWS_EC2_METADATA_DISABLED="true" \
    NATS_URL="nats://127.0.0.1:1" \
    BEADS_HTTP_ADDR="http://127.0.0.1:1" \
    BEADS_HTTP_URL="http://127.0.0.1:1" \
    KD_AGENT_ID="$AGENT_BEAD" \
    BOAT_AGENT_BEAD_ID="$AGENT_BEAD" \
    "$@"
}

# ── Fixture: origin + a WIP checkout carrying every class salvage rescues ──
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

# Three classes of lost work, so "recovered" means all of it came back:
#   1. a committed-but-UNPUSHED commit  → lives only in the dying EmptyDir
#   2. a tracked edit                   → uncommitted working-tree change
#   3. an untracked file                → salvage snapshots with git add -A
echo "COMMITTED_NOT_PUSHED_${RUN_ID}" > "${WORK}/committed.txt"
git -C "$WORK" add -A
git -C "$WORK" commit -qm "committed but never pushed"
UNPUSHED_SHA="$(git -C "$WORK" rev-parse HEAD)"
echo "TRACKED_EDIT_${RUN_ID}" >> "${WORK}/base.txt"
echo "UNTRACKED_WIP_${RUN_ID}" > "${WORK}/newfile.txt"

# ===========================================================================
# SETUP — the dying pod's preStop `gb salvage` bundles the WIP to S3.
# (This is the SAME real binary test-gb-salvage.sh exercises; here it is only
# the precondition for the auto-restore under test, so we assert just enough to
# know the bundle really landed before proceeding.)
# ===========================================================================
blue "[setup] real gb salvage seeds the WIP bundle to S3 (dying pod preStop)"
SALVAGE_LOG="${TMPROOT}/salvage.log"
hermetic_gb timeout 120 "$GB_BIN" salvage \
  --workspace "$WORK" --reason involuntary_termination --timeout 60s \
  >"$SALVAGE_LOG" 2>&1

# salvagePrefix(): <prefix>/<agent-bead>/<session>/salvage/ — on the preStop
# path COOP_S3_SESSION_KEY is unset, so session == agent bead.
SALV_DIR="${S3ROOT}/${E2E_BUCKET}/${E2E_PREFIX}/${AGENT_BEAD}/${AGENT_BEAD}/salvage"
if [ -f "${SALV_DIR}/work.bundle" ]; then
  pass "salvage bundle seeded at <prefix>/<agent-bead>/<session>/salvage/work.bundle"
else
  fail "salvage bundle missing from the S3 stub" \
    "expected ${SALV_DIR}/work.bundle; got: $(find "${S3ROOT}" -type f 2>/dev/null | head -5); log: $(tail -3 "$SALVAGE_LOG")"
  abort_failed "no salvage bundle to auto-restore — cannot exercise the respawn hook"
fi
echo

# ===========================================================================
# LEG 1 — AUTO-RESTORE (the headline): the REAL `gb init --mock` respawn hook
# fetches the salvage into a FRESH clone and writes the restore note, with NO
# manual `gb salvage restore` anywhere.
# ===========================================================================
blue "[auto-restore] gb init --mock respawn fetches salvage + writes the note"

# The successor pod: a clean re-clone of origin that never saw the WIP.
FRESH="${TMPROOT}/fresh"
git clone -q "$ORIGIN" "$FRESH" 2>/dev/null
git -C "$FRESH" config user.email "e2e@gardenio"
git -C "$FRESH" config user.name "e2e"

# ANTI-VACUITY control: prove the fresh clone lacks the WIP BEFORE restore, so a
# later "recovered" assertion cannot pass vacuously.
if grep -q "TRACKED_EDIT_${RUN_ID}" "${FRESH}/base.txt" 2>/dev/null; then
  fail "control: fresh clone already has the WIP" "the auto-restore assertion would be vacuous"
else
  pass "control: fresh clone has no WIP before the respawn auto-restore"
fi
if git -C "$FRESH" cat-file -t "$UNPUSHED_SHA" >/dev/null 2>&1; then
  fail "control: fresh clone already has the unpushed commit" "auto-restore assertion would be vacuous"
else
  pass "control: fresh clone lacks the unpushed commit before restore"
fi

# Drive the REAL respawn. COOP_MAX_RESTARTS=0 → runInitLoop fires
# autoRestoreSalvageOnRespawn (init_agent_loop.go:107) ONCE and then returns on
# the restarts>=max guard WITHOUT ever launching coop — so the assertion targets
# purely the auto-restore hook. BOAT_WORKSPACE points resolveCoopWorkdir at the
# fresh clone (BOAT_PROJECT empty → no /home/agent/bot/<project>/work lookup).
INIT_LOG="${TMPROOT}/init-autorestore.log"
hermetic_gb env \
  BOAT_WORKSPACE="$FRESH" \
  BOAT_PROJECT="" \
  BOAT_ROLE="crew-e2e" \
  BOAT_AGENT="$AGENT_BEAD" \
  COOP_MAX_RESTARTS=0 \
  timeout 60 "$GB_BIN" init --mock \
  >"$INIT_LOG" 2>&1

# The respawn must NOT have launched coop (this leg is the pre-coop hook only) —
# a guard so a future refactor that moves the hook after coop launch is caught.
if grep -q "starting coop" "$INIT_LOG"; then
  fail "respawn launched coop under COOP_MAX_RESTARTS=0" "auto-restore must fire before the coop loop"
else
  pass "respawn fired auto-restore before any coop launch (pre-coop hook)"
fi

# (a) refs/salvage/work fetched into the fresh clone by the AUTOMATIC hook.
if git -C "$FRESH" show-ref --verify --quiet refs/salvage/work 2>/dev/null; then
  pass "AUTO-RESTORED: refs/salvage/work fetched into the fresh clone (no manual restore)"
else
  fail "refs/salvage/work not auto-fetched on respawn" \
    "auto-restore lines: $(grep -i salvage "$INIT_LOG" | tail -3)"
  abort_failed "auto-restore did not fetch the salvage ref — downstream asserts would be vacuous"
fi

# (b) the killed agent's work is recoverable from that ref — objects, not just
# the ref pointer. All three classes must survive.
if git -C "$FRESH" show refs/salvage/work:base.txt 2>/dev/null | grep -q "TRACKED_EDIT_${RUN_ID}"; then
  pass "RECOVERED: tracked edit survived kill → S3 → auto-restore"
else
  fail "tracked edit LOST" "base.txt in refs/salvage/work lacks the WIP marker"
fi
if git -C "$FRESH" show refs/salvage/work:newfile.txt 2>/dev/null | grep -q "UNTRACKED_WIP_${RUN_ID}"; then
  pass "RECOVERED: untracked file survived (salvage snapshots with git add -A)"
else
  fail "untracked file LOST" "newfile.txt absent from refs/salvage/work"
fi
if git -C "$FRESH" merge-base --is-ancestor "$UNPUSHED_SHA" refs/salvage/work 2>/dev/null; then
  pass "RECOVERED: committed-but-unpushed HEAD is an ancestor of refs/salvage/work"
else
  fail "unpushed commit LOST" "${UNPUSHED_SHA} not an ancestor of refs/salvage/work"
fi

# (c) the auto-restore is read-only w.r.t. branches — it writes ONLY refs/salvage/*.
STRAY_BRANCHES="$(git -C "$FRESH" for-each-ref --format='%(refname)' refs/heads/ | grep -v 'refs/heads/main$' || true)"
if [ -z "$STRAY_BRANCHES" ]; then
  pass "auto-restore created no branches (writes only refs/salvage/*)"
else
  fail "auto-restore mutated branches" "$STRAY_BRANCHES"
fi

# (d) the restore note file exists AND its CONTENT references the salvaged work
# (the anti-vacuity requirement: not merely that a file exists).
NOTE_FILE="${FRESH}/.gb/salvage-restore-note.md"
if [ -f "$NOTE_FILE" ]; then
  pass "salvage-restore-note.md written under the pod workdir's .gb/"
  NOTE="$(cat "$NOTE_FILE")"
  if grep -q "refs/salvage/work" <<<"$NOTE"; then
    pass "note CONTENT names the restored ref (refs/salvage/work)"
  else
    fail "note does not name the restored ref" "note:\n$NOTE"
  fi
  if grep -q "cherry-pick" <<<"$NOTE" && grep -q "grep salvage" <<<"$NOTE"; then
    pass "note CONTENT carries the recovery recipe (grep salvage + cherry-pick)"
  else
    fail "note missing the recovery recipe" "note:\n$NOTE"
  fi
  if grep -q "gb salvage restore --agent-bead ${AGENT_BEAD}" <<<"$NOTE"; then
    pass "note points at the full-bundle recovery command for this agent"
  else
    fail "note missing the copy-pasteable 'gb salvage restore' recipe" "note:\n$NOTE"
  fi
else
  fail "salvage-restore-note.md not written by the respawn auto-restore" \
    "expected ${NOTE_FILE}; init log: $(grep -i salvage "$INIT_LOG" | tail -3)"
fi
echo

# ===========================================================================
# LEG 2 — DELIVERY: gb prime --system-prompt emits the restore note into the
# system-prompt body (outputSalvageRestoreNote) when the note env is set — so
# the note actually reaches the resumed agent's first turn and survives
# compaction (it rides the cached system prompt).
# ===========================================================================
blue "[delivery] gb prime --system-prompt emits the restore note"
if [ -f "$NOTE_FILE" ]; then
  PRIME_OUT="${TMPROOT}/prime.out"
  # Point BOAT_SALVAGE_RESTORE_NOTE at the note the AUTO-RESTORE just wrote (the
  # env autoRestoreSalvageOnRespawn exports in-process is what production reads).
  hermetic_gb env \
    BOAT_WORKSPACE="$FRESH" \
    BOAT_PROJECT="gardenio" \
    BOAT_ROLE="crew-e2e" \
    BOAT_SALVAGE_RESTORE_NOTE="$NOTE_FILE" \
    timeout 60 "$GB_BIN" prime --system-prompt \
    >"$PRIME_OUT" 2>/dev/null
  if grep -q "Salvaged work restored from a prior life" "$PRIME_OUT"; then
    pass "system prompt carries the salvaged-WIP recovery note heading"
  else
    fail "gb prime --system-prompt did not emit the note heading" \
      "output tail: $(tail -5 "$PRIME_OUT")"
  fi
  if grep -q "refs/salvage/work" "$PRIME_OUT"; then
    pass "system prompt names the restored ref (refs/salvage/work)"
  else
    fail "system prompt is missing the restored ref" "output tail: $(tail -5 "$PRIME_OUT")"
  fi

  # Negative control for the emitter: with the note env UNSET, the system prompt
  # must NOT carry the note (a fresh spawn must not pay the budget for it).
  PRIME_NONOTE="${TMPROOT}/prime-nonote.out"
  hermetic_gb env \
    BOAT_WORKSPACE="$FRESH" \
    BOAT_PROJECT="gardenio" \
    BOAT_ROLE="crew-e2e" \
    timeout 60 "$GB_BIN" prime --system-prompt \
    >"$PRIME_NONOTE" 2>/dev/null
  if grep -q "Salvaged work restored from a prior life" "$PRIME_NONOTE"; then
    fail "system prompt emitted the note with the env UNSET" "must be a no-op for fresh spawns"
  else
    pass "note is absent from the system prompt when BOAT_SALVAGE_RESTORE_NOTE is unset (fresh-spawn no-op)"
  fi
else
  fail "skipping delivery leg — no note file was produced by leg 1"
fi
echo

# ===========================================================================
# LEG 3 — END-TO-END: a full `gb init --mock` (COOP_MAX_RESTARTS=1) writes the
# pod's .gb/system-prompt.md and it CONTAINS the note. This proves the complete
# in-process hand-off — autoRestore's os.Setenv(BOAT_SALVAGE_RESTORE_NOTE) →
# writeSystemPromptFile → `gb prime --system-prompt` → --append-system-prompt-
# file — that no single-command leg above can prove on its own. The note is
# written BEFORE coop launches, so this holds even though coop then fails to
# start against the dead env (which is fine: we assert the file, not coop).
# ===========================================================================
blue "[end-to-end] gb init writes .gb/system-prompt.md carrying the note"
FRESH2="${TMPROOT}/fresh2"
git clone -q "$ORIGIN" "$FRESH2" 2>/dev/null
git -C "$FRESH2" config user.email "e2e@gardenio"
git -C "$FRESH2" config user.name "e2e"
SP_FILE="${FRESH2}/.gb/system-prompt.md"
INIT2_LOG="${TMPROOT}/init-e2e.log"
hermetic_gb env \
  BOAT_WORKSPACE="$FRESH2" \
  BOAT_PROJECT="" \
  BOAT_ROLE="crew-e2e" \
  BOAT_AGENT="$AGENT_BEAD" \
  COOP_MAX_RESTARTS=1 \
  timeout 90 "$GB_BIN" init --mock \
  >"$INIT2_LOG" 2>&1
if [ -f "$SP_FILE" ]; then
  pass "gb init wrote .gb/system-prompt.md for --append-system-prompt-file"
  if grep -q "Salvaged work restored from a prior life" "$SP_FILE" \
     && grep -q "refs/salvage/work" "$SP_FILE"; then
    pass "END-TO-END: the pod's system prompt carries the restore note referencing refs/salvage/work"
  else
    fail "system-prompt.md does not carry the restore note" \
      "auto-restore lines: $(grep -i salvage "$INIT2_LOG" | tail -3)"
  fi
else
  fail "gb init did not write .gb/system-prompt.md" "init log: $(tail -5 "$INIT2_LOG")"
fi
echo

# ===========================================================================
# NEGATIVE CONTROL — a respawn whose S3 salvage prefix is EMPTY must be a quiet
# no-op: no ref fetched, no note written. (A fresh first-life spawn with no
# prior salvage.) This is the direction that a broken "always write a note"
# regression would flip — and it is the exact vacuity the anti-vacuity rule
# guards against, proven from the OTHER side.
# ===========================================================================
blue "[negative] respawn with an empty salvage prefix writes no note, fetches no ref"
FRESH_EMPTY="${TMPROOT}/fresh-empty"
git clone -q "$ORIGIN" "$FRESH_EMPTY" 2>/dev/null
EMPTY_BEAD="gd-nosalvage${RUN_ID}"
INITN_LOG="${TMPROOT}/init-empty.log"
# Same hermetic env but a DIFFERENT agent bead → its salvage prefix is empty.
env -i \
  PATH="$PATH" HOME="$SBHOME" \
  COOP_S3_ENDPOINT="http://127.0.0.1:${S3_PORT}" COOP_S3_BUCKET="$E2E_BUCKET" \
  COOP_S3_PREFIX="$E2E_PREFIX" COOP_S3_REGION="us-east-1" \
  AWS_ACCESS_KEY_ID="e2e" AWS_SECRET_ACCESS_KEY="e2e" AWS_EC2_METADATA_DISABLED="true" \
  NATS_URL="nats://127.0.0.1:1" BEADS_HTTP_ADDR="http://127.0.0.1:1" BEADS_HTTP_URL="http://127.0.0.1:1" \
  KD_AGENT_ID="$EMPTY_BEAD" BOAT_AGENT_BEAD_ID="$EMPTY_BEAD" \
  env BOAT_WORKSPACE="$FRESH_EMPTY" BOAT_PROJECT="" BOAT_ROLE="crew-e2e" BOAT_AGENT="$EMPTY_BEAD" \
  COOP_MAX_RESTARTS=0 \
  timeout 60 "$GB_BIN" init --mock \
  >"$INITN_LOG" 2>&1
if [ -f "${FRESH_EMPTY}/.gb/salvage-restore-note.md" ]; then
  fail "empty-salvage respawn wrote a restore note" "must be a quiet no-op"
else
  pass "empty-salvage respawn wrote NO restore note (quiet no-op)"
fi
if git -C "$FRESH_EMPTY" for-each-ref --format='%(refname)' refs/salvage/ 2>/dev/null | grep -q .; then
  fail "empty-salvage respawn fetched a salvage ref" "nothing was salvaged for this agent"
else
  pass "empty-salvage respawn fetched NO salvage ref"
fi
echo

# ── Summary ───────────────────────────────────────────────────────
print_summary
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
green "All auto-restore assertions passed — a respawned agent recovers its salvaged WIP + restore note automatically."
exit 0
