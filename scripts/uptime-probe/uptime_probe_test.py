#!/usr/bin/env python3
"""Tests for uptime_probe.py. No network: the HTTP GET is injected.

Run: python3 scripts/uptime-probe/uptime_probe_test.py
"""

import json
import os
import socket
import sys
import contextlib
import io
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import uptime_probe as up  # noqa: E402

T0 = 1792627200  # 2026-10-22 00:00:00 UTC

LANDS = {"name": "lands", "url": "https://gardenio.clonk.tv/lands/", "expect_status": [200],
         "expect_body": "Gardenio Lands"}
GARDEN = {"name": "garden", "url": "https://gardenio.clonk.tv/garden", "expect_status": [302],
          "expect_location_prefix": "https://gardenio.clonk.tv/oauth2/start"}
COMMONS = {"name": "commons_api", "url": "https://gardenio.clonk.tv/garden/c/unread",
           "headers": {"Sec-Fetch-Mode": "cors"}, "expect_status": [401]}


def fake_get(table):
    """table: url -> (status, headers, body) or an exception instance."""
    seen = []

    def get(url, headers=None):
        seen.append((url, headers))
        r = table[url]
        if isinstance(r, Exception):
            raise r
        return r
    get.seen = seen
    return get


HEALTHY = {
    LANDS["url"]: (200, {}, "<title>Gardenio Lands</title>"),
    GARDEN["url"]: (302, {"location": "https://gardenio.clonk.tv/oauth2/start?rd=https://gardenio.clonk.tv/garden&state=SECRET"}, ""),
    COMMONS["url"]: (401, {}, ""),
}


class ProbeTest(unittest.TestCase):
    def test_healthy_targets_pass(self):
        get = fake_get(HEALTHY)
        for t in (LANDS, GARDEN, COMMONS):
            r = up.probe(t, get=get)
            self.assertTrue(r["ok"], r)

    def test_redirect_detail_drops_query(self):
        r = up.probe(GARDEN, get=fake_get(HEALTHY))
        self.assertEqual(r["detail"], "302 -> https://gardenio.clonk.tv/oauth2/start")
        self.assertNotIn("SECRET", json.dumps(r))

    def test_headers_sent(self):
        get = fake_get(HEALTHY)
        up.probe(COMMONS, get=get)
        self.assertEqual(get.seen[0][1], {"Sec-Fetch-Mode": "cors"})

    def test_failures(self):
        cases = [
            ("wrong status", LANDS, (502, {}, "Bad Gateway"), "502, expected 200"),
            ("missing body marker", LANDS, (200, {}, "<title>nginx</title>"), "200 without the expected page"),
            ("redirect elsewhere", GARDEN, (302, {"location": "https://evil.example/x?y=1"}, ""), "302 -> https://evil.example/x"),
            ("open instead of sign-in", COMMONS, (200, {}, "{}"), "200, expected 401"),
            ("timeout", LANDS, socket.timeout("timed out"), None),
            ("refused", LANDS, ConnectionRefusedError(), "ConnectionRefusedError"),
        ]
        for name, target, resp, detail in cases:
            with self.subTest(name):
                r = up.probe(target, get=fake_get({target["url"]: resp}))
                self.assertFalse(r["ok"])
                if detail:
                    self.assertEqual(r["detail"], detail)

    def test_load_targets_rejects_bad_files(self):
        bad = [
            {"targets": []},
            {"targets": [{"name": "a-b", "url": "https://x/", "expect_status": [200]}]},
            {"targets": [{"name": "a", "url": "http://x/", "expect_status": [200]}]},
            {"targets": [{"name": "a", "url": "https://x/", "expect_status": [200]},
                         {"name": "a", "url": "https://y/", "expect_status": [200]}]},
        ]
        for doc in bad:
            with self.subTest(doc), tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
                json.dump(doc, fh)
            with self.assertRaises(ValueError):
                up.load_targets(fh.name)
            os.unlink(fh.name)

    def test_shipped_targets_load(self):
        targets = up.load_targets(os.path.join(HERE, "targets.json"))
        self.assertEqual([t["name"] for t in targets], ["lands", "garden", "commons_api"])


