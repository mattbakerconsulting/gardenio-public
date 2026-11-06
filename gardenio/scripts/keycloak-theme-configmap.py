#!/usr/bin/env python3
"""keycloak-theme-configmap.py

Render the Keycloak login-theme tree (gardenio/keycloak-theme/gardenio/**) as a
ConfigMap manifest the Keycloak runtime pod mounts, and print it as JSON on
stdout for `kubectl apply -f -`.

WHY THE CHART DOES NOT OWN THIS (gd-qiUiXvTX0k): Helm stores the whole chart
(every file base64-encoded) AND the rendered manifest in the release Secret
sh.helm.release.v1.<release>.vN, which may not exceed 1 MiB. The theme tree was
stored TWICE there (as chart files/ and again inside the rendered ConfigMap),
and the Dawn Meadow assets tipped release 2026.292.1 over the limit. The theme
now lives outside the chart; the release-deployer (gardenio/scripts/
release-deploy.sh) applies this ConfigMap right before `helm upgrade`, and the
chart's StatefulSet only references it by name.

LAYOUT (unchanged from the chart-rendered ConfigMap, gd-S6sixAadTe): ConfigMap
keys cannot contain "/", so every file becomes one key with "/" encoded as
"__". Text files go in `data`; binary ones (fonts, raster images, by
extension) go in `binaryData` as base64, because the text path would replace
non-UTF-8 bytes with U+FFFD (S1h, gd-0SYmLWCCM5). The pod's theme-unpack
initContainer maps every key back to its path. A path component containing
"__" is refused: it could not be decoded unambiguously.

RESTART ON THEME CHANGE (#544 semantics, gd-cW3og8FqhN): the manifest carries
the annotation gardenio.dev/theme-checksum = sha256 over the theme FILES (one
"<path> <sha256 of content>" line per file, paths sorted), never over the
manifest. The chart reads that annotation at upgrade time (`lookup`) into the
pod template's checksum/theme annotation, so a real theme edit restarts
Keycloak (it caches templates) and a calver release that touches no theme file
leaves the pod alone.

Usage:
  keycloak-theme-configmap.py [--theme-dir DIR] [--name NAME] [--namespace NS]
                              [--release NAME] [--max-bytes N] [--checksum-only]

  --theme-dir   theme root, default gardenio/keycloak-theme/gardenio (relative
                to the repo this script lives in)
  --name        ConfigMap name, default <release>-keycloak-runtime-theme-files
                (what the chart derives for keycloak.runtime.theme.configMapName
                when that value is empty)
  --release     Helm release name the chart is installed as, default gardenio
  --namespace   default gardenio
  --max-bytes   refuse a ConfigMap larger than this as stored (binary files
                count base64-encoded), default 921600 (900 KiB), under the
                1 MiB ConfigMap limit
  --checksum-only  print only the checksum (what checksum/theme will carry)

Exit codes: 0 ok; 1 the theme tree is unusable (missing, empty, undecodable
name, over the cap); 2 usage.
"""
import argparse
import base64
import hashlib
import json
import os
import re
import sys

BINARY_RE = re.compile(r"\.(woff2?|ttf|otf|eot|png|jpe?g|gif|ico|webp)$")
KEY_RE = re.compile(r"^[-._a-zA-Z0-9]+$")
CHECKSUM_ANNOTATION = "gardenio.dev/theme-checksum"
DEFAULT_MAX_BYTES = 921600


def repo_root():
    return os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))


def is_binary(rel):
    return bool(BINARY_RE.search(rel))


def key_for(rel):
    return rel.replace("/", "__")


def theme_files(theme_dir):
    """Sorted (relative slash path, bytes) pairs for every file in the tree."""
    out = []
    for root, _dirs, names in os.walk(theme_dir):
        for n in names:
            p = os.path.join(root, n)
            rel = os.path.relpath(p, theme_dir).replace(os.sep, "/")
            with open(p, "rb") as f:
                out.append((rel, f.read()))
    out.sort(key=lambda x: x[0])
    return out


def checksum(files):
    """sha256 over "<rel> <sha256(content)>\\n" lines, paths sorted."""
    acc = "".join("%s %s\n" % (rel, hashlib.sha256(data).hexdigest()) for rel, data in files)
    return hashlib.sha256(acc.encode()).hexdigest()


def build(files, name, namespace, release, max_bytes):
    data, binary, stored = {}, {}, 0
    for rel, content in files:
        if "__" in rel:
            raise ValueError("theme path %r contains '__', which encodes '/' in ConfigMap keys" % rel)
        key = key_for(rel)
        if not KEY_RE.match(key):
            raise ValueError("theme path %r does not map to a valid ConfigMap key (%r)" % (rel, key))
        if is_binary(rel):
            b64 = base64.b64encode(content).decode()
            binary[key] = b64
            stored += len(b64)
        else:
            try:
                data[key] = content.decode("utf-8")
            except UnicodeDecodeError as e:
                raise ValueError("theme file %r is not UTF-8 text and has no binary extension: %s" % (rel, e))
            stored += len(content)
    if stored > max_bytes:
        raise ValueError(
            "theme files total %d bytes as stored (binary files count base64-encoded); the cap is %d"
            % (stored, max_bytes)
        )
    cm = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/name": "gardenio",
                "app.kubernetes.io/instance": release,
                "app.kubernetes.io/component": "keycloak-runtime",
                "app.kubernetes.io/managed-by": "release-deploy",
            },
            "annotations": {CHECKSUM_ANNOTATION: checksum(files)},
        },
        "data": data,
    }
    if binary:
        cm["binaryData"] = binary
    return cm


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0], add_help=True)
    ap.add_argument("--theme-dir", default=os.path.join(repo_root(), "gardenio", "keycloak-theme", "gardenio"))
    ap.add_argument("--release", default="gardenio")
    ap.add_argument("--name", default="")
    ap.add_argument("--namespace", default="gardenio")
    ap.add_argument("--max-bytes", type=int, default=DEFAULT_MAX_BYTES)
    ap.add_argument("--checksum-only", action="store_true")
    args = ap.parse_args(argv[1:])

    if not os.path.isdir(args.theme_dir):
        sys.stderr.write("keycloak-theme-configmap.py: theme dir not found: %s\n" % args.theme_dir)
        return 1
    files = theme_files(args.theme_dir)
    if not files:
        sys.stderr.write("keycloak-theme-configmap.py: no files under %s\n" % args.theme_dir)
        return 1
    if args.checksum_only:
        print(checksum(files))
        return 0
    name = args.name or "%s-keycloak-runtime-theme-files" % args.release
    try:
        cm = build(files, name, args.namespace, args.release, args.max_bytes)
    except ValueError as e:
        sys.stderr.write("keycloak-theme-configmap.py: %s\n" % e)
        return 1
    json.dump(cm, sys.stdout, separators=(",", ":"), sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
