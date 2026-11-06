#!/usr/bin/env python3
"""build-oci-archive.py — merge per-image OCI layouts into one combined OCI
layout suitable for `k3s ctr images import` (the air-gap --airgap-import-only
path, epic ka-0000002730, evaluation doc gardenio-in-a-box-ctr-import-eval.md §4).

INPUT  : the per-image OCI-layout tree the release bundle step produces
         (the RWX-era bundle pipeline; no workflow builds it today) —
         images/<slug>/ (one `crane pull --format=oci`
         layout per image) + manifest.tsv ("<slug>\\t<original-ref>" rows).
OUTPUT : a single combined OCI layout (oci-layout + index.json + shared
         blobs/) that, once tarred, is a docker/OCI image-ARCHIVE .tar —
         exactly the shape `ctr images import` consumes (a *tarred* OCI
         layout, NOT a layout directory, and NOT a docker-save tarball).

WHY oci-archive and not `crane pull --format=tarball` (docker save): the
import-only overlay (helm/gardenio/values-inbox.yaml) DIGEST-PINS every
first-party image and runs with `agentPullPolicy: Never`, so containerd must
already hold each image under its EXACT registry manifest digest. An OCI
layout preserves that digest byte-for-byte (the top-level manifest entry is
the unmodified registry blob); docker-save recomputes a fresh config +
manifest, producing a different digest → ErrImageNeverPull. So the archive
must be a tarred OCI layout.

Each image's single top-level manifest entry is copied into the combined
index.json with `org.opencontainers.image.ref.name` set to its ORIGINAL ref
(from manifest.tsv) so `ctr images import` names it identically to prod —
Helm/Go refs stay byte-identical, exactly like the zot seed path.

Usage:
  build-oci-archive.py <images-dir> <manifest.tsv> <out-layout-dir>
"""
import json
import os
import shutil
import sys


def main(argv):
    if len(argv) != 4:
        sys.exit("usage: build-oci-archive.py <images-dir> <manifest.tsv> "
                 "<out-layout-dir>")
    images_dir, manifest_tsv, arch_root = argv[1:4]

    arch_blobs = os.path.join(arch_root, "blobs", "sha256")
    os.makedirs(arch_blobs, exist_ok=True)
    # OCI image-layout marker required by the spec / ctr import.
    with open(os.path.join(arch_root, "oci-layout"), "w") as fh:
        json.dump({"imageLayoutVersion": "1.0.0"}, fh)

    out_index = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [],
    }

    with open(manifest_tsv) as fh:
        rows = [ln.rstrip("\n").split("\t", 1) for ln in fh if ln.strip()]

    seen_refs = set()
    for parts in rows:
        if len(parts) != 2:
            sys.exit(f"FATAL: malformed manifest.tsv row: {parts!r}")
        slug, ref = parts
        layout = os.path.join(images_dir, slug)
        with open(os.path.join(layout, "index.json")) as fh:
            idx = json.load(fh)
        mans = idx.get("manifests", [])
        if len(mans) != 1:
            # `crane pull --format=oci` emits exactly one top-level entry per
            # image (the registry index or single-arch manifest). Anything
            # else means the pull shape changed — fail loud, don't ship a bad
            # archive.
            sys.exit(f"FATAL: {slug} ({ref}) has {len(mans)} top-level "
                     f"manifests, expected 1")
        if ref in seen_refs:
            sys.exit(f"FATAL: duplicate ref in manifest.tsv: {ref}")
        seen_refs.add(ref)

        man = dict(mans[0])
        ann = dict(man.get("annotations", {}))
        ann["org.opencontainers.image.ref.name"] = ref
        man["annotations"] = ann
        out_index["manifests"].append(man)

        # Copy every blob (manifest + config + layers) into the shared store.
        # Blob filenames are content digests, so identical blobs across images
        # collide on the same name — a plain copy dedups the agent-base layers.
        src_blobs = os.path.join(layout, "blobs", "sha256")
        for blob in os.listdir(src_blobs):
            dst = os.path.join(arch_blobs, blob)
            if not os.path.exists(dst):
                shutil.copy2(os.path.join(src_blobs, blob), dst)

    with open(os.path.join(arch_root, "index.json"), "w") as fh:
        json.dump(out_index, fh)

    print(f"combined OCI layout: {len(out_index['manifests'])} images, "
          f"{len(os.listdir(arch_blobs))} unique blobs")


if __name__ == "__main__":
    main(sys.argv)
