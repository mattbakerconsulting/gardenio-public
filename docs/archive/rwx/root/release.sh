#!/usr/bin/env bash
set -euo pipefail

# Gardenio monorepo release script.
# Usage: ./scripts/release.sh [--dry-run] [--preflight] [--skip-preflight] [--skip-mirror] [--skip-lock] [--skip-breaker] [--skip-quiet-hours] [--no-push]
#
# Generates a calver tag (YYYY.DDD.N), bumps Chart.yaml,
# commits, tags, and ATOMICALLY pushes main + tag together. CI handles
# the rest:
#
#   Tag push triggers (via RWX GitLab integration):
#     RWX docker.yml    → build all images → push to GHCR
#                        → deploy to gardenio-rwx (staging)
#                        → run E2E smoke tests
#                        → gate-prod-deploy → trigger GitLab prod deploy
#     RWX helm.yml      → package + push Helm chart
#     GitLab CI         → create GitLab Release
#
# Flags:
#   --dry-run          Preview the version and pipeline steps without making changes
#   --preflight        Run RWX CI (build + lint + test) before tagging (default)
#   --skip-preflight   Skip the preflight check (use when CI already passed on HEAD)
#   --skip-mirror      Skip the mirror-images dispatch even if .rwx/mirror.yml changed
#                      (emergency only — see ka-0000001365)
#   --skip-lock        Skip the distributed single-flight lock (ka-0000003407).
#                      Use only when you KNOW no other release-owner is alive
#                      (e.g. a manual local cut with no captains running). The
#                      lock prevents two concurrent captains from double-cutting
#                      the same version.
#   --skip-breaker     Skip the deploy-failure circuit-breaker gate (ka-0000007280).
#                      Use only for a deliberate operator cut that you KNOW must
#                      proceed despite an open release-blocker (e.g. the cut that
#                      itself carries the deploy fix). The breaker otherwise
#                      SUPPRESSES a cut when the last tag is stuck at a broken,
#                      operator-gated deploy AND a release-blocker is open — see
#                      the breaker section below.
#   --skip-quiet-hours Skip the PST-business-hours quiet-hours gate
#                      (ka-0000004693). This is the "unless explicitly asked"
#                      escape hatch: a human (or the operator-fire manual
#                      trigger entry) who deliberately wants a cut during PST
#                      business hours passes this flag. The scheduled
#                      captain-release-owner never passes it, so its cadence is
#                      held during the quiet window and resumes at 18:00 PT.
#   --no-push          Commit + tag locally but do NOT push. Prints the atomic
#                      push command for you to run by hand. Default is to push
#                      the branch and tag together with `git push --atomic`
#                      (ka-0000001299) so a rejected main update can never leave
#                      an orphaned tag pointing off-main.
#
# Release quiet-hours invariant (ka-0000004693):
#   Operator policy (Matt Baker): "for the release captain, for gardenio itself,
#   let's make it so we don't do releases during PST business hours (7AM-6PM)
#   unless explicitly asked." This script refuses a cut during PST/PDT business
#   hours (07:00-18:00 America/Los_Angeles, weekdays) unless --skip-quiet-hours
#   is passed. Two subtleties this gate gets right:
#     1. tzdata is ABSENT in agent + CI pods, so `TZ=America/Los_Angeles` in a
#        shell silently resolves to UTC — a naive hour-gate would be off by
#        7-8h and would block/allow exactly the wrong window. We use the
#        self-contained POSIX TZ string PST8PDT,M3.2.0,M11.1.0, which glibc
#        parses WITHOUT tzdata and which encodes US DST correctly (verified in
#        both PST and PDT).
#     2. It is SELF-CLEARING: a cut held at 10:00 PT proceeds automatically the
#        next cycle after 18:00 PT. No tag is cut and no error is raised — a
#        quiet-hours hold is a SUCCESS exit (rc 0), like the deploy-breaker.
#   Weekends release freely (business hours = weekdays). Override with
#   --skip-quiet-hours for a deliberate/human-asked cut.
#
# Deploy-failure circuit-breaker invariant (ka-0000007280):
#   The captain-release-owner Step 4.0 circuit-breaker (doc ka-0000000327) was
#   only a PROSE instruction the LLM captain was told to follow — nothing in the
#   actual cut path enforced it. During the 2026-07-23 deploy-prod drought the
#   captain kept cutting hourly tags (2026.204.7 -> 204.17) while emitting
#   "suppressed cut" comments; every wasted cut fired ~98 image builds + a full
#   deploy+e2e for a release that provably could not publish. This script now
#   enforces the breaker MECHANICALLY, BEFORE the mirror/preflight/tag/push, so a
#   cut is refused whenever BOTH Step-4.0 conditions positively hold:
#     (a) the latest calver tag is NOT a promoted golden (no GitLab Release), and
#     (b) an open release-blocker escalation/bug holds against tip-of-main.
#   It is SELF-CLEARING (recomputed live every run) and FAILS OPEN — any read
#   error or missing tooling never blocks a cut, it only ever suppresses on a
#   POSITIVE both-conditions-true signal. Override with --skip-breaker.
#
# Fresh-base + atomic-push invariant (ka-0000001299):
#   Release 2026.200.15 orphaned itself because two things lined up: (1) the
#   release commit was cut on a STALE local base while origin/main had already
#   advanced under it, and (2) the publish step ran a NON-ATOMIC
#   `git push origin main <TAG>` — the `main` ref was rejected as non-fast-
#   -forward but the tag ref still landed, so the tag pointed at a commit that
#   is not reachable from main. This script now closes BOTH holes: before the
#   preflight it fast-forwards HEAD onto origin/main and refuses to cut unless
#   HEAD is that live tip, and it pushes the branch and tag in a single
#   `--atomic` transaction so the tag can never land without main advancing in
#   lockstep.
#
# Environment:
#   GARDENIO_RWX_TOKEN  Optional. If set, used as RWX_ACCESS_TOKEN for the
#                      `rwx run` preflight + mirror dispatches, so those runs
#                      bill a LOL-owned RWX org instead of the ambient token
#                      (ka-0000000040). Leave unset to use the existing
#                      RWX_ACCESS_TOKEN (unchanged default behaviour).
#   RELEASE_SIGNING_KEY / RELEASE_SIGNING_KEY_B64
#                      Optional. SSH signing key for the release commit + tag
#                      (F4, ka-0000005321). _KEY is a path to an unencrypted
#                      private key; _B64 is base64 of that key (materialised to a
#                      shredded 0600 temp file, takes precedence). When set +
#                      usable, the cut runs `git commit -S` + annotated `git tag
#                      -s` (SSH backend — the pod has ssh-keygen, no gpg). FAIL-
#                      OPEN: an absent/encrypted/malformed key logs a warning and
#                      the cut proceeds UNSIGNED rather than aborting the release
#                      lane. Operator setup (key provisioning, GitLab Verified
#                      badge, reject_unsigned_commits push rule):
#                      gardenio/docs/release-signing.md.
#   RELEASE_LOCK_TTL   Optional. Lock lifetime in seconds (default 1200). See
#                      scripts/release-lock.sh.
#   RELEASE_RWX_RUN_TIMEOUT
#                      Optional. Hard timeout in seconds for each blocking
#                      `rwx run ... --wait` dispatch — the mirror gate and the
#                      preflight (default 1200). A stalled RWX run that never
#                      reaches a terminal state would otherwise hang release.sh
#                      forever while it holds the single-flight lock
#                      (ka-0000001167 and the preflight-hang cluster). Raise it
#                      only if a legitimately slow run needs more headroom.
#   RELEASE_PUSH_MAX_ATTEMPTS
#                      Optional. Max cut+push attempts in one invocation before
#                      giving up (default 3). If the atomic push is rejected
#                      non-fast-forward because a peer landed on main during the
#                      preflight, the script re-syncs onto the new tip and
#                      re-cuts automatically instead of forcing a manual
#                      `reset --hard && ./scripts/release.sh` re-run — the retry
#                      storm from ka-0000007981. Raise it only for a legitimately
#                      busy release window (main advancing faster than a cut).

DRY_RUN=false
PREFLIGHT=true
MIRROR=true
LOCK=true
BREAKER=true
QUIET_HOURS=true
PUSH=true
for arg in "$@"; do
    case "$arg" in
        --dry-run)          DRY_RUN=true ;;
        --preflight)        PREFLIGHT=true ;;
        --skip-preflight)   PREFLIGHT=false ;;
        --skip-mirror)      MIRROR=false ;;
        --skip-lock)        LOCK=false ;;
        --skip-breaker)     BREAKER=false ;;
        --skip-quiet-hours) QUIET_HOURS=false ;;
        --no-push)          PUSH=false ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Configurable RWX token (ka-0000000040) ────────────────────────────
# The preflight + mirror gate run via `rwx run`, which authenticates with
# RWX_ACCESS_TOKEN. To bill a LOL-owned RWX org rather than whatever token
# is ambient in the environment, export GARDENIO_RWX_TOKEN — it takes
# precedence for the duration of this script. Unset = current behaviour.
if [[ -n "${GARDENIO_RWX_TOKEN:-}" ]]; then
    export RWX_ACCESS_TOKEN="$GARDENIO_RWX_TOKEN"
    echo "Using GARDENIO_RWX_TOKEN for RWX preflight/mirror dispatch."
fi

