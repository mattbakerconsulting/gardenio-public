#!/usr/bin/env python3
"""Tests for edge_probe.py: positive and negative controls, no network beyond loopback.

Run: python3 scripts/edge-probe/edge_probe_test.py
"""

import http.server
import json
import os
import re
import socket
import ssl
import sys
import threading
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import edge_probe as ep  # noqa: E402

HOST = "claimlayer.clonk.tv"
REALM = "https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/auth?state=SECRET&nonce=SECRET"


class ClassifierTest(unittest.TestCase):
    def test_table(self):
        cases = [
            ("401", 401, None, ep.PASS),
            ("302 to the realm", 302, REALM, ep.PASS),
            ("303 to the realm", 303, REALM, ep.PASS),
            ("302 to same-host /oauth2/start with rd", 302, "/oauth2/start?rd=https%3A%2F%2Fclaimlayer.clonk.tv%2F", ep.PASS),
            ("302 to absolute same-host /oauth2/start", 302, f"https://{HOST}/oauth2/start?rd=/", ep.PASS),
            ("200", 200, None, ep.OPEN),
            ("204", 204, None, ep.OPEN),
            ("302 to evil", 302, "https://evil.example/", ep.OPEN),
            ("302 to realm lookalike host", 302, "https://auth.clonk.tv.evil.example/auth/realms/gardenio/x", ep.OPEN),
            ("302 userinfo trick", 302, "https://auth.clonk.tv@evil.example/auth/realms/gardenio/x", ep.OPEN),
            ("302 protocol-relative evil", 302, "//evil.example/oauth2/start", ep.OPEN),
            ("302 to other realm", 302, "https://auth.clonk.tv/auth/realms/master/x", ep.OPEN),
            ("302 oauth2/start prefix trick", 302, "/oauth2/startevil", ep.OPEN),
            ("302 other host oauth2/start", 302, "https://evil.example/oauth2/start", ep.OPEN),
            ("302 no location", 302, None, ep.OPEN),
            ("302 backslash trick", 302, "https:\\\\evil.example/oauth2/start", ep.OPEN),
            ("301 even to the realm", 301, REALM, ep.OPEN),
            ("404 is not open but not the wall", 404, None, ep.UNEXPECTED),
            ("500", 500, None, ep.UNEXPECTED),
        ]
        for name, status, location, want in cases:
            with self.subTest(name):
                got, _ = ep.classify(HOST, status, location)
                # 301 to the realm is a redirect "elsewhere" by the rule: OPEN
                self.assertEqual(got, want)

    def test_canary_response_reads_open(self):
        verdict, _ = ep.classify("auth.clonk.tv", 200, None)
        self.assertEqual(verdict, ep.OPEN)

    def test_detail_never_carries_the_query_string(self):
        _, detail = ep.classify(HOST, 302, REALM)
        self.assertNotIn("SECRET", detail)
        _, detail = ep.classify(HOST, 302, "https://evil.example/x?token=SECRET")
        self.assertNotIn("SECRET", detail)


def sg(name, gid, *perms):
    return {"GroupName": name, "GroupId": gid, "IpPermissions": list(perms)}


def world(proto, lo, hi, v6=False):
    perm = {"IpProtocol": proto, "FromPort": lo, "ToPort": hi, "IpRanges": [], "Ipv6Ranges": []}
    if v6:
        perm["Ipv6Ranges"] = [{"CidrIpv6": "::/0"}]
    else:
        perm["IpRanges"] = [{"CidrIp": "0.0.0.0/0"}]
    return perm


def private(proto, lo, hi):
    return {"IpProtocol": proto, "FromPort": lo, "ToPort": hi,
            "IpRanges": [{"CidrIp": "10.0.0.0/16"}], "Ipv6Ranges": []}


