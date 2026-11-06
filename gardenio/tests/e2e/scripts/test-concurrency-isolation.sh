#!/usr/bin/env bash
# test-concurrency-isolation.sh — assert e2e test scripts honor $NAMESPACE so
# two concurrent runs (e.g. the lane's gardenio-e2e-0 + gardenio-e2e-1) can run
# without crossing namespaces (E5, ka-0000002597).
#
# Each run gets its own namespace (e2e.yml leases one from the lane pool via
# lane.sh). The test scripts here are dispatched into that namespace via the
# NAMESPACE env var. If any script literally hardcodes "gardenio-e2e" in a
# kubectl/helm invocation — instead of "$NAMESPACE" or "${NAMESPACE:-...}"
# — two concurrent runs would both target the same (or wrong) namespace
# and stomp on each other's port-forwards, beads, and Helm state.
#
# This script is hermetic: it greps the script tree, never runs kubectl/helm,
# never touches the network, and is safe to run anywhere `bash + grep + awk`
# exist. Pattern matches test-shard-isolation.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

shopt -s nullglob
SCRIPTS=("$SCRIPT_DIR"/*.sh)
shopt -u nullglob

if [ "${#SCRIPTS[@]}" -eq 0 ]; then
  echo "FAIL: no test scripts found under $SCRIPT_DIR" >&2
  exit 1
fi

# Drop self from the audit set so the literal `gardenio-e2e` strings used in
# this script's own comments and regex examples don't trigger its own checks.
FILTERED=()
for s in "${SCRIPTS[@]}"; do
  case "$(basename "$s")" in
    test-concurrency-isolation.sh) continue ;;
  esac
  FILTERED+=("$s")
done
SCRIPTS=("${FILTERED[@]}")

echo "Auditing ${#SCRIPTS[@]} scripts for namespace-hardcoding regressions"

OVERALL_RC=0

# ── Invariant 1: every kubectl/helm namespace flag is parameterized ─────
# A bare `kubectl -n gardenio-e2e ...` or `helm --namespace gardenio-e2e ...`
# in any test script breaks ephemeral isolation. Allow `${NAMESPACE...}`
# (any expansion form) as the namespace argument.
#
# We scan for the literal token `gardenio-e2e` after `-n `, `--namespace `,
# `--namespace=`, or `-n=`. Anything else (comments, defaults, log strings)
# is fine and is checked separately below.
check_kubectl_helm_hardcoded() {
  local rc=0 hits
  # Match (kubectl|helm) ... (-n|--namespace)[ =]gardenio-e2e (literal, no $).
  # We require either end-of-line, whitespace, or quote after the literal so
  # we don't false-positive on `gardenio-e2e-${INSTANCE_ID}`.
  hits=$(grep -nE \
    '(kubectl|helm)[^#]*(-n|--namespace)([= ])gardenio-e2e([[:space:]"'\''$]|$)' \
    "${SCRIPTS[@]}" 2>/dev/null \
    | grep -vE '^\s*#' || true)
  if [ -n "$hits" ]; then
    echo "FAIL: kubectl/helm calls hardcoding gardenio-e2e as namespace arg:" >&2
    printf '  %s\n' "$hits" >&2
    rc=1
  else
    echo "PASS: no kubectl/helm calls hardcode gardenio-e2e as namespace"
  fi
  return $rc
}

# ── Invariant 2: every script that uses kubectl declares NAMESPACE ──────
# A script that reaches for kubectl without sourcing/declaring NAMESPACE
# silently falls back to whatever the kubeconfig context picks — which, on
# a developer machine pointed at the cluster, may not be the ephemeral
# instance. We require either `NAMESPACE=` somewhere in the file (declares
# it locally or as a fallback) OR sourcing of a lib that declares it.
check_kubectl_uses_namespace_var() {
  local rc=0
  local script base
  for script in "${SCRIPTS[@]}"; do
    base=$(basename "$script")
    # Skip non-test helper scripts, which don't talk to the cluster directly.
    case "$base" in
      lib-*.sh|seed-types-and-users.sh|test-shard-isolation.sh|\
      test-concurrency-isolation.sh|test-lib-gvisor-forensics.sh|\
      test-gb-whoami.sh)
        # test-lib-gvisor-forensics.sh is a HERMETIC unit test (Rig R2,
        # ka-0000006948): it drives a FAKE kubectl + FAKE node runner, never a
        # real cluster, so the ephemeral-NAMESPACE isolation invariant does not
        # apply. The literal "kubectl" tokens it carries are fake-var names and
        # assertion message strings, not cluster calls. Skipped alongside the
        # other hermetic lints.
        #
        # test-gb-whoami.sh (ka-0000001750) is likewise HERMETIC: it drives the
        # real `gb whoami` binary against a FAKE kubectl (plus fake aws/glab)
        # shadowed on a curated PATH — never a real cluster or namespace. Its
        # `kubectl` tokens are the stub it writes and the fail-open leg that
        # removes that stub, not a namespaced cluster call, so the
        # ephemeral-NAMESPACE invariant does not apply.
        continue ;;
    esac
    # Does the script invoke kubectl?
    if ! grep -qE '^\s*[^#]*\bkubectl\b' "$script"; then
      continue
    fi
    # Does it bind NAMESPACE somewhere? Either as a literal assignment or
    # as a positional arg in a sourced helper. We accept either pattern.
    if ! grep -qE '^\s*NAMESPACE=|NAMESPACE="\$\{NAMESPACE' "$script"; then
      echo "FAIL: $base invokes kubectl but never declares NAMESPACE" >&2
      rc=1
    fi
  done
  if [ "$rc" -eq 0 ]; then
    echo "PASS: every kubectl-using script declares NAMESPACE"
  fi
  return $rc
}

# ── Invariant 3: NAMESPACE default fallbacks point at a known dev ns ───
# Scripts use `NAMESPACE="${NAMESPACE:-<default>}"` for back-compat with
# single-instance dev workflows where the operator runs the script directly.
# The fallback only matters when CI didn't export NAMESPACE, so it never
# affects ephemeral isolation — but a typo'd fallback (e.g. "gardenio-rxw")
# would silently misroute developer invocations. We allow the one
# well-known dev namespace, `gardenio-e2e`. A new fallback name would
# need to be deliberately added here.
check_namespace_default_consistency() {
  local rc=0 hits
  hits=$(grep -nE 'NAMESPACE="\$\{NAMESPACE:-[^}]+\}"' "${SCRIPTS[@]}" 2>/dev/null \
    | grep -vE 'NAMESPACE:-gardenio-e2e\}' || true)
  if [ -n "$hits" ]; then
    echo "FAIL: NAMESPACE fallback uses an unknown dev namespace in:" >&2
    printf '  %s\n' "$hits" >&2
    echo "  (allowed: gardenio-e2e — extend this check if you add another)" >&2
    rc=1
  else
    echo "PASS: NAMESPACE fallback defaults point at known dev namespaces"
  fi
  return $rc
}

# ── Invariant 4: BEADS_SVC / BRIDGE_SVC are derived from NAMESPACE ──────
# Service names are templated by Helm as `{{ include "gardenio.fullname" . }}-…`
# which expands to `<release-name>-…`. Since release name == namespace
# (lane.sh installs the release under the namespace's own name), the in-cluster
# service names are `${NAMESPACE}-beads`, `${NAMESPACE}-slack-bridge`, etc. Any
# script that hardcodes `gardenio-e2e-beads` or `gardenio-e2e-slack-bridge`
# would target the wrong instance.
check_service_names_templated() {
  local rc=0 hits
  hits=$(grep -nE '"gardenio-e2e-(beads|slack-bridge|postgres|nats)[" ]' \
    "${SCRIPTS[@]}" 2>/dev/null \
    | grep -vE '^\s*[^:]+:[0-9]+:\s*#' || true)
  if [ -n "$hits" ]; then
    echo "FAIL: scripts reference hardcoded service names instead of \${NAMESPACE}-svc:" >&2
    printf '  %s\n' "$hits" >&2
    rc=1
  else
    echo "PASS: service references are templated via \${NAMESPACE}"
  fi
  return $rc
}

check_kubectl_helm_hardcoded   || OVERALL_RC=1
check_kubectl_uses_namespace_var || OVERALL_RC=1
check_namespace_default_consistency || OVERALL_RC=1
check_service_names_templated  || OVERALL_RC=1

echo ""
if [ "$OVERALL_RC" -eq 0 ]; then
  echo "OK: e2e scripts honor \$NAMESPACE — concurrent runs are isolated"
else
  echo "FAIL: namespace-isolation invariants violated — see errors above" >&2
fi
exit "$OVERALL_RC"
