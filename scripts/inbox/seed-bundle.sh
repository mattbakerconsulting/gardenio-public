#!/usr/bin/env bash
# seed-bundle.sh — Gardenio in a Box: seed the in-box zot registry from the
# single crane OCI-layout image bundle.
#
# Phase 4 P4.1 (ka-0000003363), epic ka-0000007881. Source research:
# gardenio/docs/research/inbox-image-distribution.md (ka-0000005069).
#
# THE BUNDLE: one content-addressed tarball, `gardenio-bundle-${CALVER}.oci.tar.zst`
# (+ a .sha256), that the RWX-era release pipeline produced at release time (no
# workflow builds it today; it was deleted with RWX on 2026-09-16). It
# expands to images/<slug>/ (one OCI layout per image) + a manifest.tsv
# ("<slug>\t<original-ref>" per line) covering the union of:
#   - 13 first-party images (<registry>/gardenio/*)
#   - 13 mirror images (.../mirror/*)
#   - the refs no chart override catches: alpine/git, docker:dind (Go
#     constants), public.ecr.aws ubuntu, ghcr.io k8s-sidecar + litellm
#
# WHAT THIS DOES: decompress the bundle, then for each manifest.tsv row
# `crane push images/<slug> <REGISTRY>/<repo-path>` into the in-box zot at
# 127.0.0.1:5000 under the IDENTICAL repo path (the source host stripped from
# the ref). Because paths are preserved, the containerd mirror (P4.2) can
# rewrite any prod ref to the local registry with NO path rewrite — Helm/Go
# refs stay byte-identical to prod. (zot storage.dedupe=true reclaims the
# shared agent-base layers on the registry side.)
#
# Idempotent: crane push/copy is content-addressed; re-seeding is a no-op for
# blobs already present (and zot dedups shared layers).
#
# Usage:
#   sudo scripts/inbox/seed-bundle.sh [--bundle PATH] [--registry HOST:PORT]
#        [--work-dir DIR] [--no-verify-sha]
#
# Env / flags (flags win over env):
#   BUNDLE       path to gardenio-bundle-*.oci.tar.zst (default: newest under
#                /opt/gardenio/bundle/)
#   REGISTRY     target in-box registry host:port    (default: 127.0.0.1:5000)
#   WORK_DIR     scratch dir for the unpacked layout  (default: mktemp -d)
#   VERIFY_SHA   1=check the sibling .sha256          (default: 1)
set -euo pipefail

BUNDLE="${BUNDLE:-}"
REGISTRY="${REGISTRY:-127.0.0.1:5000}"
WORK_DIR="${WORK_DIR:-}"
VERIFY_SHA="${VERIFY_SHA:-1}"

while [ $# -gt 0 ]; do
  case "$1" in
    --bundle)         BUNDLE="$2"; shift 2;;
    --registry)       REGISTRY="$2"; shift 2;;
    --work-dir)       WORK_DIR="$2"; shift 2;;
    --no-verify-sha)  VERIFY_SHA=0; shift;;
    -h|--help)        sed -n '2,40p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[seed]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[seed] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[seed] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }
need crane
need zstd
need tar
need curl

# ── Resolve the bundle path ──────────────────────────────────────────────────
if [ -z "$BUNDLE" ]; then
  # newest bundle under the default land dir (find handles odd names; sort -V
  # then tail gives the highest calver).
  BUNDLE="$(find /opt/gardenio/bundle -maxdepth 1 -name 'gardenio-bundle-*.oci.tar.zst' 2>/dev/null \
              | sort -V | tail -1)"
  [ -n "$BUNDLE" ] || die "no bundle found under /opt/gardenio/bundle/ — pass --bundle PATH"
fi
[ -f "$BUNDLE" ] || die "bundle not found: $BUNDLE"
log "bundle: $BUNDLE"
log "target: $REGISTRY"

# ── Verify the sha256 sidecar (offline integrity) ────────────────────────────
if [ "$VERIFY_SHA" = "1" ] && [ -f "${BUNDLE}.sha256" ]; then
  log "verifying ${BUNDLE}.sha256"
  ( cd "$(dirname "$BUNDLE")" && sha256sum -c "$(basename "$BUNDLE").sha256" ) \
    || die "bundle sha256 mismatch — refusing to seed a corrupt bundle"
else
  [ "$VERIFY_SHA" = "1" ] && warn "no ${BUNDLE}.sha256 sidecar — skipping integrity check"
fi

# ── Wait for the registry to be reachable ────────────────────────────────────
log "checking registry ${REGISTRY} ..."
i=0
until curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -ge 30 ] && die "registry ${REGISTRY} not reachable — run install-registry.sh first"
  sleep 1
done

# ── Decompress to an OCI layout ──────────────────────────────────────────────
CLEAN_WORK=0
if [ -z "$WORK_DIR" ]; then
  WORK_DIR="$(mktemp -d)"
  CLEAN_WORK=1
fi
ROOT="$WORK_DIR/bundle"
install -d -m 0755 "$ROOT"
cleanup() { [ "$CLEAN_WORK" = "1" ] && rm -rf "$WORK_DIR"; }
trap cleanup EXIT

log "decompressing bundle → $ROOT"
# --long=31 to match the build-side window; -d auto-detects but be explicit.
zstd -dc --long=31 "$BUNDLE" | tar -C "$ROOT" -xf -
MANIFEST="$ROOT/manifest.tsv"
[ -f "$MANIFEST" ] || die "decompressed bundle has no manifest.tsv — is it a gardenio release bundle?"
[ -d "$ROOT/images" ] || die "decompressed bundle has no images/ dir"

# ── Push every per-image layout to the in-box registry ───────────────────────
# manifest.tsv carries one "<slug>\t<original-ref>" row per image. The ref is
# the ORIGINAL repo path + tag, e.g.
#   <registry>/gardenio/agent:2026.161.3
# Strip the source host so we land it under the IDENTICAL repo path locally —
# the containerd mirror (P4.2) then needs no path rewrite.
COUNT=0
FAILED=()
while IFS=$'\t' read -r slug ref; do
  if [ -z "$slug" ] || [ -z "$ref" ]; then continue; fi
  layout="$ROOT/images/$slug"
  [ -d "$layout" ] || { warn "  missing layout dir for $slug ($ref)"; FAILED+=("$ref"); continue; }
  repo_path="${ref#*/}"          # drop "<host>/"
  dst="${REGISTRY}/${repo_path}"
  COUNT=$((COUNT + 1))
  log "  [${slug}] ${ref} → ${dst}"
  if ! crane push --insecure "$layout" "$dst" 2>&1 | sed 's/^/    /'; then
    warn "  failed to seed ${ref}"
    FAILED+=("$ref")
  fi
done < "$MANIFEST"

if [ "${#FAILED[@]}" -gt 0 ]; then
  die "${#FAILED[@]} image(s) failed to seed: ${FAILED[*]}"
fi

# ── Verify a representative image resolves ───────────────────────────────────
log "post-seed verification: listing repositories"
curl -fsS "http://${REGISTRY}/v2/_catalog" 2>/dev/null | head -c 2000 || true
echo
log "done. ${COUNT} images seeded into ${REGISTRY}."
log "Next: redirect containerd pulls with scripts/inbox/configure-mirror.sh (P4.2)."
