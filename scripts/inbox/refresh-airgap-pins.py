#!/usr/bin/env python3
"""refresh-airgap-pins.py — keep the in-box air-gap first-party digest pins in
gardenio/helm/gardenio/values-inbox.yaml equal to the images the current
Chart.AppVersion actually ships.

WHY THIS EXISTS (ka-0000004971)
-------------------------------
Under `global.airgap=true` the pinned digest in values-inbox.yaml WINS over the
floated Chart.AppVersion tag (gardenio.imageRef renders "<repo>:<tag>@<digest>",
digest binding at pull time). Those pins were refreshed BY HAND per air-gap
bundle cut and silently rot between releases.

Concrete failure this caused (ka-0000000443): pins frozen at appVersion
2026.161.4 (2026-06-10) shipped a June-10 beads daemon on chart 2026.238.7. The
chart's beads probe path changed to /v1/health/live but the pinned old daemon
401s that path -> CrashLoop -> tarboat beads_data + acceptance gates red. ~2.5
months of undetected drift.

WHAT IT DOES
------------
For every first-party image block in values-inbox.yaml, resolve the digest the
current Chart.AppVersion tag points at in the source registry, then either:

  --check  (default) : FAIL (exit 1) listing every pin that != the registry
                       digest for Chart.AppVersion. This is the CI guard — it
                       reds any MR whose values-inbox.yaml pins have drifted
                       from the chart appVersion, so the drift is caught at
                       merge time instead of on an air-gapped box.

  --write            : rewrite each drifted pin in place (approach (a) from the
                       bead — the auto-refresh the bundle cutter runs so the
                       manual step disappears). Surgical, comment-preserving:
                       only the "sha256:…" value on each digest line changes;
                       every other byte (blank lines, comments, ordering) is
                       untouched.

Digest resolution shells out to `crane digest <registry>/<component>:<tag>` —
crane is already installed in the bundle/promote CI tasks and needs only the
registry read auth those tasks already set up (`crane auth login`). In tests the
external `crane` is mocked on PATH (see refresh-airgap-pins_test.py), matching
the sibling script tests under scripts/.

Golden promotion is digest-preserving (the bare "<appVersion>" tag points at the
same manifest as "<appVersion>-rc"), so resolving the bare tag yields the value
the release ships.

Usage:
  scripts/inbox/refresh-airgap-pins.py [--check | --write] [options]

Options:
  --check                  verify pins == registry (default; exit 1 on drift)
  --write                  rewrite drifted pins in place
  --values PATH            values-inbox.yaml (default: derived from repo layout)
  --chart PATH             Chart.yaml to read appVersion (default: derived)
  --tag TAG                override the tag to resolve (default: Chart.AppVersion)
  --registry REG           image registry base
                           (default: registry.example.com/gardenio)
  --crane CMD              crane invocation (default: "crane")
"""
import argparse
import os
import subprocess
import sys

# Repo-relative default paths (this file lives at scripts/inbox/).
_SELF_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.abspath(os.path.join(_SELF_DIR, "..", ".."))
DEFAULT_VALUES = os.path.join(
    _REPO_ROOT, "gardenio", "helm", "gardenio", "values-inbox.yaml"
)
DEFAULT_CHART = os.path.join(_REPO_ROOT, "gardenio", "helm", "gardenio", "Chart.yaml")
DEFAULT_REGISTRY = "registry.example.com/gardenio"

# Parent key-path in values-inbox.yaml -> registry component (image repo) slug.
# The digest under each parent is the pin for that component's image. The set
# and count (11) MUST stay in lockstep with the first-party digest blocks in
# values-inbox.yaml; a new first-party image block needs a row here (and the
# --check guard fails loud below if the file carries a digest we don't map).
PARENT_TO_COMPONENT = {
    "agents.image": "controller",
    "agents.agentImage": "agent",
    "coopmux.image": "coopmux",
    "beads.image": "kbeads",
    "beadsViewer.image": "beads-viewer",
    "slackBridge.image": "slack-bridge",
    "jiraBridge.image": "jira-bridge",
    "gitlabBridge.image": "gitlab-bridge",
    "triggerEngine.image": "trigger-engine",
    "edgeAuth.image": "edge-auth",
}


