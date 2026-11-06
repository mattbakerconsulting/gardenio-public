#!/usr/bin/env python3
"""check-ci-required-checks_test.py -- unit tests for check-ci-required-checks.py

Each case renders a synthetic .github/workflows/ in a tmpdir and asserts the
guard's verdict; GARDENIO_ROOT points the guard at it. The aggregator job in
every synthetic file is the REAL `ci` job cut from ci.yml with its `needs:`
swapped, so the script the guard executes is the one CI runs.

The POSITIVE CONTROL comes first: a compliant pair passes, and says how many
needs it read and how many fixtures it ran (a parser that read nothing would
otherwise pass in the same words). Then one case per way to lose the
required check, then the real tree.

Usage:  python3 scripts/check-ci-required-checks_test.py
"""
import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, "check-ci-required-checks.py")
REAL_CI = os.path.join(os.path.dirname(HERE), ".github", "workflows", "ci.yml")

PASS = FAIL = 0


def real_aggregator():
    """The `ci` job block from the real ci.yml, comments dropped."""
    lines = open(REAL_CI).read().split("\n")
    start = lines.index("  ci:")
    out = [lines[start]]
    for line in lines[start + 1:]:
        if line.strip() and not line.lstrip().startswith("#") and \
                len(line) - len(line.lstrip()) <= 2:
            break
        out.append(line)
    return "\n".join(out).rstrip() + "\n"


AGG = real_aggregator()


def aggregator(job, needs):
    text = AGG.replace("  ci:\n", "  %s:\n" % job, 1)
    text = text.replace("    name: ci\n", "    name: %s\n" % job, 1)
    text = text.replace("does not fail ci", "does not fail %s" % job)
    m = re.search(r"    needs:\n(?:      - .*\n)+", text)
    assert m, "the real `ci` job has no block-list `needs:`"
    return (text[:m.start()] + "    needs:\n" +
            "".join("      - %s\n" % n for n in needs) + text[m.end():])


def job(job_id, name=None, extra=""):
    out = "  %s:\n" % job_id
    if name is not None:
        out += "    name: %s\n" % name
    out += extra
    out += "    runs-on: gardenio-k3s\n    steps:\n      - run: \"true\"\n"
    return out


def workflow(name, jobs):
    return "name: %s\n\non:\n  push:\n\njobs:\n%s" % (name, "\n".join(jobs))


def ci_wf(agg=None, extra_jobs=()):
    jobs = [job("gate", "gate"), job("go", "build + test (${{ matrix.m }})"),
            job("rust", "build + test (coop)")] + list(extra_jobs)
    ids = [re.match(r"  (\S+):", j).group(1) for j in jobs]
    return workflow("ci", jobs + [agg if agg is not None
                                  else aggregator("ci", ids)])


def checks_wf(agg=None):
    jobs = [job("supersession", "superseded by a newer push?"),
            job("lint-go", "golangci-lint"),
            job("deferred", "deferred", extra="    if: false\n")]
    ids = [re.match(r"  (\S+):", j).group(1) for j in jobs]
    return workflow("checks", jobs + [agg if agg is not None
                                      else aggregator("checks", ids)])


def default_set():
    return {"ci.yml": ci_wf(), "checks.yml": checks_wf(),
            "images.yml": workflow("images", [job("build", "build ${{ matrix.image }}")])}


def with_file(fname, text):
    s = default_set()
    s[fname] = text
    return s


def mutate(old, new, fname="ci.yml"):
    s = default_set()
    assert old in s[fname], old
    s[fname] = s[fname].replace(old, new, 1)
    return s


def check(name, want_hit, want_substr=None, files=None, real_tree=False):
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix="ccrc-test-")
    try:
        env = dict(os.environ)
        if real_tree:
            env.pop("GARDENIO_ROOT", None)
        else:
            wf_dir = os.path.join(root, ".github", "workflows")
            os.makedirs(wf_dir)
            for fname, text in (files or {}).items():
                with open(os.path.join(wf_dir, fname), "w") as f:
                    f.write(text)
            env["GARDENIO_ROOT"] = root
        p = subprocess.run([sys.executable, GUARD], capture_output=True,
                           text=True, env=env)
        out = p.stdout + p.stderr
        ok = (p.returncode != 0) == want_hit
        if ok and want_substr and want_substr not in out:
            ok = False
            out += "\n(expected output to contain %r)" % want_substr
        if ok:
            print("PASS: %s" % name)
            PASS += 1
        else:
            print("FAIL: %s: expected %s, got rc=%d\n%s"
                  % (name, "a hit" if want_hit else "no hit", p.returncode, out))
            FAIL += 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


# ── positive control ───────────────────────────────────────────────────────
check("compliant pair passes and counts what it read", False,
      "`ci` needs 3 jobs, 7 fixtures run; `checks` needs 3 jobs, 7 fixtures "
      "run; 3 workflows", files=default_set())

check("a matrix-named job elsewhere is not a collision", False,
      files=with_file("e2e.yml", workflow("e2e", [
          job("suite", "${{ matrix.suite }}")])))

check("a job with continue-on-error listed in ADVISORY passes", False,
      files=with_file("ci.yml", ci_wf().replace(
          "  rust:\n", "  rust:\n    continue-on-error: true\n", 1)
          .replace('ADVISORY: ""', 'ADVISORY: "rust"', 1)))

