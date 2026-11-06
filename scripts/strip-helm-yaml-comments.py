#!/usr/bin/env python3
"""strip-helm-yaml-comments.py

Remove comment-only YAML lines (`^\\s*#...`) from Helm chart template files IN
PLACE, so the packaged chart, and the Helm release Secret it is embedded in,
carry no documentation prose. Companion to strip-helm-template-comments.py,
which removes Go-template comments ({{/* ... */}}); this one removes the YAML
ones, which that script leaves alone because they are rendered into the
manifest.

WHY (gd-qiUiXvTX0k): the gardenio release record sh.helm.release.v1.gardenio.vN
hit the 1 MiB Secret limit at release 2026.292.1. Measured on the packed chart,
comment-only YAML lines were 6,248 lines / 453 KB of the 1.74 MB of templates,
and because Helm renders them, the same lines sat in the stored manifest too.
Stripping them is the single biggest lever after moving the Keycloak theme out
of the chart.

WHAT IS REMOVED. Only whole lines whose first non-blank character is `#`, and
only when the line is a YAML comment:
  * NOT inside a block scalar (`key: |`, `key: >-`, `- |` ...): a `#` line that
    is more indented than the block's parent key is content (a shell script's
    comment or shebang in a ConfigMap) and is kept verbatim. Lines that are
    only a template action (`{{- if .x }}`) are transparent to this tracking:
    they neither start nor end a block.
  * NOT inside a `{{ define }}`/`{{ block }}` body: a helper may build text
    that an `include` later places inside a block scalar.
  * NOT carrying a template action: `# {{- $x := 1 }}` has a side effect, so it
    stays.
Trailing comments (`key: v  # note`) are never touched: `#` can sit inside a
quoted string or a URL, and telling those apart is not worth the few bytes.

Only templates/**.yaml and **.yml are rewritten; .tpl helpers and NOTES.txt are
left alone.

THE RENDER IS NOT BYTE-IDENTICAL after this strip (the comment lines leave the
manifest too), but it is identical as YAML: pack-gardenio-chart.sh verifies
this by diffing the two renders with comment-only lines dropped from both, AND
by comparing them as parsed documents (yq), which is what catches a wrongly
removed content line.

USAGE:
  strip-helm-yaml-comments.py <chart-dir>
"""
import os
import re
import sys

_COMMENT_LINE = re.compile(r"^[ \t]*#")
_BLANK = re.compile(r"^[ \t]*$")
_ACTION = re.compile(r"\{\{-?\s*(.*?)\s*-?\}\}", re.S)
_TEMPLATE_ONLY = re.compile(r"^[ \t]*(\{\{.*?\}\}[ \t]*)+$")
# `key: |`, `- key: >-`, `- |`, with an optional trailing comment or action.
_BLOCK_START = re.compile(
    r"^(?P<indent>[ \t]*)(?:-[ \t]+)?(?:[^#\s][^:]*?:[ \t]+|)[|>][-+0-9]*[ \t]*(?:#.*|\{\{.*\}\}[ \t]*)?$"
)
_OPENERS = ("if", "range", "with", "define", "block")


def _indent(line):
    return len(line) - len(line.lstrip(" \t"))


def strip(text):
    lines = text.split("\n")
    out = []
    block_indent = None  # indent of the line that opened the current block scalar
    stack = []  # open template control structures, "define" when a define/block
    for line in lines:
        # Template control flow, for the define/block tracking.
        template_only = bool(_TEMPLATE_ONLY.match(line))
        for m in _ACTION.finditer(line):
            word = m.group(1).split(None, 1)[0] if m.group(1) else ""
            if word in _OPENERS:
                stack.append("define" if word in ("define", "block") else word)
            elif word == "end" and stack:
                stack.pop()
        in_define = "define" in stack

        if block_indent is not None:
            if _BLANK.match(line) or template_only:
                out.append(line)
                continue
            if _indent(line) > block_indent:
                out.append(line)
                continue
            block_indent = None  # this line ends the block; fall through

        if _COMMENT_LINE.match(line) and not in_define and "{{" not in line:
            continue  # the YAML comment line, dropped
        m = _BLOCK_START.match(line)
        if m and not template_only and not _COMMENT_LINE.match(line):
            block_indent = len(m.group("indent"))
        out.append(line)
    return "\n".join(out)


def main(argv):
    if len(argv) != 2:
        sys.stderr.write("usage: strip-helm-yaml-comments.py <chart-dir>\n")
        return 2
    tpl_dir = os.path.join(argv[1], "templates")
    if not os.path.isdir(tpl_dir):
        sys.stderr.write("strip-helm-yaml-comments.py: no templates/ dir in %s\n" % argv[1])
        return 0
    files = changed = removed = 0
    for root, _dirs, names in os.walk(tpl_dir):
        for n in names:
            if not n.endswith((".yaml", ".yml")):
                continue
            p = os.path.join(root, n)
            files += 1
            with open(p, "r", encoding="utf-8") as f:
                src = f.read()
            dst = strip(src)
            if dst != src:
                removed += src.count("\n") - dst.count("\n")
                with open(p, "w", encoding="utf-8") as f:
                    f.write(dst)
                changed += 1
    sys.stderr.write(
        "strip-helm-yaml-comments.py: removed %d comment lines from %d/%d template files\n"
        % (removed, changed, files)
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
