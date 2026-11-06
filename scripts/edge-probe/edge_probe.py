#!/usr/bin/env python3
"""edge_probe.py -- check the public edge from the internet, the way an attacker would.

Stdlib only (boto3 is imported lazily, and only for --vantage lambda).
See README.md for what each check means and how to add a host.

Every check returns one of:
  PASS    the edge looks the way expectations.json says it should
  FAIL    the edge is wrong (something is open that should not be)
  BROKEN  a positive control failed, so the probe cannot be trusted

Exit code: 0 all PASS, 1 any FAIL, 2 any BROKEN (BROKEN wins over FAIL).

The probe never sends a real credential anywhere, and the report holds no
token, cookie or secret: redirect targets are reduced to scheme+host+path.
"""

import argparse
import concurrent.futures
import http.client
import json
import os
import secrets
import socket
import ssl
import sys
import time
import urllib.parse

PASS, FAIL, BROKEN = "PASS", "FAIL", "BROKEN"

TIMEOUT = 3          # TCP connect timeout for the sweep and TLS probes (seconds)
HTTP_TIMEOUT = 10    # whole-request timeout for HTTP probes (seconds)
USER_AGENT = "edge-probe/1 (+gardenio)"

SWEEP_PORTS = [22, 80, 443, 2379, 2380, 3000, 3022, 4180, 5432, 6443, 8080,
               8091, 8095, 8443, 8446, 9090, 9800, 10250, 30777, 32356]
CANARY_BODY = "edge-canary-open"
REALM_HOST = "auth.clonk.tv"
REALM_PATH_PREFIX = "/auth/realms/gardenio/"
OAUTH_START_PATH = "/oauth2/start"
MIN_CERT_DAYS = 14
DEFAULT_ZONE = "clonk.tv"
DEFAULT_K3S_HOST = "k3s.clonk.tv"
DEFAULT_API_URL = "https://api.clonk.tv/v1/beads"
UNKNOWN_SNI = "probe-unknown.clonk.tv"
HOOKS_HOST = "hooks.clonk.tv"
ADMIT_STATUSES = (200, 202)  # oauth2-proxy /oauth2/auth: 202 admitted, 403 group refusal

# E1 verdicts from classify(); only PASS is good, and the canary must read OPEN.
OPEN, UNEXPECTED = "OPEN", "UNEXPECTED"


# --------------------------------------------------------------------------
# E1 classifier

def redact_location(location):
    """scheme://host/path only: query strings can carry state/nonce values."""
    if not location:
        return ""
    try:
        parts = urllib.parse.urlsplit(location)
    except ValueError:
        return "<unparseable>"
    return urllib.parse.urlunsplit((parts.scheme, parts.netloc, parts.path, "", ""))


def _location_ok(host, location):
    if not location or any(c in location for c in "\\ \t\r\n") or \
            any(ord(c) < 0x20 for c in location):
        return False
    try:
        parts = urllib.parse.urlsplit(location)
    except ValueError:
        return False
    if parts.scheme == "https" and parts.netloc == REALM_HOST \
            and parts.path.startswith(REALM_PATH_PREFIX):
        return True
    same_host = (parts.scheme == "" and parts.netloc == "") or \
        (parts.scheme == "https" and parts.netloc == host)
    return same_host and parts.path == OAUTH_START_PATH


def classify(host, status, location):
    """The E1 rule. Returns (PASS|OPEN|UNEXPECTED, detail).

    PASS: 401, or 302/303 to the gardenio realm or same-host /oauth2/start.
    OPEN: any 2xx, or a redirect anywhere else (an attacker gets in or gets sent on).
    UNEXPECTED: anything else (404, 403, 5xx, 301...): not the sign-in wall, not open.
    """
    if status == 401:
        return PASS, "401"
    if status in (302, 303):
        if _location_ok(host, location):
            return PASS, f"{status} -> {redact_location(location)}"
        return OPEN, f"{status} redirect to {redact_location(location)}"
    if 200 <= status < 300:
        return OPEN, f"{status}"
    if 300 <= status < 400:
        return OPEN, f"{status} redirect to {redact_location(location)}"
    return UNEXPECTED, f"{status}"


# --------------------------------------------------------------------------
# network helpers