# Remote to read tags from. Same default as the release lock so a --skip-lock
# local run and a locked captain cut agree on where "the tags" live.
RELEASE_REMOTE="${RELEASE_LOCK_REMOTE:-origin}"
# Timeout for every remote git read. release-owner pods run a shallow,
# --no-tags, single-branch clone (controller initclone.go), so a bare
# `git fetch --tags` there negotiates the ENTIRE tag pack and can hang
# indefinitely in index-pack — the exact wedge that stalled the release lane
# for 16h (ka-0000003116 / ka-0000005733). We never `git fetch` tags into the
# workspace at all now; we read ref NAMES only via `git ls-remote` (no pack,
# no objects), and cap even that with a hard timeout so a stuck network fails
# the cut fast instead of hanging the captain forever.
GIT_REMOTE_TIMEOUT="${RELEASE_GIT_REMOTE_TIMEOUT:-60}"

# ── Resolve the `timeout` binary — a HARD dependency; fail CLOSED if absent ──
# Every remote read below (the deploy-failure breaker, the mirror gate, the
# fresh-base/version reads) is wrapped in `timeout "$GIT_REMOTE_TIMEOUT" ...` so
# a wedged network fails the cut FAST instead of hanging the release lane. If the
# `timeout` binary is ABSENT (an operator Mac without GNU coreutils), those wraps
# exit 127 (command not found); the helpers read that as rc≠0 and `return 2`, and
# every caller then FAILS OPEN — the breaker, the mirror gate, and the fresh-base
# guard are all silently skipped and the cut proceeds unguarded, even computing a
# COLLIDING version while still offering to cut (measured live 2026-08-26,
# ka-0000007503 / ka-0000000605). Rather than run the guards blinded, resolve the
# binary ONCE at startup and ABORT with a clear message if neither `timeout`
# (Linux coreutils) nor `gtimeout` (macOS `brew install coreutils`) exists —
# fail CLOSED on a missing hard dependency. Because a missing binary now aborts
# here, no wrapped read can ever see rc 127 for an absent `timeout`, so a genuine
# timeout (rc 124) stays the only timeout-related exit code the helpers observe.
# Overridable via RELEASE_TIMEOUT_BIN so the unit test can inject a mock.
resolve_timeout_bin() {
    if [[ -n "${RELEASE_TIMEOUT_BIN:-}" ]]; then
        printf '%s\n' "$RELEASE_TIMEOUT_BIN"
        return 0
    fi
    command -v timeout 2>/dev/null && return 0
    command -v gtimeout 2>/dev/null && return 0
    return 1
}

TIMEOUT_BIN="$(resolve_timeout_bin)" || {
    cat >&2 <<'EOF'

RELEASE ABORTED — no `timeout` binary found (checked `timeout` and `gtimeout`).

  release.sh bounds every remote read (the deploy-failure breaker, the mirror
  gate, the fresh-base/version reads) with `timeout` so a wedged network fails
  the cut fast. Without it, those wraps exit 127 and EVERY guard silently fails
  OPEN — the breaker and mirror gate are skipped and the cut can compute a
  COLLIDING version while still offering to proceed (ka-0000000605). Refusing to
  cut with the guards blinded.

  Fix (macOS): brew install coreutils   # provides `gtimeout`
  Fix (Linux): install GNU coreutils    # provides `timeout`

  Or run the cut from a host/pod that ships coreutils — the in-cluster
  release-owner pod always does; this bug only bites hosts lacking it.
EOF
    exit 1
}

# rwx CLI binary. Overridable so the unit test can inject a mock (see
# scripts/release_test.sh); defaults to the real CLI in the release flow.
RWX_BIN="${RWX_BIN:-rwx}"

# Hard ceiling for a single blocking `rwx run ... --wait` dispatch (the mirror
# gate + the preflight). `rwx run --wait` polls the dispatched run until it
# reaches a TERMINAL state with NO deadline of its own, so an RWX run that
# stalls in `in_progress` — queue/resource contention, a wedged code.git-clone
# task, or an RWX backend stall — wedges release.sh FOREVER while it still holds
# the single-flight lock. That is the exact 15/25/40+min preflight hang that
# broke the release lane repeatedly (ka-0000001167, ka-0000003629,
# ka-0000004640, ka-0000004157). We cap every blocking `rwx run --wait` with
# `timeout` — the same guardrail GIT_REMOTE_TIMEOUT applies to the remote git
# reads — so a stalled run fails the cut FAST instead of hanging the captain.
# Default 1200s (== RELEASE_LOCK_TTL default): comfortably above a legitimate
# cold-cache preflight (build+lint+test, ~3-10 min) yet well below the
# pathological hangs, and once the lock TTL has elapsed a still-running
# preflight is pointless anyway. Override with RELEASE_RWX_RUN_TIMEOUT.
RWX_RUN_TIMEOUT="${RELEASE_RWX_RUN_TIMEOUT:-1200}"

# List release-tag NAMES on the remote, newest last. Name-only ls-remote pulls
# no git objects, so it is safe + fast on a shallow workspace and cannot hang
# in index-pack the way `git fetch --tags` does. `--refs` strips peeled
# `^{}` entries; the timeout bounds a wedged connection.
remote_release_tags() {
    "$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" \
        git ls-remote --tags --refs "$RELEASE_REMOTE" '20*.*.*' 2>/dev/null \
        | sed -E 's#^.*refs/tags/##' \
        | sort -t. -k1,1n -k2,2n -k3,3n
}

# Run a blocking `rwx run ... --wait` under a hard timeout so a stalled RWX run
# can never hang the release cut. Passes all args through to `rwx run`. Returns
# the rwx exit code on completion; on a stall past RWX_RUN_TIMEOUT it prints a
# distinct, actionable message and returns 124 (the `timeout` convention). The
# `-k 10` SIGKILL grace guarantees a CLI that ignores SIGTERM is still reaped;
# both the SIGTERM (124) and SIGKILL (137) outcomes are normalised to 124. The
# orphaned remote run is harmless: the mirror gate's crane copy is idempotent
# and the preflight is read-only build/lint/test — neither mutates release
# state, so a re-run is safe once the stalled run is cancelled.
rwx_run_bounded() {
    local rc=0
    "$TIMEOUT_BIN" -k 10 "$RWX_RUN_TIMEOUT" "$RWX_BIN" run "$@" || rc=$?
    if [[ $rc -eq 124 || $rc -eq 137 ]]; then
        echo "" >&2
        echo "RWX run exceeded RELEASE_RWX_RUN_TIMEOUT=${RWX_RUN_TIMEOUT}s and was aborted." >&2
        echo "The dispatched run stalled (typically stuck 'in_progress' with no terminal result)." >&2
        echo "Inspect and cancel it at https://cloud.rwx.com/mint/acme, then re-run the release." >&2
        echo "If a legitimately slow run needs more time, raise RELEASE_RWX_RUN_TIMEOUT=<seconds>." >&2
        return 124
    fi
    return $rc
}

# Generate calver: YYYY.DDD.N (year.day-of-year.sequence).
# Always use UTC so agents and humans produce consistent tags.
# Factored into a function so it can be recomputed authoritatively AFTER the
# single-flight lock is held — see the lock section below (ka-0000003407).
# Reads the sequence from the REMOTE tag list (remote_release_tags), never from
# local `git tag -l`: the workspace is a --no-tags clone, so local tags are
# unreliable, and the remote read is the same source of truth the locked
# recompute uses — no fetch required.
compute_version() {
    local year doy prefix existing last_seq seq
    year=$(TZ=UTC date +%Y)
    doy=$(TZ=UTC date +%-j)  # day of year, no leading zero
    prefix="${year}.${doy}"
    existing=$(remote_release_tags | grep -E "^${prefix}\.[0-9]+$" | tail -1 || true)
    if [[ -n "$existing" ]]; then
        last_seq=$(echo "$existing" | awk -F. '{print $3}')
        seq=$((last_seq + 1))
    else
        seq=1
    fi
    echo "${prefix}.${seq}"
}

# ── Release signing configuration (ka-0000005321, F4) ─────────────────────
# The release commit + tag are the trust root of the whole pipeline: a tag push
# triggers image builds, staging deploy, E2E, and a terraform-driven prod
# rollout. Before F4 the cut used a PLAIN `git commit` + `git tag` (no `-S`/`-s`),
# so nothing cryptographically bound "this tag was cut by a release-owner" — a
# forged tag pointing at attacker code would look identical to a genuine one.
#
# F4 signs both with an SSH signing key (git 2.34+; the release-owner pod ships
# `ssh-keygen` + git 2.43 but NO gpg binary, so SSH — not GPG — is the only
# signer available in-pod). The key is provisioned out-of-band as a secret and
# surfaced to this script via RELEASE_SIGNING_KEY (a path) or
# RELEASE_SIGNING_KEY_B64 (base64 of the private key, materialised to a
# 0600 temp file). The matching PUBLIC key is registered on the bot's GitLab
# account so GitLab shows the commit/tag "Verified", and — once the operator
# enables it (Maintainer-only, bot is 403) — the `reject_unsigned_commits` push
# rule enforces that every commit on main carries a signature.
#
# FAIL-OPEN by design. If no key is configured, or the configured key is
# unusable (missing / encrypted / malformed), the cut proceeds UNSIGNED with a
# loud warning rather than aborting. Rationale: the release lane is the fleet's
# only path to ship a fix, and a signing misconfiguration must never wedge it
# (the same fail-open discipline as the deploy-failure breaker and the RWX
# timeouts). Signing HARDENS the cut; it must not become a new way to break it.
# When the operator later turns on `reject_unsigned_commits`, an unsigned commit
# is rejected at push time by GitLab — a loud, recoverable failure at the
# perimeter, not a silent gap — so fail-open here does not defeat enforcement.
#
# Sets three globals consumed by the commit/tag step and the wrap-up log:
#   SIGN_COMMITS    "true"|"false" — pass -S to git commit / -s to git tag
#   SIGN_GIT_ARGS   array of `-c gpg.format=ssh -c user.signingkey=<path>` (empty
#                   when unsigned) — spliced into the git invocations
#   SIGN_STATUS     human-readable one-liner for the cut log / wrap-up
SIGN_COMMITS=false
SIGN_GIT_ARGS=()
SIGN_STATUS="unsigned (no signing key configured — set RELEASE_SIGNING_KEY[_B64])"
# Temp key file to shred on exit when we materialise RELEASE_SIGNING_KEY_B64.
SIGNING_KEY_TMP=""
release_signing_cleanup() {
    [[ -n "$SIGNING_KEY_TMP" && -f "$SIGNING_KEY_TMP" ]] && rm -f "$SIGNING_KEY_TMP"
    return 0
}

