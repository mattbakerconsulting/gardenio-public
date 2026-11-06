#!/usr/bin/env python3
"""Planted controls for check-security-md.py."""
import os
import pathlib
import subprocess
import sys
import tempfile

LINT = pathlib.Path(__file__).resolve().parent / "check-security-md.py"
GOOD = """# Security
## Reporting a vulnerability
## Threat model
## What is closed
## What stays open
Escalations: designed, exercised, enforcement in progress.
"""
cases = {
    "good file": (GOOD, 0),
    "says enforced": (GOOD + "\nEscalations are enforced.\n", 1),
    "missing heading": (GOOD.replace("## What stays open\n", ""), 1),
    "placeholder PR": (GOOD + "\n| x | PR #? |\n", 1),
}
fails = 0
for name, (text, want) in cases.items():
    with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as f:
        f.write(text)
    rc = subprocess.run([sys.executable, "-I", str(LINT), f.name], capture_output=True).returncode
    os.unlink(f.name)
    ok = rc == want
    fails += not ok
    print(("PASS" if ok else "FAIL"), name, rc, want)
sys.exit(1 if fails else 0)