def http_get(host, path, *, scheme="https", ip=None, port=None, headers=None,
             verify=True, sni=None, timeout=HTTP_TIMEOUT):
    """One GET, no cookies, no auth, no redirects followed.

    Returns (status, headers dict lowercased, body text up to 4 KiB).
    Raises OSError / ssl.SSLError / http.client.HTTPException on failure.
    `ip` connects there while `host` stays the Host header (and the SNI unless
    `sni` says otherwise); verify=False is for IP probes that cannot match a cert.
    """
    hdrs = {"User-Agent": USER_AGENT, "Accept": "*/*", "Connection": "close", "Host": host}
    hdrs.update(headers or {})
    if scheme == "https":
        ctx = ssl.create_default_context()
        if not verify:
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
        conn = _SniHTTPSConnection(ip or host, port or 443, sni or host,
                                   timeout=timeout, context=ctx)
    else:
        conn = http.client.HTTPConnection(ip or host, port or 80, timeout=timeout)
    try:
        conn.request("GET", path, headers=hdrs)
        resp = conn.getresponse()
        body = resp.read(4096).decode("utf-8", "replace")
        return resp.status, {k.lower(): v for k, v in resp.getheaders()}, body
    finally:
        conn.close()


class _SniHTTPSConnection(http.client.HTTPSConnection):
    """HTTPSConnection that dials one name (an IP) but presents another as SNI."""

    def __init__(self, dial, port, sni, **kw):
        super().__init__(dial, port, **kw)
        self._sni = sni

    def connect(self):
        http.client.HTTPConnection.connect(self)
        self.sock = self._context.wrap_socket(self.sock, server_hostname=self._sni)


def resolve_a(host):
    """All IPv4 addresses of `host`, sorted."""
    infos = socket.getaddrinfo(host, None, socket.AF_INET, socket.SOCK_STREAM)
    return sorted({i[4][0] for i in infos})


def tls_handshake(ip, sni, timeout=TIMEOUT, port=443):
    """Handshake to ip:port with SNI `sni`. Returns 'accepted' | 'rejected' | 'unreachable'."""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    try:
        with socket.create_connection((ip, port), timeout=timeout) as raw:
            with ctx.wrap_socket(raw, server_hostname=sni):
                return "accepted", ""
    except ssl.SSLError as exc:
        return "rejected", f"{type(exc).__name__}: {exc.reason or exc}"
    except (ConnectionResetError, BrokenPipeError, EOFError) as exc:
        return "rejected", type(exc).__name__
    except OSError as exc:
        return "unreachable", f"{type(exc).__name__}: {exc}"


def sweep_ports(ip, ports, timeout=TIMEOUT, workers=32):
    """TCP connect to each port. Returns {port: 'open'|'refused'|'timeout'|'error'}."""
    def one(port):
        try:
            with socket.create_connection((ip, port), timeout=timeout):
                return port, "open"
        except ConnectionRefusedError:
            return port, "refused"
        except socket.timeout:
            return port, "timeout"
        except OSError:
            return port, "error"
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        return dict(pool.map(one, ports))


# --------------------------------------------------------------------------
# pure evaluators (unit-tested without a network)

def evaluate_sweep(roles, states, ziti_public_router, server_http_ok, edge_http_ok=None):
    """Judge one sweep target. Returns (result, detail).

    roles: subset of {'edge', 'server'} (an IP can be both).
    states: {port: state} from sweep_ports.
    server_http_ok(port) -> (bool, why): the hooks-route-only check for server 80/443.
    edge_http_ok(port) -> (bool, why): the redirect-only check for edge 80. Without
    it an open edge 80 fails, as before the edge had an HTTP listener.
    """
    open_ports = sorted(p for p, s in states.items() if s == "open")
    bad = []
    for port in open_ports:
        if port == 6443:
            bad.append("6443 open (must never connect)")
        elif "edge" in roles and port == 443:
            continue
        elif "edge" in roles and port == 3022 and ziti_public_router:
            continue
        elif "edge" in roles and port == 80 and edge_http_ok is not None:
            ok, why = edge_http_ok(port)
            if not ok:
                bad.append(f"80 answers more than an https redirect ({why})")
        elif "server" in roles and port in (80, 443):
            ok, why = server_http_ok(port)
            if not ok:
                bad.append(f"{port} answers more than the hooks route ({why})")
        else:
            bad.append(f"{port} open")
    summary = f"open={open_ports}; 6443={states.get(6443, 'not-probed')}"
    if bad:
        return FAIL, "; ".join(bad) + f" [{summary}]"
    return PASS, summary


def _san_matches(host, pattern):
    if pattern == host:
        return True
    if pattern.startswith("*."):
        return host.split(".", 1)[-1] == pattern[2:] and "." in host
    return False


