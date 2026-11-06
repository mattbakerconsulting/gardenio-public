#!/usr/bin/env python3
"""strip-helm-template-comments.py

Remove Go text/template comments ({{/* ... */}}, including the whitespace-trim
variants {{- /* ... */ -}}) from Helm chart template files IN PLACE, reproducing
the Helm/Go template engine's own trim-marker semantics so the RENDERED output is
byte-identical before and after.

WHY THIS EXISTS (ka-0000007585):
The gardenio Helm release-history Secret `sh.helm.release.v1.gardenio.vN` embeds the
whole chart (templates included, gzipped) and must stay under the Kubernetes
1,048,576-byte Secret hard limit. A DECOMPOSITION of the live prod Secret v517
(gzip(release_json) = 774,701 B, only ~11.7 KiB under the ~786 KiB gzip ceiling)
showed:

  * chart.values is stored as a PARSED MAP (84 KB, comments already discarded when
    Helm loads the chart) — so stripping values.yaml comments does NOTHING to the
    Secret. That was the flaw in the first fix attempt (!7183/!7184): it shrank the
    packaged .tgz but left the release Secret unchanged, and deploy-prod 2026.224.12
    failed at the IDENTICAL v518 "Too long" error.
  * chart.templates is the real bulk: 2.26 MB raw / 606 KB gzipped, of which
    ~154 KB (gzipped) is Go-template comments. Removing them (render-identical)
    recovers ~166 KB of gzip headroom under the ceiling — ~14x the current margin.

This is a PACKAGE-TIME transform (run against a COPY of the chart). The committed
templates keep their full inline documentation for maintainers.

USAGE:
  strip-helm-template-comments.py <chart-dir>

Walks <chart-dir>/templates/**.{yaml,yml,tpl,txt} and strips template comments in
place. Prints a one-line summary to stderr. Non-template files are untouched.

TRIM SEMANTICS (must match Go text/template exactly, else the render drifts):
  {{/* c */}}        -> removed, surrounding whitespace UNTOUCHED
  {{- /* c */}}      -> removed + trailing whitespace of the PRECEDING text trimmed
  {{/* c */ -}}      -> removed + leading whitespace of the FOLLOWING text trimmed
  {{- /* c */ -}}    -> removed + both sides trimmed
Go's trim set is [ \t\r\n] (unicode.IsSpace over ASCII). We replicate precisely.
"""
import os
import re
import sys

# A template comment action. Group 2 = left trim marker, group 3 = right trim
# marker. Go requires the comment to be the ENTIRE action body (no other content
# may share a comment action), so `{{ ... /* ... */ ... }}` is not a comment —
# this pattern deliberately only matches actions whose body is exactly a comment.
_COMMENT = re.compile(r"\{\{(-)?[ \t\r\n]*/\*.*?\*/[ \t\r\n]*(-)?\}\}", re.S)
_TRIM = " \t\r\n"


def strip(text: str) -> str:
    out = []
    last = 0
    for m in _COMMENT.finditer(text):
        pre = text[last:m.start()]
        if m.group(1) == "-":  # left trim: drop trailing whitespace of preceding text
            pre = pre.rstrip(_TRIM)
        out.append(pre)
        last = m.end()
        if m.group(2) == "-":  # right trim: drop leading whitespace of following text
            j = last
            while j < len(text) and text[j] in _TRIM:
                j += 1
            last = j
    out.append(text[last:])
    return "".join(out)


def main(argv):
    if len(argv) != 2:
        sys.stderr.write("usage: strip-helm-template-comments.py <chart-dir>\n")
        return 2
    chart_dir = argv[1]
    tpl_dir = os.path.join(chart_dir, "templates")
    if not os.path.isdir(tpl_dir):
        sys.stderr.write(
            "strip-helm-template-comments.py: no templates/ dir in %s\n" % chart_dir
        )
        return 0
    changed = 0
    files = 0
    for root, _dirs, names in os.walk(tpl_dir):
        for n in names:
            if not n.endswith((".yaml", ".yml", ".tpl", ".txt")):
                continue
            p = os.path.join(root, n)
            files += 1
            with open(p, "r", encoding="utf-8") as f:
                src = f.read()
            dst = strip(src)
            if dst != src:
                with open(p, "w", encoding="utf-8") as f:
                    f.write(dst)
                changed += 1
    sys.stderr.write(
        "strip-helm-template-comments.py: stripped comments in %d/%d template files\n"
        % (changed, files)
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
