#!/usr/bin/env bash
# regen.sh — regenerate coop/tests/fixtures/gemini/ from the pinned Gemini CLI.
#
# Runs the CLI (the real pinned bundle, installed by install/gemini.sh into a
# temp prefix) against tools/mock-api/mock_gemini_api.mjs, so NO API KEY is
# needed and none is ever read. What this captures is the CLI-side contract:
# stream-json/json event shapes, hook payloads and exit contract, session
# record, ACP frames, PTY screens, exit codes. It cannot capture real model
# ids' availability, real usage numbers or real quota errors (spike.md "needs a
# live key").
#
# Usage: docs/research/gemini-harness/regen.sh [outdir]   (default: coop/tests/fixtures/gemini)
#        WITH_429=1 also captures the 10-attempt 429 retry run (~3 min).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
OUT="$(mkdir -p "${1:-$ROOT/coop/tests/fixtures/gemini}" && cd "${1:-$ROOT/coop/tests/fixtures/gemini}" && pwd)"
W="$(TMPDIR=/tmp mktemp -d)"; trap 'kill $MOCK_PID 2>/dev/null || true; rm -rf "$W"' EXIT
PORT="${PORT:-18181}"

INSTALL_PREFIX="$W/prefix" HOME="$W/inst-home" bash "$ROOT/gardenio/images/agent/install/gemini.sh" > /dev/null
export GBIN="$W/prefix/lib/gemini-cli/gemini.js"
G="node $GBIN"

mkdir -p "$W/home/.gemini" "$W/ws" "$W/hooklog" "$OUT"/{headless,hooks,session,acp,pty,settings}
node "$HERE/tools/mock-api/mock_gemini_api.mjs" "$PORT" "$W/req.jsonl" 2> "$W/mock.err" & MOCK_PID=$!
sleep 1

export HOME="$W/home" GOOGLE_GEMINI_BASE_URL="http://127.0.0.1:$PORT" GEMINI_API_KEY=mock-key \
  GEMINI_CLI_TRUST_WORKSPACE=true HOOKLOG="$W/hooklog"
cd "$W/ws"

