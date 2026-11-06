#!/usr/bin/env bash
# ci-fetch-verified.sh URL SHA256 DEST [MIRROR_URL...] -- download a pinned
# artifact for CI, verify its sha256, and keep a verified copy in the
# node-local /cache.
#
# Why (gd-OJQWL1W3S3): the runners share a slow uplink. On 2026-10-28 the
# 56 MB node tarball from nodejs.org crawled at 50-100 KB/s, so a bare curl
# either hung until the job was cancelled or died with curl (92) HTTP/2
# INTERNAL_ERROR, and `curl --retry --max-time` restarted from zero every
# attempt and never finished. So this script:
#   - reuses /cache/dl/<sha256> when present: /cache is a hostPath shared by
#     every job on the node (gardenio/helm/ci/arc-runners-values.yaml), so
#     one good download serves the node until the pin changes;
#   - otherwise downloads over HTTP/1.1, resuming the partial file (-C -) on
#     each attempt, and aborts a stalled transfer rather than hanging;
#   - never trusts an unverified byte: the cache entry is written only after
#     the sha256 matches, via a rename so concurrent jobs never see a partial.
#
# Why mirrors (gd-zEF9GEEgkp): resuming was not enough. Later that day
# nodejs.org itself served under 10 KB/s -- to the runners and to agent pods
# alike -- and a cold node ran out all 12 attempts at 11 of 56 MB (ci run
# 40670310223). So attempts rotate round-robin over URL and every
# MIRROR_URL. Mirrors must serve the SAME BYTES as URL: the one sha256
# verifies whatever arrived, and a resume may stitch bytes from two sources.
# A source that serves different bytes costs a retry, never a bad install.
set -euo pipefail

url=$1 sha=$2 dest=$3
shift 3
sources=("$url" "$@")
cache_dir=${CI_FETCH_CACHE_DIR:-/cache/dl}
attempts=${CI_FETCH_ATTEMPTS:-12}
backoff=${CI_FETCH_BACKOFF:-1}

verified() { echo "$sha  $1" | sha256sum -c --status -; }

if [ -f "$cache_dir/$sha" ] && verified "$cache_dir/$sha"; then
  cp "$cache_dir/$sha" "$dest"
  echo "ci-fetch-verified: cache hit $cache_dir/$sha"
  exit 0
fi

part="$dest.part"
rm -f "$part"
for i in $(seq 1 "$attempts"); do
  # A previous attempt may have landed every byte and still exited non-zero.
  [ -s "$part" ] && verified "$part" && break
  src=${sources[$(((i - 1) % ${#sources[@]}))]}
  # --speed-limit/--speed-time: give up on an attempt that moves <10 KB/s for
  # 30s; the next attempt resumes from the bytes already on disk.
  rc=0
  curl -fsSL --http1.1 -C - --connect-timeout 20 \
    --speed-limit 10240 --speed-time 30 "$src" -o "$part" || rc=$?
  if [ "$rc" -eq 0 ]; then
    verified "$part" && break
    # Every byte arrived and the sum is wrong: a source served something
    # else, maybe stitched onto another's partial. Start clean.
    echo "ci-fetch-verified: attempt $i/$attempts from $src completed with a sha256 mismatch; discarding" >&2
    rm -f "$part"
  else
    # 33: the source refused the range, so this partial cannot resume there.
    [ "$rc" -eq 33 ] && rm -f "$part"
    echo "ci-fetch-verified: attempt $i/$attempts from $src stopped (curl $rc) at $(stat -c %s "$part" 2>/dev/null || echo 0) bytes; resuming" >&2
  fi
  if [ "$i" -lt "$attempts" ]; then sleep $((backoff * (i < 5 ? i * 2 : 10))); fi
done

if ! { [ -f "$part" ] && verified "$part"; }; then
  echo "ci-fetch-verified: sha256 mismatch or incomplete download for ${sources[*]}" >&2
  rm -f "$part"
  exit 1
fi
mv "$part" "$dest"

if mkdir -p "$cache_dir" 2>/dev/null && cp "$dest" "$cache_dir/$sha.tmp.$$" 2>/dev/null; then
  mv -f "$cache_dir/$sha.tmp.$$" "$cache_dir/$sha"
  echo "ci-fetch-verified: cached $cache_dir/$sha"
fi
