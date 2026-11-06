#!/usr/bin/env python3
"""Per-harness split for gardenio/helm/gardenio/dashboards/agent-latency-distribution.json (gd-0D8aGbRwYp).
Usage: python3 harness_split.py <path-to-dashboard.json>   (rewrites in place, preserving key order)"""
import json, sys
p = sys.argv[1]
d = json.load(open(p))

d["templating"]["list"].append({
    "current": {"selected": True, "text": "All", "value": "$__all"},
    "datasource": {"type": "prometheus", "uid": "${DS_PROMETHEUS}"},
    "definition": "label_values(gardenio_tool_latency_seconds_bucket, harness)",
    "hide": 0, "includeAll": True, "multi": True, "name": "harness", "allValue": ".*",
    "query": "label_values(gardenio_tool_latency_seconds_bucket, harness)",
    "refresh": 2, "sort": 1, "type": "query"})

def sub(e, pairs):
    for a, b in pairs:
        assert a in e, (a, e)
        e = e.replace(a, b)
    return e

def walk(ps):
    for pn in ps:
        for t in pn.get("targets", []):
            e = t.get("expr", "")
            if "gardenio_tool_latency_seconds_bucket" in e:
                t["expr"] = sub(e, [("sum by (le, tool)", "sum by (le, tool, harness)"),
                                    ('{project=~"$project",tool=~"$tool"}', '{project=~"$project",tool=~"$tool",harness=~"$harness"}')])
                t["legendFormat"] = "{{tool}} · {{harness}}"
            elif "gardenio_bash_cmd_latency_seconds_bucket" in e:
                t["expr"] = sub(e, [("sum by (le, cmd_class)", "sum by (le, cmd_class, harness)"),
                                    ('{project=~"$project",cmd_class=~"$cmd_class"}', '{project=~"$project",cmd_class=~"$cmd_class",harness=~"$harness"}')])
                t["legendFormat"] = "{{cmd_class}} · {{harness}}"
            elif "gardenio_tool_harness_overhead_seconds_bucket" in e:
                t["expr"] = sub(e, [("sum by (le, phase)", "sum by (le, phase, harness)"),
                                    ("_bucket[", '_bucket{harness=~"$harness"}[')])
                t["legendFormat"] = t["legendFormat"].replace("{{phase}}", "{{phase}} · {{harness}}")
        walk(pn.get("panels", []))
walk(d["panels"])
d["version"] = d.get("version", 1) + 1
json.dump(d, open(p, "w"), separators=(",", ":"), ensure_ascii=False)