# Strip machine-specific paths so the fixtures diff cleanly between runs.
clean() { sed -e "s#$W/ws#/work#g" -e "s#$W/home#/home/user#g" -e "s#$W/prefix#/usr/local#g" -e "s#$W#/tmp/spike#g" "$@"; }
settings() { # settings <json-fragment merged over the auth block>
  python3 - "$1" <<'PY' > "$HOME/.gemini/settings.json"
import json, sys
s = {"security": {"auth": {"selectedType": "gemini-api-key"}},
     "general": {"enableAutoUpdate": False, "enableAutoUpdateNotification": False},
     "privacy": {"usageStatisticsEnabled": False}}
s.update(json.loads(sys.argv[1])); print(json.dumps(s, indent=1))
PY
}
cp "$HERE/tools/hook.sh" "$W/hook.sh"   # short, repo-independent path: it shows up in the PTY screen
hooks_json() { python3 - "$W/hook.sh" "$@" <<'PY'
import json, sys
h = sys.argv[1]; evs = sys.argv[2:]
print(json.dumps({"hooks": {e: [{"hooks": [{"type": "command", "command": f"{h} {e}"}]}] for e in evs}}))
PY
}
ALL_EVENTS="SessionStart SessionEnd BeforeAgent AfterAgent BeforeTool AfterTool Notification PreCompress"
reset_hooklog() { rm -f "${HOOKLOG:?}"/*; }
save_hooks() { # save_hooks <prefix>: sanitized payload per event, first of each
  for f in "$HOOKLOG"/*-*.json; do
    [ -e "$f" ] || continue
    ev="$(basename "$f" .json | sed 's/^[0-9]*-//')"
    [ -e "$OUT/hooks/$1$ev.json" ] || python3 -m json.tool "$f" | clean > "$OUT/hooks/$1$ev.json"
  done
}

settings '{}'
$G -p "say pong" -o json 2> /dev/null | clean > "$OUT/headless/json-text.json"
$G -p "say pong" -o stream-json 2> /dev/null | clean > "$OUT/headless/stream-json-text.jsonl"
# Default approval mode: the shell tool is not even registered headlessly.
$G -p "MOCK:TOOL echo hi" -o stream-json 2> /dev/null | clean > "$OUT/headless/stream-json-tool-default-mode.jsonl"
$G -p "MOCK:TOOL echo hi" -o stream-json --approval-mode yolo 2> /dev/null | clean > "$OUT/headless/stream-json-tool-yolo.jsonl"
$G -p "x" -o stream-json 2> /dev/null > /dev/null || true
# Exit codes: no auth (41), untrusted workspace (55).
# OK: disable errexit only so the two failing gemini runs below can record their
# non-zero exit codes into the fixtures; set -e is restored right after.
set +e
HOME="$W/noauth" GEMINI_API_KEY= $G -p x > /dev/null 2> "$W/e41"; echo "exit=$?" > "$OUT/headless/exit-no-auth.txt"; clean "$W/e41" >> "$OUT/headless/exit-no-auth.txt"
GEMINI_CLI_TRUST_WORKSPACE= $G -p x > /dev/null 2> "$W/e55"; echo "exit=$?" > "$OUT/headless/exit-untrusted.txt"; clean "$W/e55" >> "$OUT/headless/exit-untrusted.txt"
set -e

# Hooks in headless mode, all events, yolo tool call.
settings "$(hooks_json $ALL_EVENTS)"
reset_hooklog
$G -p "MOCK:TOOL echo hi" -o stream-json --approval-mode yolo > /dev/null 2>&1
save_hooks headless-
sid="$(ls -t "$HOME"/.gemini/tmp/*/chats/*.jsonl | head -1)"
clean "$sid" > "$OUT/session/session-record-tool.jsonl"

# Stop-gate contract: which AfterAgent outputs continue the turn.
for m in continue-true continue-false exit2 block; do
  reset_hooklog
  AFTERAGENT_MODE=$m $G -p "say pong" -o stream-json 2> /dev/null | clean > "$OUT/headless/stopgate-$m.jsonl"
  echo "afteragent_fires=$(ls "$HOOKLOG" | grep -c AfterAgent)" >> "$OUT/headless/stopgate-$m.jsonl.meta"
  [ "$m" = block ] && save_hooks stopgate-block-
done

# Resume from the session file alone, in a fresh HOME.
sid_id="$(head -1 "$sid" | python3 -c 'import json,sys;print(json.loads(sys.stdin.read())["sessionId"])')"
slug="$(basename "$(dirname "$(dirname "$sid")")")"
mkdir -p "$W/home2/.gemini/tmp/$slug/chats"; cp "$sid" "$W/home2/.gemini/tmp/$slug/chats/"
cp "$HOME/.gemini/settings.json" "$W/home2/.gemini/settings.json"; settings '{}'; cp "$HOME/.gemini/settings.json" "$W/home2/.gemini/settings.json"
: > "$W/req.jsonl"
HOME="$W/home2" $G --resume "$sid_id" -p "and again" -o stream-json 2> /dev/null | clean > "$OUT/session/resume-fresh-home.jsonl"
python3 - "$W/req.jsonl" <<'PY' > "$OUT/session/resume-fresh-home.history.json"
import json, sys
last = [json.loads(l) for l in open(sys.argv[1])][-1]["body"]["contents"]
print(json.dumps([{"role": c["role"], "parts": [(p.get("text") or "/".join(p))[:60] for p in c["parts"]]} for c in last], indent=1))
PY

# ACP: basic, tool + permission, cancel, second prompt during a turn, hooks.
settings "$(hooks_json $ALL_EVENTS)"
reset_hooklog
python3 "$HERE/tools/acp_client.py" $G -- basic "$W/acp-basic.jsonl" > /dev/null
python3 "$HERE/tools/acp_client.py" $G -- tool "$W/acp-tool.jsonl" > /dev/null
ACP_TOOLCMD="MOCK:TOOL touch acp-created.txt" python3 "$HERE/tools/acp_client.py" $G -- tool "$W/acp-perm.jsonl" > /dev/null
python3 "$HERE/tools/acp_client.py" $G -- cancel "$W/acp-cancel.jsonl" > /dev/null
python3 "$HERE/tools/acp_client.py" $G -- queue "$W/acp-queue.jsonl" > /dev/null || true
for n in basic tool perm cancel queue; do clean "$W/acp-$n.jsonl" > "$OUT/acp/$n.jsonl"; done
ls "$HOOKLOG" | sed 's/^[0-9]*-//; s/\.json$//' | sort | uniq -c > "$OUT/acp/hooks-fired.txt"

# MCP: mcpServers shape in user settings.
settings "$(python3 -c "import json;print(json.dumps({'mcpServers':{'mini':{'command':'node','args':['$HERE/tools/mini_mcp.mjs'],'trust':True}}}))")"
$G mcp list 2> /dev/null | clean | sed "s#$HERE#<repo>/docs/research/gemini-harness#" > "$OUT/settings/mcp-list.txt"
: > "$W/req.jsonl"; $G -p "say pong" -o stream-json > /dev/null 2>&1
python3 - "$W/req.jsonl" <<'PY' > "$OUT/settings/main-request-tools.json"
import json, sys
last = [json.loads(l) for l in open(sys.argv[1])][-1]["body"]
print(json.dumps(sorted(d["name"] for t in last.get("tools", []) for d in t.get("functionDeclarations", [])), indent=1))
PY
settings "$(python3 -c "import json;print(json.dumps({'mcpServers':{'mini':{'command':'node','args':['<repo>/docs/research/gemini-harness/tools/mini_mcp.mjs'],'trust':True}}}))")"
cp "$HOME/.gemini/settings.json" "$OUT/settings/user-settings-example.json"

# System-settings precedence guard (parent dir must be root-owned).
echo '{"model":{"name":"sys-model"}}' > "$W/sys.json"
GEMINI_CLI_SYSTEM_SETTINGS_PATH="$W/sys.json" $G -p x -o stream-json 2>&1 >/dev/null | grep "Security Warning" | clean | sed -E "s#(Directory|Parent directory) '[^']*'#\1 '<dir>'#g; s#Skipping system settings file '[^']*'#Skipping system settings file '<path>'#" > "$OUT/settings/system-settings-insecure-dir.txt" || true

# PTY (interactive TUI): startup screen and a permission prompt.
settings "$(hooks_json $ALL_EVENTS)"
reset_hooklog
python3 "$HERE/tools/pty_run.py" "$W/pty-turn.raw" "wait:8" "send:MOCK:TOOL touch pty-created.txt" "wait:1" "send:\r" "wait:5" "send:\r" "wait:3" > /dev/null
clean "$W/pty-turn.raw.txt" | tr -s '\n ' > "$OUT/pty/turn-with-permission.txt"
cp "$W/pty-turn.raw" "$OUT/pty/turn-with-permission.raw"
save_hooks pty-

# Default-config egress: which hosts a default run contacts (usage statistics).
if [ "${WITH_429:-}" = 1 ]; then
  settings '{}'
  timeout 600 $G -p "MOCK:429" -o stream-json 2> /dev/null | clean > "$OUT/headless/stream-json-429.jsonl" || echo "exit=$?" >> "$OUT/headless/stream-json-429.jsonl"
fi
echo "fixtures written to $OUT"
