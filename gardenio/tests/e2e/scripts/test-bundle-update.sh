#!/usr/bin/env bash
# test-bundle-update.sh — LIVE beadsapi contract test for `gb bundle update`,
# the bundle CONVERGE verb shipped in MR !6651 (coverage-backfill ka-0000004386,
# epic ka-0000000958 — "E2E coverage gap sprint").
#
# The property under test:
#
#   `gb bundle update <doc-id> --doc <path>` REWRITES an existing doc bead's
#   body IN PLACE — same doc id, same TOC entry, no new doc bead — rather than
#   forking a second copy the way a re-run of `gb bundle add --doc` does. This
#   is the exact defect the verb exists to prevent (ka-0000003568): a gardener
#   who re-ran `add --doc` against an edited plan file silently duplicated the
#   doc in the bundle's table of contents, so the plan stopped reading as a
#   single plan-of-record. `update` is the idempotent converge path.
#
# The hermetic twin of this module is bundle_test.go (drives the cobra command
# against an in-process daemon in `make test`). THIS module is the from-scratch,
# against-a-real-OIDC-Keycloak-gated daemon assertion the backfill task calls
# for: it seeds an origin task + bundle + doc through the REAL `gb` binary
# (authenticating with only BEADS_AUTH_TOKEN, both BEADS_VIEWER_* unset), drives
# the ACTUAL `gb bundle update` binary, and asserts the mutation PERSISTED by
# reading the doc back with `gb bundle doc`.
#
# Scenarios:
#   1. In-place body rewrite persists — seed bundle + doc (body v1), run
#      `gb bundle update --doc` with a v2 body (plus a new headline + title),
#      and assert a fresh `gb bundle doc --json` read surfaces the v2 body,
#      the new headline, and the new title — the mutation LANDED on the daemon.
#   2. Converge, don't fork (child-doc handling) — after the update, the owning
#      bundle's `gb bundle show` still lists EXACTLY ONE doc, and it is the SAME
#      doc id (not a forked duplicate), with the updated title/headline in the
#      TOC. This is the core "no duplicate doc" contract the verb delivers.
#   3. Idempotent no-op — re-running the identical update reports unchanged:true
#      and writes nothing, so a per-cycle converge loop does not spam the feed.
#   4. Inline ↔ out-of-band switch clears the leaving field — `update
#      --content-url` drops the inline content; a following `update --doc`
#      drops the stale content_url, so a doc is never left with both.
#   5. Negative guards — updating a BUNDLE id (not a doc) errors clearly, and an
#      update with no mutating flag errors with "nothing to update".
#
# Self-SKIPs (exit 0) when the daemon is unreachable / no token, so it is inert
# where it cannot run and never reds the suite (ka-0000000847) — a skip is never
# mistaken for a pass.
#
# Idempotency: every seeded bead carries the title prefix bundle-update-e2e-<rand>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-bundle-update.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19837}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PROJECT="${BUNDLE_UPDATE_PROJECT:-gardenio}"
TITLE_PREFIX="bundle-update-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
OPERATOR="${OPERATOR:-oidc:e2e-bundle-update@example.com}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
CREATED_BEADS=()
WORKDIR=""

export KD_ACTOR="${KD_ACTOR:-e2e-test}"
# kd/gb mutations need a non-empty operated_by (ka-0000005772). Seeding uses gb.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-$OPERATOR}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
  [ -n "$WORKDIR" ] && rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup: reach the daemon + load the token ──────────────────────
# If BEADS_HTTP_URL/BEADS_AUTH_TOKEN are already exported (a direct daemon run),
# use them as-is. Otherwise port-forward the in-cluster daemon and read the
# kd-beads-token secret — the same token an agent pod carries as BEADS_AUTH_TOKEN.
setup_daemon() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
      "${BEADS_PF_PORT}:8080" >/tmp/bundle-update-beads-pf.log 2>&1 &
    BEADS_PF_PID=$!
    sleep 3
    export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  fi
  if [ -z "${BEADS_AUTH_TOKEN:-}" ]; then
    # Accept BEADS_TOKEN (other modules' name) as a fallback source.
    if [ -n "${BEADS_TOKEN:-}" ]; then
      export BEADS_AUTH_TOKEN="$BEADS_TOKEN"
    else
      local secret
      secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
      [ -n "$secret" ] && export BEADS_AUTH_TOKEN="$secret"
    fi
  fi
  # kd reads BEADS_HTTP_URL/BEADS_TOKEN; align both names so seeding works.
  export BEADS_TOKEN="${BEADS_TOKEN:-${BEADS_AUTH_TOKEN:-}}"
}