def evaluate_cert(host, cert, now=None):
    """E5 certificate half. cert is ssl.getpeercert() output. Returns (result, detail)."""
    now = time.time() if now is None else now
    problems = []
    issuer_orgs = [v for rdn in cert.get("issuer", ()) for (k, v) in rdn
                   if k == "organizationName"]
    if "Let's Encrypt" not in issuer_orgs:
        problems.append(f"issuer org {issuer_orgs or 'none'} is not Let's Encrypt")
    sans = [v for (k, v) in cert.get("subjectAltName", ()) if k == "DNS"]
    if not any(_san_matches(host, s) for s in sans):
        problems.append(f"SAN list {sans} does not contain {host}")
    days = (ssl.cert_time_to_seconds(cert["notAfter"]) - now) / 86400
    if days <= MIN_CERT_DAYS:
        problems.append(f"only {days:.1f} days left (need > {MIN_CERT_DAYS})")
    detail = f"issuer={issuer_orgs}, {days:.1f} days left"
    return (FAIL, "; ".join(problems)) if problems else (PASS, detail)


def _world_cidrs(perm):
    out = [r.get("CidrIp") for r in perm.get("IpRanges", [])]
    out += [r.get("CidrIpv6") for r in perm.get("Ipv6Ranges", [])]
    return [c for c in out if c in ("0.0.0.0/0", "::/0")]


def audit_security_groups(groups, ziti_public_router):
    """E7. groups = DescribeSecurityGroups()['SecurityGroups']. Returns list of problem strings.

    World-open ingress is allowed only on the edge SG (name ends -ziti-edge):
    tcp 80 and 443, and tcp 3022 while ziti_public_router. World-open 6443 anywhere is
    a problem. Edge 80 is the redirect-only HTTP listener (8f2a50ab); E6 checks that it
    answers nothing but a redirect to https.
    """
    problems = []
    for sg in groups:
        name = sg.get("GroupName", "")
        gid = sg.get("GroupId", "?")
        is_edge = name.endswith("-ziti-edge")
        allowed = {80, 443} | ({3022} if ziti_public_router else set())
        for perm in sg.get("IpPermissions", []):
            cidrs = _world_cidrs(perm)
            if not cidrs:
                continue
            proto = perm.get("IpProtocol")
            lo, hi = perm.get("FromPort"), perm.get("ToPort")
            label = f"{name or gid} ({gid}) world-open {proto} {lo}-{hi}"
            if proto in ("tcp", "6") and lo is not None and hi is not None:
                if lo <= 6443 <= hi:
                    problems.append(f"{label} includes 6443")
                    continue
                if is_edge and lo == hi and lo in allowed:
                    continue
            elif proto == "-1":
                label = f"{name or gid} ({gid}) world-open all protocols"
                if lo is None or lo <= 6443 <= (hi if hi is not None else 65535):
                    problems.append(f"{label} includes 6443")
                    continue
            problems.append(label)
    return problems


def hooks_probe_ok(port, host, status, location):
    """One server 80/443 probe response. 404 is the rule. On port 80 Traefik's entrypoint
    also answers every Host with a 301/308 to https://<same host>/ (measured live,
    2026-10-13); that redirect fronts no application, so it counts, and the 443 probe of
    the same two Hosts still has to 404."""
    if status == 404:
        return True
    if port == 80 and status in (301, 308) and location:
        try:
            parts = urllib.parse.urlsplit(location)
        except ValueError:
            return False
        return parts.scheme == "https" and parts.netloc == host and parts.path in ("", "/") \
            and not parts.query
    return False


def edge_redirect_ok(host, status, location):
    """One edge port-80 probe of a served host: it must be a 301/308 to
    https://<same host>/ (the probe asks for "/") and nothing else."""
    if status not in (301, 308) or not location:
        return False
    try:
        parts = urllib.parse.urlsplit(location)
    except ValueError:
        return False
    return parts.scheme == "https" and parts.netloc == host and parts.path == "/" \
        and not parts.query


def exit_code(report):
    results = {c["result"] for c in report["checks"]}
    if BROKEN in results:
        return 2
    if FAIL in results:
        return 1
    return 0


# --------------------------------------------------------------------------
# the probe

