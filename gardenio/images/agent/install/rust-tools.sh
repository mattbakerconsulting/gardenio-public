#!/bin/bash
# rust-tools.sh — install Rust toolchain, rust-analyzer, and quench.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

PREFIX=${INSTALL_PREFIX:-/usr/local}
export RUSTUP_HOME="$PREFIX/rustup"
export CARGO_HOME="$PREFIX/cargo"

# Rustup installer — download the archived, per-release rustup-init binary
# (immutable per (version, target)) rather than the rolling sh.rustup.rs
# wrapper, whose sha drifts on every upstream push (ka-0000004123).
# rustup-init is GPG-verified internally against static.rust-lang.org
# manifests.
ARCH=${TARGETARCH:-amd64}
case "$ARCH" in
  arm64) RUSTUP_TARGET=aarch64-unknown-linux-gnu ;;
  *)     RUSTUP_TARGET=x86_64-unknown-linux-gnu ;;
esac
fetch_verify \
  "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/${RUSTUP_TARGET}/rustup-init" \
  "$(pick_sha_alt RUSTUP_INIT_SHA256)" \
  /tmp/rustup-init
chmod +x /tmp/rustup-init
# The default toolchain is the one coop pins (coop/rust-toolchain.toml, copied
# beside this script by the Dockerfile), with the components it names: a pod
# then builds coop without rustup first downloading a toolchain (gd-Kec4Xy0UtT).
TOOLCHAIN_TOML="$SCRIPT_DIR/rust-toolchain.toml"
RUST_CHANNEL=$(sed -n 's/^channel *= *"\([^"]*\)".*/\1/p' "$TOOLCHAIN_TOML")
RUST_COMPONENTS=$(sed -n 's/^components *= *\[\(.*\)\].*/\1/p' "$TOOLCHAIN_TOML" | tr -d '" ' | tr ',' ' ')
[ -n "$RUST_CHANNEL" ] || { echo "rust-tools.sh: no channel in $TOOLCHAIN_TOML" >&2; exit 1; }
COMPONENT_ARGS=(--component rust-analyzer)
for c in $RUST_COMPONENTS; do COMPONENT_ARGS+=(--component "$c"); done
/tmp/rustup-init -y --default-toolchain "$RUST_CHANNEL" --profile default "${COMPONENT_ARGS[@]}"
rm -f /tmp/rustup-init

# quench (code quality checker) — prebuilt release tarball, sha-verified.
# Only amd64/x86_64 ships as a prebuilt; arm64 falls back to a source build
# against the pinned git ref, which is verified via cargo's own integrity.
ARCH=${TARGETARCH:-amd64}
case "$ARCH" in arm64) Q_ARCH=aarch64 ;; *) Q_ARCH=x86_64 ;; esac
if [ "$Q_ARCH" = "x86_64" ]; then
  fetch_verify_stdout \
    "https://github.com/alfredjeanlab/quench/releases/download/v${QUENCH_VERSION}/quench-linux-${Q_ARCH}.tar.gz" \
    "${QUENCH_SHA256_X86_64}" \
    | tar -xz -C "$PREFIX/bin" quench
  echo "quench: installed from prebuilt binary v${QUENCH_VERSION}"
else
  echo "quench: no prebuilt for ${Q_ARCH}, building from source at v${QUENCH_VERSION}"
  "$CARGO_HOME/bin/cargo" install quench \
    --git https://github.com/alfredjeanlab/quench \
    --tag "v${QUENCH_VERSION}"
  mv "$CARGO_HOME/bin/quench" "$PREFIX/bin/quench"
fi

# The agent (uid 1000) must be able to write cargo's registry, git checkouts
# and its lock files: without these dirs cargo fails at `mkdir registry/cache`
# under the root-owned cargo home (gd-Kec4Xy0UtT).
mkdir -p "$CARGO_HOME/registry" "$CARGO_HOME/git"
chmod -R a+rw "$RUSTUP_HOME" "$CARGO_HOME"

echo "Rust + tools installed."
