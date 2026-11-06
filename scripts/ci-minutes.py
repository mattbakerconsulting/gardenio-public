#!/usr/bin/env python3
"""ci-minutes.py -- runner-minutes per day, workflow, event and job.

Every phase of the CI minutes plan (gd-WFMvg9sDKE) claims a saving in
runner-minutes; this is the one measurement they are all compared with.

    scripts/ci-minutes.py --repo mattbakerconsulting/gardenio \\
        --from 2026-10-25 --to 2026-10-27 [--sample 30] [--seed 7] \\
        [--asg gardenio-k3s-ci --region us-east-1 --profile gardenio] [--json]

How it counts:

- Runs are listed with `gh api .../actions/runs -f created=A..B --paginate` in
  windows of 3 hours. The runs search silently returns at most 1,000 results
  for one query, so a window whose total_count is 1,000 or more is split in
  half until each part is under the cap. Runs are deduplicated by id.
- For each (workflow, event) a seeded random sample of up to --sample
  non-skipped runs (0 = every run) has its jobs fetched with filter=all, so
  re-run attempts count. A job's minutes are completed_at - started_at; a
  skipped job, one that never started or finished, and one whose end is before
  its start count nothing.
- Daily minutes = non-skipped runs that day x the sample's minutes per run.
  A skipped run does no work, so it is not multiplied.
- A job whose runner label starts with ubuntu- / windows- / macos- is
  GitHub-hosted (billed by GitHub) and is reported apart from the self-hosted
  ARC minutes.
- "ci runs per PR" is non-skipped ci pull_request runs over the distinct head
  branches of all pull_request runs that day.

Stdlib only; needs `gh` signed in, and `aws` only with --asg.
Tests: scripts/ci-minutes_test.py (fixtures, no network).
"""

import argparse
import concurrent.futures
import datetime
import json
import random
import subprocess
import sys

SEARCH_CAP = 1000  # the runs search returns at most this many results per query
WINDOW = datetime.timedelta(hours=3)
ONE_SECOND = datetime.timedelta(seconds=1)
HOSTED_PREFIXES = ("ubuntu-", "windows-", "macos-")
TS = "%Y-%m-%dT%H:%M:%SZ"


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def parse_ts(s):
    if not s:
        return None
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def fmt_ts(t):
    return t.astimezone(datetime.timezone.utc).strftime(TS)


# --- pure functions (tested by ci-minutes_test.py) -------------------------


def job_minutes(job):
    """Minutes a job held its runner, or None when it should not count."""
    if job.get("conclusion") == "skipped":
        return None
    start = parse_ts(job.get("started_at"))
    end = parse_ts(job.get("completed_at"))
    if start is None or end is None or end < start:
        return None
    return (end - start).total_seconds() / 60.0


