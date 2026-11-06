#!/usr/bin/env bash
# assemble-tarboat.sh — Tarboat T1 / G1.3 (ka-0000000192): assemble the single
# signed `gardenio-tarboat-<calver>.tar.zst` that a friend untars on a fresh EC2
# and installs with NO public-internet fetch and NO private-repo clone.
#
# This is the artifact that POPULATES the `--offline-assets DIR` that sibling
# G1.1 (bootstrap-ec2.sh, MERGED) consumes. The bin/ filenames, manifests/
# names, and the charts/keycloak-operator-<ver>.tgz name MUST byte-match what
# bootstrap-ec2.sh + install-registry.sh look for — so the staged-asset version
# pins here are DERIVED from those scripts (single source of truth, zero drift),
# not re-declared.
#
# LAYOUT it produces (plan artifact ka-0000003281 §B; gap doc G1.3):
#   gardenio-tarboat-<calver>/
#     README.md                README-tarboat.md (friend-grade front page)
#     MANIFEST                 every file + sha256 + provenance
#     SHA256SUMS               top-level checksum (sha256sum -c verifiable)
#     LICENSES/                component + bundled-image licenses + NOTICE (MinIO AGPL-v3)
#     repo/                    monorepo-rooted subset (scripts/ deploy-values/ gardenio/helm/gardenio/)
#     bin/                     k3s k3s-install.sh helm crane zot  (pinned, in MANIFEST)
#     manifests/               cert-manager.yaml servicemonitor-crd.yaml
#     charts/                  keycloak-operator-<ver>.tgz
#     images/                  gardenio-bundle-<calver>.oci.tar.zst (+ .sha256, REUSED — not rebuilt; the ZOT crane OCI-layout bundle --airgap/seed-bundle.sh consumes)
#     seed/                    gardenio-seed-<calver>.jsonl (+ .sha256, manifest/report) — box-config seed, restored by seed-config-dump.sh (M3, ka-0000003598)
#     k3s-airgap-images-<arch>.tar.zst   k3s system images (k3s itself offline too)
#     SETUP-PROMPT.txt         stub for the T2 single prompt
#
# Then the whole tree is tar+zstd'd into one artifact + a .sha256 sidecar.
#
# Usage:
#   assemble-tarboat.sh --version <calver> --image-archive <path/to/...oci.tar.zst>
#       [--repo-root DIR] [--out-dir DIR] [--staging DIR] [--arch amd64|arm64]
#       [--commit SHA] [--run-id ID]
#
# Env knobs (defaults are the pins below):
#   K3S_VERSION    k3s release tag         (default: v1.31.5+k3s1 — LTS-ish, airgap assets exist)
#   HELM_VERSION   helm version            (default: matches images/agent/install/versions.env)
#   CRANE_VERSION  crane release tag       (default: v0.20.2, the RWX-era bundle pipeline's pin)
#   ZOT_VERSION    zot release tag         (default: derived from install-registry.sh)
#   TARBOAT_FETCH  1=download bin/manifests/chart, 0=require pre-seeded under --staging (tests/local cache)
#
# bash -n + shellcheck clean. Companion unit test: assemble-tarboat_test.sh.

set -euo pipefail

# ── locate ourselves + the repo root ────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT_DEFAULT="$(cd "$SCRIPT_DIR/../.." && pwd)"

log()  { printf '\033[1;36m[tarboat]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[tarboat] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[tarboat] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

# ── version pins ─────────────────────────────────────────────────────────────
# DERIVED from the install scripts so the staged filenames byte-match what the
# consumer looks for. derive_version <var> <file> <default> — echo the value of
# a `VAR="..."`/`VAR=v...` assignment in <file>, else the default.
derive_version() {
  local var="$1" file="$2" def="$3" v=""
  if [ -f "$file" ]; then
    v="$(sed -n "s/^[[:space:]]*${var}=\"\{0,1\}\([^\"#[:space:]]*\)\"\{0,1\}.*/\1/p" "$file" | head -1)"
    # install-registry.sh wraps it: ZOT_VERSION="${ZOT_VERSION:-v2.1.2}" — pull the fallback.
    case "$v" in
      *':-'*) v="${v##*:-}"; v="${v%%\}*}";;
    esac
  fi
  printf '%s\n' "${v:-$def}"
}

