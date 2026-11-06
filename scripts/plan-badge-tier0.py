#!/usr/bin/env python3
# plan-badge-tier0.py
#
# Deterministic Tier-0 plan-badge renderer: (bead id, title) -> a self-contained
# SVG. This is the ALWAYS-PRESENT fallback badge for the "every gardenio plan gets
# its own generated icon/badge" plan (epic ka-0000005980, Phase 2 generator epic
# ka-0000002812). It has zero cost, zero network, and is a pure function of its
# inputs, so every plan always has SOMETHING to render — there is no empty state
# and no generation-latency hole while the model-authored Tier-2 generator
# (ka-0000002437) lands. It is the renderer the backfill real-data check
# (task ka-0000000777) drives against all live plans.
#
# WHY BEAD-ID-DERIVED VISUAL TRAITS, NOT JUST A MONOGRAM: the plan narrative
# (ka-0000005497) measured the failure mode of a monogram-only badge — the two
# "[E2E fixture]" plans both collapse to the monogram "EF". Deriving hue, shape,
# ring style, and an accent pattern from the BEAD ID (which is unique per plan)
# guarantees two same-monogram plans still render as visibly distinct badges.
# The monogram carries a hint of the title; the bead id carries the identity.
#
# LEGIBILITY AT 24px is an explicit acceptance criterion (the Tier-2 prototype's
# "metric triad" glyph collapsed to a featureless ring at 24px). Tier-0 stays
# legible small by design: one bold background shape + a high-contrast 1-2 letter
# monogram, no fine interior detail that dissolves when scaled down.
#
# OUTPUT: a single <svg> element, 128x128 viewBox, no external refs (no <image>,
# no web fonts — text uses a generic sans-serif stack), so it is safe to inline
# anywhere, diff in an MR, and rasterize with headless Chrome.
#
# Usage:
#   plan-badge-tier0.py --id ka-0000005980 --title "Plan title here"
#     -> writes the SVG to stdout
#   plan-badge-tier0.py --id ... --title ... --out badge.svg
#
# Reproducibility: identical (id, title) -> byte-identical SVG, forever.
from __future__ import annotations

import argparse
import hashlib
import sys

# ---------------------------------------------------------------------------
# Palette: a fixed set of hues chosen to be distinguishable from each other and
# to read on both light and dark board backgrounds. Each entry is (background,
# ink) where ink is the monogram/foreground color with adequate contrast on the
# background. Colors are deliberately saturated but not neon — this is the
# "quiet wayfinding" register, not "wall of loud stickers" (art-direction
# decision ka-0000004332 is still open; Tier-0 is neutral and works under any
# outcome because it is the fallback, not the headline art).
# ---------------------------------------------------------------------------
_PALETTE = [
    ("#2563eb", "#eff6ff"),  # blue
    ("#0891b2", "#ecfeff"),  # cyan
    ("#059669", "#ecfdf5"),  # emerald
    ("#4d7c0f", "#f7fee7"),  # lime  (darkened to clear the 3.0:1 24px floor)
    ("#a16207", "#fefce8"),  # amber (darkened to clear the 3.0:1 24px floor)
    ("#dc2626", "#fef2f2"),  # red
    ("#e11d48", "#fff1f2"),  # rose
    ("#c026d3", "#fdf4ff"),  # fuchsia
    ("#7c3aed", "#f5f3ff"),  # violet
    ("#4f46e5", "#eef2ff"),  # indigo
    ("#0d9488", "#f0fdfa"),  # teal
    ("#ea580c", "#fff7ed"),  # orange
]

# Background container shapes. Each is a function (size) -> SVG path/element body
# drawn to fill the badge. Picking the shape from the bead id gives a second,
# orthogonal axis of distinctiveness beyond hue.
_SHAPES = ("rounded", "circle", "hexagon", "squircle", "shield", "diamond")

# Accent ring styles layered over the shape edge — a third distinctiveness axis.
_RINGS = ("none", "solid", "dashed", "double")


def _digest(bead_id: str) -> bytes:
    """Stable bytes derived from the bead id alone (identity axis)."""
    return hashlib.sha256(bead_id.encode("utf-8")).digest()


def _monogram(title: str) -> str:
    """1-2 char uppercase monogram from the title.

    Skips a leading bracketed tag like "[E2E fixture]" when there is real title
    text after it, so "[E2E fixture] Plans UX demo" -> "PU", not "EF". This is
    the direct fix for the measured monogram-collapse failure. When the title is
    ONLY a bracketed tag, we fall back to using the tag's words.
    """
    words = _title_words(title)
    if not words:
        return "?"
    if len(words) == 1:
        w = words[0]
        return (w[:2] if len(w) >= 2 else w).upper()
    return (words[0][0] + words[1][0]).upper()


def _title_words(title: str) -> list[str]:
    """Alphanumeric words for the monogram, preferring text after a leading
    bracketed tag."""
    stripped = title.strip()
    # Prefer the portion after a leading [..] or (..) tag, if it has letters.
    for open_c, close_c in (("[", "]"), ("(", ")")):
        if stripped.startswith(open_c) and close_c in stripped:
            after = stripped.split(close_c, 1)[1]
            after_words = _alnum_words(after)
            if after_words:
                return after_words
    return _alnum_words(stripped)


def _alnum_words(s: str) -> list[str]:
    out: list[str] = []
    cur: list[str] = []
    for ch in s:
        if ch.isalnum():
            cur.append(ch)
        elif cur:
            out.append("".join(cur))
            cur = []
    if cur:
        out.append("".join(cur))
    return out


