#!/usr/bin/env bash
# Planted positive control for check-secret-last-applied.sh: a fixture with one
# annotated Secret must fail and name it without printing the value; a clean
# fixture must pass.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0

cat >"$tmp/dirty.json" <<'JSON'
{"items":[
 {"metadata":{"namespace":"gardenio","name":"clean-one","annotations":{}}},
 {"metadata":{"namespace":"ziti","name":"planted","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"token\":\"UExBTlRFRC1TRUNSRVQ=\"}}"}}}
]}
JSON
cat >"$tmp/clean.json" <<'JSON'
{"items":[{"metadata":{"namespace":"gardenio","name":"clean-one"}}]}
JSON

set +e
out="$(bash "$here/check-secret-last-applied.sh" --from-file "$tmp/dirty.json")"; rc=$?
set -e
if [ "$rc" -eq 1 ] && [[ "$out" == *"ziti/planted"* ]] && [[ "$out" == *"secrets-with-last-applied: 1 of 2"* ]]; then
  echo "PASS dirty fixture fails and names the Secret"
else
  echo "FAIL dirty fixture: rc=$rc out=$out"; fail=1
fi
if [[ "$out" == *"UExBTlRFRC1TRUNSRVQ"* ]]; then
  echo "FAIL the annotation value was printed"; fail=1
else
  echo "PASS no value printed"
fi

set +e
out="$(bash "$here/check-secret-last-applied.sh" --from-file "$tmp/clean.json")"; rc=$?
set -e
if [ "$rc" -eq 0 ] && [[ "$out" == *"secrets-with-last-applied: 0 of 1"* ]]; then
  echo "PASS clean fixture passes"
else
  echo "FAIL clean fixture: rc=$rc out=$out"; fail=1
fi

set +e
bash "$here/check-secret-last-applied.sh" --from-file "$tmp/clean.json" --strip >/dev/null 2>&1; rc=$?
set -e
if [ "$rc" -eq 2 ]; then echo "PASS --strip refused with --from-file"; else echo "FAIL --strip with --from-file rc=$rc"; fail=1; fi

exit "$fail"
