#!/usr/bin/env bash
# Render tests for the opt-in OpenZiti overlay chart (gardenio/helm/ziti).
#
#   (a) default values render zero documents -- nothing deploys ziti unless
#       ziti.enabled=true
#   (b) ziti.enabled=true renders exactly the expected kind/namespace/name set
#   (c) ziti.hostBeads.enabled=true adds the beads ziti-host objects
#   (d) the enabled render is the template files' own content, i.e. wrapping
#       them in {{ if }} changed nothing (the one deliberate difference: the
#       Namespace lives in namespace.yaml once, not in each router file)
#   (e) a per-component switch turns just that component off
#
# Needs helm and yq (mikefarah, v4). Run from anywhere:
#   bash gardenio/helm/ziti/tests/render_test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart="$(dirname "$here")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { # check <desc> <expected-file> <actual-file>
  if diff -u "$2" "$3" >"$tmp/diff"; then
    echo "ok   $1"
  else
    echo "FAIL $1"
    cat "$tmp/diff"
    fail=1
  fi
}

render() { helm template t "$chart" "$@"; }

# kind/namespace/name of every document, sorted. A cluster-scoped object has an
# empty namespace, which is why the Namespace is spelled `Namespace//ziti`.
ids() { yq eval -N 'select(.kind) | [.kind, (.metadata.namespace // ""), .metadata.name] | join("/")' - | sed '/^$/d' | sort; }

# (a) off by default
render >"$tmp/off.yaml"
: >"$tmp/empty"
ids <"$tmp/off.yaml" >"$tmp/off.ids"
check "(a) default values render zero documents" "$tmp/empty" "$tmp/off.ids"

# (b) enabled
cat >"$tmp/want.enabled" <<'WANT'
Certificate/ziti/ziti-controller-public
ConfigMap/kube-system/coredns-custom
ConfigMap/ziti/ziti-controller-config
Deployment/gardenio/ziti-host
Deployment/ziti/router-aws-1
Deployment/ziti/router-public-1
Deployment/ziti/ziti-controller
Namespace//ziti
PersistentVolumeClaim/ziti/router-aws-1-identity
PersistentVolumeClaim/ziti/router-public-1-identity
PodDisruptionBudget/gardenio/ziti-host
PodDisruptionBudget/ziti/router-aws-1
PodDisruptionBudget/ziti/router-public-1
PodDisruptionBudget/ziti/ziti-controller
Service/ziti/ziti-controller
WANT
sort -o "$tmp/want.enabled" "$tmp/want.enabled"
render --set ziti.enabled=true >"$tmp/on.yaml"
ids <"$tmp/on.yaml" >"$tmp/on.ids"
check "(b) ziti.enabled=true renders the expected objects" "$tmp/want.enabled" "$tmp/on.ids"

# (c) beads host
{ cat "$tmp/want.enabled"; printf '%s\n' Deployment/beads/ziti-host PodDisruptionBudget/beads/ziti-host; } | sort >"$tmp/want.beads"
render --set ziti.enabled=true,ziti.hostBeads.enabled=true >"$tmp/beads.yaml"
ids <"$tmp/beads.yaml" >"$tmp/beads.ids"
check "(c) ziti.hostBeads.enabled=true adds the beads ziti-host objects" "$tmp/want.beads" "$tmp/beads.ids"

# (d) content unchanged: strip only the wrapper lines from the template files
# and compare each document, canonicalised to one line of JSON, with the render.
canon() { yq eval -N -o=json -I=0 'select(.kind)' - | sed '/^$/d' | sort; }
for f in "$chart"/templates/*.yaml; do
  [ "$(basename "$f")" = namespace.yaml ] && continue
  grep -v -e '^{{- if and \.Values\.ziti\.enabled \.Values\.ziti\.[A-Za-z]*\.enabled }}$' -e '^{{- end }}$' "$f"
  echo '---'
done | canon >"$tmp/src.canon"
yq eval -N -o=json -I=0 'select(.kind and .kind != "Namespace")' - <"$tmp/beads.yaml" | sed '/^$/d' | sort >"$tmp/rendered.canon"
check "(d) rendered documents equal the template files' own content" "$tmp/src.canon" "$tmp/rendered.canon"
if grep -q '{{' "$chart"/templates/*.yaml; then
  # Only the wrapper actions may appear; anything else needs escaping review.
  if grep -h '{{' "$chart"/templates/*.yaml | grep -v -e '^{{- if .*}}$' -e '^{{- end }}$' | grep -q .; then
    echo "FAIL (d) unexpected template action in a ziti manifest"
    fail=1
  fi
fi

# (e) one component off
render --set ziti.enabled=true,ziti.controller.enabled=false >"$tmp/nocontroller.yaml"
ids <"$tmp/nocontroller.yaml" >"$tmp/nocontroller.ids"
grep -v -e '/ziti/ziti-controller' -e '/ziti-controller-' "$tmp/want.enabled" >"$tmp/want.nocontroller"
check "(e) ziti.controller.enabled=false drops only the controller" "$tmp/want.nocontroller" "$tmp/nocontroller.ids"

exit "$fail"
