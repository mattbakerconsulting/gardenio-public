#!/usr/bin/env python3
"""func-baseline.py -- the funlen/gocyclo baseline in a module's .golangci.yml
(gd-d29n0m678l).

golangci-lint holds every Go func in gardenio/controller, kbeads and
gardenio/clicreds to the caps of gardenio/docs/code-reduction-targets.md:
funlen at 98 body lines (over 100 lines from `func` to the closing brace,
code-size.sh's "long") and gocyclo over 30 ("complex"). The funcs over a cap
when the ratchet landed are listed in the config as exclusion rules between

    # BEGIN func-baseline ...
    # END func-baseline

and this script writes that block. Each rule names one func in one file and
lets it reach a CAP, its count when listed plus headroom: max(10%, 10 lines)
for funlen, max(10%, 3) for gocyclo, so a feature PR can still touch a listed
func. A listed func past its cap, and any unlisted func past the linter's
limit, fail golangci-lint as usual.

THE LIST MAY ONLY SHRINK. The default (shrink) mode re-measures every listed
func, drops the ones that are under the limit or gone, and lowers each cap to
min(cap, count + headroom). It never adds a func and never raises a cap.
`--init` rebuilds the list from scratch; it is for the first baseline only.

Usage:
  func-baseline.py <module-dir> [--init]
  func-baseline.py <module-dir> [--init] --issues <golangci.json>

Without --issues it runs golangci-lint ($GOLANGCI_LINT, default
golangci-lint) in <module-dir> with only funlen and gocyclo, on a copy of the
config whose block is emptied, so the listed funcs are measured too. That run
is syntax-only (no type check) and takes seconds even for the controller.

Each rule carries a comment line the script reads back:
    # <linter> <path> <func> <count> <cap>
"""
import json
import os
import re
import subprocess
import sys
import tempfile

BEGIN = "# BEGIN func-baseline"
END = "# END func-baseline"
HEADER = ("# BEGIN func-baseline (scripts/ratchets/func-baseline.py writes "
          "this block; it may only shrink)")
LINTERS = ("funlen", "gocyclo")
FUNLEN = re.compile(r"^Function '([^']+)' is too long \((\d+) > \d+\)")
GOCYCLO = re.compile(r"^cyclomatic complexity (\d+) of func `([^`]+)` is high")
ENTRY = re.compile(r"^\s*# (funlen|gocyclo) (\S+) (\S+) (\d+) (\d+)\s*$")


