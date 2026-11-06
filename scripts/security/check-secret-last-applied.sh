#!/usr/bin/env bash
# check-secret-last-applied.sh: list Secrets whose metadata still carries
# kubectl.kubernetes.io/last-applied-configuration (a full cleartext copy of the
# Secret written by `kubectl apply`). Prints namespace/name only, never values.
# Guards gd-htr9j6R3aB: no Secret keeps a cleartext copy in that annotation.
#
# Usage: check-secret-last-applied.sh [--from-file FILE] [--strip]
#   --from-file FILE  read `kubectl get secrets -A -o json` output from FILE (tests)
#   --strip           OPERATOR ONLY: remove the annotation from every hit on the
#                     live cluster, then re-count. Refused with --from-file.
# Exit: 0 none, 1 some remain, 2 usage error.
set -euo pipefail

file=""; strip=0
while [ $# -gt 0 ]; do
  case "$1" in
    --from-file) file="${2:-}"; [ -n "$file" ] || { echo "usage: --from-file FILE" >&2; exit 2; }; shift 2;;
    --strip) strip=1; shift;;
    *) echo "unknown argument: $1" >&2; exit 2;;
  esac
done
if [ "$strip" -eq 1 ] && [ -n "$file" ]; then
  echo "--strip acts on the live cluster and cannot be combined with --from-file" >&2; exit 2
fi

ANN='kubectl.kubernetes.io/last-applied-configuration'
json="$(mktemp)"; trap 'rm -f "$json"' EXIT

load() {
  if [ -n "$file" ]; then cat "$file" >"$json"; else kubectl get secrets -A -o json >"$json"; fi
}
hits() {
  jq -r --arg a "$ANN" '.items[] | select(.metadata.annotations[$a] != null) | "\(.metadata.namespace)/\(.metadata.name)"' "$json"
}
count() {
  local n m
  n="$(hits | wc -l | tr -d ' ')"
  m="$(jq '.items | length' "$json")"
  echo "secrets-with-last-applied: $n of $m"
  [ "$n" -eq 0 ]
}

load
hits
if [ "$strip" -eq 1 ]; then
  echo "before: $(count || true)"
  while IFS=/ read -r ns name; do
    [ -n "$ns" ] || continue
    kubectl -n "$ns" annotate secret "$name" "${ANN}-" >/dev/null
    echo "stripped $ns/$name"
  done < <(hits)
  load
  echo "after:"
fi
count
