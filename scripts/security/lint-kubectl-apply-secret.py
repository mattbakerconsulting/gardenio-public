#!/usr/bin/env python3
"""Fail when a tracked script or a fenced code block in tracked markdown runs
`kubectl apply` on a Secret. `kubectl apply` stores the whole object, data
included, in the kubectl.kubernetes.io/last-applied-configuration annotation.
Use `kubectl create secret ...` for a new Secret and
`kubectl create secret ... --dry-run=client -o yaml | kubectl replace -f -`
to rotate an existing one (gardenio/docs/runbooks/secrets-create-not-apply.md;
gd-htr9j6R3aB).

Usage: lint-kubectl-apply-secret.py [ROOT]   exit 0 clean, 1 findings.
"""
import pathlib
import re
import subprocess
import sys

APPLY = re.compile(r"\bkubectl\b[^#\n]*\bapply\b")
SECRET_WORD = re.compile(r"(?i)\bsecrets?\b")
COMMENT = re.compile(r"(^|\s)#.*$")
KIND_SECRET = re.compile(r"^\s*kind:\s*Secret\s*$", re.M)
# Plans quote this lint's own source and fixtures, so they would always trip it.
EXEMPT = ("docs/archive/", ".worktrees/", "docs/superpowers/plans/")


def tracked(root):
    out = subprocess.run(["git", "-C", str(root), "ls-files", "*.md", "*.sh"],
                         capture_output=True, text=True, check=True).stdout
    return [p for p in out.splitlines()
            if p and not any(p.startswith(e) or f"/{e}" in p for e in EXEMPT)]


def scan(root, rel):
    try:
        lines = (root / rel).read_text(encoding="utf-8", errors="replace").split("\n")
    except OSError:  # tracked but absent or a dangling symlink
        return []
    md = rel.endswith(".md")
    in_code = not md
    found = []
    for i, line in enumerate(lines):
        if md and line.strip().startswith("```"):
            in_code = not in_code
            continue
        if not in_code:
            continue
        # A shell comment (whole-line or trailing) is prose, not a command.
        code = COMMENT.sub("", line)
        if not APPLY.search(code):
            continue
        window = "\n".join(lines[i:i + 20])
        if SECRET_WORD.search(code) or KIND_SECRET.search(window):
            found.append(f"{rel}:{i + 1}: {line.strip()[:160]}")
    return found


def main():
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    findings = [f for rel in tracked(root) for f in scan(root, rel)]
    for f in findings:
        print(f)
    if findings:
        print(f"lint-kubectl-apply-secret: {len(findings)} finding(s); use kubectl create / replace for Secrets", file=sys.stderr)
        return 1
    print("lint-kubectl-apply-secret: clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
