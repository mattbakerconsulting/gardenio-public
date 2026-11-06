#!/usr/bin/env python3
"""check-image-registry_test.py -- unit tests for check-image-registry.py.

Each case writes a synthetic .github/workflows/images.yml into a tmpdir and
runs the guard against it with GARDENIO_ROOT. The bad cases are the drift the
guard exists for: (base_push) a controller-base job that runs, and so
pushes, with no controllerBase image selected,
(ecr_push) an image, cache or credential path that drifts back to ghcr, and
(ecr_repos) an image in images.json with no ECR repo in terraform, and
(no_ghcr) a ghcr login or tag that comes back after Task A5.

Usage:  python3 scripts/check-image-registry_test.py
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, "check-image-registry.py")
PASS = FAIL = 0

GOOD = """name: images
jobs:
  changes:
    runs-on: x
    outputs:
      needs_base: ${{ steps.select.outputs.needs_base }}
    steps:
      - id: select
        run: |
          needs_base=$(echo "$sel" | jq 'any(.[]; .controllerBase == true)')
          echo "needs_base=$needs_base" >> "$GITHUB_OUTPUT"
  controller-base:
    if: needs.changes.result == 'success' && needs.changes.outputs.needs_base == 'true'
    runs-on: x
    steps:
      - name: build + push controller-base
        uses: docker/build-push-action@v6
        with:
          push: true
  build:
    runs-on: x
    steps:
      - run: true
"""


def run(text):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY="base_push")
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


def case(name, text, want_rc, want_in=""):
    global PASS, FAIL
    rc, out = run(text)
    ok = rc == want_rc and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)


case("good workflow passes", GOOD, 0)
case("controller-base running with no consumer is rejected",
     GOOD.replace("    if: needs.changes.result == 'success' && needs.changes.outputs.needs_base == 'true'\n", ""),
     1, "its `if:` must require")
case("needs_base not derived from controllerBase is rejected",
     GOOD.replace("jq 'any(.[]; .controllerBase == true)'", "echo true"),
     1, "does not compute needs_base")
case("missing job output is rejected",
     GOOD.replace("      needs_base: ${{ steps.select.outputs.needs_base }}\n", ""),
     1, "outputs.needs_base")
case("output never written is rejected",
     GOOD.replace('          echo "needs_base=$needs_base" >> "$GITHUB_OUTPUT"\n', ""),
     1, "does not write needs_base")
case("missing controller-base step is rejected",
     GOOD.replace("build + push controller-base", "build controller-base"),
     1, "no 'build + push controller-base' step")


GOOD_ECR = GOOD.replace("name: images\n", """name: images
env:
  IMAGE_PREFIX: 360190250083.dkr.ecr.us-east-1.amazonaws.com/gardenio
""").replace("""  controller-base:
    if: needs.changes.result == 'success' && needs.changes.outputs.needs_base == 'true'
    runs-on: x
    steps:
""", """  controller-base:
    if: needs.changes.result == 'success' && needs.changes.outputs.needs_base == 'true'
    runs-on: x
    permissions:
      contents: read
      id-token: write
    steps:
      - name: AWS credentials (GitHub OIDC, no IMDS)
        uses: aws-actions/configure-aws-credentials@v4
""").replace("""  build:
    runs-on: x
    steps:
      - run: true
""", """  build:
    runs-on: x
    permissions:
      contents: read
      id-token: write
    steps:
      - name: AWS credentials (GitHub OIDC, no IMDS)
        uses: aws-actions/configure-aws-credentials@v4
      - name: build + push
        uses: docker/build-push-action@v6
        with:
          cache-from: type=registry,ref=${{ env.IMAGE_PREFIX }}/cache/${{ matrix.image }}:buildcache
          cache-to: x,image-manifest=true,oci-mediatypes=true
""")


def run_ecr(text):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY="ecr_push")
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


def case_ecr(name, text, want_rc, want_in=""):
    global PASS, FAIL
    rc, out = run_ecr(text)
    ok = rc == want_rc and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)


case_ecr("ECR workflow passes", GOOD_ECR, 0)
case_ecr("ghcr image prefix is rejected",
         GOOD_ECR.replace("360190250083.dkr.ecr.us-east-1.amazonaws.com/gardenio", "ghcr.io/mattbakerconsulting/gardenio"),
         1, "IMAGE_PREFIX")
case_ecr("a job without id-token: write is rejected",
         GOOD_ECR.replace("      id-token: write\n", "", 1),
         1, "id-token: write")
case_ecr("cache on ghcr is rejected",
         GOOD_ECR.replace("ref=${{ env.IMAGE_PREFIX }}/cache/", "ref=ghcr.io/x/cache/"),
         1, "cache")
case_ecr("ECR cache without image-manifest is rejected",
         GOOD_ECR.replace(",image-manifest=true,oci-mediatypes=true", ""),
         1, "image-manifest=true")


IMAGES_JSON = '[{"image": "kbeads"}, {"image": "landgate"}, {"image": "jira-bridge", "enabled": false}]'
TF_GOOD = """variable "other" {
  default = ["nope"]
}

variable "gardenio_image_components" {
  type = list(string)
  default = [
    "kbeads", "landgate",
    # disabled
    "jira-bridge",
  ]
  description = "x"
}
"""


def case_repos(name, images_json, tf, want_rc, want_in=""):
    global PASS, FAIL
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        open(os.path.join(d, ".github", "workflows", "images.yml"), "w").close()
        with open(os.path.join(d, ".github", "images.json"), "w") as f:
            f.write(images_json)
        tfdir = os.path.join(d, "gardenio", "terraform", "composition", "gardenio")
        os.makedirs(tfdir)
        with open(os.path.join(tfdir, "variables.tf"), "w") as f:
            f.write(tf)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY="ecr_repos")
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        rc, out = p.returncode, p.stdout + p.stderr
    ok = rc == want_rc and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)


case_repos("every image has a repo passes", IMAGES_JSON, TF_GOOD, 0)
case_repos("an enabled image with no repo is rejected",
           IMAGES_JSON, TF_GOOD.replace('"kbeads", "landgate",', '"kbeads",'),
           1, "landgate is in .github/images.json but not in gardenio_image_components")
case_repos("a disabled image with no repo is rejected",
           IMAGES_JSON, TF_GOOD.replace('    "jira-bridge",\n', ""),
           1, "jira-bridge is in .github/images.json")
case_repos("a commented-out component does not count",
           IMAGES_JSON, TF_GOOD.replace('    "jira-bridge",', '    # "jira-bridge",'),
           1, "jira-bridge is in .github/images.json")
case_repos("a missing variable is rejected",
           IMAGES_JSON, TF_GOOD.replace("gardenio_image_components", "renamed"),
           1, "no gardenio_image_components default list")

def run_only(text, only):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY=only)
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


for name, text, want, want_in in [
    ("no ghcr anywhere passes", GOOD_ECR, 0, ""),
    ("a ghcr comment is allowed", GOOD_ECR + "      # the ghcr.io pull-through is the rollback\n", 0, ""),
    ("a ghcr login is rejected", GOOD_ECR + "      - uses: docker/login-action@v3\n        with:\n          registry: ghcr.io\n", 1, "still names ghcr.io: registry: ghcr.io"),
    ("a ghcr tag is rejected", GOOD_ECR + '            echo "ghcr.io/mattbakerconsulting/gardenio/x:latest"\n', 1, "still names ghcr.io: echo"),
]:
    rc, out = run_only(text, "no_ghcr")
    ok = rc == want and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)
print(f"{PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
