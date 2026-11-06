# Cluster Traefik `forwardedHeaders.trustedIPs` — verification runbook

**Audit:** `docs/audits/ingress-tls-static.md` §F9 (not in this repo — the audits tree was excluded from the 2026-09-16 export)
**Bead:** ka-0000000601
**Affected chart objects:**
[`helm/gardenio/templates/forwardauth/middleware.yaml`](../../helm/gardenio/templates/forwardauth/middleware.yaml)
(`trustForwardHeader: true`),
[`helm/gardenio/templates/security-headers.yaml`](../../helm/gardenio/templates/security-headers.yaml)
(HSTS / scheme-derived headers).

## What this is

The gardenio Helm chart's forwardAuth middleware sets
`trustForwardHeader: true`. That tells Traefik to forward the **incoming**
`X-Forwarded-*` headers to oauth2-proxy unchanged. This is only safe when the
**cluster-level Traefik install** (the upstream Traefik release that fronts
this chart — NOT shipped by this chart) is configured to trust those headers
only when they come from the front-most proxy: the cloud LB, the CDN, or the
ingress NLB.

That trust is expressed in Traefik's static config as
`entryPoints.<name>.forwardedHeaders.trustedIPs` — a list of CIDRs from
which Traefik will accept incoming `X-Forwarded-*`. From any source outside
those CIDRs, Traefik strips and rewrites the headers from its own observation
of the connection.

## What goes wrong if `trustedIPs` is missing or wrong

If `forwardedHeaders.trustedIPs` is unset, set to `0.0.0.0/0`, or set to a
CIDR that is wider than the actual front-proxy fleet, a client that can reach
Traefik directly (e.g. through a misrouted `Host:` header, a flat overlay
network, a misconfigured `LoadBalancer` Service that bypasses the LB, or an
in-cluster pivot) can forge:

- `X-Forwarded-Proto: https` — oauth2-proxy with `--reverse-proxy=true` will
  treat a plaintext request as HTTPS, set its session cookie with the
  `Secure` attribute under the wrong scheme, and answer the auth callback as
  if TLS terminated upstream.
- `X-Forwarded-For: <victim-ip>` — log/audit pipelines and any IP-based
  policy in oauth2-proxy / downstream apps will attribute the request to the
  spoofed address.
- `X-Forwarded-Host: <other-tenant>` — combined with a forwarded scheme this
  can confuse host-aware redirect logic in oauth2-proxy callbacks.

The chart cannot defend against this on its own because `trustForwardHeader`
is a per-middleware flag that runs **after** Traefik has already accepted (or
rejected) the headers based on the connection's source IP. The only place to
get this right is the cluster-Traefik static config.

## How to verify

The exact mechanism depends on how the cluster-Traefik release is installed.
The two common shapes are upstream Helm chart values and an `additionalArguments`
list. Check whichever is in use.

### 1. Find the cluster-Traefik release

```sh
# Common namespaces: traefik, traefik-system, kube-system, ingress
kubectl get pods -A -l app.kubernetes.io/name=traefik
kubectl get helmrelease,helmrelease.helm.toolkit.fluxcd.io -A 2>/dev/null | grep -i traefik
helm list -A | grep -i traefik
```

### 2. Inspect the entryPoint static config

If installed via the upstream Traefik Helm chart, look at the rendered
values:

```sh
helm get values <release> -n <namespace> -o yaml | \
  yq '.ports, .additionalArguments'
```

You are looking for one of:

```yaml
ports:
  websecure:
    forwardedHeaders:
      trustedIPs:
        - 10.0.0.0/8        # example: the VPC CIDR carrying the LB
        - 172.16.0.0/12     # example: the NLB target subnet
```

…or, equivalently, on the CLI form:

```yaml
additionalArguments:
  - "--entryPoints.websecure.forwardedHeaders.trustedIPs=10.0.0.0/8,172.16.0.0/12"
```

The CIDRs must be the **smallest set that still covers the LB / front-proxy
fleet**. `0.0.0.0/0` defeats the protection. An empty list means Traefik
accepts forwarded headers from nobody — also fine, but in that case we
should additionally set `trustForwardHeader: false` in this chart (separate
follow-up bead — see *Hardening follow-up* below).

### 3. Inspect the running Traefik static config

If the cluster-Traefik install does not use the upstream chart, dump the
config from a running pod:

```sh
TRAEFIK_POD=$(kubectl get pod -n <ns> -l app.kubernetes.io/name=traefik \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n <ns> "$TRAEFIK_POD" -- \
  /bin/sh -c 'cat /etc/traefik/traefik.yaml 2>/dev/null \
              || cat /config/traefik.yaml 2>/dev/null \
              || ps -ef | grep traefik'
```

Look for the same `entryPoints.<name>.forwardedHeaders.trustedIPs` list.

### 4. Confirm with a live probe

From outside the LB CIDR, send a request with a forged
`X-Forwarded-Proto: http` to a route that runs through forwardAuth. Then
inspect the request as oauth2-proxy received it:

```sh
kubectl logs -n <gardenio-ns> -l app.kubernetes.io/component=forwardauth \
  --tail=200 -f
```

If `trustedIPs` is correctly scoped, oauth2-proxy should see
`X-Forwarded-Proto: https` (Traefik's own observation), not the forged value.
If it sees `http`, the spoofed header was passed through — `trustedIPs` is
too wide or unset, and this is a real finding.

## What to do if it is wrong

1. **Stop**. Do not change `trustForwardHeader: false` in this chart as a
   knee-jerk fix — that has its own blast radius (oauth2-proxy under
   `--reverse-proxy=true` may attribute the wrong source IP, redirect to the
   wrong scheme, or set cookies under the wrong host) and the audit
   explicitly recommends testing in non-prod first.
2. **Fix the cluster-Traefik install.** Set
   `entryPoints.websecure.forwardedHeaders.trustedIPs` to the LB CIDR.
   This is a static-config change → Traefik restart.
3. **Re-run the live probe** above to confirm the forged header no longer
   reaches oauth2-proxy.
4. **Document the chosen CIDRs** in the cluster-Traefik release values so
   the next operator does not roll them back.

## Hardening follow-up

The audit (F9) suggests a defence-in-depth move: flip
`trustForwardHeader: false` in the gardenio chart and rely entirely on
Traefik's own observation of `X-Forwarded-*`. That is intentionally **out of
scope** for ka-0000000601 — it requires non-prod validation that
oauth2-proxy still produces correct redirect URLs, cookie domains, and
`X-Forwarded-For` attribution under the new mode. File a separate bead
before doing it. Until then, this chart's safety boundary is the
cluster-Traefik `trustedIPs` list, and operators MUST verify it per the
steps above whenever the cluster-Traefik install changes.