class StateTest(unittest.TestCase):
    def test_counters_streak_and_states(self):
        s = None
        s = up.update_state(s, [{"name": "lands", "ok": True, "status": 200, "latency_ms": 5, "detail": "200"}], T0)
        self.assertEqual(up.target_state(s["targets"]["lands"]), up.UP)
        for i in range(1, 4):
            s = up.update_state(s, [{"name": "lands", "ok": False, "status": 502, "latency_ms": 5, "detail": "502"}], T0 + 60 * i)
            want = up.DEGRADED if i < up.DOWN_STREAK else up.DOWN
            self.assertEqual(up.target_state(s["targets"]["lands"]), want, i)
        t = s["targets"]["lands"]
        self.assertEqual((t["total"], t["failures"], t["streak"]), (4, 3, 3))
        self.assertEqual(t["hours"], [[T0, 4, 3]])
        s = up.update_state(s, [{"name": "lands", "ok": True, "status": 200, "latency_ms": 5, "detail": "200"}], T0 + 3600)
        self.assertEqual(s["targets"]["lands"]["streak"], 0)
        self.assertEqual(s["targets"]["lands"]["hours"], [[T0, 4, 3], [T0 + 3600, 1, 0]])

    def test_history_is_bounded(self):
        s = None
        for h in range(up.HISTORY_HOURS + 50):
            s = up.update_state(s, [{"name": "lands", "ok": True, "status": 200, "latency_ms": 1, "detail": ""}], T0 + h * 3600)
        self.assertEqual(len(s["targets"]["lands"]["hours"]), up.HISTORY_HOURS)

    def test_availability_windows(self):
        hours = [[T0 - 48 * 3600, 60, 60], [T0, 60, 6]]
        self.assertEqual(up.availability(hours, T0 + 10, 24), 90.0)
        self.assertEqual(up.availability(hours, T0 + 10, 7 * 24), 45.0)
        self.assertIsNone(up.availability([], T0, 24))


class OutputTest(unittest.TestCase):
    def run_once(self, store, table, now):
        return up.run([LANDS, GARDEN, COMMONS], store, now=now, get=fake_get(table))

    def test_run_writes_every_output_and_keeps_state(self):
        with tempfile.TemporaryDirectory() as d:
            store = up.LocalStore(d)
            self.run_once(store, HEALTHY, T0)
            broken = dict(HEALTHY, **{LANDS["url"]: (502, {}, "")})
            status, results = self.run_once(store, broken, T0 + 60)
            self.assertEqual(sorted(os.listdir(d)), ["index.html", "metrics", "state.json", "status.json"])
            self.assertEqual(status["overall"], up.DEGRADED)
            metrics = open(os.path.join(d, "metrics")).read()
            for want in [
                'gardenio_uptime_probe_total{target="lands"} 2',
                'gardenio_uptime_probe_failures_total{target="lands"} 1',
                'gardenio_uptime_probe_failures_total{target="garden"} 0',
                'gardenio_uptime_probe_success{target="lands"} 0',
                'gardenio_uptime_probe_success{target="commons_api"} 1',
                f"gardenio_uptime_probe_last_run_timestamp_seconds {T0 + 60}",
            ]:
                self.assertIn(want, metrics)
            page = open(os.path.join(d, "index.html")).read()
            self.assertIn("Some checks are failing", page)
            self.assertIn("Degraded", page)
            for f in os.listdir(d):
                self.assertNotIn("SECRET", open(os.path.join(d, f)).read(), f)

    def test_status_json_shape(self):
        with tempfile.TemporaryDirectory() as d:
            status, _ = self.run_once(up.LocalStore(d), HEALTHY, T0 + 30)
            self.assertEqual(status["overall"], up.UP)
            row = status["targets"][0]
            self.assertEqual(row["name"], "lands")
            self.assertEqual(len(row["days"]), 90)
            self.assertEqual(row["days"][-1], {"date": "2026-10-22", "total": 1, "failures": 0})
            self.assertEqual(row["availability"]["24h"], 100.0)

    def test_unknown_before_first_check(self):
        state = {"targets": {}}
        status = up.render_status(state, [LANDS], T0)
        self.assertEqual(status["overall"], up.UNKNOWN)
        self.assertIn("Waiting for the first checks", up.render_html(status))
        self.assertNotIn('gardenio_uptime_probe_success{', up.render_metrics(state, [LANDS], T0))

    def test_html_escapes_titles(self):
        state = up.update_state(None, [{"name": "x", "ok": True, "status": 200, "latency_ms": 1, "detail": "200"}], T0)
        page = up.render_html(up.render_status(state, [{"name": "x", "title": "<b>x</b>"}], T0))
        self.assertIn("&lt;b&gt;x&lt;/b&gt;", page)

    def test_cloudwatch_payload(self):
        class CW:
            def put_metric_data(self, **kw):
                self.kw = kw
        cw = CW()
        up.publish_metrics(cw, [{"name": "lands", "ok": False, "status": 0, "latency_ms": 10000, "detail": "timeout"}])
        self.assertEqual(cw.kw["Namespace"], "GardenioUptime")
        names = [(m["MetricName"], m.get("Value")) for m in cw.kw["MetricData"]]
        self.assertEqual(names, [("Heartbeat", 1), ("Success", 0), ("LatencyMs", 10000)])


