#!/usr/bin/env bash
# gen-proto.sh — Regenerate Go code from protobuf definitions.
#
# This script runs protoc with the correct plugins and flags to produce
# the generated Go files in gen/beads/v1/. It is idempotent: running it
# twice produces identical output.
#
# When to run:
#   - After modifying any .proto file in proto/
#   - After cherry-picking from upstream (which uses a different module path)
#
# Prerequisites (install once):
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1
#   # protoc v6.x: https://github.com/protocolbuffers/protobuf/releases
#
# A protoc without its well-known types (Debian's protobuf-compiler without
# libprotobuf-dev ships no google/protobuf/*.proto) fails with
# "google/protobuf/timestamp.proto: File not found". Point PROTOC_INCLUDE at a
# directory holding google/protobuf/{timestamp,wrappers}.proto from the
# matching protobuf release.
#
# Usage:
#   ./scripts/gen-proto.sh          # from repo root
#   make proto                      # via Makefile target
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROTO_DIR="${REPO_ROOT}/proto"
GEN_DIR="${REPO_ROOT}/gen"

# Verify tools are available.
for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  if ! command -v "${tool}" &>/dev/null; then
    echo "Error: ${tool} not found in PATH." >&2
    echo "Install instructions:" >&2
    case "${tool}" in
      protoc)
        echo "  Download from https://github.com/protocolbuffers/protobuf/releases (v6.x)" >&2
        ;;
      protoc-gen-go)
        echo "  go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11" >&2
        ;;
      protoc-gen-go-grpc)
        echo "  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1" >&2
        ;;
    esac
    exit 1
  fi
done

PROTO_PATHS=(--proto_path="${PROTO_DIR}")
if [[ -n "${PROTOC_INCLUDE:-}" ]]; then
  PROTO_PATHS+=(--proto_path="${PROTOC_INCLUDE}")
fi

# Clean previous generated files to ensure no stale output.
rm -rf "${GEN_DIR}/beads"

# Generate Go code from all .proto files.
protoc \
  "${PROTO_PATHS[@]}" \
  --go_out="${GEN_DIR}" \
  --go_opt=paths=source_relative \
  --go-grpc_out="${GEN_DIR}" \
  --go-grpc_opt=paths=source_relative \
  "${PROTO_DIR}"/beads/v1/*.proto

echo "Protobuf generation complete: ${GEN_DIR}/beads/v1/"
