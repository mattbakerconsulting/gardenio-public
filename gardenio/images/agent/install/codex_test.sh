#!/usr/bin/env bash
# codex_test.sh — hermetic tests for install/codex.sh.
#
# WHY THIS SCRIPT HAS A SELF-TEST AT ALL (the guard-test justification asked
# for by gd-kCs3VDmMWR). The `agent` image is release-only: images.yml drops it
# from the matrix on `pull_request` (owner ruling gd-9mtwbE3hoK). So codex.sh
# gets ZERO execution on a PR — a broken arch mapping, a mis-wired checksum
# variable or a wrong tar member name would first surface while cutting a
# release tag. These tests run the real script on both arches against a stubbed
# `curl` and a synthetic release tarball, so the parts a contributor actually
# edits are covered without network, without Docker and without a 100MB
# download.
#
# Not covered on purpose: that the pinned digests match what GitHub serves
# today. That needs the network and belongs to the release build.
#
# Usage:  bash gardenio/images/agent/install/codex_test.sh

# shellcheck source-path=SCRIPTDIR
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

PASS=0
FAIL=0
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# Read the pins the script under test will use. Named WANT_* so they do not
# shadow the names versions.env sets inside the subshell (SC2031).
# shellcheck source=./versions.env
WANT_CODEX_VERSION="$(. "${SCRIPT_DIR}/versions.env"; echo "$CODEX_VERSION")"
# shellcheck source=./versions.env
WANT_RG_VERSION="$(. "${SCRIPT_DIR}/versions.env"; echo "$RIPGREP_VERSION")"
[ -n "$WANT_CODEX_VERSION" ] || { echo "CODEX_VERSION missing from versions.env"; exit 1; }
[ -n "$WANT_RG_VERSION" ] || { echo "RIPGREP_VERSION missing from versions.env"; exit 1; }

# make_release <dir> <triple> <version>
#   Builds a stand-in release tarball with the same shape as the real asset: a
#   single bare executable member named `codex-<triple>` (NOT a directory, and
#   not `codex` — getting that wrong is the bug these tests exist to catch).
#   Prints the tarball's sha256.
make_release() {
  local dir="$1" triple="$2" version="$3"
  mkdir -p "$dir/stage"
  cat > "$dir/stage/codex-${triple}" <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && echo "codex-cli ${version}" && exit 0
exit 1
EOF
  chmod +x "$dir/stage/codex-${triple}"
  tar -czf "$dir/release.tar.gz" -C "$dir/stage" "codex-${triple}"
  sha256sum "$dir/release.tar.gz" | awk '{print $1}'
}

# make_rg_release <dir> <triple> <version>
#   Stand-in ripgrep asset. Unlike the Codex one this wraps the binary in a
#   VERSIONED DIRECTORY (ripgrep-<ver>-<triple>/rg) — the two asset shapes
#   differing is exactly the kind of thing that breaks silently.
make_rg_release() {
  local dir="$1" triple="$2" version="$3" reported="${4:-$3}"
  local inner="ripgrep-${version}-${triple}"
  mkdir -p "$dir/stage/$inner"
  cat > "$dir/stage/$inner/rg" <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && echo "ripgrep ${reported}" && exit 0
exit 1
EOF
  chmod +x "$dir/stage/$inner/rg"
  tar -czf "$dir/rg.tar.gz" -C "$dir/stage" "$inner"
  sha256sum "$dir/rg.tar.gz" | awk '{print $1}'
}

# make_host_release <dir> <triple>
#   Stand-in codex-code-mode-host asset: one bare executable member, no
#   --version support (the real one rejects it). Prints the tarball's sha256.
make_host_release() {
  local dir="$1" triple="$2"
  mkdir -p "$dir/stage"
  printf '#!/bin/sh\nexit 0\n' > "$dir/stage/codex-code-mode-host-${triple}"
  chmod +x "$dir/stage/codex-code-mode-host-${triple}"
  tar -czf "$dir/host.tar.gz" -C "$dir/stage" "codex-code-mode-host-${triple}"
  sha256sum "$dir/host.tar.gz" | awk '{print $1}'
}

