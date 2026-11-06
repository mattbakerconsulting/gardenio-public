#!/usr/bin/env bash
# test-lib-kd-create.sh — unit test for lib-kd-create.sh.
#
# Drives kd_create_id / kd_json_id against a fake kd, no cluster needed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$SCRIPT_DIR/lib-kd-create.sh"

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  FAIL: $1"; }

check() {
  local name="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then pass "$name"; else fail "$name: want '$want', got '$got'"; fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# fake_kd prints a JSON bead only when called with --json, like kd.
fake_kd() {
  local a json=0
  for a in "$@"; do [ "$a" = "--json" ] && json=1; done
  if [ "$json" = 1 ]; then
    printf '{\n  "id": "%s",\n  "title": "t"\n}\n' "$FAKE_ID"
  else
    printf 'ID:          %s\n' "$FAKE_ID"
  fi
}
fake_kd_fail() { echo "Error: trigger type not allowed" >&2; return 1; }
fake_kd_text() { echo "Escalation created: gd-esc123"; }

FAKE_ID="gd-wndHkix9fR"
check "gd- id parsed (the lane daemon prefix)" "gd-wndHkix9fR" \
  "$(kd_create_id fake_kd create x --type=task 2>/dev/null)"
FAKE_ID="kd-abc123"
check "kd- id still parsed" "kd-abc123" "$(kd_create_id fake_kd create x 2>/dev/null)"

got=$(kd_create_id fake_kd_fail create x 2>"$TMP/err")
check "failed create prints no id" "" "$got"
if grep -q "trigger type not allowed" "$TMP/err" && grep -q "rc=1" "$TMP/err"; then
  pass "failed create: kd stderr and rc reach stderr"
else
  fail "failed create: stderr was '$(cat "$TMP/err")'"
fi

got=$(kd_create_id fake_kd_text create x 2>"$TMP/err")
check "non-JSON stdout prints no id (no prefix scraping)" "" "$got"
if grep -q "Escalation created" "$TMP/err"; then
  pass "non-JSON stdout is echoed to stderr"
else
  fail "non-JSON stdout missing from stderr: '$(cat "$TMP/err")'"
fi

check "kd_json_id parses captured JSON" "gd-x1" "$(kd_json_id '{"id":"gd-x1"}')"
check "kd_json_id on garbage is empty" "" "$(kd_json_id 'not json')"
check "kd_json_id on empty is empty" "" "$(kd_json_id '')"

echo
echo "lib-kd-create: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
