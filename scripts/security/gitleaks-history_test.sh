#!/usr/bin/env bash
# Hermetic unit test for scripts/security/gitleaks-history.sh (ka-0000001621).
#
# Drives the wrapper with a STUB `gitleaks` binary (no network, no real
# gitleaks, no repo blobs) over throwaway git repos, asserting the wrapper's
# exit code for each coverage scenario. The stub reproduces the real gitleaks
# output shapes verified against v8.21.2:
#   * clean full walk : "<n> commits scanned." + "no leaks found", exit 0
#   * leak found      : "<n> commits scanned." + a finding,        exit 1
#   * git-walk error  : "failed to scan Git repository ...",       exit 0 (!!)
#   * no .git present : same failed-to-scan line,                  exit 0 (!!)
# The stub honours --log-opts=<range> (range mode, gd-5iGDOjVCl7) by counting
# that range instead of --all.
#
# The REAL-gitleaks cases at the bottom are the positive control for range
# mode: a planted fake key in the PR's commits must still be caught, and one
# that is only in the base must not be. They need a real gitleaks on PATH;
# under GitHub Actions an absent one is a failure, not a skip.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER="$SCRIPT_DIR/gitleaks-history.sh"
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass=0; fail=0
ok()   { echo "  PASS $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL $1" >&2; fail=$((fail+1)); }

# make_stub <dir> <mode>: write a fake `gitleaks` onto PATH that emits the
# output shape for <mode> and exits with gitleaks' (buggy) real code.
make_stub() {
  local dir="$1" mode="$2"
  mkdir -p "$dir/bin"
  cat > "$dir/bin/gitleaks" <<STUB
#!/usr/bin/env bash
# stub gitleaks — mode=$mode
mode="$mode"
# Count real reachable commits so the "N commits scanned" line is realistic.
range=""
for a in "\$@"; do case "\$a" in --log-opts=*) range="\${a#--log-opts=}" ;; esac; done
if [ -n "\$range" ]; then
  n=\$(git rev-list --no-merges --count "\$range" 2>/dev/null || echo 0)
else
  n=\$(git rev-list --all --no-merges --count 2>/dev/null || echo 0)
fi
case "\$mode" in
  clean)
    echo "\$n commits scanned."
    echo "scan completed in 5ms"
    echo "no leaks found"
    exit 0 ;;
  leak)
    echo "\$n commits scanned."
    echo "Finding: AKIA...redacted"
    echo "scan completed in 5ms"
    exit 1 ;;
  short)
    # reports scanning only a small fraction, then "no leaks" + exit 0
    echo "1 commits scanned."
    echo "no leaks found"
    exit 0 ;;
  zero)
    # what real gitleaks prints for a range whose commits add nothing
    echo "0 commits scanned."
    echo "no leaks found"
    exit 0 ;;
  walkerr)
    echo "[git] fatal: unable to read deadbeef" >&2
    echo 'failed to scan Git repository error="stderr is not empty"' >&2
    echo "no leaks found"
    exit 0 ;;
  nogit)
    echo "[git] fatal: not a git repository (or any parent up to mount point /)" >&2
    echo 'failed to scan Git repository error="stderr is not empty"' >&2
    echo "no leaks found"
    exit 0 ;;
  nocount)
    # emits neither an error nor a commits-scanned line
    echo "no leaks found"
    exit 0 ;;
  interrupted)
    # killed mid-walk: gitleaks prints the FTL line and dies non-zero, so it
    # ALSO emits no 'commits scanned' line. Colors are reproduced because the
    # real run coloured despite NO_COLOR=1 -- the grep must not depend on them.
    printf '\033[90m5:21AM\033[0m \033[1m\033[31mFTL\033[0m\033[0m Interrupt signal received. Exiting...\n' >&2
    exit 130 ;;
esac
STUB
  chmod +x "$dir/bin/gitleaks"
}

