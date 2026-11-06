#!/usr/bin/env python3
"""run-selftests.py -- run self-test commands N at a time, report each whole.

Reads one shell command per line from stdin (blank lines and `#` comment
lines are skipped), runs each with `bash -c` from the current directory with
stdin closed, and up to --jobs of them at once. As each finishes it prints

    == <command>  (<seconds>s)
    <its stdout and stderr, interleaved, unedited>

as one block, so parallel runs never interleave in the log. At the end it
prints the slowest commands and every failed one, and exits 1 if any command
exited non-zero (2 on a usage error or an empty list).

Every command runs even after one fails: the old serial `set -e` loop stopped
at the first red and hid the rest.

Why: checks.yml's `shell guards + script self-tests` job ran ~95 self-tests
one after another on a light runner and hit its 20-minute timeout on main
with every test green (gd-EkMsUldjkA). The tests are independent (each builds
its own fixtures under mktemp), so they can overlap.

Usage:  python3 scripts/run-selftests.py --jobs 4 <<'EOF'
        bash scripts/foo_test.sh
        python3 scripts/bar_test.py
        EOF
"""
import argparse
import concurrent.futures
import subprocess
import sys
import time


def commands(text):
    out = []
    for line in text.splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            out.append(line)
    return out


def run(cmd):
    t0 = time.monotonic()
    r = subprocess.run(["bash", "-c", cmd], stdin=subprocess.DEVNULL,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    return cmd, r.returncode, time.monotonic() - t0, r.stdout


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--jobs", type=int, default=4)
    args = ap.parse_args()
    if args.jobs < 1:
        print("run-selftests: --jobs must be at least 1", file=sys.stderr)
        return 2
    cmds = commands(sys.stdin.read())
    if not cmds:
        print("run-selftests: no commands on stdin; nothing ran",
              file=sys.stderr)
        return 2

    results = []
    t0 = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(args.jobs) as pool:
        for fut in concurrent.futures.as_completed(
                [pool.submit(run, c) for c in cmds]):
            cmd, rc, secs, out = fut.result()
            results.append((cmd, rc, secs))
            status = "" if rc == 0 else "  FAILED rc=%d" % rc
            sys.stdout.write("== %s  (%.1fs)%s\n" % (cmd, secs, status))
            sys.stdout.flush()
            sys.stdout.buffer.write(out)
            if out and not out.endswith(b"\n"):
                sys.stdout.buffer.write(b"\n")
            sys.stdout.buffer.flush()

    failed = [r for r in results if r[1] != 0]
    print("\nrun-selftests: %d commands, %d failed, %.1fs wall at --jobs %d"
          % (len(results), len(failed), time.monotonic() - t0, args.jobs))
    print("slowest:")
    for cmd, _, secs in sorted(results, key=lambda r: -r[2])[:10]:
        print("  %7.1fs  %s" % (secs, cmd))
    for cmd, rc, _ in failed:
        print("::error::self-test failed (rc=%d): %s" % (rc, cmd))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
