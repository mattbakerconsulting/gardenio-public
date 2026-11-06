#!/usr/bin/env bash
# pack-gardenio-chart.sh
#
# Package a gardenio Helm chart with documentation COMMENTS STRIPPED from the
# payloads Helm actually embeds in the release Secret, so the packaged .tgz —
# and therefore the Helm release-history Secret it is embedded into — stays
# comfortably under the Kubernetes 1 MiB Secret hard limit.
#
# WHY THIS EXISTS (ka-0000007585, recurrence of ka-0000002370 / ka-0000005786):
# prod deploy-prod is a `terraform apply -target=module.gardenio` whose helm
# provider stores the whole rendered release (chart + manifest, gzipped) in a
# single Secret `sh.helm.release.v1.gardenio.vN`. A K8s Secret may not exceed
# 1,048,576 bytes. The gardenio chart chronically creeps back to that ceiling —
# .helmignore (ka-0000005786) and dashboard-JSON minification (ka-0000002370)
# each bought temporary headroom, then the chart regrew. On 2026-08-12 release
# 2026.224.11 tipped v518 over the limit and deploy-prod failed
# (release string was already only ~1.8 KiB under the limit at v517).
#
# WHAT ACTUALLY DOMINATES THE SECRET (measured by decomposing the LIVE prod v517
# Secret — ka-0000007585): the Helm release JSON stores the chart as a STRUCTURED
# object, not the raw source tree. `chart.values` is a PARSED MAP (~84 KiB, no
# comments — Helm discards them when it loads the chart), so stripping
# values.yaml comments shrinks the .tgz but does NOTHING to the release Secret.
# (That was the flaw in the first fix attempt: release 2026.224.12 still failed
# at the identical v518 "Too long" error.) The real bulk is `chart.templates`:
# 2.26 MiB raw / 606 KiB gzipped, of which ~154 KiB gzipped is Go-template
# comments. Removing those (render-identical) recovers ~166 KiB of gzip headroom
# under the ceiling — ~14x the pre-fix margin.
#
# THE TRANSFORM (all PACKAGE-TIME, against a COPY — committed sources keep their
# full inline docs for maintainers):
#   1. Strip Go-template comments ({{/* .. */}} incl. trim variants) from every
#      templates/ file via scripts/strip-helm-template-comments.py. This exactly
#      reproduces the Go template engine's trim-marker semantics, so the RENDER
#      is byte-identical. THIS is the lever that actually shrinks the Secret.
#   1b. Strip comment-only YAML lines (`# ...`) from templates/**.yaml via
#      scripts/strip-helm-yaml-comments.py (gd-qiUiXvTX0k: 4,600 such lines,
#      450 KB raw, and Helm renders them into the stored manifest too, so they
#      were counted twice in the release Secret). Block-scalar bodies (a shell
#      script's `#` lines), `{{ define }}` bodies and lines carrying a template
#      action are kept. The render is no longer byte-identical — the comment
#      lines leave the manifest — but it is identical as YAML, which is what
#      the self-check below asserts.
#   2. Strip values.yaml comments via `yq '... comments=""'` — kept as
#      defense-in-depth (shrinks the .tgz + any tooling that re-reads it) and
#      block-scalar-safe (preserves `userData: |` / `config: |` bodies incl. the
#      `#!/bin/bash` gVisor shebang a naive `grep -v '^#'` would destroy).
#
# SELF-VERIFYING: after the strip, this script asserts `helm template` renders
# the SAME MANIFESTS before vs after — (a) as text with comment-only lines
# dropped from both sides, and (b) as parsed YAML documents (yq), which is the
# check that catches a `#` content line wrongly removed from a block scalar.
# Per-render random secrets and checksum/* annotations (a checksum over an
# included template moves when that template loses its comments) are masked
# on both sides. A transform that would corrupt any manifest FAILS packaging
# instead of shipping a broken chart. That gate is what makes the comment
# strips safe to run in the release pipeline unattended.
#
# Usage:
#   pack-gardenio-chart.sh <chart-dir> <dest-dir> [helm package args...]
#
# Prints the path of the produced .tgz on stdout (last line). Any extra args
# after <dest-dir> are passed through to `helm package` (e.g. --version X
# --app-version X), matching the call sites in gardenio/scripts/release-deploy.sh
# and gardenio/tests/e2e/scripts/lane.sh.
set -euo pipefail

# Resolve this script's own dir so the sibling stripper is found regardless of CWD.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CHART_DIR="${1:?usage: pack-gardenio-chart.sh <chart-dir> <dest-dir> [helm package args...]}"
DEST_DIR="${2:?usage: pack-gardenio-chart.sh <chart-dir> <dest-dir> [helm package args...]}"
shift 2

if ! command -v yq >/dev/null 2>&1; then
  echo "pack-gardenio-chart.sh: FATAL: yq (mikefarah) not on PATH" >&2
  exit 1
fi

name="$(basename "$CHART_DIR")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cp -R "$CHART_DIR" "$tmp/$name"