# run_install <case> <targetarch> <tarball> <x86 sha> <arm sha>
#   Stages the REAL codex.sh next to the real versions.env/lib.sh and a
#   synthetic checksums.env carrying the digests given, then runs it with a
#   `curl` stub that serves the codex fixture for a codex URL and the ripgrep
#   fixture for a ripgrep URL, logging every URL requested. Echoes the exit
#   status; artifacts land in $TMPROOT/<case>.
#
#   RG_SERVED / RG_SHA_X86 / RG_SHA_ARM are read from the environment so the
#   ripgrep half can be varied independently without a six-positional-arg call.
run_install() {
  local case_name="$1" targetarch="$2" served="$3" sha_x86="$4" sha_arm="$5"
  local d="$TMPROOT/$case_name"
  mkdir -p "$d/bin" "$d/prefix" "$d/install"

  cp "${SCRIPT_DIR}/codex.sh" "${SCRIPT_DIR}/lib.sh" "${SCRIPT_DIR}/versions.env" "$d/install/"
  cat > "$d/install/checksums.env" <<EOF
export CODEX_SHA256_X86_64=${sha_x86}
export CODEX_SHA256_AARCH64=${sha_arm}
export CODEX_HOST_SHA256_X86_64=${HOST_SHA_X86:-unset}
export CODEX_HOST_SHA256_AARCH64=${HOST_SHA_ARM:-unset}
export RIPGREP_SHA256_X86_64=${RG_SHA_X86:-unset}
export RIPGREP_SHA256_AARCH64=${RG_SHA_ARM:-unset}
EOF

  cat > "$d/bin/curl" <<EOF
#!/bin/sh
# stub: log any https URL, then copy the matching fixture to whatever -o names.
out=""; prev=""; url=""
for a in "\$@"; do
  case "\$a" in https://*) url="\$a"; echo "\$a" >> "$d/urls" ;; esac
  [ "\$prev" = "-o" ] && out="\$a"
  prev="\$a"
done
[ -n "\$out" ] || exit 1
case "\$url" in
  *ripgrep*) cp "${RG_SERVED:-/nonexistent}" "\$out" ;;
  *code-mode-host*) cp "${HOST_SERVED:-/nonexistent}" "\$out" ;;
  *)         cp "$served" "\$out" ;;
esac
EOF
  chmod +x "$d/bin/curl"

  local rc=0
  env PATH="$d/bin:$PATH" TARGETARCH="$targetarch" INSTALL_PREFIX="$d/prefix" \
    bash "$d/install/codex.sh" > "$d/out" 2>&1 || rc=$?
  echo "$rc"
}

