#!/usr/bin/env bash
# release_test.sh — Unit tests for release.sh version computation (ka-0000005733).
#
# Regression guard for the 16h release-lane wedge: release.sh used to run a bare
# `git fetch -q --tags origin` (old line 166) to populate local tags before
# computing the next calver. On the release-owner's shallow, --no-tags,
# single-branch clone (controller initclone.go), that fetch negotiates the
# ENTIRE tag pack and can hang indefinitely in index-pack, killing the captain
# mid-cut (escalation ka-0000003116). The fix reads tag NAMES via
# `git ls-remote` (no objects, no pack, timeout-bounded) instead.
#
# These tests stand up a throwaway bare "origin" with real release tags and a
# SHALLOW --no-tags working clone (the exact shape that hung), then source
# release.sh in library mode (RELEASE_SH_LIB_ONLY=1) and assert:
#   - remote_release_tags lists every release tag from the remote, sorted,
#     even though the local clone has NONE.
#   - compute_version returns the correct next sequence for today.
#   - neither reads pull the tag pack into the shallow clone (no index-pack,
#     no hang) — verified by asserting the local tag set stays empty.
#
# Also guards the preflight-hang cluster (ka-0000001167, ka-0000003629,
# ka-0000004640, ka-0000004157): release.sh's two blocking `rwx run ... --wait`
# dispatches (the mirror gate + the preflight) used to poll a stalled RWX run
# FOREVER while holding the single-flight lock. rwx_run_bounded now wraps them
# in `timeout`. Using a mock rwx injected via RWX_BIN, these tests assert that
# success/failure exit codes pass through and that a hanging run is aborted
# with rc 124 FAST instead of hanging.
#
# Also guards the orphaned-tag defect (ka-0000001299): release 2026.200.15 was
# cut on a STALE base and published with a NON-ATOMIC push, so its tag landed
# off-main. The fresh-base guard (assert_on_fresh_main / try_fast_forward_main)
# and the atomic push are pinned here — fresh-base pass/refuse, fast-forward
# recover/refuse, and atomic-push no-orphan vs the old bare push (which the tests
# confirm DOES still orphan, so the fix test is not vacuous).
#
# Real git, no network, no mocking of git itself — mirrors release-lock_test.sh.
#
# Usage:  bash scripts/release_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RELEASE_SH="${SCRIPT_DIR}/release.sh"

PASS=0
FAIL=0
fail_test() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }
pass_test() { echo "PASS: $1"; PASS=$((PASS + 1)); }

YEAR="$(TZ=UTC date +%Y)"
DOY="$(TZ=UTC date +%-j)"
PREFIX="${YEAR}.${DOY}"

# Build a bare origin holding `main` + the given release tags, and a SHALLOW
# --no-tags clone of it (the release-owner pod's clone shape). Echoes the
# work-dir path.
setup_repos() {
    TMP="$(mktemp -d)"
    ORIGIN="${TMP}/origin.git"
    SEED="${TMP}/seed"
    WORK="${TMP}/work"
    git init -q --bare "$ORIGIN"
    git init -q "$SEED"
    (
        cd "$SEED" || exit 1
        git config user.email t@t; git config user.name t
        git commit -q --allow-empty -m init
        git remote add origin "$ORIGIN"
        git push -q origin HEAD:refs/heads/main
        for tag in "$@"; do
            git tag "$tag"
        done
        git push -q origin --tags
    )
    # The release-owner clone: shallow, single-branch, NO tags — exactly what
    # controller initclone.go creates and what made `git fetch --tags` hang.
    # file:// (not a local path) so --depth is honoured and the clone is a
    # genuine shallow repo, matching the real pod.
    git clone -q --depth 1 --single-branch --no-tags -b main "file://${ORIGIN}" "$WORK"
}
teardown() { rm -rf "$TMP"; }

# Source release.sh's functions into a subshell rooted at $WORK, with the
# remote pointed at our throwaway origin. LIB_ONLY stops before the release
# flow. Runs the given expression and echoes its stdout.
in_work() {
    # shellcheck source=scripts/release.sh
    ( cd "$WORK" \
        && RELEASE_SH_LIB_ONLY=1 RELEASE_LOCK_REMOTE=origin \
           source "$RELEASE_SH" \
        && eval "$1" )
}

# ── Test 1: remote tags are listed from a --no-tags clone (the hang case) ──
test_remote_tags_from_shallow_clone() {
    setup_repos "${PREFIX}.1" "${PREFIX}.2" "${PREFIX}.3" "2026.99.7"
    # Sanity: the local clone genuinely has NO tags (proves --no-tags took).
    local localtags
    localtags="$(cd "$WORK" && git tag -l)"
    if [[ -n "$localtags" ]]; then
        fail_test remote_tags_from_shallow_clone "clone unexpectedly has local tags: $localtags"; teardown; return
    fi
    local out
    out="$(in_work 'remote_release_tags | tr "\n" " "')"
    # Expect all four, numerically sorted (99 < 194-ish today prefix ordering
    # is by field, so 2026.99.7 sorts before today's higher DOY).
    case "$out" in
        *"${PREFIX}.1"*"${PREFIX}.2"*"${PREFIX}.3"*)
            pass_test remote_tags_from_shallow_clone ;;
        *)
            fail_test remote_tags_from_shallow_clone "missing/unsorted tags: '$out'" ;;
    esac
    teardown
}

# ── Test 2: compute_version returns next sequence for today ────────────
test_compute_version_next_seq() {
    setup_repos "${PREFIX}.1" "${PREFIX}.2" "${PREFIX}.3"
    local out
    out="$(in_work 'compute_version')"
    if [[ "$out" == "${PREFIX}.4" ]]; then
        pass_test compute_version_next_seq
    else
        fail_test compute_version_next_seq "expected ${PREFIX}.4, got '$out'"
    fi
    teardown
}

# ── Test 3: first cut of the day when no tag has today's prefix ────────
test_compute_version_first_of_day() {
    # Only an old-prefix tag exists; today's sequence must start at 1.
    setup_repos "2026.1.9"
    local out
    out="$(in_work 'compute_version')"
    if [[ "$out" == "${PREFIX}.1" ]]; then
        pass_test compute_version_first_of_day
    else
        fail_test compute_version_first_of_day "expected ${PREFIX}.1, got '$out'"
    fi
    teardown
}

# ── Test 4: seq is numeric, not lexical (…​.9 -> .10, not .2) ──────────
test_compute_version_numeric_seq() {
    setup_repos "${PREFIX}.8" "${PREFIX}.9" "${PREFIX}.10"
    local out
    out="$(in_work 'compute_version')"
    if [[ "$out" == "${PREFIX}.11" ]]; then
        pass_test compute_version_numeric_seq
    else
        fail_test compute_version_numeric_seq "expected ${PREFIX}.11 (numeric), got '$out'"
    fi
    teardown
}

# ── Test 5: computing the version does NOT pull tags into the shallow clone ──
# This is the anti-hang assertion: the old code fetched the whole tag pack;
# the new code must leave the --no-tags clone tagless.
test_no_tag_pack_pulled() {
    setup_repos "${PREFIX}.1" "${PREFIX}.2"
    in_work 'compute_version' >/dev/null
    local localtags
    localtags="$(cd "$WORK" && git tag -l)"
    if [[ -z "$localtags" ]]; then
        pass_test no_tag_pack_pulled
    else
        fail_test no_tag_pack_pulled "compute_version pulled tags locally: '$localtags'"
    fi
    teardown
}

# ── Test 6: verify_sha_on_remote accepts a SHA that IS on origin ───────
# The RWX preflight/mirror runs clone by HEAD's SHA; the guard must pass when
# HEAD is pushed. gitlab serves any commit reachable from an advertised ref by
# SHA, so a pushed tip must verify true.
test_verify_sha_present() {
    setup_repos "${PREFIX}.1"
    local sha
    sha="$(cd "$WORK" && git rev-parse HEAD)"  # tip == origin's main tip
    if in_work "verify_sha_on_remote $sha"; then
        pass_test verify_sha_present
    else
        fail_test verify_sha_present "pushed HEAD sha '$sha' reported NOT on remote"
    fi
    teardown
}

# ── Test 7: verify_sha_on_remote rejects a LOCAL-only, unpushed SHA ─────
# This is the ka-0000003172 wedge: a commit that origin has never seen. The
# guard must return non-zero so release.sh fails fast instead of dispatching an
# RWX clone that would die with 'not our ref' on every retry.
test_verify_sha_unpushed() {
    setup_repos "${PREFIX}.1"
    local bad
    bad="$(cd "$WORK" \
        && git -c user.email=t@t -c user.name=t commit -q --allow-empty -m "local unpushed" \
        && git rev-parse HEAD)"
    if in_work "verify_sha_on_remote $bad"; then
        fail_test verify_sha_unpushed "unpushed sha '$bad' reported as ON remote (guard would not fire)"
    else
        pass_test verify_sha_unpushed
    fi
    teardown
}

