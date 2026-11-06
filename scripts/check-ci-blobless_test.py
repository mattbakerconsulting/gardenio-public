#!/usr/bin/env python3
"""check-ci-blobless_test.py -- unit tests for check-ci-blobless.py

Each case writes one workflow into a tmpdir and runs the guard with
GARDENIO_ROOT pointed at it. Positive controls first (a compliant set passes
and says how many checkouts it read), then one case per way to lose the
filter, then the real tree.
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, "check-ci-blobless.py")
failures = 0


def run(workflow, root=None):
    env = dict(os.environ)
    if workflow is not None:
        root = tempfile.mkdtemp()
        os.makedirs(os.path.join(root, ".github", "workflows"))
        with open(os.path.join(root, ".github", "workflows", "w.yml"), "w") as fh:
            fh.write(workflow)
    if root is not None:
        env["GARDENIO_ROOT"] = root
    p = subprocess.run([sys.executable, GUARD], env=env,
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def check(name, workflow, want_fail, want_text, root=None):
    global failures
    rc, out = run(workflow, root)
    ok = (rc != 0) == want_fail and want_text in out
    print("%s  %s" % ("ok  " if ok else "FAIL", name))
    if not ok:
        failures += 1
        print("      rc=%d, wanted %s containing %r:\n%s"
              % (rc, "fail" if want_fail else "pass", want_text, out))


HEAD = "on: pull_request\njobs:\n  j:\n    runs-on: x\n    steps:\n"

check("block form with blob:none passes", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n"
      "          filter: blob:none\n", False, "OK (1 checkout steps")
check("flow form with blob:none passes", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with: { fetch-depth: 0, filter: blob:none }\n", False, "OK (1")
check("conditional filter naming blob:none passes", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n"
      "          filter: ${{ github.event_name == 'pull_request' && 'blob:none' || '' }}\n",
      False, "OK (1")
check("shallow checkout needs no filter", HEAD +
      "      - uses: actions/checkout@v4\n"
      "      - name: second\n"
      "        uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 1\n", False, "OK (2 checkout steps")
check("a later step's filter does not count", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n"
      "      - uses: other/action@v1\n"
      "        with:\n"
      "          filter: blob:none\n", True, "no `filter:`")

check("block form, other keys after fetch-depth, no filter fails", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n"
      "          ref: main\n", True, "no `filter:`")
check("fetch-depth 0 with no filter fails", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n", True, "no `filter:`")
check("flow form with no filter fails", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with: { fetch-depth: 0 }\n", True, "no `filter:`")
check("quoted '0' with no filter fails", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: '0'\n", True, "no `filter:`")
check("filter in a comment does not count", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0 # filter: blob:none\n", True, "no `filter:`")
check("name-first step without filter fails", HEAD +
      "      - name: checkout\n"
      "        uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n", True, "w.yml:6")
check("tree:0 filter is not blob:none", HEAD +
      "      - uses: actions/checkout@v4\n"
      "        with:\n"
      "          fetch-depth: 0\n"
      "          filter: tree:0\n", True, "never asks for `blob:none`")
check("no checkouts at all fails (parser matched nothing)",
      HEAD + "      - run: true\n", True, "read 0 checkout steps")

check("the real tree passes", None, False, "check-ci-blobless: OK",
      root=os.path.dirname(HERE))

sys.exit(1 if failures else 0)
