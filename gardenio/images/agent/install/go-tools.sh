#!/bin/bash
# go-tools.sh — install Go toolchain, gopls, golangci-lint, Task CLI, gofumpt
# and staticcheck.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

ARCH=${TARGETARCH:-amd64}
PREFIX=${INSTALL_PREFIX:-/usr/local}

# Go — pinned to GO_FULL_VERSION and sha-verified against go.dev/dl.
GO_TGZ=/tmp/go.tar.gz
fetch_verify \
  "https://go.dev/dl/go${GO_FULL_VERSION}.linux-${ARCH}.tar.gz" \
  "$(pick_sha GO_SHA256)" \
  "$GO_TGZ"
tar -C "$PREFIX" -xzf "$GO_TGZ"
rm -f "$GO_TGZ"

GOBIN="$PREFIX/go/bin"

# gopls — fetched via `go install` (Go's module proxy verifies via sumdb).
GOROOT="$PREFIX/go" GOPATH=/tmp/gopath "$GOBIN/go" install golang.org/x/tools/gopls@latest 2>/dev/null || true
cp /tmp/gopath/bin/gopls "$GOBIN/gopls" 2>/dev/null || true

# golangci-lint — direct release tarball (replaces the `curl … install.sh | sh`
# pipe, which would execute arbitrary code on a compromised raw.githubusercontent).
GOLANGCI_DIR="golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${ARCH}"
fetch_verify_stdout \
  "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/${GOLANGCI_DIR}.tar.gz" \
  "$(pick_sha GOLANGCI_LINT_SHA256)" \
  | tar -xz --strip-components=1 -C "$PREFIX/bin" "${GOLANGCI_DIR}/golangci-lint"
chmod +x "$PREFIX/bin/golangci-lint"

# Task CLI — fetched via `go install` (sumdb-verified).
GOROOT="$PREFIX/go" GOPATH=/tmp/gopath "$GOBIN/go" install github.com/go-task/task/v3/cmd/task@latest
mv /tmp/gopath/bin/task "$PREFIX/bin/task"

# gofumpt + staticcheck — `go install` (sumdb-verified) at the pinned versions,
# built by this Go so they understand its language version. GOTOOLCHAIN=local
# makes a pin that needs a newer Go fail here rather than download one.
GOROOT="$PREFIX/go" GOPATH=/tmp/gopath GOTOOLCHAIN=local CGO_ENABLED=0 \
  "$GOBIN/go" install -ldflags="-s -w" "mvdan.cc/gofumpt@v${GOFUMPT_VERSION}"
GOROOT="$PREFIX/go" GOPATH=/tmp/gopath GOTOOLCHAIN=local CGO_ENABLED=0 \
  "$GOBIN/go" install -ldflags="-s -w" "honnef.co/go/tools/cmd/staticcheck@v${STATICCHECK_VERSION}"
mv /tmp/gopath/bin/gofumpt /tmp/gopath/bin/staticcheck "$PREFIX/bin/"

# Symlinks
ln -sf "$PREFIX/go/bin/go" "$PREFIX/bin/go"
ln -sf "$PREFIX/go/bin/gofmt" "$PREFIX/bin/gofmt"
ln -sf "$PREFIX/go/bin/gopls" "$PREFIX/bin/gopls" 2>/dev/null || true

# Cleanup
chmod -R u+w /tmp/gopath 2>/dev/null || true
rm -rf /tmp/gopath

echo "Go ${GO_FULL_VERSION} + tools installed."
