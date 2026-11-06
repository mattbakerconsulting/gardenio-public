#!/usr/bin/env bash
# assemble-tarboat_test.sh — Unit tests for assemble-tarboat.sh (Tarboat T1/G1.3,
# ka-0000000192). Runs FULLY OFFLINE: TARBOAT_FETCH=0 means the assembler consumes
# pre-seeded staged assets instead of downloading, so this needs no network and
# no container tooling — just bash, tar, zstd, sha256sum (same posture as
# bootstrap-ec2_test.sh / build-oci-archive_test.py, wired into make lint + ci.yml).
#
# It builds a synthetic mini-monorepo + a fake image-archive, runs the assembler,
# and asserts the contract that G1.1 (bootstrap-ec2.sh) + G1.2 depend on:
#   - bin/{k3s,k3s-install.sh,helm,crane,zot}            (exact names)
#   - manifests/{cert-manager,servicemonitor-crd}.yaml
#   - charts/keycloak-operator-<ver>.tgz                 (derived version)
#   - images/<the reused archive> (+ .sha256)            (NOT rebuilt)
#   - repo/scripts/{bootstrap-ec2.sh,deploy-byo.sh}      (detect_repo_dir sentinel)
#   - LICENSES/NOTICE mentions MinIO AGPL-v3
#   - MANIFEST + SHA256SUMS present and SHA256SUMS verifies (sha256sum -c)
#   - one gardenio-tarboat-<calver>.tar.zst + .sha256 sidecar that round-trips
#
# Usage:  bash scripts/tarboat/assemble-tarboat_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ASSEMBLER="${SCRIPT_DIR}/assemble-tarboat.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: ${2:-}"; FAIL=$((FAIL + 1)); }

command -v zstd >/dev/null 2>&1 || { echo "SKIP: zstd not installed"; exit 0; }

VERSION="2026.161.99"
ARCH="amd64"

# ── Build a synthetic monorepo root the assembler treats as --repo-root ──────
ROOT="$TMP/repo-root"
mkdir -p "$ROOT/scripts/inbox" "$ROOT/scripts/k3s" \
         "$ROOT/deploy-values" "$ROOT/gardenio/helm/gardenio" "$ROOT/gardenio/docs"

# bootstrap-ec2.sh: the assembler DERIVES the operator-chart/manifest pins from
# the OP_* assignments here (single source of truth — proves derive_version).
cat > "$ROOT/scripts/bootstrap-ec2.sh" <<'EOS'
#!/usr/bin/env bash
OP_CERT_MANAGER_VERSION="v1.16.2"
OP_SERVICEMONITOR_CRD_VERSION="v0.76.0"
OP_KEYCLOAK_OPERATOR_VERSION="1.32.0"
OP_KEYCLOAK_OPERATOR_REPO="https://epam.github.io/edp-helm-charts/stable"
EOS
chmod +x "$ROOT/scripts/bootstrap-ec2.sh"
# deploy-byo.sh is the detect_repo_dir sentinel G1.2/G1.1 rely on.
echo '#!/usr/bin/env bash' > "$ROOT/scripts/deploy-byo.sh"; chmod +x "$ROOT/scripts/deploy-byo.sh"
# install-registry.sh: ZOT_VERSION is derived from here.
cat > "$ROOT/scripts/inbox/install-registry.sh" <<'EOS'
#!/usr/bin/env bash
ZOT_VERSION="${ZOT_VERSION:-v2.1.2}"
EOS
echo "k3s durability stub" > "$ROOT/scripts/k3s/install-k3s-durability.sh"
echo "byo values" > "$ROOT/deploy-values/gardenio-byo.yaml"
echo "apiVersion: v2" > "$ROOT/gardenio/helm/gardenio/Chart.yaml"
echo "# install doc" > "$ROOT/gardenio/docs/gardenio-in-a-box-install.md"
echo "MIT-ish" > "$ROOT/LICENSE"