def split_window(start, end, total):
    """[(start, end)] when total is under the search cap, else two halves.

    Bounds are inclusive and whole seconds, so the halves meet without
    overlapping. A one-second window cannot be split and is returned as is.
    """
    if total < SEARCH_CAP or end <= start:
        return [(start, end)]
    secs = int((end - start).total_seconds())
    mid = start + datetime.timedelta(seconds=secs // 2)
    return [(start, mid), (mid + ONE_SECOND, end)]


def plan_windows(start, end, count):
    """Split [start, end] until count(a, b) is under the cap for each part.

    Returns [(a, b, count)] in time order.
    """
    out = []
    todo = [(start, end)]
    while todo:
        a, b = todo.pop(0)
        n = count(a, b)
        parts = split_window(a, b, n)
        if len(parts) == 1:
            if n >= SEARCH_CAP:
                log("warning: %s..%s still reports %d runs; the listing is capped"
                    % (fmt_ts(a), fmt_ts(b), n))
            out.append((a, b, n))
        else:
            todo[0:0] = parts
    return out


def day_windows(first, last):
    """Inclusive 3-hour windows covering the UTC days first..last."""
    out = []
    day = first
    while day <= last:
        t = datetime.datetime(day.year, day.month, day.day, tzinfo=datetime.timezone.utc)
        for i in range(24 // 3):
            a = t + i * WINDOW
            out.append((a, a + WINDOW - ONE_SECOND))
        day += datetime.timedelta(days=1)
    return out


def is_hosted(labels):
    return any(str(l).startswith(HOSTED_PREFIXES) for l in labels or ())


def summarize_jobs(jobs_by_run):
    """Minutes per run over a sample {run_id: [job, ...]}.

    Returns {"sampled", "runner", "hosted", "jobs"}: runner is self-hosted and
    hosted is GitHub-hosted minutes per sampled run; each jobs row gives the
    job's minutes per sampled run and how many sampled runs it ran in.
    """
    n = len(jobs_by_run)
    per_job = {}
    runner = hosted = 0.0
    for rid, jobs in jobs_by_run.items():
        for j in jobs:
            m = job_minutes(j)
            if m is None:
                continue
            labels = j.get("labels") or []
            h = is_hosted(labels)
            row = per_job.setdefault(j.get("name") or "?", {
                "label": ",".join(labels) or "?", "hosted": h, "minutes": 0.0, "runs": set(),
            })
            row["minutes"] += m
            row["runs"].add(rid)
            if h:
                hosted += m
            else:
                runner += m
    rows = [
        {
            "job": name,
            "label": r["label"],
            "hosted": r["hosted"],
            "min_per_run": r["minutes"] / n,
            "runs_with_job": len(r["runs"]),
        }
        for name, r in per_job.items()
    ]
    rows.sort(key=lambda r: (-r["min_per_run"], r["job"]))
    return {
        "sampled": n,
        "runner": runner / n if n else 0.0,
        "hosted": hosted / n if n else 0.0,
        "jobs": rows,
    }


def _day(r):
    return (r.get("created_at") or "")[:10]


def _key(r):
    return (r.get("name") or "?", r.get("event") or "?")


def estimate_daily(runs, per_run):
    """Runs x minutes per run, per UTC day.

    per_run maps (workflow, event) to {"runner": min/run, "hosted": min/run}.
    Skipped runs are not multiplied. Returns {day: {"rows": {(wf, ev): {"runs",
    "runner_min", "hosted_min"}}, "runner_total", "hosted_total"}}.
    """
    out = {}
    for r in runs:
        if r.get("conclusion") == "skipped":
            continue
        d = out.setdefault(_day(r), {"rows": {}, "runner_total": 0.0, "hosted_total": 0.0})
        row = d["rows"].setdefault(_key(r), {"runs": 0, "runner_min": 0.0, "hosted_min": 0.0})
        row["runs"] += 1
    for d in out.values():
        for key, row in d["rows"].items():
            pr = per_run.get(key) or {}
            row["runner_min"] = row["runs"] * pr.get("runner", 0.0)
            row["hosted_min"] = row["runs"] * pr.get("hosted", 0.0)
            d["runner_total"] += row["runner_min"]
            d["hosted_total"] += row["hosted_min"]
    return out


def runs_per_day(runs):
    """{day: {(workflow, event): {conclusion: count}}}; None is in_progress."""
    out = {}
    for r in runs:
        c = r.get("conclusion") or "in_progress"
        by = out.setdefault(_day(r), {}).setdefault(_key(r), {})
        by[c] = by.get(c, 0) + 1
    return out


def pr_stats(runs, ci_name="ci"):
    """{day: {"branches", "ci_runs", "ci_per_pr"}} for pull_request runs."""
    branches, ci_runs = {}, {}
    for r in runs:
        if r.get("event") != "pull_request":
            continue
        d = _day(r)
        branches.setdefault(d, set()).add(r.get("head_branch"))
        if r.get("name") == ci_name and r.get("conclusion") != "skipped":
            ci_runs[d] = ci_runs.get(d, 0) + 1
    out = {}
    for d in sorted(branches):
        b, c = len(branches[d]), ci_runs.get(d, 0)
        out[d] = {"branches": b, "ci_runs": c, "ci_per_pr": c / b if b else 0.0}
    return out


# --- gh / aws --------------------------------------------------------------


def gh(args):
    p = subprocess.run(["gh", "api"] + args, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError("gh api %s: %s" % (" ".join(args), p.stderr.strip()))
    return p.stdout


def jsonl(text):
    return [json.loads(line) for line in text.splitlines() if line.strip()]


RUN_FIELDS = ".workflow_runs[] | {id, name, event, conclusion, status, created_at, head_branch, run_attempt}"


def count_runs(repo, a, b):
    out = gh(["-X", "GET", "repos/%s/actions/runs" % repo,
              "-f", "created=%s..%s" % (fmt_ts(a), fmt_ts(b)), "-f", "per_page=1",
              "--jq", ".total_count"])
    return int(out.strip() or 0)


def list_runs(repo, a, b):
    return jsonl(gh(["-X", "GET", "repos/%s/actions/runs" % repo,
                     "-f", "created=%s..%s" % (fmt_ts(a), fmt_ts(b)), "-f", "per_page=100",
                     "--paginate", "--jq", RUN_FIELDS]))


def fetch_jobs(repo, rid):
    return jsonl(gh(["repos/%s/actions/runs/%s/jobs?filter=all&per_page=100" % (repo, rid),
                     "--paginate", "--jq",
                     ".jobs[] | {name, conclusion, status, started_at, completed_at, labels}"]))


def collect_runs(repo, first, last):
    runs = {}
    for a, b in day_windows(first, last):
        for wa, wb, n in plan_windows(a, b, lambda x, y: count_runs(repo, x, y)):
            got = list_runs(repo, wa, wb)
            log("  %s..%s: %d runs (total_count %d)" % (fmt_ts(wa), fmt_ts(wb), len(got), n))
            for r in got:
                runs[r["id"]] = r
    lo, hi = first.isoformat(), last.isoformat()
    return sorted((r for r in runs.values() if lo <= _day(r) <= hi), key=lambda r: r["id"])


def sample_runs(runs, n, seed):
    """{(workflow, event): [run_id, ...]} -- seeded, non-skipped, up to n each."""
    groups = {}
    for r in runs:
        if r.get("conclusion") != "skipped":
            groups.setdefault(_key(r), []).append(r["id"])
    rng = random.Random(seed)
    out = {}
    for key in sorted(groups):
        ids = sorted(groups[key])
        out[key] = ids if n == 0 or len(ids) <= n else sorted(rng.sample(ids, n))
    return out


def asg_stats(asg, region, profile, first, last):
    cmd = ["aws", "cloudwatch", "get-metric-statistics",
           "--namespace", "AWS/AutoScaling", "--metric-name", "GroupInServiceInstances",
           "--dimensions", "Name=AutoScalingGroupName,Value=%s" % asg,
           "--start-time", "%sT00:00:00Z" % first.isoformat(),
           "--end-time", "%sT00:00:00Z" % (last + datetime.timedelta(days=1)).isoformat(),
           "--period", "86400", "--statistics", "Average", "Maximum",
           "--region", region, "--output", "json"]
    if profile:
        cmd += ["--profile", profile]
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        log("warning: aws cloudwatch failed, no ASG table: %s" % p.stderr.strip())
        return None
    pts = json.loads(p.stdout).get("Datapoints", [])
    # The aws CLI prints timestamps in the caller's local zone (2026-10-25T17:00:00-07:00
    # is the 10-26 UTC bucket), so convert to UTC before taking the day.
    def utc_day(ts):
        return datetime.datetime.fromisoformat(ts.replace("Z", "+00:00")).astimezone(
            datetime.timezone.utc).date().isoformat()
    return [{"day": utc_day(pt["Timestamp"]), "avg": pt["Average"], "max": pt["Maximum"]}
            for pt in sorted(pts, key=lambda pt: pt["Timestamp"])]


# --- report ----------------------------------------------------------------


MAIN_CONCLUSIONS = ("success", "failure", "cancelled", "skipped")


def build_report(args, runs, per_pair):
    per_run = {k: {"runner": v["runner"], "hosted": v["hosted"]} for k, v in per_pair.items()}
    counts = runs_per_day(runs)
    daily = estimate_daily(runs, per_run)
    nonskipped = {}
    for r in runs:
        if r.get("conclusion") != "skipped":
            nonskipped[_key(r)] = nonskipped.get(_key(r), 0) + 1
    rep = {
        "repo": args.repo, "from": args.date_from, "to": args.date_to,
        "sample": args.sample, "seed": args.seed, "total_runs": len(runs),
        "runs_per_day": [
            {"day": d, "workflow": k[0], "event": k[1], "runs": sum(c.values()), "conclusions": c}
            for d in sorted(counts) for k, c in sorted(counts[d].items())
        ],
        "per_run": [
            {"workflow": k[0], "event": k[1], "runs": nonskipped.get(k, 0),
             "sampled": v["sampled"], "min_per_run": v["runner"],
             "hosted_min_per_run": v["hosted"], "jobs": v["jobs"]}
            for k, v in sorted(per_pair.items())
        ],
        "daily": [
            {"day": d, "runner_total": v["runner_total"], "hosted_total": v["hosted_total"],
             "rows": [{"workflow": k[0], "event": k[1], **row}
                      for k, row in sorted(v["rows"].items(),
                                           key=lambda kv: -(kv[1]["runner_min"] + kv[1]["hosted_min"]))]}
            for d, v in sorted(daily.items())
        ],
        "prs": [{"day": d, **v} for d, v in pr_stats(runs).items()],
    }
    if args.asg:
        rep["asg"] = {"name": args.asg, "days": asg_stats(args.asg, args.region, args.profile,
                                                          parse_day(args.date_from),
                                                          parse_day(args.date_to))}
    return rep


def table(headers, rows):
    lines = ["| " + " | ".join(headers) + " |", "|" + "|".join("---" for _ in headers) + "|"]
    lines += ["| " + " | ".join(str(c) for c in row) + " |" for row in rows]
    return "\n".join(lines)


def f1(x):
    return "%.1f" % x


def n0(x):
    return "{:,.0f}".format(x)


def print_text(rep):
    print("# CI minutes: %s, %s..%s (sample %s, seed %s)\n"
          % (rep["repo"], rep["from"], rep["to"], rep["sample"], rep["seed"]))
    print("Total runs: %s\n" % "{:,}".format(rep["total_runs"]))

    print("## 1. Runs per day by workflow, event and conclusion\n")
    rows, day_tot = [], {}
    for r in rep["runs_per_day"]:
        c = r["conclusions"]
        other = sum(v for k, v in c.items() if k not in MAIN_CONCLUSIONS)
        rows.append([r["day"], r["workflow"], r["event"], r["runs"]]
                    + [c.get(k, 0) for k in MAIN_CONCLUSIONS] + [other])
        day_tot[r["day"]] = day_tot.get(r["day"], 0) + r["runs"]
    for d, n in sorted(day_tot.items()):
        rows.append([d, "**all**", "", "**%d**" % n, "", "", "", "", ""])
    print(table(["Day", "Workflow", "Event", "Runs"] + list(MAIN_CONCLUSIONS) + ["other"], rows))

    print("\n## 2. Minutes per run by workflow and event (sampled, non-skipped runs)\n")
    print(table(["Workflow", "Event", "Runs", "Sampled", "Self-hosted min/run", "GitHub-hosted min/run"],
                [[p["workflow"], p["event"], p["runs"], p["sampled"], f1(p["min_per_run"]),
                  f1(p["hosted_min_per_run"])] for p in rep["per_run"]]))
    print("\nPer job (min/run over the sample; ran on k of n sampled runs):\n")
    rows = []
    for p in rep["per_run"]:
        for j in p["jobs"]:
            rows.append(["%s / %s" % (p["workflow"], p["event"]), j["job"],
                         j["label"] + (" (GitHub-hosted)" if j["hosted"] else ""),
                         f1(j["min_per_run"]), "%d of %d" % (j["runs_with_job"], p["sampled"])])
    print(table(["Workflow / event", "Job", "Runner", "Min/run", "Ran on"], rows))

    print("\n## 3. Estimated runner-minutes per day (non-skipped runs x min/run)\n")
    rows = []
    per = {(p["workflow"], p["event"]): p for p in rep["per_run"]}
    for d in rep["daily"]:
        for r in d["rows"]:
            p = per.get((r["workflow"], r["event"]), {})
            rows.append([d["day"], r["workflow"], r["event"], r["runs"],
                         f1(p.get("min_per_run", 0.0) + p.get("hosted_min_per_run", 0.0)),
                         n0(r["runner_min"]), n0(r["hosted_min"])])
        rows.append([d["day"], "**total**", "", "", "", "**%s**" % n0(d["runner_total"]),
                     "**%s**" % n0(d["hosted_total"])])
    print(table(["Day", "Workflow", "Event", "Runs", "Min/run", "Self-hosted min/day",
                 "GitHub-hosted min/day"], rows))

    print("\n## 4. PRs per day\n")
    print(table(["Day", "Distinct PR branches", "ci PR runs", "ci runs per PR"],
                [[p["day"], p["branches"], p["ci_runs"], f1(p["ci_per_pr"])] for p in rep["prs"]]))

    if "asg" in rep:
        print("\n## 5. ASG %s, GroupInServiceInstances\n" % rep["asg"]["name"])
        days = rep["asg"]["days"]
        if days is None:
            print("(aws cloudwatch failed; see stderr)")
        else:
            print(table(["Day", "Avg nodes", "Max nodes"],
                        [[a["day"], "%.2f" % a["avg"], "%.0f" % a["max"]] for a in days]))


def parse_day(s):
    return datetime.date.fromisoformat(s)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--repo", default="mattbakerconsulting/gardenio")
    ap.add_argument("--from", dest="date_from", required=True, help="first UTC day, YYYY-MM-DD")
    ap.add_argument("--to", dest="date_to", required=True, help="last UTC day (inclusive)")
    ap.add_argument("--sample", type=int, default=30,
                    help="runs sampled per workflow and event (0 = every run)")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--asg", help="ASG for table 5, e.g. gardenio-k3s-ci")
    ap.add_argument("--region", default="us-east-1")
    ap.add_argument("--profile")
    ap.add_argument("--workers", type=int, default=8, help="parallel job fetches")
    ap.add_argument("--json", action="store_true", help="print one JSON object instead of tables")
    args = ap.parse_args(argv)
    first, last = parse_day(args.date_from), parse_day(args.date_to)
    if last < first:
        ap.error("--to is before --from")

    log("listing runs %s..%s" % (first, last))
    runs = collect_runs(args.repo, first, last)
    log("%d runs" % len(runs))

    samples = sample_runs(runs, args.sample, args.seed)
    todo = [(key, rid) for key, ids in samples.items() for rid in ids]
    log("fetching jobs for %d sampled runs" % len(todo))
    jobs = {key: {} for key in samples}
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.workers)) as ex:
        futs = {ex.submit(fetch_jobs, args.repo, rid): (key, rid) for key, rid in todo}
        for fut in concurrent.futures.as_completed(futs):
            key, rid = futs[fut]
            try:
                jobs[key][rid] = fut.result()
            except RuntimeError as e:
                log("warning: dropping run %s from the sample: %s" % (rid, e))
    per_pair = {key: summarize_jobs(by_run) for key, by_run in jobs.items()}

    rep = build_report(args, runs, per_pair)
    if args.json:
        json.dump(rep, sys.stdout, indent=1, sort_keys=True)
        print()
    else:
        print_text(rep)
    return 0


if __name__ == "__main__":
    sys.exit(main())
