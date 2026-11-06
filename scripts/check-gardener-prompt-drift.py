#!/usr/bin/env python3
"""check-gardener-prompt-drift.py — gardener-prompt drift alarm.

Enforcement for bug ka-0000000874: a merged edit to
gardenio/plans/gardener-prompts/<name>.md must reach the live gardener
schedule bead's `prompt` field. The repo->bead sync is manual and nothing
enforced it, so drift accumulated silently (8/9 gardeners drifted, 2026-08-05).

This script converts that SILENT drift into a LOUD, fast failure. For each
gardener in gardenio/plans/gardener-prompts/gardeners.manifest.json it fetches the
live bead `prompt` over the daemon HTTP API and diffs it against the repo .md.

It CANNOT run in the per-MR CI pipeline: that pipeline holds only
GITLAB_CLONE_TOKEN and cannot reach the beads daemon (verified, ka-0000000874
design doc ka-0000002692). It runs on a credentialed pipeline (scheduled/
release/e2e) that reaches the in-cluster daemon via a kubectl port-forward +
kd-beads-token — the wrapper job supplies BEADS_HTTP_URL and, if the
daemon requires auth, BEADS_AUTH_TOKEN.

POINTER-AWARE (forward-compat with Option 3, the operator-gated pointer flip
in decision ka-0000003376): if a live prompt is a recognized pointer STUB
(it references its own gardener-prompts/<name>.md as the runtime source of
truth), it is the intended end-state and counts as IN-SYNC regardless of the
.md body — the stub cannot drift from the file it points at.

RUNBOOK-AWARE (positive coverage for runbook-delegating gardeners, ka-0000002202
/ design ka-0000003223 §3): a gardener whose live prompt delegates its full
charter to a versioned runbook under gardenio/docs/runbooks/<x>.md — rather than
carrying an inline copy of the gardener-prompts/<name>.md body — declares that
runbook with a "runbook" key on its manifest row. Such a gardener has NO inline
.md to diff; instead the alarm asserts (a) the declared runbook file exists in
the repo and (b) the live prompt actually references it. If the live prompt no
longer references its runbook, that is DRIFT (the delegation was silently
broken). This puts the intake groomer (ka-0000003841, runbook
gardenio/docs/runbooks/intake-groomer.md) under monitoring instead of leaving it
simply unmonitored.

Exit status:
  0  every mapped gardener is in sync (byte-identical body), a pointer stub, OR a
     runbook-delegating gardener whose prompt still references its runbook.
  2  at least one gardener has drifted (details on stderr + a summary table).
  1  operational failure (daemon unreachable, missing manifest/.md, bad JSON).
     A drift alarm must FAIL LOUD — it never silently passes on an error.

Usage:
  BEADS_HTTP_URL=http://localhost:19091 \
    [BEADS_AUTH_TOKEN=...] \
    ./scripts/check-gardener-prompt-drift.py [--repo-root .] [--json]

Companion test: scripts/check-gardener-prompt-drift_test.py
Task/bug: ka-0000000874. Operator: slack:U0OPERATOR1.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request

MANIFEST_REL = "gardenio/plans/gardener-prompts/gardeners.manifest.json"
PROMPTS_DIR_REL = "gardenio/plans/gardener-prompts"

# The daemon stores prompts with a single trailing newline; the repo convention
# is that every prompt .md ends in exactly one "\n". Storage whitespace at the
# very end of the field is not semantic drift, so we compare bodies with the
# trailing newline(s) normalized away. (Design doc ka-0000002692 §3a treats the
# lone trailing-blank-line hunk as non-drift.)
def _normalize(body: str) -> str:
    return body.rstrip("\n")


# Pre-rename live prompts (captain -> gardener, gd-5O9QUlA8zr) still say
# "captain". Same ordered, plural-first, case-sensitive table the repo-wide
# content rename used, so a live body that predates the rename can be compared
# against the renamed repo .md without reading as drift. Retire with phase 5.
_LEGACY_RENAME = (
    ("captain-epic", "gardener-epic"),
    ("Captains", "Gardeners"),
    ("CAPTAINS", "GARDENERS"),
    ("captains", "gardeners"),
    ("Captain", "Gardener"),
    ("CAPTAIN", "GARDENER"),
    ("captain", "gardener"),
)


def _apply_legacy_rename(body: str) -> str:
    for old, new in _LEGACY_RENAME:
        body = body.replace(old, new)
    return body


def _is_pointer_stub(live: str, name: str) -> bool:
    """A pointer stub delegates to gardener-prompts/<name>.md as the runtime
    source of truth. It is short and names its own .md. This is the Option-3
    end-state; a stub is IN-SYNC by construction (it reads the file at cycle
    start, so it cannot diverge from it)."""
    needles = (f"gardener-prompts/{name}.md", f"captain-prompts/{name}.md")
    return any(n in live for n in needles) and len(live) <= 1500


def _references_runbook(live: str, runbook_rel: str) -> bool:
    """A runbook-delegating gardener names its versioned runbook in its live
    prompt and reads it at cycle start (git show origin/main:<runbook_rel>),
    rather than carrying an inline gardener-prompts/<name>.md copy. The prompt
    is a stable pointer; the runbook is the source of truth. We accept either
    the full repo-relative path or its basename appearing in the live prompt so
    that a prompt written with a shorter reference (e.g. the leaf filename)
    still verifies."""
    return runbook_rel in live or os.path.basename(runbook_rel) in live


def _fetch_prompt(base_url: str, bead: str, token: str | None) -> str:
    """GET the bead over the daemon HTTP API and return its .fields.prompt.
    Raises RuntimeError with a clear message on any failure (fail loud)."""
    url = f"{base_url.rstrip('/')}/v1/beads/{bead}"
    req = urllib.request.Request(url)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"GET {url} -> HTTP {e.code} {e.reason}") from e
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise RuntimeError(f"GET {url} unreachable: {e}") from e
    except json.JSONDecodeError as e:
        raise RuntimeError(f"GET {url} returned non-JSON: {e}") from e

    # Tolerate both a bare bead object and an envelope ({"bead": {...}}).
    bead_obj = payload.get("bead", payload) if isinstance(payload, dict) else {}
    fields = bead_obj.get("fields", {}) if isinstance(bead_obj, dict) else {}
    prompt = fields.get("prompt")
    if prompt is None:
        raise RuntimeError(f"bead {bead} has no .fields.prompt")
    return prompt


def load_manifest(repo_root: str) -> list[dict]:
    path = os.path.join(repo_root, MANIFEST_REL)
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    gardeners = data.get("gardeners")
    if not isinstance(gardeners, list) or not gardeners:
        raise RuntimeError(f"{path}: no 'gardeners' array")
    return gardeners


def check(repo_root: str, base_url: str, token: str | None) -> tuple[int, list[dict]]:
    """Return (exit_code, rows). rows carry per-gardener verdicts for reporting."""
    gardeners = load_manifest(repo_root)
    rows: list[dict] = []
    drifted = 0
    op_error = False

    for entry in gardeners:
        name = entry["name"]
        bead = entry["bead"]
        runbook_rel = entry.get("runbook")
        row = {"name": name, "bead": bead, "verdict": "?", "detail": ""}

        # A runbook-delegating gardener has no inline gardener-prompts/<name>.md to
        # diff; it points at a versioned runbook. We assert the runbook exists
        # (a manifest row must never name a missing file) rather than reading a
        # <name>.md that intentionally does not exist for this row.
        if runbook_rel:
            repo_body = None
            runbook_path = os.path.join(repo_root, runbook_rel)
            if not os.path.isfile(runbook_path):
                row["verdict"] = "ERROR"
                row["detail"] = f"declared runbook missing: {runbook_path}"
                op_error = True
                rows.append(row)
                continue
        else:
            md_path = os.path.join(repo_root, PROMPTS_DIR_REL, f"{name}.md")
            try:
                with open(md_path, encoding="utf-8") as f:
                    repo_body = f.read()
            except OSError as e:
                row["verdict"] = "ERROR"
                row["detail"] = f"cannot read {md_path}: {e}"
                op_error = True
                rows.append(row)
                continue

        try:
            live_body = _fetch_prompt(base_url, bead, token)
        except RuntimeError as e:
            row["verdict"] = "ERROR"
            row["detail"] = str(e)
            op_error = True
            rows.append(row)
            continue

        if runbook_rel:
            if _references_runbook(live_body, runbook_rel):
                row["verdict"] = "RUNBOOK"
                row["detail"] = (
                    f"live prompt delegates to {runbook_rel} -> runbook is "
                    f"runtime source of truth"
                )
            else:
                row["verdict"] = "DRIFT"
                row["detail"] = (
                    f"live={len(live_body)}B — prompt no longer references its "
                    f"declared runbook {runbook_rel} (delegation broken)"
                )
                drifted += 1
        elif _is_pointer_stub(live_body, name):
            row["verdict"] = "POINTER"
            row["detail"] = "live prompt is a pointer stub -> .md is runtime source"
        elif _normalize(live_body) == _normalize(repo_body):
            row["verdict"] = "IN-SYNC"
        elif _normalize(_apply_legacy_rename(live_body)) == _normalize(repo_body):
            row["verdict"] = "IN-SYNC"
            row["detail"] = "live prompt predates the captain->gardener rename"
        else:
            row["verdict"] = "DRIFT"
            row["detail"] = (
                f"live={len(live_body)}B repo={len(repo_body)}B — "
                f"live prompt does not match repo .md"
            )
            drifted += 1
        rows.append(row)

    if op_error:
        return 1, rows
    if drifted:
        return 2, rows
    return 0, rows


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="Gardener-prompt drift alarm (ka-0000000874)")
    ap.add_argument("--repo-root", default=".", help="repo root (default: .)")
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    args = ap.parse_args(argv)

    base_url = os.environ.get("BEADS_HTTP_URL", "").strip()
    if not base_url:
        print(
            "FATAL: BEADS_HTTP_URL is unset — this check needs the daemon HTTP API "
            "(run it on a credentialed pipeline with an in-cluster port-forward, "
            "not the per-MR CI pipeline). See ka-0000000874.",
            file=sys.stderr,
        )
        return 1
    token = os.environ.get("BEADS_AUTH_TOKEN", "").strip() or None

    try:
        code, rows = check(args.repo_root, base_url, token)
    except (RuntimeError, OSError, json.JSONDecodeError) as e:
        print(f"FATAL: {e}", file=sys.stderr)
        return 1

    if args.json:
        print(json.dumps({"exit": code, "gardeners": rows}, indent=2))
    else:
        width = max((len(r["name"]) for r in rows), default=4)
        for r in rows:
            line = f"  {r['name']:<{width}}  {r['bead']:<14}  {r['verdict']}"
            if r["detail"]:
                line += f"  — {r['detail']}"
            print(line, file=sys.stderr)
        n_drift = sum(1 for r in rows if r["verdict"] == "DRIFT")
        n_err = sum(1 for r in rows if r["verdict"] == "ERROR")
        if code == 0:
            print(f"OK: all {len(rows)} gardener prompts in sync (or pointer stubs).", file=sys.stderr)
        elif code == 2:
            print(
                f"DRIFT: {n_drift}/{len(rows)} gardener prompt(s) differ from their .md. "
                f"Sync per gardenio/plans/gardener-prompts/README.md, or flip to a pointer stub "
                f"(decision ka-0000003376).",
                file=sys.stderr,
            )
        else:
            print(
                f"ERROR: {n_err} operational failure(s) — drift status UNKNOWN, failing loud.",
                file=sys.stderr,
            )
    return code


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
