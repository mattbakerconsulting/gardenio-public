# k3s API audit-log shipper

Ships the kube-apiserver audit log (`/var/log/k3s-audit/audit.log` on each
control-plane node) to the in-chart VictoriaLogs, redacted, in its **own tenant**,
for 30 days (`vlsingle.retentionPeriod`). Task gd-ZrPUxj8wyU; decision
gd-bNVBdrmIo5. Behaviour, redaction rules and sizing:
[`gardenio/docs/k3s-cluster.md`](../../docs/k3s-cluster.md#shipping-the-audit-log-to-victorialogs).
Source: `controller/cmd/k3s-audit-shipper`, `controller/internal/k3sauditshipper`.

## Why this is NOT in the gardenio Helm chart

The shipper needs a hostPath mount, which Pod Security `baseline` (the gardenio
namespace) forbids, so it runs in `gardenio-audit` labelled PSA `privileged`. Anyone
who can create a pod there can mount the host: root on the control plane. If it
were in the release, the autonomous release deployer would need to create that
namespace and patch the DaemonSet, i.e. hold that power on every release. The
deployer has neither right today and must not get them, so the namespace and
DaemonSet are plain manifests an operator applies, the house style of
`../gardenio-ziti/` and `../beads/`. Nothing here is reachable from the chart
(`gardenio/helm/gardenio/tests/k3s_audit_shipper_test.go` fails if it reappears).

| File | What it is |
|---|---|
| `namespace.yaml` | `gardenio-audit`, PSA `privileged` |
| `shipper.yaml` | ServiceAccount (no token) + DaemonSet pinned to the control plane |
| `networkpolicy.yaml` | shipper egress (DNS + vlsingle :9428). Only for the day NetworkPolicies are enabled; see "Residual risk" |

## Before applying

1. `vlsingle.enabled=true` in the release (it is off in `deploy-values/gardenio-k3s.yaml`).
2. The audit policy has rolled to the servers. A server without `/var/log/k3s-audit`
   leaves its pod in `ContainerCreating`/`FailedMount` on purpose.
3. Edit the `image:` tag in `shipper.yaml` to the released calver.
4. Re-apply `../gardenio-ziti/ecr-token-refresh.yaml` after the namespace exists, so
   the `ecr` pull secret is kept fresh in `gardenio-audit` (the refresher skips a
   missing namespace).

```sh
export KUBECONFIG=~/.kube/gardenio.yaml
cd gardenio/helm/k3s-audit-shipper
kubectl apply -f namespace.yaml
kubectl apply -f ../gardenio-ziti/ecr-token-refresh.yaml
kubectl apply -f shipper.yaml
kubectl -n gardenio-audit rollout status ds/k3s-audit-shipper
```

Upgrade = edit the tag, `kubectl apply -f shipper.yaml`. Disable =
`kubectl delete -f shipper.yaml -f namespace.yaml` (the node-local audit log and
the checkpoint stay on the nodes).

## Reading the audit log

Records are in VictoriaLogs tenant `AccountID: 9`. A default-tenant query (what
`gb logs`, the Grafana VictoriaLogs datasource and vmalert-logs use, and which
`internal/vlogs` now pins with `AccountID: 0`) never sees them. To read:

```sh
kubectl -n gardenio port-forward svc/gardenio-victoria-logs 9428
curl -s localhost:9428/select/logsql/query -H 'AccountID: 9' -H 'ProjectID: 0' \
  --data-urlencode 'query=source:k3s-audit user.username:"system:serviceaccount:gardenio:gardenio-infra-admin"' \
  --data-urlencode 'limit=50'
```

## Residual risk (record this, do not rely on the tenant as a control)

VictoriaLogs has no authentication, and the tenant is chosen by a request header
the client supplies. A pod that can reach `gardenio-victoria-logs:9428` can set
`AccountID: 9` itself and **read or forge audit records**. The tenant keeps audit
out of every plain/default-tenant read (and the shipper refuses tenant 0), which is
what stops accidental exposure, but it is not access control. Agents have egress to
vlsingle :9428 and NetworkPolicies are off on this cluster, so today this is open.

Gate it properly when NetworkPolicies are turned on: apply `networkpolicy.yaml`
**and** add an ingress peer for the shipper (`namespaceSelector`
`kubernetes.io/metadata.name: gardenio-audit` + `podSelector`
`app.kubernetes.io/component: k3s-audit-shipper`) to the chart's
`<release>-vlsingle-ingress` policy for the write path, while the agent egress rule
to :9428 is narrowed so agents cannot reach `/insert`. A VictoriaLogs-side fix
(an auth proxy that pins `AccountID` per client, `vmauth`) is the durable answer.
