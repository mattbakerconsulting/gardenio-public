#!/usr/bin/env python3
"""Planted positive controls for lint-kubectl-apply-secret.py."""
import pathlib
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
LINT = HERE / "lint-kubectl-apply-secret.py"


def run(root):
    p = subprocess.run([sys.executable, "-I", str(LINT), str(root)], capture_output=True, text=True)
    return p.returncode, p.stdout


def make_repo(files):
    d = tempfile.mkdtemp()
    root = pathlib.Path(d)
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    for rel, text in files.items():
        f = root / rel
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(text)
    subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
    return root


failures = 0
cases = {
    "pipe into apply (the classic)": ({"docs/a.md": "```bash\nkubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml | kubectl apply -f -\n```\n"}, 1),
    "apply a secret file": ({"scripts/b.sh": "#!/bin/sh\nkubectl -n gardenio apply -f my-secret.yaml\n"}, 1),
    "heredoc with kind: Secret": ({"docs/c.md": "```\nkubectl apply -f - <<EOF\napiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nEOF\n```\n"}, 1),
    "prose mention is fine": ({"docs/d.md": "Never use `kubectl apply` for a Secret.\n"}, 0),
    "create is fine": ({"docs/e.md": "```\nkubectl create secret generic x --from-literal=a=b\n```\n"}, 0),
    "replace pipe is fine": ({"docs/f.md": "```\nkubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml | kubectl replace -f -\n```\n"}, 0),
    "apply of a non-secret is fine": ({"docs/g.md": "```\nkubectl apply -f deployment.yaml\n```\n"}, 0),
    "nested archive is exempt": ({"gardenio/docs/archive/i.md": "```\nkubectl apply -f my-secret.yaml\n```\n"}, 0),
    "comment-only mention in .sh is fine": ({"scripts/i.sh": "#!/bin/sh\n# never run kubectl apply on a secret\ntrue\n"}, 0),
    "trailing comment mention is fine": ({"scripts/j.sh": "kubectl -n x edit secret y  # not kubectl apply -f secret.yaml\n"}, 0),
    "comment in fenced bash block is fine": ({"docs/k.md": "```bash\n# kubectl apply -f my-secret.yaml is forbidden\nkubectl get secret x\n```\n"}, 0),
    "archive is exempt": ({"docs/archive/h.md": "```\nkubectl apply -f my-secret.yaml\n```\n"}, 0),
}
for name, (files, want) in cases.items():
    rc, out = run(make_repo(files))
    status = "PASS" if rc == want else "FAIL"
    if status == "FAIL":
        failures += 1
    print(f"{status} {name}: rc={rc} want={want} {out.strip()[:120]}")
sys.exit(1 if failures else 0)