# ── Pre-seed staged binaries/manifests/chart (TARBOAT_FETCH=0 path) ──────────
STAGING="$TMP/work"
TREE="$STAGING/gardenio-tarboat-${VERSION}"
mkdir -p "$TREE/bin" "$TREE/manifests" "$TREE/charts"
for b in k3s k3s-install.sh helm crane zot; do echo "#!/bin/sh" > "$TREE/bin/$b"; done
echo "cert-manager manifest" > "$TREE/manifests/cert-manager.yaml"
echo "servicemonitor crd"    > "$TREE/manifests/servicemonitor-crd.yaml"
echo "fake chart"            > "$TREE/charts/keycloak-operator-1.32.0.tgz"
echo "fake k3s images"       > "$TREE/k3s-airgap-images-${ARCH}.tar.zst"

# Fake zot image bundle (REUSED, not rebuilt) + its .sha256 sidecar. This is the
# gardenio-bundle-<cv>.oci.tar.zst crane OCI-layout bundle the tarball ships and
# --airgap/seed-bundle.sh consumes (Q3=zot); assemble stages it verbatim into
# images/ by basename, so this fixture also locks that the shipped filename is the
# one TARBOAT-SETUP.md §3/§4 + the validator staging gate assert (ka-0000004567).
ARCHIVE="$TMP/gardenio-bundle-${VERSION}.oci.tar.zst"
echo "fake oci image bundle" > "$ARCHIVE"
( cd "$TMP" && sha256sum "$(basename "$ARCHIVE")" > "$(basename "$ARCHIVE").sha256" )

# Fake box-config seed (M3.3, ka-0000003598) + its .sha256 + the reviewable
# sidecars, so this locks that assemble stages seed/ under --seed-archive and
# carries the manifest/report alongside (the Q4 "reviewable, not opaque" mitigation).
SEED="$TMP/seedsrc/gardenio-seed-${VERSION}.jsonl"
mkdir -p "$TMP/seedsrc"
printf '{"seed_kind":"advice","id":"gd-x","type":"advice","title":"t","body":"b","labels":[]}\n' > "$SEED"
( cd "$TMP/seedsrc" && sha256sum "$(basename "$SEED")" > "$(basename "$SEED").sha256" )
echo "id	family	selector	disposition	status" > "$TMP/seedsrc/seed-manifest.tsv"
echo "# report" > "$TMP/seedsrc/seed-redaction-report.md"

# ── Run the assembler offline ────────────────────────────────────────────────
OUT="$TMP/out"
if TARBOAT_FETCH=0 bash "$ASSEMBLER" \
      --version "$VERSION" --arch "$ARCH" \
      --repo-root "$ROOT" --image-archive "$ARCHIVE" \
      --seed-archive "$SEED" \
      --staging "$STAGING" --out-dir "$OUT" \
      --commit deadbeef --run-id testrun >"$TMP/log" 2>&1; then
  pass "assembler runs to completion (TARBOAT_FETCH=0)"
else
  fail "assembler run" "$(tail -20 "$TMP/log")"
  echo "PASS=$PASS FAIL=$FAIL"; exit 1
fi

t() { # t <desc> <test-expr...>
  local desc="$1"; shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}

# ── bin/ names (the exact filenames bootstrap-ec2.sh + install-registry.sh expect)
for b in k3s k3s-install.sh helm crane zot; do
  t "bin/$b present" test -s "$TREE/bin/$b"
done
# ── manifests/ names
t "manifests/cert-manager.yaml present"      test -s "$TREE/manifests/cert-manager.yaml"
t "manifests/servicemonitor-crd.yaml present" test -s "$TREE/manifests/servicemonitor-crd.yaml"
# ── charts/ name uses the DERIVED keycloak-operator version
t "charts/keycloak-operator-1.32.0.tgz (derived ver) present" \
  test -s "$TREE/charts/keycloak-operator-1.32.0.tgz"
