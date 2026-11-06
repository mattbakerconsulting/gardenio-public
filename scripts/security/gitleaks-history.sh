#!/usr/bin/env bash
# gitleaks-history.sh — run the gitleaks HISTORY scan and PROVE it covered
# history, so a partial-coverage run can never read as a pass (ka-0000001621).
#
# THE BUG THIS DEFENDS AGAINST
# ----------------------------
# `gitleaks detect` (without --no-git) walks git history and needs every blob.
# On a checkout where the git walk cannot complete, gitleaks does NOT fail — it
# prints an error line and STILL EXITS 0, reporting "no leaks found":
#
#   * No `.git` dir at all (the shallow `code` clone does `rm -rf .git`):
#       ERR [git] fatal: not a git repository ...
#       ERR failed to scan Git repository error="stderr is not empty"
#       INF no leaks found            <-- and exit 0
#
#   * A `blob:none` partial clone with the promisor unreachable mid-walk:
#       ERR [git] fatal: unable to read <blob-sha>
#       ERR failed to scan Git repository error="stderr is not empty"
#       INF no leaks found            <-- and exit 0
#
# Both were reproduced with the pinned gitleaks (8.21.2). In every silent-noop
# mode gitleaks emits a `failed to scan Git repository` line and NO
# `N commits scanned` line — that is the exact, non-heuristic signature this
# wrapper trips on. It also refuses a shallow clone up front (truncated history
# is silent under-coverage with no error at all) and applies a coverage-ratio
# floor so a drastically-short walk cannot slip through even if a future
# gitleaks prints a partial count.
#
# WHY NOT ASSERT scanned == `git rev-list --count`
# ------------------------------------------------
# gitleaks' "commits scanned" count legitimately differs from rev-list: it
# skips merge commits, pure-deletion commits, and empty commits (verified).
# The healthy baseline is therefore the NON-MERGE reachable-commit count, and
# the floor is a fraction of that — generous enough never to false-red a
# healthy full clone, strict enough to catch a ~15%-coverage regression.
#
# Requires: a FULL, .git-preserving clone, NOT a shallow one (checks.yml's
# `gitleaks history` job checks out with fetch-depth: 0).
#
# RANGE MODE (GITLEAKS_LOG_OPTS=<base>..<head>, gd-5iGDOjVCl7)
# -------------------------------------------------------------
# checks.yml sets this on a pull_request to `origin/<base_ref>..HEAD`, so the
# scan reads only the PR's own commits instead of all ~7000 (the history walk
# was ~85% of the job). It is passed to gitleaks as --log-opts, which REPLACES
# gitleaks' default `--full-history --all`. Every guard above still applies;
# what changes is the baseline the floor is measured against:
#   * Both ends of the range must resolve to commits, or exit 2. A range that
#     names a ref the checkout never fetched would otherwise walk nothing and
#     report "0 commits scanned" as clean.
#   * The baseline is the range's non-merge commits that ADD at least one line.
#     A whole-history baseline would false-red every PR, and plain non-merge
#     count would false-red a deletion-only PR: gitleaks reports a commit
#     that only deletes as "0 commits scanned" (verified on 8.21.2).
#   * A range with nothing to scan (baseline 0) is a pass, but gitleaks must
#     still print its count line -- that is still the proof the walk ran.
# Only `<base>..<head>` is accepted: gitleaks splits --log-opts on spaces, so
# anything richer would mean two parsers reading one string.
#
# EXIT CODES (distinct so a caller can enforce coverage while treating findings
# as advisory during the rollout — the advisory→enforce ramp this file already
# uses for go-vuln/osv-scan/cargo):
#   0  clean: history provably covered, no leaks.
#   1  gitleaks found a secret (coverage was proven first). This is the scanner
#      VERDICT — a caller may soak it as advisory during rollout.
#   2  COVERAGE failure: absent/shallow .git, git-walk error, no count line, or
#      below-floor coverage. This must ALWAYS hard-fail — it is the silent
#      no-op this gate exists to catch and is never advisory.
set -uo pipefail

