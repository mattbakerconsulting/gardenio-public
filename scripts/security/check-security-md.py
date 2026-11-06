#!/usr/bin/env python3
"""SECURITY.md guard (P7 Task 8). Fails when the file calls escalations
enforced, drops a required section, or still has a PR placeholder.
Protects gd-0aRM4lYkfY: the public file must not overstate escalation
enforcement (spec D13, owner Q9). It checks prose, not code, so no unit
test can catch the overclaim; it runs on every SECURITY.md edit instead.
Usage: check-security-md.py [PATH]   default SECURITY.md"""
import pathlib
import re
import sys

REQUIRED = ["## Reporting a vulnerability", "## Threat model", "## What is closed", "## What stays open"]
QUALIFIER = "designed, exercised, enforcement in progress"


def main():
    path = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "SECURITY.md")
    text = path.read_text(encoding="utf-8")
    errors = [f"missing section: {h}" for h in REQUIRED if h not in text]
    if QUALIFIER not in text:
        errors.append(f"escalations must be described as '{QUALIFIER}'")
    for i, line in enumerate(text.splitlines(), 1):
        if re.search(r"(?i)\benforced\b", line) and QUALIFIER not in line and "not enforced" not in line.lower():
            errors.append(f"{path}:{i}: says 'enforced': {line.strip()[:120]}")
        if "PR #?" in line:
            errors.append(f"{path}:{i}: PR placeholder left in")
    for e in errors:
        print(e)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