INCIDENT = {"kind": "incident", "state": "investigating", "title": "Agents are slow to <start>",
            "updates": [{"at": "2026-10-22T00:05Z", "text": "Looking into it."},
                        {"at": "2026-10-22T00:20Z", "text": "Found it & fixing."}]}


class NoticeTest(unittest.TestCase):
    def test_valid_incident_newest_first(self):
        n = up.validate_notice(json.dumps(INCIDENT).encode())
        self.assertEqual([u["at"] for u in n["updates"]], [T0 + 1200, T0 + 300])
        self.assertNotIn("window", n)

    def test_maintenance_needs_window(self):
        m = {"kind": "maintenance", "state": "scheduled", "title": "Cluster upgrade",
             "updates": [{"at": "2026-10-20T09:00Z", "text": "Planned."}]}
        with self.assertRaisesRegex(ValueError, "window"):
            up.validate_notice(json.dumps(m).encode())
        m["window"] = {"start": "2026-10-23T02:00Z", "end": "2026-10-23T03:00:00Z"}
        self.assertEqual(up.validate_notice(json.dumps(m).encode())["window"], {"start": T0 + 26 * 3600, "end": T0 + 27 * 3600})
        m["window"]["end"] = "2026-10-23T01:00Z"
        with self.assertRaisesRegex(ValueError, "after start"):
            up.validate_notice(json.dumps(m).encode())

    def test_rejects(self):
        for bad, want in [
            (b"{", "not JSON"),
            (b"[]", "object"),
            (dict(INCIDENT, kind="outage"), "kind"),
            (dict(INCIDENT, state="scheduled"), "state"),
            (dict(INCIDENT, title=" "), "title"),
            (dict(INCIDENT, title="x" * 121), "title"),
            (dict(INCIDENT, updates=[]), "updates"),
            (dict(INCIDENT, updates=[{"at": "22 Oct 14:05", "text": "x"}]), "time"),
            (dict(INCIDENT, updates=[{"at": "2026-10-22T00:05Z"}]), "text"),
            (dict(INCIDENT, updates=[{"at": "2026-10-22T00:05Z", "text": "x"}] * 21), "at most"),
        ]:
            raw = bad if isinstance(bad, bytes) else json.dumps(bad).encode()
            with self.assertRaisesRegex(ValueError, want, msg=bad):
                up.validate_notice(raw)

    def test_resolved_drops_off_after_a_day(self):
        done = dict(INCIDENT, state="resolved")
        raw = json.dumps(done).encode()
        self.assertIsNotNone(up.current_notice(raw, T0 + 1200 + 23 * 3600)[0])
        self.assertEqual(up.current_notice(raw, T0 + 1200 + 25 * 3600), (None, None))
        # an open incident stays however old it is
        self.assertIsNotNone(up.current_notice(json.dumps(INCIDENT).encode(), T0 + 30 * 86400)[0])

    def test_run_renders_notice_escaped(self):
        with tempfile.TemporaryDirectory() as d:
            store = up.LocalStore(d)
            store.put(up.NOTICE_KEY, json.dumps(INCIDENT).encode(), "application/json")
            status, _ = up.run([LANDS], store, now=T0 + 1500, get=fake_get(HEALTHY))
            self.assertEqual(status["notice"]["title"], "Agents are slow to <start>")
            self.assertNotIn("notice_error", status)
            page = open(os.path.join(d, "index.html")).read()
            self.assertIn("Investigating: Agents are slow to &lt;start&gt;", page)
            self.assertIn("Found it &amp; fixing.", page)
            self.assertLess(page.index("Found it"), page.index("Looking into it"))
            self.assertIn("Everything is up", page)  # the banner stays the probes' verdict

    def test_bad_notice_left_off_and_reported(self):
        with tempfile.TemporaryDirectory() as d:
            store = up.LocalStore(d)
            store.put(up.NOTICE_KEY, b'{"kind": "incident"}', "application/json")
            status, _ = up.run([LANDS], store, now=T0, get=fake_get(HEALTHY))
            self.assertIsNone(status["notice"])
            self.assertIn("state", status["notice_error"])
            self.assertNotIn('class="notice', open(os.path.join(d, "index.html")).read())

    def test_no_notice(self):
        with tempfile.TemporaryDirectory() as d:
            status, _ = up.run([LANDS], up.LocalStore(d), now=T0, get=fake_get(HEALTHY))
            self.assertIsNone(status["notice"])
            self.assertNotIn("notice_error", status)


