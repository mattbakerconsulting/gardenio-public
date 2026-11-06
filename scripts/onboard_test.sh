#!/usr/bin/env bash
# onboard_test.sh — hermetic tests for onboard.sh
#
# kd, gb, claude, coop, go and uname are stubs on a private PATH, HOME is a temp
# dir, and release assets come from a file:// dir, so no daemon, network, token
# or Go toolchain is needed. What is tested is the part a human cannot easily
# check by eye: the doctor changes nothing, a second run changes nothing, the rc
# block is replaced in place (old onboard-mac.sh block included), a token never
# reaches the output, and both install paths stamp a calver.
#
# Usage:  bash scripts/onboard_test.sh

# pass/fail chains and literal stub bodies are deliberate.
# shellcheck disable=SC2015,SC2016
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REAL_REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
SUT="$SCRIPT_DIR/onboard.sh"

PASS=0; FAIL=0
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }
SECRET="tok-$$-never-print-me"
# The private PATH is /usr/bin:/bin plus stubs; link in the few real tools a
# runner may keep elsewhere, without dragging in a real kd or gb beside them.
TOOLS="$TMP/tools"; mkdir -p "$TOOLS"
for t in python3 curl git; do p="$(command -v "$t")" && ln -s "$p" "$TOOLS/$t"; done

# stub <name> <body>: a /bin/sh script on the stub PATH.
stub() { printf '#!/bin/sh\n%s\n' "$2" >"$STUBS/$1"; chmod +x "$STUBS/$1"; }

# fresh: new HOME + stubs. kd/gb are stamped 2026.300 unless NO_CLI=1.
fresh() {
  H="$TMP/home.$1"; STUBS="$TMP/stubs.$1"; rm -rf "$H" "$STUBS"; mkdir -p "$H" "$STUBS"
  printf 'alias ll="ls -l"\n' >"$H/.bashrc"
  if [ -z "${NO_CLI:-}" ]; then
    stub kd 'echo 2026.300.1'
    stub gb 'case "$1" in
  --version) echo 2026.300.1 ;;
  ping) exit 0 ;;
  prime) echo "- **Agent**: test-person" ;;
  login) if [ "$2" = --status ]; then echo "Not logged in. Run gb login to authenticate."; else echo login-ran >>"$HOME/login.log"; fi ;;
esac'
  fi
  stub claude 'echo "9.9.9 (Claude Code)"'
  stub coop 'echo "coop 0.0.1"'
}

# run <args...>: onboard.sh in the sandbox; output (colour stripped) in $OUT.
run() {
  OUT="$(env -i HOME="$H" PATH="$STUBS:$TOOLS:/usr/bin:/bin" SHELL=/bin/bash TERM=dumb \
    GARDENIO_REPO="${REPO:-$REAL_REPO}" GARDENIO_RELEASE_BASE="${RELEASE:-none}" \
    GARDENIO_PROC_VERSION="${PROCV:-/dev/null}" ${TOKEN:+GARDENIO_TOKEN=$TOKEN} \
    bash "$SUT" "$@" </dev/null 2>&1 | sed 's/\x1b\[[0-9;]*m//g')"
  RC_STATUS=${PIPESTATUS[0]}
}

# snap: a checksum of everything under HOME, to prove a run changed nothing.
snap() { (cd "$H" && find . -type f -print0 | sort -z | xargs -0 cat | cksum); }
blocks() { grep -c '^# >>> gardenio' "$H/.bashrc"; }

# ── 1. doctor on a fresh machine: reports, writes nothing ────────────────────
fresh doctor
before="$(snap)"; run --doctor
[ "$RC_STATUS" -eq 1 ] && pass "doctor exits 1 when env is unset" || fail "doctor exit" "got $RC_STATUS"
grep -q "BEADS_HTTP_URL unset" <<<"$OUT" && pass "doctor flags BEADS_HTTP_URL" || fail "doctor url" "$OUT"
[ "$(snap)" = "$before" ] && pass "doctor wrote nothing" || fail "doctor wrote" "HOME changed"

# ── 2. no terminal and no --yes: plan only, nothing changed ──────────────────
run --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
grep -q "nothing was changed" <<<"$OUT" && [ "$(snap)" = "$before" ] \
  && pass "no tty without --yes changes nothing" || fail "no-tty" "$OUT"

# ── 3. apply, then a second run changes nothing ──────────────────────────────
TOKEN="$SECRET" run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
[ "$RC_STATUS" -eq 0 ] && pass "apply exits 0" || fail "apply exit" "$RC_STATUS: $OUT"
[ "$(blocks)" = 1 ] && pass "one gardenio block written" || fail "block count" "$(blocks)"
grep -qxF "export BEADS_HTTP_URL='https://gardenio.example.com/kd'" "$H/.bashrc" \
  && pass "block exports the url" || fail "block url" "$(cat "$H/.bashrc")"
head -1 "$H/.bashrc" | grep -qF 'alias ll=' && pass "existing rc lines kept" || fail "rc lines" "$(head -1 "$H/.bashrc")"
grep -q '"gb hook prime' "$H/.claude/settings.local.json" && pass "hooks merged" || fail "hooks" "missing"
[ -x "$H/.local/bin/gb-claude" ] && pass "gb-claude installed" || fail "gb-claude" "missing"
grep -qF "$SECRET" <<<"$OUT" && fail "token printed" "output contains the token" || pass "token never printed"
grep -qF "$SECRET" "$H/.bashrc" && fail "token in rc" "rc contains the token" || pass "token never written"
after1="$(snap)"; TOKEN="$SECRET" run
grep -q "nothing to change" <<<"$OUT" && pass "second run has nothing to change" || fail "second run" "$OUT"
[ "$(snap)" = "$after1" ] && pass "second run wrote nothing" || fail "second run wrote" "HOME changed"