# ── rwx_run_bounded: bound a blocking `rwx run --wait` with a timeout ──────
# Source release.sh in library mode with an injected mock `rwx` (RWX_BIN) and a
# short RELEASE_RWX_RUN_TIMEOUT, then invoke rwx_run_bounded and echo its rc.
# No git, no network — this is pure control-flow around `timeout`.
run_bounded_rc() {
    # $1 = path to mock rwx binary, $2 = timeout seconds, rest = passthrough args
    local mock="$1" tmo="$2"; shift 2
    (
        # export (not a command-prefix) so the values survive past `source`:
        # a `VAR=x source f` prefix is dropped once source returns.
        export RELEASE_SH_LIB_ONLY=1 RWX_BIN="$mock" RELEASE_RWX_RUN_TIMEOUT="$tmo"
        # shellcheck source=scripts/release.sh
        source "$RELEASE_SH" >/dev/null 2>&1
        # `|| rc=$?` shields the non-zero return from the `set -e` that
        # release.sh turns on when sourced — otherwise the subshell would exit
        # before echoing the code on the failure/hang paths.
        local rc=0
        rwx_run_bounded "$@" >/dev/null 2>&1 || rc=$?
        echo "$rc"
    )
}

# Write a throwaway mock `rwx` binary. $1=dir, $2=behavior:
#   ok    -> exit 0 immediately
#   fail  -> exit 1 immediately (genuine RWX failure, e.g. --fail-fast)
#   hang  -> sleep far longer than the test timeout (stalled in_progress run)
make_mock_rwx() {
    local behavior="$2" path="$1/rwx"
    case "$behavior" in
        ok)   printf '#!/usr/bin/env bash\nexit 0\n' > "$path" ;;
        fail) printf '#!/usr/bin/env bash\nexit 1\n' > "$path" ;;
        hang) printf '#!/usr/bin/env bash\nsleep 30\n' > "$path" ;;
    esac
    chmod +x "$path"
    echo "$path"
}

# ── Test 8: a completing rwx run passes its exit code through (success) ──
test_rwx_bounded_success() {
    local d; d="$(mktemp -d)"
    local mock; mock="$(make_mock_rwx "$d" ok)"
    local rc; rc="$(run_bounded_rc "$mock" 5 .rwx/ci.yml --wait)"
    if [[ "$rc" == "0" ]]; then
        pass_test rwx_bounded_success
    else
        fail_test rwx_bounded_success "expected rc 0, got '$rc'"
    fi
    rm -rf "$d"
}

# ── Test 9: a genuine RWX failure propagates (non-124, non-zero) ────────
test_rwx_bounded_failure_propagates() {
    local d; d="$(mktemp -d)"
    local mock; mock="$(make_mock_rwx "$d" fail)"
    local rc; rc="$(run_bounded_rc "$mock" 5 .rwx/ci.yml --wait)"
    if [[ "$rc" == "1" ]]; then
        pass_test rwx_bounded_failure_propagates
    else
        fail_test rwx_bounded_failure_propagates "expected rc 1 (real failure), got '$rc'"
    fi
    rm -rf "$d"
}

# ── Test 10: a STALLED run is aborted with rc 124, not hung (the fix) ────
# The mock sleeps 30s; a 1s timeout must reap it and return 124 quickly. This
# is the direct regression guard for the preflight-hang cluster: without the
# timeout wrapper this call would block for the full 30s (proxy for infinite).
test_rwx_bounded_hang_is_aborted() {
    local d; d="$(mktemp -d)"
    local mock; mock="$(make_mock_rwx "$d" hang)"
    local start end rc
    start="$(date +%s)"
    rc="$(run_bounded_rc "$mock" 1 .rwx/ci.yml --wait)"
    end="$(date +%s)"
    if [[ "$rc" != "124" ]]; then
        fail_test rwx_bounded_hang_is_aborted "expected rc 124 (timed out), got '$rc'"
    elif (( end - start >= 15 )); then
        fail_test rwx_bounded_hang_is_aborted "took $((end - start))s — timeout did not abort the hang"
    else
        pass_test rwx_bounded_hang_is_aborted
    fi
    rm -rf "$d"
}

# ── ka-0000001299: fresh-base guard + atomic-push regression tests ────────────
#
# Release 2026.200.15 orphaned itself: it was cut on a STALE base (origin/main
# had advanced under it) and published with a NON-ATOMIC `git push origin main
# <TAG>` — the main ref was rejected as non-fast-forward but the tag ref STILL
# landed, so the tag pointed at a commit unreachable from main. These tests pin
# both halves of the fix:
#   - assert_on_fresh_main refuses a cut whose HEAD is not the live origin/main
#     tip, and passes when it is; try_fast_forward_main recovers a behind (but
#     not diverged) HEAD and safely refuses a divergent one.
#   - `git push --atomic main <TAG>` from a stale clone lands NEITHER ref
#     (no orphaned tag), whereas the old bare push orphans the tag.

# Advance origin/main by one commit from a scratch clone (simulates a peer /
# MR merge landing on main after our shallow WORK clone was taken). Leaves WORK
# untouched and now behind origin/main.
advance_origin_main() {
    local peer="${TMP}/peer"
    git clone -q --single-branch -b main "file://${ORIGIN}" "$peer"
    (
        cd "$peer" || exit 1
        git config user.email t@t; git config user.name t
        git commit -q --allow-empty -m "peer advance (release .14 / MR merge)"
        git push -q origin main
    )
    rm -rf "$peer"
}

# ── Test 11: fresh-base guard PASSES when HEAD is the live origin/main tip ──
test_fresh_base_guard_pass() {
    setup_repos "${PREFIX}.1"
    if in_work 'assert_on_fresh_main "$(git rev-parse HEAD)" >/dev/null 2>&1 && echo OK' | grep -q OK; then
        pass_test fresh_base_guard_pass
    else
        fail_test fresh_base_guard_pass "guard rejected an up-to-date HEAD"
    fi
    teardown
}

# ── Test 12: fresh-base guard FAILS when main advanced under the cut ──────
# This is the 2026.200.15 stale-base condition — the guard must refuse.
test_fresh_base_guard_fails_on_stale() {
    setup_repos "${PREFIX}.1"
    advance_origin_main   # origin/main now ahead of WORK's HEAD
    if in_work 'if assert_on_fresh_main "$(git rev-parse HEAD)" >/dev/null 2>&1; then echo PASSED; else echo REFUSED; fi' | grep -q REFUSED; then
        pass_test fresh_base_guard_fails_on_stale
    else
        fail_test fresh_base_guard_fails_on_stale "guard allowed a stale-base cut (would re-orphan)"
    fi
    teardown
}

# ── Test 13: try_fast_forward_main RECOVERS a behind-but-not-diverged HEAD ──
# The unattended release-owner falls behind while its own preflight runs. A clean
# fast-forward must recover it (move HEAD to the live tip) so the guard passes —
# no going dark on an ordinary race. Exercised on the shallow --no-tags clone.
test_fast_forward_recovers_behind() {
    setup_repos "${PREFIX}.1"
    advance_origin_main   # origin/main ahead; WORK behind but NOT diverged
    if in_work 'try_fast_forward_main >/dev/null 2>&1; if assert_on_fresh_main "$(git rev-parse HEAD)" >/dev/null 2>&1; then echo SYNCED; else echo STUCK; fi' | grep -q SYNCED; then
        pass_test fast_forward_recovers_behind
    else
        fail_test fast_forward_recovers_behind "fast-forward did not sync a behind HEAD to the live tip"
    fi
    teardown
}

