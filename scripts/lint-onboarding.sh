#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Lint the operator-onboarding artifacts so they can't silently bit-rot.
# Fast, no daemon, no secrets — safe for the MR lint stage.
#
# Tier A (always): bash -n on every onboarding script; claude-hooks.json is
#   valid JSON; every hook command is a `gb …` invocation; the load-bearing
#   wiring is present (SessionStart→prime, Stop→stop-gate).
# Tier B (only if `gb` is on PATH): every `gb hook <sub>` / `gb workspace <sub>`
#   referenced by claude-hooks.json is a real subcommand — catches renames.
#
# Usage: ./scripts/lint-onboarding.sh
# ─────────────────────────────────────────────────────────────────────────────
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

HOOKS="scripts/claude-hooks.json"
SCRIPTS=(scripts/onboard.sh scripts/onboard_test.sh scripts/onboard-mac.sh scripts/gb-claude scripts/gardenio-doctor.sh scripts/test-operator-contract.sh scripts/test-gb-hooks.sh scripts/lint-onboarding.sh scripts/gardenio-version.sh)
fails=0
pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; fails=$((fails+1)); }

# ── Tier A ───────────────────────────────────────────────────────────────────
for s in "${SCRIPTS[@]}"; do
  if [ -f "$s" ] && bash -n "$s" 2>/dev/null; then pass "syntax: $s"; else fail "syntax: $s"; fi
done

if python3 -c "import json,sys; json.load(open('$HOOKS'))" 2>/dev/null; then
  pass "valid JSON: $HOOKS"
else
  fail "invalid JSON: $HOOKS"; echo; { [ $fails -eq 0 ] && exit 0 || exit 1; }
fi

# Every hook command must be a `gb …` invocation.
mapfile -t CMDS < <(python3 - "$HOOKS" <<'PY'
import json,sys
h=json.load(open(sys.argv[1]))["hooks"]
for ev in h.values():
    for grp in ev:
        for hk in grp.get("hooks",[]):
            print(hk["command"])
PY
)
nongb=0
for c in "${CMDS[@]}"; do [[ "$c" == gb\ * ]] || { fail "non-gb hook command: $c"; nongb=$((nongb+1)); }; done
[ $nongb -eq 0 ] && pass "all ${#CMDS[@]} hook commands are 'gb …' invocations"

grep -q 'SessionStart' 2>/dev/null <<<"${CMDS[@]}" || true
python3 - "$HOOKS" <<'PY' && pass "load-bearing wiring present (SessionStart→prime, Stop→stop-gate)" || { echo "FAIL: missing prime/stop-gate wiring"; exit 1; }
import json,sys
h=json.load(open(sys.argv[1]))["hooks"]
def cmds(ev): return [hk["command"] for g in h.get(ev,[]) for hk in g.get("hooks",[])]
ok = any("gb hook prime" in c for c in cmds("SessionStart")) and any("gb hook stop-gate" in c for c in cmds("Stop"))
sys.exit(0 if ok else 1)
PY

# ── Tier B (needs gb) ──────────────────────────────────────────────────────────
if command -v gb >/dev/null 2>&1; then
  hook_subs=$(gb hook --help 2>&1 | awk '/Available Commands:/{f=1;next} /^Flags:|^Global Flags:/{f=0} f && NF {print $1}')
  ws_subs=$(gb workspace --help 2>&1 | awk '/Available Commands:/{f=1;next} /^Flags:|^Global Flags:/{f=0} f && NF {print $1}')
  miss=0
  for c in "${CMDS[@]}"; do
    set -- $c                       # c = "gb hook prime …"
    grp="$2"; sub="${3:-}"
    case "$grp" in
      hook)      grep -qx "$sub" <<<"$hook_subs" || { fail "gb hook '$sub' not a real subcommand"; miss=$((miss+1)); } ;;
      workspace) grep -qx "$sub" <<<"$ws_subs"   || { fail "gb workspace '$sub' not a real subcommand"; miss=$((miss+1)); } ;;
    esac
  done
  [ $miss -eq 0 ] && pass "all referenced gb subcommands exist in this gb binary"
else
  echo "note: gb not on PATH — skipped subcommand-existence check (Tier B)"
fi

echo
[ $fails -eq 0 ] && { pass "onboarding artifacts lint clean"; exit 0; } || { fail "$fails issue(s)"; exit 1; }