class PinError(Exception):
    """A fatal, operator-actionable error (bad file / unmapped pin / crane)."""


def read_app_version(chart_path):
    """Return the appVersion from Chart.yaml (a flat top-level scalar)."""
    with open(chart_path, encoding="utf-8") as fh:
        for line in fh:
            stripped = line.strip()
            if stripped.startswith("appVersion:"):
                val = stripped.split(":", 1)[1].strip()
                # tolerate quotes: appVersion: "2026.238.7" | 2026.238.7
                return val.strip('"').strip("'")
    raise PinError("no appVersion found in %s" % chart_path)


def _key_of(line):
    """If `line` is a `key:` mapping line, return (indent, key); else None.

    A mapping line is `<spaces><key>:` optionally followed by an inline value.
    Comment (`#…`) and list (`- …`) lines return None so they never enter the
    parent path. Indentation is the file's native 2-space; we track raw column
    count so the exact step size does not matter.
    """
    stripped = line.lstrip(" ")
    if not stripped or stripped.startswith("#") or stripped.startswith("-"):
        return None
    indent = len(line) - len(stripped)
    # a key line has an unquoted `key:` head (keys here are plain identifiers)
    head = stripped.split(":", 1)
    if len(head) != 2:
        return None
    key = head[0]
    if not key or any(c in key for c in ' "\t'):
        return None
    return indent, key


def find_pins(lines):
    """Locate every `digest:` pin and its parent key-path.

    Returns a list of dicts: {"index": <0-based line no>, "path": "a.b",
    "indent": <col>, "value": "sha256:…"}. Uses an indentation stack so a
    `digest:` line is attributed to its enclosing mapping (e.g. global.image),
    with comments and blank lines preserved verbatim (they are simply skipped
    for path purposes).
    """
    pins = []
    # stack of (indent, key) for the currently-open mapping ancestors
    stack = []
    for i, line in enumerate(lines):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        keyed = _key_of(line)
        if keyed is None:
            continue
        indent, key = keyed
        # pop ancestors at or below this indent (we've left them)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        if key == "digest":
            path = ".".join(k for _, k in stack)
            # value after the colon, de-quoted
            raw = line.split(":", 1)[1].strip()
            value = raw.strip('"').strip("'")
            pins.append(
                {"index": i, "path": path, "indent": indent, "value": value}
            )
            # a digest is a leaf; don't push it as an ancestor
            continue
        stack.append((indent, key))
    return pins


def resolve_digest(crane_cmd, registry, component, tag):
    """Return the manifest digest (sha256:…) for <registry>/<component>:<tag>.

    Shells out to `crane digest`. Raises PinError with the crane stderr on any
    failure so a missing tag / auth problem is loud and actionable rather than
    silently pinning a stale value.
    """
    ref = "%s/%s:%s" % (registry, component, tag)
    argv = crane_cmd + ["digest", ref]
    try:
        proc = subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            text=True,
        )
    except FileNotFoundError as exc:
        raise PinError("crane not found (%s): %s" % (crane_cmd[0], exc))
    if proc.returncode != 0:
        raise PinError(
            "crane digest %s failed (exit %d): %s"
            % (ref, proc.returncode, proc.stderr.strip())
        )
    digest = proc.stdout.strip()
    if not digest.startswith("sha256:"):
        raise PinError("crane digest %s returned junk: %r" % (ref, digest))
    return digest


def rewrite_line(line, new_value):
    """Return `line` with its digest value replaced, preserving indentation,
    the quoting style, and any trailing comment."""
    indent = line[: len(line) - len(line.lstrip(" "))]
    # split off a trailing comment (only outside the quoted value; our digests
    # never contain '#', so a simple split on ' #' is safe here)
    body = line.rstrip("\n")
    comment = ""
    if " #" in body:
        body, comment = body.split(" #", 1)
        comment = " #" + comment
    # detect quote style from the existing value
    after = body.split(":", 1)[1].strip()
    if after.startswith('"'):
        rendered = '"%s"' % new_value
    elif after.startswith("'"):
        rendered = "'%s'" % new_value
    else:
        rendered = new_value
    nl = "\n" if line.endswith("\n") else ""
    return "%sdigest: %s%s%s" % (indent, rendered, comment, nl)