class Probe:
    def __init__(self, expectations, vantage, *, vpc_id=None, boto=None, zone_id=None):
        self.exp = expectations
        self.vantage = vantage
        self.vpc_id = vpc_id
        self.zone_id = zone_id     # Route53 zone id; skips ListHostedZonesByName (IAM is scoped to the zone)
        self.boto = boto           # injected boto3 module (lambda vantage)
        self.checks = []
        self.skipped = []
        self.edge_ips = []

    def rec(self, cid, host, result, detail):
        self.checks.append({"id": cid, "host": host, "result": result, "detail": detail})

    # -- E1 ---------------------------------------------------------------
    def e1_edge_names(self, hosts):
        exceptions = self.exp.get("exceptions", {})
        for host in hosts:
            declared = exceptions.get(host, {})
            if "/" not in declared:
                self._e1_rule(host)
            for path, want in sorted(declared.items()):
                try:
                    status, _, _ = http_get(host, path)
                except (OSError, http.client.HTTPException) as exc:
                    self.rec("E1", host, FAIL, f"GET {path}: {type(exc).__name__}: {exc}")
                    continue
                if status == want:
                    self.rec("E1", host, PASS, f"GET {path} -> {status} (declared exception)")
                else:
                    self.rec("E1", host, FAIL,
                             f"GET {path} -> {status}, declared exception says {want}")

    def e1_non_http_hosts(self, hosts):
        """A declared non-HTTP name on the edge IP must be refused at the TLS layer.

        Used for names that share the edge EIP but are not nginx vhosts (the ziti public
        router). nginx's default server refuses their SNI, which is the safe outcome. Any
        completed handshake means something started serving the name: that is a FAIL until
        it is declared properly. Names not listed here still go through E1/E5 as usual.
        """
        for host in hosts:
            for ip in self.edge_ips:
                outcome, why = tls_handshake(ip, host)
                if outcome == "rejected":
                    self.rec("E1", host, PASS, f"{ip}:443 SNI refused ({why}); declared non-HTTP host")
                elif outcome == "accepted":
                    self.rec("E1", host, FAIL,
                             f"{ip}:443 completed a handshake for declared non-HTTP host; "
                             "it is serving something, so declare it in edge_hosts or drop it from non_http_hosts")
                else:
                    self.rec("E1", host, FAIL, f"{ip}:443 inconclusive for declared non-HTTP host: {why}")

    def _e1_rule(self, host):
        try:
            status, hdrs, _ = http_get(host, "/")
        except (OSError, http.client.HTTPException) as exc:
            self.rec("E1", host, FAIL, f"GET /: {type(exc).__name__}: {exc}")
            return
        verdict, detail = classify(host, status, hdrs.get("location"))
        if verdict == PASS:
            self.rec("E1", host, PASS, f"GET / {detail}")
        elif verdict == OPEN:
            self.rec("E1", host, FAIL, f"OPEN: GET / {detail}")
        else:
            self.rec("E1", host, FAIL, f"GET / {detail}: neither the sign-in wall nor a declared exception")

    # -- E2 ---------------------------------------------------------------
    # The master realm's console and admin API: always 404.
    E2_HIDDEN = ("/auth/admin/", "/auth/realms/master/", "/auth/admin/master/console/",
                 "/auth/admin/realms/master")
    # The gardenio realm's console (decision gd-papvgFfwMf) and the one-click
    # Become page that calls its admin API (gd-VIm9kMDswf): behind the sign-in
    # wall once the edge serves them, 404 before. Never OPEN.
    E2_WALLED = ("/auth/admin/gardenio/console/", "/auth/admin/realms/gardenio/users",
                 "/gardenio/become/", "/gardenio/become/?email=edge-probe%40example.com")

    def e2_keycloak_admin(self):
        host = urllib.parse.urlsplit(self.exp["canary_url"]).netloc
        for path in self.E2_HIDDEN + self.E2_WALLED:
            try:
                status, hdrs, _ = http_get(host, path)
            except (OSError, http.client.HTTPException) as exc:
                self.rec("E2", host, FAIL, f"GET {path}: {type(exc).__name__}: {exc}")
                continue
            if status == 404:
                self.rec("E2", host, PASS, f"GET {path} -> 404")
            elif path in self.E2_WALLED and classify(host, status, hdrs.get("location"))[0] == PASS:
                self.rec("E2", host, PASS, f"GET {path} -> {classify(host, status, hdrs.get('location'))[1]}")
            elif path in self.E2_WALLED:
                self.rec("E2", host, FAIL, f"GET {path} -> {status}, must be the sign-in wall or 404")
            else:
                self.rec("E2", host, FAIL, f"GET {path} -> {status}, must be 404")

    # -- E3 ---------------------------------------------------------------
    def e3_canary(self):
        url = urllib.parse.urlsplit(self.exp["canary_url"])
        host = url.netloc
        try:
            status, hdrs, body = http_get(host, url.path)
        except (OSError, http.client.HTTPException) as exc:
            self.rec("E3", host, BROKEN, f"canary unreachable: {type(exc).__name__}: {exc}")
            return
        verdict, detail = classify(host, status, hdrs.get("location"))
        problems = []
        if status != 200 or body.strip() != CANARY_BODY:
            problems.append(f"expected 200 'edge-canary-open', got {status}")
        if verdict != OPEN:
            problems.append(f"the E1 classifier said {verdict}, not OPEN")
        if problems:
            self.rec("E3", host, BROKEN, "; ".join(problems) + " -- canary not applied, or the probe is broken")
        else:
            self.rec("E3", host, PASS, "canary served and classified OPEN")

    # -- E4 ---------------------------------------------------------------
    def e4_unknown_sni(self):
        ip = self.edge_ips[0]
        control, why = tls_handshake(ip, REALM_HOST)
        if control != "accepted":
            self.rec("E4", ip, BROKEN, f"control handshake SNI {REALM_HOST} was {control} ({why})")
            return
        outcome, why = tls_handshake(ip, UNKNOWN_SNI)
        if outcome == "rejected":
            self.rec("E4", ip, PASS, f"SNI {UNKNOWN_SNI} rejected ({why}); control SNI accepted")
        elif outcome == "accepted":
            self.rec("E4", ip, FAIL, f"SNI {UNKNOWN_SNI} completed a handshake; default server must reject")
        else:
            self.rec("E4", ip, BROKEN, f"SNI {UNKNOWN_SNI} probe inconclusive: {why}")

    # -- E5 ---------------------------------------------------------------
    def e5_certs(self, hosts):
        for host in hosts:
            ctx = ssl.create_default_context()
            try:
                with socket.create_connection((host, 443), timeout=HTTP_TIMEOUT) as raw:
                    with ctx.wrap_socket(raw, server_hostname=host) as tls:
                        cert = tls.getpeercert()
            except (OSError, ssl.SSLError) as exc:
                self.rec("E5", host, FAIL, f"certificate: {type(exc).__name__}: {exc}")
                continue
            result, detail = evaluate_cert(host, cert)
            try:
                _, hdrs, _ = http_get(host, "/")
            except (OSError, http.client.HTTPException) as exc:
                result, detail = FAIL, f"{detail}; HSTS fetch failed: {type(exc).__name__}"
            else:
                if "strict-transport-security" not in hdrs:
                    result, detail = FAIL, f"{detail}; no Strict-Transport-Security header"
            self.rec("E5", host, result, detail)

    # -- E6 ---------------------------------------------------------------
    def e6_sweep(self, server_ips):
        edge = set(self.edge_ips)
        targets = sorted(set(server_ips) | edge)
        ziti = bool(self.exp.get("ziti_public_router"))
        for ip in targets:
            roles = set()
            if ip in edge:
                roles.add("edge")
            if ip in server_ips:
                roles.add("server")
            states = sweep_ports(ip, SWEEP_PORTS)
            result, detail = evaluate_sweep(
                roles, states, ziti, lambda port, ip=ip: self._server_hooks_only(ip, port),
                edge_http_ok=lambda port, ip=ip: self._edge_redirect_only(ip, port))
            label = "+".join(sorted(roles))
            self.rec("E6", ip, result, f"[{label}] {detail}")
            if "edge" in roles and states.get(443) != "open":
                self.rec("E6-control", ip, BROKEN,
                         f"edge 443 did not connect ({states.get(443)}); the sweep cannot be trusted")
            elif "edge" in roles:
                self.rec("E6-control", ip, PASS, "edge 443 connects")

    def _edge_redirect_only(self, ip, port):
        """Edge 80 may only redirect: every served host gets a 301/308 to https on the
        same host, and an unknown Host gets no content (nginx closes it, 444)."""
        for host in self.exp.get("edge_hosts", []):
            try:
                status, hdrs, _ = http_get(host, "/", scheme="http", ip=ip, port=port,
                                           timeout=HTTP_TIMEOUT)
            except (OSError, http.client.HTTPException) as exc:
                return False, f"Host {host}: {type(exc).__name__}"
            if not edge_redirect_ok(host, status, hdrs.get("location")):
                return False, f"Host {host}: {status} -> {hdrs.get('location')}"
        try:
            status, _, _ = http_get(UNKNOWN_SNI, "/", scheme="http", ip=ip, port=port,
                                    timeout=HTTP_TIMEOUT)
        except (OSError, http.client.HTTPException):
            return True, ""
        if status in (400, 404, 421):
            return True, ""
        return False, f"Host {UNKNOWN_SNI}: {status} (an unknown Host must get no content)"

    def _server_hooks_only(self, ip, port):
        """Server 80/443 may answer only Traefik's hooks route: both probe Hosts must 404."""
        scheme = "https" if port == 443 else "http"
        for host in (HOOKS_HOST, UNKNOWN_SNI):
            try:
                status, hdrs, _ = http_get(host, "/", scheme=scheme, ip=ip, port=port,
                                           verify=False, sni=host, timeout=HTTP_TIMEOUT)
            except (OSError, http.client.HTTPException) as exc:
                return False, f"Host {host}: {type(exc).__name__}"
            if not hooks_probe_ok(port, host, status, hdrs.get("location")):
                return False, f"Host {host} -> {status}"
        return True, "hooks route only"

    # -- E7 (lambda only) ---------------------------------------------------
    def e7_sg_audit(self):
        ec2 = self.boto.client("ec2")
        groups = []
        for page in ec2.get_paginator("describe_security_groups").paginate(
                Filters=[{"Name": "vpc-id", "Values": [self.vpc_id]}]):
            groups.extend(page["SecurityGroups"])
        problems = audit_security_groups(groups, bool(self.exp.get("ziti_public_router")))
        if problems:
            self.rec("E7", self.vpc_id, FAIL, "; ".join(problems))
        else:
            self.rec("E7", self.vpc_id, PASS, f"{len(groups)} security groups, world-open ingress only as allowed")

    # -- E8 ---------------------------------------------------------------
    def e8_api_token(self):
        url = urllib.parse.urlsplit(self.exp.get("api_url", DEFAULT_API_URL))
        fake = secrets.token_hex(24)  # 48 hex chars, right shape, no meaning
        try:
            status, _, _ = http_get(url.netloc, url.path,
                                    headers={"Authorization": f"Bearer {fake}"})
        except (OSError, http.client.HTTPException) as exc:
            self.rec("E8", url.netloc, FAIL, f"{type(exc).__name__}: {exc}")
            return
        if status == 401:
            self.rec("E8", url.netloc, PASS, "static-token-shaped bearer rejected with 401")
        else:
            self.rec("E8", url.netloc, FAIL, f"static-token-shaped bearer got {status}, must be 401")

    # -- E9 ---------------------------------------------------------------
    def e9_scoped_identity(self):
        """A project-scoped human reaches the viewer edge and never the mux edge.

        Sends each test identity's oauth2-proxy session cookie (read from the
        environment, never written to the report) to `<host><auth_path>`, the
        edge's own group check: 202/200 admits, 403 is a group refusal. A cookie
        the edge does not accept at all (401) proves nothing about groups, so it
        is BROKEN, not PASS.
        """
        cfg = self.exp.get("scoped_identity") or {}
        viewer, mux = cfg.get("viewer_host"), cfg.get("mux_host")
        path = cfg.get("auth_path", "/oauth2/auth")
        scoped_env = cfg.get("scoped_cookie_env", "EDGE_PROBE_SCOPED_COOKIE")
        admin_env = cfg.get("admin_cookie_env", "EDGE_PROBE_ADMIN_COOKIE")
        if not viewer or not mux:
            self.rec("E9", "expectations", BROKEN, "scoped_identity needs viewer_host and mux_host")
            return
        scoped_cookie, admin_cookie = os.environ.get(scoped_env), os.environ.get(admin_env)
        if not scoped_cookie or not admin_cookie:
            missing = [n for n, v in ((scoped_env, scoped_cookie), (admin_env, admin_cookie)) if not v]
            self.skipped.append("E9: " + ", ".join(missing) + " not set (the test identities' session cookies)")
            return

        def ask(host, cookie):
            return http_get(host, path, headers={"Cookie": cookie})[0]

        try:
            # Positive control first: the admin identity is admitted on both hosts,
            # so a 403 for the scoped one below means the group rule, not a dead route.
            for host in (viewer, mux):
                got = ask(host, admin_cookie)
                if got not in ADMIT_STATUSES:
                    self.rec("E9-control", host, BROKEN,
                             f"admin identity got {got}, must be 200/202 -- cookie stale or route wrong")
                    return
            got = ask(viewer, scoped_cookie)
            if got in ADMIT_STATUSES:
                self.rec("E9", viewer, PASS, f"project-only identity admitted by the viewer edge ({got})")
            elif got == 401:
                self.rec("E9", viewer, BROKEN, "project-only identity got 401: cookie stale, so the mux 403 below proves nothing")
                return
            else:
                self.rec("E9", viewer, FAIL, f"project-only identity got {got} at the viewer edge, must be 200/202")
            got = ask(mux, scoped_cookie)
            if got == 403:
                self.rec("E9", mux, PASS, "project-only identity refused by the mux edge (403)")
            elif got == 401:
                self.rec("E9", mux, BROKEN, "project-only identity got 401 at the mux edge: not a group refusal")
            else:
                self.rec("E9", mux, FAIL, f"project-only identity got {got} at the mux edge, must be 403")
        except (OSError, http.client.HTTPException) as exc:
            self.rec("E9", viewer, BROKEN, f"{type(exc).__name__}: {exc}")

    # -- discovery -----------------------------------------------------------
    def lambda_discovery(self, edge_ip):
        """Route53 A records at the edge EIP, running instance public IPs, the VPC id."""
        r53 = self.boto.client("route53")
        zone_id = self.zone_id
        if not zone_id:
            zone = self.exp.get("zone", DEFAULT_ZONE)
            zones = r53.list_hosted_zones_by_name(DNSName=zone + ".", MaxItems="1")["HostedZones"]
            if not zones or zones[0]["Name"] != zone + ".":
                raise RuntimeError(f"Route53 zone {zone} not found")
            zone_id = zones[0]["Id"]
        names = set()
        for page in r53.get_paginator("list_resource_record_sets").paginate(
                HostedZoneId=zone_id):
            for rs in page["ResourceRecordSets"]:
                if rs["Type"] != "A" or "*" in rs["Name"] or "\\052" in rs["Name"]:
                    continue
                if any(r["Value"] == edge_ip for r in rs.get("ResourceRecords", [])):
                    names.add(rs["Name"].rstrip("."))
        ec2 = self.boto.client("ec2")
        if not self.vpc_id:
            k3s_ips = resolve_a(self.exp.get("k3s_host", DEFAULT_K3S_HOST))
            found = ec2.describe_instances(Filters=[{"Name": "ip-address", "Values": k3s_ips}])
            vpcs = {i["VpcId"] for r in found["Reservations"] for i in r["Instances"]}
            if len(vpcs) != 1:
                raise RuntimeError(f"cannot infer the cluster VPC from {k3s_ips}: {sorted(vpcs)}")
            self.vpc_id = vpcs.pop()
        ips = set()
        for page in ec2.get_paginator("describe_instances").paginate(Filters=[
                {"Name": "vpc-id", "Values": [self.vpc_id]},
                {"Name": "instance-state-name", "Values": ["running"]}]):
            for r in page["Reservations"]:
                for i in r["Instances"]:
                    if i.get("PublicIpAddress"):
                        ips.add(i["PublicIpAddress"])
        return sorted(names), sorted(ips - {edge_ip})

    # -- run --------------------------------------------------------------
    def run(self):
        exp = self.exp
        hosts = list(exp.get("edge_hosts") or [])
        if not hosts:
            # no hosts means E1/E5 emit nothing: a vacuous green on an open edge
            self.rec("E0", "expectations", BROKEN, "edge_hosts is empty, so E1 and E5 would check nothing")
        for host in sorted(set(exp.get("exceptions", {})) - set(hosts)):
            self.rec("E0", host, BROKEN, "host has exceptions but is not in edge_hosts, so it is never checked")
        non_http = list(exp.get("non_http_hosts") or [])
        for host in sorted(set(non_http) & (set(hosts) | set(exp.get("exceptions", {})))):
            self.rec("E0", host, BROKEN, "host is in both non_http_hosts and edge_hosts/exceptions, so its expectation is ambiguous")
        try:
            self.edge_ips = resolve_a(exp["edge_ip_from"])
        except OSError as exc:
            self.rec("E0", exp["edge_ip_from"], BROKEN, f"cannot resolve the edge IP: {exc}")
            return self.report()
        server_ips = []
        if self.vantage == "lambda":
            try:
                extra, server_ips = self.lambda_discovery(self.edge_ips[0])
                hosts += [h for h in extra if h not in hosts]
            # any AWS failure makes E6/E7 untrustworthy, so report it as BROKEN
            except Exception as exc:  # noqa: BLE001
                self.rec("E0", "aws", BROKEN, f"lambda discovery failed: {type(exc).__name__}: {exc}")
                return self.report()
        else:
            try:
                server_ips = resolve_a(exp.get("k3s_host", DEFAULT_K3S_HOST))
            except OSError as exc:
                self.rec("E0", exp.get("k3s_host", DEFAULT_K3S_HOST), BROKEN,
                         f"cannot resolve the k3s servers: {exc}")
                return self.report()
            self.skipped.append("E7: lambda vantage only (needs AWS read access)")
        # discovered names that are declared non-HTTP get the TLS-refusal rule, not E1/E5
        hosts = [h for h in hosts if h not in non_http]
        self.e1_edge_names(hosts)
        if non_http:
            self.e1_non_http_hosts(non_http)
            self.skipped.append("E5: non_http_hosts present no certificate by design: " + ", ".join(non_http))
        self.e2_keycloak_admin()
        self.e3_canary()
        self.e4_unknown_sni()
        self.e5_certs(hosts)
        self.e6_sweep(server_ips)
        if self.vantage == "lambda":
            try:
                self.e7_sg_audit()
            # a failed SG read must surface as BROKEN, never as a silent pass
            except Exception as exc:  # noqa: BLE001
                self.rec("E7", self.vpc_id or "?", BROKEN, f"{type(exc).__name__}: {exc}")
        if exp.get("api_host_live"):
            self.e8_api_token()
        else:
            self.skipped.append("E8: expectations.api_host_live is false")
        if exp.get("scoped_identity_live"):
            self.e9_scoped_identity()
        else:
            self.skipped.append("E9: expectations.scoped_identity_live is false")
        return self.report()

    def report(self):
        return {"vantage": self.vantage, "checks": self.checks, "skipped": self.skipped}