# ── defaults / arg parse ─────────────────────────────────────────────────────
VERSION=""
IMAGE_ARCHIVE=""
SEED_ARCHIVE="${TARBOAT_SEED_ARCHIVE:-}"
REPO_ROOT="$REPO_ROOT_DEFAULT"
OUT_DIR=""
STAGING=""
ARCH="amd64"
COMMIT="${TARBOAT_COMMIT:-unknown}"
RUN_ID="${TARBOAT_RUN_ID:-local}"
TARBOAT_FETCH="${TARBOAT_FETCH:-1}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2;;
    --image-archive) IMAGE_ARCHIVE="$2"; shift 2;;
    --seed-archive) SEED_ARCHIVE="$2"; shift 2;;
    --repo-root) REPO_ROOT="$2"; shift 2;;
    --out-dir) OUT_DIR="$2"; shift 2;;
    --staging) STAGING="$2"; shift 2;;
    --arch) ARCH="$2"; shift 2;;
    --commit) COMMIT="$2"; shift 2;;
    --run-id) RUN_ID="$2"; shift 2;;
    -h|--help) sed -n '2,46p' "$0"; exit 0;;
    *) die "unknown arg: $1 (try --help)";;
  esac
done

[ -n "$VERSION" ] || die "missing --version <calver>"
[ -d "$REPO_ROOT/scripts" ] || die "--repo-root $REPO_ROOT has no scripts/ — not a gardenio monorepo root"

BOOTSTRAP="$REPO_ROOT/scripts/bootstrap-ec2.sh"
INSTALL_REGISTRY="$REPO_ROOT/scripts/inbox/install-registry.sh"

# Operator-chart/manifest versions: DERIVE from bootstrap so the staged names
# (charts/keycloak-operator-<ver>.tgz, manifests/*) match the consumer exactly.
OP_CERT_MANAGER_VERSION="$(derive_version OP_CERT_MANAGER_VERSION "$BOOTSTRAP" v1.16.2)"
OP_SERVICEMONITOR_CRD_VERSION="$(derive_version OP_SERVICEMONITOR_CRD_VERSION "$BOOTSTRAP" v0.76.0)"
OP_KEYCLOAK_OPERATOR_VERSION="$(derive_version OP_KEYCLOAK_OPERATOR_VERSION "$BOOTSTRAP" 1.32.0)"
OP_KEYCLOAK_OPERATOR_REPO="$(derive_version OP_KEYCLOAK_OPERATOR_REPO "$BOOTSTRAP" https://epam.github.io/edp-helm-charts/stable)"

# Binary pins (env-overridable). zot derived from install-registry.sh.
K3S_VERSION="${K3S_VERSION:-v1.31.5+k3s1}"
HELM_VERSION="${HELM_VERSION:-3.20.2}"
CRANE_VERSION="${CRANE_VERSION:-v0.20.2}"
ZOT_VERSION="${ZOT_VERSION:-$(derive_version ZOT_VERSION "$INSTALL_REGISTRY" v2.1.2)}"

case "$ARCH" in
  # k3s release assets: `k3s` (amd64), `k3s-arm64` (arm64).
  amd64) GOARCH=amd64; CRANE_ARCH="Linux_x86_64"; K3S_BIN_ASSET="k3s";;
  arm64) GOARCH=arm64; CRANE_ARCH="Linux_arm64"; K3S_BIN_ASSET="k3s-arm64";;
  *) die "unsupported --arch $ARCH (amd64|arm64)";;