def _shape_body(shape: str, bg: str, size: int) -> str:
    """SVG for the filled background container of the given shape."""
    s = size
    m = 4  # small margin so the ring is not clipped
    a, b = m, s - m
    if shape == "circle":
        return f'<circle cx="{s/2}" cy="{s/2}" r="{s/2 - m}" fill="{bg}"/>'
    if shape == "rounded":
        return f'<rect x="{a}" y="{a}" width="{b-a}" height="{b-a}" rx="{s*0.22}" fill="{bg}"/>'
    if shape == "squircle":
        # Approximate a superellipse with a large corner radius.
        return f'<rect x="{a}" y="{a}" width="{b-a}" height="{b-a}" rx="{s*0.38}" fill="{bg}"/>'
    if shape == "hexagon":
        cx, cy, r = s / 2, s / 2, s / 2 - m
        pts = []
        # flat-top hexagon
        import math

        for i in range(6):
            ang = math.pi / 180 * (60 * i - 30)
            pts.append(f"{cx + r*math.cos(ang):.1f},{cy + r*math.sin(ang):.1f}")
        return f'<polygon points="{" ".join(pts)}" fill="{bg}"/>'
    if shape == "diamond":
        cx, cy, r = s / 2, s / 2, s / 2 - m
        pts = f"{cx},{cy-r} {cx+r},{cy} {cx},{cy+r} {cx-r},{cy}"
        return f'<polygon points="{pts}" fill="{bg}"/>'
    if shape == "shield":
        # A simple shield: rounded top, tapering to a point at the bottom.
        top, w = m, s - 2 * m
        cx = s / 2
        d = (
            f"M{a},{top+w*0.16} "
            f"Q{a},{top} {cx},{top} "
            f"Q{b},{top} {b},{top+w*0.16} "
            f"L{b},{top+w*0.55} "
            f"Q{b},{top+w*0.85} {cx},{b} "
            f"Q{a},{top+w*0.85} {a},{top+w*0.55} Z"
        )
        return f'<path d="{d}" fill="{bg}"/>'
    # default fallback
    return f'<rect x="{a}" y="{a}" width="{b-a}" height="{b-a}" rx="{s*0.22}" fill="{bg}"/>'


def _ring_body(ring: str, ink: str, size: int) -> str:
    """A decorative ring drawn just inside the shape edge (approximated as a
    circle — reads fine under every shape at badge scale)."""
    if ring == "none":
        return ""
    s = size
    r = s / 2 - 10
    common = f'cx="{s/2}" cy="{s/2}" r="{r}" fill="none" stroke="{ink}" stroke-opacity="0.55"'
    if ring == "solid":
        return f'<circle {common} stroke-width="2"/>'
    if ring == "dashed":
        return f'<circle {common} stroke-width="2" stroke-dasharray="6 5"/>'
    if ring == "double":
        r2 = r - 4
        c2 = f'cx="{s/2}" cy="{s/2}" r="{r2}" fill="none" stroke="{ink}" stroke-opacity="0.4"'
        return f'<circle {common} stroke-width="1.5"/><circle {c2} stroke-width="1.5"/>'
    return ""


def render_svg(bead_id: str, title: str, size: int = 128) -> str:
    """Return a deterministic Tier-0 badge SVG for (bead_id, title)."""
    d = _digest(bead_id)
    bg, ink = _PALETTE[d[0] % len(_PALETTE)]
    shape = _SHAPES[d[1] % len(_SHAPES)]
    ring = _RINGS[d[2] % len(_RINGS)]
    # A faint secondary hue from a different byte gives a subtle 2-tone gradient,
    # widening the visual space so near-collisions stay distinguishable.
    bg2, _ = _PALETTE[d[3] % len(_PALETTE)]
    mono = _monogram(title)
    # Font size shrinks for a 2-char monogram so it never overflows the shape.
    fs = size * (0.46 if len(mono) >= 2 else 0.56)
    grad_id = "g" + hashlib.sha256(bead_id.encode()).hexdigest()[:8]

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{size}" height="{size}" '
        f'viewBox="0 0 {size} {size}" role="img" '
        f'aria-label="Plan badge for {_xml_escape(title)}">',
        "<defs>",
        f'<linearGradient id="{grad_id}" x1="0" y1="0" x2="1" y2="1">',
        f'<stop offset="0" stop-color="{bg}"/>',
        f'<stop offset="1" stop-color="{bg2}"/>',
        "</linearGradient>",
        "</defs>",
    ]
    # Shape filled with the gradient (swap the flat fill for the gradient id).
    shape_svg = _shape_body(shape, f"url(#{grad_id})", size)
    parts.append(shape_svg)
    parts.append(_ring_body(ring, ink, size))
    parts.append(
        f'<text x="{size/2}" y="{size/2}" fill="{ink}" '
        f'font-family="ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif" '
        f'font-size="{fs:.1f}" font-weight="700" text-anchor="middle" '
        f'dominant-baseline="central" letter-spacing="-1">{_xml_escape(mono)}</text>'
    )
    parts.append("</svg>")
    return "".join(parts)


def _xml_escape(s: str) -> str:
    return (
        s.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Deterministic Tier-0 plan badge SVG.")
    p.add_argument("--id", required=True, help="plan bead id (identity axis)")
    p.add_argument("--title", required=True, help="plan title (monogram source)")
    p.add_argument("--size", type=int, default=128, help="SVG square size (px)")
    p.add_argument("--out", help="write to this file instead of stdout")
    args = p.parse_args(argv)
    svg = render_svg(args.id, args.title, args.size)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(svg)
    else:
        sys.stdout.write(svg)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