def compute_plan(lines, registry, tag, crane_cmd):
    """Resolve every pin and return (plan, errors).

    plan: list of dicts {path, component, index, old, new, drift(bool)}
    errors: list of human-readable strings for unmapped pins / missing
            components (these are always fatal).
    """
    pins = find_pins(lines)
    errors = []
    seen_paths = set()
    plan = []
    for pin in pins:
        path = pin["path"]
        seen_paths.add(path)
        component = PARENT_TO_COMPONENT.get(path)
        if component is None:
            errors.append(
                "unmapped digest pin at line %d (path %r) — add it to "
                "PARENT_TO_COMPONENT or remove the pin" % (pin["index"] + 1, path)
            )
            continue
        new = resolve_digest(crane_cmd, registry, component, tag)
        plan.append(
            {
                "path": path,
                "component": component,
                "index": pin["index"],
                "old": pin["value"],
                "new": new,
                "drift": pin["value"] != new,
            }
        )
    # every mapped component MUST appear in the file — a missing block means a
    # first-party image lost its pin (the exact silent-rot failure we guard).
    for path, component in PARENT_TO_COMPONENT.items():
        if path not in seen_paths:
            errors.append(
                "expected digest pin for %s (%s) not found in values file"
                % (component, path)
            )
    return plan, errors


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Refresh or check air-gap first-party digest pins."
    )
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--check",
        action="store_true",
        help="verify pins == registry for Chart.AppVersion (default; exit 1 on drift)",
    )
    mode.add_argument(
        "--write",
        action="store_true",
        help="rewrite drifted pins in place",
    )
    parser.add_argument("--values", default=DEFAULT_VALUES)
    parser.add_argument("--chart", default=DEFAULT_CHART)
    parser.add_argument("--tag", default=None, help="override tag (default: Chart.AppVersion)")
    parser.add_argument("--registry", default=DEFAULT_REGISTRY)
    parser.add_argument("--crane", default="crane", help='crane invocation (default: "crane")')
    args = parser.parse_args(argv)

    # default mode is --check
    do_write = args.write

    crane_cmd = args.crane.split()

    try:
        tag = args.tag or read_app_version(args.chart)
        with open(args.values, encoding="utf-8") as fh:
            lines = fh.readlines()
        plan, errors = compute_plan(lines, args.registry, tag, crane_cmd)
    except PinError as exc:
        print("FATAL: %s" % exc, file=sys.stderr)
        return 2

    if errors:
        for err in errors:
            print("FATAL: %s" % err, file=sys.stderr)
        return 2

    drift = [p for p in plan if p["drift"]]

    if do_write:
        if not drift:
            print("refresh-airgap-pins: all %d pins already at %s — no change" % (len(plan), tag))
            return 0
        for p in drift:
            lines[p["index"]] = rewrite_line(lines[p["index"]], p["new"])
            print(
                "  %-14s (%s): %s -> %s"
                % (p["component"], p["path"], p["old"], p["new"])
            )
        with open(args.values, "w", encoding="utf-8") as fh:
            fh.writelines(lines)
        print("refresh-airgap-pins: rewrote %d pin(s) to %s" % (len(drift), tag))
        return 0

    # --check
    if not drift:
        print("refresh-airgap-pins: OK — all %d pins match %s" % (len(plan), tag))
        return 0
    print(
        "refresh-airgap-pins: DRIFT — %d pin(s) != registry digest for %s"
        % (len(drift), tag),
        file=sys.stderr,
    )
    for p in drift:
        print(
            "  %-14s (%s):\n    pinned:   %s\n    expected: %s"
            % (p["component"], p["path"], p["old"], p["new"]),
            file=sys.stderr,
        )
    print(
        "\nFix: scripts/inbox/refresh-airgap-pins.py --write "
        "(then commit values-inbox.yaml).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