# ── Test 14: try_fast_forward_main REFUSES a divergent HEAD, non-destructively ─
# A local commit not on origin (true divergence) must NOT be fast-forwarded: the
# guard must still refuse, and the local commit must survive (nothing clobbered).
test_fast_forward_refuses_divergent() {
    setup_repos "${PREFIX}.1"
    advance_origin_main   # origin advances
    local localsha
    localsha="$(cd "$WORK" && git config user.email t@t && git config user.name t \
        && git commit -q --allow-empty -m "local divergent" && git rev-parse HEAD)"
    local out
    out="$(in_work 'if try_fast_forward_main >/dev/null 2>&1; then echo FF_OK; else echo FF_REFUSED; fi; echo "HEAD=$(git rev-parse HEAD)"; if assert_on_fresh_main "$(git rev-parse HEAD)" >/dev/null 2>&1; then echo GUARD_PASS; else echo GUARD_REFUSE; fi')"
    if echo "$out" | grep -q FF_REFUSED \
        && echo "$out" | grep -q "HEAD=${localsha}" \
        && echo "$out" | grep -q GUARD_REFUSE; then
        pass_test fast_forward_refuses_divergent
    else
        fail_test fast_forward_refuses_divergent "diverged HEAD mishandled: '$out'"
    fi
    teardown
}

# ── Test 14b: assert_head_on_main PASSES on main ─────────────────────────
# The release-owner pod clones `-b main`, so the guard must be a no-op there.
test_head_on_main_guard_pass() {
    setup_repos "${PREFIX}.1"
    if in_work 'if assert_head_on_main >/dev/null 2>&1; then echo ON_MAIN; else echo REFUSED; fi' | grep -q ON_MAIN; then
        pass_test head_on_main_guard_pass
    else
        fail_test head_on_main_guard_pass "guard refused a cut with main checked out"
    fi
    teardown
}

# ── Test 14c: assert_head_on_main REFUSES a feature-branch cut, accurately ─
# The ka-0000002455 bug: on a feature branch the fresh-base fast-forward moves
# HEAD, not local `main`, so the atomic push of the stale `main` ref is rejected
# and the old code mis-blamed a busy lane + suggested `reset --hard origin/main`.
# The guard must refuse BEFORE the fast-forward and print the ACCURATE remedy —
# naming the branch, NOT the busy-lane claim, and NOT `reset --hard`.
test_head_on_main_guard_refuses_feature_branch() {
    setup_repos "${PREFIX}.1"
    ( cd "$WORK" && git config user.email t@t && git config user.name t \
        && git checkout -q -b feat/some-work )
    local out
    out="$(in_work 'if assert_head_on_main 2>&1; then echo RET_OK; else echo RET_REFUSED; fi')"
    if echo "$out" | grep -q RET_REFUSED \
        && echo "$out" | grep -q "feat/some-work" \
        && echo "$out" | grep -qi "not a busy release lane\|NOT a busy release lane" \
        && echo "$out" | grep -q "git checkout main" \
        && ! echo "$out" | grep -q "advancing faster"; then
        pass_test head_on_main_guard_refuses_feature_branch
    else
        fail_test head_on_main_guard_refuses_feature_branch "wrong/incomplete diagnosis: '$out'"
    fi
    teardown
}

# ── Test 14d: assert_head_on_main REFUSES a detached HEAD ─────────────────
# Detached HEAD has no branch, so pushing the local `main` ref would push a ref
# unrelated to the cut. The guard must refuse (and not crash on the empty name).
test_head_on_main_guard_refuses_detached() {
    setup_repos "${PREFIX}.1"
    ( cd "$WORK" && git checkout -q --detach HEAD )
    local out
    out="$(in_work 'if assert_head_on_main 2>&1; then echo RET_OK; else echo RET_REFUSED; fi')"
    if echo "$out" | grep -q RET_REFUSED && echo "$out" | grep -q "detached HEAD"; then
        pass_test head_on_main_guard_refuses_detached
    else
        fail_test head_on_main_guard_refuses_detached "detached HEAD mishandled: '$out'"
    fi
    teardown
}

# ── Test 15: --atomic push from a stale clone orphans NOTHING ─────────────
# The core fix. Cut a tag locally on a stale base, then push --atomic main+tag.
# The main update is non-fast-forward, so the atomic transaction rejects BOTH
# refs — the tag must NOT appear on origin.
test_atomic_push_no_orphan() {
    setup_repos "${PREFIX}.1"
    local tag="${PREFIX}.2"
    advance_origin_main   # origin/main advances; WORK is now behind
    (
        cd "$WORK" || exit 1
        git config user.email t@t; git config user.name t
        git commit -q --allow-empty -m "chore: release ${tag} (stale base)"
        git tag "$tag"
        git push --atomic origin main "$tag" >/dev/null 2>&1 || true
    )
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${tag}" >/dev/null 2>&1; then
        fail_test atomic_push_no_orphan "atomic push left tag ${tag} on origin (orphaned)"
    else
        pass_test atomic_push_no_orphan
    fi
    teardown
}

# ── Test 16: the OLD bare push DOES orphan — proves the hole is real ──────
# Guards against a future refactor that silently reverts to a non-atomic push:
# if this ever stops orphaning, the atomic-push test above would be vacuous.
test_bare_push_orphans_tag() {
    setup_repos "${PREFIX}.1"
    local tag="${PREFIX}.3"
    advance_origin_main
    (
        cd "$WORK" || exit 1
        git config user.email t@t; git config user.name t
        git commit -q --allow-empty -m "chore: release ${tag} (stale base)"
        git tag "$tag"
        git push origin main "$tag" >/dev/null 2>&1 || true
    )
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${tag}" >/dev/null 2>&1 \
        && ! git --git-dir="$ORIGIN" merge-base --is-ancestor "refs/tags/${tag}" main 2>/dev/null; then
        pass_test bare_push_orphans_tag
    else
        fail_test bare_push_orphans_tag "bare push did not orphan the tag — repro no longer valid"
    fi
    teardown
}

# ── Test 17: --atomic push from a FRESH base lands BOTH refs, tag on main ──
# The happy path must still work: when HEAD is the live tip, the atomic push
# advances main and lands the tag, and the tag is reachable from main.
test_atomic_push_happy_path() {
    setup_repos "${PREFIX}.1"
    local tag="${PREFIX}.2"
    (
        cd "$WORK" || exit 1
        git config user.email t@t; git config user.name t
        git commit -q --allow-empty -m "chore: release ${tag} (fresh base)"
        git tag "$tag"
        git push --atomic origin main "$tag" >/dev/null 2>&1
    )
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${tag}" >/dev/null 2>&1 \
        && git --git-dir="$ORIGIN" merge-base --is-ancestor "refs/tags/${tag}" main 2>/dev/null; then
        pass_test atomic_push_happy_path
    else
        fail_test atomic_push_happy_path "fresh-base atomic push did not land a reachable tag"
    fi
    teardown
}

# ── ka-0000007981: bounded automatic cut+push retry loop ──────────────────────
#
# A cut is fresh-base guard → mirror/preflight (minutes of RWX) → bump/tag →
# atomic push. On a busy main a peer MR merges into origin/main during the
# preflight window, so the atomic push is correctly rejected non-fast-forward
# when it finally runs. release.sh USED to exit here printing `git reset --hard
# origin/main && ./scripts/release.sh`, so the release-owner captain re-ran the
# WHOLE script by hand — repeating the full preflight each time. A CED-audited
# cycle (session WIN0y8RR51) ran ./scripts/release.sh 8× and git push 5× fighting
# the same non-ff rejection. The loop now recovers that race in ONE invocation,
# bounded, re-running the expensive gates only when the base actually changed.
#
# These tests run the FULL script (not lib mode) end-to-end against a real bare
# origin with a real Chart.yaml + prod-real.tfvars, all RWX/lock/breaker gates
# skipped, and a WORK-side pre-push hook that deterministically simulates the
# raced main. Real git, no git mocking — same philosophy as the atomic tests.

# Build a bare origin whose `main` carries the files release.sh bumps
# (Chart.yaml + prod-real.tfvars), plus a shallow --no-tags WORK clone with a git
# identity so the FULL script can commit. Echoes nothing; sets ORIGIN/SEED/WORK.
setup_repos_with_chart() {
    TMP="$(mktemp -d)"
    ORIGIN="${TMP}/origin.git"
    SEED="${TMP}/seed"
    WORK="${TMP}/work"
    git init -q --bare "$ORIGIN"
    git init -q "$SEED"
    (
        cd "$SEED" || exit 1
        git config user.email t@t; git config user.name t
        mkdir -p gardenio/helm/gardenio gardenio/terraform/composition/canonical-prod
        printf 'apiVersion: v2\nname: gardenio\nversion: 0.0.0\nappVersion: 0.0.0\n' \
            > gardenio/helm/gardenio/Chart.yaml
        printf 'gardenio_chart_version = "0.0.0"\ngardenio_image_tag     = "0.0.0"\n' \
            > gardenio/terraform/composition/canonical-prod/prod-real.tfvars
        git add -A
        git commit -q -m init
        git remote add origin "$ORIGIN"
        git push -q origin HEAD:refs/heads/main
        for tag in "$@"; do git tag "$tag"; done
        git push -q origin --tags
    )
    git clone -q --depth 1 --single-branch --no-tags -b main "file://${ORIGIN}" "$WORK"
    ( cd "$WORK" && git config user.email t@t && git config user.name t )
}

