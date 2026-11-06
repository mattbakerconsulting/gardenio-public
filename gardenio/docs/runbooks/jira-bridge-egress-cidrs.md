# jira-bridge egress CIDRs — refresh & fallback runbook

**Bead:** ka-0000000429 (F1b)
**Audit:** `docs/audits/networkpolicies-static.md` §3 (not in this repo — the audits tree was excluded from the 2026-09-16 export)
**Helm value:** `jiraBridge.egress.atlassianCIDRs`
**Toggle:** `jiraBridge.egress.tightenSaaS` (default `true`)

## What this is

Before F1b, the `-jira-bridge-egress` NetworkPolicy allowed HTTPS to
`0.0.0.0/0` minus RFC1918 + IMDS + CGNAT — i.e. the whole public internet on
port 443. F1b tightens that to a hand-maintained CIDR allow-list of the
Atlassian Cloud IP ranges so a compromised jira-bridge cannot pivot to
arbitrary external hosts.

## CIDR source

- **Canonical machine-readable:** <https://ip-ranges.atlassian.com/> (JSON)
- **Human-readable:** <https://support.atlassian.com/organization-administration/docs/ip-addresses-and-domains-for-atlassian-cloud-products/>

The default list in `values.yaml` was sourced **2026-04-27** by deduplicating
and sorting all `cidr` entries from the JSON.

## Refresh cadence

- **Quarterly:** re-pull and diff. Atlassian changes CIDRs occasionally.
- **On failure:** if the bridge starts logging `i/o timeout` or
  `connection refused` to `*.atlassian.com` / `*.atlassian.net`, suspect a
  CIDR rotation. Refresh first, then fall back if refresh doesn't resolve it.

## Refresh procedure

```sh
# 1. Pull the canonical list
curl -s https://ip-ranges.atlassian.com/ | jq -r '.items[].cidr' \
  | sort -V | uniq > /tmp/atlassian-cidrs.txt

# 2. Diff against the current default in values.yaml
diff <(yq '.jiraBridge.egress.atlassianCIDRs[]' gardenio/helm/gardenio/values.yaml | sort -V) \
     /tmp/atlassian-cidrs.txt

# 3. If diff is non-empty, edit values.yaml — replace the atlassianCIDRs list
#    with the new sorted output. Update the comment header date.

# 4. Lint + render
helm lint gardenio/helm/gardenio/
helm template test gardenio/helm/gardenio/ \
  --set networkPolicies.enabled=true \
  --set jiraBridge.enabled=true \
  --set jiraBridge.jira.baseURL=https://x.atlassian.net \
  --set jiraBridge.jira.email=a@b.c \
  --set jiraBridge.jira.secretName=s \
  | grep -A2 'name: test-jira-bridge-egress'

# 5. Commit on a feat/<bead>-jira-cidrs-refresh-YYYY-MM-DD branch
#    with message: chore(helm/netpol): refresh jira-bridge egress CIDRs
```

## Fallback procedure (CIDRs broken in prod)

If a refresh isn't possible immediately (e.g. weekend, on-call wants to
unblock first), flip the toggle:

```sh
# Via acme-helm-chart values overlay or live helm upgrade:
--set jiraBridge.egress.tightenSaaS=false
```

This restores the prior wide-open egress (0.0.0.0/0 minus RFC1918+IMDS+CGNAT)
on port 443. The bridge can reach Jira Cloud again.

After the bridge is healthy again, **schedule a follow-up** to refresh CIDRs
and flip the toggle back to `true`. Leaving it `false` reverts the security
benefit of F1b.

## Rolling the bridge after a values change

Helm upgrade alone replaces the NetworkPolicy resource; the bridge pod itself
does not need a restart for the new egress rules to take effect (netpol is
enforced by the CNI on each connection). However, if you flipped the toggle
to recover from a connection failure, restart the pod to clear any cached
backoff state:

```sh
kubectl -n gardenio rollout restart deploy/<release>-jira-bridge
```

## Why hand-maintained vs FQDN

NetworkPolicy v1 cannot match by FQDN. Cilium's
`CiliumNetworkPolicy` CRD supports `toFQDNs:` and would replace this static
list with `toFQDNs: [{matchPattern: "*.atlassian.com"}, ...]`. Adopting that
approach is filed as a follow-up (audit §3, option 2). Until then, the
hand-maintained list is the simplest mechanism that meaningfully shrinks
blast radius without taking on a new dependency.

## Scope

This runbook covers **jira-bridge only**. slack-bridge has no egress policy
at all (F1, separate bead). gitlab-bridge uses the same wide-open shape and
is a candidate for the same treatment in a follow-up bead.
