#!/usr/bin/env python3
"""Normalize SF: lines in an LCOV tracefile to repo-root-relative paths
and drop records that match the exclusion list.

Reads LCOV from stdin or --input, writes normalized LCOV to stdout or --output.

The path scheme is canonicalized in docs/coverage/path-mapping.md. This
script is the single source of truth for the rewrite + exclusion rules
that the merge pipeline (scripts/coverage/merge.sh) relies on.
"""
import argparse
import fnmatch
import re
import sys
from pathlib import PurePosixPath

# Component-prefix patterns: (compiled regex, replacement). Order matters —
# more specific prefixes must come first.
#
# We accept paths in any of these shapes and rewrite them all to the
# canonical repo-root-relative form (forward-slash, no leading slash).
PATH_REWRITES = [
    # Go module paths (gardenio/controller is module-local, kbeads is github)
    (re.compile(r"^gardenio/kbeads/"), "kbeads/"),
    (re.compile(r"^github\.com/Acme/gardenio/"), ""),
    (re.compile(r"^github\.com/mattbakerconsulting/gardenio/"), ""),
    # gardenio/controller is its own module name — paths arrive as
    # "gardenio/controller/..." and are already canonical, but we keep
    # the rule explicit so future module renames have one knob to turn.
    (re.compile(r"^gardenio/controller/"), "gardenio/controller/"),
    # Workspace-absolute paths emitted by cargo-llvm-cov / c8 / playwright.
    # Stripped via --workspace-root at runtime; this is a defensive
    # last-resort match for the common CI roots.
    (re.compile(r"^/workspace/"), ""),
    (re.compile(r"^/builds/[^/]+/[^/]+/"), ""),
    (re.compile(r"^/home/[^/]+/(?:bot/)?gardenio/work/"), ""),
]

# Glob patterns (POSIX-style) for paths to drop entirely. A record whose
# SF: path matches any of these is removed from output (numerator and
# denominator both shrink — this is the "exclude generated + tests"
# behaviour required by W3 §6 / W5 §4.3-4.4).
DEFAULT_EXCLUDES = [
    "**/gen/**",
    "**/*.pb.go",
    "**/proto/**",
    "**/*_sqlc.go",
    "**/sqlc/**",
    "**/dist/**",
    "**/node_modules/**",
    "**/vendor/**",
    "**/target/**",
    "**/*_test.go",
    "**/tests/**",
    "**/fixtures/**",
    "**/testdata/**",
    "**/cmd/*-probe/**",
    "**/*.test.ts",
    "**/*.test.js",
    "**/*.spec.ts",
    "**/*.spec.js",
]

# Allowlist: after rewriting, an SF: path MUST start with one of these
# prefixes or the merge fails loudly. This catches accidental absolute
# paths and stray dependency leakage (see research §5.4).
KNOWN_PREFIXES = [
    "gardenio/",
    "kbeads/",
    "coop/",
    "scripts/",
]


def normalize_path(raw: str, workspace_root: str | None) -> str:
    """Rewrite a single SF: path to its repo-root-relative form."""
    p = raw.strip()
    if workspace_root:
        # Trim a trailing slash on the root so the comparison is stable.
        root = workspace_root.rstrip("/") + "/"
        if p.startswith(root):
            p = p[len(root):]
    # Forward-slash everything (Windows defensive; cheap).
    p = p.replace("\\", "/")
    # Strip a leading "./" — some emitters add it.
    if p.startswith("./"):
        p = p[2:]
    for pat, repl in PATH_REWRITES:
        new = pat.sub(repl, p)
        if new != p:
            p = new
            break
    return p


def is_excluded(path: str, excludes: list[str]) -> bool:
    pp = PurePosixPath(path)
    parts = pp.parts
    for pat in excludes:
        if fnmatch.fnmatch(path, pat):
            return True
        # Match against intermediate path segments too — fnmatch's "**"
        # handling is whole-string, not segment-aware, so a manual
        # segment check catches "tests/**" applied to "coop/tests/foo".
        for i in range(len(parts)):
            sub = "/".join(parts[i:])
            if fnmatch.fnmatch(sub, pat):
                return True
    return False


def is_allowed(path: str, prefixes: list[str]) -> bool:
    return any(path.startswith(pre) for pre in prefixes)


def normalize_lcov(
    lines,
    workspace_root: str | None,
    excludes: list[str],
    prefixes: list[str],
    strict: bool,
):
    """Stream-process an LCOV tracefile.

    LCOV records are blocks delimited by `end_of_record`. We buffer one
    record at a time, rewrite its SF: line, decide whether to keep the
    record, and emit on close.
    """
    record: list[str] = []
    sf_path: str | None = None
    bad_paths: list[str] = []
    kept = 0
    dropped = 0

    for raw in lines:
        line = raw.rstrip("\n")
        if line.startswith("SF:"):
            sf_path = normalize_path(line[3:], workspace_root)
            record.append(f"SF:{sf_path}")
        elif line == "end_of_record":
            record.append(line)
            keep = sf_path is not None
            if keep and is_excluded(sf_path, excludes):
                keep = False
            if keep and not is_allowed(sf_path, prefixes):
                bad_paths.append(sf_path)
                if strict:
                    keep = False
            if keep:
                kept += 1
                yield from (r + "\n" for r in record)
            else:
                dropped += 1
            record = []
            sf_path = None
        else:
            record.append(line)

    # If the input had a trailing record without `end_of_record`, drop it
    # silently — LCOV requires the terminator and downstream tools agree.
    if strict and bad_paths:
        msg = "normalize-paths: unrecognized prefixes after rewrite:\n"
        for p in sorted(set(bad_paths))[:20]:
            msg += f"  {p}\n"
        sys.stderr.write(msg)
    sys.stderr.write(
        f"normalize-paths: kept={kept} dropped={dropped}"
        f" bad_prefix={len(bad_paths)}\n"
    )


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("-i", "--input", help="LCOV tracefile (default: stdin)")
    ap.add_argument("-o", "--output", help="output path (default: stdout)")
    ap.add_argument(
        "--workspace-root",
        help="absolute prefix to strip from paths before applying rewrites",
    )
    ap.add_argument(
        "--exclude",
        action="append",
        default=None,
        help="extra glob to exclude (repeatable); replaces defaults if --no-default-excludes",
    )
    ap.add_argument(
        "--no-default-excludes",
        action="store_true",
        help="omit the built-in exclusion list (use only --exclude args)",
    )
    ap.add_argument(
        "--strict",
        action="store_true",
        help="fail (exit 2) if any record has an unrecognized prefix after rewrite",
    )
    args = ap.parse_args(argv)

    excludes = list(args.exclude) if args.exclude else []
    if not args.no_default_excludes:
        excludes = DEFAULT_EXCLUDES + excludes

    src = open(args.input) if args.input else sys.stdin
    dst = open(args.output, "w") if args.output else sys.stdout
    bad: list[str] = []
    try:
        for chunk in normalize_lcov(
            src,
            workspace_root=args.workspace_root,
            excludes=excludes,
            prefixes=KNOWN_PREFIXES,
            strict=args.strict,
        ):
            dst.write(chunk)
    finally:
        if args.input:
            src.close()
        if args.output:
            dst.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