class SecurityGroupAuditTest(unittest.TestCase):
    def test_edge_443_only_passes(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", world("tcp", 443, 443)),
                  sg("k3s-nodes", "sg-nodes", private("tcp", 6443, 6443))]
        self.assertEqual(ep.audit_security_groups(groups, False), [])

    def test_edge_443_and_3022_pass_with_public_router(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", world("tcp", 443, 443), world("tcp", 3022, 3022))]
        self.assertEqual(ep.audit_security_groups(groups, True), [])

    def test_edge_80_redirect_listener_passes(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", world("tcp", 80, 80), world("tcp", 443, 443))]
        self.assertEqual(ep.audit_security_groups(groups, False), [])

    def test_world_open_80_on_a_non_edge_sg_fails(self):
        groups = [sg("k3s-nodes", "sg-nodes", world("tcp", 80, 80))]
        self.assertTrue(ep.audit_security_groups(groups, True))

    def test_world_open_6443_on_nodes_fails(self):
        groups = [sg("k3s-nodes", "sg-nodes", world("tcp", 6443, 6443))]
        problems = ep.audit_security_groups(groups, True)
        self.assertEqual(len(problems), 1)
        self.assertIn("6443", problems[0])

    def test_world_open_3022_on_edge_fails_when_router_not_public(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", world("tcp", 443, 443), world("tcp", 3022, 3022))]
        problems = ep.audit_security_groups(groups, False)
        self.assertEqual(len(problems), 1)
        self.assertIn("3022", problems[0])

    def test_world_open_6443_on_the_edge_sg_fails_too(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", world("tcp", 6443, 6443))]
        self.assertTrue(ep.audit_security_groups(groups, True))

    def test_range_covering_6443_fails(self):
        groups = [sg("k3s-nodes", "sg-nodes", world("tcp", 0, 65535))]
        self.assertIn("6443", ep.audit_security_groups(groups, True)[0])

    def test_all_protocols_world_open_fails_even_on_edge(self):
        groups = [sg("k3s-ziti-edge", "sg-edge", {"IpProtocol": "-1", "IpRanges": [{"CidrIp": "0.0.0.0/0"}]})]
        self.assertTrue(ep.audit_security_groups(groups, True))

    def test_ipv6_world_open_is_world_open(self):
        groups = [sg("k3s-nodes", "sg-nodes", world("tcp", 22, 22, v6=True))]
        self.assertTrue(ep.audit_security_groups(groups, True))

    def test_world_open_443_on_a_non_edge_sg_fails(self):
        groups = [sg("k3s-nodes", "sg-nodes", world("tcp", 443, 443))]
        self.assertTrue(ep.audit_security_groups(groups, True))

    def test_private_ranges_are_ignored(self):
        groups = [sg("k3s-nodes", "sg-nodes", private("tcp", 22, 22), private("tcp", 6443, 6443))]
        self.assertEqual(ep.audit_security_groups(groups, True), [])

    def test_fixture_json_roundtrip(self):
        fixture = json.loads(json.dumps({"SecurityGroups": [
            sg("k3s-ziti-edge", "sg-edge", world("tcp", 443, 443))]}))
        self.assertEqual(ep.audit_security_groups(fixture["SecurityGroups"], False), [])


class SweepTest(unittest.TestCase):
    def test_listening_port_is_open_and_closed_port_is_refused(self):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(8)
        open_port = listener.getsockname()[1]
        spare = socket.socket()
        spare.bind(("127.0.0.1", 0))
        closed_port = spare.getsockname()[1]
        spare.close()  # bound then closed: nothing listens here
        try:
            states = ep.sweep_ports("127.0.0.1", [open_port, closed_port], timeout=2)
        finally:
            listener.close()
        self.assertEqual(states[open_port], "open", "control: a port we listen on must read open")
        self.assertEqual(states[closed_port], "refused")

    def test_just_closed_port_is_refused(self):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        listener.close()
        self.assertEqual(ep.sweep_ports("127.0.0.1", [port], timeout=2)[port], "refused")

    @staticmethod
    def ok(port):
        return True, "hooks route only"

    @staticmethod
    def not_ok(port):
        return False, "Host hooks.clonk.tv -> 200"

    def test_edge_443_only_passes(self):
        states = {22: "timeout", 443: "open", 6443: "refused"}
        result, detail = ep.evaluate_sweep({"edge"}, states, False, self.ok)
        self.assertEqual(result, ep.PASS)
        self.assertIn("6443=refused", detail)

    def test_edge_3022_allowed_only_with_public_router(self):
        states = {443: "open", 3022: "open", 6443: "timeout"}
        self.assertEqual(ep.evaluate_sweep({"edge"}, states, True, self.ok)[0], ep.PASS)
        self.assertEqual(ep.evaluate_sweep({"edge"}, states, False, self.ok)[0], ep.FAIL)

    def test_6443_open_fails_everywhere(self):
        for roles in ({"edge"}, {"server"}, {"edge", "server"}):
            with self.subTest(roles=roles):
                result, detail = ep.evaluate_sweep(roles, {6443: "open"}, True, self.ok)
                self.assertEqual(result, ep.FAIL)
                self.assertIn("6443", detail)

    def test_server_80_443_only_if_hooks_route_only(self):
        states = {80: "open", 443: "open"}
        self.assertEqual(ep.evaluate_sweep({"server"}, states, True, self.ok)[0], ep.PASS)
        self.assertEqual(ep.evaluate_sweep({"server"}, states, True, self.not_ok)[0], ep.FAIL)

    def test_server_443_is_not_allowed_on_the_edge_only_role_80(self):
        self.assertEqual(ep.evaluate_sweep({"edge"}, {443: "open", 80: "open"}, True, self.ok)[0], ep.FAIL)

    def test_edge_80_passes_only_when_the_redirect_check_does(self):
        states = {443: "open", 80: "open"}
        good = lambda port: (True, "")
        bad = lambda port: (False, "Host gardenio.clonk.tv: 200 -> None")
        self.assertEqual(ep.evaluate_sweep({"edge"}, states, True, self.ok, edge_http_ok=good)[0], ep.PASS)
        result, detail = ep.evaluate_sweep({"edge"}, states, True, self.ok, edge_http_ok=bad)
        self.assertEqual(result, ep.FAIL)
        self.assertIn("https redirect", detail)

    def test_edge_redirect_ok(self):
        self.assertTrue(ep.edge_redirect_ok("gardenio.clonk.tv", 301, "https://gardenio.clonk.tv/"))
        self.assertTrue(ep.edge_redirect_ok("gardenio.clonk.tv", 308, "https://gardenio.clonk.tv/"))
        for status, loc in ((200, None), (302, "https://gardenio.clonk.tv/"),
                            (301, "http://gardenio.clonk.tv/"), (301, "https://evil.example/"),
                            (301, "https://gardenio.clonk.tv/app"), (301, "https://gardenio.clonk.tv/?x=1"),
                            (301, None)):
            with self.subTest(status=status, loc=loc):
                self.assertFalse(ep.edge_redirect_ok("gardenio.clonk.tv", status, loc))

    def test_edge_http_redirect_names_match_edge_hosts(self):
        """The nginx port-80 redirect block names exactly the hosts the edge serves."""
        root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
        raw = open(os.path.join(root, "gardenio", "helm", "gardenio-ziti", "public-edge.yaml")).read()
        m = re.search(r"listen 8080;\s*\n\s*server_name ([^;]+);", raw)
        self.assertIsNotNone(m, "no port-80 redirect server block in public-edge.yaml")
        exp = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "expectations.json")))
        self.assertEqual(sorted(m.group(1).split()), sorted(exp["edge_hosts"]))

    def test_any_other_open_port_fails(self):
        for port in (22, 2379, 5432, 8443, 10250, 30777):
            with self.subTest(port=port):
                self.assertEqual(ep.evaluate_sweep({"server"}, {port: "open"}, True, self.ok)[0], ep.FAIL)


class HooksProbeTest(unittest.TestCase):
    def test_404_passes_on_both_ports(self):
        self.assertTrue(ep.hooks_probe_ok(80, "hooks.clonk.tv", 404, None))
        self.assertTrue(ep.hooks_probe_ok(443, "hooks.clonk.tv", 404, None))

    def test_port_80_https_entrypoint_redirect_passes(self):
        self.assertTrue(ep.hooks_probe_ok(80, "hooks.clonk.tv", 301, "https://hooks.clonk.tv/"))
        self.assertTrue(ep.hooks_probe_ok(80, "x.clonk.tv", 308, "https://x.clonk.tv/"))

    def test_redirect_elsewhere_or_on_443_fails(self):
        self.assertFalse(ep.hooks_probe_ok(80, "hooks.clonk.tv", 301, "https://evil.example/"))
        self.assertFalse(ep.hooks_probe_ok(80, "hooks.clonk.tv", 302, "https://hooks.clonk.tv/"))
        self.assertFalse(ep.hooks_probe_ok(80, "hooks.clonk.tv", 301, "https://hooks.clonk.tv/app"))
        self.assertFalse(ep.hooks_probe_ok(443, "hooks.clonk.tv", 301, "https://hooks.clonk.tv/"))

    def test_anything_else_fails(self):
        for status in (200, 401, 403, 500):
            self.assertFalse(ep.hooks_probe_ok(80, "hooks.clonk.tv", status, None))
            self.assertFalse(ep.hooks_probe_ok(443, "hooks.clonk.tv", status, None))


class ExitCodeTest(unittest.TestCase):
    @staticmethod
    def report(*results):
        return {"checks": [{"id": "E", "host": "h", "result": r, "detail": ""} for r in results]}

    def test_codes(self):
        self.assertEqual(ep.exit_code(self.report(ep.PASS, ep.PASS)), 0)
        self.assertEqual(ep.exit_code(self.report(ep.PASS, ep.FAIL)), 1)
        self.assertEqual(ep.exit_code(self.report(ep.PASS, ep.BROKEN)), 2)

    def test_broken_wins_over_fail(self):
        self.assertEqual(ep.exit_code(self.report(ep.FAIL, ep.BROKEN)), 2)
        self.assertEqual(ep.exit_code(self.report(ep.BROKEN, ep.FAIL)), 2)


class RunExpectationsTest(unittest.TestCase):
    """Probe.run() must not go green when the expectations would check nothing."""

    CHECKS = ("e1_edge_names", "e2_keycloak_admin", "e3_canary", "e4_unknown_sni",
              "e5_certs", "e6_sweep", "e8_api_token", "e9_scoped_identity")

    def run_probe(self, exp):
        base = {"edge_ip_from": "auth.clonk.tv", "canary_url": "https://auth.clonk.tv/__edge_canary"}
        probe = ep.Probe({**base, **exp}, "github")
        saved = {n: getattr(probe, n) for n in self.CHECKS}
        orig = ep.resolve_a
        ep.resolve_a = lambda host: ["192.0.2.1"]
        for n in self.CHECKS:
            setattr(probe, n, lambda *a, **k: None)
        try:
            return probe.run()
        finally:
            ep.resolve_a = orig
            for n, f in saved.items():
                setattr(probe, n, f)

    def test_empty_edge_hosts_is_broken(self):
        report = self.run_probe({"edge_hosts": []})
        self.assertEqual(ep.exit_code(report), 2)
        self.assertEqual(report["checks"][0]["id"], "E0")

    def test_missing_edge_hosts_is_broken(self):
        self.assertEqual(ep.exit_code(self.run_probe({})), 2)

    def test_exception_host_outside_edge_hosts_is_broken(self):
        report = self.run_probe({"edge_hosts": ["a.example"], "exceptions": {"b.example": {"/": 404}}})
        self.assertEqual(ep.exit_code(report), 2)
        self.assertIn("b.example", [c["host"] for c in report["checks"]])

    def test_declared_hosts_do_not_trip_the_guard(self):
        report = self.run_probe({"edge_hosts": ["a.example"], "exceptions": {"a.example": {"/": 404}}})
        self.assertEqual(report["checks"], [])


class CertTest(unittest.TestCase):
    NOW = ssl.cert_time_to_seconds("Jan  1 00:00:00 2030 GMT")

    @staticmethod
    def cert(org="Let's Encrypt", sans=("auth.clonk.tv",), not_after="Mar  1 00:00:00 2030 GMT"):
        return {"issuer": ((("countryName", "US"),), (("organizationName", org),)),
                "subjectAltName": tuple(("DNS", s) for s in sans), "notAfter": not_after}

    def test_good_cert(self):
        self.assertEqual(ep.evaluate_cert("auth.clonk.tv", self.cert(), self.NOW)[0], ep.PASS)

    def test_wrong_issuer_fails(self):
        self.assertEqual(ep.evaluate_cert("auth.clonk.tv", self.cert(org="Acme CA"), self.NOW)[0], ep.FAIL)

    def test_missing_san_fails(self):
        self.assertEqual(ep.evaluate_cert("auth.clonk.tv", self.cert(sans=("other.clonk.tv",)), self.NOW)[0], ep.FAIL)

    def test_wildcard_san_matches_one_label(self):
        self.assertEqual(ep.evaluate_cert("auth.clonk.tv", self.cert(sans=("*.clonk.tv",)), self.NOW)[0], ep.PASS)
        self.assertEqual(ep.evaluate_cert("a.b.clonk.tv", self.cert(sans=("*.clonk.tv",)), self.NOW)[0], ep.FAIL)

    def test_expiring_cert_fails(self):
        soon = self.cert(not_after="Jan 10 00:00:00 2030 GMT")
        self.assertEqual(ep.evaluate_cert("auth.clonk.tv", soon, self.NOW)[0], ep.FAIL)


class _Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        if self.path == "/canary":
            body = b"edge-canary-open\n"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(302)
            self.send_header("Location", "https://evil.example/?token=SECRET")
            self.send_header("Content-Length", "0")
            self.end_headers()

    def log_message(self, *a):
        pass


class HttpGetTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = http.server.HTTPServer(("127.0.0.1", 0), _Handler)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def test_redirect_is_not_followed(self):
        status, hdrs, _ = ep.http_get("example.test", "/", scheme="http", ip="127.0.0.1", port=self.port)
        self.assertEqual(status, 302)
        self.assertTrue(hdrs["location"].startswith("https://evil.example/"))

    def test_canary_over_the_wire_classifies_open(self):
        """E3's two conditions, end to end against a local server standing in for the edge."""
        status, hdrs, body = ep.http_get("auth.clonk.tv", "/canary", scheme="http", ip="127.0.0.1", port=self.port)
        self.assertEqual((status, body.strip()), (200, ep.CANARY_BODY))
        self.assertEqual(ep.classify("auth.clonk.tv", status, hdrs.get("location"))[0], ep.OPEN)


class NonHttpHostTest(unittest.TestCase):
    """A declared non-HTTP host passes on an SNI refusal; anything served on the EIP still FAILs."""

    EXP = {"edge_hosts": ["a.example"], "non_http_hosts": ["ziti.example"]}

    def run_probe(self, exp, handshake):
        base = {"edge_ip_from": "auth.clonk.tv", "canary_url": "https://auth.clonk.tv/__edge_canary"}
        probe = ep.Probe({**base, **exp}, "github")
        for n in RunExpectationsTest.CHECKS:
            if n != "e1_edge_names":
                setattr(probe, n, lambda *a, **k: None)
        seen = []
        probe.e1_edge_names = lambda hosts: seen.append(list(hosts))
        orig = (ep.resolve_a, ep.tls_handshake)
        ep.resolve_a = lambda host: ["192.0.2.1"]
        ep.tls_handshake = lambda ip, sni, **kw: handshake
        try:
            return probe.run(), seen
        finally:
            ep.resolve_a, ep.tls_handshake = orig

    def test_sni_refusal_is_the_expected_pass(self):
        report, seen = self.run_probe(self.EXP, ("rejected", "SSLError: TLSV1_UNRECOGNIZED_NAME"))
        self.assertEqual(ep.exit_code(report), 0)
        self.assertEqual(report["checks"][0]["host"], "ziti.example")
        self.assertEqual(seen, [["a.example"]])      # not run through the HTTP rule

    def test_unexpected_host_on_the_eip_still_fails(self):
        # positive control: the same declared name served a handshake, so it is no longer inert
        report, _ = self.run_probe(self.EXP, ("accepted", ""))
        self.assertEqual(ep.exit_code(report), 1)

    def test_inconclusive_is_a_fail_not_a_pass(self):
        report, _ = self.run_probe(self.EXP, ("unreachable", "timeout"))
        self.assertEqual(ep.exit_code(report), 1)

    def test_undeclared_host_is_not_exempt(self):
        probe = ep.Probe({}, "github")
        probe.edge_ips = ["192.0.2.1"]
        orig = ep.tls_handshake
        ep.tls_handshake = lambda *a, **k: ("accepted", "")
        try:
            probe.e1_non_http_hosts(["other.example"])
        finally:
            ep.tls_handshake = orig
        self.assertEqual(probe.checks[0]["result"], ep.FAIL)

    def test_discovered_non_http_host_leaves_the_http_rules(self):
        base = {"edge_ip_from": "auth.clonk.tv", "canary_url": "https://auth.clonk.tv/__edge_canary"}
        probe = ep.Probe({**base, **self.EXP}, "lambda", vpc_id="vpc-1", boto=object())
        for n in RunExpectationsTest.CHECKS:
            if n != "e1_edge_names":
                setattr(probe, n, lambda *a, **k: None)
        probe.e7_sg_audit = lambda: None
        probe.lambda_discovery = lambda ip: (["ziti.example", "b.example"], [])
        probe.e1_non_http_hosts = lambda hosts: None
        seen = []
        probe.e1_edge_names = lambda hosts: seen.append(list(hosts))
        orig = ep.resolve_a
        ep.resolve_a = lambda host: ["192.0.2.1"]
        try:
            probe.run()
        finally:
            ep.resolve_a = orig
        self.assertEqual(seen, [["a.example", "b.example"]])

    def test_host_in_both_lists_is_broken(self):
        report, _ = self.run_probe({"edge_hosts": ["a.example"], "non_http_hosts": ["a.example"]},
                                   ("rejected", ""))
        self.assertEqual(ep.exit_code(report), 2)


class ProbeCanaryTest(unittest.TestCase):
    """E3 is BROKEN, not FAIL, when the canary is missing or the classifier would not call it OPEN."""

    def run_e3(self, status, body, location=None):
        orig = ep.http_get
        ep.http_get = lambda host, path, **kw: (status, {"location": location} if location else {}, body)
        try:
            probe = ep.Probe({"canary_url": "https://auth.clonk.tv/__edge_canary"}, "github")
            probe.e3_canary()
        finally:
            ep.http_get = orig
        return probe.checks[0]["result"]

    def test_canary_present_passes(self):
        self.assertEqual(self.run_e3(200, "edge-canary-open\n"), ep.PASS)

    def test_canary_missing_is_broken(self):
        self.assertEqual(self.run_e3(404, "not found"), ep.BROKEN)

    def test_wrong_body_is_broken(self):
        self.assertEqual(self.run_e3(200, "hello"), ep.BROKEN)

    def test_unreachable_is_broken(self):
        orig = ep.http_get

        def boom(*a, **k):
            raise OSError("down")
        ep.http_get = boom
        try:
            probe = ep.Probe({"canary_url": "https://auth.clonk.tv/__edge_canary"}, "github")
            probe.e3_canary()
        finally:
            ep.http_get = orig
        self.assertEqual(probe.checks[0]["result"], ep.BROKEN)


class ProbeKeycloakAdminTest(unittest.TestCase):
    """E2: the master console and API are 404; the gardenio realm console is walled or 404, never OPEN."""

    def run_e2(self, answer):
        orig = ep.http_get
        ep.http_get = lambda host, path, **kw: answer(path)
        try:
            probe = ep.Probe({"canary_url": "https://auth.clonk.tv/__edge_canary"}, "github")
            probe.e2_keycloak_admin()
        finally:
            ep.http_get = orig
        return {c["detail"].split(" ")[1]: c["result"] for c in probe.checks}

    @staticmethod
    def walled(path):
        if path in ep.Probe.E2_WALLED:
            return 302, {"location": "https://auth.clonk.tv/oauth2/start?rd=https://auth.clonk.tv" + path}, ""
        return 404, {}, ""

    def test_walled_console_passes(self):
        results = self.run_e2(self.walled)
        self.assertEqual(set(results.values()), {ep.PASS})
        self.assertIn("/auth/admin/realms/master", results)

    def test_closed_console_passes(self):
        self.assertEqual(set(self.run_e2(lambda path: (404, {}, "")).values()), {ep.PASS})

    def test_open_console_fails(self):
        results = self.run_e2(lambda path: (200, {}, "") if path in ep.Probe.E2_WALLED else (404, {}, ""))
        self.assertEqual(results["/auth/admin/gardenio/console/"], ep.FAIL)
        self.assertEqual(results["/gardenio/become/"], ep.FAIL)

    def test_open_become_page_fails(self):
        # The Become page served with no session is OPEN even when the console is walled.
        results = self.run_e2(lambda path: (200, {}, "") if path.startswith("/gardenio/become/") else self.walled(path))
        self.assertEqual(results["/gardenio/become/?email=edge-probe%40example.com"], ep.FAIL)
        self.assertEqual(results["/auth/admin/gardenio/console/"], ep.PASS)

    def test_walled_master_fails(self):
        # The master console behind the wall is still a FAIL: it must be 404.
        results = self.run_e2(lambda path: self.walled("/auth/admin/gardenio/console/") if "master" in path else self.walled(path))
        self.assertEqual(results["/auth/admin/master/console/"], ep.FAIL)
        self.assertEqual(results["/auth/admin/realms/master"], ep.FAIL)


class FakeClient:
    def __init__(self):
        self.calls = []

    def put_metric_data(self, **kw):
        self.calls.append(("put_metric_data", kw))

    def publish(self, **kw):
        self.calls.append(("publish", kw))


class PublishResultsTest(unittest.TestCase):
    def report(self, *results):
        return {"vantage": "lambda", "skipped": [], "checks": [
            {"id": f"E{i}", "host": "h", "result": r, "detail": "d"} for i, r in enumerate(results)]}

    def run_publish(self, report, topic="arn:aws:sns:us-east-1:1:t"):
        cw, sns = FakeClient(), FakeClient()
        ep.publish_results(report, cw, sns, topic)
        return cw.calls, sns.calls

    def test_green_run_writes_heartbeat_and_no_alert(self):
        cw, sns = self.run_publish(self.report(ep.PASS, ep.PASS))
        data = {m["MetricName"]: m["Value"] for m in cw[0][1]["MetricData"]}
        self.assertEqual(data, {"Heartbeat": 1, "Failures": 0, "Broken": 0})
        self.assertEqual(cw[0][1]["Namespace"], "GardenioEdgeProbe")
        self.assertEqual(sns, [])

    def test_fail_and_broken_are_counted_and_alerted(self):
        cw, sns = self.run_publish(self.report(ep.FAIL, ep.FAIL, ep.BROKEN, ep.PASS))
        data = {m["MetricName"]: m["Value"] for m in cw[0][1]["MetricData"]}
        self.assertEqual(data, {"Heartbeat": 1, "Failures": 2, "Broken": 1})
        self.assertEqual(len(sns), 1)
        msg = sns[0][1]["Message"]
        self.assertIn("2 FAIL, 1 BROKEN", msg)
        self.assertNotIn("PASS E3", msg)

    def test_no_topic_means_no_publish(self):
        _, sns = self.run_publish(self.report(ep.FAIL), topic="")
        self.assertEqual(sns, [])


class ScopedIdentityTest(unittest.TestCase):
    """E9: a project-only identity is admitted by the viewer edge, refused by the mux edge."""

    EXP = {"scoped_identity": {"viewer_host": "viewer.test", "mux_host": "mux.test"}}
    SCOPED, ADMIN = "_oauth2_proxy=scoped-SECRET", "_oauth2_proxy=admin-SECRET"

    def run_e9(self, statuses, env=True):
        """statuses maps (host, 'scoped'|'admin') to the edge's answer."""
        calls = []

        def fake(host, path, **kw):
            who = "scoped" if self.SCOPED in kw["headers"]["Cookie"] else "admin"
            calls.append((host, path, who))
            return statuses[(host, who)], {}, ""

        probe = ep.Probe(self.EXP, "github")
        orig_get, orig_env = ep.http_get, dict(os.environ)
        ep.http_get = fake
        os.environ.pop("EDGE_PROBE_SCOPED_COOKIE", None)
        os.environ.pop("EDGE_PROBE_ADMIN_COOKIE", None)
        if env:
            os.environ["EDGE_PROBE_SCOPED_COOKIE"] = self.SCOPED
            os.environ["EDGE_PROBE_ADMIN_COOKIE"] = self.ADMIN
        try:
            probe.e9_scoped_identity()
        finally:
            ep.http_get = orig_get
            os.environ.clear()
            os.environ.update(orig_env)
        return probe, calls

    GOOD = {("viewer.test", "admin"): 202, ("mux.test", "admin"): 202,
            ("viewer.test", "scoped"): 202, ("mux.test", "scoped"): 403}

    def results(self, probe):
        return [(c["id"], c["host"], c["result"]) for c in probe.checks]

    def test_good_edge_passes_and_uses_the_auth_path(self):
        probe, calls = self.run_e9(self.GOOD)
        self.assertEqual(self.results(probe), [("E9", "viewer.test", ep.PASS), ("E9", "mux.test", ep.PASS)])
        self.assertTrue(all(path == "/oauth2/auth" for _, path, _ in calls))

    def test_mux_admitting_the_project_identity_fails(self):
        for status in (200, 202, 302):
            with self.subTest(status):
                probe, _ = self.run_e9({**self.GOOD, ("mux.test", "scoped"): status})
                self.assertEqual(self.results(probe)[-1], ("E9", "mux.test", ep.FAIL))
                self.assertEqual(ep.exit_code(probe.report()), 1)

    def test_viewer_refusing_the_project_identity_fails(self):
        probe, _ = self.run_e9({**self.GOOD, ("viewer.test", "scoped"): 403})
        self.assertIn(("E9", "viewer.test", ep.FAIL), self.results(probe))

    def test_failed_positive_control_is_broken_and_stops_before_the_verdicts(self):
        for host in ("viewer.test", "mux.test"):
            with self.subTest(host):
                probe, _ = self.run_e9({**self.GOOD, (host, "admin"): 403})
                self.assertEqual(self.results(probe), [("E9-control", host, ep.BROKEN)])
                self.assertEqual(ep.exit_code(probe.report()), 2)

    def test_stale_project_cookie_is_broken_not_a_pass(self):
        probe, _ = self.run_e9({**self.GOOD, ("viewer.test", "scoped"): 401, ("mux.test", "scoped"): 401})
        self.assertEqual(self.results(probe), [("E9", "viewer.test", ep.BROKEN)])
        probe, _ = self.run_e9({**self.GOOD, ("mux.test", "scoped"): 401})
        self.assertEqual(self.results(probe)[-1], ("E9", "mux.test", ep.BROKEN))

    def test_missing_cookies_skip_without_a_request(self):
        probe, calls = self.run_e9(self.GOOD, env=False)
        self.assertEqual(calls, [])
        self.assertEqual(probe.checks, [])
        self.assertIn("E9", probe.skipped[0])

    def test_incomplete_expectations_are_broken(self):
        probe = ep.Probe({"scoped_identity": {"viewer_host": "viewer.test"}}, "github")
        probe.e9_scoped_identity()
        self.assertEqual(probe.checks[0]["result"], ep.BROKEN)

    def test_report_never_carries_a_cookie(self):
        probe, _ = self.run_e9({**self.GOOD, ("mux.test", "scoped"): 200})
        self.assertNotIn("SECRET", json.dumps(probe.report()))


class ExpectationsTest(unittest.TestCase):
    def test_shipped_expectations_parse_and_match_the_documented_edge(self):
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "expectations.json")
        with open(path, encoding="utf-8") as fh:
            exp = json.load(fh)
        self.assertEqual(exp["edge_hosts"][0], "auth.clonk.tv")
        self.assertEqual(exp["canary_url"], "https://auth.clonk.tv/__edge_canary")
        self.assertIs(exp["api_host_live"], True)
        self.assertIn("api.clonk.tv", exp["edge_hosts"])
        self.assertIs(exp["scoped_identity_live"], False)
        self.assertEqual(exp["scoped_identity"]["mux_host"], "mux.clonk.tv")
        # Ziti is off (S9c, gd-xNMLrsgajy): no public router, so tcp 3022 on the
        # edge is a FAIL and ziti.clonk.tv is no longer a declared non-HTTP name.
        self.assertIs(exp["ziti_public_router"], False)
        self.assertEqual(exp["non_http_hosts"], [])
        self.assertNotIn("ziti.clonk.tv", exp["edge_hosts"])

    def test_hooks_host_is_served_by_the_edge(self):
        """GitHub webhooks moved to the edge (cost plan C4, gd-040vrRf2II). The probe is
        not a GitHub address, so /webhooks/ (trigger-engine) and /webhook (github-bridge,
        gd-blqlgxJmap) must refuse it; / is not served."""
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "expectations.json")
        with open(path, encoding="utf-8") as fh:
            exp = json.load(fh)
        self.assertIn("hooks.clonk.tv", exp["edge_hosts"])
        self.assertEqual(exp["exceptions"]["hooks.clonk.tv"], {"/": 404, "/webhooks/edge-probe": 403, "/webhook": 403})

    def test_front_door_is_open_and_nothing_else(self):
        """clonk.tv is the public splash (gd-gQ8hZLd4AQ): / is 200 with no sign-in,
        and the app's paths are not behind it. www.clonk.tv only redirects (E1 does
        not follow it)."""
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "expectations.json")
        with open(path, encoding="utf-8") as fh:
            exp = json.load(fh)
        for host in ("clonk.tv", "www.clonk.tv"):
            self.assertIn(host, exp["edge_hosts"])
        self.assertEqual(exp["exceptions"]["clonk.tv"], {"/": 200, "/lands/": 404})
        self.assertEqual(exp["exceptions"]["www.clonk.tv"], {"/": 301})


if __name__ == "__main__":
    unittest.main()