# Install a WORK-side pre-push hook that fires per push attempt. $1 = mode:
#   advance-once  : the FIRST push advances origin/main by one commit (so that
#                   push loses the non-ff race), later pushes proceed clean →
#                   exercises re-sync + a real base change on the retry.
#   advance-always: EVERY push advances origin/main first → the cut can never win
#                   → exercises the retry cap.
#   reject-once   : the FIRST push is aborted (hook exit 1) WITHOUT advancing
#                   origin → the base is UNCHANGED on the retry, exercising the
#                   "reuse the validated gates, no redundant RWX re-run" branch.
# The one-shot modes gate on a sentinel file under WORK/.git so state survives
# across the two push attempts of a single script run.
install_prepush_hook() {
    local mode="$1" hook="${WORK}/.git/hooks/pre-push" sentinel="${WORK}/.git/.raced"
    mkdir -p "${WORK}/.git/hooks"
    cat > "$hook" <<EOS
#!/usr/bin/env bash
mode="${mode}"
sentinel="${sentinel}"
origin="${ORIGIN}"
advance() {
    local peer; peer="\$(mktemp -d)"
    git clone -q --single-branch -b main "file://\${origin}" "\${peer}/p" || return 0
    ( cd "\${peer}/p" && git config user.email t@t && git config user.name t \
        && git commit -q --allow-empty -m "peer advance" && git push -q origin main ) || true
    rm -rf "\${peer}"
}
case "\$mode" in
    advance-always) advance ;;
    advance-once)   [[ -e "\$sentinel" ]] || { touch "\$sentinel"; advance; } ;;
    reject-once)    [[ -e "\$sentinel" ]] || { touch "\$sentinel"; exit 1; } ;;
esac
exit 0
EOS
    chmod +x "$hook"
}

# Run the full release.sh from WORK with all RWX/lock/breaker gates skipped and
# origin as the release remote. Any extra args are per-run env assignments (e.g.
# RELEASE_PUSH_MAX_ATTEMPTS=1) applied via `env` — they CANNOT be a bare command
# prefix, because assignment-words are recognised at parse time, before "$@"
# expands, so an expanded VAR=val would be run as a command, not an assignment.
# Captures combined stdout+stderr into $RUN_OUT and the exit code into $RUN_RC
# (both set in the CALLER's shell — do NOT invoke this inside a command
# substitution, or those assignments die in the subshell).
run_full_release() {
    RUN_OUT="$( cd "$WORK" && RELEASE_LOCK_REMOTE=origin env "$@" \
        bash "$RELEASE_SH" --skip-lock --skip-breaker --skip-quiet-hours --skip-mirror --skip-preflight 2>&1 )"
    RUN_RC=$?
}

# ── Test 19: a lost push race is recovered automatically in ONE invocation ──
# main advances during the (skipped) preflight window; the first atomic push is
# rejected non-ff. The loop must re-sync onto the new tip and re-cut WITHOUT a
# second manual invocation, landing a tag reachable from main.
test_push_retry_recovers_lost_race() {
    setup_repos_with_chart "${PREFIX}.1"
    install_prepush_hook advance-once
    local RUN_OUT="" RUN_RC=0; run_full_release
    # compute_version yields PREFIX.2; the retry re-cuts it after the race.
    if [[ $RUN_RC -ne 0 ]]; then
        fail_test push_retry_recovers_lost_race "script exited ${RUN_RC}, expected 0 (recovered): $RUN_OUT"; teardown; return
    fi
    if ! echo "$RUN_OUT" | grep -q "attempt 2/3"; then
        fail_test push_retry_recovers_lost_race "no automatic retry happened (missing 'attempt 2/3'): $RUN_OUT"; teardown; return
    fi
    # The tag must be published AND reachable from main (no orphan, ka-0000001299).
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${PREFIX}.2" >/dev/null 2>&1 \
        && git --git-dir="$ORIGIN" merge-base --is-ancestor "refs/tags/${PREFIX}.2" main 2>/dev/null; then
        pass_test push_retry_recovers_lost_race
    else
        fail_test push_retry_recovers_lost_race "retry did not land a reachable ${PREFIX}.2 tag"
    fi
    teardown
}

# ── Test 20: a retry whose base did NOT change reuses the validated gates ──
# The first push is aborted without advancing origin (transient), so the retry's
# base equals the one already validated. The script must print the "reusing …
# no redundant RWX re-run" line — the efficiency core of ka-0000007981 — and
# still land the tag.
test_push_retry_reuses_unchanged_base() {
    setup_repos_with_chart "${PREFIX}.1"
    install_prepush_hook reject-once
    local RUN_OUT="" RUN_RC=0; run_full_release
    if [[ $RUN_RC -ne 0 ]]; then
        fail_test push_retry_reuses_unchanged_base "script exited ${RUN_RC}, expected 0: $RUN_OUT"; teardown; return
    fi
    if ! echo "$RUN_OUT" | grep -qi "Base unchanged since last validated cut"; then
        fail_test push_retry_reuses_unchanged_base "did not reuse validation on an unchanged base: $RUN_OUT"; teardown; return
    fi
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${PREFIX}.2" >/dev/null 2>&1; then
        pass_test push_retry_reuses_unchanged_base
    else
        fail_test push_retry_reuses_unchanged_base "unchanged-base retry did not land the ${PREFIX}.2 tag"
    fi
    teardown
}

# ── Test 21: the retry loop is BOUNDED — it stops, it never storms ─────────
# origin advances on every push, so the cut can never win. With
# RELEASE_PUSH_MAX_ATTEMPTS=1 the loop must give up after one attempt with the
# loud "still rejected after N attempts" message and a non-zero exit — and,
# because every attempt was atomic, leave NO orphaned tag on origin.
test_push_retry_is_bounded() {
    setup_repos_with_chart "${PREFIX}.1"
    install_prepush_hook advance-always
    local RUN_OUT="" RUN_RC=0; run_full_release RELEASE_PUSH_MAX_ATTEMPTS=1
    local ok=1
    [[ $RUN_RC -ne 0 ]] || { ok=0; echo "  expected non-zero exit on exhausted retries, got 0"; }
    echo "$RUN_OUT" | grep -q "still rejected after 1 attempts" || { ok=0; echo "  missing bounded-give-up message"; }
    if git --git-dir="$ORIGIN" rev-parse "refs/tags/${PREFIX}.2" >/dev/null 2>&1; then
        ok=0; echo "  atomic retry left an orphaned ${PREFIX}.2 tag on origin"
    fi
    if [[ $ok -eq 1 ]]; then
        pass_test push_retry_is_bounded
    else
        fail_test push_retry_is_bounded "bounded-retry contract violated"
    fi
    teardown
}

# ── Test 18: a STALLED preflight must NOT suggest --skip-preflight ────────
# Regression guard for the false-alarm bypass (ka-0000007506; operator
# directive Matt Baker 2026-07-22 "don't skip preflight tho, fix"). A stalled
# RWX run (rc 124) is NOT a red build — release.sh must branch on rc 124 and
# emit "did not complete / do NOT use --skip-preflight" guidance, never the
# generic "PREFLIGHT FAILED … --skip-preflight to bypass" line that nudged a
# captain into bypassing preflight on a run that had actually SUCCEEDED. This
# is a source-level assertion: the top-level preflight block runs only after
# the lock/version machinery, so it is not reachable in RELEASE_SH_LIB_ONLY
# mode — we pin the control-flow + message contract against the script text.
test_preflight_stall_no_skip_suggestion() {
    local pf_block
    # Extract the preflight block: from its header to the closing "Preflight passed".
    # The block is emitted inside attempt_cut_and_push (ka-0000007981 moved the
    # linear cut tail into a function so the bounded push-retry loop can re-invoke
    # it), so its header is indented — anchor on the comment text, whitespace-lax.
    pf_block="$(awk '/# ── Preflight: catch build\/lint\/test failures/{f=1} f{print} /=== Preflight passed ===/{if(f)exit}' "$RELEASE_SH")"
    local ok=1
    # Consider only executable lines — strip shell comments so the assertions
    # below can't be fooled by the explanatory comments in the branch itself.
    local pf_code
    pf_code="$(echo "$pf_block" | grep -v '^[[:space:]]*#')"
    # (a) the block must branch on the rc-124 (timeout/stall) path.
    echo "$pf_code" | grep -q 'pf_rc -eq 124' || { ok=0; echo "  missing rc-124 branch"; }
    # (b) the stall path must tell the user NOT to skip preflight.
    echo "$pf_code" | grep -qi 'do NOT use --skip-preflight' || { ok=0; echo "  stall path does not warn against --skip-preflight"; }
    # (c) no echoed "PREFLIGHT FAILED" line may advertise --skip-preflight as a bypass.
    if echo "$pf_code" | grep -i 'PREFLIGHT FAILED' | grep -qi 'skip-preflight'; then
        ok=0; echo "  'PREFLIGHT FAILED' line still advertises --skip-preflight bypass"
    fi
    if [[ $ok -eq 1 ]]; then
        pass_test preflight_stall_no_skip_suggestion
    else
        fail_test preflight_stall_no_skip_suggestion "preflight stall/skip message contract violated"
    fi
}