# run_case <name> <mode> <expected_rc> <repo-setup-fn> [want-msg-substring]
# want-msg-substring: several distinct causes share exit 2, so a case that
# needs to prove WHICH guard fired must assert on the message too.
run_case() {
  local name="$1" mode="$2" want="$3" setup="$4" want_msg="${5:-}"
  local d="$TMPROOT/$name"; mkdir -p "$d"
  make_stub "$d" "$mode"
  ( cd "$d" && "$setup" )
  local got out
  out=$( { cd "$d/repo" 2>/dev/null || cd "$d"; } || exit 99
    PATH="$d/bin:$PATH" GITLEAKS_CONFIG=/dev/null GITLEAKS_LOG_OPTS="${CASE_LOG_OPTS:-}" bash "$WRAPPER" 2>&1 )
  got=$?
  if [ "$got" != "$want" ]; then bad "$name: want exit $want, got $got"; return; fi
  # Here-string, not `printf '%s' "$out" |`: grep -qF exits at the first match
  # without draining its input, and a write() to a pipe whose reader is gone
  # takes EPIPE immediately -- under `set -o pipefail` that flips the branch
  # even though grep matched. See scripts/check-sigpipe-producers.py, which
  # bans exactly this and caught this line on main.
  if [ -n "$want_msg" ] && ! grep -qF "$want_msg" <<<"$out"; then
    bad "$name: exit $got correct, but message did not contain '$want_msg'"; return
  fi
  ok "$name (exit $got)"
}

# ── repo fixtures ────────────────────────────────────────────────────────────
mk_full_repo() {  # a normal, non-shallow repo with a few commits + a merge
  mkdir -p repo; cd repo || exit 99; git init -q -b main
  git config user.email t@t; git config user.name t
  echo a>f; git add f; git commit -qm c1
  echo b>f; git add f; git commit -qm c2
  git checkout -q -b side; echo x>g; git add g; git commit -qm s1
  git checkout -q main; echo c>f; git add f; git commit -qm c3
  git merge -q --no-edit side
}
mk_nogit_tree() { mkdir -p repo; cd repo || exit 99; echo content > file.txt; }  # no `git init`
mk_shallow_repo() {
  # build an origin, then a depth=1 shallow clone of it (local, no network)
  mkdir -p origin; ( cd origin || exit 99; git init -q -b main; git config user.email t@t; git config user.name t
    for i in 1 2 3 4; do echo "l$i">f; git add f; git commit -qm "c$i"; done )
  git clone -q --depth=1 "file://$PWD/origin" repo
}
mk_range_repo() {  # main with 2 commits; HEAD on `pr`, 3 content commits ahead
  mkdir -p repo; cd repo || exit 99; git init -q -b main
  git config user.email t@t; git config user.name t
  echo a>f; git add f; git commit -qm c1
  echo b>f; git add f; git commit -qm c2
  git checkout -q -b pr
  for i in 1 2 3; do echo "p$i">"p$i"; git add "p$i"; git commit -qm "p$i"; done
}
mk_delete_only_repo() {  # HEAD on `pr`, whose one commit only deletes a file
  mkdir -p repo; cd repo || exit 99; git init -q -b main
  git config user.email t@t; git config user.name t
  echo a>f; echo g>g; git add f g; git commit -qm c1
  git checkout -q -b pr; git rm -q g; git commit -qm "delete g"
}

echo "gitleaks-history_test:"

# Exit-code contract: 0 clean, 1 gitleaks finding (coverage OK), 2 coverage fail.
# 1. Healthy full clone, clean history -> exit 0
run_case healthy-clean   clean   0 mk_full_repo
# 2. Healthy full clone, a real leak -> gitleaks FINDING exit 1 (coverage proven first)
run_case healthy-leak    leak    1 mk_full_repo
# 3. No .git at all (the shallow `code` clone bug) -> COVERAGE FAIL exit 2
run_case no-git-dir      nogit   2 mk_nogit_tree
# 4. Git-walk error (partial/promisor failure) despite exit-0 gitleaks -> COVERAGE FAIL exit 2
run_case walk-error      walkerr 2 mk_full_repo
# 5. gitleaks emits no 'commits scanned' line -> COVERAGE FAIL exit 2
run_case no-count-line   nocount 2 mk_full_repo    "the silent no-op the gate exists to catch"
# 5b. Killed mid-walk (spot reclaim / cancellation). Same exit 2 -- an
#     interrupted walk is not coverage -- but it must NOT be reported as a
#     gitleaks no-op, which sends the reader after the wrong bug.
run_case interrupted     interrupted 2 mk_full_repo "interrupted mid-walk"
# 6. Drastically short coverage (below the ratio floor) -> COVERAGE FAIL exit 2
run_case short-coverage  short   2 mk_full_repo
# 7. Shallow clone -> COVERAGE FAIL exit 2 (before gitleaks even runs)
run_case shallow-clone   clean   2 mk_shallow_repo