def headroom(linter, n):
    floor = 10 if linter == "funlen" else 3
    return n + max(-(-n // 10), floor)


def le_regex(n):
    """A regex for a decimal integer from 0 to n (no leading zeros)."""
    s = str(n)
    alts = []
    if len(s) > 1:
        alts.append("[0-9]" if len(s) == 2 else "[0-9]{1,%d}" % (len(s) - 1))
    for i, ch in enumerate(s):
        d = int(ch)
        lo = 1 if i == 0 and len(s) > 1 else 0
        if d - 1 >= lo:
            rng = "%d" % lo if d - 1 == lo else "[%d-%d]" % (lo, d - 1)
            rest = len(s) - i - 1
            tail = "" if rest == 0 else ("[0-9]" if rest == 1 else "[0-9]{%d}" % rest)
            alts.append(s[:i] + rng + tail)
    alts.append(s)
    return "(?:%s)" % "|".join(alts)


def go_quote(s):
    """Escape a literal for a Go regexp (RE2 accepts \\ before punctuation)."""
    return re.sub(r"([\\.+*?()|\[\]{}^$])", r"\\\1", s)


def yaml_sq(s):
    return "'%s'" % s.replace("'", "''")


def rule(indent, linter, path, name, count, cap):
    if linter == "funlen":
        text = "^Function '%s' is too long \\(%s > " % (go_quote(name), le_regex(cap))
    else:
        text = "^cyclomatic complexity %s of func `%s` is high" % (le_regex(cap), go_quote(name))
    p = " " * indent
    return [
        "%s# %s %s %s %d %d" % (p, linter, path, name, count, cap),
        "%s- path: %s" % (p, yaml_sq("^%s$" % go_quote(path))),
        "%s  linters: [%s]" % (p, linter),
        "%s  text: %s" % (p, yaml_sq(text)),
    ]


def split_block(lines, cfg):
    b = [i for i, l in enumerate(lines) if l.strip().startswith(BEGIN)]
    e = [i for i, l in enumerate(lines) if l.strip().startswith(END)]
    if len(b) != 1 or len(e) != 1 or e[0] < b[0]:
        sys.exit("%s: want exactly one '%s' line and one '%s' line after it"
                 % (cfg, BEGIN, END))
    return b[0], e[0]


def measured(issues, module):
    """{(linter, path, name): count} from golangci-lint JSON."""
    out = {}
    for it in issues.get("Issues") or []:
        linter = it.get("FromLinter")
        if linter not in LINTERS:
            continue
        text = it.get("Text", "")
        if linter == "funlen":
            m = FUNLEN.match(text)
            name, n = (m.group(1), int(m.group(2))) if m else (None, 0)
        else:
            m = GOCYCLO.match(text)
            name, n = (m.group(2), int(m.group(1))) if m else (None, 0)
        if not name:
            sys.exit("unrecognised %s message: %r" % (linter, text))
        path = it["Pos"]["Filename"]
        if os.path.isabs(path):
            path = os.path.relpath(path, module)
        key = (linter, path.replace(os.sep, "/"), name)
        out[key] = max(out.get(key, 0), n)
    return out


def run_golangci(module, cfg_lines, b, e):
    """Lint with the block emptied; return the parsed JSON."""
    fd, tmp_cfg = tempfile.mkstemp(prefix=".golangci-func-baseline-",
                                   suffix=".yml", dir=module)
    out_json = tmp_cfg + ".json"
    try:
        with os.fdopen(fd, "w") as f:
            f.write("\n".join(cfg_lines[:b + 1] + cfg_lines[e:]) + "\n")
        env = dict(os.environ, GOWORK="off")
        cmd = [os.environ.get("GOLANGCI_LINT", "golangci-lint"), "run",
               "-c", tmp_cfg, "--enable-only", ",".join(LINTERS),
               "--uniq-by-line=false", "--max-issues-per-linter", "0",
               "--max-same-issues", "0", "--path-mode", "abs",
               "--output.json.path", out_json, "--output.text.path", "stderr",
               "./..."]
        p = subprocess.run(cmd, cwd=module, env=env, stdout=subprocess.DEVNULL,
                           stderr=subprocess.PIPE, text=True)
        # golangci-lint exits 1 when it found issues, which is expected here.
        if p.returncode not in (0, 1) or not os.path.exists(out_json):
            sys.stderr.write(p.stderr)
            sys.exit("golangci-lint failed (exit %d)" % p.returncode)
        with open(out_json) as f:
            return json.load(f)
    finally:
        for f in (tmp_cfg, out_json):
            if os.path.exists(f):
                os.unlink(f)


def main(argv):
    args = list(argv)
    init = "--init" in args
    if init:
        args.remove("--init")
    issues_path = None
    if "--issues" in args:
        i = args.index("--issues")
        issues_path = args[i + 1]
        del args[i:i + 2]
    if len(args) != 1:
        sys.exit("usage: func-baseline.py <module-dir> [--init] "
                 "[--issues <golangci.json>]")
    module = os.path.abspath(args[0])
    cfg = os.path.join(module, ".golangci.yml")
    with open(cfg) as f:
        lines = f.read().split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    b, e = split_block(lines, cfg)
    indent = len(lines[b]) - len(lines[b].lstrip())

    old = {}
    for l in lines[b + 1:e]:
        m = ENTRY.match(l)
        if m:
            old[(m.group(1), m.group(2), m.group(3))] = int(m.group(5))

    if issues_path:
        with open(issues_path) as f:
            issues = json.load(f)
    else:
        issues = run_golangci(module, lines, b, e)
    now = measured(issues, module)

    entries = {}
    for key, n in now.items():
        cap = headroom(key[0], n)
        if not init:
            if key not in old:
                continue  # a new offender: the lint reports it, never listed here
            cap = min(cap, old[key])
        entries[key] = (n, cap)

    block = [" " * indent + HEADER]
    for key in sorted(entries):
        block += rule(indent, key[0], key[1], key[2], *entries[key])
    block.append(" " * indent + END)
    with open(cfg, "w") as f:
        f.write("\n".join(lines[:b] + block + lines[e + 1:]) + "\n")

    dropped = len(set(old) - set(entries)) if not init else 0
    new = sorted(set(now) - set(entries))
    print("%s: %d funcs listed (%d funlen, %d gocyclo), %d dropped"
          % (os.path.join(args[0], ".golangci.yml"), len(entries),
             sum(1 for k in entries if k[0] == "funlen"),
             sum(1 for k in entries if k[0] == "gocyclo"), dropped))
    for k in new:
        print("  not listed, golangci-lint reports it: %s %s %s (%d)"
              % (k[0], k[1], k[2], now[k]))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
