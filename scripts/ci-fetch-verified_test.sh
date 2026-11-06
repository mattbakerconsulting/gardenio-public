#!/usr/bin/env bash
# Self-test for ci-fetch-verified.sh and ci-fetch-node.sh. Hermetic: file://
# sources and a refused localhost port, no network.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
fetch="$here/ci-fetch-verified.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export CI_FETCH_CACHE_DIR="$tmp/cache" CI_FETCH_ATTEMPTS=4 CI_FETCH_BACKOFF=0
head -c 200000 /dev/urandom >"$tmp/good"
head -c 200000 /dev/urandom >"$tmp/evil"
sha=$(sha256sum "$tmp/good" | cut -d' ' -f1)
dead=http://127.0.0.1:9/node.tar.gz

fail=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1" >&2; fail=1; }

# A dead primary rotates onto the mirror, and the result is cached.
if "$fetch" "$dead" "$sha" "$tmp/out1" "file://$tmp/good" 2>"$tmp/err1" &&
  cmp -s "$tmp/good" "$tmp/out1" && cmp -s "$tmp/good" "$CI_FETCH_CACHE_DIR/$sha"; then
  ok "dead primary falls over to the mirror and caches"
else
  bad "dead primary falls over to the mirror and caches"; cat "$tmp/err1" >&2
fi

# The cache answers without touching any source.
if "$fetch" "$dead" "$sha" "$tmp/out2" | grep -q 'cache hit' && cmp -s "$tmp/good" "$tmp/out2"; then
  ok "cache hit needs no source"
else
  bad "cache hit needs no source"
fi
rm -rf "$CI_FETCH_CACHE_DIR"

# A source serving different bytes never installs and never caches.
if "$fetch" "file://$tmp/evil" "$sha" "$tmp/out3" 2>"$tmp/err3"; then
  bad "wrong bytes are refused"
elif [ -e "$tmp/out3" ] || [ -e "$tmp/out3.part" ] || [ -e "$CI_FETCH_CACHE_DIR/$sha" ]; then
  bad "wrong bytes leave nothing behind"
elif grep -q 'sha256 mismatch' "$tmp/err3"; then
  ok "wrong bytes are refused and leave nothing behind"
else
  bad "wrong bytes are refused with a reason"; cat "$tmp/err3" >&2
fi

# A wrong-bytes primary is discarded and the good mirror still wins.
if "$fetch" "file://$tmp/evil" "$sha" "$tmp/out4" "file://$tmp/good" 2>/dev/null &&
  cmp -s "$tmp/good" "$tmp/out4"; then
  ok "wrong-bytes primary does not poison the mirror's download"
else
  bad "wrong-bytes primary does not poison the mirror's download"
fi
rm -rf "$CI_FETCH_CACHE_DIR"

# A partial from one source resumes from the other (same bytes).
head -c 70000 "$tmp/good" >"$tmp/half"
if "$fetch" "file://$tmp/half" "$sha" "$tmp/out5" "file://$tmp/good" 2>/dev/null &&
  cmp -s "$tmp/good" "$tmp/out5"; then
  ok "a short source is completed by the mirror"
else
  bad "a short source is completed by the mirror"
fi

# ci-fetch-node.sh rejects an unknown extension before fetching anything.
rc=0; "$here/ci-fetch-node.sh" zip "$tmp/out6" 2>/dev/null || rc=$?
if [ "$rc" -eq 2 ]; then ok "ci-fetch-node rejects an unknown EXT"; else bad "ci-fetch-node rejects an unknown EXT (rc=$rc)"; fi

exit "$fail"
