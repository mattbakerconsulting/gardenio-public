#!/usr/bin/env python3
"""check-no-pr-goldens_test.py -- unit tests for check-no-pr-goldens.py

Each case builds a git repo in a tmpdir: `main` holds one golden and one
source file, a `feat` branch (or the named branch) changes what the case says,
and the guard runs there with GARDENIO_ROOT pointed at it and --base main.
Positive controls first (a PR with no golden passes and says how many paths it
read), then the bless branch, then the ways to fail, including a diff that
cannot run, which must fail rather than pass on an empty list.
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, "check-no-pr-goldens.py")
G = "gardenio/controller/internal/gardenhub/testdata/golden/"
V = "gardenio/controller/cmd/beads-viewer/testdata/golden/lands/"
failures = 0


def git(root, *args):
    subprocess.run(("git", "-c", "user.email=t@t", "-c", "user.name=t",
                    "-c", "commit.gpgsign=false") + args,
                   cwd=root, check=True, capture_output=True)


def write(root, path, text):
    full = os.path.join(root, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w") as fh:
        fh.write(text)


def repo(branch, changes, deletes=()):
    root = tempfile.mkdtemp()
    git(root, "init", "-q", "-b", "main")
    write(root, G + "bead-phone-light.png", "old")
    write(root, G + "fonts/NotoSans-Regular.ttf", "font")
    write(root, "gardenio/controller/internal/gardenhub/view.go", "package x")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "main")
    git(root, "checkout", "-q", "-b", branch)
    for path in changes:
        write(root, path, "new")
    for path in deletes:
        os.remove(os.path.join(root, path))
    git(root, "add", "-A")
    git(root, "commit", "-q", "--allow-empty", "-m", "change")
    return root


def check(name, root, want_fail, want_text, args=("--base", "main")):
    global failures
    env = dict(os.environ, GARDENIO_ROOT=root)
    env.pop("GITHUB_HEAD_REF", None)
    p = subprocess.run([sys.executable, GUARD] + list(args), env=env,
                       capture_output=True, text=True)
    out = p.stdout + p.stderr
    ok = (p.returncode != 0) == want_fail and want_text in out
    print("%s  %s" % ("ok  " if ok else "FAIL", name))
    if not ok:
        failures += 1
        print("      rc=%d, wanted %s containing %r:\n%s"
              % (p.returncode, "fail" if want_fail else "pass", want_text, out))


check("no golden path passes",
      repo("feat", ["gardenio/controller/internal/gardenhub/view.go",
                    "gardenio/controller/internal/gardenhub/testdata/golden_browser.js"]),
      False, "OK (2 changed paths vs main, none a golden)")
check("zero changed files passes with a count", repo("feat", []),
      False, "OK (0 changed paths")
check("a pinned font is not a golden",
      repo("feat", [G + "fonts/NotoSans-Regular.ttf"]), False, "OK (1 changed")
check("bless branch passes",
      repo("bless/goldens-202611040330", [G + "bead-phone-light.png"]),
      False, "OK (bless branch bless/goldens-202611040330; 1 of 1")
check("--head-branch names the bless branch",
      repo("feat", [G + "bead-phone-light.png"]), False, "OK (bless branch",
      args=("--base", "main", "--head-branch", "bless/goldens-test"))

check("a changed golden fails",
      repo("feat", [G + "bead-phone-light.png", "x.go"]), True,
      G + "bead-phone-light.png")
check("a new golden fails", repo("feat", [G + "new-phone-dark.png"]), True,
      "changes 1 golden")
check("a deleted golden fails",
      repo("feat", [], deletes=[G + "bead-phone-light.png"]), True,
      "changes 1 golden")
check("a beads-viewer golden fails", repo("feat", [V + "terms-desktop-light.png"]),
      True, V + "terms-desktop-light.png")
check("the failure says how to drop them", repo("feat", [G + "a.png"]), True,
      "git restore --source=main --staged --worktree")
check("a branch merely named bless fails",
      repo("bless-goldens", [G + "bead-phone-light.png"]), True, "FAIL")
check("a diff that cannot run fails, not passes", repo("feat", [G + "a.png"]),
      True, "cannot tell which paths",
      args=("--base", "no-such-ref"))
check("not a git repo fails", tempfile.mkdtemp(), True, "cannot tell which paths")

sys.exit(1 if failures else 0)
