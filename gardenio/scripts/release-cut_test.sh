#!/usr/bin/env bash
# release-cut_test.sh — Unit tests for release-cut.sh.
#
# `git` is a thin logging wrapper on PATH that delegates every call straight
# to the real binary against a throwaway bare "origin" + working clone (no
# network, no real gardenio repo touched) — so candidate ordering, tag
# ancestry (`merge-base --is-ancestor`), and ref semantics are REAL, not
# reimplemented. It
# also logs every invocation's argv, which is what lets the push-argv test
# assert exactly what was pushed, and can be told to reject a `push
# .../refs/tags/*` once or always, to deterministically exercise the
# tag-already-exists race-retry path without real concurrency.
#
# `gh` is a stub that logs and FAILS: release-cut.sh no longer reads CI at all
# (owner 2026-11-03; the tag's own runs gate the deploy, release-deploy.sh rule
# 4), so any `gh` call is a regression, and the cut tests assert the log is
# empty.
#
# Usage: bash scripts/release-cut_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNDER_TEST="${SCRIPT_DIR}/release-cut.sh"
REAL_GIT="$(command -v git)"

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── repo + stub scaffolding ─────────────────────────────────────────────

make_stubs() {
  local bin="$1"
  mkdir -p "$bin"

  cat > "${bin}/git" <<EOF
#!/usr/bin/env bash
printf 'git %s\n' "\$*" >> "\$GIT_CALL_LOG"
if [ "\$1" = "push" ] && [ "\$2" = "origin" ]; then
  case "\$3" in
    refs/tags/*)
      if [ -n "\${FORCE_PUSH_REJECT_ALWAYS:-}" ] || { [ -n "\${FORCE_PUSH_REJECT_ONCE:-}" ] && [ -f "\${FORCE_PUSH_REJECT_ONCE}" ]; }; then
        [ -n "\${FORCE_PUSH_REJECT_ONCE:-}" ] && rm -f "\${FORCE_PUSH_REJECT_ONCE}"
        echo " ! [rejected]        \${3} -> \${3} (already exists)" >&2
        echo "error: failed to push some refs to 'origin'" >&2
        exit 1
      fi
      ;;
  esac
fi
exec "${REAL_GIT}" "\$@"
EOF
  chmod +x "${bin}/git"

  cat > "${bin}/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >> "$GH_CALL_LOG"
echo "stub gh: release-cut.sh must not call gh" >&2
exit 1
EOF
  chmod +x "${bin}/gh"

  # gb: logs its argv and exits GB_STUB_RC (default 0). The read-only probe
  # `release record --help` is not logged; it lists --no-gardener unless
  # GB_STUB_NO_CUT_OPEN (a gb from before 2026.308.5).
  cat > "${bin}/gb" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "release record --help" ]]; then
  echo "Usage:"
  echo "  gb release record <tag> [flags]"
  echo "      --dry-run           print what would be written; write nothing"
  [[ -z "${GB_STUB_NO_CUT_OPEN:-}" ]] && echo "      --no-gardener       do not bind the gardener of the release the cut opens"
  exit 0
fi
printf 'gb %s\n' "$*" >> "$GB_CALL_LOG"
if [[ -n "${GB_STUB_OLD:-}" && "$*" == *--images-from-tag* ]]; then
  echo "Error: unknown flag: --images-from-tag" >&2
  exit 1
fi
echo "stub gb: recorded"
exit "${GB_STUB_RC:-0}"
EOF
  chmod +x "${bin}/gb"
}

setup_repo() {
  TMP="$(mktemp -d)"
  ORIGIN="${TMP}/origin.git"
  WORK="${TMP}/work"
  "$REAL_GIT" init -q --bare "$ORIGIN"
  "$REAL_GIT" clone -q "$ORIGIN" "$WORK"
  (
    cd "$WORK"
    "$REAL_GIT" config user.email t@t
    "$REAL_GIT" config user.name t
    "$REAL_GIT" commit -q --allow-empty -m init
    "$REAL_GIT" push -q origin HEAD:refs/heads/main
  )
  GH_CALL_LOG="${TMP}/gh-calls.log"
  GIT_CALL_LOG="${TMP}/git-calls.log"
  GB_CALL_LOG="${TMP}/gb-calls.log"
  : > "$GB_CALL_LOG"
  : > "$GH_CALL_LOG"
  : > "$GIT_CALL_LOG"
  STUB_BIN="${TMP}/bin"
  make_stubs "$STUB_BIN"
}

teardown_repo() { rm -rf "$TMP"; }

# commit_on_main <msg> -- commits (empty) on WORK's main and pushes to
# origin, printing the new sha.
commit_on_main() {
  ( cd "$WORK" \
    && "$REAL_GIT" commit -q --allow-empty -m "$1" \
    && "$REAL_GIT" push -q origin HEAD:refs/heads/main \
    && "$REAL_GIT" rev-parse HEAD )
}

# tag_release <tag> <sha> -- pushes an annotated calver tag straight to
# origin (as if a prior release.sh cut it), without leaving a local tag
# behind, so release-cut.sh's own fetch is what populates it.
# add_images_workflow -- commits an images.yml naming a registry to main, as
# release-cut.sh reads IMAGE_PREFIX from the tag it cuts.
add_images_workflow() {
  ( cd "$WORK" \
    && mkdir -p .github/workflows \
    && printf 'env:\n  IMAGE_PREFIX: ecr.example/gardenio\n' > .github/workflows/images.yml \
    && "$REAL_GIT" add .github/workflows/images.yml \
    && "$REAL_GIT" commit -q -m images \
    && "$REAL_GIT" push -q origin HEAD:refs/heads/main )
}

tag_release() {
  local tag="$1" sha="$2"
  ( cd "$WORK" \
    && "$REAL_GIT" tag -a "$tag" "$sha" -m "release $tag" \
    && "$REAL_GIT" push -q origin "refs/tags/${tag}" \
    && "$REAL_GIT" tag -d "$tag" >/dev/null )
}

# run_under_test [args...] -- runs release-cut.sh against $WORK with the
# stubs on PATH; sets OUT and RC. BOAT_ROLE and GARDENIO_RELEASE_OPERATOR are
# taken from RUN_BOAT_ROLE / RUN_RELEASE_OPERATOR (default: unset, i.e. the
# operator's laptop), never from the caller's env: inside an agent pod the
# caller's BOAT_ROLE=crew would trip the role guard in every other test.
run_under_test() {
  set +e
  OUT="$(cd "$WORK" && env -u BOAT_ROLE -u GARDENIO_RELEASE_OPERATOR \
    ${RUN_BOAT_ROLE:+BOAT_ROLE="$RUN_BOAT_ROLE"} \
    ${RUN_RELEASE_OPERATOR:+GARDENIO_RELEASE_OPERATOR="$RUN_RELEASE_OPERATOR"} \
    PATH="${STUB_BIN}:${PATH}" \
    GH_CALL_LOG="$GH_CALL_LOG" GIT_CALL_LOG="$GIT_CALL_LOG" \
    GB_CALL_LOG="$GB_CALL_LOG" GB_STUB_RC="${GB_STUB_RC:-0}" GB_STUB_OLD="${GB_STUB_OLD:-}" \
    GB_STUB_NO_CUT_OPEN="${GB_STUB_NO_CUT_OPEN:-}" \
    FORCE_PUSH_REJECT_ONCE="${FORCE_PUSH_REJECT_ONCE:-}" FORCE_PUSH_REJECT_ALWAYS="${FORCE_PUSH_REJECT_ALWAYS:-}" \
    bash "$UNDER_TEST" "$@" 2>&1)"
  RC=$?
  set -e
}

# ── 1. Nothing to release: newest candidate IS the last release ─────────
D="nothing_to_release_newest_is_release"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
run_under_test
if [[ "$RC" -eq 3 && "$OUT" == *"nothing to release"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
if grep -q '^git push' "$GIT_CALL_LOG"; then
  fail "${D}_no_push" "expected no push when there is nothing to release; log: $(cat "$GIT_CALL_LOG")"
else
  pass "${D}_no_push"
fi
teardown_repo

# ── 2. Nothing to release: the last tag is HEAD after several commits ───
D="nothing_to_release_tag_is_head"
setup_repo
commit_on_main c1 >/dev/null
SHA_C="$(commit_on_main c2)"
tag_release "2020.1.1" "$SHA_C"
run_under_test
if [[ "$RC" -eq 3 && "$OUT" == *"nothing to release"* ]] && ! grep -q '^git push' "$GIT_CALL_LOG"; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 3. HEAD is tagged, never an older commit, and CI is never asked ─────
# Owner 2026-11-03: the cut tags main's head; the tag's own ci.yml, checks.yml
# and images.yml runs decide whether it rolls (release-deploy.sh rule 4).
D="tags_head_without_asking_ci"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
commit_on_main c1 >/dev/null
commit_on_main c2 >/dev/null
SHA_NEW="$(commit_on_main c3)"
run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_NEW}"* && "$OUT" == *"3 first-parent commit(s) on main since 2020.1.1"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT} expected SHA=${SHA_NEW}"
fi
if [ -s "$GH_CALL_LOG" ]; then
  fail "${D}_no_gh" "release-cut.sh called gh: $(cat "$GH_CALL_LOG")"
else
  pass "${D}_no_gh"
fi
teardown_repo

# ── 4. No calver tag at all: HEAD is unreleased ──────────────────────────
D="no_calver_tag_tags_head"
setup_repo
SHA_C="$(commit_on_main c1)"
( cd "$WORK" && "$REAL_GIT" tag -a not-a-calver "$SHA_C" -m x && "$REAL_GIT" push -q origin refs/tags/not-a-calver )
run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 5. Real run pushes EXACTLY `push origin refs/tags/<TAG>`, no branch ─
D="push_argv_is_exactly_tag_ref"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
run_under_test
TAG_LINE="$(printf '%s\n' "$OUT" | sed -n 's/^TAG=//p')"
if [[ "$RC" -ne 0 || -z "$TAG_LINE" ]]; then
  fail "$D" "rc=${RC} out=${OUT}"
else
  PUSH_LINES="$(grep '^git push' "$GIT_CALL_LOG" || true)"
  EXPECTED="git push origin refs/tags/${TAG_LINE}"
  if [[ "$PUSH_LINES" == "$EXPECTED" ]]; then
    pass "$D"
  else
    fail "$D" "push log was [${PUSH_LINES}], expected exactly [${EXPECTED}]"
  fi
  # And the tag really landed on origin, at the chosen sha.
  LANDED="$("$REAL_GIT" --git-dir="$ORIGIN" rev-parse "refs/tags/${TAG_LINE}^{commit}" 2>/dev/null || true)"
  if [[ "$LANDED" != "$SHA_C" ]]; then
    fail "${D}_landed" "expected tag ${TAG_LINE} on origin at ${SHA_C}, got [${LANDED}]"
  else
    pass "${D}_landed"
  fi
fi
teardown_repo

# ── 6. --dry-run never pushes and never creates a local tag ─────────────
D="dry_run_never_pushes"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* && "$OUT" == *"(dry-run)"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
if grep -q '^git push' "$GIT_CALL_LOG"; then
  fail "${D}_no_push_call" "dry-run must not push; log: $(cat "$GIT_CALL_LOG")"
else
  pass "${D}_no_push_call"
fi
teardown_repo

# ── 7. A calver tag main does not contain is not the last release ───────
# A tag cut from a branch says nothing about main: the last release is the
# newest calver tag that is an ancestor of origin/main.
D="off_main_calver_tag_ignored"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
( cd "$WORK" && "$REAL_GIT" checkout -q -b side "$SHA0" && "$REAL_GIT" commit -q --allow-empty -m side \
  && "$REAL_GIT" tag -a 2020.9.9 HEAD -m x && "$REAL_GIT" push -q origin refs/tags/2020.9.9 \
  && "$REAL_GIT" tag -d 2020.9.9 >/dev/null && "$REAL_GIT" checkout -q - )
run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* && "$OUT" == *"since 2020.1.1"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 8. A failed `git fetch` is exit 1, naming the failing command ───────
D="git_fetch_failure_is_hard_error"
setup_repo
( cd "$WORK" && "$REAL_GIT" remote set-url origin "${TMP}/does-not-exist.git" )
run_under_test
if [[ "$RC" -eq 1 && "$OUT" == *"git fetch origin main --tags"* && "$OUT" == *"failed"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 9. Push rejected once (tag-exists race) recovers on the retry ───────
D="push_race_recovers_on_retry"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
FORCE_PUSH_REJECT_ONCE="${TMP}/reject-once"
: > "$FORCE_PUSH_REJECT_ONCE"
run_under_test
FORCE_PUSH_REJECT_ONCE=""
PUSH_ATTEMPTS="$(grep -c '^git push origin refs/tags/' "$GIT_CALL_LOG" || true)"
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* && "$PUSH_ATTEMPTS" -eq 2 ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT} push_attempts=${PUSH_ATTEMPTS}"
fi
teardown_repo

# ── 10. Push rejected on both tries -> bounded retry, exit 1 ────────────
D="push_race_exhausted_exits_1"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
FORCE_PUSH_REJECT_ALWAYS=1
run_under_test
FORCE_PUSH_REJECT_ALWAYS=""
PUSH_ATTEMPTS="$(grep -c '^git push origin refs/tags/' "$GIT_CALL_LOG" || true)"
if [[ "$RC" -eq 1 && "$OUT" == *"git push origin refs/tags/"* && "$PUSH_ATTEMPTS" -eq 2 ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT} push_attempts=${PUSH_ATTEMPTS}"
fi
teardown_repo

# ── 11. An unrecognized flag is rejected (exit 1) ────────────────────────
D="unknown_flag_rejected"
setup_repo
run_under_test --bogus
if [[ "$RC" -eq 1 ]]; then
  pass "$D"
else
  fail "$D" "expected exit 1, got ${RC}"
fi
teardown_repo

# ── 12. Role guard: a crew is refused before anything is fetched or pushed ─
D="crew_role_refused_exit_4"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
RUN_BOAT_ROLE=crew run_under_test
if [[ "$RC" -eq 4 && "$OUT" == *"refused"* && "$OUT" == *"helm_deploy escalation"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
if [ -s "$GIT_CALL_LOG" ] || [ -s "$GH_CALL_LOG" ]; then
  fail "${D}_no_git_or_gh" "expected no git/gh call; git: $(cat "$GIT_CALL_LOG") gh: $(cat "$GH_CALL_LOG")"
else
  pass "${D}_no_git_or_gh"
fi
teardown_repo

# ── 13. Role guard: any non-captain role is refused, even in a role list ──
D="non_captain_role_list_refused"
setup_repo
RUN_BOAT_ROLE="captain,merge-desk" run_under_test
if [[ "$RC" -eq 4 ]]; then
  pass "$D"
else
  fail "$D" "expected exit 4, got ${RC} out=${OUT}"
fi
teardown_repo

# ── 14. Role guard: a crew may still --dry-run (it pushes nothing) ────────
D="crew_dry_run_allowed"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
RUN_BOAT_ROLE=crew run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* ]] && ! grep -q '^git push' "$GIT_CALL_LOG"; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 15. Role guard: the release captain cuts (also inside a role list) ────
for role in release-captain "gardener,release-captain"; do
  D="captain_role_cuts[${role}]"
  setup_repo
  SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
  tag_release "2020.1.1" "$SHA0"
  SHA_C="$(commit_on_main c1)"
  RUN_BOAT_ROLE="$role" run_under_test
  if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* ]] && grep -q '^git push origin refs/tags/' "$GIT_CALL_LOG"; then
    pass "$D"
  else
    fail "$D" "rc=${RC} out=${OUT}"
  fi
  teardown_repo
done

# ── 16. Role guard: the operator override lets a role-tagged shell cut ────
D="operator_override_cuts"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
RUN_BOAT_ROLE=crew RUN_RELEASE_OPERATOR=1 run_under_test
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_C}"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 17. The last tag on a merged side branch: the merge is unreleased ────
# The tag is an ancestor of main but not on its first-parent line; main's
# head (the merge) is still newer than it.
D="tag_on_merged_branch_head_unreleased"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
( cd "$WORK" && "$REAL_GIT" checkout -q -b side "$SHA0" && "$REAL_GIT" commit -q --allow-empty -m side \
  && "$REAL_GIT" tag -a 2020.1.1 HEAD -m x && "$REAL_GIT" push -q origin refs/tags/2020.1.1 \
  && "$REAL_GIT" tag -d 2020.1.1 >/dev/null && "$REAL_GIT" checkout -q - \
  && "$REAL_GIT" merge -q --no-ff --no-edit side && "$REAL_GIT" push -q origin HEAD:refs/heads/main )
SHA_M="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
run_under_test --dry-run
if [[ "$RC" -eq 0 && "$OUT" == *"SHA=${SHA_M}"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT} expected SHA=${SHA_M}"
fi
teardown_repo

# ── 18. --max-scan is gone with the walk it bounded ──────────────────────
D="max_scan_flag_removed"
setup_repo
run_under_test --max-scan 50
if [[ "$RC" -eq 1 && "$OUT" == *"unknown flag: --max-scan"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 21. A cut records the release and backfills; a dry run calls no gb ──
D="cut_records_release"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
add_images_workflow
SHA_C="$(commit_on_main c1)"
run_under_test --dry-run
if [[ -s "$GB_CALL_LOG" ]]; then
  fail "${D}_not_on_dry_run" "dry run called gb: $(cat "$GB_CALL_LOG")"
else
  pass "${D}_not_on_dry_run"
fi
run_under_test
TAG_LINE="$(printf '%s\n' "$OUT" | sed -n 's/^TAG=//p')"
WANT_GB_CALLS="gb release record ${TAG_LINE} --project gardenio
gb release backfill --project gardenio --last 8 --built --images-file .github/images.json --images-from-tag --registry ecr.example/gardenio"
if [[ "$RC" -eq 0 && "$(cat "$GB_CALL_LOG")" == "$WANT_GB_CALLS" ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} gb calls=[$(cat "$GB_CALL_LOG")] out=${OUT}"
fi
teardown_repo

# ── 22. A failed record or backfill is a warning: the tag is pushed, exit 0 ──
D="cut_survives_failed_record"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
GB_STUB_RC=1 run_under_test
if [[ "$RC" -eq 0 && "$OUT" == *"TAG="* && "$OUT" == *"warning: 'gb release record"* \
  && "$OUT" == *"warning: 'gb release backfill --project gardenio --last 8' failed"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} out=${OUT}"
fi
teardown_repo

# ── 23. A gb without --images-from-tag still backfills the records ──────
D="cut_backfills_with_old_gb"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
add_images_workflow
SHA_C="$(commit_on_main c1)"
GB_STUB_OLD=1 run_under_test
LAST_GB_CALL="$(tail -n 1 "$GB_CALL_LOG")"
if [[ "$RC" -eq 0 && "$LAST_GB_CALL" == "gb release backfill --project gardenio --last 8" \
  && "$OUT" == *"this gb cannot mark releases built"* && "$OUT" != *"backfill --project gardenio --last 8' failed"* ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} gb calls=[$(cat "$GB_CALL_LOG")] out=${OUT}"
fi
teardown_repo

# ── 24. No IMAGE_PREFIX at the tag: marked built without image refs ─────
D="cut_marks_built_without_registry"
setup_repo
SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
tag_release "2020.1.1" "$SHA0"
SHA_C="$(commit_on_main c1)"
run_under_test
LAST_GB_CALL="$(tail -n 1 "$GB_CALL_LOG")"
if [[ "$RC" -eq 0 && "$LAST_GB_CALL" == "gb release backfill --project gardenio --last 8 --built" ]]; then
  pass "$D"
else
  fail "$D" "rc=${RC} gb calls=[$(cat "$GB_CALL_LOG")] out=${OUT}"
fi
teardown_repo

# ── 25. A gb that cannot cut onto the open release is refused, exit 5 ──
# gd-6Z9rCUcfqS: its record would mint a fresh release beside the open one
# and bind no gardener, so the cut stops before anything is fetched or tagged.
for args in "" "--dry-run"; do
  D="old_gb_refused_exit_5[${args:-cut}]"
  setup_repo
  SHA0="$(cd "$WORK" && "$REAL_GIT" rev-parse HEAD)"
  tag_release "2020.1.1" "$SHA0"
  commit_on_main c1 >/dev/null
  # shellcheck disable=SC2086 # args is empty or one flag
  GB_STUB_NO_CUT_OPEN=1 run_under_test $args
  if [[ "$RC" -eq 5 && "$OUT" == *"cannot cut onto the open release"* && "$OUT" == *"Update gb"* ]]; then
    pass "$D"
  else
    fail "$D" "rc=${RC} out=${OUT}"
  fi
  if [ -s "$GIT_CALL_LOG" ] || [ -s "$GB_CALL_LOG" ]; then
    fail "${D}_no_git_or_record" "expected no git or gb record call; git: $(cat "$GIT_CALL_LOG") gb: $(cat "$GB_CALL_LOG")"
  else
    pass "${D}_no_git_or_record"
  fi
  teardown_repo
done

echo
echo "release-cut_test: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
