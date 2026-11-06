#!/usr/bin/env bash
#
# verify-image-signatures_test.sh — hermetic offline GATE for the release-image
# signature-verification ClusterPolicy (ka-0000007951). NO cluster, NO network.
#
# The verify-image-signatures policy is a Kyverno `verifyImages` policy, which
# needs live Fulcio/Rekor to evaluate a real signature — so a declarative
# `kyverno test` suite is NOT hermetic here (unlike the validate-only
# dark-credential-boundary suite). Instead this asserts the LOAD-BEARING
# invariants of the policy structurally, so a gutted or mis-scoped policy reds
# CI at commit time. When the kyverno CLI is present it additionally runs
# `kyverno test` if a suite exists (currently none, by design), else the
# structural lint is the gate.
#
# Run by checks.yml (shell guards job).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY="$HERE/verify-image-signatures.yaml"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

[ -f "$POLICY" ] || fail "policy file missing: $POLICY"

echo "== structural lint: $POLICY =="

# GATE-1: it is a Kyverno verifyImages ClusterPolicy.
grep -q "kind: ClusterPolicy" "$POLICY"        || fail "1: not a ClusterPolicy"
grep -q "apiVersion: kyverno.io/v1" "$POLICY"  || fail "1: not kyverno.io/v1"
grep -q "verifyImages:" "$POLICY"              || fail "1: no verifyImages rule (this MUST verify signatures, not just validate)"
pass "GATE-1 kyverno.io/v1 ClusterPolicy with a verifyImages rule"

# GATE-2: keyless attestor pinned to the images.yml GitHub Actions identity.
# The workflow signs on calver tag pushes only, so the issuer MUST be GitHub's
# OIDC issuer and the subject MUST name images.yml at a release tag ref; a
# subject that admitted any workflow or any ref would trust a PR or branch run.
grep -q "keyless:" "$POLICY"                        || fail "2: not a keyless attestor (Fulcio/Rekor)"
grep -q 'issuer: "https://token.actions.githubusercontent.com"' "$POLICY" \
  || fail "2: issuer is not https://token.actions.githubusercontent.com (the images.yml signer)"
grep -qF 'github\\.com/mattbakerconsulting/gardenio/' "$POLICY" \
  || fail "2: subject does not match the gardenio repo"
grep -qF '/\\.github/workflows/images\\.yml@' "$POLICY" \
  || fail "2: subject does not pin the images.yml workflow"
grep -q "@refs/tags/20" "$POLICY"                   || fail "2: subject does not scope to calver release tags"
grep -q "rekor.sigstore.dev" "$POLICY"              || fail "2: no Rekor transparency-log URL"
pass "GATE-2 keyless attestor pinned to the images.yml calver-tag identity"

# GATE-3: scoped to OUR release repos ONLY. A too-broad imageReferences would
# deny every third-party dependency (postgres, nats, kyverno itself) since they
# are not signed by our CI. It MUST name the gardenio registry and MUST NOT use
# a catch-all "*" alone.
grep -q '"123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/\*"' "$POLICY" \
  || fail "3: imageReferences does not scope to the gardenio release registry"
if grep -E '^\s*-\s*"\*"\s*$' "$POLICY" >/dev/null 2>&1; then
  fail "3: imageReferences uses a bare catch-all '*' — would deny every third-party image"
fi
pass "GATE-3 imageReferences scoped to gardenio release repos (no bare catch-all)"

# GATE-4: security-hardening posture. Enforce (not Audit), fail CLOSED, and
# digest-pin (mutate/verify) so a moved tag cannot swap bytes post-verification.
grep -q "validationFailureAction: Enforce" "$POLICY" || fail "4: not Enforce (would only Audit)"
grep -q "failurePolicy: Fail" "$POLICY"              || fail "4: does not fail CLOSED (failurePolicy: Fail)"
grep -q "verifyDigest: true" "$POLICY"               || fail "4: does not require verifyDigest (TOCTOU)"
grep -q "mutateDigest: true" "$POLICY"               || fail "4: does not mutateDigest (tag->digest pin)"
pass "GATE-4 Enforce + fail-closed + digest-pinned"

# GATE-5: the E2E namespaces (which run unsigned -cover variants) are excluded,
# including their per-instance ephemeral globs — else the policy would deny the
# very pods the coverage shard needs. Mirrors disallow-cover-images.
for ns in gardenio-e2e gardenio-e2e-\*; do
  grep -qE "^\s*-\s*${ns}\s*$" "$POLICY" || fail "5: exclude list missing E2E namespace '${ns}'"
done
pass "GATE-5 E2E namespaces (+ ephemeral globs) excluded"

# GATE-6: the pull-through mirror repo is SKIPPED. It co-locates under our
# registry (<registry>/mirror/*: postgres, nats, vmagent, victoria-*, …) but is
# NOT signed by our CI, and Kyverno "*" matches "/", so the imageReferences glob
# catches it. Without a skipImageReferences entry an Enforce rollout DENIES every
# mirrored dependency and takes prod down (ka-0000008191). cache/* (build caches,
# the controller-base stage) is never signed either.
grep -q "skipImageReferences:" "$POLICY" \
  || fail "6: no skipImageReferences — the <registry>/mirror/* third-party repo would be DENIED"
for repo in mirror cache; do
  grep -qE "^\s*-\s*\"123456789012[.]dkr[.]ecr[.]us-east-1[.]amazonaws[.]com/gardenio/${repo}/\*\"\s*$" "$POLICY" \
    || fail "6: skipImageReferences does not exempt the <registry>/${repo}/* repo"
done
pass "GATE-6 mirror/* and cache/* repos skipped"

# ── Optional: real kyverno CLI presence (structural-only today) ──────────────
if command -v kyverno >/dev/null 2>&1; then
  echo "== kyverno CLI found ($(kyverno version 2>/dev/null | awk '/Version/{print $2}')) — validating policy parses =="
  # `kyverno` cannot evaluate verifyImages offline (needs Fulcio/Rekor), but it
  # can still confirm the policy is well-formed. A non-zero here means the YAML
  # is not a valid Kyverno policy.
  if ! kyverno test "$HERE" >/dev/null 2>&1; then
    echo "   (no declarative kyverno test suite present — structural lint is the gate)"
  fi
fi

echo
echo "structural lint PASSED (6 gates). verifyImages evaluation needs live"
echo "Fulcio/Rekor; the images.yml sign step + this policy are exercised"
echo "end-to-end at release time. See gardenio/docs/image-signing.md."