# Coverage failures exit 2 (always fatal); gitleaks findings exit 1 (advisory-
# swallowable). Keep these distinct from gitleaks' own 0/1.
COVERAGE_FAIL=2

# Non-tty CI already suppresses color, but pin it so the log greps stay robust.
export NO_COLOR=1

GITLEAKS_BIN="${GITLEAKS_BIN:-gitleaks}"
GITLEAKS_CONFIG="${GITLEAKS_CONFIG:-.gitleaks.toml}"
# Minimum fraction of non-merge reachable commits gitleaks must report scanning.
# Non-merge is gitleaks' natural ceiling; deletion-only/empty commits shave a
# little off, so a healthy repo sits well above this. 50% still catches the
# 2026-08-03 "~15% of commits then silently completed" failure by a wide margin.
FLOOR_PCT="${GITLEAKS_HISTORY_FLOOR_PCT:-50}"

fatal() { echo "gitleaks-history: FATAL — $*" >&2; exit "$COVERAGE_FAIL"; }

# ── Guard 1: a real git repository must be present ───────────────────────────
if ! git rev-parse --git-dir >/dev/null 2>&1; then
  fatal "no .git directory — the history scan needs a full clone (code-git), not the shallow .git-less 'code' clone. A working-tree-only scan cannot see secrets that were committed and later removed."
fi

# ── Guard 2: the clone must NOT be shallow ───────────────────────────────────
# A shallow clone silently truncates history with NO error from gitleaks.
if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
  fatal "shallow clone (git rev-parse --is-shallow-repository=true) — history is truncated; fetch full depth before scanning."
fi

# A blob:none partial clone is not fatal by itself (gitleaks may lazily fetch),
# but it is the setup for the silent-noop, so surface it loudly.
if [ -n "$(git config --get remote.origin.partialclonefilter 2>/dev/null || true)" ]; then
  echo "gitleaks-history: WARNING — remote.origin is a partial clone (filter=$(git config --get remote.origin.partialclonefilter)); relying on the git-walk-error guard below to catch any unfetchable blob." >&2
fi

LOG_OPTS="${GITLEAKS_LOG_OPTS:-}"
log_opts_args=()
if [ -n "$LOG_OPTS" ]; then
  # ── Range mode: baseline is the range's content-adding commits ───────────
  if ! [[ "$LOG_OPTS" =~ ^([^[:space:].][^[:space:]]*)\.\.([^[:space:]]+)$ ]]; then
    fatal "GITLEAKS_LOG_OPTS='$LOG_OPTS' is not a single <base>..<head> range — refusing to guess what gitleaks would scan."
  fi
  range_base="${BASH_REMATCH[1]}"; range_head="${BASH_REMATCH[2]}"
  for end in "$range_base" "$range_head"; do
    git rev-parse --verify --quiet "$end^{commit}" >/dev/null \
      || fatal "range end '$end' (GITLEAKS_LOG_OPTS='$LOG_OPTS') does not resolve to a commit — the checkout did not fetch it, so the range would scan nothing."
  done
  if ! numstat="$(git log --no-merges --numstat --format=@%H "$LOG_OPTS" 2>&1)"; then
    fatal "git log $LOG_OPTS failed: $numstat"
  fi
  # A numstat row is "<added>\t<deleted>\t<path>"; binary files read "-".
  expected_nonmerge="$(awk '/^@/ { c = $0; next } $1 ~ /^[0-9]+$/ && $1 > 0 && !seen[c]++ { n++ } END { print n + 0 }' <<<"$numstat")"
  in_range="$(git rev-list --count "$LOG_OPTS")"
  # --verbose prints each (redacted) finding with its file and commit. The full
  # scan stays terse: it would list the ~50 known fixture findings every run,
  # but a range's findings are the PR's own and its author needs to see where.
  log_opts_args=(--log-opts="$LOG_OPTS" --verbose)
  echo "gitleaks-history: RANGE $LOG_OPTS has $in_range commit(s) ($expected_nonmerge non-merge adding content); running range scan..."