OCT = 1790812800  # 2026-10-01 00:00:00 UTC
NOV = 1793491200  # 2026-11-01 00:00:00 UTC


class MonthReportTest(unittest.TestCase):
    def test_counts_only_the_month(self):
        state = {"first_run": OCT - 86400, "last_run": NOV + 7200, "targets": {
            "lands": {"hours": [[OCT - 3600, 60, 60], [OCT, 60, 6], [OCT + 3600, 60, 0], [NOV, 60, 60]]},
            "garden": {"hours": [[OCT, 60, 0]]},
            "commons_api": {"hours": []},
        }}
        r = up.month_report(state, "2026-10")
        self.assertEqual(r["targets"]["lands"], {"probes": 120, "failures": 6, "hours_without_probes": 742,
                                                 "availability": 95.0})
        self.assertEqual(r["targets"]["garden"]["availability"], 100.0)
        self.assertIsNone(r["targets"]["commons_api"]["availability"])
        self.assertEqual(r["service_availability"], 95.0)  # the worst target with data
        self.assertEqual(r["excluded_hours"], 0)
        self.assertNotIn("warning", r)

    def test_maintenance_window_excluded(self):
        state = {"last_run": NOV, "targets": {"lands": {"hours": [[OCT, 60, 60], [OCT + 3600, 60, 0]]}}}
        w = up.parse_window("2026-10-01T00:00Z/2026-10-01T01:00Z")
        r = up.month_report(state, "2026-10", [w])
        self.assertEqual(r["targets"]["lands"], {"probes": 60, "failures": 0, "hours_without_probes": 743 - 1,
                                                 "availability": 100.0})
        self.assertEqual(r["excluded_hours"], 1)
        for bad in ("2026-10-01T00:00Z", "2026-10-01T00:30Z/2026-10-01T01:00Z", "2026-10-01T02:00Z/2026-10-01T01:00Z"):
            with self.assertRaises(ValueError, msg=bad):
                up.parse_window(bad)

    def test_december_and_old_history(self):
        state = {"last_run": NOV + 200 * 86400, "targets": {"lands": {"hours": []}}}
        r = up.month_report(state, "2026-12")
        self.assertEqual(r["targets"]["lands"]["hours_without_probes"], 31 * 24)
        self.assertIsNone(r["service_availability"])
        self.assertIn("90-day", r["warning"])


class CliTest(unittest.TestCase):
    def test_check_notice(self):
        with tempfile.TemporaryDirectory() as d:
            good, bad = os.path.join(d, "good.json"), os.path.join(d, "bad.json")
            with open(good, "w") as fh:
                json.dump(INCIDENT, fh)
            with open(bad, "w") as fh:
                fh.write("{}")
            with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(up.main(["--check-notice", good]), 0)
                self.assertEqual(up.main(["--check-notice", bad]), 1)
            self.assertIn('"state": "investigating"', out.getvalue())
            self.assertIn("kind", err.getvalue())

    def test_sla_month_needs_state(self):
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            up.main(["--sla-month", "2026-10"])


if __name__ == "__main__":
    unittest.main()
