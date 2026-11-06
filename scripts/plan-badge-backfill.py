#!/usr/bin/env python3
# plan-badge-backfill.py
#
# Backfill Tier-0 plan badges for all live gardenio plans and assemble a "wall"
# contact sheet for the real-data check (task ka-0000000777, Phase 2 epic
# ka-0000002812, plan ka-0000005980).
#
# WHAT IT DOES:
#   1. Queries live plan epics from the beads daemon via `kd list` (label=plan,
#      status=open), or reads a fixture JSON (--from-json) for hermetic testing.
#   2. Renders a deterministic Tier-0 SVG badge for each via plan-badge-tier0.py.
#   3. Emits a single self-contained HTML contact sheet showing every badge at
#      128 / 40 / 24px next to its real plan title and bead id — the three sizes
#      the plan's acceptance criteria are judged at (128px detail header, 40px
#      /plans card, 24px cmdk row).
#   4. Optionally rasterizes that HTML to a PNG via headless Chrome (--png), so
#      the wall can be uploaded inline to Slack (the /image proxy URL does not
#      render in Slack — see gb/image.go:68-94 — so bytes must travel).
#
# The HTML is the source of truth; the PNG is a Slack-friendly snapshot of it.
#
# Usage:
#   plan-badge-backfill.py --out-html wall.html [--png wall.png]
#   plan-badge-backfill.py --from-json plans.json --out-html wall.html
#
# plans.json shape (also what `kd list ... --json` emits): a JSON array of
# objects each with at least {"id": "...", "title": "...", optionally
# "plan_state": "..."}.
from __future__ import annotations

import argparse
import importlib.util
import json
import os
import subprocess
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "pbt0", os.path.join(_HERE, "plan-badge-tier0.py")
)
assert _spec and _spec.loader
pbt0 = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pbt0)


def fetch_live_plans(project: str | None = None, all_projects: bool = False) -> list[dict]:
    """Query live plan epics from the beads daemon via kd.

    A plan is an epic carrying the `plan` label; live = status open. By default
    we scope to the gardenio project (the ~20 live plans the operator watches on
    the gardenio /plans board); pass all_projects=True for the whole fleet.
    """
    cmd = ["kd", "list", "--type", "epic", "--status", "open", "--limit", "500", "--json"]
    if all_projects:
        cmd += ["--all-projects"]
    elif project:
        cmd += ["--project", project]
    out = subprocess.check_output(cmd, text=True)
    rows = json.loads(out)
    plans = [r for r in rows if "plan" in (r.get("labels") or [])]
    return plans


def normalize(plans: list[dict]) -> list[dict]:
    """Keep only the fields the wall needs, sorted for a stable render order."""
    norm = []
    for p in plans:
        pid = p.get("id", "")
        title = p.get("title", "") or ""
        fields = p.get("fields") or {}
        state = fields.get("plan_state") or p.get("plan_state") or ""
        project = p.get("project") or ""
        norm.append(
            {"id": pid, "title": title, "state": state, "project": project}
        )
    # Deterministic order: by title, then id, so the wall is diff-stable.
    norm.sort(key=lambda r: (r["title"].lower(), r["id"]))
    return norm