esac

# ── working dirs ─────────────────────────────────────────────────────────────
NAME="gardenio-tarboat-${VERSION}"
WORK="${STAGING:-$(mktemp -d)}"
TREE="$WORK/$NAME"
OUT_DIR="${OUT_DIR:-$WORK/out}"
mkdir -p "$TREE/bin" "$TREE/manifests" "$TREE/charts" "$TREE/images" \
         "$TREE/LICENSES" "$TREE/repo" "$OUT_DIR"

log "assembling $NAME (arch=$ARCH commit=$COMMIT run=$RUN_ID)"
log "  pins: k3s=$K3S_VERSION helm=$HELM_VERSION crane=$CRANE_VERSION zot=$ZOT_VERSION"
log "        cert-manager=$OP_CERT_MANAGER_VERSION servicemonitor=$OP_SERVICEMONITOR_CRD_VERSION keycloak-operator=$OP_KEYCLOAK_OPERATOR_VERSION"

# fetch <url> <dest> — download iff TARBOAT_FETCH=1; else require a pre-seeded
# file at <dest> (lets unit tests + a local binary cache populate bin/ offline).
fetch() {
  local url="$1" dest="$2"
  if [ "$TARBOAT_FETCH" = "1" ]; then
    log "  fetch $url"
    curl -fsSL --retry 3 --retry-delay 2 "$url" -o "$dest" || die "download failed: $url"
  else
    [ -s "$dest" ] || die "TARBOAT_FETCH=0 but required asset not pre-seeded: $dest"
    log "  using pre-seeded $dest (no fetch)"
  fi
}

# ── 1. bin/ — k3s, k3s-install.sh, helm, crane, zot (the names G1.1 expects) ─
K3S_TAG_ENC="$(printf '%s' "$K3S_VERSION" | sed 's/+/%2B/')"
fetch "https://github.com/k3s-io/k3s/releases/download/${K3S_TAG_ENC}/${K3S_BIN_ASSET}" "$TREE/bin/k3s"
# k3s-install.sh is the static get.k3s.io installer; with INSTALL_K3S_SKIP_DOWNLOAD
# + the staged bin/k3s it runs fully offline (the official k3s air-gap procedure).
fetch "https://get.k3s.io" "$TREE/bin/k3s-install.sh"

# helm: extract the single binary from the official tarball.
if [ "$TARBOAT_FETCH" = "1" ]; then
  log "  fetch helm v${HELM_VERSION}"
  _htar="$WORK/helm.tgz"
  curl -fsSL --retry 3 --retry-delay 2 \
    "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${GOARCH}.tar.gz" -o "$_htar" \
    || die "helm download failed"
  tar -xzf "$_htar" -C "$WORK" "linux-${GOARCH}/helm"
  install -m 0755 "$WORK/linux-${GOARCH}/helm" "$TREE/bin/helm"
else
  [ -s "$TREE/bin/helm" ] || die "TARBOAT_FETCH=0 but bin/helm not pre-seeded"
fi

# crane: extract from the go-containerregistry tarball.
if [ "$TARBOAT_FETCH" = "1" ]; then
  log "  fetch crane ${CRANE_VERSION}"
  _ctar="$WORK/crane.tgz"
  curl -fsSL --retry 3 --retry-delay 2 \
    "https://github.com/google/go-containerregistry/releases/download/${CRANE_VERSION}/go-containerregistry_${CRANE_ARCH}.tar.gz" \
    -o "$_ctar" || die "crane download failed"
  tar -xzf "$_ctar" -C "$WORK" crane
  install -m 0755 "$WORK/crane" "$TREE/bin/crane"
else
  [ -s "$TREE/bin/crane" ] || die "TARBOAT_FETCH=0 but bin/crane not pre-seeded"
fi

