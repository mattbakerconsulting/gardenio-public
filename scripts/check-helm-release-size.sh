#!/usr/bin/env bash
# check-helm-release-size.sh
#
# Estimate the size of the Helm release record (the Secret
# sh.helm.release.v1.<release>.vN) that `helm upgrade` would store for the
# gardenio chart, and FAIL when it exceeds a cap. A Kubernetes Secret may not
# exceed 1,048,576 bytes; release 2026.292.1 (gd-qiUiXvTX0k) failed with
#   Secret "sh.helm.release.v1.gardenio.v186" is invalid: data: Too long
# because the record had crept to 96% of that limit, and EVERY later release
# fails the same way until the chart shrinks. This guard runs in CI (the
# checks.yml helm job) so the next creep is caught on the PR, not in the
# release-deployer.
#
# WHAT IS MEASURED. Helm (pkg/storage/driver) stores
#   base64( gzip( json(release) ) )
# in the Secret's single `release` key, and the 1 MiB limit applies to that
# base64 string. json(release) carries: the chart as a structured object
# (metadata, every templates/** file and files/** file base64-encoded, the
# PARSED default values, the schema), the user-supplied config, the rendered
# manifest, hooks, and info. This script rebuilds that JSON from the PACKED
# chart (scripts/pack-gardenio-chart.sh, exactly what release-deploy.sh
# installs) and `helm template`, then gzips and base64-encodes it the same way.
# It is an estimate: hooks are counted inside the manifest rather than as
# separate objects, and Go's gzip and python's zlib differ at level 9.
# Calibrated against the live v185 record (tag 2026.291.7, 1,008,312 B as
# stored): this script said 991,472 B, 1.7% UNDER, so the cap below keeps its
# own margin on top of that. Measured here (gd-qiUiXvTX0k, same tag inputs):
#   main at 2026.292.1 + 2 commits   1,118,396 B   (over the 1 MiB limit)
#   theme out of the chart + YAML comment strip + values-*.yaml ignored
#                                      644,636 B
#
# Usage:
#   check-helm-release-size.sh [--chart DIR] [--values FILE] [--limit BYTES]
#                              [--release NAME] [--namespace NS] [--tag CALVER]
#                              [--keep DIR]
#
# Defaults mirror release-deploy.sh: chart gardenio/helm/gardenio, values
# deploy-values/gardenio-k3s.yaml, release/namespace gardenio. --limit
# defaults to 921600 (900 KiB, 12% under the 1 MiB hard limit). --keep saves
# the packed chart, manifest and release JSON for inspection.
#
# Exit codes: 0 under the limit, 1 over the limit, 2 usage / tooling error.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOPLEVEL="$(cd "$SCRIPT_DIR/.." && pwd)"

CHART_DIR="$TOPLEVEL/gardenio/helm/gardenio"
VALUES="$TOPLEVEL/deploy-values/gardenio-k3s.yaml"
LIMIT=921600
RELEASE="gardenio"
NAMESPACE="gardenio"
TAG="2026.999.9"
KEEP=""

usage() {
  sed -n '/^# Usage:/,/^# Exit codes/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --chart)     CHART_DIR="$2"; shift 2 ;;
    --values)    VALUES="$2"; shift 2 ;;
    --limit)     LIMIT="$2"; shift 2 ;;
    --release)   RELEASE="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --tag)       TAG="$2"; shift 2 ;;
    --keep)      KEEP="$2"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "check-helm-release-size.sh: unknown flag: $1" >&2; usage; exit 2 ;;
  esac
done

for t in helm yq python3; do
  command -v "$t" >/dev/null 2>&1 || { echo "check-helm-release-size.sh: FATAL: '$t' not on PATH" >&2; exit 2; }
done
[ -d "$CHART_DIR" ] || { echo "check-helm-release-size.sh: FATAL: chart dir not found: $CHART_DIR" >&2; exit 2; }
[ -f "$VALUES" ] || { echo "check-helm-release-size.sh: FATAL: values file not found: $VALUES" >&2; exit 2; }

if [ -n "$KEEP" ]; then
  mkdir -p "$KEEP"
  WORK="$KEEP"
else
  WORK="$(mktemp -d)"
  trap 'rm -rf "$WORK"' EXIT
fi

# 1. Pack the chart exactly as release-deploy.sh does (comment-stripped,
#    calver-versioned), so the measurement is of what gets installed.
if ! PACK_OUT="$(bash "$SCRIPT_DIR/pack-gardenio-chart.sh" "$CHART_DIR" "$WORK/pack" \
    --version "$TAG" --app-version "$TAG" 2>"$WORK/pack.err")"; then
  echo "check-helm-release-size.sh: FATAL: chart pack failed:" >&2
  cat "$WORK/pack.err" >&2
  exit 2
