#!/usr/bin/env python3
"""uptime_probe.py -- availability probes for /lands/, /garden and the Commons API.

Lands Track B §5.8 (gd-rqtbyqCMZJ). Runs every minute from a Lambda outside
the cluster (module gardenio/terraform/modules/edge-probes), so it keeps
working, and keeps the status page current, while gardenio is down.

Each run probes every target in targets.json once, then writes:

  state.json   private: cumulative counters, the failure streak and hourly
               buckets for 90 days (the only state the prober keeps)
  status.json  public: the status page data
  index.html   public: the status page, server-rendered, no script
  metrics      public: Prometheus text for the in-cluster vmagent
               (vmagent.uptimeProbeScrape), the SLIs the Lands SLO rules read

It reads one more key, notice.json, private and written only by the operator
(gardenio/docs/lands/status-page-runbook.md): an incident or maintenance notice
shown on the page above the checks. A notice that does not validate is left
off the page, and status.json says why (notice_error).

and, in Lambda mode, CloudWatch GardenioUptime/{Success,LatencyMs} per target
plus GardenioUptime/Heartbeat, which the module's down and dead-man alarms
read.

Stdlib only (boto3 is imported lazily, and only in the Lambda handler). The
probes send no credential, and nothing written holds one: a detail is a status
code, an error class or a redirect target reduced to scheme+host+path.
"""

import argparse
import calendar
import html
import http.client
import json
import os
import socket
import ssl
import sys
import time
import urllib.parse

USER_AGENT = "uptime-probe/1 (+gardenio)"
HTTP_TIMEOUT = 10          # whole request, seconds
HISTORY_HOURS = 90 * 24    # hourly buckets kept in state.json
DOWN_STREAK = 3            # consecutive failures before a target reads "down"
WINDOWS = (("24h", 24), ("7d", 7 * 24), ("30d", 30 * 24), ("90d", 90 * 24))
UP, DEGRADED, DOWN, UNKNOWN = "up", "degraded", "down", "unknown"
_RANK = {UP: 0, UNKNOWN: 1, DEGRADED: 2, DOWN: 3}
NOTICE_KEY = "notice.json"
NOTICE_STATES = {"incident": ("investigating", "identified", "monitoring", "resolved"),
                 "maintenance": ("scheduled", "in_progress", "completed")}
NOTICE_DONE = ("resolved", "completed")
NOTICE_KEEP_HOURS = 24     # a resolved notice leaves the page this long after its last update
NOTICE_MAX_UPDATES = 20


# --------------------------------------------------------------------------
# targets

def load_targets(path):
    with open(path, encoding="utf-8") as fh:
        targets = json.load(fh)["targets"]
    seen = set()
    for t in targets:
        name = t.get("name", "")
        if not name or not name.replace("_", "").isalnum() or name in seen:
            raise ValueError(f"target name {name!r}: must be unique and [A-Za-z0-9_]")
        if not t.get("url", "").startswith("https://") or not t.get("expect_status"):
            raise ValueError(f"target {name}: needs an https url and expect_status")
        seen.add(name)
    if not targets:
        raise ValueError("no targets")
    return targets


def redact(location):
    """scheme://host/path only: query strings carry state and redirect targets."""
    try:
        p = urllib.parse.urlsplit(location or "")
    except ValueError:
        return "<unparseable>"
    return urllib.parse.urlunsplit((p.scheme, p.netloc, p.path, "", ""))


def http_get(url, headers=None, timeout=HTTP_TIMEOUT):
    """One GET, no cookies, no auth, no redirects followed.

    Returns (status, headers lowercased, body up to 64 KiB)."""
    p = urllib.parse.urlsplit(url)
    path = p.path or "/"
    if p.query:
        path += "?" + p.query
    hdrs = {"User-Agent": USER_AGENT, "Accept": "text/html,*/*", "Connection": "close"}
    hdrs.update(headers or {})
    conn = http.client.HTTPSConnection(p.hostname, p.port or 443, timeout=timeout,
                                       context=ssl.create_default_context())
    try:
        conn.request("GET", path, headers=hdrs)
        resp = conn.getresponse()
        body = resp.read(65536).decode("utf-8", "replace")
        return resp.status, {k.lower(): v for k, v in resp.getheaders()}, body
    finally:
        conn.close()


