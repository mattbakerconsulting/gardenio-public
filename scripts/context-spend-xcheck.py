#!/usr/bin/env python3
"""context-spend-xcheck.py — validate context-load reductions against real LiteLLM spend.

Task ka-0000004868 (parent epic ka-0000003733 Part B / context-audit ka-0000007337).

THE QUESTION
  The context-crisis work (advice budgets, gb prime --size) and the qualitative
  context audit (CLAUDE.md trims, SessionStart dedup) both shrink the *bytes* an
  agent boots with. This tool answers whether that byte reduction is backed by
  REAL SPEND — the epic's "reconstruct cost two ways as a self-validating
  cross-check" (LiteLLM authoritative USD vs a turns x token-load proxy).

THE TWO SIGNALS (both already on every session bead, written by coop at close)
  A. Authoritative spend  -> settled_cost_usd. The controller's spend reconciler
     writes this from LiteLLM /spend/tags as a per-agent baseline-delta
     (reconcile_session_spend.go). It is the real dollars LiteLLM billed.
  B. Context-load proxy    -> cache_read_tokens / request_count. cache_read is the
     resident context re-read on every turn; it is exactly what boot/turn trimming
     shrinks. This is the "byte count" the audit optimizes, in tokens.

  cost/turn = settled_cost_usd / request_count      (real money per turn)
  ctx/turn  = cache_read_tokens / request_count     (resident context per turn)

WHAT "VALIDATED" MEANS HERE (read the caveats the tool prints — they are the finding)
  1. Structural: price the tokens with the known Opus ratio map and show which
     term dominates cost. cache_read dominating => trimming resident context is
     dollar-for-dollar real, not a vanity byte metric.
  2. Directional: cost/turn and ctx/turn should move together across the fleet.
  3. The tool is HONEST about two confounds that make a naive $/turn before/after
     misleading, and refuses to launder them into a headline number:
       - the opus-4-8 -> opus-5 default flip (2026-07-24 22:40Z) changes the
         LiteLLM price map right at the audit cutover;
       - the fleet runs subscription/-oauth keys, so LiteLLM prices tokens well
         below public API sticker and per-session settled_cost is noisy.

USAGE
  # snapshot live session beads (fetch once, project many — advice ka-0000002706)
  gb session list --all --limit 20000 --json > /tmp/sessions.json
  python3 scripts/context-spend-xcheck.py /tmp/sessions.json
  python3 scripts/context-spend-xcheck.py /tmp/sessions.json --project gardenio --json

Deliberately dependency-free (stdlib only) so it runs in any agent pod.
"""
import argparse
import json
import sys
from collections import defaultdict

# opus-4-8 -> opus-5 fleet default flip (git: feat ka-0000003576, 2026-07-24 22:40Z).
# Segment on this so a per-token price change is never mistaken for a context change.
MODEL_FLIP = "2026-07-24T22:40:00Z"
# Qualitative context-audit remediations (ka-0000007337 children) landed 04:30-06:35Z
# on 2026-07-25 — CLAUDE.md trims, SessionStart dedup, advice-lint over-constraint.
AUDIT_CUTOVER = "2026-07-25T05:00:00Z"

# Claude Opus public per-MTok prices (5-minute cache TTL): the KNOWN ratio map used
# only for the structural reconstruction. input $5 / output $25 (5x) /
# cache_read $0.50 (0.1x) / cache_write $6.25 (1.25x). These are sticker prices;
# the fleet's -oauth keys bill far below them, which is why reconstruction is a
# STRUCTURAL check (which term dominates), not a $-accurate one.
P_IN, P_OUT, P_CR, P_CW = 5.00, 25.00, 0.50, 6.25
MTOK = 1_000_000

# Capacity/build roles that actually pay the boot-context tax we care about.
CAPCREW_ROLES = {
    "gardener", "crew", "gardener-release-owner", "gardener-delivery",
    "gardener-bugfix", "plan-gardener", "crew-fable", "crew-ultralite",
}