# ── Tests 1-2: a good install on each arch ───────────────────────────
for spec in "amd64 x86_64" "arm64 aarch64"; do
  # shellcheck disable=SC2086  # deliberate word split of the "arch triple" pair
  set -- $spec
  arch="$1"; triple_arch="$2"
  triple="${triple_arch}-unknown-linux-musl"
  fx="$TMPROOT/fx-$arch"
  sha="$(make_release "$fx" "$triple" "$WANT_CODEX_VERSION")"
  rgfx="$TMPROOT/rgfx-$arch"
  rgsha="$(make_rg_release "$rgfx" "$triple" "$WANT_RG_VERSION")"
  export RG_SERVED="$rgfx/rg.tar.gz"
  hostfx="$TMPROOT/hostfx-$arch"
  hostsha="$(make_host_release "$hostfx" "$triple")"
  export HOST_SERVED="$hostfx/host.tar.gz"
  if [ "$triple_arch" = "x86_64" ]; then
    export HOST_SHA_X86="$hostsha" HOST_SHA_ARM=wrong-arch-digest
    export RG_SHA_X86="$rgsha" RG_SHA_ARM=wrong-arch-digest
    rc="$(run_install "ok-$arch" "$arch" "$fx/release.tar.gz" "$sha" "wrong-arch-digest")"
  else
    export HOST_SHA_X86=wrong-arch-digest HOST_SHA_ARM="$hostsha"
    export RG_SHA_X86=wrong-arch-digest RG_SHA_ARM="$rgsha"
    rc="$(run_install "ok-$arch" "$arch" "$fx/release.tar.gz" "wrong-arch-digest" "$sha")"
  fi
  d="$TMPROOT/ok-$arch"

  if [ "$rc" = "0" ]; then pass "$arch: install exits 0"
  else fail "$arch: install exits 0" "rc=$rc: $(cat "$d/out")"; fi

  if [ -x "$d/prefix/bin/codex" ]; then pass "$arch: installs an executable at bin/codex"
  else fail "$arch: installs an executable at bin/codex" "missing or not +x"; fi

  # The member name is the thing most likely to be wrong, and extracting the
  # wrong one yields an empty file rather than a loud failure.
  if [ -s "$d/prefix/bin/codex" ] && "$d/prefix/bin/codex" --version 2>/dev/null | grep -q "$WANT_CODEX_VERSION"; then
    pass "$arch: extracted the codex-<triple> member, not a directory"
  else
    fail "$arch: extracted the codex-<triple> member, not a directory" "$(cat "$d/out")"
  fi

  # Asserts BOTH halves of the arch mapping: this arch's URL, and that the
  # digest it verified against was this arch's (the other was a bad value, so a
  # swapped pick_sha_alt mapping could not have got here).
  if grep -qF "/releases/download/rust-v${WANT_CODEX_VERSION}/codex-${triple}.tar.gz" "$d/urls"; then
    pass "$arch: requests the rust-v<ver> ${triple_arch} musl asset"
  else
    fail "$arch: requests the rust-v<ver> ${triple_arch} musl asset" "urls=$(cat "$d/urls" 2>/dev/null)"
  fi

  # codex-code-mode-host: without it every Codex tool call fails
  # (gd-QpUS8SPrhc), so a missing/empty/non-+x file here is the regression.
  if [ -s "$d/prefix/bin/codex-code-mode-host" ] && [ -x "$d/prefix/bin/codex-code-mode-host" ]; then
    pass "$arch: installs codex-code-mode-host as a non-empty executable"
  else
    fail "$arch: installs codex-code-mode-host as a non-empty executable" "$(cat "$d/out")"
  fi
  if grep -qF "/releases/download/rust-v${WANT_CODEX_VERSION}/codex-code-mode-host-${triple}.tar.gz" "$d/urls"; then
    pass "$arch: requests the code-mode-host ${triple_arch} musl asset"
  else
    fail "$arch: requests the code-mode-host ${triple_arch} musl asset" "urls=$(cat "$d/urls" 2>/dev/null)"
  fi

  # ripgrep: a REAL executable is the whole point — the image already had an
  # `rg` bash function that Codex cannot see, so "rg resolves in bash" is not
  # the property under test. Being a file at bin/rg is.
  if [ -x "$d/prefix/bin/rg" ] && "$d/prefix/bin/rg" --version 2>/dev/null | grep -q "$WANT_RG_VERSION"; then
    pass "$arch: installs ripgrep as a real executable at bin/rg"
  else
    fail "$arch: installs ripgrep as a real executable at bin/rg" "$(cat "$d/out")"
  fi

  # The ripgrep asset nests its binary in a versioned directory while the Codex
  # one does not; this asserts the strip is right for THIS arch's URL.
  if grep -qF "/ripgrep/releases/download/${WANT_RG_VERSION}/ripgrep-${WANT_RG_VERSION}-${triple}.tar.gz" "$d/urls"; then
    pass "$arch: requests the ripgrep ${triple_arch} musl asset"
  else
    fail "$arch: requests the ripgrep ${triple_arch} musl asset" "urls=$(cat "$d/urls" 2>/dev/null)"
  fi
done

# Later cases exercise the CODEX half, so give them a ripgrep fixture that
# always verifies — otherwise they could pass on a ripgrep failure instead.
rgok="$TMPROOT/rgfx-ok"
rgoksha="$(make_rg_release "$rgok" "x86_64-unknown-linux-musl" "$WANT_RG_VERSION")"
export RG_SERVED="$rgok/rg.tar.gz" RG_SHA_X86="$rgoksha" RG_SHA_ARM=unused
hostok="$TMPROOT/hostfx-ok"
hostoksha="$(make_host_release "$hostok" "x86_64-unknown-linux-musl")"
export HOST_SERVED="$hostok/host.tar.gz" HOST_SHA_X86="$hostoksha" HOST_SHA_ARM=unused

# ── Test 3: a bad checksum must fail closed ──────────────────────────
fx="$TMPROOT/fx-bad"
make_release "$fx" "x86_64-unknown-linux-musl" "$WANT_CODEX_VERSION" > /dev/null
rc="$(run_install badsha amd64 "$fx/release.tar.gz" \
      "0000000000000000000000000000000000000000000000000000000000000000" unused)"