# ── Test 19: a RED preflight makes the WHOLE script exit NON-ZERO ─────────
# Regression guard for gd-E2N1Vmp5Or: release.sh printed "PREFLIGHT FAILED"
# and then exited 0, so anything checking $? — a wrapper script, a Makefile
# chain, or an agent running `./scripts/release.sh && <next step>` — concluded
# the release SUCCEEDED and proceeded to steps that assume a tag exists (bump a
# pin, trigger a deploy, close a bead, report "released"). The exit code is the
# contract; a wrong one is a silent lie.
#
# Unlike test 18 (a source-level message assertion) this drives the FULL cut
# flow end-to-end: a real throwaway origin + shallow clone, the single-flight
# lock held against it (so the EXIT-trap release-lock cleanup runs — the exact
# path in the filed incident where "[release-lock] released" printed and the
# code still exited 0), and a mock `rwx` that fails like a red build (rc 1).
# Asserts the process exits non-zero AND that no tag was pushed.
test_preflight_failure_exits_nonzero() {
    setup_repos "${PREFIX}.1"
    local mockdir; mockdir="$(mktemp -d)"
    local rwx="${mockdir}/rwx"
    printf '#!/usr/bin/env bash\nexit 1\n' > "$rwx"; chmod +x "$rwx"
    local rc=0
    # Full run: keep the lock ON (matches the incident's trap path); skip only
    # the gates that need external state (mirror/breaker/quiet-hours) and don't
    # push. Preflight runs against the failing mock rwx and must fail the cut.
    ( cd "$WORK" \
        && RELEASE_LOCK_REMOTE=origin RWX_BIN="$rwx" \
           bash "$RELEASE_SH" --skip-mirror --skip-breaker --skip-quiet-hours --no-push \
    ) >/dev/null 2>&1 || rc=$?
    local ok=1
    if [[ $rc -eq 0 ]]; then
        ok=0
        echo "  release.sh exited 0 after PREFLIGHT FAILED — a caller checking \$? wrongly believes a release was cut"
    fi
    # Belt-and-suspenders: a refused cut must not have pushed a new tag either.
    local pushed; pushed="$(git ls-remote --tags "$ORIGIN" "refs/tags/${PREFIX}.2" 2>/dev/null)"
    if [[ -n "$pushed" ]]; then
        ok=0; echo "  a tag (${PREFIX}.2) was pushed despite the preflight failure"
    fi
    if [[ $ok -eq 1 ]]; then
        pass_test preflight_failure_exits_nonzero
    else
        fail_test preflight_failure_exits_nonzero "red preflight must exit non-zero and cut nothing (rc=$rc)"
    fi
    rm -rf "$mockdir"
    teardown
}

# ── ka-0000007280: deploy-failure circuit-breaker tests ───────────────────────
#
# The Step-4.0 breaker (doc ka-0000000327) was prose-only — the LLM captain was
# told to suppress cuts while the last tag was stuck at a broken deploy AND a
# release-blocker held, but nothing in the cut path enforced it, so it kept
# cutting hourly tags (2026.204.7 -> 204.17) through the 2026-07-23 drought.
# release.sh now enforces the conjunction MECHANICALLY. These tests source the
# breaker helpers in library mode with mock `glab`/`kd` binaries (GLAB_BIN /
# KD_BIN, mirroring the RWX_BIN injection) and pin the truth table:
#   golden tag                → DO NOT suppress (a is false)
#   red-rc + open blocker      → SUPPRESS (both true — the drought case)
#   red-rc + NO blocker        → DO NOT suppress (b is false; Step 3/4 governs)
#   read error (either probe)  → DO NOT suppress (FAIL OPEN — never wedge the lane)
# plus the label-vs-shape blocker match and the glab golden/404 classification.

# Write a throwaway mock `glab` binary. $1=dir, $2=behavior:
#   golden  -> `api projects/.../releases/<tag>` prints release JSON (rc 0)
#   notfound-> prints a 404 body (rc 1) — an un-promoted red-rc tag
#   error   -> prints nothing, rc 1 — an unreadable network/auth blip
make_mock_glab() {
    local behavior="$2" path="$1/glab"
    case "$behavior" in
        golden)   printf '#!/usr/bin/env bash\nprintf %%s "{\\"tag_name\\":\\"2026.204.6\\",\\"released_at\\":\\"2026-07-23T05:56:40Z\\"}"\nexit 0\n' > "$path" ;;
        notfound) printf '#!/usr/bin/env bash\nprintf %%s "{\\"message\\":\\"404 Not Found\\"}"\nexit 1\n' > "$path" ;;
        error)    printf '#!/usr/bin/env bash\nexit 1\n' > "$path" ;;
    esac
    chmod +x "$path"
    echo "$path"
}

# Write a throwaway mock `kd` binary. $1=dir, $2=behavior:
#   label   -> the release-pipeline-broken label query returns one bead
#   shape   -> label query empty; escalation query returns a title-matched drought bead
#   none    -> both queries return an empty array (no open blocker)
#   error   -> exits non-zero (kd read failure)
# The mock dispatches on the args: a `-l release-pipeline-broken` invocation is
# the label probe; a `--type=escalation` invocation is the shape probe.
make_mock_kd() {
    local behavior="$2" path="$1/kd"
    case "$behavior" in
        label)
            cat > "$path" <<'EOS'
#!/usr/bin/env bash
for a in "$@"; do [[ "$a" == "release-pipeline-broken" ]] && { printf '%s' '[{"id":"ka-0000001996","title":"deploy wedged"}]'; exit 0; }; done
printf '%s' '[]'; exit 0
EOS
            ;;
        shape)
            cat > "$path" <<'EOS'
#!/usr/bin/env bash
is_esc=false
for a in "$@"; do [[ "$a" == "--type=escalation" ]] && is_esc=true; done
if $is_esc; then printf '%s' '[{"id":"ka-0000001012","title":"Clear wedged prod terraform apply so releases can advance prod past 2026.204.6"}]'; else printf '%s' '[]'; fi
exit 0
EOS
            ;;
        notrelease)
            # label query empty; escalation query returns ONLY an unrelated open
            # escalation whose title merely contains "terraform apply" (the gVisor
            # canary-node escalation ka-0000000361). This must NOT match the
            # release-blocker shape (ka-0000007546) — otherwise it freezes the
            # release train even though no release is actually blocked.
            cat > "$path" <<'EOS'
#!/usr/bin/env bash
is_esc=false
for a in "$@"; do [[ "$a" == "--type=escalation" ]] && is_esc=true; done
if $is_esc; then printf '%s' '[{"id":"ka-0000000361","title":"S1 canary#16: provision m6a.xlarge gVisor node (gvisor_pool floor 6000->12000 MiB), targeted terraform apply on prod-gardenio-eks"}]'; else printf '%s' '[]'; fi
exit 0
EOS
            ;;
        none)
            printf '#!/usr/bin/env bash\nprintf %%s "[]"\nexit 0\n' > "$path" ;;
        error)
            printf '#!/usr/bin/env bash\nexit 1\n' > "$path" ;;
    esac
    chmod +x "$path"
    echo "$path"
}

# Source release.sh in library mode with mock glab/kd injected, then run the
# given breaker expression and echo "<rc>|<stdout>".
breaker_eval() {
    local glab_mock="$1" kd_mock="$2" expr="$3"
    (
        export RELEASE_SH_LIB_ONLY=1 GLAB_BIN="$glab_mock" KD_BIN="$kd_mock" \
               RELEASE_GLAB_PROJECT="proj" RELEASE_GIT_REMOTE_TIMEOUT=5
        # shellcheck source=scripts/release.sh
        source "$RELEASE_SH" >/dev/null 2>&1
        local out rc
        out="$(eval "$expr")"; rc=$?
        printf '%s|%s' "$rc" "$out"
    )
}