# ── 1. the job ─────────────────────────────────────────────────────────────
check("no aggregator job", True, "no `ci` job",
      files=with_file("ci.yml", workflow("ci", [job("gate", "gate")])))

check("aggregator file missing", True, "checks.yml: does not exist",
      files={"ci.yml": ci_wf()})

# ── 2. the name ────────────────────────────────────────────────────────────
check("the pre-part-B draft-split name (a draft run never reports `ci`)",
      True, "on a draft pull_request run",
      files=mutate("    name: ci\n",
                   "    name: ${{ github.event.pull_request.draft && "
                   "'ci (draft)' || 'ci' }}\n"))

check("a literal name that is not the check", True, "'CI suite'",
      files=mutate("    name: ci\n", "    name: CI suite\n"))

check("an expression that is `ci` on every run passes", False,
      files=mutate("    name: ci\n",
                   "    name: ${{ github.event_name && 'ci' || 'ci' }}\n"))

check("name expression that misnames the ready run", True,
      "on a ready pull_request run",
      files=mutate("    name: ci\n",
                   "    name: ${{ github.event.pull_request.draft == false && "
                   "'CI suite' || 'ci' }}\n"))

check("name expression the evaluator cannot read", True, "cannot be evaluated",
      files=mutate("    name: ci\n",
                   "    name: ${{ inputs.draft && 'x' || 'ci' }}\n"))

# ── 3. the if ──────────────────────────────────────────────────────────────
check("`if: !cancelled()` skips it on a cancelled run", True,
      "a skipped check satisfies a required one",
      files=mutate("    if: always()", "    if: ${{ !cancelled() }}"))

check("no `if:` at all", True, "is None, not `always()`",
      files=mutate("    if: always()\n", ""))

# ── 4. needs ───────────────────────────────────────────────────────────────
check("a job missing from needs", True, "`needs:` lacks rust",
      files=mutate("      - rust\n", ""))

check("an `if: false` job missing from needs", True, "`needs:` lacks deferred",
      files=mutate("      - deferred\n", "", "checks.yml"))

check("needs names a job that does not exist", True,
      "names ghost, which ci.yml does not define",
      files=mutate("      - rust\n", "      - rust\n      - ghost\n"))

check("needs lists a job twice", True, "lists gate more than once",
      files=mutate("      - rust\n", "      - rust\n      - gate\n"))

# ── 5. the step and its env ────────────────────────────────────────────────
check("a second step", True, "has 2 steps",
      files=mutate("      - name: every job passed or skipped\n",
                   "      - run: echo hi\n"
                   "      - name: every job passed or skipped\n"))

check("NEEDS reads one job, not all of needs", True, "must set `NEEDS:",
      files=mutate("${{ toJSON(needs) }}", "${{ toJSON(needs.gate) }}"))

check("no ADVISORY env", True, "has no `ADVISORY` env",
      files=mutate('          ADVISORY: ""\n', ""))

check("a continue-on-error job not in ADVISORY", True,
      "rust carries job-level `continue-on-error`",
      files=mutate("  rust:\n", "  rust:\n    continue-on-error: true\n"))

check("ADVISORY lists a job that is not continue-on-error", True,
      "ADVISORY lists go, which carries no job-level",
      files=mutate('ADVISORY: ""', 'ADVISORY: "go"'))

# ── 6. the script, executed ────────────────────────────────────────────────
check("a script that passes everything", True,
      "on fixture 'gate failure'",
      files=mutate("          set -euo pipefail\n",
                   "          set -euo pipefail\n          exit 0\n"))

check("a jq filter that treats cancelled as fine", True,
      "on fixture 'gate cancelled'",
      files=mutate('.value.result != "skipped"',
                   '.value.result != "skipped" and .value.result != "cancelled"'))

check("a script that judges `{}` green", True, "on fixture 'no needs at all",
      files=mutate('if [ "$n" -eq 0 ]; then', 'if [ "$n" -lt 0 ]; then'))

check("a script that ignores ADVISORY", True,
      "on fixture 'ADVISORY gate failure'",
      files=mutate("--arg adv \"$ADVISORY\"", "--arg adv \"\""))

# ── collisions in any workflow ─────────────────────────────────────────────
check("another workflow's job literally named `checks`", True,
      "e2e.yml: job suite reports a check named 'checks'",
      files=with_file("e2e.yml", workflow("e2e", [job("suite", "checks")])))

check("a job id `ci` with no name in another workflow", True,
      "images.yml: job ci reports a check named 'ci'",
      files=with_file("images.yml", workflow("images", [job("ci")])))

check("a name expression elsewhere that evaluates to `ci`", True,
      "reports a check named 'ci'",
      files=with_file("e2e.yml", workflow("e2e", [job(
          "suite", "${{ github.event_name == 'push' && 'ci' || 'e2e' }}")])))

check("a second `ci` inside ci.yml", True,
      "ci.yml: job rust reports a check named 'CI'",
      files=mutate("    name: build + test (coop)\n", "    name: CI\n"))

# ── the real tree ──────────────────────────────────────────────────────────
check("REAL tree: ci.yml `ci` and checks.yml `checks` hold", False,
      "check-ci-required-checks: OK", real_tree=True)

print("\ncheck-ci-required-checks_test: %d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