if [ "$rc" != "0" ]; then pass "checksum mismatch fails the install"
else fail "checksum mismatch fails the install" "rc=0"; fi
if [ ! -s "$TMPROOT/badsha/prefix/bin/codex" ]; then
  pass "checksum mismatch leaves no usable binary behind"
else
  fail "checksum mismatch leaves no usable binary behind" "a non-empty codex was written"
fi

# ── Test 4: a version/digest mismatch must fail closed ───────────────
# The real failure this models: CODEX_VERSION bumped and CODEX_SHA256_* not (or
# the reverse), so the bytes that verify are the OLD release. The smoke test is
# the only thing between that and a mislabelled release image.
fx="$TMPROOT/fx-stale"
sha="$(make_release "$fx" "x86_64-unknown-linux-musl" "0.0.1-stale")"
rc="$(run_install stale amd64 "$fx/release.tar.gz" "$sha" unused)"
if [ "$rc" != "0" ]; then pass "version mismatch after install fails the build"
else fail "version mismatch after install fails the build" "rc=0: $(cat "$TMPROOT/stale/out")"; fi
if grep -q "version mismatch" "$TMPROOT/stale/out"; then
  pass "version mismatch prints both expected and actual"
else
  fail "version mismatch prints both expected and actual" "$(cat "$TMPROOT/stale/out")"
fi

# ── Test 5: a bad ripgrep digest must fail the install too ──────────
# ripgrep is a dependency, not a nice-to-have: a Codex image without a real rg
# is one where search silently degrades, so this must not install "most of it".
fx="$TMPROOT/fx-rgbad"
sha="$(make_release "$fx" "x86_64-unknown-linux-musl" "$WANT_CODEX_VERSION")"
rc="$(RG_SHA_X86=0000000000000000000000000000000000000000000000000000000000000000 \
      run_install rgbadsha amd64 "$fx/release.tar.gz" "$sha" unused)"
if [ "$rc" != "0" ]; then pass "a bad ripgrep digest fails the install"
else fail "a bad ripgrep digest fails the install" "rc=0: $(cat "$TMPROOT/rgbadsha/out")"; fi

# ── Test 6: a stale ripgrep BINARY must fail closed ─────────────────
# Note the asymmetry with Test 4: ripgrep's asset nests its binary in a
# version-NAMED directory, so a bumped RIPGREP_VERSION with a stale digest is
# already caught by tar ("not found in archive"). What tar cannot catch is an
# archive with the right directory name holding a binary that reports
# something else — a re-cut upstream release. That is what the `rg --version`
# assertion is for, so that is what this fixture models: correct dir name,
# stale binary inside.
fx="$TMPROOT/fx-rgstale"
sha="$(make_release "$fx" "x86_64-unknown-linux-musl" "$WANT_CODEX_VERSION")"
rgstale="$TMPROOT/rgfx-stale"
rgstalesha="$(make_rg_release "$rgstale" "x86_64-unknown-linux-musl" "$WANT_RG_VERSION" "0.0.1-stale")"
rc="$(RG_SERVED="$rgstale/rg.tar.gz" RG_SHA_X86="$rgstalesha" \
      run_install rgstale amd64 "$fx/release.tar.gz" "$sha" unused)"
if [ "$rc" != "0" ]; then pass "a stale ripgrep version fails the install"
else fail "a stale ripgrep version fails the install" "rc=0: $(cat "$TMPROOT/rgstale/out")"; fi
if grep -q "ripgrep version mismatch" "$TMPROOT/rgstale/out"; then
  pass "stale ripgrep prints both expected and actual"
else
  fail "stale ripgrep prints both expected and actual" "$(cat "$TMPROOT/rgstale/out")"
fi

# ── Test 7: a bad code-mode-host digest must fail the install ───────
fx="$TMPROOT/fx-hostbad"
sha="$(make_release "$fx" "x86_64-unknown-linux-musl" "$WANT_CODEX_VERSION")"
rc="$(HOST_SHA_X86=0000000000000000000000000000000000000000000000000000000000000000 \
      run_install hostbadsha amd64 "$fx/release.tar.gz" "$sha" unused)"
if [ "$rc" != "0" ]; then pass "a bad code-mode-host digest fails the install"
else fail "a bad code-mode-host digest fails the install" "rc=0: $(cat "$TMPROOT/hostbadsha/out")"; fi

echo
echo "codex_test.sh: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