# ── Test B1: golden latest tag → breaker DOES NOT suppress (a is false) ──
test_breaker_golden_no_suppress() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" golden)"
    local k; k="$(make_mock_kd "$d" label)"   # blocker OPEN, but (a) is false
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.6')"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test breaker_golden_no_suppress
    else
        fail_test breaker_golden_no_suppress "suppressed a golden tag (a must be false): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B2: red-rc + open blocker → breaker SUPPRESSES (the drought case) ──
test_breaker_redrc_blocker_suppresses() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" notfound)"  # (a) un-promoted red-rc
    local k; k="$(make_mock_kd "$d" label)"        # (b) open blocker
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" == "0" && "$r" == *"ka-0000001996"* ]]; then
        pass_test breaker_redrc_blocker_suppresses
    else
        fail_test breaker_redrc_blocker_suppresses "did not suppress a red-rc+blocker cut (the 204 drought bug): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B3: red-rc + NO blocker → DO NOT suppress (b is false) ──────────
# Step 3/4 governs a red pipeline that has no matching release-blocker.
test_breaker_redrc_no_blocker_no_suppress() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" notfound)"
    local k; k="$(make_mock_kd "$d" none)"
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test breaker_redrc_no_blocker_no_suppress
    else
        fail_test breaker_redrc_no_blocker_no_suppress "suppressed with no open blocker (b must be false): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B4: glab read error → FAIL OPEN (do not suppress) ──────────────
# A transient network/auth blip on the release-state read must never wedge the
# lane — suppression requires a POSITIVE red-rc signal, never an absence.
test_breaker_glab_error_fails_open() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" error)"
    local k; k="$(make_mock_kd "$d" label)"
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test breaker_glab_error_fails_open
    else
        fail_test breaker_glab_error_fails_open "suppressed on an unreadable release state (must fail open): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B5: kd read error → FAIL OPEN (do not suppress) ────────────────
test_breaker_kd_error_fails_open() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" notfound)"  # (a) positively red-rc
    local k; k="$(make_mock_kd "$d" error)"        # blocker probe errors
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test breaker_kd_error_fails_open
    else
        fail_test breaker_kd_error_fails_open "suppressed on an unreadable blocker probe (must fail open): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B6: blocker match by SHAPE (title), not just label ─────────────
# The 204 drought escalation ka-0000001012 carried only project:gardenio — a
# label-only match would miss it. breaker_open_release_blocker must fall back to
# a title/shape match over open escalations.
test_breaker_blocker_shape_match() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" notfound)"
    local k; k="$(make_mock_kd "$d" shape)"   # label empty, escalation title matches
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" == "0" && "$r" == *"ka-0000001012"* ]]; then
        pass_test breaker_blocker_shape_match
    else
        fail_test breaker_blocker_shape_match "shape-only (label-less) escalation not matched: '$r'"
    fi
    rm -rf "$d"
}

# ── Test B6b: unrelated "terraform apply" escalation must NOT match ─────
# Regression for ka-0000007546: after the real 204 drought blockers closed, the
# breaker kept suppressing because an unrelated open escalation (gVisor canary
# node, "targeted terraform apply on prod-gardenio-eks") matched the over-broad
# `terraform (wedged|apply)` shape term. The shape must name a broken RELEASE
# path, not any bare apply — so this red-rc + only-unrelated-escalation case
# must NOT suppress.
test_breaker_unrelated_apply_no_suppress() {
    local d; d="$(mktemp -d)"
    local g; g="$(make_mock_glab "$d" notfound)"     # (a) un-promoted red-rc
    local k; k="$(make_mock_kd "$d" notrelease)"      # (b) only an unrelated apply escalation
    local r; r="$(breaker_eval "$g" "$k" 'breaker_should_suppress 2026.204.17')"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test breaker_unrelated_apply_no_suppress
    else
        fail_test breaker_unrelated_apply_no_suppress "suppressed on an unrelated 'terraform apply' escalation (ka-0000007546): '$r'"
    fi
    rm -rf "$d"
}

# ── Test B7: glab golden/404 classification is correct ──────────────────
test_breaker_tag_is_golden_classification() {
    local d; d="$(mktemp -d)"
    local gg; gg="$(make_mock_glab "$d" golden)"
    local gn; gn="$(make_mock_glab "$d/n" 2>/dev/null || true)"
    mkdir -p "$d/n"; gn="$(make_mock_glab "$d/n" notfound)"
    local ge; mkdir -p "$d/e"; ge="$(make_mock_glab "$d/e" error)"
    local rg rn re
    rg="$(breaker_eval "$gg" "$gg" 'breaker_tag_is_golden 2026.204.6; echo $?')"
    rn="$(breaker_eval "$gn" "$gn" 'breaker_tag_is_golden 2026.204.17; echo $?')"
    re="$(breaker_eval "$ge" "$ge" 'breaker_tag_is_golden 2026.204.17; echo $?')"
    # breaker_eval echoes "<rc>|<stdout>"; the helper's own `echo $?` is in stdout.
    if [[ "${rg#*|}" == "0" && "${rn#*|}" == "1" && "${re#*|}" == "2" ]]; then
        pass_test breaker_tag_is_golden_classification
    else
        fail_test breaker_tag_is_golden_classification "golden=0/404=1/error=2 expected, got g='${rg#*|}' n='${rn#*|}' e='${re#*|}'"
    fi
    rm -rf "$d"
}

# ── Release signing (ka-0000005321, F4) ────────────────────────────────
# configure_release_signing resolves an SSH signing key from RELEASE_SIGNING_KEY
# / RELEASE_SIGNING_KEY_B64 and sets SIGN_COMMITS/SIGN_GIT_ARGS/SIGN_STATUS. It
# is FAIL-OPEN: any missing/encrypted/malformed key leaves the cut unsigned
# rather than aborting the release lane. These tests source release.sh in
# library mode (no repo/network needed) and drive the function directly.

# Evaluate configure_release_signing with the given `KEY=VAL ...` env prefix and
# echo "<SIGN_COMMITS>|<SIGN_STATUS>|<SIGN_GIT_ARGS...>".
sign_eval() {
    local env_prefix="$1"
    ( RELEASE_SH_LIB_ONLY=1 source "$RELEASE_SH" \
        && eval "export $env_prefix" 2>/dev/null \
        && configure_release_signing \
        && printf '%s|%s|%s' "$SIGN_COMMITS" "$SIGN_STATUS" "${SIGN_GIT_ARGS[*]}" )
}

# A real unencrypted ed25519 private key on disk => the cut signs.
test_sign_valid_key_signs() {
    local d; d="$(mktemp -d)"
    ssh-keygen -t ed25519 -f "$d/k" -N "" -q -C "release@test"
    local r; r="$(sign_eval "RELEASE_SIGNING_KEY=$d/k")"
    if [[ "${r%%|*}" == "true" && "$r" == *"gpg.format=ssh"* && "$r" == *"user.signingkey=$d/k"* ]]; then
        pass_test sign_valid_key_signs
    else
        fail_test sign_valid_key_signs "expected signed with ssh args, got '$r'"
    fi
    rm -rf "$d"
}

# No key configured => unsigned, but SIGN_COMMITS=false (fail-open, not error).
test_sign_no_key_unsigned() {
    local r; r="$(sign_eval "PATH=$PATH")"   # no RELEASE_SIGNING_KEY*
    if [[ "${r%%|*}" == "false" && "$r" == *"no signing key configured"* ]]; then
        pass_test sign_no_key_unsigned
    else
        fail_test sign_no_key_unsigned "expected unsigned/no-key, got '$r'"
    fi
}

# Configured path does not exist => fail-open unsigned.
test_sign_missing_key_fails_open() {
    local r; r="$(sign_eval "RELEASE_SIGNING_KEY=/nonexistent/key")"
    if [[ "${r%%|*}" == "false" && "$r" == *"not found"* ]]; then
        pass_test sign_missing_key_fails_open
    else
        fail_test sign_missing_key_fails_open "expected unsigned/not-found, got '$r'"
    fi
}