def probe(target, get=http_get, clock=time.monotonic):
    """Probe one target. Returns {name, ok, status, latency_ms, detail}."""
    start = clock()
    try:
        status, headers, body = get(target["url"], target.get("headers"))
    except (OSError, ssl.SSLError, http.client.HTTPException, socket.timeout) as exc:
        latency = round((clock() - start) * 1000)
        return {"name": target["name"], "ok": False, "status": 0,
                "latency_ms": latency, "detail": type(exc).__name__}
    latency = round((clock() - start) * 1000)
    ok, detail = True, str(status)
    if status not in target["expect_status"]:
        ok, detail = False, f"{status}, expected {'/'.join(map(str, target['expect_status']))}"
    elif target.get("expect_body") and target["expect_body"] not in body:
        ok, detail = False, f"{status} without the expected page"
    elif target.get("expect_location_prefix"):
        loc = headers.get("location", "")
        if not loc.startswith(target["expect_location_prefix"]):
            ok, detail = False, f"{status} -> {redact(loc)}"
        else:
            detail = f"{status} -> {redact(loc)}"
    return {"name": target["name"], "ok": ok, "status": status,
            "latency_ms": latency, "detail": detail}


# --------------------------------------------------------------------------
# state

def update_state(state, results, now):
    """Fold one run's results into state (mutated and returned)."""
    state = state or {}
    state.setdefault("version", 1)
    state.setdefault("first_run", int(now))
    targets = state.setdefault("targets", {})
    hour = int(now) // 3600 * 3600
    for r in results:
        t = targets.setdefault(r["name"], {"total": 0, "failures": 0, "streak": 0, "hours": []})
        t["total"] += 1
        t["failures"] += 0 if r["ok"] else 1
        t["streak"] = 0 if r["ok"] else t["streak"] + 1
        t["last"] = {"at": int(now), "ok": r["ok"], "status": r["status"],
                     "latency_ms": r["latency_ms"], "detail": r["detail"]}
        hours = t["hours"]
        if hours and hours[-1][0] == hour:
            hours[-1][1] += 1
            hours[-1][2] += 0 if r["ok"] else 1
        else:
            hours.append([hour, 1, 0 if r["ok"] else 1])
        cutoff = hour - (HISTORY_HOURS - 1) * 3600
        t["hours"] = [h for h in hours if h[0] >= cutoff]
    state["last_run"] = int(now)
    return state


def target_state(t):
    if not t or "last" not in t:
        return UNKNOWN
    if t["streak"] >= DOWN_STREAK:
        return DOWN
    if t["streak"] > 0:
        return DEGRADED
    return UP


def availability(hours, now, window_hours):
    since = int(now) // 3600 * 3600 - (window_hours - 1) * 3600
    total = sum(h[1] for h in hours if h[0] >= since)
    fails = sum(h[2] for h in hours if h[0] >= since)
    if total == 0:
        return None
    return round(100.0 * (total - fails) / total, 3)


def daily(hours, now, days=90):
    """Per-UTC-day {date, total, failures} for the last `days` days, oldest first."""
    today = int(now) // 86400 * 86400
    out = []
    for i in range(days - 1, -1, -1):
        start = today - i * 86400
        hs = [h for h in hours if start <= h[0] < start + 86400]
        out.append({"date": time.strftime("%Y-%m-%d", time.gmtime(start)),
                    "total": sum(h[1] for h in hs), "failures": sum(h[2] for h in hs)})
    return out


def parse_window(s):
    """'2026-10-23T02:00Z/2026-10-23T04:00Z' -> (start, end), whole UTC hours only."""
    start, sep, end = (s or "").partition("/")
    if not sep:
        raise ValueError(f"window {s!r}: want START/END")
    start, end = parse_utc(start), parse_utc(end)
    if start % 3600 or end % 3600 or end <= start:
        raise ValueError(f"window {s!r}: whole hours, end after start (state.json keeps hourly buckets)")
    return start, end