# Resolve the signing key and decide whether this cut signs. Fail-open: any
# problem leaves SIGN_COMMITS=false with a reason in SIGN_STATUS and returns 0.
# Never hangs: every ssh-keygen/git probe reads stdin from /dev/null so an
# encrypted key prompts nothing (it fails fast instead — verified ka-0000005321).
configure_release_signing() {
    local key=""
    if [[ -n "${RELEASE_SIGNING_KEY_B64:-}" ]]; then
        # Materialise the base64 private key to a 0600 temp file we own + shred.
        SIGNING_KEY_TMP="$(mktemp)"
        chmod 600 "$SIGNING_KEY_TMP"
        if ! printf '%s' "$RELEASE_SIGNING_KEY_B64" | base64 -d > "$SIGNING_KEY_TMP" 2>/dev/null; then
            SIGN_STATUS="unsigned (RELEASE_SIGNING_KEY_B64 is not valid base64)"
            rm -f "$SIGNING_KEY_TMP"; SIGNING_KEY_TMP=""
            return 0
        fi
        key="$SIGNING_KEY_TMP"
    elif [[ -n "${RELEASE_SIGNING_KEY:-}" ]]; then
        key="$RELEASE_SIGNING_KEY"
    else
        return 0  # SIGN_STATUS already says "no signing key configured"
    fi

    if [[ ! -f "$key" ]]; then
        SIGN_STATUS="unsigned (signing key not found at configured path)"
        return 0
    fi
    # An SSH signing key must be an unencrypted private key we can read headless.
    # `ssh-keygen -y -f <key> </dev/null` derives the public key iff the private
    # key is usable without a passphrase; on an encrypted/malformed key it fails
    # FAST (rc!=0) instead of prompting — no hang, no --wait. (ka-0000005321)
    if ! ssh-keygen -y -f "$key" < /dev/null > /dev/null 2>&1; then
        SIGN_STATUS="unsigned (signing key is unreadable, encrypted, or not a valid private key)"
        return 0
    fi

    SIGN_COMMITS=true
    SIGN_GIT_ARGS=(-c gpg.format=ssh -c "user.signingkey=${key}")
    local fp
    fp="$(ssh-keygen -lf "$key" 2>/dev/null | awk '{print $2}')"
    SIGN_STATUS="SSH-signed (key ${fp:-unknown})"
    return 0
}

# Verify a commit SHA is actually reachable on the release remote BEFORE we
# dispatch any RWX run that clones by that SHA (ka-0000003172).
#
# Both the mirror gate and preflight dispatch `rwx run ... --init
# commit-sha="$(git rev-parse HEAD)"`, and .rwx/ci.yml's `code` task clones by
# that exact SHA (`git/clone` with `ref: ${{ init.commit-sha }}`). If HEAD is a
# LOCAL-only commit that was never pushed to origin, gitlab's upload-pack
# rejects the unreachable SHA with `fatal: remote error: upload-pack: not our
# ref <sha>` (exit 128) — and it does so on EVERY retry, because the SHA is
# simply not there. That is a deterministic wedge, not a transient flake: two
# consecutive preflight runs (8acfdac1, 3a3aabc2) failed byte-identically on the
# same missing SHA and burned RWX minutes each time.
#
# `git fetch --depth 1 <remote> <sha>` is a faithful oracle for the clone's
# outcome: gitlab serves any commit reachable from an advertised ref by SHA
# (verified against real tags/ancestors, rc=0), and rejects an unpushed SHA with
# the identical "not our ref" error (rc≠0). We fetch into FETCH_HEAD only (no
# ref update, depth 1 so it cannot hang negotiating a full pack) and bound it
# with the same timeout as every other remote read.
#
# Returns 0 if the SHA is fetchable from $RELEASE_REMOTE, non-zero otherwise.
verify_sha_on_remote() {
    local sha="$1"
    "$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" \
        git fetch -q --depth 1 "$RELEASE_REMOTE" "$sha" >/dev/null 2>&1
}

# Guard the release flow: refuse to dispatch RWX runs against a SHA that origin
# cannot serve. Fails fast with an actionable message (push HEAD first) instead
# of burning RWX minutes on clone tasks that are guaranteed to fail identically
# on every retry. $DRY_RUN callers skip this (no dispatch happens).
require_head_on_remote() {
    local sha
    sha="$(git rev-parse HEAD)"
    if verify_sha_on_remote "$sha"; then
        return 0
    fi
    cat >&2 <<EOF

RELEASE ABORTED — HEAD ($(git rev-parse --short HEAD)) is not on ${RELEASE_REMOTE}.

  The RWX preflight/mirror runs clone the repo by this exact commit SHA:
      $sha
  ${RELEASE_REMOTE} does not have it, so every git-clone task would fail with
  'fatal: remote error: upload-pack: not our ref' (exit 128) on every retry —
  burning RWX minutes with no chance of passing. This is a local, unpushed
  commit, NOT a code or pipeline bug.

  Fix: push HEAD to ${RELEASE_REMOTE} first, then re-run the release:
      git push ${RELEASE_REMOTE} HEAD:main

  (If you intentionally want to test a throwaway branch, push it and dispatch
  'rwx run .rwx/ci.yml --init commit-sha=<pushed-sha>' directly.)
EOF
    return 1
}

# Read the live tip of the release branch on the remote by NAME (ka-0000001299).
# Like remote_release_tags this uses `git ls-remote` — it negotiates no git
# objects and pulls no pack, so it is safe + fast on the release-owner's shallow
# --no-tags clone and cannot hang in index-pack (the wedge that took the release
# lane down for 16h, ka-0000003116). Timeout-bounded so a stuck network fails the
# cut fast rather than hanging the captain. Echoes the 40-char main SHA, or
# nothing on failure (caller treats empty as "could not verify").
remote_main_tip() {
    "$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" \
        git ls-remote "$RELEASE_REMOTE" refs/heads/main 2>/dev/null \
        | awk 'NR==1 {print $1}'
}

# Fresh-base guard (ka-0000001299): return 0 only if the commit we are about to
# tag sits on the LIVE tip of origin/main. Release 2026.200.15 orphaned itself by
# being cut on a stale base while main advanced underneath; combined with a
# non-atomic push, the tag landed off-main. The caller runs this AFTER
# try_fast_forward_main and treats a non-zero return as "refuse the cut" (before
# any commit/tag/push), printing the remediation. Returns non-zero when HEAD is
# not the tip, OR when the remote is unreadable — fail SAFE, since we cannot
# prove freshness (the unreadable case prints its own diagnostic here because it
# is a distinct, network-level failure).
#   $1 = the local commit SHA we intend to tag (defaults to HEAD)
assert_on_fresh_main() {
    local head_sha remote_sha
    head_sha="${1:-$(git rev-parse HEAD)}"
    remote_sha="$(remote_main_tip)"
    if [[ -z "$remote_sha" ]]; then
        echo "FRESH-BASE GUARD: could not read origin/main tip (network/timeout) — refusing to cut." >&2
        echo "  A release cut on an unverified base is how tag 2026.200.15 orphaned itself (ka-0000001299)." >&2
        return 1
    fi
    if [[ "$head_sha" != "$remote_sha" ]]; then
        return 1
    fi
    return 0
}

# Attempt a SAFE fast-forward of the current branch onto RELEASE_REMOTE/main
# (ka-0000001299). The unattended release-owner can fall behind while its own
# multi-minute preflight runs — main advances under it — so a hard refuse would
# leave the release lane dark on an ordinary race. A clean fast-forward recovers
# that case: it moves HEAD forward to the live tip and NOTHING is discarded.
# `--ff-only` is the safety valve: on a genuine divergence (a local commit not on
# origin) it refuses and leaves HEAD untouched rather than clobbering work, so we
# fall through to the guard's refusal path. `--no-tags` keeps the fetch from
# pulling the tag pack (the index-pack hang, ka-0000003116); timeout-bounded.
# Returns 0 iff HEAD now equals the origin/main tip that was fetched.
try_fast_forward_main() {
    "$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" \
        git fetch --no-tags -q "$RELEASE_REMOTE" main 2>/dev/null || return 1
    git merge --ff-only FETCH_HEAD >/dev/null 2>&1
}