else
  # ── Baseline: non-merge reachable commits (gitleaks' natural ceiling) ────────
  # rev-list walks commit objects only (present even on a blob:none clone), so
  # this is cheap and correct regardless of blob availability.
  expected_nonmerge="$(git rev-list --all --no-merges --count 2>/dev/null || echo 0)"
  total_all="$(git rev-list --all --count 2>/dev/null || echo 0)"
  if ! [ "$expected_nonmerge" -ge 1 ] 2>/dev/null; then
    fatal "no commits visible (git rev-list --all --no-merges --count=$expected_nonmerge) — history absent, refusing to pass."
  fi
  echo "gitleaks-history: repo has $total_all reachable commit(s) ($expected_nonmerge non-merge); running history scan..."
fi

# ── Run the history scan (NO --no-git — this walks git history, all of it
# unless range mode narrowed it) ─────────────────────────────────────────────
out="$(mktemp)"
trap 'rm -f "$out"' EXIT
"$GITLEAKS_BIN" detect \
  --no-banner \
  --redact \
  --exit-code=1 \
  ${log_opts_args[@]+"${log_opts_args[@]}"} \
  --config "$GITLEAKS_CONFIG" >"$out" 2>&1
rc=$?
cat "$out" >&2

# ── Guard 3: the git walk must not have errored ──────────────────────────────
# The silent-noop signature: gitleaks prints this then exits 0. Treat it as
# hard failure regardless of exit code.
if grep -qE 'failed to scan Git repository|fatal: (unable to read|not a git repository)' "$out"; then
  fatal "the git-history walk errored (see output above) — coverage is not trustworthy. A blob-unavailable / promisor-failure condition must fail loudly, not report a pass."
fi

# ── Guard 3b: the scan must not have been killed mid-walk ───────────────────
# An interrupted gitleaks ALSO prints no 'commits scanned' line, so without
# this guard Guard 4 fires and blames a silent no-op. That accusation is
# wrong and expensive: it sends the reader after a gitleaks bug when the
# actual cause is the runner going away. Observed 2026-09-22 on run
# 35690309004 — spot node ip-10-90-10-242 was reclaimed 30s into the walk
# and the job died with "The runner has received a shutdown signal".
# Still exit 2: an interrupted walk is not coverage. Only the story changes.
if grep -q 'Interrupt signal received' "$out"; then
  fatal "gitleaks was interrupted mid-walk (signal), so the history scan never completed. This is NOT a gitleaks no-op and NOT a coverage regression — the runner went away (spot reclaim, eviction, or job cancellation). Re-run the job."
fi

# ── Guard 4: gitleaks must report a scanned-commit count ─────────────────────
# In every silent-noop mode gitleaks emits NO 'commits scanned' line.
scanned="$(grep -oE '[0-9]+ commits scanned' "$out" | grep -oE '^[0-9]+' | tail -1 || true)"
if [ -z "$scanned" ]; then
  fatal "gitleaks printed no 'N commits scanned' line — the history walk did not run. This is the silent no-op the gate exists to catch."
fi

# ── Guard 5: coverage-ratio floor ────────────────────────────────────────────
# floor = ceil(expected_nonmerge * FLOOR_PCT / 100)
floor=$(( (expected_nonmerge * FLOOR_PCT + 99) / 100 ))
if [ "$scanned" -lt "$floor" ]; then
  fatal "scanned $scanned commit(s), below the floor of $floor ($FLOOR_PCT% of $expected_nonmerge non-merge commits). History coverage is PARTIAL — a partial scan must never read as a pass (ka-0000001621)."
fi

echo "gitleaks-history: OK — scanned $scanned commit(s) (>= floor $floor of $expected_nonmerge non-merge); history coverage proven."

# Preserve gitleaks' verdict: 0 = clean, 1 = leak found. A real finding still reds.
exit "$rc"