def month_report(state, month, exclude=()):
    """Availability of each target over one calendar month (UTC, 'YYYY-MM'), from
    state.json's hourly buckets: the month's figure in the incident process, and
    the one a published SLA would use (gardenio/docs/lands/sla.md). One probe a
    minute, so failures are about the failing minutes. `exclude` is (start, end)
    windows of announced maintenance; their hours count neither way, and neither
    does an hour with no probe (the prober not running), which the report counts."""
    start = calendar.timegm(time.strptime(month, "%Y-%m"))
    y, m = int(month[:4]), int(month[5:7])
    end = calendar.timegm((y + m // 12, m % 12 + 1, 1, 0, 0, 0))

    def excluded(hour):
        return any(a <= hour < b for a, b in exclude)
    n_excluded = sum(1 for h in range(start, end, 3600) if excluded(h))
    rows = {}
    for name, t in sorted(state.get("targets", {}).items()):
        hs = [h for h in t.get("hours", []) if start <= h[0] < end and not excluded(h[0])]
        total, fails = sum(h[1] for h in hs), sum(h[2] for h in hs)
        rows[name] = {"probes": total, "failures": fails,
                      "hours_without_probes": (end - start) // 3600 - n_excluded - len(hs),
                      "availability": None if total == 0 else round(100.0 * (total - fails) / total, 3)}
    known = [r["availability"] for r in rows.values() if r["availability"] is not None]
    out = {"month": month, "service_availability": min(known) if known else None,
           "excluded_hours": n_excluded, "targets": rows}
    if state.get("last_run", 0) - HISTORY_HOURS * 3600 > start:
        out["warning"] = "part of this month is older than state.json's 90-day history"
    return out


# --------------------------------------------------------------------------
# the operator's notice

def parse_utc(s):
    """'2026-10-22T14:05Z' (seconds optional) -> unix seconds."""
    for fmt in ("%Y-%m-%dT%H:%MZ", "%Y-%m-%dT%H:%M:%SZ"):
        try:
            return calendar.timegm(time.strptime(s, fmt))
        except (TypeError, ValueError):
            pass
    raise ValueError(f"time {s!r}: want UTC like 2026-10-22T14:05Z")


def _text(v, field, limit):
    if not isinstance(v, str) or not v.strip():
        raise ValueError(f"{field}: must be non-empty text")
    if len(v) > limit:
        raise ValueError(f"{field}: longer than {limit} characters")
    return v.strip()


def validate_notice(raw):
    """notice.json bytes -> the notice as status.json carries it. Raises ValueError."""
    try:
        n = json.loads(raw)
    except ValueError as e:
        raise ValueError(f"not JSON: {e}") from None
    if not isinstance(n, dict):
        raise ValueError("must be a JSON object")
    kind = n.get("kind")
    if kind not in NOTICE_STATES:
        raise ValueError(f"kind {kind!r}: one of {', '.join(NOTICE_STATES)}")
    if n.get("state") not in NOTICE_STATES[kind]:
        raise ValueError(f"state {n.get('state')!r}: for {kind}, one of {', '.join(NOTICE_STATES[kind])}")
    updates = n.get("updates")
    if not isinstance(updates, list) or not updates:
        raise ValueError("updates: a non-empty list of {at, text}")
    if len(updates) > NOTICE_MAX_UPDATES:
        raise ValueError(f"updates: at most {NOTICE_MAX_UPDATES}")
    out = {"kind": kind, "state": n["state"], "title": _text(n.get("title"), "title", 120), "updates": []}
    for i, u in enumerate(updates):
        if not isinstance(u, dict):
            raise ValueError(f"updates[{i}]: must be an object")
        out["updates"].append({"at": parse_utc(u.get("at")), "text": _text(u.get("text"), f"updates[{i}].text", 1000)})
    out["updates"].sort(key=lambda u: u["at"], reverse=True)
    if kind == "maintenance":
        w = n.get("window")
        if not isinstance(w, dict):
            raise ValueError("window: maintenance needs {start, end}")
        out["window"] = {"start": parse_utc(w.get("start")), "end": parse_utc(w.get("end"))}
        if out["window"]["end"] <= out["window"]["start"]:
            raise ValueError("window: end must be after start")
    return out


def current_notice(raw, now):
    """(notice or None, error or None). A finished notice drops off after NOTICE_KEEP_HOURS."""
    if raw is None:
        return None, None
    try:
        n = validate_notice(raw)
    except ValueError as e:
        return None, str(e)
    if n["state"] in NOTICE_DONE and now - n["updates"][0]["at"] > NOTICE_KEEP_HOURS * 3600:
        return None, None
    return n, None


# --------------------------------------------------------------------------
# outputs

def render_status(state, targets, now, notice=None, notice_error=None):
    rows = []
    for tgt in targets:
        t = state["targets"].get(tgt["name"], {})
        hours = t.get("hours", [])
        rows.append({
            "name": tgt["name"],
            "title": tgt.get("title", tgt["name"]),
            "state": target_state(t),
            "last": t.get("last"),
            "availability": {w: availability(hours, now, h) for w, h in WINDOWS},
            "days": daily(hours, now),
        })
    overall = max((r["state"] for r in rows), key=_RANK.get)
    status = {"generated_at": int(now), "overall": overall, "targets": rows, "notice": notice}
    if notice_error:
        status["notice_error"] = notice_error
    return status


_WORDS = {UP: "Operational", DEGRADED: "Degraded", DOWN: "Down", UNKNOWN: "No data yet"}
_OVERALL = {UP: "Everything is up", DEGRADED: "Some checks are failing",
            DOWN: "Something is down", UNKNOWN: "Waiting for the first checks"}


_NOTICE_WORDS = {"investigating": "Investigating", "identified": "Identified", "monitoring": "Monitoring",
                 "resolved": "Resolved", "scheduled": "Scheduled maintenance",
                 "in_progress": "Maintenance in progress", "completed": "Maintenance completed"}


def _utc(t):
    return time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime(t))


def render_notice(n):
    if not n:
        return ""
    window = ""
    if "window" in n:
        window = f'<p class="when">{_utc(n["window"]["start"])} to {_utc(n["window"]["end"])}</p>'
    updates = "".join(f'<li><time>{_utc(u["at"])}</time> {html.escape(u["text"])}</li>' for u in n["updates"])
    return (f'<section class="notice {n["kind"]} {n["state"]}"><h2>{_NOTICE_WORDS[n["state"]]}: '
            f'{html.escape(n["title"])}</h2>{window}<ul>{updates}</ul></section>')


def _day_class(d):
    if d["total"] == 0:
        return "none"
    ratio = d["failures"] / d["total"]
    return "ok" if ratio == 0 else ("minor" if ratio < 0.01 else "major")


def render_html(status):
    """The status page: one file, inline CSS, no script, refreshes every minute."""
    ts = _utc(status["generated_at"])
    parts = []
    for r in status["targets"]:
        avail = r["availability"]["30d"]
        avail_txt = "no data" if avail is None else f"{avail:.2f}% over 30 days"
        last = r["last"] or {}
        bars = "".join(
            f'<i class="{_day_class(d)}" title="{d["date"]}: '
            f'{d["total"] - d["failures"]}/{d["total"]} checks passed"></i>'
            for d in r["days"])
        parts.append(
            f'<section class="t {r["state"]}"><h2>{html.escape(r["title"])}'
            f'<span>{_WORDS[r["state"]]}</span></h2>'
            f'<div class="bars" aria-label="last 90 days">{bars}</div>'
            f'<p>{html.escape(avail_txt)}'
            + (f' &middot; last check {html.escape(str(last.get("detail", "")))}, '
               f'{last.get("latency_ms", 0)} ms' if last else "")
            + "</p></section>")
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="60">
<title>Gardenio status</title>
<style>
:root{{--bg:#f7f5ee;--fg:#1f2a1f;--mute:#5b665b;--card:#fff;--line:#dcd8cb;--ok:#3f8f4f;--minor:#d6a231;--major:#c2412d;--none:#d9d6cc}}
@media (prefers-color-scheme:dark){{:root{{--bg:#151a15;--fg:#e8ece4;--mute:#a3ad9f;--card:#1e251e;--line:#334033;--none:#3a443a}}}}
body{{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}}
main{{max-width:720px;margin:0 auto;padding:24px 16px}}
h1{{font-size:1.4rem;margin:0 0 4px}} .sub{{color:var(--mute);margin:0 0 20px}}
.banner{{padding:14px 16px;border-radius:10px;color:#fff;font-weight:600;margin-bottom:20px}}
.banner.up{{background:var(--ok)}} .banner.degraded{{background:var(--minor)}} .banner.down{{background:var(--major)}} .banner.unknown{{background:var(--mute)}}
.t{{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:12px}}
.t h2{{font-size:1.05rem;margin:0 0 10px;display:flex;justify-content:space-between;gap:8px}}
.t h2 span{{font-weight:500;font-size:.95rem}} .t.up h2 span{{color:var(--ok)}} .t.degraded h2 span{{color:var(--minor)}} .t.down h2 span{{color:var(--major)}}
.bars{{display:flex;gap:2px;height:28px}} .bars i{{flex:1;border-radius:2px;background:var(--none)}}
.bars i.ok{{background:var(--ok)}} .bars i.minor{{background:var(--minor)}} .bars i.major{{background:var(--major)}}
.t p{{margin:8px 0 0;color:var(--mute);font-size:.9rem}}
.notice{{background:var(--card);border:1px solid var(--line);border-left:6px solid var(--major);border-radius:10px;padding:14px 16px;margin-bottom:20px}}
.notice.monitoring,.notice.maintenance{{border-left-color:var(--minor)}} .notice.resolved,.notice.completed{{border-left-color:var(--ok)}}
.notice h2{{font-size:1.05rem;margin:0 0 8px}} .notice ul{{margin:0;padding:0;list-style:none}}
.notice li{{margin:6px 0 0}} .notice time,.notice .when{{color:var(--mute);font-size:.9rem;margin:0 0 4px}} .notice time{{display:block}}
footer{{color:var(--mute);font-size:.85rem;margin-top:20px}}
</style></head>
<body><main>
<h1>Gardenio status</h1>
<p class="sub">Checked every minute from outside the cluster. Updated {ts}.</p>
<div class="banner {status["overall"]}">{_OVERALL[status["overall"]]}</div>
{render_notice(status.get("notice"))}
{"".join(parts)}
<footer>Each bar is one UTC day. This page is served from outside gardenio, so it stays up when gardenio is down. Data: <a href="status.json">status.json</a>.</footer>
</main></body></html>
"""


def render_metrics(state, targets, now):
    """Prometheus text. Counters are cumulative since the first run (state.json),
    so the in-cluster SLO rules can take increase() over any window."""
    lines = [
        "# HELP gardenio_uptime_probe_total Availability probes run against the target, since the prober's first run.",
        "# TYPE gardenio_uptime_probe_total counter",
    ]
    names = [t["name"] for t in targets]
    tstate = state["targets"]
    for n in names:
        lines.append(f'gardenio_uptime_probe_total{{target="{n}"}} {tstate.get(n, {}).get("total", 0)}')
    lines += ["# HELP gardenio_uptime_probe_failures_total Availability probes that failed.",
              "# TYPE gardenio_uptime_probe_failures_total counter"]
    for n in names:
        lines.append(f'gardenio_uptime_probe_failures_total{{target="{n}"}} {tstate.get(n, {}).get("failures", 0)}')
    lines += ["# HELP gardenio_uptime_probe_success Whether the target's last probe passed (1) or failed (0).",
              "# TYPE gardenio_uptime_probe_success gauge"]
    for n in names:
        last = tstate.get(n, {}).get("last")
        if last:
            lines.append(f'gardenio_uptime_probe_success{{target="{n}"}} {1 if last["ok"] else 0}')
    lines += ["# HELP gardenio_uptime_probe_duration_seconds How long the target's last probe took.",
              "# TYPE gardenio_uptime_probe_duration_seconds gauge"]
    for n in names:
        last = tstate.get(n, {}).get("last")
        if last:
            lines.append(f'gardenio_uptime_probe_duration_seconds{{target="{n}"}} {last["latency_ms"] / 1000:.3f}')
    lines += ["# HELP gardenio_uptime_probe_last_run_timestamp_seconds When the prober last ran (unix seconds).",
              "# TYPE gardenio_uptime_probe_last_run_timestamp_seconds gauge",
              f"gardenio_uptime_probe_last_run_timestamp_seconds {int(now)}"]
    return "\n".join(lines) + "\n"


# --------------------------------------------------------------------------
# storage

class LocalStore:
    def __init__(self, root):
        self.root = root
        os.makedirs(root, exist_ok=True)

    def get(self, key):
        try:
            with open(os.path.join(self.root, key), "rb") as fh:
                return fh.read()
        except FileNotFoundError:
            return None

    def put(self, key, body, content_type):
        with open(os.path.join(self.root, key), "wb") as fh:
            fh.write(body)


class S3Store:
    def __init__(self, client, bucket):
        self.client, self.bucket = client, bucket

    def get(self, key):
        try:
            return self.client.get_object(Bucket=self.bucket, Key=key)["Body"].read()
        except self.client.exceptions.NoSuchKey:
            return None

    def put(self, key, body, content_type):
        self.client.put_object(Bucket=self.bucket, Key=key, Body=body, ContentType=content_type,
                               CacheControl="no-cache, max-age=30")


STATE_KEY = "state.json"
PUBLIC = {"status.json": "application/json", "index.html": "text/html; charset=utf-8",
          "metrics": "text/plain; version=0.0.4; charset=utf-8"}


def run(targets, store, now=None, get=http_get):
    """One prober run: probe, fold into state, write every output. Returns status."""
    results = [probe(t, get=get) for t in targets]
    now = time.time() if now is None else now
    raw = store.get(STATE_KEY)
    state = update_state(json.loads(raw) if raw else None, results, now)
    notice, notice_error = current_notice(store.get(NOTICE_KEY), now)
    if notice_error:
        print(f"notice.json left off the page: {notice_error}", file=sys.stderr)
    status = render_status(state, targets, now, notice, notice_error)
    store.put(STATE_KEY, json.dumps(state, separators=(",", ":")).encode(), "application/json")
    store.put("status.json", json.dumps(status, separators=(",", ":")).encode(), PUBLIC["status.json"])
    store.put("index.html", render_html(status).encode(), PUBLIC["index.html"])
    store.put("metrics", render_metrics(state, targets, now).encode(), PUBLIC["metrics"])
    return status, results


def publish_metrics(cloudwatch, results):
    data = [{"MetricName": "Heartbeat", "Value": 1, "Unit": "Count"}]
    for r in results:
        dims = [{"Name": "Target", "Value": r["name"]}]
        data.append({"MetricName": "Success", "Dimensions": dims, "Value": 1 if r["ok"] else 0, "Unit": "Count"})
        data.append({"MetricName": "LatencyMs", "Dimensions": dims, "Value": r["latency_ms"], "Unit": "Milliseconds"})
    cloudwatch.put_metric_data(Namespace="GardenioUptime", MetricData=data)


def handler(event, context):
    """AWS Lambda entry point. Env: UPTIME_PROBE_BUCKET, optional UPTIME_PROBE_TARGETS."""
    import boto3  # lazy: stdlib-only everywhere else
    here = os.path.dirname(os.path.abspath(__file__))
    targets = load_targets(os.environ.get("UPTIME_PROBE_TARGETS", os.path.join(here, "targets.json")))
    status, results = run(targets, S3Store(boto3.client("s3"), os.environ["UPTIME_PROBE_BUCKET"]))
    publish_metrics(boto3.client("cloudwatch"), results)
    return {"overall": status["overall"], "results": results}


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--targets", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "targets.json"))
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--out-dir",
                      help="one run: directory for state.json and the public files (kept between runs)")
    mode.add_argument("--check-notice", metavar="NOTICE_JSON",
                      help="validate an operator notice before uploading it; print it as the page will carry it")
    mode.add_argument("--sla-month", metavar="YYYY-MM",
                      help="print each target's availability for one UTC calendar month (needs --state)")
    ap.add_argument("--state", metavar="STATE_JSON", help="with --sla-month: a copy of the bucket's state.json")
    ap.add_argument("--exclude", metavar="START/END", action="append", default=[],
                    help="with --sla-month: an announced maintenance window, whole UTC hours, "
                         "e.g. 2026-10-23T02:00Z/2026-10-23T04:00Z (repeatable)")
    args = ap.parse_args(argv)
    if args.check_notice:
        with open(args.check_notice, "rb") as fh:
            raw = fh.read()
        try:
            notice = validate_notice(raw)
        except ValueError as e:
            print(f"notice is not valid: {e}", file=sys.stderr)
            return 1
        print(json.dumps(notice, indent=2))
        return 0
    if args.sla_month:
        if not args.state:
            ap.error("--sla-month needs --state")
        try:
            exclude = [parse_window(w) for w in args.exclude]
        except ValueError as e:
            ap.error(str(e))
        with open(args.state, encoding="utf-8") as fh:
            print(json.dumps(month_report(json.load(fh), args.sla_month, exclude), indent=2))
        return 0
    status, results = run(load_targets(args.targets), LocalStore(args.out_dir))
    print(json.dumps(results, indent=2))
    return 0 if all(r["ok"] for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