# HEAD-on-main guard (ka-0000002455). release.sh cuts the version-bump commit +
# tag on HEAD, but PUBLISHES them by pushing the local `main` REF
# (`git push --atomic <remote> main <TAG>`). Those two refs coincide ONLY when
# `main` is the checked-out branch. If a cut is run from a FEATURE branch,
# try_fast_forward_main fast-forwards HEAD (the feature branch) — NOT the local
# `main` ref, which stays stale — the commit + tag land on the feature branch,
# and the atomic push of the stale `main` ref is rejected non-fast-forward. The
# retry loop then mis-blames a busy release lane and suggests
# `git reset --hard <remote>/main`, which cannot fix it while HEAD is elsewhere
# (ka-0000005650 secondary). Refuse HERE — before try_fast_forward_main masks the
# branch and before any RWX preflight minutes are burned — with an accurate
# remedy. Preserves the --atomic anti-orphan invariant (ka-0000001299): we never
# reach a push on this path. The release-owner pod always clones `-b main`, so
# this guard is a no-op for it and only catches a manual operator misuse.
# Returns 0 when HEAD is the `main` branch; prints the remedy and returns 1
# otherwise (detached HEAD included).
assert_head_on_main() {
    local branch
    branch="$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
    if [[ "$branch" == "main" ]]; then
        return 0
    fi
    echo "RELEASE ABORTED — HEAD is on '${branch:-(detached HEAD)}', not 'main'." >&2
    echo "  release.sh cuts the version bump + tag on HEAD but publishes by pushing the" >&2
    echo "  local 'main' ref (git push --atomic ${RELEASE_REMOTE} main <TAG>). Cutting from" >&2
    echo "  another branch leaves local 'main' stale, so the atomic push is rejected" >&2
    echo "  non-fast-forward. This is NOT a busy release lane, and" >&2
    echo "  'git reset --hard ${RELEASE_REMOTE}/main' cannot fix it while HEAD is elsewhere." >&2
    echo "  Check out main and cut from there:" >&2
    echo "    git checkout main && git pull --ff-only ${RELEASE_REMOTE} main && ./scripts/release.sh" >&2
    return 1
}

# ── Deploy-failure circuit-breaker (ka-0000007280) ─────────────────────
# GitLab project path (URL-encoded) for the glab release/API reads below.
# Overridable so the unit test can point at nothing (the reads then fail and
# the breaker fails OPEN, which the test asserts).
GLAB_PROJECT_PATH="${RELEASE_GLAB_PROJECT:-Acme%2FPlatform%2Fgardenio}"

# glab CLI binary. Overridable so the unit test can inject a mock (mirrors
# RWX_BIN); defaults to the real CLI.
GLAB_BIN="${GLAB_BIN:-glab}"

# kd CLI binary. Same override rationale as GLAB_BIN.
KD_BIN="${KD_BIN:-kd}"

# breaker_tag_is_golden: return 0 iff $1 (a calver tag) has a promoted GitLab
# Release — i.e. the release pipeline ran create-release and the tag is golden.
# `glab api projects/<p>/releases/<tag>` returns the release JSON (rc 0) for a
# promoted tag and a 404 (rc≠0) for an un-promoted `red-rc`/pending tag — the
# exact positive/negative split verified live on 2026.204.6 (golden) vs
# 2026.204.17 (404). Returns 2 on a read ERROR we cannot interpret (empty
# output with a non-zero rc that isn't a clean 404) so the caller can fail OPEN
# rather than mis-suppress on a transient network/auth blip.
breaker_tag_is_golden() {
    local tag="$1" out rc
    out="$("$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" "$GLAB_BIN" api \
        "projects/${GLAB_PROJECT_PATH}/releases/${tag}" 2>/dev/null)"
    rc=$?
    if [[ $rc -eq 0 && "$out" == *'"tag_name"'* ]]; then
        return 0   # promoted golden
    fi
    if [[ "$out" == *'404'* || "$out" == *'Not Found'* ]]; then
        return 1   # definitively NOT promoted (clean 404)
    fi
    return 2       # unreadable — caller fails OPEN
}

