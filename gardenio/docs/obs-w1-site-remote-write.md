# W1 — site-deployment second remote-write → gardenio vmsingle ingest

> Phase-1 runbook for epic [[ka-0000006776]] / task [[ka-0000002192]].
> Design basis: `docs/research/obs-w1-victoria-to-grafana.md` (Option A, §5 path 2) and
> `docs/research/obs-synthesis-rollout.md` §3 Step 1. Phase-1 gate [[ka-0000004494]]
> resolved `approve_all`.

This is the **config-flip** that proves the ECS→EKS metrics pipe: a *second* remote-write
on the existing site-deployment (ACME) vmagent, ACME-allow-listed and basic-authed, pushing
into gardenio's in-chart `vmsingle`. No new binary on either side.

```text
site vmagent ──remote_write #1──▶ site vmsingle    (unchanged, primary)
            └─remote_write #2──▶ gardenio ingest ──▶ gardenio vmsingle  (NEW, secondary)
```

The two sides:

| Side | Owner | What lands | Where |
|---|---|---|---|
| **Receiver (gardenio, EKS)** | this repo | Traefik IngressRoute over vmsingle's write paths + basicAuth + allow-list + NetworkPolicy + ESO credential | `helm/gardenio/` (this MR) |
| **Sender (site, ECS)** | site-deployment, **a human lands it** | second `remote_write` on the env vmagent, with `writeRelabelConfigs` allow-list + `basic_auth` | site-deployment terragrunt — **advisory**, gardenio never writes site terraform |

---

## 1. Receiver side (gardenio) — what this MR adds

All gated behind `vmsingle.ingest.enabled` (default `false`); the chart renders identically
until an env opts in. Only the **write** path is exposed — queries stay in-cluster
(Grafana → ClusterIP). vmsingle has no auth of its own, so basicAuth + the path allow-list
are the only thing in front of an unauthenticated TSDB write API.

`templates/vmsingle/ingressroute.yaml`:
- Traefik `IngressRoute` on `vmsingle.ingest.host`, routing ONLY the prefixes in
  `vmsingle.ingest.pathPrefixes` (default: `/api/v1/write`, `/prometheus/api/v1/write`,
  `/influx/write`, `/api/v1/import`) to the vmsingle ClusterIP `:8428`.
- `basicAuth` middleware (`removeHeader: true`) reading an htpasswd Secret.
- Optional `ipWhiteList` (source CIDR allow-list) and `rateLimit` middlewares.
- TLS via the shared chart cert when `tls.enabled`.

`templates/external-secrets.yaml`:
- An `ExternalSecret` (when `externalSecrets.vmsingleIngest.remoteRef` is set) that
  materializes the basicAuth Secret (`users` key, htpasswd format) from the secret store.

`templates/networkpolicies/networkpolicies.yaml`:
- The existing `vmsingle-ingress` policy gains a Traefik→`:8428` clause when
  `vmsingle.ingest.enabled` (only added when the Traefik selectors are configured).

### Example gardenio overlay (devel, public-ingress + bearer MVP — design §5 path 2)

```yaml
vmsingle:
  enabled: true
  ingest:
    enabled: true
    host: vm-ingest.gardenio.e2e.example.com
    # default pathPrefixes are write-only; override only to narrow further
    auth:
      enabled: true
      secretName: gardenio-vmsingle-ingest-auth   # htpasswd blob, key `users`
    ipWhitelist:
      enabled: true
      sourceRange:
        - "<site-devel NAT egress CIDR>/32"       # belt-and-suspenders
      depth: 1
    rateLimit:
      enabled: true
      average: 200
      burst: 400

externalSecrets:
  enabled: true
  vmsingleIngest:
    remoteRef: gardenio/vmsingle-ingest-htpasswd   # store key holding the `users` blob
```

### Provisioning the credential

The basicAuth Secret holds a Traefik `usersFile`-format blob under key `users` — one
`user:bcrypt-hash` line per credential:

```sh
# generate a credential (htpasswd from apache2-utils; bcrypt -B)
htpasswd -nbB site-devel '<strong-random-password>'
# → site-devel:$2y$05$....   put this whole line in the store under the `users` key
```

Store the **htpasswd line** in the secret store at `externalSecrets.vmsingleIngest.remoteRef`;
hand the **plaintext** `site-devel:<password>` to whoever lands the site-side change (below).
If not using ESO, create the Secret directly:

```sh
kubectl create secret generic gardenio-vmsingle-ingest-auth \
  --from-literal=users="$(htpasswd -nbB site-devel '<password>')" -n <gardenio-ns>
```

---

## 2. Sender side (site-deployment) — ADVISORY, a human lands this

gardenio **never writes site terraform**. The snippet below is the config a site-deployment
maintainer applies to the target env's vmagent (mirrors the MR !879 second-exporter
precedent — add one fan-out target, do not re-architect). Start with `devel`/`qa`
(non-prod, no PHI); promote per the design doc phasing.

