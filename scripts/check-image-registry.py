#!/usr/bin/env python3
"""check-image-registry.py -- guard where .github/workflows/images.yml pushes.

WHY: internet egress from the CI nodes is paid ($0.09/GB). Measured
2026-10-13..20: 0.308 GB of egress per controller-base push, r=0.963, and
a push nothing pulls is pure cost. This guard keeps the push gated on a
consumer (gd-eGXedWQJfw). Plan:
docs/superpowers/plans/2026-10-21-aws-cost-cuts.md.

Checks (each a function returning a list of error strings):
  base_push  controller-base (which always pushes) runs only when the
             selection holds a controllerBase image (outputs.needs_base).
             Until gd-whdmfznixO a PR built it without pushing; images.yml
             has no PR trigger now, so the job itself is the gate.
  ecr_push   images and caches go to ECR; every job that builds has
             id-token: write and an OIDC credentials step; ECR caches carry
             image-manifest=true,oci-mediatypes=true.
  ecr_repos  every image in .github/images.json, enabled or not, is in
             gardenio_image_components (terraform), which creates its
             gardenio/<image> and gardenio/cache/<image> repos. ECR does not
             create a repo on push here and the release role has no
             ecr:CreateRepository, so a missing one fails that image's push
             and, on a calver tag, its ghcr mirror tags too (PR #844).

Text matching on purpose: the runners have no PyYAML.
Usage:  python3 scripts/check-image-registry.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-image-registry.py
        CHECK_IMAGE_REGISTRY_ONLY=base_push (run one check; used by the tests)
"""
import json
import os
import re
import sys

ROOT = os.environ.get(
    "GARDENIO_ROOT", os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
)
WF = os.path.join(ROOT, ".github", "workflows", "images.yml")
IMAGES_JSON = os.path.join(ROOT, ".github", "images.json")
TF_VARS = os.path.join(
    ROOT, "gardenio", "terraform", "composition", "gardenio", "variables.tf"
)
BASE_IF = "needs.changes.outputs.needs_base == 'true'"
NEEDS_BASE = "needs_base=$(echo \"$sel\" | jq 'any(.[]; .controllerBase == true)')"


def job_block(text, job):
    """Body of one job under `jobs:` (2-space job keys)."""
    m = re.search(
        rf"^  {re.escape(job)}:\n(.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)", text, re.S | re.M
    )
    return m.group(1) if m else ""


def step_block(job_text, name):
    m = re.search(
        rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)", job_text, re.S | re.M
    )
    return m.group(1) if m else None


def check_base_push(text):
    errs = []
    changes = job_block(text, "changes")
    if not re.search(
        r"^\s+needs_base: \$\{\{ steps\.select\.outputs\.needs_base \}\}\s*$",
        changes,
        re.M,
    ):
        errs.append("changes job does not export outputs.needs_base")
    if NEEDS_BASE not in changes:
        errs.append(f"select step does not compute needs_base as: {NEEDS_BASE}")
    if 'echo "needs_base=$needs_base" >> "$GITHUB_OUTPUT"' not in changes:
        errs.append("select step does not write needs_base to $GITHUB_OUTPUT")
    base = job_block(text, "controller-base")
    im = re.search(r"^    if: (.+?)\s*$", base, re.M)
    if not im or BASE_IF not in im.group(1):
        errs.append(f"controller-base job: its `if:` must require {BASE_IF}")
    if step_block(base, "build + push controller-base") is None:
        errs.append("controller-base job has no 'build + push controller-base' step")
    return errs


ECR_PREFIX = "360190250083.dkr.ecr.us-east-1.amazonaws.com/gardenio"


def check_ecr_push(text):
    errs = []
    m = re.search(r"^  IMAGE_PREFIX: (\S+)\s*$", text, re.M)
    if not m or m.group(1) != ECR_PREFIX:
        errs.append(f"env IMAGE_PREFIX must be {ECR_PREFIX}")
    for job in ("controller-base", "build"):
        body = job_block(text, job)
        if not body:
            errs.append(f"job {job} not found")
            continue
        if not re.search(r"^      id-token: write\b", body, re.M):
            errs.append(f"job {job} lacks permissions id-token: write")
        if "aws-actions/configure-aws-credentials@" not in body:
            errs.append(f"job {job} has no aws-actions/configure-aws-credentials step")
    for line in re.findall(r"^\s+cache-(?:from|to): (.+)$", text, re.M):
        if "ghcr.io" in line or "env.REGISTRY" in line:
            errs.append(f"cache must not live on ghcr: {line.strip()}")
    for line in re.findall(r"^\s+cache-to: (.+)$", text, re.M):
        if line.strip() not in ("", "''") and "image-manifest=true,oci-mediatypes=true" not in line:
            errs.append(f"ECR registry cache needs image-manifest=true,oci-mediatypes=true: {line.strip()}")
    return errs


def tf_image_components(tf):
    """Default list of variable "gardenio_image_components", or None."""
    m = re.search(
        r'^variable "gardenio_image_components" \{\n(.*?)^\}', tf, re.S | re.M
    )
    if not m:
        return None
    d = re.search(r"^\s*default\s*=\s*\[(.*?)\]", m.group(1), re.S | re.M)
    if not d:
        return None
    body = re.sub(r"#[^\n]*", "", d.group(1))
    return re.findall(r'"([^"]+)"', body)


def check_ecr_repos(_text):
    try:
        with open(IMAGES_JSON) as f:
            images = [e["image"] for e in json.load(f)]
        with open(TF_VARS) as f:
            components = tf_image_components(f.read())
    except (OSError, ValueError, KeyError, TypeError) as e:
        return [f"cannot read images.json or variables.tf: {e}"]
    if components is None:
        return ["variables.tf has no gardenio_image_components default list"]
    return [
        f"{i} is in .github/images.json but not in gardenio_image_components "
        "(gardenio/terraform/composition/gardenio/variables.tf): it has no ECR "
        "repo, so its push fails; add it and have the operator apply"
        for i in images
        if i not in components
    ]


def check_no_ghcr(text):
    """After the transition, nothing in images.yml may push to or log in to ghcr."""
    errs = []
    for i, line in enumerate(text.splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue
        if "ghcr.io" in line:
            errs.append(f"images.yml:{i} still names ghcr.io: {line.strip()}")
    return errs


CHECKS = {
    "base_push": check_base_push,
    "ecr_push": check_ecr_push,
    "ecr_repos": check_ecr_repos,
    "no_ghcr": check_no_ghcr,
}


def main():
    with open(WF) as f:
        text = f.read()
    only = os.environ.get("CHECK_IMAGE_REGISTRY_ONLY")
    errs = [] if not only or only in CHECKS else [f"{only}: unknown check"]
    for name, fn in CHECKS.items():
        if only and name != only:
            continue
        errs += [f"{name}: {e}" for e in fn(text)]
    for e in errs:
        print(f"check-image-registry: FAIL {e}")
    if not errs:
        print("check-image-registry: ok")
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main())