def build_html(plans: list[dict]) -> str:
    """Self-contained HTML contact sheet: each plan's badge at 128/40/24px."""
    rows = []
    for p in plans:
        svg128 = pbt0.render_svg(p["id"], p["title"], size=128)
        svg40 = pbt0.render_svg(p["id"], p["title"], size=40)
        svg24 = pbt0.render_svg(p["id"], p["title"], size=24)
        title = _html_escape(p["title"])
        meta = _html_escape(
            " · ".join([x for x in (p["id"], p["state"], p["project"]) if x])
        )
        rows.append(
            f"""
    <div class="row">
      <div class="badges">
        <div class="b b128">{svg128}</div>
        <div class="b b40">{svg40}</div>
        <div class="b b24">{svg24}</div>
      </div>
      <div class="label">
        <div class="title">{title}</div>
        <div class="meta">{meta}</div>
      </div>
    </div>"""
        )
    body = "".join(rows)
    n = len(plans)
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Plan badge wall — Tier-0 backfill ({n} live plans)</title>
<style>
  :root {{ color-scheme: light dark; }}
  body {{
    margin: 0; padding: 24px;
    font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    background: #0b0f19; color: #e5e7eb;
  }}
  h1 {{ font-size: 18px; margin: 0 0 4px; }}
  .sub {{ color: #9ca3af; font-size: 13px; margin-bottom: 20px; }}
  .grid {{ display: grid; grid-template-columns: 1fr 1fr; gap: 14px 28px; }}
  .row {{
    display: flex; align-items: center; gap: 16px;
    background: #111827; border: 1px solid #1f2937;
    border-radius: 12px; padding: 12px 14px;
  }}
  .badges {{ display: flex; align-items: center; gap: 12px; flex: 0 0 auto; }}
  .b {{ display: flex; align-items: center; justify-content: center; }}
  .b128 svg {{ width: 64px; height: 64px; }}  /* 128 viewBox shown at 64 for density */
  .b40 svg {{ width: 40px; height: 40px; }}
  .b24 svg {{ width: 24px; height: 24px; }}
  .label {{ min-width: 0; }}
  .title {{ font-size: 14px; font-weight: 600; line-height: 1.3;
            overflow: hidden; text-overflow: ellipsis;
            display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; }}
  .meta {{ font-size: 11px; color: #6b7280; margin-top: 3px; font-variant-numeric: tabular-nums; }}
  .legend {{ color:#9ca3af; font-size:12px; margin: 4px 0 16px; }}
</style></head>
<body>
  <h1>Plan-badge wall — Tier-0 deterministic backfill</h1>
  <div class="sub">{n} live plans · badges are a pure function of (bead id, title) · task ka-0000000777</div>
  <div class="legend">Each row shows the same badge at 64px · 40px · 24px (detail header · /plans card · cmdk row).</div>
  <div class="grid">{body}
  </div>
</body></html>"""


def _html_escape(s: str) -> str:
    return (
        s.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )


def rasterize(html_path: str, png_path: str) -> None:
    """Render the HTML wall to a PNG via headless Chrome (agent pod ships it)."""
    chrome = _find_chrome()
    if not chrome:
        raise RuntimeError("no headless Chrome binary found")
    url = "file://" + os.path.abspath(html_path)
    # A fresh throwaway --user-data-dir is REQUIRED: without it headless Chrome
    # blocks indefinitely on a profile lock in the agent pod. --disable-dev-shm-usage
    # avoids the tiny /dev/shm in containers. Wrapped in a timeout so a Chrome
    # hang can never wedge the caller.
    import tempfile

    with tempfile.TemporaryDirectory(prefix="chrome-badge-") as profile:
        subprocess.run(
            [
                chrome,
                "--headless=new",
                "--no-sandbox",
                "--disable-gpu",
                "--disable-dev-shm-usage",
                "--hide-scrollbars",
                f"--user-data-dir={profile}",
                "--force-device-scale-factor=2",
                "--window-size=1500,2400",
                f"--screenshot={png_path}",
                url,
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=120,
            check=True,
        )


def _find_chrome() -> str | None:
    for c in (
        "/opt/google/chrome/chrome",
        "/usr/bin/google-chrome",
        "/usr/bin/chromium",
        "/usr/bin/chromium-browser",
    ):
        if os.path.exists(c):
            return c
    return None


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Backfill Tier-0 plan badges + wall.")
    p.add_argument("--from-json", help="read plans from this JSON file instead of kd")
    p.add_argument("--project", default="gardenio", help="scope kd query to this project (default gardenio)")
    p.add_argument("--all-projects", action="store_true", help="query plans across all projects")
    p.add_argument("--out-html", required=True, help="write the wall HTML here")
    p.add_argument("--png", help="also rasterize the wall to this PNG (headless Chrome)")
    p.add_argument("--svg-dir", help="also dump each plan's 128px SVG into this dir")
    args = p.parse_args(argv)

    if args.from_json:
        with open(args.from_json, encoding="utf-8") as fh:
            raw = json.load(fh)
    else:
        raw = fetch_live_plans(project=args.project, all_projects=args.all_projects)
    plans = normalize(raw)
    if not plans:
        print("no live plans found", file=sys.stderr)
        return 1

    html = build_html(plans)
    with open(args.out_html, "w", encoding="utf-8") as fh:
        fh.write(html)
    print(f"wrote {args.out_html} ({len(plans)} plans)", file=sys.stderr)

    if args.svg_dir:
        os.makedirs(args.svg_dir, exist_ok=True)
        for pl in plans:
            with open(
                os.path.join(args.svg_dir, f"{pl['id']}.svg"), "w", encoding="utf-8"
            ) as fh:
                fh.write(pbt0.render_svg(pl["id"], pl["title"], size=128))
        print(f"wrote {len(plans)} SVGs to {args.svg_dir}", file=sys.stderr)

    if args.png:
        rasterize(args.out_html, args.png)
        print(f"wrote {args.png}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
