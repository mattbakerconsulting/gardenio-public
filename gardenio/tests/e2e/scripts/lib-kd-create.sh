#!/usr/bin/env bash
# lib-kd-create.sh — read a new bead's id from `kd create --json`.
#
# The e2e scripts used to scrape kd create's table output with
# `2>/dev/null | grep -oP 'kd-\S+'`. The lane daemon mints gd- ids, so a
# create that worked read as "kd create returned no id", and a create that
# really failed had its error thrown away with stderr (gd-hLx8BizDTr).
#
# Usage:
#   # shellcheck source=SCRIPTDIR/lib-kd-create.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
#
#   id=$(kd_create_id "$KD_BIN" create "title" --type=task -l e2e)
#   id=$(kd_json_id "$captured_kd_create_json")
#
# kd_create_id appends --json to the command, prints the id (nothing on
# failure) and always returns 0, so it drops into the old
# `$(... || echo "")` call sites under set -e. kd's stderr passes through to
# the job log; when no id comes back, kd's stdout goes to stderr as well.

# Guard against multiple sourcing within the same shell.
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_KD_CREATE_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_KD_CREATE_LOADED=1

# kd_json_id — print the .id of a captured `kd create --json` document.
kd_json_id() {
  printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || true
}

# kd_create_id — run `<kd> create ... --json` and print the new bead's id.
kd_create_id() {
  local out rc=0 id
  out=$("$@" --json) || rc=$?
  id=$(kd_json_id "$out")
  if [ -z "$id" ]; then
    printf 'kd_create_id: no id from kd create (rc=%s): %s\n' \
      "$rc" "$(printf '%s' "$out" | tr '\n' ' ' | head -c 400)" >&2
  fi
  printf '%s' "$id"
}
