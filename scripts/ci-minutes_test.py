#!/usr/bin/env python3
"""Self-test for ci-minutes.py. Fixtures only, no network.

Every later phase of the CI minutes plan (gd-WFMvg9sDKE) quotes a saving in
runner-minutes from this script, so the arithmetic is what is under test:

- job_minutes drops a skipped job, a job that never started, and a job whose
  completed_at is before its started_at (a re-run can leave that shape).
- split_window halves a window the runs search reports at 1,000 results,
  because the API silently returns only the first 1,000.
- a GitHub-hosted job (ubuntu-*) is kept apart from self-hosted minutes, since
  only the former is billed by GitHub.

T_positive_control is the one that matters most: a fixture day with a known
total that must come out EXACTLY. A filter that matches nothing reports 0.0
minutes in the same format as a quiet day, so a test that only checks for
exclusions cannot tell a correct script from one that counts nothing.

Usage:  python3 scripts/ci-minutes_test.py
"""

import datetime
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

_spec = importlib.util.spec_from_file_location(
    "ci_minutes", os.path.join(HERE, "ci-minutes.py")
)
cm = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(cm)

PASS = 0
FAIL = 0


def check(name, got, want):
    global PASS, FAIL
    if got == want:
        PASS += 1
        print("ok   %s" % name)
    else:
        FAIL += 1
        print("FAIL %s\n     got:  %r\n     want: %r" % (name, got, want))


def dt(s):
    return datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(
        tzinfo=datetime.timezone.utc
    )


def job(name, start, end, conclusion="success", labels=("gardenio-k3s",)):
    return {
        "name": name,
        "conclusion": conclusion,
        "status": "completed",
        "started_at": start,
        "completed_at": end,
        "labels": list(labels),
    }


def run(rid, name, event, created, conclusion="success", branch="main"):
    return {
        "id": rid,
        "name": name,
        "event": event,
        "conclusion": conclusion,
        "created_at": created,
        "head_branch": branch,
    }


# --- job_minutes -----------------------------------------------------------

check(
    "job_minutes: a ten-minute job is 10.0",
    cm.job_minutes(job("build", "2026-10-27T01:00:00Z", "2026-10-27T01:10:00Z")),
    10.0,
)
check(
    "job_minutes: seconds are kept, not rounded away",
    cm.job_minutes(job("gate", "2026-10-27T01:00:00Z", "2026-10-27T01:00:30Z")),
    0.5,
)
check(
    "job_minutes: a skipped job counts nothing",
    cm.job_minutes(
        job("browser", "2026-10-27T01:00:00Z", "2026-10-27T01:00:00Z", "skipped")
    ),
    None,
)
check(
    "job_minutes: a null started_at counts nothing",
    cm.job_minutes(job("build", None, "2026-10-27T01:10:00Z")),
    None,
)
check(
    "job_minutes: a null completed_at (still running) counts nothing",
    cm.job_minutes(job("build", "2026-10-27T01:00:00Z", None, conclusion=None)),
    None,
)
check(
    "job_minutes: an inverted start and end counts nothing",
    cm.job_minutes(job("build", "2026-10-27T01:10:00Z", "2026-10-27T01:00:00Z")),
    None,
)
check(
    "job_minutes: a failed job still used its runner",
    cm.job_minutes(
        job("build", "2026-10-27T01:00:00Z", "2026-10-27T01:06:00Z", "failure")
    ),
    6.0,
)

# --- split_window ----------------------------------------------------------

S, E = dt("2026-10-27T00:00:00Z"), dt("2026-10-27T02:59:59Z")
check("split_window: 999 results stays one window", cm.split_window(S, E, 999), [(S, E)])
halves = cm.split_window(S, E, 1000)
check("split_window: exactly 1,000 results splits in two", len(halves), 2)
check("split_window: halves start and end where the window did",
      (halves[0][0], halves[1][1]), (S, E))
check("split_window: halves neither overlap nor leave a gap",
      halves[1][0] - halves[0][1], datetime.timedelta(seconds=1))
check("split_window: a one-second window cannot split further",
      cm.split_window(S, S, 5000), [(S, S)])

# plan_windows drives split_window with a counter; a window over the cap must
# end up as halves that are each under it, and an under-cap one is untouched.
calls = []


def fake_count(a, b):
    calls.append((a, b))
    return 1500 if (b - a) > datetime.timedelta(hours=2) else 600


planned = cm.plan_windows(S, E, fake_count)
check("plan_windows: a 1,500-result window becomes two under-cap windows",
      [(a, b) for a, b, _ in planned], halves)
check("plan_windows: each planned window carries its own count",
      [n for _, _, n in planned], [600, 600])

days = cm.day_windows(datetime.date(2026, 10, 27), datetime.date(2026, 10, 27))
check("day_windows: one day is eight windows of 3 hours", len(days), 8)
check("day_windows: first window starts at midnight UTC", days[0][0], S)
check("day_windows: last window ends one second before the next day",
      days[-1][1], dt("2026-10-27T23:59:59Z"))

# --- runner labels ---------------------------------------------------------

check("is_hosted: ubuntu-latest is GitHub-hosted", cm.is_hosted(["ubuntu-latest"]), True)
check("is_hosted: ubuntu-24.04 is GitHub-hosted", cm.is_hosted(["ubuntu-24.04"]), True)
check("is_hosted: the k3s ARC set is not", cm.is_hosted(["gardenio-k3s"]), False)
check("is_hosted: the light ARC set is not", cm.is_hosted(["gardenio-k3s-light"]), False)