# daemon_reachable — a token-authenticated read must succeed, else SKIP.
daemon_reachable() {
  [ -n "${BEADS_AUTH_TOKEN:-}" ] || return 1
  curl -sf -o /dev/null --max-time 10 \
    -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" \
    "${BEADS_HTTP_URL}/v1/health" 2>/dev/null
}

# gb_scrubbed — run the REAL gb binary with BOTH viewer env vars UNSET, only
# BEADS_AUTH_TOKEN + BEADS_HTTP_URL surviving. This is the crux — bundle
# create/add/update/doc/show must all resolve straight from the daemon (an agent
# pod does not get BEADS_VIEWER_URL injected, ka-0000003798). If any path
# secretly needed the viewer, it would fail here.
gb_scrubbed() {
  env -u BEADS_VIEWER_URL -u BEADS_VIEWER_INTERNAL_URL \
    BEADS_HTTP_URL="$BEADS_HTTP_URL" \
    BEADS_AUTH_TOKEN="$BEADS_AUTH_TOKEN" \
    BEADS_OPERATED_BY="$OPERATOR" \
    "$GB_BIN" "$@"
}

bead_id_from_json() { jq -r '.id // empty' 2>/dev/null; }

# ── Seed an origin task + bundle + a single md doc (body v1) ───────
# Populates ORIGIN/BUNDLE/DOC; appends every created id to CREATED_BEADS. All
# through the REAL gb binary, token-only (viewer env scrubbed).
ORIGIN="" BUNDLE="" DOC=""
DOC_V1_BODY="bundle-update-e2e v1 body ${RANDOM_SUFFIX}"
DOC_V2_BODY=$'bundle-update-e2e v2 body '"${RANDOM_SUFFIX}"$'\nsecond mutated line'
seed_bundle() {
  local tag="${TITLE_PREFIX}-${RANDOM_SUFFIX}"

  # Origin task the bundle parents to (kd, token-only).
  ORIGIN=$(bead_id_from_json < <("$KD_BIN" create "${tag}-origin" --type=task \
      -l "project:${PROJECT}" --no-parent --json 2>/dev/null))
  [ -n "$ORIGIN" ] && CREATED_BEADS+=("$ORIGIN") || return 1

  # Bundle parented to the origin, via the REAL gb binary.
  BUNDLE=$(gb_scrubbed bundle create "$ORIGIN" \
      --title "${tag} bundle" --kind report --json 2>/dev/null | bead_id_from_json)
  [ -n "$BUNDLE" ] && CREATED_BEADS+=("$BUNDLE") || return 1

  # A single inline md doc, body v1, with an initial headline.
  printf '%s\n' "$DOC_V1_BODY" > "${WORKDIR}/doc.md"
  DOC=$(gb_scrubbed bundle add "$BUNDLE" --doc "${WORKDIR}/doc.md" \
      --headline "orig headline ${RANDOM_SUFFIX}" --json 2>/dev/null | bead_id_from_json)
  [ -n "$DOC" ] && CREATED_BEADS+=("$DOC") || return 1

  return 0
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: in-place body rewrite persists to the daemon
# ══════════════════════════════════════════════════════════════════
scenario_update_persists() {
  blue "Scenario 1: gb bundle update rewrites the doc body in place (persisted)"

  # Confirm the seeded body is v1 before we mutate it, so a later v2 read is a
  # genuine change and not a pre-existing state.
  local before
  before=$(gb_scrubbed bundle doc "$DOC" --json 2>&1)
  if ! jq -e . >/dev/null 2>&1 <<<"$before"; then
    fail "gb bundle doc --json (pre-update read)" "not JSON: $(head -3 <<<"$before")"
    return
  fi
  # NOTE: $(...) strips the doc body's single trailing newline, so compare
  # against the bare body variable (no appended \n) — the stored content does
  # carry a trailing newline (printf '%s\n'), it is just eaten by the capture.
  local b0
  b0=$(jq -r '.content' <<<"$before")
  [ "$b0" = "${DOC_V1_BODY}" ] && pass "seeded doc body is v1 (baseline)" \
    || fail "baseline body" "want v1 body, got: $(head -1 <<<"$b0")"

  # Rewrite the body (v2) plus a new headline and title, in one update.
  printf '%s\n' "$DOC_V2_BODY" > "${WORKDIR}/doc.md"
  local upd rc=0
  upd=$(gb_scrubbed bundle update "$DOC" --doc "${WORKDIR}/doc.md" \
      --headline "updated headline ${RANDOM_SUFFIX}" \
      --title "renamed doc ${RANDOM_SUFFIX}" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb bundle update (viewer env unset)" "exited non-zero: $(head -3 <<<"$upd")"
    return
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"$upd"; then
    fail "gb bundle update returns valid JSON" "got: $(head -3 <<<"$upd")"
    return
  fi
  pass "gb bundle update succeeds with only BEADS_AUTH_TOKEN (both BEADS_VIEWER_* unset)"

  # The update reports it actually changed something (not a phantom no-op), and
  # keeps operating on the SAME doc id — no fork.
  local u_id u_unchanged
  u_id=$(jq -r '.id // empty' <<<"$upd")
  u_unchanged=$(jq -r '.unchanged' <<<"$upd")
  [ "$u_id" = "$DOC" ] && pass "update targets the same doc id ($DOC)" \
    || fail "update doc id" "want $DOC, got $u_id"
  [ "$u_unchanged" = "false" ] && pass "update reports unchanged=false (a real mutation)" \
    || fail "update unchanged flag" "want false, got '$u_unchanged'"

  # The mutation must PERSIST: a fresh read surfaces the v2 body + new metadata.
  local after body headline title
  after=$(gb_scrubbed bundle doc "$DOC" --json 2>&1)
  if ! jq -e . >/dev/null 2>&1 <<<"$after"; then
    fail "gb bundle doc --json (post-update read)" "not JSON: $(head -3 <<<"$after")"
    return
  fi
  body=$(jq -r '.content' <<<"$after")
  headline=$(jq -r '.headline' <<<"$after")
  title=$(jq -r '.title' <<<"$after")
  [ "$body" = "${DOC_V2_BODY}" ] \
    && pass "doc body persisted as v2 (re-read surfaces the mutation)" \
    || fail "persisted body" "want v2 body, got: $(head -3 <<<"$body")"
  [ "$headline" = "updated headline ${RANDOM_SUFFIX}" ] \
    && pass "doc headline persisted (updated headline)" \
    || fail "persisted headline" "want 'updated headline …', got '$headline'"
  [ "$title" = "renamed doc ${RANDOM_SUFFIX}" ] \
    && pass "doc title persisted (renamed doc)" \
    || fail "persisted title" "want 'renamed doc …', got '$title'"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2: converge, don't fork — the bundle still holds ONE doc
# ══════════════════════════════════════════════════════════════════
# The whole reason `update` exists: re-running `add --doc` forks a duplicate doc
# into the TOC, but `update` rewrites in place. After scenario 1's update the
# owning bundle must still list EXACTLY ONE doc, and it must be the SAME doc id
# with the updated title/headline — no forked second copy (ka-0000003568).
scenario_no_fork() {
  blue "Scenario 2: bundle still holds exactly one doc after update (no fork)"
  local show
  show=$(gb_scrubbed bundle show "$BUNDLE" --json 2>&1)
  if ! jq -e . >/dev/null 2>&1 <<<"$show"; then
    fail "gb bundle show --json" "not JSON: $(head -3 <<<"$show")"
    return
  fi
  local ndocs id0 title0 headline0
  ndocs=$(jq '.docs | length' <<<"$show")
  id0=$(jq -r '.docs[0].ID // empty' <<<"$show")
  title0=$(jq -r '.docs[0].Title // empty' <<<"$show")
  headline0=$(jq -r '.docs[0].Headline // empty' <<<"$show")
  [ "$ndocs" = "1" ] && pass "bundle TOC has exactly 1 doc (update did not fork)" \
    || fail "no fork" "want 1 doc, got $ndocs (a re-run add would have made 2)"
  [ "$id0" = "$DOC" ] && pass "the one doc is the SAME id ($DOC), not a duplicate" \
    || fail "same doc id" "want $DOC, got $id0"
  [ "$title0" = "renamed doc ${RANDOM_SUFFIX}" ] \
    && pass "TOC entry shows the updated title" \
    || fail "toc title" "want 'renamed doc …', got '$title0'"
  [ "$headline0" = "updated headline ${RANDOM_SUFFIX}" ] \
    && pass "TOC entry shows the updated headline" \
    || fail "toc headline" "want 'updated headline …', got '$headline0'"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: idempotent no-op — re-update with same body writes nothing
# ══════════════════════════════════════════════════════════════════
scenario_idempotent() {
  blue "Scenario 3: re-running the identical update is an idempotent no-op"
  # The file already holds the v2 body from scenario 1 — pushing it again must
  # report unchanged and write nothing (a per-cycle converge loop stays quiet).
  local out rc=0
  out=$(gb_scrubbed bundle update "$DOC" --doc "${WORKDIR}/doc.md" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "idempotent re-update" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  local unchanged
  unchanged=$(jq -r '.unchanged' <<<"$out" 2>/dev/null)
  [ "$unchanged" = "true" ] \
    && pass "identical re-update reports unchanged=true (no write)" \
    || fail "idempotent no-op" "want unchanged=true, got '$unchanged'"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4: inline ↔ out-of-band switch clears the field it leaves
# ══════════════════════════════════════════════════════════════════
# A doc must never carry BOTH an inline body and a content_url — the viewer
# would render a mix of the old inline body and the new URL. `update` drops the
# field it is leaving behind: --content-url clears content, --doc clears
# content_url. Assert both directions on a re-read.
scenario_inline_outofband_switch() {
  blue "Scenario 4: update switches inline ↔ out-of-band, clearing the leaving field"

  local url="https://example.com/bundle-update-e2e-${RANDOM_SUFFIX}.md"
  local out rc=0
  out=$(gb_scrubbed bundle update "$DOC" --content-url "$url" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "update → content-url" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  local read1 c1 u1
  read1=$(gb_scrubbed bundle doc "$DOC" --json 2>&1)
  c1=$(jq -r '.content' <<<"$read1" 2>/dev/null)
  u1=$(jq -r '.content_url' <<<"$read1" 2>/dev/null)
  { [ -z "$c1" ] && [ "$u1" = "$url" ]; } \
    && pass "switch to --content-url dropped the inline body (content cleared, url set)" \
    || fail "content-url switch" "want empty content + url=$url, got content='$(head -1 <<<"$c1")' url='$u1'"

  # Switch back to inline: the stale content_url must clear.
  printf '%s\n' "back to inline ${RANDOM_SUFFIX}" > "${WORKDIR}/doc.md"
  rc=0
  out=$(gb_scrubbed bundle update "$DOC" --doc "${WORKDIR}/doc.md" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "update → --doc (back to inline)" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  local read2 c2 u2
  read2=$(gb_scrubbed bundle doc "$DOC" --json 2>&1)
  c2=$(jq -r '.content' <<<"$read2" 2>/dev/null)
  u2=$(jq -r '.content_url' <<<"$read2" 2>/dev/null)
  { [ "$c2" = "back to inline ${RANDOM_SUFFIX}" ] && [ -z "$u2" ]; } \
    && pass "switch back to --doc dropped the stale content_url (url cleared, content set)" \
    || fail "inline switch-back" "want inline body + empty url, got content='$(head -1 <<<"$c2")' url='$u2'"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 5: negative guards — bundle-id target + nothing-to-update
# ══════════════════════════════════════════════════════════════════
scenario_negative_guards() {
  blue "Scenario 5: update refuses a bundle-id target and a no-flag call"

  # Updating a BUNDLE id (not a doc) must fail clearly — the guard that keeps a
  # `content` field off an unrelated bead.
  local out rc=0
  out=$(gb_scrubbed bundle update "$BUNDLE" --doc "${WORKDIR}/doc.md" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ] && grep -qi "is a bundle, not a doc" <<<"$out"; then
    pass "update on a bundle id errors clearly (guards against mis-targeted write)"
  else
    fail "bundle-id guard" "want non-zero + 'is a bundle, not a doc', got rc=$rc: $(head -2 <<<"$out")"
  fi

  # An update with no mutating flag must fail with "nothing to update".
  rc=0
  out=$(gb_scrubbed bundle update "$DOC" --json 2>&1) || rc=$?
  if [ "$rc" -ne 0 ] && grep -qi "nothing to update" <<<"$out"; then
    pass "update with no flags errors with 'nothing to update'"
  else
    fail "no-flag guard" "want non-zero + 'nothing to update', got rc=$rc: $(head -2 <<<"$out")"
  fi
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "╔══════════════════════════════════════════════════════╗"
  blue "║   gb bundle update e2e (ka-0000004386, MR !6651)      ║"
  blue "╚══════════════════════════════════════════════════════╝"

  WORKDIR=$(mktemp -d)
  setup_daemon
  if ! daemon_reachable; then
    skip "bundle-update" "daemon unreachable or no BEADS_AUTH_TOKEN at ${BEADS_HTTP_URL:-unset}"
    echo ""; blue "Passed: $PASS  Skipped: $SKIP"
    return 0
  fi

  if ! seed_bundle; then
    fail "seed origin+bundle+doc" "gb bundle create/add failed (origin=$ORIGIN bundle=$BUNDLE doc=$DOC)"
    echo ""; red "Failed: $FAIL"; return 1
  fi
  dim "  seeded origin $ORIGIN → bundle $BUNDLE → doc $DOC"

  scenario_update_persists
  scenario_no_fork
  scenario_idempotent
  scenario_inline_outofband_switch
  scenario_negative_guards

  echo ""
  blue "────────────────────────────────────────────────────"
  green "Passed: $PASS"
  [ "$SKIP" -gt 0 ] && yellow "Skipped: $SKIP"
  if [ "$FAIL" -gt 0 ]; then
    red "Failed: $FAIL"
    for e in "${ERRORS[@]}"; do red "  ✗ $e"; done
  fi
  blue "────────────────────────────────────────────────────"
  [ "$FAIL" -eq 0 ]
}

main