# Range mode (GITLEAKS_LOG_OPTS, set by checks.yml on a pull_request).
# 8. The PR's commits scanned in full -> exit 0, and it says it was a range.
CASE_LOG_OPTS=main..HEAD run_case range-clean        clean 0 mk_range_repo "RANGE main..HEAD has 3 commit(s)"
# 9. A finding in range is still exit 1.
CASE_LOG_OPTS=main..HEAD run_case range-leak         leak  1 mk_range_repo
# 10. A base the checkout never fetched -> COVERAGE FAIL, not "0 scanned, clean".
CASE_LOG_OPTS=origin/main..HEAD run_case range-unfetched-base clean 2 mk_range_repo "does not resolve to a commit"
# 11. Anything but one <base>..<head> -> COVERAGE FAIL.
CASE_LOG_OPTS="main..HEAD --all" run_case range-malformed clean 2 mk_range_repo "not a single <base>..<head> range"
# 12. gitleaks walked none of 3 content commits -> COVERAGE FAIL.
CASE_LOG_OPTS=main..HEAD run_case range-zero-scanned zero  2 mk_range_repo "below the floor"
# 13. A deletion-only PR: gitleaks says "0 commits scanned" and that is right.
CASE_LOG_OPTS=main..HEAD run_case range-delete-only  zero  0 mk_delete_only_repo
# 14. An empty range still needs gitleaks' count line.
CASE_LOG_OPTS=main..main run_case range-empty-nocount nocount 2 mk_range_repo "the silent no-op the gate exists to catch"

# ── Positive control: REAL gitleaks, range mode ──────────────────────────────
# The fake AWS key id is split across two shell words so this file never holds
# a contiguous token for gitleaks' aws rule to match: a PR that edits this file
# is scanned by the very job it tests. Not the poison fixture's key: gitleaks
# 8.30's aws rule drops that one on entropy, and this control must survive a
# bump of the 8.21.2 pin.
FAKE_AWS_KEY="AKIA""QYLPMN5HHHFPZAM2"
mk_real_repo() {  # planted key in main's history; HEAD on a clean `pr`
  mkdir -p repo; cd repo || exit 99; git init -q -b main
  git config user.email t@t; git config user.name t
  echo a>f; git add f; git commit -qm c1
  echo "aws_access_key_id = $FAKE_AWS_KEY" > base-key; git add base-key; git commit -qm "planted key in base"
  git checkout -q -b pr; echo p>p; git add p; git commit -qm p1
}
mk_real_repo_pr_key() {  # same, plus a planted key in the PR's own commits
  mk_real_repo
  echo "aws_access_key_id = $FAKE_AWS_KEY" > pr-key; git add pr-key; git commit -qm "planted key in pr"
}
# The repo's own config, so an allowlist entry that swallowed the planted key
# would red this control too.
REPO_GITLEAKS_CONFIG="$SCRIPT_DIR/../../.gitleaks.toml"
# run_real_case <name> <expected_rc> <repo-setup-fn> <log-opts>
run_real_case() {
  local name="$1" want="$2" setup="$3" range="$4"
  local d="$TMPROOT/real-$name"; mkdir -p "$d"
  ( cd "$d" && "$setup" )
  local got
  ( cd "$d/repo" || exit 99
    GITLEAKS_CONFIG="$REPO_GITLEAKS_CONFIG" GITLEAKS_LOG_OPTS="$range" bash "$WRAPPER" >/dev/null 2>&1 )
  got=$?
  if [ "$got" != "$want" ]; then bad "real-$name: want exit $want, got $got"; return; fi
  ok "real-$name (exit $got)"
}
if command -v gitleaks >/dev/null 2>&1; then
  # 15. The planted key in the PR's commits is caught.
  run_real_case pr-key-caught      1 mk_real_repo_pr_key main..HEAD
  # 16. The key that is only in the base is out of the PR's scope.
  run_real_case base-key-out-of-range 0 mk_real_repo     main..HEAD
  # 17. ...and the full-history scan still finds it.
  run_real_case full-history-finds-base-key 1 mk_real_repo ""
elif [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  bad "real gitleaks not on PATH under GitHub Actions -- the range-mode positive control did not run"
else
  echo "  SKIP real-gitleaks cases -- gitleaks not installed"
fi

echo "gitleaks-history_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