Add a **second** `-remoteWrite.url` to the env vmagent pointing at the gardenio ingest host,
with a `writeRelabelConfigs` **allow-list** so only the ACME series we want cross the wire
(this is the single biggest cardinality/cost lever — design §7), and `basic_auth` matching
the credential above:

```yaml
# (illustrative vmagent remote_write config — adapt to the tg-templates shape)
remote_write:
  - url: https://<site-primary-vmsingle>/api/v1/write   # #1 primary, UNCHANGED
  - url: https://vm-ingest.gardenio.e2e.example.com/api/v1/write   # #2 NEW → gardenio
    basic_auth:
      username: site-devel
      password: "<password>"                              # from §1; use a secret ref
    # ALLOW-LIST: keep only the ACME series gardenio should hold. Without this the
    # whole site stream lands on gardenio vmsingle and blows up cardinality/cost.
    write_relabel_configs:
      - source_labels: [__name__]
        regex: "(acme_.*|http_server_.*|process_.*|up)"   # ← curate per env
        action: keep
    # Secondary queue must not back-pressure the primary if gardenio is down.
    # vmagent buffers then drops the secondary queue independently; the primary
    # write is unaffected (config-flip fail-safe).
```

Equivalent CLI flags if the env vmagent is flag-configured rather than YAML:

```
-remoteWrite.url=https://vm-ingest.gardenio.e2e.example.com/api/v1/write
-remoteWrite.basicAuth.username=site-devel
-remoteWrite.basicAuth.password=<password>
-remoteWrite.urlRelabelConfig=/path/to/gardenio-keep.yml   # the keep allow-list above
```

### Fail-safe properties (why this is the lowest-risk step)

- **Primary unaffected.** vmagent maintains an independent persistent queue per
  remote-write target. If gardenio is down/unreachable, the *secondary* queue buffers then
  drops; the primary site write keeps working unchanged. Site-deployment grows **no hard
  dependency** on the gardenio EKS cluster (the centralization-epic [[ka-0000001783]]
  constraint).
- **Reversible.** Removing the second URL is a one-line terragrunt revert.
- **Bounded.** The `keep` allow-list caps what crosses the wire; gardenio's vmagent
  `labeldrop` backstop (`templates/vmagent/configmap.yaml`) still strips forbidden labels
  defense-in-depth.

---

## 3. Network path: devel → prod

| Phase | Transport | Notes |
|---|---|---|
| devel/qa (MVP) | **public ingress + basicAuth + TLS** (this MR) | simplest; gardenio `vmsingle.ingest` host + `ipWhitelist` to the site NAT CIDR |
| prod | **private** (VPC peering / Transit Gateway / PrivateLink + internal NLB) | promote before any prod env; no public exposure of the write API. Which of peering/TGW/PrivateLink exists between the ACME ECS VPCs and the gardenio EKS VPC is an **open question** needing site-deployment terraform — carry it on the prod-promote sub-task (synthesis breakdown #7), not here. |

---

## 4. PII / PHI — compose, don't duplicate

- **Source-side strip ([[ka-0000001783]]) is the primary defense** and runs before fan-out.
- **Metrics-first** is itself a mitigation — metric labels are bounded/reviewable. **Logs and
  traces are explicitly out of this MVP** (they need the full strip pipeline; design §6).
- gardenio keeps its scrape-side `labeldrop` backstop as defense-in-depth.
- **prod promotion carries a fresh PII/PHI + cost `gb decision`** — the `approve_all` gate
  does NOT pre-approve prod (synthesis §7).

---

## 5. Verifying the receiver side

```sh
# 1. Render and eyeball the new objects
helm template gardenio helm/gardenio/ \
  --set vmsingle.enabled=true --set vmsingle.ingest.enabled=true \
  --set vmsingle.ingest.host=vm-ingest.example \
  --set vmsingle.ingest.auth.secretName=ingest-auth \
  --set traefik.enabled=true \
  | grep -A40 'kind: IngressRoute' | grep -A40 vmsingle-ingest

# 2. After deploy, a write with a valid credential should 204; without should 401
curl -i -u site-devel:<password> \
  https://vm-ingest.gardenio.e2e.example.com/api/v1/import/prometheus \
  --data-binary 'acme_probe_test 1'
# 3. A query path must NOT be routed (404 from Traefik, not vmsingle) — only write is exposed
curl -i https://vm-ingest.gardenio.e2e.example.com/api/v1/query?query=up   # → 404
```

---

## 6. Related

- Epic [[ka-0000006776]] · gate [[ka-0000004494]] · this task [[ka-0000002192]]
- Design: `docs/research/obs-w1-victoria-to-grafana.md`, synthesis `docs/research/obs-synthesis-rollout.md`
- PII/centralization: [[ka-0000001783]] · gardenio Grafana epic [[ka-0000007895]] · vmalert epic [[ka-0000006536]]
