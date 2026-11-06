#!/usr/bin/env python3
"""Self-test for func-baseline.py: fixture golangci-lint JSON and a fixture
.golangci.yml, so no Go toolchain is needed. When golangci-lint and go are on
PATH it also lints a fixture module with the generated rules, which proves
golangci-lint reads them the way the script writes them."""
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location(
    "fb", os.path.join(HERE, "func-baseline.py"))
fb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fb)

FAIL = 0


def check(name, want, got):
    global FAIL
    if want == got:
        print("PASS", name)
    else:
        print("FAIL", name)
        print("  want:", want)
        print("  got: ", got)
        FAIL = 1


CONFIG = """version: "2"
linters:
  exclusions:
    rules:
      # BEGIN func-baseline
      # END func-baseline
      - path: keep\\.go
        linters: [errcheck]
"""


def issue(linter, path, text):
    return {"FromLinter": linter, "Text": text, "Pos": {"Filename": path}}


def funlen(path, name, n):
    return issue("funlen", path, "Function '%s' is too long (%d > 98)" % (name, n))


def gocyclo(path, name, n):
    return issue("gocyclo", path,
                 "cyclomatic complexity %d of func `%s` is high (> 30)" % (n, name))


def run(module, issues, *flags):
    p = os.path.join(module, "issues.json")
    with open(p, "w") as f:
        json.dump({"Issues": issues}, f)
    return fb.main([module, "--issues", p] + list(flags))


def entries(module):
    with open(os.path.join(module, ".golangci.yml")) as f:
        return [l.strip()[2:] for l in f if fb.ENTRY.match(l)]


# le_regex matches exactly 0..n.
bad = []
for n in (0, 7, 10, 34, 99, 100, 130, 1047, 2000):
    r = re.compile("^%s$" % fb.le_regex(n))
    bad += [(n, i) for i in range(0, 2500) if bool(r.match(str(i))) != (i <= n)]
check("le_regex matches 0..n exactly", [], bad[:5])

check("funlen headroom: 10% or 10 lines", (110, 1047), (fb.headroom("funlen", 100), fb.headroom("funlen", 951)))
check("gocyclo headroom: 10% or 3", (33, 35, 266), (fb.headroom("gocyclo", 30), fb.headroom("gocyclo", 31), fb.headroom("gocyclo", 241)))

tmp = tempfile.mkdtemp()
try:
    with open(os.path.join(tmp, ".golangci.yml"), "w") as f:
        f.write(CONFIG)
    first = [
        funlen("a/b.go", "long", 120),
        funlen(os.path.join(tmp, "a/b.go"), "huge", 951),  # --path-mode abs
        gocyclo("a/b.go", "(*T).twisty", 40),
        gocyclo("c.go", "gone", 35),
    ]
    run(tmp, first, "--init")
    check("init lists every offender with its cap", [
        "funlen a/b.go huge 951 1047",
        "funlen a/b.go long 120 132",
        "gocyclo a/b.go (*T).twisty 40 44",
        "gocyclo c.go gone 35 39",
    ], entries(tmp))
    with open(os.path.join(tmp, ".golangci.yml")) as f:
        text = f.read()
    check("rules outside the block are kept", True, "      - path: keep\\.go\n" in text)
    check("funlen rule text", True,
          "text: '^Function ''long'' is too long \\((?:[0-9]{1,2}|1[0-2][0-9]|13[0-1]|132) > '" in text)
    check("gocyclo rule quotes the receiver", True,
          "of func `\\(\\*T\\)\\.twisty` is high'" in text)
    check("path rule", True, "- path: '^a/b\\.go$'" in text)

    second = [
        funlen("a/b.go", "long", 125),       # grew within its cap: cap stays
        funlen("a/b.go", "huge", 500),       # shrank: cap drops to 550
        gocyclo("a/b.go", "(*T).twisty", 50),  # past its cap: still listed at 44
        funlen("new.go", "fresh", 300),      # new offender: never listed
    ]                                        # c.go gone: dropped
    run(tmp, second)
    check("shrink never adds, never raises, drops the gone", [
        "funlen a/b.go huge 500 550",
        "funlen a/b.go long 125 132",
        "gocyclo a/b.go (*T).twisty 50 44",
    ], entries(tmp))

    with open(os.path.join(tmp, ".golangci.yml"), "w") as f:
        f.write('version: "2"\n')
    try:
        run(tmp, second)
        check("missing markers exits", "SystemExit", "returned")
    except SystemExit:
        check("missing markers exits", "SystemExit", "SystemExit")

    # The rules as golangci-lint reads them: a fixture module, linted for real.
    if shutil.which("golangci-lint") and shutil.which("go"):
        mod = os.path.join(tmp, "mod")
        os.makedirs(mod)
        with open(os.path.join(mod, "go.mod"), "w") as f:
            f.write("module example.com/m\n\ngo 1.22\n")
        body = "".join("\t_ = %d\n" % i for i in range(120))
        with open(os.path.join(mod, "a.go"), "w") as f:
            f.write("package m\n\nfunc listed() {\n%s}\n\nfunc (t *T) grown() {\n%s}\n\ntype T struct{}\n"
                    % (body, body))
        with open(os.path.join(mod, ".golangci.yml"), "w") as f:
            f.write('version: "2"\nlinters:\n  default: none\n  enable: [funlen, gocyclo]\n'
                    '  settings:\n    funlen:\n      lines: 98\n      statements: -1\n'
                    '      ignore-comments: false\n  exclusions:\n    rules:\n'
                    '      # BEGIN func-baseline\n      # END func-baseline\n')
        fb.main([mod, "--init"])
        check("live: both listed", 2, len(entries(mod)))
        with open(os.path.join(mod, "a.go")) as f:
            src = f.read()
        more = "".join("\t_ = %d\n" % i for i in range(20))
        with open(os.path.join(mod, "a.go"), "w") as f:
            f.write(src.replace("func (t *T) grown() {\n", "func (t *T) grown() {\n" + more))
        p = subprocess.run(["golangci-lint", "run", "./..."], cwd=mod,
                           env=dict(os.environ, GOWORK="off"),
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        found = sorted(re.findall(r"Function '(\w+)' is too long", p.stdout))
        check("live: only the func past its cap is reported", ["grown"], found)
    else:
        print("SKIP live golangci-lint fixture (needs go and golangci-lint on PATH)")
finally:
    shutil.rmtree(tmp)

sys.exit(FAIL)
