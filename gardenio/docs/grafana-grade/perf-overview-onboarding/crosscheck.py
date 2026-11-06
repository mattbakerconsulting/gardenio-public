#!/usr/bin/env python3
"""T3.2 cross-check: replay every perf-overview panel target against VictoriaMetrics
ground truth and classify each verdict (OK / HAS-DATA / NO-DATA / MISSING-SERIES /
QUERY-ERROR). Template vars are resolved from live VM label values; Grafana macros
($__rate_interval) are substituted with a concrete window.

This mirrors the GRADE formula step-3 accuracy cross-check, scoped to perf-overview.
"""
import json, os, re, sys, urllib.parse, urllib.request

VM = os.environ.get("VM_URL", "http://%s:8428" % os.environ.get("GARDENIO_VMSINGLE_SERVICE_HOST", "gardenio-vmsingle"))
MODEL = sys.argv[1] if len(sys.argv) > 1 else "perf-overview.json"
RATE_INTERVAL = "5m"          # concrete substitution for $__rate_interval
RANGE = "30m"                 # query_range window for graph panels
STEP = "30s"

def vm_get(path, params):
    url = VM.rstrip("/") + path + "?" + urllib.parse.urlencode(params)
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.load(r)

def label_values(label, match):
    d = vm_get("/api/v1/label/%s/values" % label, {"match[]": match})
    return d.get("data", []) if d.get("status") == "success" else []

def metric_exists(name):
    d = vm_get("/api/v1/query", {"query": "count(%s)" % name})
    res = d.get("data", {}).get("result", [])
    return d.get("status") == "success" and len(res) > 0

# --- resolve template vars from live VM (regex-anchored, all values OR'd) ---
def resolve_vars():
    ecs = label_values("Dimensions_ClusterName",
                       'amazonaws_com_ECS_ContainerInsights_RunningTaskCount{source="perf"}')
    db  = label_values("Dimensions_DBInstanceIdentifier",
                       'amazonaws_com_AWS_RDS_DBLoad{source="perf"}')
    load = label_values("cluster",
                        'fastapi_sponsor_api_http_requests_total{source="perf"}')
    def rx(vals):
        # Grafana multi-value =~ regex is the OR of the selected values.
        # Escape only true RE2 metacharacters (NOT '-', which Python's re.escape
        # backslashes and RE2 then rejects as an invalid escape -> HTTP 422).
        def esc(v):
            return re.sub(r'([.^$*+?()\[\]{}|\\])', r'\\\1', v)
        return "|".join(esc(v) for v in vals) if vals else ".*"
    return {"ecs_cluster": rx(ecs), "db": rx(db), "load_cluster": rx(load)}, {
        "ecs_cluster": ecs, "db": db, "load_cluster": load}

VARS, RAWVALS = resolve_vars()

def subst(expr):
    e = expr
    for k, v in VARS.items():
        e = e.replace("$" + k, v).replace("${%s}" % k, v)
    e = e.replace("$__rate_interval", RATE_INTERVAL)
    return e

# metric families referenced (for MISSING-SERIES classification)
KNOWN_FUNCS = {"sum","rate","max","min","avg","by","histogram_quantile","clamp_min",
               "vector","topk","up","count","le","or","and","increase","irate","label_values"}
def metric_families(expr):
    # Strip label sets {...} and range selectors [...] first, so label KEYS and
    # VALUES (source, cluster, outcome=CLIENT_ERROR) and macros ($__rate_interval)
    # can't be mistaken for metric names.
    stripped = re.sub(r'\{[^}]*\}', ' ', expr)
    stripped = re.sub(r'\[[^\]]*\]', ' ', stripped)
    names = set()
    for m in re.finditer(r'\b([a-zA-Z_][a-zA-Z0-9_]{2,})\b', stripped):
        n = m.group(1)
        if n in KNOWN_FUNCS: continue
        # metric families are long identifiers with an underscore, or the up job
        if "_" in n or n == "up":
            names.add(n)
    return names

model = json.load(open(MODEL))
rows = []
for p in model["panels"]:
    if p.get("type") in ("row", "text"):
        continue
    is_graph = p.get("type") == "timeseries"
    for tg in p.get("targets", []):
        expr = tg.get("expr")
        if not expr:
            continue
        q = subst(expr)
        verdict, detail, npoints = "OK", "", 0
        try:
            if is_graph:
                import time
                d = vm_get("/api/v1/query_range", {"query": q, "start": "now-%s" % RANGE,
                            "end": "now", "step": STEP})
            else:
                d = vm_get("/api/v1/query", {"query": q})
            if d.get("status") != "success":
                verdict, detail = "QUERY-ERROR", d.get("error", "")[:200]
            else:
                res = d.get("data", {}).get("result", [])
                npoints = len(res)
                if npoints == 0:
                    # distinguish MISSING-SERIES (metric name absent) vs NO-DATA (idle)
                    fams = metric_families(expr)
                    missing = [f for f in fams if not metric_exists(f)]
                    if missing:
                        verdict, detail = "MISSING-SERIES", "absent families: " + ",".join(sorted(missing))
                    else:
                        verdict, detail = "NO-DATA", "all families present but no series in window"
                else:
                    verdict = "HAS-DATA"
                    # sample the first value for the record
                    try:
                        if is_graph:
                            vals = res[0].get("values", [])
                            detail = "series=%d last=%s" % (npoints, vals[-1][1] if vals else "?")
                        else:
                            detail = "series=%d val=%s" % (npoints, res[0]["value"][1])
                    except Exception:
                        detail = "series=%d" % npoints
        except urllib.error.HTTPError as ex:
            body = ex.read().decode("utf-8", "replace")[:200]
            verdict, detail = "QUERY-ERROR", "HTTP %s %s" % (ex.code, body)
        except Exception as ex:
            verdict, detail = "QUERY-ERROR", str(ex)[:200]
        rows.append({
            "panel_id": p["id"], "panel_title": p["title"], "panel_type": p["type"],
            "unit": p.get("fieldConfig", {}).get("defaults", {}).get("unit"),
            "expr": expr, "resolved": q, "verdict": verdict, "series": npoints, "detail": detail,
        })

out = {"vm_url": VM, "resolved_vars": RAWVALS, "rate_interval": RATE_INTERVAL,
       "range": RANGE, "rows": rows}
json.dump(out, open("crosscheck.json", "w"), indent=2)

# summary
from collections import Counter
c = Counter(r["verdict"] for r in rows)
print("VM:", VM)
print("resolved vars:", {k: v for k, v in RAWVALS.items()})
print("total targets:", len(rows))
for k in ("OK","HAS-DATA","NO-DATA","MISSING-SERIES","AGGREGATION-DRIFT","UNIT","STALE","QUERY-ERROR"):
    if c.get(k): print("  %-16s %d" % (k, c[k]))
print()
for r in rows:
    flag = "" if r["verdict"] in ("HAS-DATA","OK") else "  <-- REVIEW"
    print("[%-14s] id=%-2d %-42s %s%s" % (r["verdict"], r["panel_id"], r["panel_title"][:42], r["detail"][:60], flag))
