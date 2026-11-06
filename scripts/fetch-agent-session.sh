#!/usr/bin/env bash
# fetch-agent-session.sh — pull a coop agent session from S3 for forensic analysis.
#
# Old agent sessions are archived to the S3 bucket named by $COOP_S3_BUCKET
# (prod: gardenio-sessions-prod) under $COOP_S3_PREFIX (default coop/sessions),
# keyed by AGENT BEAD ID then coop spawn-UUID:
#
#   s3://<bucket>/<prefix>/<agent-bead-id>/<spawn-uuid>/
#       meta.json            SessionMeta: role, started_at, ended_at, exit_status, exit_code
#       state_events.jsonl   coop state machine (starting->idle->working->error->...)
#       hook_events.jsonl    every UserPromptSubmit + Pre/PostToolUse (full tool I/O)
#       transcripts/N.jsonl  numbered snapshots of the Claude session log (highest N = latest)
#       recording/*.jsonl    terminal screen recordings (final frame = death state)
#       stdout-tail.txt      coop process stdout tail (SIGTERM / drain / restore lines)
#
# Access is via the pod's IRSA role (arn .../gardenio-prod-agent) — no extra creds.
#
# Usage:
#   scripts/fetch-agent-session.sh <agent-bead-id> [dest-dir]     # newest spawn under the bead
#   scripts/fetch-agent-session.sh <agent-bead-id>/<uuid> [dest]  # a specific spawn
#   scripts/fetch-agent-session.sh --list <agent-bead-id>         # list spawns under the bead
#
# Then eyeball it:
#   jq . <dest>/meta.json
#   jq -r '.last_message // empty' <dest>/state_events.jsonl | tail
#   scripts/fetch-agent-session.sh --summary <dest>              # one-line verdict
set -euo pipefail

BUCKET="${COOP_S3_BUCKET:-gardenio-sessions-prod}"
PREFIX="${COOP_S3_PREFIX:-coop/sessions}"
BASE="s3://${BUCKET}/${PREFIX}"

die() { echo "error: $*" >&2; exit 1; }

# --summary <local-session-dir>: print a compact forensic verdict from a fetched dir.
if [[ "${1:-}" == "--summary" ]]; then
  d="${2:?usage: --summary <session-dir>}"
  [[ -f "$d/meta.json" ]] || die "no meta.json under $d"
  python3 - "$d" <<'PY'
import json, sys, datetime, collections
d = sys.argv[1]
m = json.load(open(f"{d}/meta.json"))
st, en = m.get("started_at"), m.get("ended_at")
dur = (en - st) if (st and en) else None
def hhmm(t): return datetime.datetime.fromtimestamp(t, datetime.UTC).strftime("%Y-%m-%d %H:%M:%SZ") if t else "?"
print(f"agent      : {m.get('agent_name')}  ({m.get('role')})  bead={m.get('bead_id')}")
print(f"window     : {hhmm(st)} -> {hhmm(en)}" + (f"  ({dur/60:.1f} min)" if dur else ""))
print(f"exit       : status={m.get('exit_status')} code={m.get('exit_code')}  transcripts={m.get('transcript_count')}")
try:
    ev = [json.loads(l) for l in open(f"{d}/state_events.jsonl") if l.strip()]
    errs = sum(1 for e in ev if e.get("next") == "error")
    print(f"states     : {len(ev)} events, {errs} error-transitions (retry loops)")
    last = next((e.get("last_message") for e in reversed(ev) if e.get("last_message")), None)
    if last:
        print(f"last error : {last.splitlines()[0][:200]}")
except FileNotFoundError:
    print("states     : (no state_events.jsonl)")
try:
    hk = [json.loads(l) for l in open(f"{d}/hook_events.jsonl") if l.strip()]
    names = collections.Counter((h.get("json", {}).get("data", {}) or {}).get("hook_event_name") for h in hk)
    tools = collections.Counter()
    for h in hk:
        data = h.get("json", {}).get("data", {}) or {}
        if data.get("hook_event_name") == "PostToolUse":
            tools[(data.get("tool_input", {}) or {}).get("command", "")[:30] or "?"] += 1
    print(f"hooks      : {sum(names.values())} events  {dict(names)}")
    if tools:
        print(f"tools run  : {dict(tools)}")
except FileNotFoundError:
    print("hooks      : (no hook_events.jsonl)")
PY
  exit 0
fi

# --list <agent-bead-id>: enumerate spawn-UUIDs under a bead.
if [[ "${1:-}" == "--list" ]]; then
  bead="${2:?usage: --list <agent-bead-id>}"
  aws s3 ls "${BASE}/${bead}/" | awk '{print $2}'
  exit 0
fi

key="${1:?usage: fetch-agent-session.sh <agent-bead-id>[/<uuid>] [dest-dir]}"
dest="${2:-}"

# If no explicit UUID, resolve the newest spawn under the bead.
if [[ "$key" != */* || "$key" == */ ]]; then
  bead="${key%/}"
  uuid="$(aws s3 ls "${BASE}/${bead}/" | awk '{print $2}' | sed 's#/$##' | tail -n1)"
  [[ -n "$uuid" ]] || die "no spawns found under ${BASE}/${bead}/"
  key="${bead}/${uuid}"
  echo "resolved newest spawn: ${key}" >&2
fi

dest="${dest:-./session-${key//\//_}}"
mkdir -p "$dest"
echo "fetching ${BASE}/${key}/ -> ${dest}" >&2
aws s3 cp "${BASE}/${key}/" "${dest}/" --recursive
echo "done. summarize with: $0 --summary ${dest}" >&2
