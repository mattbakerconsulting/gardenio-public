#!/bin/bash
# logs event name + stdin payload; prints {} ; behaviour knobs via env
ev="$1"
payload="$(cat)"
echo "$payload" > "$HOOKLOG/$(date +%s%N)-$ev.json"
echo "$ev env: GEMINI_SESSION_ID=${GEMINI_SESSION_ID:-} GEMINI_PROJECT_DIR=${GEMINI_PROJECT_DIR:-} CLAUDE_PROJECT_DIR=${CLAUDE_PROJECT_DIR:-}" >> "$HOOKLOG/env.txt"
if [ "$ev" = AfterAgent ] && [ -n "$AFTERAGENT_MODE" ] && [ ! -f "$HOOKLOG/denied" ]; then
  touch "$HOOKLOG/denied"
  case "$AFTERAGENT_MODE" in
    continue-true) echo '{"continue":true}'; exit 0 ;;
    continue-false) echo '{"continue":false,"stopReason":"halt"}'; exit 0 ;;
    exit2) echo "STOPGATE-EXIT2: say banana" >&2; exit 2 ;;
    block) echo '{"decision":"block","reason":"STOPGATE-BLOCK: say banana"}'; exit 0 ;;
  esac
fi
if [ "$ev" = AfterAgent ] && [ -n "$AFTERAGENT_DENY_ONCE" ] && [ ! -f "$HOOKLOG/denied" ]; then
  touch "$HOOKLOG/denied"
  echo '{"decision":"deny","reason":"STOPGATE: please also say the word banana"}'
  exit 0
fi
echo '{}'