# zot: the minimal static binary install-registry.sh consumes.
fetch "https://github.com/project-zot/zot/releases/download/${ZOT_VERSION}/zot-linux-${GOARCH}-minimal" "$TREE/bin/zot"
chmod 0755 "$TREE/bin"/* 2>/dev/null || true

# ── 2. manifests/ — cert-manager + ServiceMonitor CRD at the pinned versions ─
fetch "https://github.com/cert-manager/cert-manager/releases/download/${OP_CERT_MANAGER_VERSION}/cert-manager.yaml" \
  "$TREE/manifests/cert-manager.yaml"
fetch "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${OP_SERVICEMONITOR_CRD_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml" \
  "$TREE/manifests/servicemonitor-crd.yaml"

# ── 3. charts/ — keycloak-operator-<ver>.tgz (helm pull; local path = no repo add) ─
KO_TGZ="$TREE/charts/keycloak-operator-${OP_KEYCLOAK_OPERATOR_VERSION}.tgz"
if [ "$TARBOAT_FETCH" = "1" ]; then
  command -v helm >/dev/null 2>&1 || die "helm not on PATH (needed to pull the keycloak-operator chart)"
  log "  helm pull keycloak-operator ${OP_KEYCLOAK_OPERATOR_VERSION}"
  helm repo add epamedp "$OP_KEYCLOAK_OPERATOR_REPO" >/dev/null 2>&1 || true
  helm repo update epamedp >/dev/null 2>&1 || helm repo update >/dev/null 2>&1 || true
  helm pull epamedp/keycloak-operator --version "$OP_KEYCLOAK_OPERATOR_VERSION" \
    --destination "$TREE/charts" || die "helm pull keycloak-operator failed"
  # helm pull names it keycloak-operator-<ver>.tgz already; assert the contract.
  [ -s "$KO_TGZ" ] || die "expected $KO_TGZ after helm pull (got: $(ls "$TREE/charts" 2>/dev/null))"
else
  [ -s "$KO_TGZ" ] || die "TARBOAT_FETCH=0 but $KO_TGZ not pre-seeded"
fi

# ── 4. k3s system images (so k3s itself is offline too) ──────────────────────
fetch "https://github.com/k3s-io/k3s/releases/download/${K3S_TAG_ENC}/k3s-airgap-images-${GOARCH}.tar.zst" \
  "$TREE/k3s-airgap-images-${GOARCH}.tar.zst"

# ── 5. images/ — REUSE the existing OCI image-archive (do NOT rebuild) ───────
[ -n "$IMAGE_ARCHIVE" ] || die "missing --image-archive (the release bundle output — we do NOT rebuild images)"
[ -s "$IMAGE_ARCHIVE" ] || die "--image-archive not found/empty: $IMAGE_ARCHIVE"
log "  staging image-archive $(basename "$IMAGE_ARCHIVE") (reused, not rebuilt)"
cp "$IMAGE_ARCHIVE" "$TREE/images/"
if [ -s "${IMAGE_ARCHIVE}.sha256" ]; then
  cp "${IMAGE_ARCHIVE}.sha256" "$TREE/images/"
else
  ( cd "$TREE/images" && sha256sum "$(basename "$IMAGE_ARCHIVE")" > "$(basename "$IMAGE_ARCHIVE").sha256" )
fi

# ── 5b. seed/ — box-config seed (M3.3, ka-0000003598) ────────────────────────
# The pre-baked, already-redacted box-config seed produced at release time by
# scripts/inbox/build-seed-config.sh (extract → redact → guard → manifest). It
# rides in the tarball under tarboat/seed/; on first boot seed-config-dump.sh
# restores it into the box's fresh beads DB. Optional: a tarball built without a
# seed simply boots with no seeded advice (non-strict), so --seed-archive is not
# required — but a strict release SHOULD pass it. We reuse (never rebuild) the
# archive + its .sha256 + the reviewable manifest/report if they sit alongside it.
install -d -m 0755 "$TREE/seed"
if [ -n "$SEED_ARCHIVE" ]; then
  [ -s "$SEED_ARCHIVE" ] || die "--seed-archive not found/empty: $SEED_ARCHIVE"
  log "  staging box-config seed $(basename "$SEED_ARCHIVE")"
  cp "$SEED_ARCHIVE" "$TREE/seed/"
  if [ -s "${SEED_ARCHIVE}.sha256" ]; then
    cp "${SEED_ARCHIVE}.sha256" "$TREE/seed/"
  else
    ( cd "$TREE/seed" && sha256sum "$(basename "$SEED_ARCHIVE")" > "$(basename "$SEED_ARCHIVE").sha256" )
  fi
  # Carry the reviewable sidecars (manifest + redaction report) if present, so a
  # human can audit WHAT shipped without cracking the seed (Q4 mitigation).
  seed_src_dir="$(dirname "$SEED_ARCHIVE")"
  for sidecar in seed-manifest.tsv seed-redaction-report.md; do
    [ -s "$seed_src_dir/$sidecar" ] && cp "$seed_src_dir/$sidecar" "$TREE/seed/"
  done
else
  warn "no --seed-archive — tarball ships with NO box-config seed (box boots with empty advice unless --airgap-strict is off)"
fi

# ── 6. repo/ — monorepo-rooted subset (G1.2 consumes this at REPO_DIR) ───────
# bootstrap-ec2.sh detect_repo_dir keys on $REPO_DIR/scripts/deploy-byo.sh, so
# the tree must be monorepo-rooted: repo/scripts/, repo/deploy-values/,
# repo/gardenio/helm/gardenio/. coop/ (Rust), kbeads/, controller/ source are NOT
# needed on the box (their images are in the OCI archive). Coordinated with the
# G1.2 crew (ka-0000008214).
stage_repo_subset() {
  local root="$1" dest="$2"
  # scripts/ — bootstrap, deploy-byo, k3s/, inbox/, gvisor/, cloud-config, etc.
  cp -a "$root/scripts" "$dest/scripts"
  # deploy-values/ — the values overlays the install path layers.
  cp -a "$root/deploy-values" "$dest/deploy-values"
  # The Helm chart only (not the whole gardenio/ Go tree).
  mkdir -p "$dest/gardenio/helm"
  cp -a "$root/gardenio/helm/gardenio" "$dest/gardenio/helm/gardenio"
  # The in-box install docs the path references.
  mkdir -p "$dest/gardenio/docs"
  for d in gardenio-in-a-box-install.md gardenio-in-a-box-synthesis.md \
           gardenio-in-a-box-restore.md tarboat-gap-analysis.md; do
    if [ -f "$root/gardenio/docs/$d" ]; then cp -a "$root/gardenio/docs/$d" "$dest/gardenio/docs/$d"; fi
  done
  # Drop any VCS noise.
  find "$dest" -name '.git' -prune -exec rm -rf {} + 2>/dev/null || true
}
log "  staging repo/ subset (scripts/ deploy-values/ gardenio/helm/gardenio/ + install docs)"
stage_repo_subset "$REPO_ROOT" "$TREE/repo"
[ -f "$TREE/repo/scripts/bootstrap-ec2.sh" ] || die "repo/ subset missing scripts/bootstrap-ec2.sh"
[ -f "$TREE/repo/scripts/deploy-byo.sh" ]    || die "repo/ subset missing scripts/deploy-byo.sh (detect_repo_dir sentinel)"

# ── 7. LICENSES/ + NOTICE (G3.3, Q4=ok: carry MinIO AGPL-v3) ─────────────────
write_licenses() {
  local dest="$1"
  # The gardenio repo LICENSE.
  if [ -f "$REPO_ROOT/LICENSE" ]; then cp -a "$REPO_ROOT/LICENSE" "$dest/LICENSE-gardenio"; fi
  cat > "$dest/NOTICE" <<NOTICE
gardenio-tarboat ${VERSION} — third-party NOTICE
================================================

This redistributable tarball bundles container images and binaries that carry
their own licenses. The full list lives in MANIFEST (component versions) and the
per-image OCI archive under images/. Notable obligations:

  MinIO (minio/minio, minio/mc) — GNU AGPL-v3.0.
    The in-box object store ships MinIO under the GNU Affero General Public
    License v3.0 (operator decision ka-0000004293 / Q4 ka-0000002491: OK for
    now; SeaweedFS is the documented fallback). The AGPL requires that the
    complete corresponding source for MinIO be made available to anyone who
    interacts with it over a network. MinIO source: https://github.com/minio/minio
    AGPL-v3 text: LICENSES/AGPL-3.0.txt

  k3s            — Apache-2.0 (https://github.com/k3s-io/k3s)
  helm           — Apache-2.0 (https://github.com/helm/helm)
  crane          — Apache-2.0 (https://github.com/google/go-containerregistry)
  zot            — Apache-2.0 (https://github.com/project-zot/zot)
  cert-manager   — Apache-2.0 (https://github.com/cert-manager/cert-manager)
  keycloak-operator (EDP) — Apache-2.0 (https://github.com/epam/edp-keycloak-operator)

Every other bundled image retains its upstream license. See each image's
metadata in the OCI archive (images/) and MANIFEST for exact versions.

This NOTICE is informational; it does not modify any bundled license.
NOTICE
  # Carry the full AGPL-3.0 text (the obligation Q4 explicitly calls out).
  if [ "$TARBOAT_FETCH" = "1" ]; then
    curl -fsSL --retry 3 --retry-delay 2 \
      "https://www.gnu.org/licenses/agpl-3.0.txt" -o "$dest/AGPL-3.0.txt" \
      || warn "could not fetch AGPL-3.0.txt (NOTICE still references it)"
  fi
  [ -s "$dest/AGPL-3.0.txt" ] || printf '%s\n' \
    "GNU AFFERO GENERAL PUBLIC LICENSE Version 3 — full text at https://www.gnu.org/licenses/agpl-3.0.txt" \
    > "$dest/AGPL-3.0.txt"
}
log "  assembling LICENSES/ + NOTICE (incl. MinIO AGPL-v3)"
write_licenses "$TREE/LICENSES"

# ── 8. README + SETUP-PROMPT stub ────────────────────────────────────────────
if [ -f "$REPO_ROOT/gardenio/docs/tarboat-README.md" ]; then
  cp -a "$REPO_ROOT/gardenio/docs/tarboat-README.md" "$TREE/README.md"
else
  cat > "$TREE/README.md" <<README
# gardenio-tarboat ${VERSION}

A self-contained, air-gapped Gardenio distribution. Untar on a fresh single EC2
instance and install with NO public-internet fetch and NO private-repo clone.

What's inside:
  bin/         k3s, k3s-install.sh, helm, crane, zot (pinned)
  manifests/   cert-manager + ServiceMonitor CRD
  charts/      keycloak-operator
  images/      every runtime container image (OCI archive)
  seed/        box-config seed (redacted advice) + reviewable manifest/report
  k3s-airgap-images-*.tar.zst   k3s system images
  repo/        the gardenio tree (scripts + chart + values) — no clone needed
  LICENSES/    component + bundled-image licenses, incl. MinIO AGPL-v3 NOTICE
  MANIFEST     every file + sha256 + provenance (calver, commit, build run)
  SHA256SUMS   verify with: sha256sum -c SHA256SUMS

Verify, then install (true air-gap):
  sha256sum -c SHA256SUMS
  sudo repo/scripts/bootstrap-ec2.sh --scenario A --harden \\
    --airgap --airgap-strict --offline-assets "\$PWD" \\
    --bundle-dir "\$PWD/images" --repo-dir "\$PWD/repo"

The single on-box-agent setup prompt lands in SETUP-PROMPT.txt (Tarboat T2).

Provenance: calver ${VERSION}, commit ${COMMIT}, build run ${RUN_ID}.
Operator: Matt Baker.
README
fi

cat > "$TREE/SETUP-PROMPT.txt" <<'STUB'
# SETUP-PROMPT.txt — placeholder (Tarboat T2 fills this in).
#
# This file will carry the single prompt a raw on-box Claude Code instance is
# given to drive the entire air-gapped Gardenio setup from this tarball, with no
# human intervention. T2 (G2.2/G2.5) authors it against TARBOAT-SETUP.md and the
# agent-executable validation harness (G2.3). Until then, follow README.md.
STUB

# ── 9. MANIFEST — every file + sha256 + provenance ───────────────────────────
log "  writing MANIFEST + SHA256SUMS"
{
  echo "# gardenio-tarboat MANIFEST"
  echo "# calver:    ${VERSION}"
  echo "# commit:    ${COMMIT}"
  echo "# build-run: ${RUN_ID}"
  echo "# arch:      ${ARCH}"
  echo "# pins:      k3s=${K3S_VERSION} helm=${HELM_VERSION} crane=${CRANE_VERSION} zot=${ZOT_VERSION}"
  echo "#            cert-manager=${OP_CERT_MANAGER_VERSION} servicemonitor=${OP_SERVICEMONITOR_CRD_VERSION} keycloak-operator=${OP_KEYCLOAK_OPERATOR_VERSION}"
  echo "# format:    <sha256>  <relative-path>"
  ( cd "$TREE" && find . -type f ! -name MANIFEST ! -name SHA256SUMS -print0 \
      | sort -z | xargs -0 sha256sum | sed 's#  \./#  #' )
} > "$TREE/MANIFEST"

# SHA256SUMS — the top-level integrity floor (sha256sum -c verifiable). Covers
# every file EXCEPT itself (and MANIFEST, which embeds the same hashes).
( cd "$TREE" && find . -type f ! -name SHA256SUMS -print0 \
    | sort -z | xargs -0 sha256sum | sed 's#  \./#  #' ) > "$TREE/SHA256SUMS"

# ── 10. tar + zstd → one artifact + .sha256 sidecar ──────────────────────────
OUT="$OUT_DIR/${NAME}.tar.zst"
log "  compressing → $OUT"
# --long=27 (128 MB window): the image-archive is already blob-deduped, so a 2 GB
# window buys nothing and would exceed common decoder defaults. -19 -T0 = strong + parallel.
tar -C "$WORK" -cf - "$NAME" | zstd -19 -T0 --long=27 -o "$OUT" -f
( cd "$OUT_DIR" && sha256sum "${NAME}.tar.zst" > "${NAME}.tar.zst.sha256" )

# Signing: a sha256 sidecar is the integrity floor. If a detached-signature key
# is available (TARBOAT_SIGNING_KEY = an ASCII-armored private key, or a gpg
# secret already in the keyring via TARBOAT_GPG_KEYID), emit a .asc too.
if [ -n "${TARBOAT_GPG_KEYID:-}" ] && command -v gpg >/dev/null 2>&1; then
  log "  signing with gpg key ${TARBOAT_GPG_KEYID}"
  if gpg --batch --yes --local-user "$TARBOAT_GPG_KEYID" \
       --output "${OUT}.asc" --detach-sign --armor "$OUT"; then
    log "  detached signature: ${OUT}.asc"
  else
    warn "gpg signing failed — sha256 remains the integrity floor"
  fi
else
  log "  no signing key (TARBOAT_GPG_KEYID unset) — sha256 + MANIFEST + SHA256SUMS are the integrity floor"
fi

log "=== tarboat ready ==="
ls -lh "$OUT_DIR"
cat "$OUT_DIR/${NAME}.tar.zst.sha256"