# Encrypted key (passphrase-protected) => fail-open unsigned, and must not hang
# (ssh-keygen -y reads /dev/null so it fails fast instead of prompting).
test_sign_encrypted_key_fails_open() {
    local d; d="$(mktemp -d)"
    ssh-keygen -t ed25519 -f "$d/k" -N "secret" -q -C "enc@test"
    local r; r="$(timeout 20 bash -c "$(declare -f sign_eval); RELEASE_SH='$RELEASE_SH'; sign_eval 'RELEASE_SIGNING_KEY=$d/k'")"
    if [[ "${r%%|*}" == "false" && "$r" == *"unreadable, encrypted"* ]]; then
        pass_test sign_encrypted_key_fails_open
    else
        fail_test sign_encrypted_key_fails_open "expected unsigned/encrypted, got '$r'"
    fi
    rm -rf "$d"
}

# RELEASE_SIGNING_KEY_B64 (base64 of a real private key) is materialised + used.
test_sign_b64_key_signs() {
    local d; d="$(mktemp -d)"
    ssh-keygen -t ed25519 -f "$d/k" -N "" -q -C "b64@test"
    local b64; b64="$(base64 -w0 < "$d/k" 2>/dev/null || base64 < "$d/k" | tr -d '\n')"
    local r; r="$(sign_eval "RELEASE_SIGNING_KEY_B64=$b64")"
    if [[ "${r%%|*}" == "true" && "$r" == *"gpg.format=ssh"* ]]; then
        pass_test sign_b64_key_signs
    else
        fail_test sign_b64_key_signs "expected signed from B64 key, got '$r'"
    fi
    rm -rf "$d"
}

# Invalid base64 in RELEASE_SIGNING_KEY_B64 => fail-open unsigned.
test_sign_b64_invalid_fails_open() {
    local r; r="$(sign_eval "RELEASE_SIGNING_KEY_B64=@@@not-base64@@@")"
    if [[ "${r%%|*}" == "false" ]]; then
        pass_test sign_b64_invalid_fails_open
    else
        fail_test sign_b64_invalid_fails_open "expected unsigned on bad base64, got '$r'"
    fi
}

# An UNSIGNED cut leaves SIGN_GIT_ARGS empty. macOS ships bash 3.2, where
# expanding an empty array as "${arr[@]}" under `set -u` aborts with an unbound
# variable — deterministically killing every unsigned cut from a Mac mid-bump,
# and invisibly to bash 4.4+ CI (ka-0000005650). Assert both git call sites use
# the portable ${arr[@]+"${arr[@]}"} guard, and that the guard yields ZERO words
# on an empty array yet preserves the elements when populated.
test_sign_empty_args_portable_expansion() {
    # (1) Neither the commit nor the tag call may use the bare, 3.2-unsafe form.
    local bare
    bare="$(grep -nE 'git[[:space:]]+"\$\{SIGN_GIT_ARGS\[@\]\}"' "$RELEASE_SH" || true)"
    if [[ -n "$bare" ]]; then
        fail_test sign_empty_args_portable_expansion \
            "bare \"\${SIGN_GIT_ARGS[@]}\" expansion (bash 3.2 set -u unbound) at: $bare"
        return
    fi
    local guarded
    guarded="$(grep -cE 'git \$\{SIGN_GIT_ARGS\[@\]\+"\$\{SIGN_GIT_ARGS\[@\]\}"\} (commit|tag)' "$RELEASE_SH")"
    if [[ "$guarded" -ne 2 ]]; then
        fail_test sign_empty_args_portable_expansion \
            "expected 2 guarded git commit/tag call sites, found $guarded"
        return
    fi
    # (2) Behavioural: the guard is a no-op on empty and lossless when populated,
    # both under `set -u`.
    local empty_n full
    empty_n="$(set -u; SIGN_GIT_ARGS=(); printf '%s\n' "${SIGN_GIT_ARGS[@]+"${SIGN_GIT_ARGS[@]}"}" | grep -c . || true)"
    full="$(set -u; SIGN_GIT_ARGS=(-c gpg.format=ssh -c "user.signingkey=/k"); \
        printf '<%s>' "${SIGN_GIT_ARGS[@]+"${SIGN_GIT_ARGS[@]}"}")"
    if [[ "$empty_n" -eq 0 && "$full" == "<-c><gpg.format=ssh><-c><user.signingkey=/k>" ]]; then
        pass_test sign_empty_args_portable_expansion
    else
        fail_test sign_empty_args_portable_expansion \
            "guard misbehaved: empty words=$empty_n full='$full'"
    fi
}

# ── Release quiet-hours gate (ka-0000004693) ───────────────────────────
# Source release.sh in lib mode and evaluate quiet_hours_should_suppress with a
# pinned Pacific "now" (HOUR DOW) via RELEASE_QUIET_HOURS_NOW, so the test is
# deterministic regardless of the wall clock. Echoes "<rc>|<stdout>".
quiet_eval() {
    local now="$1"
    (
        export RELEASE_SH_LIB_ONLY=1 RELEASE_QUIET_HOURS_NOW="$now"
        # shellcheck source=scripts/release.sh
        source "$RELEASE_SH" >/dev/null 2>&1
        local out rc
        out="$(quiet_hours_should_suppress)"; rc=$?
        printf '%s|%s' "$rc" "$out"
    )
}

# Q1: Thu 10:00 PT (business hours, weekday) → SUPPRESS.
test_quiet_hours_business_hours_suppresses() {
    local r; r="$(quiet_eval "10 4")"
    if [[ "${r%%|*}" == "0" && "$r" == *"quiet hours"* ]]; then
        pass_test quiet_hours_business_hours_suppresses
    else
        fail_test quiet_hours_business_hours_suppresses "did not suppress a weekday business-hours cut: '$r'"
    fi
}

# Q2: Thu 06:00 PT (before 07:00 open) → proceed.
test_quiet_hours_before_open_proceeds() {
    local r; r="$(quiet_eval "6 4")"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test quiet_hours_before_open_proceeds
    else
        fail_test quiet_hours_before_open_proceeds "suppressed before the 07:00 open: '$r'"
    fi
}

# Q3: Thu 18:00 PT (close edge — window is [07:00,18:00)) → proceed.
test_quiet_hours_close_edge_proceeds() {
    local r; r="$(quiet_eval "18 4")"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test quiet_hours_close_edge_proceeds
    else
        fail_test quiet_hours_close_edge_proceeds "suppressed at the 18:00 close edge (must be exclusive): '$r'"
    fi
}

# Q4: Thu 17:00 PT (last blocked hour) → SUPPRESS.
test_quiet_hours_last_blocked_hour_suppresses() {
    local r; r="$(quiet_eval "17 4")"
    if [[ "${r%%|*}" == "0" ]]; then
        pass_test quiet_hours_last_blocked_hour_suppresses
    else
        fail_test quiet_hours_last_blocked_hour_suppresses "did not suppress 17:00 (last business hour): '$r'"
    fi
}

# Q5: Sat 10:00 PT (weekend, dow=6) → proceed. Weekends release freely.
test_quiet_hours_saturday_proceeds() {
    local r; r="$(quiet_eval "10 6")"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test quiet_hours_saturday_proceeds
    else
        fail_test quiet_hours_saturday_proceeds "suppressed on a Saturday (weekends release freely): '$r'"
    fi
}

# Q6: Sun 10:00 PT (weekend, dow=7 per `date +%u`) → proceed. Guards the
# off-by-one where a 0=Sunday assumption would wrongly treat Sunday as in-scope.
test_quiet_hours_sunday_proceeds() {
    local r; r="$(quiet_eval "10 7")"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test quiet_hours_sunday_proceeds
    else
        fail_test quiet_hours_sunday_proceeds "suppressed on a Sunday (dow=7): '$r'"
    fi
}

# Q7: garbage clock → FAIL OPEN (proceed). A tooling blip must never wedge the
# lane — same discipline as the deploy breaker.
test_quiet_hours_bad_clock_fails_open() {
    local r; r="$(quiet_eval "xx yy")"
    if [[ "${r%%|*}" != "0" ]]; then
        pass_test quiet_hours_bad_clock_fails_open
    else
        fail_test quiet_hours_bad_clock_fails_open "suppressed on an unparseable clock (must fail open): '$r'"
    fi
}

