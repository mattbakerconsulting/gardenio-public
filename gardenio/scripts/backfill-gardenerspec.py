#!/usr/bin/env python3
"""Repeatable, idempotent backfill of identity-only GardenerSpec blobs onto
legacy spec-less gardeners.

Task:  ka-0000007453  (epic ka-0000000380 — "Backfill GardenerSpec engine blobs
       onto legacy spawn_agent gardeners").
Shape: gardenio/docs/gardenerspec-backfill-shape-2026-08-06.md  (design ka-0000007784, T2).
Set:   mirrors gardenio/scripts/enumerate-legacy-gardeners.py    (manifest ka-0000000351, T1).

WHAT THIS DOES
--------------
A "legacy spec-less gardener" is a gardener bead (role:gardener by LABEL or by the
`role` field) whose `fields.gardener` GardenerSpec blob is empty/absent. The engine
only bumps RunsSpawned/RunsCompleted when trig.Gardener != nil, so these gardeners
never accrue a native tally and beads-viewer must DERIVE the count at render time
(bug ka-0000005661). This script writes a minimal, behavior-preserving,
identity-only GardenerSpec blob so (a) the historical tally is seeded and (b) the
engine tracks future runs natively.

THE BLOB (exactly the 6 keys of the design's §2 shape, nothing that gates firing)::

    {
      "name":               <schedule_name, else cleaned trigger title>,
      "terminal_goal":      <best-effort one-liner from title>,
      "runs_spawned":       <derived spawned count>,   # seeded from deriveGardenerRunCount
      "runs_completed":     <derived completed count>,
      "last_agent_id":      <head of the life-thread>,
      "last_checked_agent": <SAME as last_agent_id>     # §5.2 de-dup guard: without
                                                        # it the tracker re-credits the
                                                        # head agent (+1 phantom run)
    }

Per the design the write mirrors the counters into the registered INTEGER-typed
top-level fields (runs_spawned / runs_completed as JSON numbers, last_agent_id as
a string) in the SAME update — the external-observer mirror the engine itself
maintains (gardener_evaluator.go:1301,1566). And when a gardener's
`max_concurrent > 1` we also set `max_concurrent_runs` to that same value, because
any non-nil blob activates the concurrency cap whose unset default is 1 — mirroring
max_concurrent *preserves* the pre-blob effective concurrency (design §5.1). This
is the only non-identity field set, and only to preserve behavior.

DELIBERATELY ABSENT (design §3/§7): run_budget, max_runs, stop, exit_condition,
on_exit, assign_bead, standing — every field that forward-controls the loop is
left at its zero value, because a spec-less trigger's loop is already uncontrolled
(zero == uncontrolled == unchanged firing behavior).

SAFETY PROPERTIES
-----------------
* IDEMPOTENT / RE-RUNNABLE: any gardener that already carries a non-empty `gardener`
  blob is SKIPPED (never overwritten). Re-running after a partial apply only writes
  the gardeners still missing a blob. A fully-applied set produces an all-skipped,
  no-op run.
* DRY-RUN BY DEFAULT: with no flags the script writes NOTHING — it prints, per
  gardener, the diff (old: field absent -> new: the blob + mirrored fields) for
  operator review. Mutation happens only under --apply.
* READ-BEFORE-WRITE: even under --apply, each gardener is re-fetched immediately
  before its write and re-checked for a blob, so a gardener that gained a blob
  concurrently (another apply, the engine, an operator) is still skipped.

USAGE
-----
    # dry-run: print the diff for every spec-less trigger gardener (default)
    gardenio/scripts/backfill-gardenerspec.py

    # include type=schedule legacy gardeners too (default: type=trigger only)
    gardenio/scripts/backfill-gardenerspec.py --include-schedules

    # limit to specific gardeners (repeatable / comma-separated)
    gardenio/scripts/backfill-gardenerspec.py --only ka-0000003150,ka-0000003783

    # emit the computed plan as JSON (for the manifest / review bundle)
    gardenio/scripts/backfill-gardenerspec.py --json

    # ACTUALLY WRITE (idempotent; skips any gardener that already has a blob)
    gardenio/scripts/backfill-gardenerspec.py --apply
    gardenio/scripts/backfill-gardenerspec.py --apply --only ka-0000003150   # one gardener
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile

GARDENER_TYPES = ("trigger", "schedule")
OWNERSHIP_KEYS = ("trigger_id", "schedule_id")
CLOSED_STATUSES = {"closed"}
ALL_STATUSES = ("open", "closed", "in_progress", "blocked")


# ---------------------------------------------------------------------------
# kd plumbing (mirrors enumerate-legacy-gardeners.py exactly for the set + counts)
# ---------------------------------------------------------------------------
def kd_list(args):
    cmd = ["kd", "list", "--all-types", "--json", *args]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
    if out.returncode != 0:
        sys.stderr.write(f"kd list failed: {' '.join(cmd)}\n{out.stderr}\n")
        return None
    return json.loads(out.stdout)


def kd_show(bead_id):
    cmd = ["kd", "show", bead_id, "--json"]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        sys.stderr.write(f"kd show failed: {bead_id}\n{out.stderr}\n")
        return None
    return json.loads(out.stdout)


def is_gardener(b):
    labels = b.get("labels") or []
    fields = b.get("fields") or {}
    return ("role:gardener" in labels) or (fields.get("role") == "gardener")


def has_spec(b):
    """True iff the bead already carries a non-empty `gardener` GardenerSpec blob.
    Such gardeners are SKIPPED (never overwritten) — the idempotency guard."""
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


def collect_gardeners(types):
    """Union of role:gardener beads across the requested types, deduped by id."""
    by_id = {}
    for typ in types:
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
    """deriveGardenerRunCount port (gardeners_enrich.go:115): MAX over the dual
    ownership keys trigger_id / schedule_id. runs_spawned = keyed agent beads;
    runs_completed = those closed; last_agent_id = newest-created keyed bead."""
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
    return spawned, completed, last_agent, any_ok


# ---------------------------------------------------------------------------
# spec derivation (design §2)
# ---------------------------------------------------------------------------
_PREFIX_RE = re.compile(r"^(gardener|scheduled|schedule trigger)\s*[:\-]\s*", re.IGNORECASE)


def derive_name(bead):
    """`name`: schedule_name field, else the trigger title with a leading
    Gardener:/Scheduled: prefix stripped (design §2.1)."""
    f = bead.get("fields") or {}
    sn = (f.get("schedule_name") or "").strip()
    if sn:
        return sn
    return _PREFIX_RE.sub("", (bead.get("title") or "").strip()).strip()


def derive_terminal_goal(bead):
    """`terminal_goal`: best-effort one-liner from the title, naming the anchor
    epic when the title carries `(epic kd-…)` / `(kd-…)` (design §2.1).
    Identity/observability only — never a live gate for the identity-only shape
    (§4), so a best-effort string is sufficient and safe."""
    title = (bead.get("title") or "").strip()
    m = re.search(r"\((?:epic\s+)?((gd|ka)-[A-Za-z0-9]+)\)", title)
    if m:
        return f"Drive {m.group(1)} to done ({title})."
    return title


def int_field(bead, key):
    v = (bead.get("fields") or {}).get(key)
    if v is None:
        return None
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def build_spec(bead):
    """Compute the identity-only blob + the top-level typed mirror for one
    gardener. Returns a dict describing the planned write, or None if the counts
    could not be derived (fail-closed — never guess a tally)."""
    cid = bead["id"]
    spawned, completed, last_agent, ok = derive_run_count(cid)
    if not ok:
        return None

    blob = {
        "name": derive_name(bead),
        "terminal_goal": derive_terminal_goal(bead),
        "runs_spawned": spawned,
        "runs_completed": completed,
    }
    # last_agent_id / last_checked_agent only when a life-thread head exists;
    # a 0/0 gardener has no head, and seeding an empty last_agent_id would leave
    # the tracker guard off (harmless) — but we mirror the engine and simply
    # omit both when there is no head (design §2.1: head of the life-thread).
    if last_agent:
        blob["last_agent_id"] = last_agent
        blob["last_checked_agent"] = last_agent  # §5.2 de-dup guard

    # Top-level typed mirror the engine itself maintains (design §2.2).
    top_fields = {
        "runs_spawned": spawned,      # JSON integer (kbeads 400-rejects strings)
        "runs_completed": completed,
    }
    if last_agent:
        top_fields["last_agent_id"] = last_agent

    # §5.1 concurrency-cap preservation: any non-nil blob activates the cap,
    # whose unset default is 1. Mirror max_concurrent when > 1 so the newly
    # active cap reproduces the pre-blob effective concurrency.
    mc = int_field(bead, "max_concurrent")
    mcr = int_field(bead, "max_concurrent_runs")
    concurrency_note = None
    if mc is not None and mc > 1 and (mcr is None or mcr < mc):
        top_fields["max_concurrent_runs"] = mc
        concurrency_note = (
            f"max_concurrent={mc} > 1: set max_concurrent_runs={mc} "
            f"to preserve pre-blob concurrency (design §5.1)"
        )

    return {
        "id": cid,
        "type": bead.get("type"),
        "title": bead.get("title"),
        "project": project_of(bead),
        "blob": blob,
        "top_fields": top_fields,
        "concurrency_note": concurrency_note,
        "derived": {
            "runs_spawned": spawned,
            "runs_completed": completed,
            "last_agent_id": last_agent,
        },
    }


# ---------------------------------------------------------------------------
# write (idempotent, read-before-write)
# ---------------------------------------------------------------------------
def apply_spec(plan):
    """Write the blob + typed mirror in a single `kd update`. Re-fetches and
    re-checks the blob immediately before writing so a gardener that gained a
    blob concurrently is still skipped. Returns (ok, message).

    The `gardener` field is stored as a JSON *string* (the engine writes it as
    `string(json.Marshal(spec))` and ParseTriggerDef reads
    `bead.Fields["gardener"]` as a string, triggers.go:4364). A bare `-f
    gardener={...}` would trip kd's rawOrString JSON auto-detection and store a
    nested object instead — so we pass the blob via `-f gardener=@file`, which
    kd reads as a literal string with no JSON coercion. The counter fields are
    passed bare so they land as JSON integers (kbeads 400-rejects them as
    strings), and last_agent_id / max_concurrent_runs land as their natural
    string / integer types."""
    cid = plan["id"]
    cur = kd_show(cid)
    if cur is None:
        return False, "re-fetch failed"
    if has_spec(cur):
        return True, "SKIP (gained a blob before write — idempotent)"

    blob_str = json.dumps(plan["blob"], separators=(",", ":"), sort_keys=True)
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as tf:
        tf.write(blob_str)
        blob_path = tf.name
    try:
        args = ["kd", "update", cid, "-f", f"gardener=@{blob_path}"]
        for k, v in plan["top_fields"].items():
            args += ["-f", f"{k}={v}"]  # ints render bare -> JSON numbers; ids -> strings
        out = subprocess.run(args, capture_output=True, text=True, timeout=60)
    finally:
        os.unlink(blob_path)
    if out.returncode != 0:
        return False, f"kd update failed: {out.stderr.strip()}"
    return True, "WROTE blob + typed mirror"


# ---------------------------------------------------------------------------
# rendering
# ---------------------------------------------------------------------------
def print_diff(plan):
    blob_json = json.dumps(plan["blob"], indent=2, sort_keys=True)
    print(f"\n=== {plan['id']}  [{plan['type']}, {plan['project']}] {plan['title']}")
    print("  gardener field:  (absent)  ->")
    for line in blob_json.splitlines():
        print(f"      {line}")
    print("  top-level typed mirror:")
    for k, v in sorted(plan["top_fields"].items()):
        print(f"      {k} = {v}")
    if plan["concurrency_note"]:
        print(f"  NOTE: {plan['concurrency_note']}")


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--apply", action="store_true",
                    help="actually write (default: dry-run, prints diffs only)")
    ap.add_argument("--include-schedules", action="store_true",
                    help="also target type=schedule gardeners (default: type=trigger only)")
    ap.add_argument("--only", action="append", default=[],
                    help="limit to these gardener ids (repeatable or comma-separated)")
    ap.add_argument("--json", action="store_true",
                    help="emit the computed plan as JSON instead of diffs")
    args = ap.parse_args()

    only = set()
    for spec in args.only:
        only.update(x.strip() for x in spec.split(",") if x.strip())

    types = GARDENER_TYPES if args.include_schedules else ("trigger",)
    gardeners = collect_gardeners(types)

    # Idempotency: partition into skip (already has a blob) vs candidates.
    already = [c for c in gardeners if has_spec(c)]
    candidates = [c for c in gardeners if not has_spec(c)]
    if only:
        candidates = [c for c in candidates if c["id"] in only]
        already = [c for c in already if c["id"] in only]

    plans, undeterminable = [], []
    for c in candidates:
        p = build_spec(c)
        (plans if p is not None else undeterminable).append(p or c)

    if args.json:
        json.dump({
            "mode": "apply" if args.apply else "dry-run",
            "types": list(types),
            "skipped_have_blob": [c["id"] for c in already],
            "undeterminable_counts": [c["id"] for c in undeterminable],
            "plans": plans,
        }, sys.stdout, indent=2)
        print()
        return

    print(f"# GardenerSpec backfill — {'APPLY' if args.apply else 'DRY-RUN (no writes)'}")
    print(f"# target types: {', '.join(types)}")
    print(f"# {len(candidates)} spec-less candidate(s); "
          f"{len(already)} already have a blob (skipped); "
          f"{len(undeterminable)} with underivable counts (skipped).")
    if already:
        print(f"# skipped (idempotent, already have a blob): "
              f"{', '.join(sorted(c['id'] for c in already))}")
    if undeterminable:
        print(f"# skipped (count derivation failed — NOT written): "
              f"{', '.join(sorted(c['id'] for c in undeterminable))}")

    for p in sorted(plans, key=lambda p: -p["derived"]["runs_spawned"]):
        print_diff(p)
        if args.apply:
            ok, msg = apply_spec(p)
            print(f"  -> {'OK ' if ok else 'ERR'} {msg}")

    if not args.apply:
        print(f"\n# DRY-RUN complete. {len(plans)} gardener(s) would be written. "
              f"Re-run with --apply to write. Safe to re-run: gardeners with a "
              f"blob are always skipped.")


if __name__ == "__main__":
    main()
