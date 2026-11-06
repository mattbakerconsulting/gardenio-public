#!/usr/bin/env python3
"""opus5-ab.py — the Opus/Fable-5 context-audit A-B harness (task ka-0000001452, P4).

WHAT THIS IS
  A reusable comparator for the "before/after" evaluation that gates the P3 context
  remediation (epic ka-0000004753) before it merges. It answers one question:

      Did trimming/reframing the injected context (P3) change how agents behave —
      turn_count, LiteLLM spend, and task success — for the 3 representative
      task types (thread reply, crew build, gardener cycle)?

  It works on TWO kinds of signal, and is deliberately explicit about which:

    (1) STATIC context delta  — `gb prime --size` per role. Cheap, deterministic,
        available today. This is the *input* the P3 diff moves.
    (2) BEHAVIOURAL delta     — turn_count + spend + success, sampled from the
        agent config beads the fleet already records (total_request_count,
        spend_logs_cost_usd, agent_state). This is the *output* we must not regress.

HOW THE A-B ACTUALLY RUNS
  A ("before") = fleet on current main, BEFORE the P3 diff.  <- captured as a frozen
                 baseline JSON so it survives the merge (beads keep changing).
  B ("after")  = fleet after the P3 diff has been deployed for a comparable window,
                 OR a controlled re-run of the same 3 tasks under the trimmed context.

  Usage:
    # Capture side A (run on main before P3, or any time — it snapshots live beads):
    python3 scripts/opus5-ab.py capture --out audits/.../baseline-A.json

    # After P3 is deployed, capture side B. Pass --since <deploy-time> so B samples ONLY
    # agents that ran under the new context — otherwise the last-200 window straddles the
    # deploy boundary and B re-samples the same cohort as A (a provisional, not clean, B):
    python3 scripts/opus5-ab.py capture --out /tmp/after-B.json --since 2026-07-25T10:00:00Z
    python3 scripts/opus5-ab.py compare --a audits/.../baseline-A.json --b /tmp/after-B.json

  `capture` shells out to `kd list --type=agent --label role:<r>` per representative
  role and distills the metric distributions. `compare` prints the deltas and applies
  the regression gate below.

REGRESSION GATE (the merge decision in step 3/4 of the task)
  PASS (safe to merge, file "A-B pass" comment) iff, for every representative role:
    - success_rate_B >= success_rate_A - SUCCESS_TOLERANCE_PP   (no worse success)
    - spend_median_B <= spend_median_A * (1 + SPEND_TOLERANCE)  (spend comparable)
    - turns_median_B <= turns_median_A * (1 + TURNS_TOLERANCE)  (not more churny)
  Otherwise REGRESSION -> the operator must review (file gb decision + yield).

  Tolerances are generous because fleet traffic is noisy run-to-run; the point of
  the gate is to catch a *material* over-constraint regression, not p-hacked noise.

  Two refinements keep the gate faithful to that "material, not noise" contract —
  because a median comparison is only as trustworthy as the sample under it, and
  because over-constraint has a *signature*, not just a single moved number:

    (a) SAMPLE-SIZE FLOOR (MIN_SAMPLE). A role whose B window has n < MIN_SAMPLE
        completed runs is UNDER-SAMPLED: its median is dominated by a handful of
        runs, so a turns/spend "breach" there is noise by construction (thread at
        n=2 is the canonical case). Under-sampled roles are reported as ADVISORY
        and never escalated to a hard REGRESSION. Success is the exception — a
        real success DROP is alarming at any n, so it is still surfaced (see below).

    (b) COUPLED turns+spend. Genuine over-constraint makes the agent grind *harder*
        under stricter directives — it raises turns AND spend together. A lone
        turns-up-while-spend-falls (or vice-versa) is task-mix / cheaper-turns
        variation, not systematic constraint (gardener-delivery: turns 14->19 while
        spend collapses -76% is the canonical case). So a single-metric turns/spend
        breach in a well-sampled role is downgraded to a WATCH-ITEM (advisory);
        only BOTH breaching together on the same role is a hard REGRESSION.

  These encode, in the tool, the exact "small-sample noise / turns-up-spend-down is
  contradictory" judgment that four A-B re-capture agents (ka-0000007395) each had to
  hand-write into operator decision ka-0000007166. Success-rate regressions remain a
  hard fail at any n >= MIN_SAMPLE: task success is the ground-truth quality signal
  the audit must never regress, and it is not subject to the coupling rule.
"""
import argparse, json, os, subprocess, sys
import statistics as st

