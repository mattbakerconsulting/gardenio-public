# Agent-image prewarm — eliminate cold-pull spawn latency

Task ka-0000000861 · epic ka-0000007417 (Phase 4 gVisor productionization) · operator: Matt Baker.

## What this is

A per-node-pool **DaemonSet** that pulls the agent image onto every node of the
agent pool(s) **before** an agent is scheduled there, so a fresh agent spawn does
not pay the first-image-pull cost on a cold node.

Karpenter scales agent pools up and down, so "cold node with no agent image
cached" is the common case after a scale-up, not the exception. Measured
spawn-to-ready:

| Runtime | Cold spawn → ready | Dominant cost |
|---|---|---|
| runc agent | ~135 s | mixed |
| **runsc (gVisor) agent** | **~4–5 min** | first agent-image pull into the new node |

Prewarming collapses the pull cost: once the DaemonSet pod has pulled the image,
the layers live in the node's containerd content store, and a subsequently-spawned
agent pod pulls from that local cache instead of the registry.

## How it works

`templates/agent-image-prewarm.yaml` renders one DaemonSet per enabled pool. Each
pod:

- runs the **same** agent image as spawned agents (`agents.agentImage`, resolved
  through `gardenio.imageRef` so a release bump re-pulls the new layers
  automatically — it never pins a stale tag);
- runs `sleep infinity` as a no-op — its only job is to make the kubelet pull and
  cache the image;
- carries **no credentials** and reaches **no services**
  (`automountServiceAccountToken: false`);
- is pinned to the pool's nodes via `nodeSelector` + `tolerations` (DaemonSet ⇒
  exactly one pod per matching node);
- requests tiny resources (10m CPU / 16Mi mem) so it never displaces real agent
  capacity.

### Why the gVisor prewarm pod runs under runc, not runsc

The prewarm pod carries **no** `runtimeClassName`, so it runs under the node's
default runtime (runc) even on gVisor nodes. That is intentional: the image
layers are stored in the node's shared containerd content store regardless of
which runtime ultimately executes them, so a runc-launched prewarm pull warms the
cache for a later runsc agent just as well — without needing the privileged
runsc plumbing.

### Namespace / PSA

Both DaemonSets render into `.Release.Namespace` (the restricted-PSA main
namespace), **not** the gVisor privileged namespace. The namespace does not
constrain which node a pod schedules onto — only `nodeSelector` + `tolerations`
do. The prewarm pod is restricted-PSA compliant (agent image UID 1000,
`runAsNonRoot`, drop ALL caps, no privilege escalation, `RuntimeDefault`
seccomp, read-only rootfs). `.Values.imagePullSecrets` (the same secret agent
pods use) is attached so the private-registry pull succeeds.

## Values

```yaml
agents:
  prewarmImage:
    enabled: false          # DEFAULT OFF — master switch
    pools:
      agent:                # standard runc agent pool
        enabled: true
        nodeSelector: { gardenio.io/pool: "agent" }
        tolerations:
          - { key: gardenio.agent, operator: Exists, effect: NoSchedule }
      gvisor:               # gVisor (runsc) pool — worst cold-pull penalty
        enabled: true
        nodeSelector: { gvisor: "enabled" }
        tolerations:
          - { key: gvisor, operator: Equal, value: enabled, effect: NoSchedule }
    resources:
      requests: { cpu: 10m, memory: 16Mi }
      limits:   { cpu: 50m, memory: 64Mi }
```

The pool node labels/taints come from
`terraform/modules/karpenter/nodepool.tf` (standard pool: label
`gardenio.io/pool=agent`, taint `gardenio.agent=true:NoSchedule`) and the
`gvisor-probe` DaemonSet (gVisor pool: label/taint `gvisor=enabled`).

## Enabling

```bash
helm upgrade gardenio ... --set agents.prewarmImage.enabled=true
```

Enable a single pool by disabling the other, e.g. gVisor-only:

```bash
--set agents.prewarmImage.enabled=true \
--set agents.prewarmImage.pools.agent.enabled=false
```

## Verifying

1. Image layers present on a fresh node **before** any agent runs there:

   ```bash
   # On a freshly-scaled node (exec the prewarm pod or an SSH/debug pod):
   crictl images | grep gardenio/agent
   # → the agent image is listed, pulled by the prewarm DaemonSet, not an agent pod.
   ```

2. Measure cold runsc spawn-to-ready before/after. Scale the gVisor pool to zero,
   enable prewarm, trigger a node scale-up, wait for the prewarm pod to go Ready
   (image cached), then spawn a runsc agent onto that node and time
   spawn → ready. It should drop materially from the ~4–5 min baseline toward the
   cached-layer case.

## Rollback

```bash
helm upgrade gardenio ... --set agents.prewarmImage.enabled=false
```

The DaemonSet(s) are removed. Already-warmed nodes keep their cached layers until
normal kubelet image GC; new nodes simply fall back to cold-pull-on-first-agent —
i.e. exactly today's behaviour.

## Blast radius

Zero by default (`enabled=false`). When enabled, the only added workload is one
tiny `sleep infinity` pod per agent-pool node, with no credentials and no API
access. It cannot affect agent scheduling beyond the negligible 10m CPU / 16Mi
request it reserves.
