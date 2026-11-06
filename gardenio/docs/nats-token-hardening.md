# Agent NATS Token: tmpfs file mount (W5)

Source: ka-0000006585 (rolled up under ka-0000002746). Companion doc to
[`coop-credentials-hardening.md`](coop-credentials-hardening.md) — both
remove a credential from the agent pod's env-var surface.

## Invariant

The NATS agent token is **not** present in the agent pod's environment.
It lives only as a tmpfs-backed file projected from a K8s `Secret`.
Specifically:

- The K8s `Secret` `<release>-nats-agent-creds` (key `token`) is mounted
  into the agent container at `/etc/nats-token/token` with mode `0400`
  via a single-item `Secret` volume. K8s `Secret` volumes are
  tmpfs-backed by default — the token bytes never touch a node disk.
- The agent container has `COOP_NATS_TOKEN_FILE=/etc/nats-token/token`
  set in env. The previous `COOP_NATS_TOKEN` env-var is no longer
  injected by the controller into agent pods.
- Consumers (coop, `gb hook-relay`) read the token from the file at
  startup. Empty/missing file falls back to `COOP_NATS_TOKEN` env for
  `--local` and out-of-cluster operator workflows.

## Verifying

```sh
# 1. /proc/self/environ in the running agent must NOT contain COOP_NATS_TOKEN=
kubectl exec -n <ns> <agent-pod> -- tr '\0' '\n' < /proc/self/environ | \
  grep -E '^COOP_NATS_TOKEN(=|_FILE=)'
#   COOP_NATS_TOKEN_FILE=/etc/nats-token/token        ← OK (path only)
#   COOP_NATS_TOKEN=…                                  ← regression, must not appear

# 2. kubectl describe must not print the token. Only the file mount.
kubectl describe pod -n <ns> <agent-pod> | grep -A2 -iE 'nats|token'
#   COOP_NATS_TOKEN_FILE:           /etc/nats-token/token
#   nats-token from <release>-nats-agent-creds-…

# 3. File mode and tmpfs backing
kubectl exec -n <ns> <agent-pod> -- stat -c '%a %n' /etc/nats-token/token
#   400 /etc/nats-token/token
kubectl exec -n <ns> <agent-pod> -- mount | grep /etc/nats-token
#   tmpfs on /etc/nats-token type tmpfs (ro,relatime,...)

# 4. The agent connects to NATS successfully (logs)
kubectl logs -n <ns> <agent-pod> | grep -iE 'nats-(publisher|relay|forward): (started|connect)'
```

## Where to look

| Concern | File |
|---|---|
| Volume + mount (Go) | `gardenio/controller/internal/podmanager/volumes.go` (`buildVolumes`, `buildVolumeMounts`) |
| Constants (`MountNatsToken`, `NatsTokenFileMode`) | `gardenio/controller/internal/podmanager/manager.go` |
| Spec field `NatsTokenSecret` | `gardenio/controller/internal/podmanager/manager.go` (`AgentPodSpec`) |
| Controller wiring (`COOP_NATS_TOKEN_FILE`) | `gardenio/controller/cmd/controller/podspec.go` (`applyCommonConfig`) |
| Helm Secret/ExternalSecret | `gardenio/helm/gardenio/templates/nats/agent-creds-secret.yaml` |
| Coop reader (`nats_token_resolved`) | `coop/crates/cli/src/config.rs` |
| `gb hook-relay` reader (`readNATSToken`) | `gardenio/controller/cmd/gb/hook_relay.go` |

## Out of scope (separate tasks)

- The Slack-bridge pod still gets `COOP_NATS_TOKEN` via env-var
  (`templates/slack-bridge/deployment.yaml`). That is a different pod
  with a different threat model (no agent code runs inside it).
- The controller pod reads `NATS_TOKEN` from its own env. Same.
- `coop --local` (out-of-cluster operator mode) still uses the
  `COOP_NATS_TOKEN` env-var path — agents started by `gb agent start
  --local` derive the token from the operator's beads creds at runtime
  and there is no projected file to read.
