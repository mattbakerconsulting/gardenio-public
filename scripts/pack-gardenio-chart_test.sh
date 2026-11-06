#!/usr/bin/env bash
# pack-gardenio-chart_test.sh — Unit tests for pack-gardenio-chart.sh
#
# Builds a tiny synthetic chart in a temp dir, packages it through the script,
# and asserts: (1) the packaged values.yaml has its documentation comments
# stripped, (2) block-scalar bodies (incl. literal #-leading lines like a
# shebang) survive verbatim, (3) values/keys are preserved (semantic identity),
# (4) an explicit --version override lands in the packaged Chart.yaml and the
# emitted path, (5) the committed SOURCE chart is never mutated.
#
# Requires yq (mikefarah) + helm on PATH — CI installs both. If either is
# missing the test SKIPS with exit 0 (dev convenience); the real callers
# (release-deploy.sh, the e2e lane) need both, so a missing tool cannot ship silently.
#
# Usage:  bash scripts/pack-gardenio-chart_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/pack-gardenio-chart.sh"

if ! command -v yq >/dev/null 2>&1 || ! command -v helm >/dev/null 2>&1; then
  echo "SKIP: yq and/or helm not installed; CI installs both before packaging"
  exit 0
fi

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── Build a minimal synthetic chart with a documented values.yaml ──────────
CHART="${TMPDIR_T}/mychart"
mkdir -p "$CHART/templates"
cat >"$CHART/Chart.yaml" <<'EOF'
apiVersion: v2
name: mychart
version: 1.2.3
appVersion: 1.2.3
EOF
cat >"$CHART/values.yaml" <<'EOF'
# Top-of-file documentation comment that MUST be stripped from the package.
replicas: 3   # inline doc comment, also stripped
image:
  # nested doc comment
  repo: example/app
  tag: v9
node:
  # This block scalar contains literal #-leading lines that MUST survive.
  userData: |
    #!/bin/bash
    # a real shell comment inside the script body
    echo hello
EOF
# cm.yaml carries Go-template comments (incl. trim variants) that MUST be
# stripped from the package but leave the RENDER byte-identical. The `userData`
# block scalar reproduces a template that emits a literal #-leading shebang — a
# `#` inside RENDERED output, not a template comment, so it must survive.
# The YAML comment lines ("yaml doc comment") are stripped too (gd-qiUiXvTX0k)
# — from the package AND, since Helm renders them, from the manifest — while
# the `#` lines inside the script.sh block scalar are content and must survive
# in both.
cat >"$CHART/templates/cm.yaml" <<'EOF'
{{/* leading template comment — stripped, no whitespace effect */}}
# yaml doc comment at the top: stripped
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
data:
  {{- /* trim-left comment: eats preceding blank line, render-neutral here */}}
  # yaml doc comment inside data: stripped
  replicas: {{ .Values.replicas | quote }}
  repo: {{ .Values.image.repo }}
  userData: {{ .Values.node.userData | quote }}
  script.sh: |
    #!/bin/sh
    # shell comment inside a block scalar: content, kept
    echo hi
EOF

# A helper .tpl with template comments (also stripped; no rendered output).
cat >"$CHART/templates/_helpers.tpl" <<'EOF'
{{/*
Multi-line
template comment block.
*/}}
{{- define "mychart.name" -}}mychart{{- end -}}
EOF

# Snapshot the source values.yaml + a template to prove they are not mutated.
SRC_HASH_BEFORE="$(sha256sum "$CHART/values.yaml" | awk '{print $1}')"
TPL_HASH_BEFORE="$(sha256sum "$CHART/templates/cm.yaml" | awk '{print $1}')"

# ── 1. Package (default version) ───────────────────────────────────────────
DEST1="${TMPDIR_T}/dest1"
TGZ1="$(bash "$SCRIPT_UNDER_TEST" "$CHART" "$DEST1" 2>/dev/null | tail -1)"
if [ -f "$TGZ1" ] && [ "$TGZ1" = "$DEST1/mychart-1.2.3.tgz" ]; then
  pass "default version: emitted deterministic tgz path"
else
  fail "default version" "unexpected path or missing file: '$TGZ1'"
fi

# Extract the packaged values.yaml.
PV="${TMPDIR_T}/packaged-values.yaml"
tar xzOf "$TGZ1" mychart/values.yaml > "$PV"

# 2. Documentation comments are gone.
if ! grep -q 'documentation comment' "$PV" && ! grep -q 'inline doc comment' "$PV" && ! grep -q 'nested doc comment' "$PV"; then
  pass "doc comments stripped from packaged values.yaml"
else
  fail "doc comments stripped" "a documentation comment survived: $(grep -n 'comment' "$PV" | head)"
fi

# 3. Block-scalar body (shebang + in-script comment) preserved verbatim.
if grep -q '#!/bin/bash' "$PV" && grep -q 'a real shell comment inside the script body' "$PV"; then
  pass "block-scalar body preserved (shebang + in-body #-lines)"
else
  fail "block-scalar preserved" "shebang or in-body comment lost: $(grep -n 'bash\|hello' "$PV")"
fi

