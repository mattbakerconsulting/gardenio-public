#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Repeatable proof that the gardenio OPERATOR contract (see
# docs/onboarding-mac-operator.md) is complete and correct.
#
# Builds the kd+gb CLIs as static linux binaries, drops them into a bare Alpine
# container with NOTHING else, and asserts:
#   A. token + BEADS_HTTP_URL            -> reaches the daemon (bearer path)   PASS
#   B. token WITHOUT BEADS_HTTP_URL      -> "login session expired" (cookie)   PASS (negative)
#   C. token + BEADS_HTTP_URL, gb ready  -> reaches the daemon                 PASS
#
# This guards the onboarding guide: if the minimal env contract ever changes,
# this test goes red. No VPN, no host mounts, no config files.
#
# Requires: docker, go, and a valid Acme gitlab.com PAT in $GITLAB_TOKEN.
# Usage:    GITLAB_TOKEN=glpat-… ./scripts/test-operator-contract.sh
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST="https://cli.gardenio.example.com/kd"   # 133 new-prod off-VPN CLI/PAT edge (ka-0000002359); old 909 shim cli.gardenio.e2e.example.com is dead
EMAIL="${BEADS_OPERATED_BY_EMAIL:-onboarding-test@acme.example}"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
ARCH="$(uname -m)"; [ "$ARCH" = "x86_64" ] && GOARCH=amd64 || GOARCH=arm64

pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; exit 1; }

[ -n "${GITLAB_TOKEN:-}" ] || fail "set GITLAB_TOKEN to a Acme gitlab.com PAT (scope read_api)"
command -v docker >/dev/null || fail "docker not found"
command -v go     >/dev/null || fail "go not found"

echo "==> cross-compiling static linux/$GOARCH kd + gb"
( cd "$REPO/kbeads"             && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o "$WORK/kd" ./cmd/kd/ )
( cd "$REPO/gardenio/controller" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o "$WORK/gb" ./cmd/gb/ )

cat > "$WORK/Dockerfile" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY kd /usr/local/bin/kd
COPY gb /usr/local/bin/gb
ENTRYPOINT ["/bin/sh","-c"]
EOF

echo "==> building clean-room image"
docker build -q -t gardenio-operator-contract:test "$WORK" >/dev/null

run() { docker run --rm -e GARDENIO_TOKEN="$GITLAB_TOKEN" "$@" gardenio-operator-contract:test "$CMD"; }

echo "==> A: token + BEADS_HTTP_URL  (expect live beads)"
CMD='kd list --limit 2 2>&1'
out="$(run -e BEADS_HTTP_URL="$HOST" -e BEADS_OPERATED_BY="oidc:$EMAIL")"
grep -qiE 'beads \(|^ID ' <<<"$out" && pass "A: bearer path reaches daemon" || fail "A: $out"

echo "==> B: token WITHOUT BEADS_HTTP_URL  (expect cookie-path failure)"
CMD='kd list --limit 1 2>&1'
out="$(run -e BEADS_OPERATED_BY="oidc:$EMAIL" || true)"
grep -qi 'login session expired' <<<"$out" \
  && pass "B: missing BEADS_HTTP_URL correctly falls back to cookie path" \
  || fail "B: expected 'login session expired', got: $out"

echo "==> C: gb ready + BEADS_HTTP_URL  (expect live beads)"
CMD='gb ready --limit 2 2>&1'
out="$(run -e BEADS_HTTP_URL="$HOST" -e BEADS_OPERATED_BY="oidc:$EMAIL")"
grep -qiE 'beads \(|kd-' <<<"$out" && pass "C: gb reaches daemon" || fail "C: $out"

echo
pass "operator contract intact: GARDENIO_TOKEN + BEADS_HTTP_URL + BEADS_OPERATED_BY"