# --------------------------------------------------------------------------
# entry points

def load_expectations(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


METRIC_NAMESPACE = "GardenioEdgeProbe"


def summarize(report):
    """(failures, broken, message): counts plus a short, secret-free alert body."""
    bad = [c for c in report["checks"] if c["result"] in (FAIL, BROKEN)]
    failures = sum(1 for c in bad if c["result"] == FAIL)
    broken = len(bad) - failures
    lines = [f"edge-probe ({report['vantage']}): {failures} FAIL, {broken} BROKEN"]
    lines += [f"{c['result']} {c['id']} {c['host']}: {c['detail']}" for c in bad]
    return failures, broken, "\n".join(lines)


def publish_results(report, cw, sns, topic_arn):
    """Every run: Heartbeat/Failures/Broken metrics. On any FAIL or BROKEN: one SNS message.

    Metrics go first so a failed publish still leaves the heartbeat; the SNS error
    then propagates and trips the Lambda Errors alarm instead of passing silently.
    """
    failures, broken, message = summarize(report)
    cw.put_metric_data(Namespace=METRIC_NAMESPACE, MetricData=[
        {"MetricName": "Heartbeat", "Value": 1, "Unit": "Count"},
        {"MetricName": "Failures", "Value": failures, "Unit": "Count"},
        {"MetricName": "Broken", "Value": broken, "Unit": "Count"},
    ])
    if (failures or broken) and topic_arn:
        sns.publish(TopicArn=topic_arn, Subject="edge-probe: FAIL/BROKEN"[:100], Message=message)


def handler(event, context):
    """AWS Lambda entry point (gd-xuxyIMbfCU).

    Env: EDGE_PROBE_VANTAGE (must be lambda), EDGE_PROBE_ZONE_ID, EDGE_PROBE_VPC_ID,
    EDGE_PROBE_TOPIC_ARN, optional EDGE_PROBE_EXPECTATIONS.
    """
    import boto3  # lazy: stdlib-only everywhere else
    env = os.environ
    here = os.path.dirname(os.path.abspath(__file__))
    exp = load_expectations(env.get("EDGE_PROBE_EXPECTATIONS", os.path.join(here, "expectations.json")))
    report = Probe(exp, env.get("EDGE_PROBE_VANTAGE", "lambda"),
                   vpc_id=env.get("EDGE_PROBE_VPC_ID") or None,
                   zone_id=env.get("EDGE_PROBE_ZONE_ID") or None, boto=boto3).run()
    report["exit_code"] = exit_code(report)
    publish_results(report, boto3.client("cloudwatch"), boto3.client("sns"),
                    env.get("EDGE_PROBE_TOPIC_ARN"))
    return report


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--vantage", choices=["github", "lambda"], default="github")
    ap.add_argument("--expectations", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "expectations.json"))
    ap.add_argument("--vpc-id", default=os.environ.get("PROBE_VPC_ID"),
                    help="cluster VPC (lambda vantage; inferred from k3s.clonk.tv when omitted)")
    ap.add_argument("--output", help="also write the JSON report to this file")
    args = ap.parse_args(argv)
    boto = None
    if args.vantage == "lambda":
        import boto3  # lazy
        boto = boto3
    report = Probe(load_expectations(args.expectations), args.vantage,
                   vpc_id=args.vpc_id, boto=boto).run()
    text = json.dumps(report, indent=2, sort_keys=True)
    print(text)
    if args.output:
        with open(args.output, "w", encoding="utf-8") as fh:
            fh.write(text + "\n")
    return exit_code(report)


if __name__ == "__main__":
    sys.exit(main())