# Q8: the self-contained POSIX TZ string resolves US Pacific with CORRECT DST,
# independent of whether tzdata is installed. This is the whole reason the gate
# uses a POSIX string rather than the IANA name `America/Los_Angeles`: glibc
# parses `PST8PDT,M3.2.0,M11.1.0` from the string itself, so it works even in a
# tzdata-less shell (agent + CI pods) where the IANA name silently falls back to
# UTC. We assert the environment-INDEPENDENT fact — the POSIX zone yields the
# correct Pacific hour on BOTH sides of the DST boundary (PDT=UTC-7 in summer,
# PST=UTC-8 in winter). (We deliberately do NOT assert that the IANA name yields
# UTC: some RWX build images DO ship tzdata, so that fact is host-specific and
# not something to gate CI on — only the POSIX-string correctness matters.)
test_quiet_hours_posix_tz_resolves_without_tzdata() {
    local posix_tz='PST8PDT,M3.2.0,M11.1.0'
    # Summer instant (PDT, UTC-7): 2026-07-02T13:46:00Z → 06:46 Pacific.
    local summer; summer="$(TZ="$posix_tz" date -d "2026-07-02T13:46:00Z" '+%-H' 2>/dev/null)"
    # Winter instant (PST, UTC-8): 2025-12-29T09:20:00Z → 01:20 Pacific.
    local winter; winter="$(TZ="$posix_tz" date -d "2025-12-29T09:20:00Z" '+%-H' 2>/dev/null)"
    if [[ -z "$summer" || -z "$winter" ]]; then
        fail_test quiet_hours_posix_tz_resolves_without_tzdata "date arithmetic unavailable"
        return
    fi
    if [[ "$summer" == "6" && "$winter" == "1" ]]; then
        pass_test quiet_hours_posix_tz_resolves_without_tzdata
    else
        fail_test quiet_hours_posix_tz_resolves_without_tzdata \
            "POSIX TZ DST wrong: summer_hour=$summer (want 6, PDT=UTC-7), winter_hour=$winter (want 1, PST=UTC-8)"
    fi
}

# ── Timeout-binary resolution (ka-0000000605) ──────────────────────────
# release.sh bounds every remote read (the deploy-failure breaker, the mirror
# gate, the fresh-base/version reads) with `timeout "$GIT_REMOTE_TIMEOUT" ...`.
# On a host WITHOUT GNU coreutils (an operator Mac), the `timeout` binary is
# absent: those wraps exit 127, the helpers `return 2`, and EVERY guard silently
# FAILS OPEN — the breaker and mirror gate are skipped and the cut can compute a
# COLLIDING version while still offering to proceed (measured live 2026-08-26,
# ka-0000007503). release.sh now resolves the binary ONCE at startup and ABORTS
# (fail CLOSED) if neither `timeout` (Linux) nor `gtimeout` (macOS coreutils)
# exists. These tests pin: (T1) a run with no timeout binary on PATH aborts
# non-zero with the actionable message rather than proceeding; (T2) `gtimeout` is
# accepted when `timeout` is absent; (T3) with the binary absent the guards are
# NOT reachable at all — the abort happens before any guard runs, so there is no
# fail-open path left.

# Build a minimal bin dir holding just the utilities release.sh needs to reach
# the timeout-resolution point, symlinked from the real ones. $2... are extra
# binaries to include (e.g. "gtimeout"); `timeout`/`gtimeout` are NEVER included
# unless named explicitly. Echoes the bin dir path.
make_min_bin() {
    local dir="$1"; shift
    mkdir -p "$dir"
    local c p
    for c in bash sh env cat sed awk sort printf command git dirname pwd grep date jq mktemp head tail cut basename; do
        p="$(command -v "$c" 2>/dev/null)" && ln -sf "$p" "$dir/$c"
    done
    for c in "$@"; do
        case "$c" in
            gtimeout) p="$(command -v timeout 2>/dev/null)" && ln -sf "$p" "$dir/gtimeout" ;;
            timeout)  p="$(command -v timeout 2>/dev/null)" && ln -sf "$p" "$dir/timeout" ;;
        esac
    done
    echo "$dir"
}

# T1: no `timeout` and no `gtimeout` on PATH → the full flow must ABORT non-zero
# and print the actionable message, NOT proceed with the guards blinded.
test_timeout_missing_aborts_fail_closed() {
    local d; d="$(mktemp -d)"
    local bin; bin="$(make_min_bin "$d/bin")"   # no timeout, no gtimeout
    local out rc
    out="$(PATH="$bin" bash "$RELEASE_SH" --dry-run 2>&1)"; rc=$?
    if [[ $rc -ne 0 && "$out" == *"no \`timeout\` binary found"* ]]; then
        pass_test timeout_missing_aborts_fail_closed
    else
        fail_test timeout_missing_aborts_fail_closed "did not fail closed on a missing timeout binary (rc=$rc): '$out'"
    fi
    rm -rf "$d"
}

# T2: only `gtimeout` present (the macOS coreutils case) → resolve_timeout_bin
# accepts it and TIMEOUT_BIN points at gtimeout; the flow does NOT abort.
test_timeout_gtimeout_accepted() {
    local d; d="$(mktemp -d)"
    local bin; bin="$(make_min_bin "$d/bin" gtimeout)"   # gtimeout only, no timeout
    local out
    out="$(PATH="$bin" RELEASE_SH_LIB_ONLY=1 bash -c "source '$RELEASE_SH' >/dev/null 2>&1; printf '%s' \"\$TIMEOUT_BIN\"" 2>&1)"
    if [[ "$out" == */gtimeout ]]; then
        pass_test timeout_gtimeout_accepted
    else
        fail_test timeout_gtimeout_accepted "gtimeout not accepted as the timeout binary: TIMEOUT_BIN='$out'"
    fi
    rm -rf "$d"
}

# T3: RELEASE_TIMEOUT_BIN override wins (test-injection hook) and a genuine
# timeout (rc 124) stays distinguishable from a missing binary. With a real
# timeout bound to a fast deadline against `sleep`, the wrap returns 124 — NOT
# 127 — proving the "binary missing" (aborted at startup) and "genuine timeout"
# (rc 124) cases can never be confused.
test_timeout_genuine_vs_missing_distinct() {
    local real; real="$(command -v timeout 2>/dev/null)"
    if [[ -z "$real" ]]; then
        fail_test timeout_genuine_vs_missing_distinct "no real timeout to exercise the 124 path"
        return
    fi
    local rc=0
    RELEASE_TIMEOUT_BIN="$real" RELEASE_SH_LIB_ONLY=1 bash -c "
        source '$RELEASE_SH' >/dev/null 2>&1
        \"\$TIMEOUT_BIN\" 1 sleep 5
    " || rc=$?
    if [[ $rc -eq 124 ]]; then
        pass_test timeout_genuine_vs_missing_distinct
    else
        fail_test timeout_genuine_vs_missing_distinct "genuine timeout did not yield rc 124 (got $rc) — 124 must stay distinct from a missing-binary abort"
    fi
}

test_remote_tags_from_shallow_clone
test_compute_version_next_seq
test_compute_version_first_of_day
test_compute_version_numeric_seq
test_no_tag_pack_pulled
test_verify_sha_present
test_verify_sha_unpushed
test_rwx_bounded_success
test_rwx_bounded_failure_propagates
test_rwx_bounded_hang_is_aborted
test_fresh_base_guard_pass
test_fresh_base_guard_fails_on_stale
test_fast_forward_recovers_behind
test_fast_forward_refuses_divergent
test_head_on_main_guard_pass
test_head_on_main_guard_refuses_feature_branch
test_head_on_main_guard_refuses_detached
test_atomic_push_no_orphan
test_bare_push_orphans_tag
test_atomic_push_happy_path
test_push_retry_recovers_lost_race
test_push_retry_reuses_unchanged_base
test_push_retry_is_bounded
test_preflight_stall_no_skip_suggestion
test_preflight_failure_exits_nonzero
test_breaker_golden_no_suppress
test_breaker_redrc_blocker_suppresses
test_breaker_redrc_no_blocker_no_suppress
test_breaker_glab_error_fails_open
test_breaker_kd_error_fails_open
test_breaker_blocker_shape_match
test_breaker_unrelated_apply_no_suppress
test_breaker_tag_is_golden_classification
test_sign_valid_key_signs
test_sign_no_key_unsigned
test_sign_missing_key_fails_open
test_sign_encrypted_key_fails_open
test_sign_b64_key_signs
test_sign_b64_invalid_fails_open
test_sign_empty_args_portable_expansion
test_quiet_hours_business_hours_suppresses
test_quiet_hours_before_open_proceeds
test_quiet_hours_close_edge_proceeds
test_quiet_hours_last_blocked_hour_suppresses
test_quiet_hours_saturday_proceeds
test_quiet_hours_sunday_proceeds
test_quiet_hours_bad_clock_fails_open
test_quiet_hours_posix_tz_resolves_without_tzdata
test_timeout_missing_aborts_fail_closed
test_timeout_gtimeout_accepted
test_timeout_genuine_vs_missing_distinct

echo ""
echo "release_test.sh: ${PASS} passed, ${FAIL} failed"
[[ $FAIL -eq 0 ]]