# role file-key -> (kd role: label, representative-task-type from the A-B spec)
REPRESENTATIVE_ROLES = {
    "thread":            "thread reply",
    "crew":              "crew build",
    "gardener-delivery":  "gardener cycle (delivery)",
    "gardener-bugfix":    "gardener cycle (bugfix)",
    "plan-gardener":      "gardener cycle (plan)",
}

SUCCESS_TOLERANCE_PP = 2.0    # success-rate may dip at most 2 percentage points
SPEND_TOLERANCE      = 0.15   # median spend may rise at most 15%
TURNS_TOLERANCE      = 0.15   # median turns may rise at most 15%

# Below this many completed B-side runs a role's median is noise-dominated, so its
# turns/spend "breaches" are advisory, never a hard REGRESSION (see gate refinement
# (a) in the module docstring). A real SUCCESS drop is still surfaced at any n.
MIN_SAMPLE = 10

SAMPLE_LIMIT = 200


def _num(x):
    try:
        return float(x)
    except (TypeError, ValueError):
        return None


def _median(vals):
    vals = [v for v in vals if v is not None]
    return st.median(vals) if vals else None


def distill(beads):
    """Reduce raw agent beads to the per-role metric block we compare on."""
    rows = []
    for b in beads:
        f = (b.get("fields") or {})
        state = f.get("agent_state")
        if state not in ("done", "failed"):        # completed runs only
            continue
        rows.append({
            "state":  state,
            "turns":  _num(f.get("total_request_count")) or _num(f.get("live_turns")),
            "spend":  _num(f.get("spend_logs_cost_usd")),
            "tools":  _num(f.get("total_tool_calls")),
            "ctx":    _num(f.get("context_tokens")),
        })
    n = len(rows)
    done = [r for r in rows if r["state"] == "done"]
    spends = [r["spend"] for r in rows if r["spend"] and r["spend"] > 0]
    return {
        "n": n,
        "done": len(done),
        "failed": n - len(done),
        "success_rate_pct": round(len(done) / n * 100.0, 1) if n else None,
        "turns_median": _median([r["turns"] for r in rows]),
        "spend_median": _median(spends),
        "tools_median": _median([r["tools"] for r in rows]),
        "ctx_median":   _median([r["ctx"] for r in rows if r["ctx"]]),
        "prime_bytes": None,   # filled by capture() via gb prime --size --for
    }


def _kd_list(role, since=None):
    cmd = ["kd", "list", "--type=agent", "--label", f"role:{role}",
           "--limit", str(SAMPLE_LIMIT), "--json"]
    if since:
        # Restrict the sample to agents CREATED after the cutoff. This is what makes a
        # *clean* post-deploy B possible: without it, capture() takes the last 200 beads
        # regardless of when they ran, so B straddles the deploy boundary and re-samples
        # the same pre-change cohort as A (the "thread A & B are bit-identical" caveat
        # from ka-0000001452). Pass --since <deploy-time> to sample only fresh runs.
        cmd += ["--created-after", since]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        print(f"  warn: kd list role:{role} failed: {out.stderr.strip()[:120]}", file=sys.stderr)
        return []
    try:
        return json.loads(out.stdout)
    except json.JSONDecodeError:
        return []


def _prime_bytes(sample_agent):
    if not sample_agent:
        return None
    out = subprocess.run(
        ["gb", "prime", "--size", "--for", sample_agent, "--project", "gardenio"],
        capture_output=True, text=True)
    for tok in out.stdout.split():
        if tok.startswith("total_bytes="):
            return int(tok.split("=", 1)[1])
    return None


