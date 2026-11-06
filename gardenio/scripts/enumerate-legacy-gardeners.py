#!/usr/bin/env python3
"""Enumerate the legacy spec-less gardener set + derive per-gardener run counts.

Task: ka-0000000351 (epic ka-0000000380 — "Backfill GardenerSpec engine blobs
onto legacy spawn_agent gardeners").

A "legacy spec-less gardener" is a gardener bead — role:gardener by LABEL or by the
`role` field — whose `fields.gardener` GardenerSpec blob is empty/absent. The
engine only bumps RunsSpawned/RunsCompleted when trig.Gardener != nil
(gardener_evaluator.go), so these gardeners never accumulate a native tally and the
beads-viewer must DERIVE the count at render time from their iteration agent beads
(bug ka-0000005661).

This script mirrors the viewer exactly:

  * Gardener set: BOTH type=trigger (modern) and type=schedule (legacy), matched by
    the role:gardener LABEL *and* by the role=gardener FIELD, deduped by id — the
    same union loadGardenerRows performs (gardeners.go:551).
  * Run derivation: deriveGardenerRunCount (gardeners_enrich.go:115) — union of the
    dual ownership keys trigger_id / schedule_id over type=agent iteration beads,
    taking MAX (not SUM) per key. runs_spawned = total agent beads keyed to the
    gardener; runs_completed = those with status=closed; last_agent_id = the
    most-recently-created (created_at) keyed agent bead.

READ-ONLY: this script never mutates a bead. It only runs `kd list`.

Usage:
    gardenio/scripts/enumerate-legacy-gardeners.py            # markdown manifest to stdout
    gardenio/scripts/enumerate-legacy-gardeners.py --json     # raw JSON rows
"""
import argparse
import json
import subprocess
import sys

GARDENER_TYPES = ("trigger", "schedule")
OWNERSHIP_KEYS = ("trigger_id", "schedule_id")
CLOSED_STATUSES = {"closed"}
ALL_STATUSES = ("open", "closed", "in_progress", "blocked")


def kd_list(args):
    """Run `kd list --all-types ... --json` and return the parsed bead list."""
    cmd = ["kd", "list", "--all-types", "--json", *args]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
    if out.returncode != 0:
        sys.stderr.write(f"kd list failed: {' '.join(cmd)}\n{out.stderr}\n")
        return None
    return json.loads(out.stdout)


def is_gardener(b):
    labels = b.get("labels") or []
    fields = b.get("fields") or {}
    return ("role:gardener" in labels) or (fields.get("role") == "gardener")


def has_spec(b):
    blob = (b.get("fields") or {}).get("gardener")
    if blob is None:
        return False
    if isinstance(blob, dict):
        return len(blob) > 0
    return str(blob).strip() not in ("", "{}", "null")


def project_of(b):
    for lab in b.get("labels") or []:
        if lab.startswith("project:"):
            return lab.split(":", 1)[1]
    return (b.get("fields") or {}).get("project", "?")


def collect_gardeners():
    """Union of role:gardener beads across both types, deduped by id."""
    by_id = {}
    for typ in GARDENER_TYPES:
        status_flags = []
        for s in ALL_STATUSES:
            status_flags += ["--status", s]
        rows = kd_list(["--type", typ, *status_flags, "--limit", "1000"])
        if rows is None:
            continue
        for b in rows:
            if is_gardener(b):
                by_id[b["id"]] = b
    return list(by_id.values())


def derive_run_count(gardener_id):
    """deriveGardenerRunCount port: MAX over the dual ownership keys."""
    spawned = completed = 0
    last_agent = None
    last_ts = ""
    any_ok = False
    for key in OWNERSHIP_KEYS:
        beads = kd_list(["--type", "agent", "-f", f"{key}={gardener_id}", "--limit", "10000"])
        if beads is None:
            continue
        any_ok = True
        spawned = max(spawned, len(beads))
        completed = max(completed, sum(1 for b in beads if b.get("status") in CLOSED_STATUSES))
        for b in beads:
            ts = b.get("created_at", "")
            if ts > last_ts:
                last_ts, last_agent = ts, b.get("id")
    return spawned, completed, (last_agent or "-"), any_ok


def build_rows():
    gardeners = collect_gardeners()
    specless = [b for b in gardeners if not has_spec(b)]
    rows = []
    for c in specless:
        spawned, completed, last_agent, ok = derive_run_count(c["id"])
        f = c.get("fields") or {}
        rows.append({
            "id": c["id"],
            "type": c["type"],
            "title": c["title"],
            "status": c["status"],
            "action": f.get("action", "-"),
            "cron": f.get("cron", "-"),
            "project": project_of(c),
            "owner": f.get("operated_by") or c.get("operated_by") or "-",
            "runs_spawned": spawned,
            "runs_completed": completed,
            "last_agent_id": last_agent,
            "derivation_ok": ok,
        })
    return rows, len(gardeners)


def md_table(rows):
    lines = [
        "| bead-id | type | gardener name | spawned | completed | last_agent_id | status | action | cron | project |",
        "|---|---|---|--:|--:|---|---|---|---|---|",
    ]
    for r in sorted(rows, key=lambda r: -r["runs_spawned"]):
        name = r["title"].replace("|", "\\|")[:60]
        lines.append(
            f"| {r['id']} | {r['type']} | {name} | {r['runs_spawned']} | "
            f"{r['runs_completed']} | {r['last_agent_id']} | {r['status']} | "
            f"{r['action']} | `{r['cron']}` | {r['project']} |"
        )
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--json", action="store_true", help="emit raw JSON rows instead of markdown")
    args = ap.parse_args()

    rows, total_gardeners = build_rows()
    if args.json:
        json.dump(rows, sys.stdout, indent=2)
        print()
        return

    trig = [r for r in rows if r["type"] == "trigger"]
    sched = [r for r in rows if r["type"] == "schedule"]
    print("# Legacy spec-less gardener manifest — derived run counts\n")
    print(f"- {total_gardeners} gardener beads total; {len(rows)} are spec-less "
          f"({len(trig)} type=trigger + {len(sched)} type=schedule).\n")
    print("## A. type=trigger (backfill-execute scope, ka-0000001186)\n")
    print(md_table(trig))
    print("\n## B. type=schedule (legacy dead primitive)\n")
    print(md_table(sched))


if __name__ == "__main__":
    main()
