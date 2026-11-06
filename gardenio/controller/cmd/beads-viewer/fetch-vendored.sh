#!/usr/bin/env bash
# fetch-vendored.sh: download the third-party JS libraries beads-viewer serves from templates/static/ (gd-wNQzR9ptZI).
#
# mermaid.min.js (3.5 MB) and echarts.min.js (1.0 MB) are upstream release files, byte for byte, so they are fetched
# at build time instead of committed: every agent pod clones the tip tree on cold start (scripts/check-large-tracked-files.sh).
# Each file is pinned by version and sha256 and checked after download; a file already present with the right hash is
# left alone, so a re-run is free. They are gitignored, and `//go:embed templates/static` embeds whatever is there, so a
# build that skipped this still compiles but serves 404 for them (and beads-viewer logs a warning at startup).
#
# Who runs it: the controller-base image (gardenio/images/_controller-base/Dockerfile), the controller `test` step in
# .github/workflows/ci.yml, e2e.yml's beads-viewer build, `make -C gardenio/controller test`, `make run-beads-viewer`
# and `make fetch-viewer-vendor`. Run it yourself before `go test ./cmd/beads-viewer/` or `go build` of beads-viewer.
#
# To upgrade a library, change its line below (path and sha256 together), run this, and check the page that uses it:
# mermaid renders diagrams in markdown (templates/static/mermaid_render.js), echarts draws /dashboards.
#
# Usage: fetch-vendored.sh [dest-dir]   (default: templates/static next to this script; needs curl and sha256sum or shasum)
set -euo pipefail

# name  npm-path (served by every mirror below)  sha256
LIBS=(
  "mermaid.min.js mermaid@11.16.0/dist/mermaid.min.js 74d7c46dabca328c2294733910a8aa1ed0c37451776e8d5295da38a2b758fb9b"
  "echarts.min.js echarts@5.5.1/dist/echarts.min.js e84270bd0cd5bdf60fefc26d00c2a391cb2e81f4d26a7a9ee16185a54773a3cf"
)
MIRRORS=(https://cdn.jsdelivr.net/npm https://unpkg.com)

dest=${1:-"$(cd "$(dirname "$0")" && pwd)/templates/static"}
mkdir -p "$dest"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

for lib in "${LIBS[@]}"; do
  read -r name path want <<<"$lib"
  if [[ -f "$dest/$name" && "$(sha256 "$dest/$name")" == "$want" ]]; then
    continue
  fi
  got=""
  for mirror in "${MIRRORS[@]}"; do
    if curl -fsSL --retry 3 --retry-all-errors --connect-timeout 10 --max-time 120 -o "$tmp/$name" "$mirror/$path"; then
      got=$(sha256 "$tmp/$name")
      [[ "$got" == "$want" ]] && break
      echo "fetch-vendored: $mirror/$path has sha256 $got, want $want" >&2
    else
      echo "fetch-vendored: download failed from $mirror/$path" >&2
    fi
  done
  if [[ "$got" != "$want" ]]; then
    echo "fetch-vendored: could not fetch $name with sha256 $want from any mirror" >&2
    exit 1
  fi
  mv "$tmp/$name" "$dest/$name"
  echo "fetch-vendored: $dest/$name ($path)"
done