# ── images/ reuses the archive + sidecar (NOT rebuilt)
t "images/<archive> reused" test -s "$TREE/images/$(basename "$ARCHIVE")"
t "images/<archive>.sha256 present" test -s "$TREE/images/$(basename "$ARCHIVE").sha256"
# ── seed/ box-config seed staged (M3.3) + reviewable sidecars carried
t "seed/<seed>.jsonl staged" test -s "$TREE/seed/$(basename "$SEED")"
t "seed/<seed>.jsonl.sha256 present" test -s "$TREE/seed/$(basename "$SEED").sha256"
t "seed/seed-manifest.tsv carried" test -s "$TREE/seed/seed-manifest.tsv"
t "seed/seed-redaction-report.md carried" test -s "$TREE/seed/seed-redaction-report.md"
# ── repo/ subset: detect_repo_dir sentinel + chart present, no .git
t "repo/scripts/bootstrap-ec2.sh present" test -f "$TREE/repo/scripts/bootstrap-ec2.sh"
t "repo/scripts/deploy-byo.sh sentinel present" test -f "$TREE/repo/scripts/deploy-byo.sh"
t "repo/gardenio/helm/gardenio/Chart.yaml present" test -f "$TREE/repo/gardenio/helm/gardenio/Chart.yaml"
t "repo/deploy-values staged" test -f "$TREE/repo/deploy-values/gardenio-byo.yaml"
t "repo/ carries NO .git" test ! -e "$TREE/repo/.git"
# ── LICENSES / NOTICE incl. MinIO AGPL-v3
t "LICENSES/NOTICE present" test -s "$TREE/LICENSES/NOTICE"
if grep -qi "MinIO" "$TREE/LICENSES/NOTICE" && grep -qi "AGPL" "$TREE/LICENSES/NOTICE"; then
  pass "NOTICE mentions MinIO AGPL-v3"
else
  fail "NOTICE MinIO/AGPL" "missing"
fi
# ── MANIFEST + SHA256SUMS + README + SETUP-PROMPT stub
t "MANIFEST present" test -s "$TREE/MANIFEST"
t "SHA256SUMS present" test -s "$TREE/SHA256SUMS"
t "README.md present" test -s "$TREE/README.md"
t "SETUP-PROMPT.txt stub present" test -s "$TREE/SETUP-PROMPT.txt"
# MANIFEST records provenance.
if grep -q "commit:    deadbeef" "$TREE/MANIFEST" && grep -q "calver:    ${VERSION}" "$TREE/MANIFEST"; then
  pass "MANIFEST carries provenance (calver+commit)"
else
  fail "MANIFEST provenance" "$(grep -E 'calver|commit' "$TREE/MANIFEST" || true)"
fi

# ── SHA256SUMS verifies against the staged tree
if ( cd "$TREE" && sha256sum -c SHA256SUMS >/dev/null 2>&1 ); then
  pass "SHA256SUMS verifies (sha256sum -c)"
else
  fail "SHA256SUMS -c" "checksum mismatch"
fi

# ── the single artifact + sidecar exist and the sidecar verifies
ART="$OUT/gardenio-tarboat-${VERSION}.tar.zst"
t "tar.zst artifact present" test -s "$ART"
t "tar.zst .sha256 sidecar present" test -s "${ART}.sha256"
if ( cd "$OUT" && sha256sum -c "gardenio-tarboat-${VERSION}.tar.zst.sha256" >/dev/null 2>&1 ); then
  pass "artifact .sha256 sidecar verifies"
else
  fail "artifact sidecar -c" "mismatch"
fi

# ── the artifact round-trips: untar and confirm the layout survives
RT="$TMP/roundtrip"; mkdir -p "$RT"
if zstd -dc --long=27 "$ART" | tar -C "$RT" -xf - 2>/dev/null \
   && test -s "$RT/gardenio-tarboat-${VERSION}/bin/k3s" \
   && test -f "$RT/gardenio-tarboat-${VERSION}/repo/scripts/deploy-byo.sh"; then
  pass "artifact round-trips (untar → layout intact)"
else
  fail "artifact round-trip" "untar/layout check failed"
fi

# ── --airgap-strict consumer contract: every staged asset bootstrap looks up
#    must resolve under the unpacked tree (this is what makes true air-gap real)
UNP="$RT/gardenio-tarboat-${VERSION}"
missing=0
for rel in bin/k3s bin/k3s-install.sh bin/helm bin/crane bin/zot \
           manifests/cert-manager.yaml manifests/servicemonitor-crd.yaml \
           charts/keycloak-operator-1.32.0.tgz; do
  [ -s "$UNP/$rel" ] || { echo "  missing $rel"; missing=1; }
done
if [ "$missing" = 0 ]; then
  pass "every --offline-assets lookup path resolves in the unpacked tree"
else
  fail "offline-assets lookup completeness" "see above"
fi

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
