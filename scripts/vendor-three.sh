#!/usr/bin/env bash
# vendor-three.sh: re-vendor three.js for the splash (gardenio/controller/internal/gardenhub/web/vendor/, gd-xN0lNXOT4l).
#
# three.js no longer ships a single-file build: build/three.module.js imports build/three.core.js, and r186 dropped the
# .min.js files. So this downloads the pinned npm tarball, checks its sha256, bundles the two into one minified ES module
# with a pinned esbuild (deterministic: the same inputs give the same bytes) and checks the result's sha256 too. Both pins
# are recorded in web/vendor/VENDOR.md; a version bump changes all of them in this script and that file together.
#
# Usage: scripts/vendor-three.sh            (run by `make vendor-three`; needs curl, tar, sha256sum and npx)
set -euo pipefail

THREE_VERSION=0.186.1
THREE_TGZ_SHA256=8cd068708ea44f2c73c944b1cead2ba2f0d5c15c8fc194e5700f4e4f4a033fe7
ESBUILD_VERSION=0.28.2
OUT_SHA256=ed11a90814a27be663f430328985511ad86e03fd56a610d2347eea6f3139e705

root=$(cd "$(dirname "$0")/.." && pwd)
dest="$root/gardenio/controller/internal/gardenhub/web/vendor/three.module.min.js"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/three.tgz" "https://registry.npmjs.org/three/-/three-$THREE_VERSION.tgz"
echo "$THREE_TGZ_SHA256  $tmp/three.tgz" | sha256sum -c --quiet
tar -xzf "$tmp/three.tgz" -C "$tmp" package/build/three.module.js package/build/three.core.js
(cd "$tmp" && npx --yes "esbuild@$ESBUILD_VERSION" package/build/three.module.js \
  --bundle --minify --format=esm --target=es2020 --legal-comments=inline --log-level=warning --outfile=three.module.min.js)
echo "$OUT_SHA256  $tmp/three.module.min.js" | sha256sum -c --quiet
cp "$tmp/three.module.min.js" "$dest"
echo "vendored three.js $THREE_VERSION: $dest ($(wc -c <"$dest") bytes, sha256 $OUT_SHA256)"
