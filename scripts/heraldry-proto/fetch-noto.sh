#!/usr/bin/env bash
# fetch-noto.sh DIR — sparse, blob-filtered checkout of googlefonts/noto-emoji at
# the pinned tag (noto.go NotoTag), holding only 2D/svg and 2D/png/72 (~66 MB,
# a few seconds). The prototype reads artwork from DIR; nothing is vendored.
set -euo pipefail

TAG="v2026-09-24-unicode18_0"
DIR="${1:?usage: fetch-noto.sh DIR}"

if [[ -d "$DIR/.git" ]]; then
  have="$(git -C "$DIR" describe --tags --exact-match 2>/dev/null || true)"
  if [[ "$have" == "$TAG" ]]; then
    echo "noto-emoji $TAG already at $DIR"
    exit 0
  fi
  echo "fetch-noto: $DIR holds ${have:-an untagged checkout}, not $TAG; remove it first" >&2
  exit 1
fi

git clone -q --depth 1 --branch "$TAG" --filter=blob:none --sparse \
  https://github.com/googlefonts/noto-emoji.git "$DIR"
git -C "$DIR" sparse-checkout set 2D/svg 2D/png/72
echo "noto-emoji $TAG ($(git -C "$DIR" rev-parse --short HEAD)) at $DIR"