mixed = cm.summarize_jobs(
    {
        1: [
            job("theme", "2026-10-27T01:00:00Z", "2026-10-27T01:08:00Z",
                labels=["ubuntu-latest"]),
            job("gate", "2026-10-27T01:00:00Z", "2026-10-27T01:02:00Z",
                labels=["gardenio-k3s-light"]),
        ]
    }
)
check("summarize_jobs: GitHub-hosted minutes are kept apart", mixed["hosted"], 8.0)
check("summarize_jobs: self-hosted minutes exclude the hosted job", mixed["runner"], 2.0)
check("summarize_jobs: the hosted job carries its label",
      [(j["job"], j["label"], j["hosted"]) for j in mixed["jobs"]],
      [("theme", "ubuntu-latest", True), ("gate", "gardenio-k3s-light", False)])

# --- positive control ------------------------------------------------------
#
# One day, 2026-10-27. ci/pull_request has three non-skipped runs and one
# skipped; checks/push has two. Two ci runs are sampled:
#   run 11: build 20m + gate 2m + browser skipped          = 22m
#   run 12: build 24m + gate 2m + browser 10m (re-run, so the first attempt's
#           inverted timestamps must not count)            = 36m
#   -> ci/pull_request = (22 + 36) / 2 = 29.0 min/run
# One checks push run is sampled: lint 12m + gitleaks 6m   = 18.0 min/run
# Daily total = 3 x 29.0 + 2 x 18.0 = 87 + 36 = 123.0 exactly.
# A run created on 10-28 and a skipped ci run must not move it.

ci_jobs = {
    11: [
        job("build + test", "2026-10-27T01:00:00Z", "2026-10-27T01:20:00Z"),
        job("gate", "2026-10-27T00:58:00Z", "2026-10-27T01:00:00Z", labels=["gardenio-k3s-light"]),
        job("browser", None, None, "skipped"),
    ],
    12: [
        job("build + test", "2026-10-27T02:00:00Z", "2026-10-27T02:24:00Z"),
        job("gate", "2026-10-27T01:58:00Z", "2026-10-27T02:00:00Z", labels=["gardenio-k3s-light"]),
        job("browser", "2026-10-27T02:40:00Z", "2026-10-27T02:30:00Z", "failure"),
        job("browser", "2026-10-27T03:00:00Z", "2026-10-27T03:10:00Z"),
    ],
}
checks_jobs = {
    21: [
        job("golangci-lint", "2026-10-27T05:00:00Z", "2026-10-27T05:12:00Z"),
        job("gitleaks", "2026-10-27T05:00:00Z", "2026-10-27T05:06:00Z"),
    ]
}
ci_sum = cm.summarize_jobs(ci_jobs)
checks_sum = cm.summarize_jobs(checks_jobs)
check("positive control: ci/pull_request is 29.0 min/run", ci_sum["runner"], 29.0)
check("positive control: checks/push is 18.0 min/run", checks_sum["runner"], 18.0)
browser = [j for j in ci_sum["jobs"] if j["job"] == "browser"][0]
check("positive control: browser ran on 1 of 2 sampled runs", browser["runs_with_job"], 1)
check("positive control: browser is 5.0 min/run over the sample", browser["min_per_run"], 5.0)

runs = [
    run(11, "ci", "pull_request", "2026-10-27T00:57:00Z", branch="agent/a"),
    run(12, "ci", "pull_request", "2026-10-27T01:57:00Z", branch="agent/a"),
    run(13, "ci", "pull_request", "2026-10-27T23:59:59Z", "failure", branch="agent/b"),
    run(14, "ci", "pull_request", "2026-10-27T10:00:00Z", "skipped", branch="agent/c"),
    run(21, "checks", "push", "2026-10-27T04:59:00Z"),
    run(22, "checks", "push", "2026-10-27T12:00:00Z", "cancelled"),
    run(31, "ci", "pull_request", "2026-10-28T00:00:00Z", branch="agent/d"),
]
per_run = {
    ("ci", "pull_request"): {"runner": ci_sum["runner"], "hosted": ci_sum["hosted"]},
    ("checks", "push"): {"runner": checks_sum["runner"], "hosted": checks_sum["hosted"]},
}
daily = cm.estimate_daily(runs, per_run)
d27 = daily["2026-10-27"]
check("positive control: the fixture day totals exactly 123.0 runner-minutes",
      d27["runner_total"], 123.0)
check("positive control: no GitHub-hosted minutes on the fixture day", d27["hosted_total"], 0.0)
check("positive control: the skipped ci run is not multiplied",
      d27["rows"][("ci", "pull_request")]["runs"], 3)
check("positive control: a run at 00:00:00 on 10-28 belongs to 10-28",
      daily["2026-10-28"]["rows"][("ci", "pull_request")]["runs"], 1)

# A GitHub-hosted workflow on the same day lands in hosted_total, not runner_total.
daily_h = cm.estimate_daily(
    runs + [run(41, "keycloak-theme", "pull_request", "2026-10-27T09:00:00Z")],
    {**per_run, ("keycloak-theme", "pull_request"): {"runner": 0.0, "hosted": 8.0}},
)
check("estimate_daily: GitHub-hosted minutes do not inflate runner_total",
      daily_h["2026-10-27"]["runner_total"], 123.0)
check("estimate_daily: GitHub-hosted minutes are totalled apart",
      daily_h["2026-10-27"]["hosted_total"], 8.0)

counts = cm.runs_per_day(runs)
check("runs_per_day: conclusions are counted per workflow and event",
      counts["2026-10-27"][("ci", "pull_request")],
      {"success": 2, "failure": 1, "skipped": 1})

prs = cm.pr_stats(runs)
check("pr_stats: distinct PR branches on 10-27 (skipped runs still name a branch)",
      prs["2026-10-27"]["branches"], 3)
check("pr_stats: full ci runs on 10-27 exclude the skipped one",
      prs["2026-10-27"]["ci_runs"], 3)

print("\n%d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