# breaker_open_release_blocker: echo the id of an OPEN release-blocker
# escalation/bug (empty if none), and return 0 on a successful read, 2 on a
# read error. Matches the Step-4.0(b) shape two ways, in order:
#   1. any OPEN bead carrying the `release-pipeline-broken` label (the canonical
#      release-blocker label — e.g. ka-0000001996 during the 204 drought), OR
#   2. any OPEN type=escalation whose title names the broken deploy path
#      (release drought / deploy-prod / deploy-rwx / EKS allowlist / CI-deploy
#      IAM / terraform wedged) — the drought escalation ka-0000001012 carried
#      only `project:gardenio`, so a label-only match would miss it.
# Reads are label/type-scoped `kd list --json`; the shape-match is a jq regex
# over the title. A kd read failure returns 2 (caller fails OPEN).
breaker_open_release_blocker() {
    local out rc id
    # (1) canonical label.
    out="$("$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" "$KD_BIN" list --status=open \
        -l release-pipeline-broken --json 2>/dev/null)"
    rc=$?
    if [[ $rc -ne 0 ]]; then return 2; fi
    id="$(printf '%s' "$out" | jq -r 'if type=="array" then (.[0].id // "") else "" end' 2>/dev/null)"
    if [[ -n "$id" ]]; then echo "$id"; return 0; fi
    # (2) shape-matched OPEN escalation title.
    out="$("$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" "$KD_BIN" list --type=escalation \
        --status=open -l project:gardenio --json 2>/dev/null)"
    rc=$?
    if [[ $rc -ne 0 ]]; then return 2; fi
    # Shape terms name a BROKEN RELEASE-DEPLOY path — deliberately NOT a bare
    # `terraform apply`: an unrelated open escalation that merely runs a targeted
    # apply on prod (e.g. the gVisor canary-node escalation ka-0000000361,
    # "targeted terraform apply on prod-gardenio-eks") is NOT a release blocker,
    # and matching it froze the whole release train after the real drought
    # blockers closed (ka-0000007546). Use `terraform wedged` (the documented
    # shape, release-pipeline.md) — the real drought escalation ka-0000001012
    # still matches via `advance prod`.
    id="$(printf '%s' "$out" | jq -r '
        if type=="array" then
          [ .[] | select((.title // "") | test(
            "release drought|release-blocker|release-pipeline-broken|deploy-prod|deploy-rwx|EKS allowlist|CI-deploy IAM|terraform wedged|advance prod";
            "i")) | .id ] | (.[0] // "")
        else "" end' 2>/dev/null)"
    echo "$id"
    return 0
}

# breaker_should_suppress: the Step-4.0 conjunction. Echoes a one-line reason
# and returns 0 (SUPPRESS the cut) iff BOTH conditions positively hold:
#   (a) $1 (latest tag) is NOT a promoted golden, AND
#   (b) an open release-blocker holds.
# Returns non-zero (DO NOT suppress — proceed with the cut) in every other case,
# INCLUDING any read error on either probe: the breaker fails OPEN so a network
# / auth / tooling blip can never wedge the release lane. A suppression requires
# a POSITIVE red-rc signal AND a POSITIVE open-blocker id — never an absence.
breaker_should_suppress() {
    local latest="$1" golden_rc blocker blocker_rc
    breaker_tag_is_golden "$latest"; golden_rc=$?
    if [[ $golden_rc -eq 0 ]]; then
        return 1   # (a) FALSE: latest tag is golden → deploy is fine, cut freely
    fi
    if [[ $golden_rc -eq 2 ]]; then
        # Could not read release state → fail OPEN (do not suppress).
        return 1
    fi
    # golden_rc == 1: latest tag is a positively-un-promoted red-rc. Now (b).
    blocker="$(breaker_open_release_blocker)"; blocker_rc=$?
    if [[ $blocker_rc -ne 0 ]]; then
        return 1   # kd read error → fail OPEN
    fi
    if [[ -z "$blocker" ]]; then
        return 1   # (b) FALSE: no open release-blocker → normal Step-3/4 governs
    fi
    echo "latest tag ${latest} is not promoted to golden (deploy failed) AND open release-blocker ${blocker} holds"
    return 0       # BOTH hold → SUPPRESS
}

# ── Release quiet-hours gate (ka-0000004693) ───────────────────────────
# POSIX TZ string for US Pacific with correct DST rules, self-contained so it
# needs NO tzdata (absent in agent + CI pods). "PST8PDT" sets the standard/DST
# abbreviations and the 8h base offset; ",M3.2.0,M11.1.0" is the US DST rule
# (spring forward 2nd Sunday of March, fall back 1st Sunday of November).
# glibc's strftime honors this without /usr/share/zoneinfo — the exact reason a
# naive `TZ=America/Los_Angeles date` (which silently yields UTC here) is wrong.
# Overridable only so the unit test can pin a fixed zone if ever needed.
RELEASE_QUIET_HOURS_TZ="${RELEASE_QUIET_HOURS_TZ:-PST8PDT,M3.2.0,M11.1.0}"
# The business-hours window, as start/end hour (24h local) and the set of
# in-scope weekday numbers (1=Mon..7=Sun, `date +%u`). Defaults encode the
# operator policy: 07:00-18:00, Mon-Fri. Overridable for tests.
RELEASE_QUIET_HOURS_START="${RELEASE_QUIET_HOURS_START:-7}"
RELEASE_QUIET_HOURS_END="${RELEASE_QUIET_HOURS_END:-18}"
RELEASE_QUIET_HOURS_DAYS="${RELEASE_QUIET_HOURS_DAYS:-1 2 3 4 5}"

# quiet_hours_now_parts: echo "HOUR DOW" for the current Pacific wall clock,
# using the tzdata-free POSIX TZ string. Overridable via RELEASE_QUIET_HOURS_NOW
# ("HOUR DOW", e.g. "10 4") so the unit test can pin a deterministic instant
# without touching the system clock.
quiet_hours_now_parts() {
    if [[ -n "${RELEASE_QUIET_HOURS_NOW:-}" ]]; then
        echo "$RELEASE_QUIET_HOURS_NOW"
        return 0
    fi
    TZ="$RELEASE_QUIET_HOURS_TZ" date '+%-H %u'
}

# quiet_hours_should_suppress: echo a one-line reason and return 0 (SUPPRESS the
# cut) iff the current Pacific time falls inside the business-hours window on an
# in-scope weekday. Returns non-zero (proceed) otherwise, INCLUDING any failure
# to read the clock — like the deploy breaker, this gate fails OPEN so a tooling
# blip can never wedge the release lane. It is the "unless explicitly asked"
# policy's mechanical half: --skip-quiet-hours bypasses it entirely.
quiet_hours_should_suppress() {
    local parts hour dow d
    parts="$(quiet_hours_now_parts 2>/dev/null)" || return 1
    hour="${parts%% *}"
    dow="${parts##* }"
    # Defensive: both must be integers, else fail OPEN (do not suppress).
    [[ "$hour" =~ ^[0-9]+$ && "$dow" =~ ^[0-9]+$ ]] || return 1
    # In-scope weekday?
    local in_scope=false
    for d in $RELEASE_QUIET_HOURS_DAYS; do
        if [[ "$dow" == "$d" ]]; then in_scope=true; break; fi
    done
    $in_scope || return 1
    # Inside [start,end) business hours?
    if (( hour >= RELEASE_QUIET_HOURS_START && hour < RELEASE_QUIET_HOURS_END )); then
        echo "current Pacific time is ${hour}:00 (business hours ${RELEASE_QUIET_HOURS_START}:00-${RELEASE_QUIET_HOURS_END}:00, weekday ${dow}) — release quiet hours in effect"
        return 0
    fi
    return 1
}

# Allow this script to be sourced by the unit test to exercise
# remote_release_tags/compute_version/verify_sha_on_remote/assert_on_fresh_main
# and the breaker helpers in isolation, without running the release flow (no
# network read, no mutation). The test sources this file (so `return` is valid)
# with RELEASE_SH_LIB_ONLY=1 set; everything below this line is the release flow
# proper.
if [[ -n "${RELEASE_SH_LIB_ONLY:-}" ]]; then
    return 0
fi

VERSION="$(compute_version)"
echo "Release version: ${VERSION}"

# ── Mirror gate: detect whether .rwx/mirror.yml has new entries ───────
# Root-cause of ka-0000000965 was an upstream image bump that added a
# new tag to .rwx/mirror.yml but skipped the manual mirror-images
# dispatch. Detect that case and force a mirror dispatch before tagging.
# Tracking bug: ka-0000001365.
MIRROR_FILE=".rwx/mirror.yml"
MIRROR_NEEDED=false
MIRROR_REASON=""
# Read the latest tag NAME from the remote (name-only, no pack — see
# remote_release_tags), not local `git tag -l`: the workspace is a --no-tags
# clone so local tags are empty/unreliable.
LAST_TAG=$(remote_release_tags | tail -1 || true)
if [[ -z "$LAST_TAG" ]]; then
    MIRROR_NEEDED=true
    MIRROR_REASON="no prior release tag found"
else
    # Diffing mirror.yml against LAST_TAG needs that tag's object locally. The
    # shallow clone doesn't have it, so fetch JUST that one ref at depth 1
    # (one commit — cannot hang on a full tag pack the way --tags does),
    # timeout-bounded. If we still can't resolve it, fail SAFE: run the mirror
    # gate (crane copy is idempotent, so a redundant dispatch is only a small
    # cost; a MISSED bump would break the release, ka-0000000965).
    if ! git rev-parse -q --verify "refs/tags/${LAST_TAG}^{commit}" >/dev/null 2>&1; then
        "$TIMEOUT_BIN" "$GIT_REMOTE_TIMEOUT" \
            git fetch -q --depth 1 "$RELEASE_REMOTE" \
            "refs/tags/${LAST_TAG}:refs/tags/${LAST_TAG}" 2>/dev/null || true
    fi
    if ! git diff --quiet "${LAST_TAG}" -- "$MIRROR_FILE" 2>/dev/null; then
        MIRROR_NEEDED=true
        MIRROR_REASON=".rwx/mirror.yml changed since ${LAST_TAG} (or ${LAST_TAG} unavailable locally)"
    fi
fi

# ── Deploy-failure circuit-breaker decision (ka-0000007280) ────────────
# Evaluate the Step-4.0 breaker ONCE, here, before any expensive/mutating step
# (the single-flight lock, the mirror gate, the preflight, the tag+push). This
# is the mechanical enforcement the prose-only breaker never had: if the last
# tag is stuck at a broken, operator-gated deploy AND a release-blocker is open,
# a re-cut provably cannot promote — so we refuse it here instead of burning
# ~98 image builds + a full deploy/e2e on a release that cannot publish. Fails
# OPEN: any read error leaves BREAKER_SUPPRESS=false (proceed with the cut).
BREAKER_SUPPRESS=false
BREAKER_REASON=""
if $BREAKER && [[ -n "$LAST_TAG" ]]; then
    if BREAKER_REASON="$(breaker_should_suppress "$LAST_TAG")"; then
        BREAKER_SUPPRESS=true
    fi
fi

# ── Release quiet-hours decision (ka-0000004693) ───────────────────────
# Evaluate the PST-business-hours gate ONCE, here, alongside the breaker. Like
# the breaker it fails OPEN (a clock-read failure never suppresses) and is a
# SUCCESS exit when it does suppress. --skip-quiet-hours ("explicitly asked")
# bypasses it.
QUIET_SUPPRESS=false
QUIET_REASON=""
if $QUIET_HOURS; then
    if QUIET_REASON="$(quiet_hours_should_suppress)"; then
        QUIET_SUPPRESS=true
    fi
fi

if $DRY_RUN; then
    if ! $QUIET_HOURS; then
        echo "[dry-run] Release quiet-hours: skipped (--skip-quiet-hours)"
    elif $QUIET_SUPPRESS; then
        echo "[dry-run] Release quiet-hours: WOULD SUPPRESS this cut — ${QUIET_REASON}"
        echo "[dry-run]   (self-clearing: resumes automatically after 18:00 Pacific; --skip-quiet-hours to force)"
    else
        echo "[dry-run] Release quiet-hours: clear (would cut) — outside PST business hours (or weekend)"
    fi
    if ! $BREAKER; then
        echo "[dry-run] Deploy-failure breaker: skipped (--skip-breaker)"
    elif $BREAKER_SUPPRESS; then
        echo "[dry-run] Deploy-failure breaker: WOULD SUPPRESS this cut — ${BREAKER_REASON}"
        echo "[dry-run]   (self-clearing: resumes automatically once the tag deploys green or the blocker closes)"
    else
        echo "[dry-run] Deploy-failure breaker: clear (would cut) — last tag ${LAST_TAG:-<none>} not deploy-blocked"
    fi
    echo "[dry-run] Would fast-forward HEAD onto origin/main and verify it is the live tip (fresh-base guard, ka-0000001299)"
    echo "[dry-run] Would bump gardenio/helm/gardenio/Chart.yaml to ${VERSION}"
    echo "[dry-run] Would bump gardenio_chart_version/gardenio_image_tag in prod-real.tfvars to ${VERSION} (Option B: terraform-driven prod deploy)"
    echo "[dry-run] Would commit, tag ${VERSION}, and $( $PUSH && echo 'push (git push --atomic main + tag, all remotes)' || echo 'stop before push (--no-push)' )"
    # Signing preview (F4, ka-0000005321) — presence-only, no side effects. The
    # real cut validates the key (fail-open) after the lock; here we only report
    # whether one is configured.
    if [[ -n "${RELEASE_SIGNING_KEY_B64:-}" || -n "${RELEASE_SIGNING_KEY:-}" ]]; then
        echo "[dry-run] Release signing: key configured — would SSH-sign the commit + tag if the key is usable (fail-open; see gardenio/docs/release-signing.md)"
    else
        echo "[dry-run] Release signing: no key configured — would cut UNSIGNED (fail-open). Set RELEASE_SIGNING_KEY[_B64]; see gardenio/docs/release-signing.md"
    fi
    echo "[dry-run] Preflight: $( $PREFLIGHT && echo "enabled (build + lint + test via RWX, ${RWX_RUN_TIMEOUT}s timeout)" || echo 'skipped' )"
    if $MIRROR && $MIRROR_NEEDED; then
        echo "[dry-run] Mirror gate: dispatch gardenio-mirror-images (${MIRROR_REASON})"
    elif ! $MIRROR; then
        echo "[dry-run] Mirror gate: skipped (--skip-mirror)"
    else
        echo "[dry-run] Mirror gate: skipped (.rwx/mirror.yml unchanged since ${LAST_TAG})"
    fi
    echo ""
    echo "Release pipeline (triggered by tag push):"
    echo "  1. RWX docker.yml  → build all images → push to GHCR"
    echo "  2. RWX docker.yml  → deploy to gardenio-rwx (staging)"
    echo "  3. RWX docker.yml  → E2E tests against gardenio-rwx"
    echo "  4. RWX docker.yml  → gate-prod-deploy → trigger GitLab acme-helm-chart deploy"
    echo "  5. RWX helm.yml    → package + push Helm chart to GHCR OCI"
    echo "  6. GitLab CI       → create GitLab Release"
    exit 0
fi

# ── Release quiet-hours gate (ka-0000004693) ───────────────────────────
# Enforce the PST-business-hours policy BEFORE any expensive/mutating step
# (checked first, ahead of the breaker: it is the cheapest suppression and the
# most common one during a weekday). When the current Pacific time is inside
# business hours on a weekday, exit cleanly (rc 0) with a "held" verdict rather
# than cutting. This is a SUCCESS exit, not a failure — the cut was correctly
# withheld per operator policy. SELF-CLEARING: the next cycle after 18:00 PT
# cuts automatically. Override with --skip-quiet-hours for a deliberate/asked
# cut (this is what the manual operator-fire trigger entry maps to).
if $QUIET_SUPPRESS; then
    echo "=== Release quiet hours IN EFFECT — holding this cut (ka-0000004693) ==="
    echo "  ${QUIET_REASON}"
    echo ""
    echo "  Operator policy: no gardenio releases during PST business hours"
    echo "  (07:00-18:00 Pacific, weekdays) unless explicitly asked. No tag was cut."
    echo "  SELF-CLEARING: the next run resumes cutting automatically after 18:00 Pacific."
    echo "  Override (deliberate/asked cut):"
    echo "    ./scripts/release.sh --skip-quiet-hours"
    exit 0
fi
if ! $QUIET_HOURS; then
    echo "=== Release quiet-hours gate skipped (--skip-quiet-hours) ==="
fi

# ── Deploy-failure circuit-breaker gate (ka-0000007280) ────────────────
# Enforce the Step-4.0 breaker BEFORE the single-flight lock, mirror gate,
# preflight, and tag+push — the first point a cut would start burning real cost.
# When BOTH conditions hold (last tag is an un-promoted red-rc AND an open
# release-blocker holds) a re-cut cannot promote, so we exit cleanly (rc 0) with
# a "suppressed" verdict rather than tagging into a wedged, operator-gated deploy
# path. This is a SUCCESS exit, not a failure: the cut was correctly withheld
# (doc ka-0000000327 Step 4.0 — a breaker-suppressed cycle is a SUCCESS cycle).
# The breaker is recomputed live every run and self-clears the instant EITHER
# condition lifts (the tag deploys green, or the blocker closes) — no manual
# re-enable. Override with --skip-breaker for a deliberate operator cut (e.g.
# the cut that carries the deploy fix itself).
if $BREAKER_SUPPRESS; then
    echo "=== Deploy-failure circuit-breaker ENGAGED — suppressing this cut (ka-0000007280) ==="
    echo "  ${BREAKER_REASON}"
    echo ""
    echo "  Re-cutting into a known-broken, operator-gated deploy path only burns a"
    echo "  full ~98-image RWX build with zero promotion. No tag was cut."
    echo "  SELF-CLEARING: the next run resumes cutting automatically the instant the"
    echo "  latest tag deploys green OR the release-blocker escalation closes."
    echo "  Override (deliberate operator cut, e.g. carrying the deploy fix):"
    echo "    ./scripts/release.sh --skip-breaker"
    exit 0
fi
if ! $BREAKER; then
    echo "=== Deploy-failure circuit-breaker skipped (--skip-breaker) ==="
fi

# ── Single-flight lock: prevent concurrent captains double-cutting ────
# Ephemeral captain-release-owner pods spawned by the 30-min schedule can
# overlap, so two captains may both pass the cut predicates and both run
# this script for the same version (the 2026.149.15 double-cut,
# ka-0000003407). Acquire a distributed lock on origin BEFORE the first
# expensive/mutating step (the mirror gate). If a live peer holds it, exit
# cleanly with EX_TEMPFAIL(75) so the captain backs off instead of cutting.
# The lock auto-releases on exit and self-expires via TTL if this pod dies.
LOCK_OID=""
release_lock_cleanup() {
    if $LOCK && [[ -n "$LOCK_OID" ]]; then
        RELEASE_LOCK_OID="$LOCK_OID" bash "${SCRIPT_DIR}/release-lock.sh" release || true
    fi
}
if $LOCK; then
    echo "=== Acquiring release single-flight lock (ka-0000003407) ==="
    # OK: disable errexit only to capture release-lock.sh's exit code in lock_rc;
    # rc 75 (held) and any non-zero are handled explicitly below, then set -e is restored.
    set +e
    LOCK_OID="$(bash "${SCRIPT_DIR}/release-lock.sh" acquire)"
    lock_rc=$?
    set -e
    if [[ $lock_rc -eq 75 ]]; then
        echo "Another release-owner holds the lock — backing off without cutting."
        echo "(Run with --skip-lock only if you KNOW no other captain is alive.)"
        exit 75
    elif [[ $lock_rc -ne 0 ]]; then
        echo "release-lock acquire failed (rc=${lock_rc}) — aborting to be safe." >&2
        exit 1
    fi
    trap release_lock_cleanup EXIT
    echo "=== Lock acquired ==="

    # Now that we hold the lock, recompute the version authoritatively from the
    # remote tag list. This closes the window where our first read was stale (a
    # peer cut between our spawn and our acquire), which is exactly how the
    # double-cut produced two release commits for one version. With the lock
    # held, no peer can cut concurrently, so this recompute is the single source
    # of truth for the version we burn.
    #
    # compute_version reads ref NAMES via `git ls-remote` (see
    # remote_release_tags) — it pulls no git objects, so there is NO
    # `git fetch --tags` here. That fetch used to hang indefinitely in
    # index-pack on the release-owner's shallow --no-tags clone and wedged the
    # whole release lane (ka-0000003116 / ka-0000005733); the name-only read is
    # both hang-proof and timeout-bounded.
    NEW_VERSION="$(compute_version)"
    if [[ "$NEW_VERSION" != "$VERSION" ]]; then
        echo "Version advanced under lock: ${VERSION} -> ${NEW_VERSION} (a peer cut while we waited)."
        VERSION="$NEW_VERSION"
    fi
    echo "Release version (locked): ${VERSION}"
else
    echo "=== Release lock skipped (--skip-lock) ==="
fi

# ── Release signing setup (ka-0000005321, F4) ─────────────────────────────
# Resolve the signing key once, here, so the commit/tag step below just consults
# SIGN_COMMITS/SIGN_GIT_ARGS. Fail-open: an absent/broken key logs a warning and
# the cut proceeds unsigned rather than aborting the release lane. Compose the
# key-shred cleanup with any lock cleanup already trapped (don't clobber it).
configure_release_signing
if [[ -n "$SIGNING_KEY_TMP" ]]; then
    trap 'release_signing_cleanup; release_lock_cleanup' EXIT
fi
if $SIGN_COMMITS; then
    echo "=== Release signing: ${SIGN_STATUS} ==="
else
    echo "=== Release signing: ${SIGN_STATUS} ===" >&2
    echo "    Proceeding UNSIGNED (fail-open). Provision a key via RELEASE_SIGNING_KEY[_B64]" >&2
    echo "    to sign the release commit + tag; see gardenio/docs/release-signing.md." >&2
fi

# ── One cut attempt: fresh-base guard → mirror/preflight → bump/tag → push ──
# Factored into a function so the bounded retry loop below can re-run it after a
# lost-race push rejection WITHOUT the release-owner captain having to re-invoke
# the whole script by hand. Returns:
#   0                       — published (or --no-push staged) successfully
#   $CUT_RETRY_LOST_RACE(10)— atomic push rejected non-fast-forward (main
#                             advanced under us); HEAD has been rolled back to
#                             the pre-bump base and the local tag deleted, so the
#                             caller can fast-forward and re-cut cleanly
# Any HARD failure (diverged base, mirror-gate red, preflight red/stall) still
# `exit`s the whole script from inside here — those are NOT retryable races.
#
# Reads globals: VERSION, MIRROR/MIRROR_NEEDED/MIRROR_REASON, PREFLIGHT, PUSH,
# RELEASE_REMOTE, LAST_TAG, RWX_RUN_TIMEOUT, LAST_VALIDATED_BASE (updated here).
CUT_RETRY_LOST_RACE=10
attempt_cut_and_push() {
    local base_sha pf_rc r
    # ── HEAD-on-main guard (ka-0000002455) ────────────────────────────────
    # Cutting from a feature branch leaves the local `main` ref stale (the
    # fresh-base fast-forward below moves HEAD, not `main`), which turns the
    # atomic push of `main` into a non-fast-forward rejection the retry loop
    # mis-diagnoses. Refuse fast, before the fast-forward masks the branch and
    # before any preflight minutes are spent. See assert_head_on_main.
    assert_head_on_main || exit 1
    # ── Fresh-base guard (ka-0000001299) ──────────────────────────────────
    # Ensure the commit we are about to bump/tag is the live tip of origin/main
    # BEFORE the mirror/preflight gates — so those gates validate the exact
    # commit that gets tagged, and a stale-base cut never reaches the push. Tag
    # 2026.200.15 orphaned off-main because it was cut on a base main had already
    # advanced past. We first try a SAFE fast-forward (recovers the ordinary
    # "main advanced during my preflight" race without going dark), then
    # hard-verify. If HEAD still is not the live tip — a genuine divergence or an
    # unreadable remote — refuse here, before any commit/tag/push.
    echo "=== Fresh-base guard: syncing HEAD to the live tip of ${RELEASE_REMOTE}/main ==="
    if ! try_fast_forward_main; then
        echo "Fresh-base guard: could not fast-forward onto ${RELEASE_REMOTE}/main (diverged or unreachable)." >&2
    fi
    if ! assert_on_fresh_main "$(git rev-parse HEAD)"; then
        echo "FRESH-BASE GUARD: local HEAD is not the live tip of ${RELEASE_REMOTE}/main — refusing to cut." >&2
        echo "  local HEAD:      $(git rev-parse HEAD)" >&2
        echo "  ${RELEASE_REMOTE}/main tip: $(remote_main_tip)" >&2
        echo "  A clean fast-forward was not possible (local commits diverge from main, or" >&2
        echo "  the remote is unreadable). Reconcile and re-run:" >&2
        echo "    git fetch ${RELEASE_REMOTE} main && git reset --hard ${RELEASE_REMOTE}/main && ./scripts/release.sh" >&2
        echo "  (Cutting on a stale base is how tag 2026.200.15 orphaned off-main — ka-0000001299.)" >&2
        exit 1
    fi
    echo "=== Fresh-base guard passed (HEAD is the live tip of ${RELEASE_REMOTE}/main) ==="
    base_sha="$(git rev-parse HEAD)"

    # ── HEAD-on-remote guard (ka-0000003172) ──────────────────────────────
    # Every RWX dispatch below clones the repo by HEAD's SHA (`.rwx/ci.yml` +
    # `.rwx/mirror.yml` `code`/`git/clone` tasks with `ref: init.commit-sha`). If
    # HEAD is a local-only commit origin has never seen, those clones fail
    # deterministically with "not our ref" on every retry (the 8acfdac1/3a3aabc2
    # preflight wedge). Refuse to burn RWX minutes on an unfetchable SHA — fail
    # fast with a push-first message instead. (After the fresh-base guard HEAD is
    # the live origin/main tip, so this normally passes trivially; it still
    # catches a local-only commit on manual --skip-lock cuts where the base check
    # was skipped.)
    echo "=== Verifying HEAD is on ${RELEASE_REMOTE} (RWX clones by SHA) ==="
    require_head_on_remote || exit 1

    # ── Mirror + preflight gates (re-run ONLY when the base changed) ──────
    # The gates below dispatch RWX runs that validate the EXACT tree at $base_sha
    # (build/lint/test + the image mirror). They are the expensive part of a cut
    # — minutes of RWX. On a retry after a lost-race push, the fresh-base guard
    # above may have fast-forwarded us onto a NEW origin/main tip (a peer MR
    # merged during our preflight); that new merged tree has NOT been validated,
    # so we MUST re-run the gates on it — this is never a --skip-preflight bypass
    # (ka-0000007506, operator directive "don't skip preflight tho, fix"). But
    # when the base is UNCHANGED since the last validated run (e.g. the push
    # failed for a transient reason, or main did not actually move), re-running a
    # full RWX build/lint/test is pure churn — the exact redundant-work shape this
    # retry loop exists to kill (ka-0000007981). So we gate the re-run on a real
    # base change and otherwise reuse the validation we already paid for.
    if [[ "$base_sha" != "$LAST_VALIDATED_BASE" ]]; then
        # ── Mirror gate: dispatch gardenio-mirror-images if .rwx/mirror.yml ────
        # has new/changed entries since the last release tag. crane copy is
        # idempotent, but we only burn an RWX run when the file actually
        # changed — keeping the common no-op path free. Cannot be bypassed by
        # --skip-preflight (this is a separate, mandatory gate). Use
        # --skip-mirror for emergencies (ka-0000001365).
        if $MIRROR && $MIRROR_NEEDED; then
            echo "=== Running mirror gate: ${MIRROR_REASON} ==="
            echo "Dispatching gardenio-mirror-images (.rwx/mirror.yml) — required before tagging."
            if ! rwx_run_bounded .rwx/mirror.yml \
                --init commit-sha="$base_sha" \
                --target mirror-images \
                --wait --fail-fast; then
                echo ""
                echo "MIRROR GATE FAILED — fix the failures above before releasing."
                echo "Re-run after the underlying issue is resolved, or use --skip-mirror"
                echo "if you have manually verified all .rwx/mirror.yml entries are"
                echo "present in the mirror registry."
                exit 1
            fi
            echo "=== Mirror gate passed ==="
        elif ! $MIRROR; then
            echo "=== Mirror gate skipped (--skip-mirror) ==="
        else
            echo "=== Mirror gate skipped (.rwx/mirror.yml unchanged since ${LAST_TAG}) ==="
        fi

        # ── Preflight: catch build/lint/test failures before burning a tag ────
        if $PREFLIGHT; then
            echo "=== Running preflight (build + lint + test via RWX) ==="
            echo "This catches failures before tagging. Use --skip-preflight to bypass."
            # Target list is DERIVED from .rwx/ci.yml — the union of the
            # `CI / Build` + `CI / Lint` + `CI / Test` status checks GitLab
            # enforces on an MR. This used to be a hand-enumerated 11-task list
            # (identical to the one in the root Makefile, so the two agreed with
            # each other while BOTH under-approximated CI by 40 tasks — a
            # chart-only change passed preflight and reddened CI / Test in
            # test-helm, ka-0000004412). A failure to derive it ABORTS the cut:
            # a silently narrowed preflight tags an under-validated commit,
            # which is the same class of harm as --skip-preflight.
            if ! pf_task_list="$("${SCRIPT_DIR}/preflight-targets.sh" --tasks)"; then
                echo ""
                echo "PREFLIGHT ABORTED — could not derive the target list from .rwx/ci.yml (see above)."
                echo "Nothing was tagged. Fix .rwx/ci.yml (or scripts/preflight-targets.sh) and re-run."
                exit 1
            fi
            pf_targets=()
            while IFS= read -r pf_task; do
                [[ -n "$pf_task" ]] && pf_targets+=(--target "$pf_task")
            done <<< "$pf_task_list"
            echo "Preflight: dispatching ${#pf_targets[@]} targets derived from .rwx/ci.yml" \
                 "(CI / Build + CI / Lint + CI / Test)."
            pf_rc=0
            rwx_run_bounded .rwx/ci.yml \
                --init commit-sha="$base_sha" \
                "${pf_targets[@]}" \
                --wait --fail-fast || pf_rc=$?
            if [[ $pf_rc -eq 124 ]]; then
                # STALL, not a red preflight. rwx_run_bounded already printed the
                # "exceeded RELEASE_RWX_RUN_TIMEOUT / stalled in_progress" guidance
                # + the RWX-console link. The dispatched run may well succeed on
                # its own (a slow queue / eventual-consistency lag misread as a
                # hang is exactly the preflight-hang cluster, ka-0000005933 et al.).
                # Do NOT frame this as "PREFLIGHT FAILED" and do NOT suggest
                # --skip-preflight: skipping ships an UNVALIDATED build. The correct
                # recovery is to re-run the cut once the stall clears (operator
                # directive, Matt Baker 2026-07-22: "don't skip preflight tho,
                # fix"). Fail the cut so nothing is tagged.
                echo ""
                echo "PREFLIGHT DID NOT COMPLETE (RWX run stalled) — this is NOT a red build."
                echo "The build/lint/test result is unknown; re-run the release once the"
                echo "stalled run clears. Do NOT use --skip-preflight to work around a"
                echo "stall — that tags an unvalidated commit."
                exit 1
            elif [[ $pf_rc -ne 0 ]]; then
                echo ""
                echo "PREFLIGHT FAILED — fix the errors above before releasing."
                echo "Run 'make preflight' to debug."
                exit 1
            fi
            echo "=== Preflight passed ==="
        fi
        LAST_VALIDATED_BASE="$base_sha"
    else
        echo "=== Base unchanged since last validated cut ($(git rev-parse --short "$base_sha")) — reusing that mirror/preflight result (no redundant RWX re-run) ==="
    fi

    # Bump Chart.yaml version + appVersion
    CHART_FILE="gardenio/helm/gardenio/Chart.yaml"
    sed -i.bak "s/^version:.*/version: ${VERSION}/" "$CHART_FILE"
    sed -i.bak "s/^appVersion:.*/appVersion: ${VERSION}/" "$CHART_FILE"
    rm -f "${CHART_FILE}.bak"

    # Bump the prod chart pin (Option B, P4.4): production is deployed by
    # `terraform apply` of composition/canonical-prod (module.gardenio), whose
    # chart version is the `gardenio_chart_version` / `gardenio_image_tag` pins in
    # prod-real.tfvars — NOT a `helm upgrade`. Bumping them here, in the release
    # commit, makes the release tag the single source of truth: the deploy-prod CI
    # task runs a bare `terraform apply -var-file=prod-real.tfvars`, so there is no
    # -var override to drift and no bare infra apply can revert the chart version.
    # Whole-line replace (stable comment) so a version-specific note can't go stale.
    PROD_TFVARS="gardenio/terraform/composition/canonical-prod/prod-real.tfvars"
    if [[ -f "$PROD_TFVARS" ]]; then
        sed -i.bak -E "s|^gardenio_chart_version[[:space:]]*=.*|gardenio_chart_version = \"${VERSION}\" # bumped by scripts/release.sh (Option B: terraform-driven prod deploy)|" "$PROD_TFVARS"
        sed -i.bak -E "s|^gardenio_image_tag[[:space:]]*=.*|gardenio_image_tag     = \"${VERSION}\" # lockstep with the chart pin; bumped by scripts/release.sh|" "$PROD_TFVARS"
        rm -f "${PROD_TFVARS}.bak"
    fi

    # Commit and tag. attempt_cut_and_push is invoked under `|| cut_rc=$?` by the
    # retry loop, which suppresses `set -e` INSIDE this function — so these
    # mutations no longer abort the cut automatically on failure the way the old
    # linear code relied on. Guard them explicitly: a failed commit or a `git tag`
    # collision (e.g. a stale local ${VERSION} tag left by an aborted prior run)
    # must hard-exit, never fall through to a push that could publish the wrong
    # ref (ka-0000007981 review).
    # Sign the commit + tag when a key was resolved (ka-0000005321, F4).
    # SIGN_GIT_ARGS carries `-c gpg.format=ssh -c user.signingkey=<path>` (empty
    # when unsigned); `-S`/`-s` request the signature. An annotated `git tag -s`
    # (vs the old lightweight tag) also gives the tag a proper tagger + message,
    # which the release pipeline is agnostic to (it triggers on the ref name).
    local commit_sign_flag="" tag_sign_flag=""
    if $SIGN_COMMITS; then
        commit_sign_flag="-S"
        tag_sign_flag="-s"
    fi
    git add "$CHART_FILE" "$PROD_TFVARS" || { echo "RELEASE ABORTED — git add failed." >&2; exit 1; }
    # Expand with the portable empty-array guard: macOS ships bash 3.2, where
    # "${arr[@]}" on an EMPTY array trips `set -u` (unbound variable). SIGN_GIT_ARGS
    # is empty on every unsigned cut, so a bare expansion aborts the release mid-bump
    # on a Mac (ka-0000005650). ${arr[@]+"${arr[@]}"} expands to nothing when unset
    # and to the quoted elements otherwise, on both bash 3.2 and 4.4+.
    git ${SIGN_GIT_ARGS[@]+"${SIGN_GIT_ARGS[@]}"} commit ${commit_sign_flag} -m "chore: release ${VERSION}" \
        || { echo "RELEASE ABORTED — git commit failed." >&2; exit 1; }
    if ! git ${SIGN_GIT_ARGS[@]+"${SIGN_GIT_ARGS[@]}"} tag ${tag_sign_flag} -m "release ${VERSION}" "${VERSION}"; then
        echo "RELEASE ABORTED — git tag ${VERSION} failed (a stale local tag may already exist)." >&2
        echo "  Drop it and re-run: git tag -d ${VERSION} && ./scripts/release.sh" >&2
        exit 1
    fi

    if ! $PUSH; then
        # --no-push: print the ATOMIC command (never the orphan-prone bare push).
        echo "Tagged ${VERSION} locally (--no-push). Publish atomically with:"
        echo "  git push --atomic ${RELEASE_REMOTE} main ${VERSION}"
        for r in $(git remote); do
            [[ "$r" == "$RELEASE_REMOTE" ]] && continue
            echo "  git push --atomic ${r} main ${VERSION}    # mirror"
        done
        echo ""
        echo "IMPORTANT: use --atomic so main + tag land together. A bare"
        echo "'git push <remote> main ${VERSION}' can orphan the tag off-main (ka-0000001299)."
        return 0
    fi

    # ── Atomic push (ka-0000001299) ───────────────────────────────────────
    # Publish the branch and the tag in a SINGLE `git push --atomic` transaction
    # so either both refs land or neither does. A bare `git push <remote> main
    # <TAG>` pushes the two refs independently: if the `main` update is rejected
    # as non-fast-forward (main advanced under us) the tag ref STILL lands,
    # leaving the tag pointing at a commit unreachable from main — exactly how
    # 2026.200.15 orphaned itself. --atomic makes that impossible: a rejected main
    # update also rejects the tag, and the whole release fails loudly instead of
    # shipping a broken tag. --atomic is server-side and both GitLab and GitHub
    # honour it.
    #
    # On the release-owner pod the tag ref cut here is a NEW ref, and the branch
    # update is a fast-forward from the fresh base the guard verified, so the
    # atomic push is the normal happy path — the guard only bites when something
    # raced us. When it DOES bite (a peer landed on main during our multi-minute
    # preflight), we do NOT hand the captain a manual re-cut command: we roll the
    # cut back to the pre-bump base and return CUT_RETRY_LOST_RACE so the bounded
    # loop below re-syncs and re-cuts automatically (ka-0000007981).
    echo "=== Atomic push to authoritative remote '${RELEASE_REMOTE}' (main + ${VERSION} together) ==="
    if ! git push --atomic "$RELEASE_REMOTE" main "${VERSION}"; then
        echo "ATOMIC PUSH TO '${RELEASE_REMOTE}' rejected — main likely advanced under this cut." >&2
        echo "  This is the guard working: the whole transaction was rejected, so" >&2
        echo "  nothing was orphaned and nothing published. Will re-sync + re-cut." >&2
        # Roll the local cut back to the fresh base so the retry re-runs the
        # fresh-base guard (fast-forwarding onto the new tip) and re-cuts cleanly,
        # recomputing a fresh sequence rather than colliding with this abandoned
        # local tag.
        git tag -d "${VERSION}" >/dev/null 2>&1 || true
        git reset --hard "$base_sha" >/dev/null 2>&1 || true
        return "$CUT_RETRY_LOST_RACE"
    fi
    echo "=== Released ${VERSION}: main + tag pushed atomically to '${RELEASE_REMOTE}' ==="

    # Mirror to any other remotes best-effort — still --atomic (never orphan a
    # mirror either), but a failure here does not unpublish the release.
    for r in $(git remote); do
        [[ "$r" == "$RELEASE_REMOTE" ]] && continue
        echo "=== Mirroring to '${r}' (best-effort, atomic) ==="
        if ! git push --atomic "$r" main "${VERSION}"; then
            echo "WARNING: mirror push to '${r}' failed — release already published to" >&2
            echo "  '${RELEASE_REMOTE}'. Re-push the mirror by hand if it is needed:" >&2
            echo "    git push --atomic ${r} main ${VERSION}" >&2
        fi
    done
    return 0
}

# ── Bounded automatic cut+push retry loop (ka-0000007981) ─────────────
# A release cut is fresh-base guard → mirror/preflight (minutes of RWX) →
# bump/commit/tag → atomic push. On a busy main a peer MR can merge into
# origin/main during the preflight window, so the atomic push is correctly
# rejected as non-fast-forward when it finally runs. Previously the script exited
# here and printed `git reset --hard origin/main && ./scripts/release.sh`, so the
# release-owner captain re-ran the ENTIRE script by hand — repeating the full RWX
# preflight every time. On a busy main that turns one cut into a storm: a
# CED-audited cycle (session WIN0y8RR51) ran ./scripts/release.sh 8× and
# git push 5×, each fighting the same non-fast-forward rejection.
#
# We now recover the lost race IN-SCRIPT, automatically and BOUNDED: on a
# non-fast-forward rejection attempt_cut_and_push rolls back to the fresh base
# and returns CUT_RETRY_LOST_RACE; we re-sync (the next attempt's fresh-base
# guard fast-forwards onto the new tip), recompute the version, and re-cut — up
# to RELEASE_PUSH_MAX_ATTEMPTS times, still holding the single-flight lock so no
# peer can double-cut while we retry. The expensive gates re-run only on a real
# base change (see attempt_cut_and_push), never as a preflight bypass. After the
# cap we fall back to the same loud manual message, so a genuinely stuck lane
# still surfaces instead of spinning forever.
PUSH_MAX_ATTEMPTS="${RELEASE_PUSH_MAX_ATTEMPTS:-3}"
LAST_VALIDATED_BASE=""
cut_attempt=1
while : ; do
    cut_rc=0
    attempt_cut_and_push || cut_rc=$?
    if [[ $cut_rc -eq 0 ]]; then
        break
    fi
    # Only CUT_RETRY_LOST_RACE reaches here — every hard failure `exit`s from
    # inside attempt_cut_and_push. main advanced under us; retry if we have budget.
    if [[ $cut_attempt -ge $PUSH_MAX_ATTEMPTS ]]; then
        echo "" >&2
        # Distinguish the two non-fast-forward causes before prescribing a remedy
        # (ka-0000002455). assert_head_on_main at the top of every attempt normally
        # rules out a stale local `main`, but re-check here so a --skip-lock /
        # manual cut can never be handed the wrong remedy: if HEAD is not on `main`
        # (or the local `main` ref is not HEAD), the push rejects because we are
        # pushing a stale `main` ref — NOT because the lane is busy — and
        # `git reset --hard ${RELEASE_REMOTE}/main` cannot fix it while HEAD is elsewhere.
        if [[ "$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)" != "main" ]] \
            || [[ "$(git rev-parse --verify --quiet main 2>/dev/null)" != "$(git rev-parse HEAD)" ]]; then
            echo "ATOMIC PUSH rejected: the local 'main' ref is stale because HEAD is not on 'main'." >&2
            echo "  The cut landed on another branch and 'main' was never advanced, so pushing it" >&2
            echo "  is non-fast-forward. This is NOT a busy release lane. Check out main and cut there:" >&2
            echo "    git checkout main && git pull --ff-only ${RELEASE_REMOTE} main && ./scripts/release.sh" >&2
            exit 1
        fi
        echo "ATOMIC PUSH still rejected after ${PUSH_MAX_ATTEMPTS} attempts — ${RELEASE_REMOTE}/main is advancing faster than a cut completes." >&2
        echo "  Nothing was orphaned or published (every attempt was atomic). This is not a" >&2
        echo "  code bug — the release lane is unusually busy. Reconcile and re-run:" >&2
        echo "    git fetch ${RELEASE_REMOTE} main && git reset --hard ${RELEASE_REMOTE}/main && ./scripts/release.sh" >&2
        echo "  (Raise the retry budget with RELEASE_PUSH_MAX_ATTEMPTS=<n> if this recurs.)" >&2
        exit 1
    fi
    cut_attempt=$((cut_attempt + 1))
    echo "=== Lost the push race — re-syncing onto ${RELEASE_REMOTE}/main and re-cutting (attempt ${cut_attempt}/${PUSH_MAX_ATTEMPTS}) ==="
    # Recompute the version in case a tag appeared while we raced (under the lock
    # no peer cuts, so this is normally stable; it is a cheap ls-remote read and
    # guarantees the retry never collides with a freshly-landed tag).
    VERSION="$(compute_version)"
done
