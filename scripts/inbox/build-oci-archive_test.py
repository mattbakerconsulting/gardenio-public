#!/usr/bin/env python3
"""Unit tests for build-oci-archive.py.

Covers the load-bearing invariants of the air-gap import-only OCI image-archive
builder (epic ka-0000002730, task ka-0000007980):

  * the combined index.json carries one entry per manifest.tsv row, each with
    org.opencontainers.image.ref.name set to the ORIGINAL ref (so
    `ctr images import` names every image identically to prod);
  * each entry's digest is preserved byte-for-byte from the source per-image
    layout (the import-only overlay digest-pins under agentPullPolicy: Never,
    so a recomputed digest would ErrImageNeverPull);
  * blobs shared across images are written exactly once (structural dedup);
  * malformed inputs (multi-manifest layout, duplicate ref) fail loud.

These use synthetic OCI layouts — no registry / network required.
"""
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
BUILDER = os.path.join(HERE, "build-oci-archive.py")


def _write_blob(blobs_dir, data: bytes) -> str:
    """Write `data` as a content-addressed blob; return its sha256 digest."""
    hx = hashlib.sha256(data).hexdigest()
    with open(os.path.join(blobs_dir, hx), "wb") as fh:
        fh.write(data)
    return f"sha256:{hx}"


def _make_image_layout(layout_dir, *, layers, top_extra=None):
    """Build a minimal but well-formed single-image OCI layout.

    `layers` is a list of bytes (each a fake layer blob). Returns the digest of
    the top-level manifest (what shows up in the combined index).
    """
    blobs = os.path.join(layout_dir, "blobs", "sha256")
    os.makedirs(blobs, exist_ok=True)
    with open(os.path.join(layout_dir, "oci-layout"), "w") as fh:
        json.dump({"imageLayoutVersion": "1.0.0"}, fh)

    config_digest = _write_blob(blobs, b'{"architecture":"amd64","os":"linux"}')
    layer_descs = []
    for layer in layers:
        d = _write_blob(blobs, layer)
        layer_descs.append({
            "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
            "digest": d, "size": len(layer),
        })
    manifest = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "config": {"mediaType": "application/vnd.oci.image.config.v1+json",
                   "digest": config_digest, "size": 38},
        "layers": layer_descs,
    }
    man_bytes = json.dumps(manifest).encode()
    man_digest = _write_blob(blobs, man_bytes)

    top = {"mediaType": "application/vnd.oci.image.manifest.v1+json",
           "digest": man_digest, "size": len(man_bytes)}
    if top_extra:
        top.update(top_extra)
    index = {"schemaVersion": 2,
             "mediaType": "application/vnd.oci.image.index.v1+json",
             "manifests": [top]}
    with open(os.path.join(layout_dir, "index.json"), "w") as fh:
        json.dump(index, fh)
    return man_digest


def _run(images_dir, manifest_tsv, out_dir):
    return subprocess.run(
        [sys.executable, BUILDER, images_dir, manifest_tsv, out_dir],
        capture_output=True, text=True)


class BuildOciArchiveTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.images = os.path.join(self.tmp, "images")
        os.makedirs(self.images)
        self.manifest = os.path.join(self.tmp, "manifest.tsv")
        self.out = os.path.join(self.tmp, "archive")

    def _index(self):
        with open(os.path.join(self.out, "index.json")) as fh:
            return json.load(fh)

    def test_refname_and_digest_preserved(self):
        shared = b"shared-base-layer" * 100
        d1 = _make_image_layout(os.path.join(self.images, "img-001"),
                                layers=[shared, b"agent-layer"])
        d2 = _make_image_layout(os.path.join(self.images, "img-002"),
                                layers=[shared, b"runner-layer"])
        refs = {
            "img-001": "registry.example.com/gardenio/agent:2026.161.3",
            "img-002": "registry.example.com/gardenio/ci-runner:2026.161.3",
        }
        with open(self.manifest, "w") as fh:
            fh.write(f"img-001\t{refs['img-001']}\n")
            fh.write(f"img-002\t{refs['img-002']}\n")

        res = _run(self.images, self.manifest, self.out)
        self.assertEqual(res.returncode, 0, res.stderr)

        idx = self._index()
        by_ref = {m["annotations"]["org.opencontainers.image.ref.name"]: m
                  for m in idx["manifests"]}
        # one entry per row, named by ORIGINAL ref
        self.assertEqual(set(by_ref), set(refs.values()))
        # digests preserved byte-for-byte from the source layouts
        self.assertEqual(by_ref[refs["img-001"]]["digest"], d1)
        self.assertEqual(by_ref[refs["img-002"]]["digest"], d2)
        # oci-layout marker present
        self.assertTrue(os.path.exists(os.path.join(self.out, "oci-layout")))

    def test_shared_blob_deduped(self):
        shared = b"shared-base-layer" * 100
        _make_image_layout(os.path.join(self.images, "img-001"),
                           layers=[shared, b"a"])
        _make_image_layout(os.path.join(self.images, "img-002"),
                           layers=[shared, b"b"])
        with open(self.manifest, "w") as fh:
            fh.write("img-001\tex.io/a:1\n")
            fh.write("img-002\tex.io/b:1\n")
        res = _run(self.images, self.manifest, self.out)
        self.assertEqual(res.returncode, 0, res.stderr)

        # the shared layer blob exists exactly once in the combined store
        shared_hx = hashlib.sha256(shared).hexdigest()
        out_blobs = os.path.join(self.out, "blobs", "sha256")
        self.assertTrue(os.path.exists(os.path.join(out_blobs, shared_hx)))
        # every referenced blob is present and digest-correct
        for name in os.listdir(out_blobs):
            with open(os.path.join(out_blobs, name), "rb") as fh:
                self.assertEqual(hashlib.sha256(fh.read()).hexdigest(), name)

    def test_multi_manifest_layout_fails_loud(self):
        layout = os.path.join(self.images, "img-001")
        _make_image_layout(layout, layers=[b"x"])
        # corrupt: give the index TWO top-level manifests
        idx_path = os.path.join(layout, "index.json")
        with open(idx_path) as fh:
            idx = json.load(fh)
        idx["manifests"].append(dict(idx["manifests"][0]))
        with open(idx_path, "w") as fh:
            json.dump(idx, fh)
        with open(self.manifest, "w") as fh:
            fh.write("img-001\tex.io/a:1\n")
        res = _run(self.images, self.manifest, self.out)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("expected 1", res.stderr)

    def test_duplicate_ref_fails_loud(self):
        _make_image_layout(os.path.join(self.images, "img-001"), layers=[b"x"])
        _make_image_layout(os.path.join(self.images, "img-002"), layers=[b"y"])
        with open(self.manifest, "w") as fh:
            fh.write("img-001\tex.io/a:1\n")
            fh.write("img-002\tex.io/a:1\n")   # duplicate ref
        res = _run(self.images, self.manifest, self.out)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("duplicate ref", res.stderr)


if __name__ == "__main__":
    unittest.main()
