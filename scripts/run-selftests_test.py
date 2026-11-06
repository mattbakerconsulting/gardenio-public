#!/usr/bin/env python3
"""run-selftests_test.py -- unit tests for run-selftests.py

The runner replaced a serial `set -e` loop in checks.yml's script-selftests
job (gd-EkMsUldjkA). The way it can go wrong silently is a runner that loses
a failure, so the failing cases come first: one red command among greens must
exit 1 and be named, and the commands after it must still run. Then: the
list's comments are skipped, an empty list is an error rather than a green,
a command reading stdin gets EOF instead of the list, each command's output
is one contiguous block, and two commands really overlap (a rendezvous, not a
timing threshold). The last cases check checks.yml still pipes its list into
the runner and lists this test.

Usage:  python3 scripts/run-selftests_test.py
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
RUNNER = os.path.join(HERE, "run-selftests.py")
CHECKS = os.path.join(HERE, os.pardir, ".github", "workflows", "checks.yml")

PASS = FAIL = 0


def record(name, ok, detail=""):
    global PASS, FAIL
    if ok:
        PASS += 1
        print("PASS: %s" % name)
    else:
        FAIL += 1
        print("FAIL: %s\n%s" % (name, detail))


def run(stdin, jobs=4):
    r = subprocess.run([sys.executable, RUNNER, "--jobs", str(jobs)],
                       input=stdin, stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True, timeout=120)
    return r.returncode, r.stdout


tmp = tempfile.mkdtemp(prefix="run-selftests-test-")

rc, out = run("true\nexit 3\necho after-the-red > %s/after\n" % tmp, jobs=1)
record("one red command among greens exits 1", rc == 1, out)
record("the red command is named with its exit code",
       "self-test failed (rc=3): exit 3" in out, out)
record("commands after the red one still run",
       os.path.exists(os.path.join(tmp, "after")), out)

rc, out = run("true\necho ok\n")
record("all green exits 0", rc == 0, out)
record("summary counts what ran", "2 commands, 0 failed" in out, out)

rc, out = run("# a comment\n\n   # indented comment\necho only-this\n")
record("comment and blank lines are skipped",
       rc == 0 and "1 commands, 0 failed" in out, out)

rc, out = run("# nothing but comments\n\n")
record("an empty list is an error, not a green", rc == 2, out)

rc, out = run("read -r line && echo \"stole=$line\"; true\n"
              "echo second-command\n", jobs=1)
record("a command reading stdin gets EOF, not the list",
       rc == 0 and "stole=echo" not in out and "2 commands" in out, out)

rc, out = run("for i in 1 2 3 4 5; do echo A$i; sleep 0.05; done\n"
              "for i in 1 2 3 4 5; do echo B$i; sleep 0.05; done\n", jobs=2)
lines = [l for l in out.splitlines() if l[:1] in ("A", "B") and l[1:].isdigit()]
contiguous = (lines in (["A%d" % i for i in range(1, 6)] +
                        ["B%d" % i for i in range(1, 6)],
                        ["B%d" % i for i in range(1, 6)] +
                        ["A%d" % i for i in range(1, 6)]))
record("each command's output is one contiguous block", contiguous, out)

# Each side waits for the other's file: only finishes if both run at once.
a, b = os.path.join(tmp, "a"), os.path.join(tmp, "b")
wait = "for i in $(seq 300); do [ -e %s ] && exit 0; sleep 0.1; done; exit 9"
rc, out = run("touch %s; %s\ntouch %s; %s\n"
              % (a, wait % b, b, wait % a), jobs=2)
record("--jobs 2 runs two commands at the same time", rc == 0, out)

rc, out = run("true\n", jobs=0)
record("--jobs 0 is a usage error", rc == 2, out)

try:
    with open(CHECKS, encoding="utf-8") as fh:
        src = fh.read()
except OSError as e:
    src = ""
    CHECKS = "%s (%s)" % (CHECKS, e)
record("checks.yml pipes its self-test list into the runner",
       "python3 scripts/run-selftests.py --jobs" in src,
       "no `python3 scripts/run-selftests.py --jobs` in %s" % CHECKS)
record("this self-test is in the script self-tests list",
       "python3 scripts/run-selftests_test.py" in src,
       "add `python3 scripts/run-selftests_test.py` to %s" % CHECKS)

subprocess.run(["rm", "-rf", tmp])
print("\nrun-selftests_test: %d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