# ── 4. a new value replaces the block in place ───────────────────────────────
printf 'export AFTER=1\n' >>"$H/.bashrc"
run --yes --url https://other.example.com/kd
[ "$(blocks)" = 1 ] && grep -qF "other.example.com" "$H/.bashrc" && tail -1 "$H/.bashrc" | grep -qxF 'export AFTER=1' \
  && pass "rerun replaces the block in place" || fail "replace" "$(cat "$H/.bashrc")"

# ── 5. the old onboard-mac.sh block is replaced, PAT and all ─────────────────
fresh old
cat >>"$H/.bashrc" <<'EOF'
# >>> gardenio operator env >>>
export GITLAB_TOKEN='glpat-oldoldold'
export BEADS_HTTP_URL="https://retired.example/kd"
# <<< gardenio operator env <<<
export AFTER=1
EOF
run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
! grep -q 'glpat-oldoldold\|operator env' "$H/.bashrc" && [ "$(blocks)" = 1 ] && grep -qxF 'export AFTER=1' "$H/.bashrc" \
  && pass "old onboard-mac.sh block replaced" || fail "old block" "$(cat "$H/.bashrc")"
grep -q "revoke that PAT" <<<"$OUT" && pass "plan warns about the old PAT" || fail "PAT warning" "$OUT"

# ── 6. an unmatched marker is refused, rc untouched ──────────────────────────
fresh broken
printf '# >>> gardenio env (scripts/onboard.sh) >>>\nexport KEEP=1\n' >>"$H/.bashrc"
before="$(cksum <"$H/.bashrc")"
run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
[ "$(cksum <"$H/.bashrc")" = "$before" ] && grep -q "unmatched gardenio marker" <<<"$OUT" \
  && pass "unmatched marker refused" || fail "unmatched" "$OUT"

# ── 7. not signed in, no token: offers gb login ──────────────────────────────
fresh login
run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
grep -q "run gb login" <<<"$OUT" && [ -f "$H/login.log" ] && pass "gb login started" || fail "login" "$OUT"

# ── 8. platform: macOS bash writes ~/.bash_profile; WSL is named ─────────────
fresh mac; stub uname 'case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; *) echo 24.0 ;; esac'; stub sw_vers 'echo 15.0'
run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
grep -q "macOS 15.0 arm64" <<<"$OUT" && grep -qF "BEADS_HTTP_URL" "$H/.bash_profile" 2>/dev/null \
  && pass "macOS bash uses ~/.bash_profile" || fail "mac rc" "$OUT"
fresh wsl; printf 'Linux version 5.15.0-microsoft-standard-WSL2\n' >"$TMP/procv"
PROCV="$TMP/procv" run --doctor
grep -q "✅ WSL" <<<"$OUT" && pass "WSL detected" || fail "wsl" "$OUT"

# ── 9. install from release assets, checksum verified ────────────────────────
NO_CLI=1 fresh release
REL="$TMP/rel"; mkdir -p "$REL/pkg"
for b in kd gb; do
  printf '#!/bin/sh\n[ "$1" = --version ] && echo 2026.301.0\nexit 0\n' >"$REL/pkg/$b"; chmod +x "$REL/pkg/$b"
  tar -czf "$REL/$b-linux-amd64.tar.gz" -C "$REL/pkg" "$b"
  (cd "$REL" && sha256sum "$b-linux-amd64.tar.gz" >"$b-linux-amd64.tar.gz.sha256")
done
RELEASE="file://$REL" run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
[ "$("$H/.local/bin/kd" --version)" = 2026.301.0 ] && grep -q "installed release kd + gb" <<<"$OUT" \
  && pass "release kd + gb installed" || fail "release" "$OUT"
NO_CLI=1 fresh badsum
echo "0000  kd-linux-amd64.tar.gz" >"$REL/kd-linux-amd64.tar.gz.sha256"
RELEASE="file://$REL" run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
[ ! -e "$H/.local/bin/kd" ] && grep -q "checksum mismatch" <<<"$OUT" && pass "bad checksum refused" || fail "badsum" "$OUT"

# ── 10. no release: go build with the calver ldflags ─────────────────────────
NO_CLI=1 fresh build
REPO="$TMP/repo"; mkdir -p "$REPO/kbeads" "$REPO/gardenio/controller" "$REPO/scripts"
cp "$SCRIPT_DIR/gardenio-version.sh" "$SCRIPT_DIR/gb-claude" "$SCRIPT_DIR/claude-hooks.json" "$REPO/scripts/"
git -C "$REPO" init -q && git -C "$REPO" -c user.email=t@e -c user.name=t commit -q --allow-empty -m x
# The stub go writes a binary that reports the -X ...ersion= value it was given.
stub go 'out=""; v=""; while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; -ldflags) v="$(printf "%s" "$2" | sed -n "s/.*-X main\.[Vv]ersion=\([^ ]*\).*/\1/p")" ;; esac; shift; done
printf "#!/bin/sh\necho %s\n" "$v" >"$out"'
want="$(git -C "$REPO" log -1 --format=%cd --date=format:%Y.%j)"
REPO="$REPO" run --yes --url https://gardenio.example.com/kd --operated-by oidc:t@example.com
[ "$("$H/.local/bin/kd" --version)" = "$want" ] && [ "$("$H/.local/bin/gb" --version)" = "$want" ] \
  && pass "go build stamps the commit calver" || fail "build" "$OUT"
REPO=""

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" -eq 0 ]
