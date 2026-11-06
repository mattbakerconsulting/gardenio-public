#!/usr/bin/env bash
# check-no-curl-pipe-shell_test.sh — Unit tests for check-no-curl-pipe-shell.sh
#
# Each test writes a synthetic file to a temp dir and asserts the guard's exit
# status when pointed at it (with an explicit-file argument, so the test never
# depends on the real git index). No external services. Runs in the main shell
# so the pass/fail counters are reliable.
#
# The final test invokes the guard against the REAL tracked tree, which is the
# only part that exercises the allowlist and its stale-entry pass. A GREEN
# SELF-TEST IS NOT COVERAGE (see gd-odH13TT1hu for the class of guards in this
# repo that are self-tested and wired to nothing) -- this guard is additionally
# wired into checks.yml's `script-selftests` job as its own named step.
#
# The round-trip that actually justifies the guard is not reproducible here
# because it needs history: run it against 99ae7e34 and it reports all three
# `curl … | sh` rustup installs (ci.yml:669, e2e.yml:159, checks.yml:689) that
# da6889f7 and 2a7c7e98 removed. It would have caught them before they shipped.
#
# Usage:  bash scripts/check-no-curl-pipe-shell_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/check-no-curl-pipe-shell.sh"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file..>
assert_exit() {
  local expected="$1" name="$2"; shift 2
  local out rc
  out="$(bash "$SCRIPT_UNDER_TEST" "$@" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# write <name> <<'EOF' … EOF   -> echoes the path
write() { local p="${TMPDIR_T}/$1"; mkdir -p "$(dirname "$p")"; cat > "$p"; echo "$p"; }

# ---------------------------------------------------------------- must FLAG

f="$(write plain.sh <<'EOF'
#!/bin/sh
curl -sSf https://example.invalid/install.sh | sh -s -- -y
EOF
)"
assert_exit 1 "plain curl | sh" "$f"

f="$(write bash.sh <<'EOF'
curl -fsSL https://example.invalid/setup | bash
EOF
)"
assert_exit 1 "curl | bash" "$f"

f="$(write sudo.sh <<'EOF'
curl -fsSL https://example.invalid/setup | sudo bash
EOF
)"
assert_exit 1 "curl | sudo bash" "$f"

f="$(write wget.sh <<'EOF'
wget -qO- https://example.invalid/install.sh | sh
EOF
)"
assert_exit 1 "wget | sh" "$f"

# Environment assignments between the pipe and the shell. THIS IS THE FORM THE
# GUARD ORIGINALLY MISSED: it read clean over all three get.k3s.io installs in
# gardenio/terraform/modules/k3s/templates/node.sh.tftpl, because the first
# pattern required `sh` immediately after the pipe. It is also the form the
# k3s and rustup docs print, so it is the one most likely to be copied in.
f="$(write envprefix.sh <<'EOF'
curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="v1.31.2+k3s1" K3S_TOKEN="$TOKEN" sh -s - agent
EOF
)"
assert_exit 1 "curl | ENV=val sh (assignments between the pipe and the shell)" "$f"

f="$(write envprefix-bash.sh <<'EOF'
curl -fsSL https://example.invalid/setup | DEBIAN_FRONTEND=noninteractive sudo bash
EOF
)"
assert_exit 1 "curl | ENV=val ... bash" "$f"

# The evasion that a line-oriented grep misses. It is not hypothetical: the
# repo's own workflows wrap long curl invocations this way.
f="$(write continuation.sh <<'EOF'
curl -sSfL --retry 5 \
     --max-time 180 https://example.invalid/install.sh \
  | sh -s -- -y
EOF
)"
assert_exit 1 "pipe split across a backslash continuation" "$f"

f="$(write in-workflow.yml <<'EOF'
jobs:
  build:
    steps:
      - run: |
          curl -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain none
EOF
)"
assert_exit 1 "inside a workflow run block" "$f"

# ---------------------------------------------------------------- must PASS

# Comments describing the banned pattern are how this repo documents the ban --
# clis.sh:8, go-tools.sh:28 and the three fixed workflows all contain the
# literal string. Flagging them would make the guard unlandable.
f="$(write comment.sh <<'EOF'
# DO NOT collapse this back to `curl -sSf https://example.invalid/i.sh | sh`.
#   curl … /install.sh | sh -s -- -b $(go env GOPATH)/bin v2.11.2
EOF
)"
assert_exit 0 "comment mentioning the pattern" "$f"

f="$(write correct.sh <<'EOF'
init="$(mktemp)"
curl -sSfL --retry 5 --retry-delay 2 --retry-connrefused \
     --max-time 180 -o "$init" https://example.invalid/install.sh
[ -s "$init" ] || { echo "::error::installer downloaded empty"; exit 1; }
sh "$init" -y
rm -f "$init"
EOF
)"
assert_exit 0 "the correct download-then-run form" "$f"

# Deliberately out of scope -- tar fails loudly on a truncated or empty
# archive, so it is not the silent-success case this guard exists for. If that
# decision is ever revisited it needs its own allowlist.
f="$(write tar.sh <<'EOF'
curl -sSL https://example.invalid/shellcheck.tar.xz | tar -xJ
EOF
)"
assert_exit 0 "curl | tar is out of scope" "$f"

# `sh` must be the command, not a prefix of one.
f="$(write shasum.sh <<'EOF'
curl -fsSL https://example.invalid/pkg.tgz | sha512sum
EOF
)"
assert_exit 0 "curl | sha512sum is not curl | sh" "$f"

# The three cases below pin the env-assignment widening. Skipping over `sudo`,
# its flags and a run of `VAR=value` must not turn into "skip over anything
# until something ending in sh" -- the token that lands on the shell still has
# to BE the shell. Without these, a later loosening of that pattern would flag
# every pipeline whose last argument happens to end in `.sh`, the guard would
# red on innocent lines, and the usual response to a noisy guard is to delete
# it.
f="$(write envprefix-shasum.sh <<'EOF'
curl -fsSL https://example.invalid/pkg.tgz | LC_ALL=C sha512sum
EOF
)"
assert_exit 0 "env prefix does not make sha512sum a shell" "$f"

f="$(write tee-dot-sh.sh <<'EOF'
curl -fsSL https://example.invalid/install | tee -a /var/log/provision.sh
EOF
)"
assert_exit 0 "a path ending in .sh is not a shell invocation" "$f"

f="$(write sudo-not-shell.sh <<'EOF'
curl -fsSL https://example.invalid/pkg.deb | sudo -E tee /tmp/pkg.deb
EOF
)"
assert_exit 0 "sudo with flags into a non-shell command" "$f"

f="$(write notdownload.sh <<'EOF'
echo "$payload" | sh
EOF
)"
assert_exit 0 "a non-download pipeline into sh" "$f"

# ------------------------------------------------- the real tree (allowlist)

# Exercises the allowlist AND the stale-entry pass, neither of which the
# synthetic cases above can reach. If this reds, either a new violation landed
# or an allowlisted one was fixed without deleting its line.
cd "$REPO_ROOT"
assert_exit 0 "real tracked tree is clean (allowlist current)"

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "$FAIL" -eq 0 ]