# 4. Values/keys preserved — render both and compare. The YAML comment lines
#    legitimately leave the render, so compare with comment-only lines dropped
#    from both sides, and as parsed documents (what the pack self-check does).
R_SRC="$(helm template t "$CHART" 2>/dev/null)"
# Re-expand the packaged chart to a dir and render it.
EXPAND="${TMPDIR_T}/expand"; mkdir -p "$EXPAND"
tar xzf "$TGZ1" -C "$EXPAND"
R_PKG="$(helm template t "$EXPAND/mychart" 2>/dev/null)"
if [ "$(grep -v -E '^[[:space:]]*#' <<<"$R_SRC")" = "$(grep -v -E '^[[:space:]]*#' <<<"$R_PKG")" ]; then
  pass "rendered output identical before/after comment strip (comment lines aside)"
else
  fail "render identity" "rendered manifests differ: $(diff <(echo "$R_SRC") <(echo "$R_PKG") | head)"
fi
if [ "$(yq ea -o=json '[.]' - <<<"$R_SRC")" = "$(yq ea -o=json '[.]' - <<<"$R_PKG")" ]; then
  pass "rendered objects identical as parsed YAML"
else
  fail "render semantic identity" "parsed documents differ"
fi
# The render lost the YAML doc comments and kept the block-scalar content.
if ! grep -q 'yaml doc comment' <<<"$R_PKG" && grep -q 'shell comment inside a block scalar' <<<"$R_PKG"; then
  pass "render: yaml doc comments gone, block-scalar #-lines kept"
else
  fail "render comment strip" "$(grep -n 'comment' <<<"$R_PKG")"
fi

# 5. --version / --app-version override lands in packaged Chart.yaml + path.
DEST2="${TMPDIR_T}/dest2"
TGZ2="$(bash "$SCRIPT_UNDER_TEST" "$CHART" "$DEST2" --version 1.2.3-rc --app-version 1.2.3-rc 2>/dev/null | tail -1)"
if [ "$TGZ2" = "$DEST2/mychart-1.2.3-rc.tgz" ] && [ -f "$TGZ2" ]; then
  APPV="$(tar xzOf "$TGZ2" mychart/Chart.yaml | yq '.appVersion' -)"
  if [ "$APPV" = "1.2.3-rc" ]; then
    pass "--version override lands in path and packaged Chart.yaml"
  else
    fail "version override" "packaged appVersion=$APPV, expected 1.2.3-rc"
  fi
else
  fail "version override" "unexpected path or missing file: '$TGZ2'"
fi

# 6. Source chart values.yaml is untouched.
SRC_HASH_AFTER="$(sha256sum "$CHART/values.yaml" | awk '{print $1}')"
if [ "$SRC_HASH_BEFORE" = "$SRC_HASH_AFTER" ]; then
  pass "source values.yaml not mutated"
else
  fail "source untouched" "source values.yaml changed on disk"
fi

# 7. Go-template comments stripped from the PACKAGED templates (the lever that
#    actually shrinks the release Secret), and the committed template untouched.
PT="${TMPDIR_T}/packaged-cm.yaml"
tar xzOf "$TGZ1" mychart/templates/cm.yaml > "$PT"
if ! grep -q 'leading template comment' "$PT" \
   && ! grep -q 'trim-left comment' "$PT"; then
  pass "go-template comments stripped from packaged templates"
else
  fail "template comments stripped" "a template comment survived: $(grep -n 'comment' "$PT")"
fi
# 7b. YAML comment lines stripped from the packaged template; the block-scalar
#     body's #-lines (shebang, shell comment) kept verbatim (gd-qiUiXvTX0k).
if ! grep -q 'yaml doc comment' "$PT" \
   && grep -q '#!/bin/sh' "$PT" \
   && grep -q 'shell comment inside a block scalar' "$PT"; then
  pass "yaml comment lines stripped from packaged templates, block-scalar #-lines kept"
else
  fail "yaml comments stripped" "$(grep -n '#' "$PT")"
fi
TPL_HASH_AFTER="$(sha256sum "$CHART/templates/cm.yaml" | awk '{print $1}')"
if [ "$TPL_HASH_BEFORE" = "$TPL_HASH_AFTER" ]; then
  pass "source templates not mutated"
else
  fail "source templates untouched" "source cm.yaml changed on disk"
fi

# 8. Stripping template comments measurably shrinks the packaged tgz vs a
#    values-only strip would — assert the packaged templates dir is smaller than
#    source (sanity that the strip ran and did something).
SRC_TPL_BYTES="$(cat "$CHART"/templates/* | wc -c)"
PKG_TPL_BYTES="$(tar xzOf "$TGZ1" mychart/templates/cm.yaml mychart/templates/_helpers.tpl 2>/dev/null | wc -c)"
if [ "$PKG_TPL_BYTES" -lt "$SRC_TPL_BYTES" ]; then
  pass "packaged templates smaller than source (comment strip effective)"
else
  fail "template shrink" "packaged=$PKG_TPL_BYTES not < source=$SRC_TPL_BYTES"
fi

echo
echo "pack-gardenio-chart_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