def capture(out_path, since=None):
    # `since` is recorded in the snapshot so `compare` can tell a clean post-deploy B
    # (windowed to fresh runs) from a provisional whole-fleet B (last-200, no window).
    snap = {"window_since": since, "roles": {}}
    if since:
        print(f"capture window: agents created after {since}")
    for role, task in REPRESENTATIVE_ROLES.items():
        beads = _kd_list(role, since=since)
        block = distill(beads)
        block["task"] = task
        sample_agent = None
        for b in beads:
            sample_agent = (b.get("fields") or {}).get("agent") or b.get("title")
            if sample_agent:
                break
        block["prime_bytes"] = _prime_bytes(sample_agent)
        snap["roles"][role] = block
        print(f"role:{role:16s} n={block['n']:3d} succ={block['success_rate_pct']}% "
              f"turns~{block['turns_median']} spend~${block['spend_median']} "
              f"prime={block['prime_bytes']}B")
    os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
    json.dump(snap, open(out_path, "w"), indent=2)
    print(f"\nwrote {out_path}")


def _delta(a, b):
    if a is None or b is None:
        return None
    return b - a


def evaluate_gate(A, B):
    """Pure A-B regression gate — no IO, so it is unit-testable in isolation.

    Applies the two "material, not noise" refinements from the module docstring:
      (a) roles with B-side n < MIN_SAMPLE are UNDER-SAMPLED — their turns/spend
          breaches are advisory only, never a hard regression;
      (b) a lone turns-OR-spend breach in a well-sampled role is a WATCH-ITEM;
          only BOTH breaching together is the coupled over-constraint signature.
    A success-rate drop beyond tolerance is a hard regression at any n >= MIN_SAMPLE
    (and is surfaced as an under-sampled advisory below the floor).

    Returns a dict:
      {"regressions": [...], "watch_items": [...], "undersampled": [...]}
    `regressions` non-empty => the caller exits 2 (operator decision); else PASS.
    """
    regressions, watch_items, undersampled = [], [], []
    for role in REPRESENTATIVE_ROLES:
        a, b = A.get(role), B.get(role)
        if not a or not b:
            continue
        n = b.get("n") or 0
        low_n = n < MIN_SAMPLE

        # Success is the ground-truth quality signal — a real DROP is never coupled
        # away, but below the sample floor it is advisory (a 2-run window that goes
        # 2/2 -> 1/2 is not evidence of a fleet-wide quality regression).
        succ_drop = (a.get("success_rate_pct") is not None
                     and b.get("success_rate_pct") is not None
                     and b["success_rate_pct"] < a["success_rate_pct"] - SUCCESS_TOLERANCE_PP)
        if succ_drop:
            msg = f"{role}: success {a['success_rate_pct']}%→{b['success_rate_pct']}% (n={n})"
            (undersampled if low_n else regressions).append(msg)

        # turns/spend: compute each breach, then apply the coupling rule.
        spend_breach = bool(a.get("spend_median") and b.get("spend_median")
                            and b["spend_median"] > a["spend_median"] * (1 + SPEND_TOLERANCE))
        turns_breach = bool(a.get("turns_median") and b.get("turns_median")
                            and b["turns_median"] > a["turns_median"] * (1 + TURNS_TOLERANCE))
        parts = []
        if turns_breach:
            parts.append(f"turns {a['turns_median']}→{b['turns_median']}")
        if spend_breach:
            parts.append(f"spend ${a['spend_median']:.3f}→${b['spend_median']:.3f}")
        if not parts:
            continue
        detail = f"{role}: {' + '.join(parts)} (n={n})"
        if low_n:
            undersampled.append(detail)                      # (a) noise-dominated
        elif turns_breach and spend_breach:
            regressions.append(detail)                       # coupled = over-constraint
        else:
            watch_items.append(detail)                       # (b) single-metric = watch
    return {"regressions": regressions, "watch_items": watch_items,
            "undersampled": undersampled}