# (1) Strip Go-template comments from templates/ — the lever that actually
# shrinks the release Secret. Render-identical (reproduces Go's trim semantics).
python3 "$SCRIPT_DIR/strip-helm-template-comments.py" "$tmp/$name"

# (1b) Strip comment-only YAML lines from templates/**.yaml. Render-equivalent
# as YAML (block scalars, define bodies and action-carrying lines are kept).
python3 "$SCRIPT_DIR/strip-helm-yaml-comments.py" "$tmp/$name"

# (2) Strip comments from values.yaml (defense-in-depth). `... comments=""` walks
# every node and clears leading/head/line/foot comments while preserving values,
# key order, and block-scalar bodies verbatim.
if [ -f "$tmp/$name/values.yaml" ]; then
  yq -i '... comments=""' "$tmp/$name/values.yaml"
fi

# (3) SELF-VERIFY: the packaged chart must render the same manifests as the
# source chart, else a comment strip corrupted a manifest — fail packaging
# loudly rather than ship a broken chart into a release. Two comparisons:
#   (a) text, with comment-only lines dropped from both sides (the YAML strip
#       legitimately removes them from the render);
#   (b) parsed YAML (yq -o=json), which catches a `#` CONTENT line (inside a
#       block scalar) that (a) would hide.
# Masked on both sides: the per-render random secrets (client-secret,
# cookie-secret, generated passwords) and checksum/* annotations, which move
# when the template they hash loses its comment lines (forwardauth's
# checksum/error-template). The mask keeps the line valid YAML.
if command -v helm >/dev/null 2>&1; then
  _mask() {
    sed -E \
      -e 's/^([[:space:]]*)(cookie-secret|client-secret|clientSecret|cookieSecret|password|password_url):.*/\1\2: MASKED/' \
      -e 's/(checksum\/[A-Za-z0-9_.-]+):[[:space:]]*"?[0-9a-f]{64}"?/\1: MASKED/'
  }
  _nocomment() { grep -v -E '^[[:space:]]*#' || true; }
  _semantic() { yq ea -o=json '[.]' -; }
  if helm template "$CHART_DIR" >/dev/null 2>&1; then
    helm template "$CHART_DIR" 2>/dev/null | _mask > "$tmp/render-src.yaml"
    helm template "$tmp/$name" 2>/dev/null | _mask > "$tmp/render-pkg.yaml"
    if ! diff -q <(_nocomment < "$tmp/render-src.yaml") <(_nocomment < "$tmp/render-pkg.yaml") >/dev/null; then
      echo "pack-gardenio-chart.sh: FATAL: comment strip changed helm template render — refusing to package." >&2
      diff <(_nocomment < "$tmp/render-src.yaml") <(_nocomment < "$tmp/render-pkg.yaml") | head -40 >&2
      exit 1
    fi
    if ! _semantic < "$tmp/render-src.yaml" > "$tmp/render-src.json" 2>"$tmp/yq.err"; then
      echo "pack-gardenio-chart.sh: FATAL: source render is not parseable YAML (cannot self-verify): $(cat "$tmp/yq.err")" >&2
      exit 1
    fi
    if ! _semantic < "$tmp/render-pkg.yaml" > "$tmp/render-pkg.json" 2>"$tmp/yq.err"; then
      echo "pack-gardenio-chart.sh: FATAL: packaged render is not parseable YAML — a comment strip corrupted a manifest: $(cat "$tmp/yq.err")" >&2
      exit 1
    fi
    if ! diff -q "$tmp/render-src.json" "$tmp/render-pkg.json" >/dev/null; then
      echo "pack-gardenio-chart.sh: FATAL: comment strip changed the rendered objects (parsed YAML differs) — refusing to package." >&2
      diff "$tmp/render-src.json" "$tmp/render-pkg.json" | head -40 >&2
      exit 1
    fi
  else
    echo "pack-gardenio-chart.sh: WARNING: source chart does not render with default values; skipping render-identity self-check." >&2
  fi
fi

mkdir -p "$DEST_DIR"
# Send helm's own chatter to stderr so stdout's last line is the tgz path.
helm package "$tmp/$name" --destination "$DEST_DIR" "$@" >&2

# helm names the artifact <name>-<version>.tgz. The version is whatever helm
# stamped: a passed-through --version wins, else Chart.yaml's version. Resolve it
# the same way so the emitted path is deterministic (no ls/glob races).
version=""
# Scan the passed-through args for an explicit --version override.
prev=""
for a in "$@"; do
  if [ "$prev" = "--version" ]; then version="$a"; fi
  case "$a" in --version=*) version="${a#--version=}" ;; esac
  prev="$a"
done
if [ -z "$version" ]; then
  version="$(yq '.version' "$tmp/$name/Chart.yaml")"
fi
tgz="$DEST_DIR/${name}-${version}.tgz"
if [ ! -f "$tgz" ]; then
  echo "pack-gardenio-chart.sh: FATAL: expected $tgz not produced" >&2
  exit 1
fi
echo "$tgz"