fi
TGZ="$(tail -n 1 <<<"$PACK_OUT")"
[ -f "$TGZ" ] || { echo "check-helm-release-size.sh: FATAL: pack produced no tgz ('$TGZ')" >&2; exit 2; }

# 2. Render the manifest from the packed chart with the deploy values, the
#    same inputs as the upgrade. --set global.imageTag mirrors the deployer.
if ! helm template "$RELEASE" "$TGZ" -n "$NAMESPACE" -f "$VALUES" \
    --set "global.imageTag=${TAG}" > "$WORK/manifest.yaml" 2>"$WORK/template.err"; then
  echo "check-helm-release-size.sh: FATAL: helm template failed:" >&2
  cat "$WORK/template.err" >&2
  exit 2
fi

# 3. The user-supplied config helm stores: the values file plus the --set.
yq -o=json '.global.imageTag = "'"$TAG"'"' "$VALUES" > "$WORK/config.json"

# 4. Rebuild json(release), gzip -9, base64, and measure. Per-part sizes are
#    each part gzipped alone, for attribution (they do not sum to the total).
python3 - "$TGZ" "$WORK/manifest.yaml" "$WORK/config.json" "$RELEASE" "$NAMESPACE" "$LIMIT" "$WORK" <<'PY'
import base64, gzip, io, json, os, subprocess, sys, tarfile

tgz, manifest_path, config_path, release, namespace, limit, work = sys.argv[1:8]
limit = int(limit)

def yaml_json(data: bytes):
    """Parse YAML with yq (no pyyaml on the CI runner)."""
    out = subprocess.run(["yq", "-o=json", "-I0", "."], input=data, capture_output=True, check=True)
    return json.loads(out.stdout or b"null")

templates, files, values, metadata, schema, lock = [], [], None, None, None, None
raw = {"templates": 0, "files": 0}
with tarfile.open(tgz) as tar:
    for m in tar.getmembers():
        if not m.isfile():
            continue
        rel = m.name.split("/", 1)[1] if "/" in m.name else m.name
        data = tar.extractfile(m).read()
        if rel == "Chart.yaml":
            metadata = yaml_json(data)
        elif rel == "values.yaml":
            values = yaml_json(data)
        elif rel == "values.schema.json":
            schema = base64.b64encode(data).decode()
        elif rel == "Chart.lock":
            lock = yaml_json(data)
        elif rel.startswith("templates/"):
            templates.append({"name": rel, "data": base64.b64encode(data).decode()})
            raw["templates"] += len(data)
        elif rel == ".helmignore":
            continue
        else:
            files.append({"name": rel, "data": base64.b64encode(data).decode()})
            raw["files"] += len(data)

with open(manifest_path, "rb") as f:
    manifest = f.read().decode()
with open(config_path) as f:
    config = json.load(f)

rel = {
    "name": release,
    "info": {
        "first_deployed": "2026-01-01T00:00:00.000000000Z",
        "last_deployed": "2026-01-01T00:00:00.000000000Z",
        "deleted": "",
        "description": "Upgrade complete",
        "status": "deployed",
    },
    "chart": {
        "metadata": metadata,
        "lock": lock,
        "templates": templates,
        "values": values,
        "schema": schema,
        "files": files,
    },
    "config": config,
    "manifest": manifest,
    "version": 999,
    "namespace": namespace,
    "labels": {},
}

def encode(obj) -> int:
    b = json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode()
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", compresslevel=9, mtime=0) as gz:
        gz.write(b)
    return len(base64.b64encode(buf.getvalue()))

total = encode(rel)
with open(os.path.join(work, "release.json"), "w") as f:
    json.dump(rel, f, separators=(",", ":"), ensure_ascii=False)

parts = [
    ("chart.templates", encode(templates), raw["templates"]),
    ("chart.files", encode(files), raw["files"]),
    ("chart.values", encode(values), None),
    ("manifest", encode(manifest), len(manifest.encode())),
    ("config", encode(config), None),
]
print(f"helm release record estimate for {os.path.basename(tgz)}")
print(f"  {'part':18} {'encoded alone':>14} {'raw':>10}")
for name, enc, rawsz in parts:
    print(f"  {name:18} {enc:>14,} {('' if rawsz is None else f'{rawsz:,}'):>10}")
print(f"  {'TOTAL (base64 of gzip of json)':34} {total:>12,} B")
print(f"  limit {limit:,} B ({limit / 1048576:.0%} of the 1,048,576 B Secret max); headroom {limit - total:,} B")
if total > limit:
    print(f"FAIL: estimated release record {total:,} B exceeds {limit:,} B", file=sys.stderr)
    sys.exit(1)
print("OK")
PY