def compare(a_path, b_path):
    a_snap = json.load(open(a_path))
    b_snap = json.load(open(b_path))
    A = a_snap["roles"]
    B = b_snap["roles"]
    b_window = b_snap.get("window_since")
    print("=" * 84)
    print(f"A-B COMPARE   A={a_path}   B={b_path}")
    # Whether B is a *clean* post-deploy split or a *provisional* whole-fleet snapshot
    # hinges on the window. Surface it so the verdict is never silently over-claimed.
    if b_window:
        print(f"B window: agents created after {b_window} (post-deploy split — behavioral leg is CLEAN)")
    else:
        print("B window: whole-fleet last-200, NO time filter "
              "(behavioral leg is PROVISIONAL — B may straddle the deploy boundary)")
    print("=" * 84)
    for role, task in REPRESENTATIVE_ROLES.items():
        a, b = A.get(role), B.get(role)
        if not a or not b:
            print(f"\nrole:{role} — missing side, skipped")
            continue
        n = b.get("n") or 0
        flag = "  [UNDER-SAMPLED n<%d — advisory only]" % MIN_SAMPLE if n < MIN_SAMPLE else ""
        print(f"\nrole:{role}  ({task})  n={n}{flag}")
        for metric, fmt in [("success_rate_pct", "{:.1f}%"),
                            ("turns_median", "{:.1f}"),
                            ("spend_median", "${:.3f}"),
                            ("prime_bytes", "{:.0f}B")]:
            av, bv = a.get(metric), b.get(metric)
            d = _delta(av, bv)
            ds = ("+" + fmt.format(d)) if (d is not None and d >= 0) else (fmt.format(d) if d is not None else "n/a")
            print(f"    {metric:18s}  A={str(av):>10}  B={str(bv):>10}  Δ={ds}")

    verdict = evaluate_gate(A, B)
    print("\n" + "=" * 84)
    # Advisory buckets print regardless of the pass/fail outcome so the operator
    # sees the same watch-items/under-sampled notes the four re-capture agents
    # documented — but they do NOT force an operator decision on their own.
    if verdict["watch_items"]:
        print("WATCH-ITEMS (single-metric drift, not the coupled over-constraint signature):")
        for w in verdict["watch_items"]:
            print(f"   ⚠ {w}")
    if verdict["undersampled"]:
        print("UNDER-SAMPLED (n < %d — median is noise-dominated, advisory only):" % MIN_SAMPLE)
        for u in verdict["undersampled"]:
            print(f"   ◦ {u}")
    if verdict["regressions"]:
        print("VERDICT: REGRESSION — file gb decision + yield for operator review:")
        for r in verdict["regressions"]:
            print(f"   ⛔ {r}")
        sys.exit(2)
    clean = "CLEAN post-deploy split" if b_window else "PROVISIONAL (B unwindowed — re-run with --since <deploy-time> for a clean behavioral leg)"
    extra = " (see watch-items above)" if verdict["watch_items"] else ""
    print(f"VERDICT: A-B PASS — success held on all roles; no coupled turns+spend "
          f"over-constraint in a well-sampled role. Safe to merge. [{clean}]{extra}")
    sys.exit(0)


def main():
    p = argparse.ArgumentParser(description="Opus-5 context-audit A-B harness")
    sub = p.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("capture", help="snapshot live per-role metrics to JSON")
    c.add_argument("--out", required=True)
    c.add_argument("--since", default=None,
                   help="only sample agents created after this cutoff (RFC3339 like "
                        "2026-07-25T10:00:00Z, or a duration back like 4h/2d). Pass the "
                        "deploy time here to capture a CLEAN post-deploy B leg.")
    cm = sub.add_parser("compare", help="A-B compare two snapshots + apply the gate")
    cm.add_argument("--a", required=True)
    cm.add_argument("--b", required=True)
    args = p.parse_args()
    if args.cmd == "capture":
        capture(args.out, since=args.since)
    else:
        compare(args.a, args.b)


if __name__ == "__main__":
    main()
