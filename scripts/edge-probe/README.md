# edge-probe

Checks the public edge from the internet, the way an attacker would, with positive
controls. One implementation, several vantages:

| Vantage | Where | Notes |
|---|---|---|
| `github` | `.github/workflows/edge-probe.yml`, daily 06:17 UTC + manual dispatch | GitHub-hosted runner: a different network from AWS and the laptop. No secrets, no AWS creds. |
| `lambda` | gd-xuxyIMbfCU (`edge_probe.handler`, every 15 min; module `gardenio/terraform/modules/edge-probes`) | Adds Route53 host discovery, per-run instance enumeration and the SG audit (E7). `boto3` is imported lazily, only here. |

```
python3 scripts/edge-probe/edge_probe.py --vantage github \
    --expectations scripts/edge-probe/expectations.json [--output report.json]
python3 scripts/edge-probe/edge_probe_test.py        # unit tests, loopback only
```

stdlib only. The report is one JSON object on stdout:
`{"vantage", "checks": [{"id","host","result","detail"}], "skipped": [...]}`.
It holds no token, cookie or secret (redirect targets are reduced to scheme+host+path; the
E8 bearer is random per run and never printed).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | every check PASS |
| 1 | at least one FAIL: the edge is wrong |
| 2 | at least one BROKEN: a positive control failed, the probe cannot be trusted. **Wins over FAIL.** |

## Checks

| ID | Question | PASS | Otherwise |
|---|---|---|---|
| E0 | Expectations sanity, plus resolving the edge IP and (Lambda) AWS discovery | `edge_hosts` is non-empty and every host under `exceptions` is also in `edge_hosts`; DNS and discovery succeed | **BROKEN**: an empty list would make E1 and E5 check nothing and exit 0 on an open edge |
| E1 | `GET https://<host>/`, no cookies, no auth, no redirects followed, for every `edge_hosts` entry (plus, in Lambda mode, every Route53 A record at the edge EIP) | 401, or 302/303 to `https://auth.clonk.tv/auth/realms/gardenio/...` or same-host `/oauth2/start` | FAIL. 2xx or a redirect anywhere else is reported as `OPEN`. A declared exception (`expectations.exceptions`) replaces the rule for that path: the status must match exactly. |
| E2 | Keycloak admin hidden | `auth.clonk.tv/auth/admin/`, `/auth/realms/master/`, `/auth/admin/master/console/` and `/auth/admin/realms/master` are 404; the gardenio realm console (`/auth/admin/gardenio/console/`, `/auth/admin/realms/gardenio/users`, decision gd-papvgFfwMf) and the one-click Become page (`/gardenio/become/`, with and without `?email=`, gd-VIm9kMDswf) are the E1 sign-in wall, or 404 before the edge serves them | FAIL |
| E3 | **Positive control.** `canary_url` | 200 `edge-canary-open` *and* the E1 classifier says OPEN | **BROKEN**: the canary is not applied (run the operator step), or the classifier is wrong |
| E4 | Unknown SNI at the edge IP | `probe-unknown.clonk.tv` handshake rejected (`ssl_reject_handshake on`); control SNI `auth.clonk.tv` accepted | FAIL if the unknown SNI is served; BROKEN if the control handshake fails |
| E5 | Certificates + HSTS per edge host | Let's Encrypt issuer, SAN contains the host, > 14 days left, `Strict-Transport-Security` present | FAIL |
| E6 | TCP sweep (3 s connect timeout) of 20 ports on the edge IP and the k3s servers | every port refused/timeout except the allowances below | FAIL; `E6-control` is BROKEN if edge 443 does not connect |
| E7 | SG audit (Lambda only) | world-open ingress only on the `*-ziti-edge` SG: tcp 443, and tcp 3022 while `ziti_public_router` | FAIL (a world-open 6443 anywhere always) |
| E8 | Static API token (only when `api_host_live`) | `GET api.clonk.tv/v1/beads` with a random 48-hex bearer gets 401 | FAIL |
| E9 | Project-scoped identity (only when `scoped_identity_live`) | with the **test** identity's session cookie, `<viewer_host>/oauth2/auth` is 200/202 and `<mux_host>/oauth2/auth` is 403; positive control: the admin test identity gets 200/202 on both | FAIL if the mux admits the project-only identity or the viewer refuses it; **BROKEN** if the admin control is refused or the project cookie gets 401 (stale cookie, so a 403 would prove nothing) |

E6 targets: GitHub vantage = every A record of `k3s.clonk.tv` plus the edge IP (resolved from
`edge_ip_from`). Allowances: edge 443 (the sweep's control), edge 3022 while
`ziti_public_router`, and server 80/443 only when both `Host: hooks.clonk.tv` and
`Host: probe-unknown.clonk.tv` return 404. **Deviation, measured live:** Traefik's port-80
entrypoint answers every Host with a 301 to `https://<same host>/`; that fronts no
application, so on port 80 only, that exact redirect also counts. 443 must still be 404.
6443 must never connect. The E6 detail for every target names its open ports and the state of
6443 (`6443=timeout`), so a green run is auditable.

## E9: running it with the test identity

Off by default (`scoped_identity_live: false`): the viewer and mux public names land with
plan gd-P78ygCUgA9 task S4, and the test identity exists only for the project-scoped-humans
live check (gd-6q0cRd9V7Q). To run it, sign in as the realm user who holds only
`/project-scopetest` and as the owner, copy each oauth2-proxy session cookie
(`_oauth2_proxy=...`) and export them, then flip the flag locally:

```
EDGE_PROBE_SCOPED_COOKIE='_oauth2_proxy=...' EDGE_PROBE_ADMIN_COOKIE='_oauth2_proxy=...' \
  python3 scripts/edge-probe/edge_probe.py --vantage github --expectations <expectations with scoped_identity_live=true>
```

Without both variables E9 is listed under `skipped`, not passed. The cookies are read from the
environment, sent only to the two hosts, and never appear in the report. Session cookies expire,
so a 401 is BROKEN: re-sign-in rather than trust the run.

## Non-HTTP hosts on the edge IP

`non_http_hosts` lists names that resolve to the edge EIP but are not nginx vhosts. It is
empty since ziti was switched off (S9c, gd-xNMLrsgajy); `ziti.clonk.tv`, the ziti public
router (tcp 3022), was the only one, and goes back in if ziti does. nginx's default server refuses
their SNI on 443, which is the safe outcome, so for these names E1 passes on a TLS refusal
(a refusal, not a served page) and E5 is skipped (no certificate is presented). A completed
handshake, or an unreachable port, is a FAIL: if the name starts serving something it must be
declared in `edge_hosts` and meet the sign-in rule. Names not listed still follow E1/E5, so an
unexpected host on the EIP fails. A name may not be in both lists (E0 BROKEN).

## Positive controls (why BROKEN exists)

A probe that reports "nothing open" is only evidence if it can see something open. The canary
`location = /__edge_canary` in `gardenio/helm/gardenio-ziti/public-edge.yaml` is open on
purpose; E3 must read it OPEN. E4 needs the control SNI to succeed, E6 needs edge 443 to
connect. If any control fails the run exits 2 instead of reporting a clean result.

## Add a host

1. Add it to `edge_hosts` in `expectations.json` (in the PR that exposes it).
2. If it legitimately answers something other than the sign-in wall, declare it under
   `exceptions` as `{"/path": status}`; undeclared paths follow the E1 rule.
3. Flip `ziti_public_router` / `api_host_live` if the edge now exposes those.
4. Run `python3 scripts/edge-probe/edge_probe.py` locally and check the report.