def num(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return 0.0


def load(path):
    with open(path) as f:
        rows = json.load(f)
    if not isinstance(rows, list):
        sys.exit(f"expected a JSON array from `gb session list --json`, got {type(rows).__name__}")
    return rows


def prep(rows, project, roles):
    out = []
    for r in rows:
        fl = r.get("fields", {})
        if fl.get("ended_at") is None:
            continue
        if project and fl.get("project") != project:
            continue
        if roles and fl.get("role") not in roles:
            continue
        req = num(fl.get("request_count"))
        if req <= 0:
            continue
        out.append({
            "ts": r.get("created_at", ""),
            "day": r.get("created_at", "")[:10],
            "role": fl.get("role", "?"),
            "req": req,
            "cost": num(fl.get("settled_cost_usd")),
            "cr": num(fl.get("cache_read_tokens")),
            "cw": num(fl.get("cache_write_tokens")),
            "in": num(fl.get("input_tokens")),
            "out": num(fl.get("output_tokens")),
        })
    return out


def fleet(sessions):
    """Fleet ratios: sum then divide. Never mean-of-ratios (tiny sessions skew it)."""
    n = len(sessions)
    req = sum(s["req"] for s in sessions)
    cost = sum(s["cost"] for s in sessions)
    cr = sum(s["cr"] for s in sessions)
    cw = sum(s["cw"] for s in sessions)
    return {
        "sessions": n, "turns": req, "cost": cost,
        "cost_per_turn": cost / req if req else 0.0,
        "ctx_per_turn": cr / req if req else 0.0,
        "cw_per_turn": cw / req if req else 0.0,
    }


def recon(s):
    return (P_IN * s["in"] + P_OUT * s["out"] + P_CR * s["cr"] + P_CW * s["cw"]) / MTOK


def pearson(xs, ys):
    n = len(xs)
    if n < 2:
        return 0.0
    mx, my = sum(xs) / n, sum(ys) / n
    cov = sum((a - mx) * (b - my) for a, b in zip(xs, ys))
    sx = sum((a - mx) ** 2 for a in xs) ** 0.5
    sy = sum((b - my) ** 2 for b in ys) ** 0.5
    return cov / (sx * sy) if sx and sy else 0.0


def report_text(capcrew, cutover):
    out = []
    w = out.append

    # --- daily trend ---
    w("=" * 76)
    w("DAILY TREND — gardenio gardener/crew (ended sessions, request_count>0)")
    w("cost columns blank before settled_cost was populated fleet-wide (~07-16)")
    w("=" * 76)
    w(f"{'day':<12}{'sess':>6}{'turns':>8}{'$/turn':>10}{'ctxRead/turn':>14}{'cWrite/turn':>13}")
    by_day = defaultdict(list)
    for s in capcrew:
        by_day[s["day"]].append(s)
    for day in sorted(by_day):
        a = fleet(by_day[day])
        cpt = f"{a['cost_per_turn']:.4f}" if a["cost"] > 0 else "  --"
        w(f"{day:<12}{a['sessions']:>6}{int(a['turns']):>8}{cpt:>10}"
          f"{int(a['ctx_per_turn']):>14}{int(a['cw_per_turn']):>13}")

    # --- before/after the qualitative audit ---
    w("")
    w("=" * 76)
    w(f"BEFORE / AFTER qualitative context audit (ka-0000007337), cutover {cutover}")
    w("=" * 76)
    before = [s for s in capcrew if s["ts"] < cutover]
    after = [s for s in capcrew if s["ts"] >= cutover]
    b, a = fleet(before), fleet(after)
    w(f"{'metric':<16}{'BEFORE':>14}{'AFTER':>14}{'delta%':>10}")
    for key, fmt, label in [
        ("sessions", "{:.0f}", "sessions"),
        ("turns", "{:.0f}", "turns"),
        ("ctx_per_turn", "{:.0f}", "ctxRead/turn"),
        ("cw_per_turn", "{:.0f}", "cWrite/turn"),
        ("cost_per_turn", "{:.4f}", "$/turn"),
    ]:
        bv, av = b[key], a[key]
        pct = (av - bv) / bv * 100 if bv else 0.0
        w(f"{label:<16}{fmt.format(bv):>14}{fmt.format(av):>14}{pct:>+9.1f}%")
    w("")
    w(f"  CAVEAT — after-window is only {a['sessions']} sessions (<2h old at snapshot);")
    w("  treat these deltas as directional, not significant. And the opus-4-8->opus-5")
    w(f"  model flip ({MODEL_FLIP}) lands ~6h before this cutover, so raw $/turn")
    w("  mixes a price change with the context change. ctxRead/turn (model-price-")
    w("  independent) is the trustworthy column here.")

    # --- two-way cross-check ---
    w("")
    w("=" * 76)
    w("TWO-WAY CROSS-CHECK — does the cheap proxy move WITH authoritative $?")
    w("=" * 76)
    paid = [s for s in capcrew if s["cost"] > 0 and s["day"] >= "2026-07-16"]
    dd = defaultdict(list)
    for s in paid:
        dd[s["day"]].append(s)
    days = sorted(dd)
    cser, xser = [], []
    for day in days:
        a = fleet(dd[day])
        cser.append(a["cost_per_turn"])
        xser.append(a["ctx_per_turn"])
    r = pearson(cser, xser)
    w(f"Pearson r(daily $/turn, daily ctxRead/turn) = {r:+.3f}  over {len(days)} days")
    w("  r>0 => the byte proxy the audit optimizes tracks real LiteLLM dollars.")

    # --- structural reconstruction (opus-4-8 era, known price map) ---
    w("")
    w("=" * 76)
    w("STRUCTURAL RECONSTRUCTION — opus-4-8 era only (known Opus price map)")
    w("=" * 76)
    o48 = [s for s in capcrew if s["ts"] < MODEL_FLIP and s["cost"] > 0]
    if o48:
        tot_settled = sum(s["cost"] for s in o48)
        tot_recon = sum(recon(s) for s in o48)
        w(f"n={len(o48)}  settled ${tot_settled:,.2f}  reconstructed(sticker) ${tot_recon:,.2f}")
        w(f"  reconstruction/settled = {tot_recon / tot_settled:.1f}x  <- fleet -oauth keys")
        w("  bill FAR below public Opus sticker; sticker recon overstates ~10x. This is")
        w("  why authoritative settled_cost is primary and sticker recon is STRUCTURAL only.")
        tot_req = sum(s["req"] for s in o48)
        comp = {
            "cache_read (resident context)": P_CR * sum(s["cr"] for s in o48) / MTOK,
            "output": P_OUT * sum(s["out"] for s in o48) / MTOK,
            "cache_write": P_CW * sum(s["cw"] for s in o48) / MTOK,
            "input (fresh)": P_IN * sum(s["in"] for s in o48) / MTOK,
        }
        total = sum(comp.values())
        w("  cost attribution by token class (share of modeled spend):")
        for k in sorted(comp, key=lambda x: -comp[x]):
            w(f"    {k:<32}{comp[k] / total * 100:5.1f}%")
        w(f"  => cache_read is {comp['cache_read (resident context)'] / total * 100:.0f}% of spend:")
        w("     resident context re-read every turn is the dominant cost term, so")
        w("     trimming boot/turn context is dollar-for-dollar real.")
    else:
        w("no opus-4-8-era cost sessions in this snapshot")

    return "\n".join(out)


def report_json(capcrew, cutover):
    before = [s for s in capcrew if s["ts"] < cutover]
    after = [s for s in capcrew if s["ts"] >= cutover]
    paid = [s for s in capcrew if s["cost"] > 0 and s["day"] >= "2026-07-16"]
    dd = defaultdict(list)
    for s in paid:
        dd[s["day"]].append(s)
    days = sorted(dd)
    cser = [fleet(dd[d])["cost_per_turn"] for d in days]
    xser = [fleet(dd[d])["ctx_per_turn"] for d in days]
    o48 = [s for s in capcrew if s["ts"] < MODEL_FLIP and s["cost"] > 0]
    tot_settled = sum(s["cost"] for s in o48) or 0.0
    tot_recon = sum(recon(s) for s in o48) or 0.0
    tot_req = sum(s["req"] for s in o48) or 1
    attribution = {
        "cache_read": P_CR * sum(s["cr"] for s in o48) / MTOK,
        "output": P_OUT * sum(s["out"] for s in o48) / MTOK,
        "cache_write": P_CW * sum(s["cw"] for s in o48) / MTOK,
        "input": P_IN * sum(s["in"] for s in o48) / MTOK,
    }
    return {
        "cutover": cutover,
        "model_flip": MODEL_FLIP,
        "before": fleet(before),
        "after": fleet(after),
        "xcheck_pearson_r": pearson(cser, xser),
        "xcheck_days": len(days),
        "recon": {
            "n": len(o48),
            "settled_usd": tot_settled,
            "reconstructed_sticker_usd": tot_recon,
            "recon_over_settled": (tot_recon / tot_settled) if tot_settled else None,
            "attribution_share": {k: (v / sum(attribution.values()) if sum(attribution.values()) else 0)
                                  for k, v in attribution.items()},
        },
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("sessions_json", help="path to `gb session list --all --json` capture")
    ap.add_argument("--project", default="gardenio", help="filter to one project (default gardenio)")
    ap.add_argument("--all-roles", action="store_true",
                    help="include every role, not just capacity/build roles")
    ap.add_argument("--cutover", default=AUDIT_CUTOVER,
                    help=f"before/after split (RFC3339, default {AUDIT_CUTOVER})")
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    args = ap.parse_args()

    rows = load(args.sessions_json)
    roles = None if args.all_roles else CAPCREW_ROLES
    capcrew = prep(rows, args.project, roles)
    if not capcrew:
        sys.exit("no matching ended sessions with request_count>0 in the snapshot")

    if args.json:
        print(json.dumps(report_json(capcrew, args.cutover), indent=2))
    else:
        print(report_text(capcrew, args.cutover))


if __name__ == "__main__":
    main()
