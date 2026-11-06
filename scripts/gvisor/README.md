# Gardenio in a Box — gVisor RuntimeClass-only mode (Phase 0.3)

Single-node gVisor (runsc) sandbox runtime for the in-box build (epic
[ka-0000007690], task ka-0000002716). Sources:
`gardenio/docs/research/inbox-ec2-sizing-bootstrap.md` §4 and
`gardenio/docs/research/inbox-k8s-distro-choice.md` §4.

## What "RuntimeClass-only mode" means

A pod runs under gVisor **purely** by carrying `runtimeClassName: runsc`. The
controller's only routing is `pod.spec.runtimeClassName` from the agent's
`runtime_class` metadata — there is **no**:

- Karpenter node pool (`gvisor.nodePool.create=false`),
- dedicated-pool node selector / taint (`runtimeClass.scheduling: null`),
- freeze/thaw checkpoint lifecycle (`freezeThaw.enabled=false`),
- PSA-privileged namespace + probe DaemonSet (`namespace.name=""`,
  `probe.create=false`).

Two halves, deployed together by `scripts/bootstrap-ec2.sh --gvisor`:

| Half | Artifact | Does |
|---|---|---|
| **Host** | `scripts/gvisor/bootstrap-gvisor.sh` | Installs runsc + shim (pinned, checksum-verified), registers the runsc handler in containerd via the k3s config **template**, drops a PodSecurity `AdmissionConfiguration` exempting `runtimeClass=runsc`, restarts k3s, labels the node. |
| **Chart** | `deploy-values/gardenio-gvisor-inbox.yaml` | Renders the `runsc` RuntimeClass with single-node `scheduling: null`; everything else off. |

## The two correctness traps (research §4) — and how this handles them

### 1. containerd config schema/filename depends on the containerd version

Current k3s ships **containerd 2.x**, which renders `config-v3.toml.tmpl` with
the `[plugins.'io.containerd.cri.v1.runtime'...]` schema. Older k3s (containerd
1.7) uses `config.toml.tmpl` with `io.containerd.grpc.v1.cri`. **Pairing the v3
schema with the v2 filename silently registers nothing.**

`bootstrap-gvisor.sh` detects the containerd major version (`k3s ctr version`)
and writes the matching template+schema. It extends the k3s base template with
`{{ template "base" . }}` (never `cat`s the rendered `config.toml`, which k3s
warns against and which has a first-boot ordering bug). The rendered
`config.toml` is overwritten on every k3s restart, so the template **is** the
durable injection point — exactly the lesson from the EKS nodeadm path.

### 2. Single-node scheduling — drop the dedicated-pool selector

The chart's default RuntimeClass carries `scheduling.nodeSelector {gvisor: enabled}`
+ a `NoSchedule` toleration (it assumes a dedicated pool). On one node that
would pin runsc pods onto a node that may not carry the label.

The overlay sets `runtimeClass.scheduling: null`. **It must be `null`, not
`{}`** — Helm *deep-merges* values, so `{}` merges with (does not replace) the
populated map and the `gvisor=enabled` selector survives. `null` clears it; the
template's `{{- with $rc.scheduling }}` then emits no scheduling block.
(`bootstrap-gvisor.sh` also labels the node, so either path works, but `null`
is what makes a single unlabelled node schedule runsc pods.)

We do **not** hand-apply a second `runsc` RuntimeClass — the chart owns it
(`runtimeClass.create=true`, `helm.sh/resource-policy: keep`); a duplicate
collides.

## Platform + release pin

- `--platform systrap` (default) is correct for EC2 **Nitro guest** instances
  (no nested virt, no `/dev/kvm`). `--platform kvm` only works on `.metal`.
- The runsc **release is pinned** (default `20260525.0`, matching the
  audited/deployed runsc on the gvisor-poc pool) and the binaries are
  **sha512-verified** before install — never silently `latest`.

## PodSecurity AdmissionConfiguration (in-box advantage)

On EKS the managed control plane hides the apiserver PodSecurity
`AdmissionConfiguration`, which is why the prod chart needs a dedicated
PSA-`privileged` namespace. **In-box we own the apiserver**, so
`bootstrap-gvisor.sh` drops an `AdmissionConfiguration` exempting
`runtimeClasses: [runsc]` and wires it via a k3s apiserver-arg **drop-in**
(`/etc/rancher/k3s/config.yaml.d/10-gvisor-psa.yaml`) — keeping P0.2's main
`config.yaml` (node-ip/tls-san) intact.

> ⚠ The exemption is **broad** (any runsc pod skips PodSecurity). The agent
> namespace must stay locked down — that hardening is Phase 3 (ka-0000005904).

## Usage

```sh
# As part of the full bootstrap (installs host side + layers the chart overlay):
sudo scripts/bootstrap-ec2.sh --scenario A --gvisor

# Or just the host side on an existing k3s box:
sudo scripts/gvisor/bootstrap-gvisor.sh --runsc-release 20260525.0 --platform systrap
```

A pod with `runtimeClassName: runsc` then runs sandboxed on the single node.

[ka-0000007690]: https://beads-viewer.e2e.example.com/bead/ka-0000007690

---

# Freeze→thaw assertion ladder — `freeze-thaw-rungs.sh` (Rig R1)

`freeze-thaw-rungs.sh` codifies the gVisor freeze→thaw assertion ladder as **7
named rungs** with **stable `FT_RUNG_*` markers** (bead [ka-0000005639], epic
[ka-0000000867], canary [ka-0000001355]). It is a **pure, sourceable shell
classifier** — it only PARSES the `FT_*` sentinel stream the freeze + thaw node
scripts already emit (`gardenio/controller/internal/podmanager/freezethaw_node.go`)
plus three harness-observed post-thaw signals; it never drives `runsc`, touches
the controller, the `freezeThaw` flag, or prod. Sourcing it has zero side
effects, so the canary HARD HOLD (one canary, zero orphans, prod untouched) is
respected by construction.

## Usage

```sh
source scripts/gvisor/freeze-thaw-rungs.sh
ft_classify_rungs <sentinel-file>          # or:  ... | ft_classify_rungs -
# prints the FT_RUNG_* marker block; returns 0 iff no rung FAILED.
```

The canary driver (R4) concatenates the freeze node stdout, the thaw node
stdout, and its own post-thaw observations into ONE stream and feeds it here.

## The 7 rungs and their markers

For each rung the classifier emits `FT_RUNG_<name>=ok|fail|skip` +
`FT_RUNG_<name>_REASON=…`, and names the **first failing rung** as the cycle's
classified mode via `FT_FIRST_FAILED_RUNG=<name>|none` + `FT_FIRST_FAILED_REASON`.
The ladder is walked 1→7; the first rung that FAILS is the mode and every rung
below it is `skip` (never reached).

| # | rung | OK evidence | sentinel source | absent |
|---|------|-------------|-----------------|--------|
| 1 | `snapshot_persisted` | `FT_CHECKPOINT_RC=0` & `FT_IMAGE_PRESENT=yes` & `FT_SANDBOX_STATE=running` & `FT_CONTAINER_COUNT>=1` & `FT_BUNDLE_OK=yes` & `FT_IMAGE_OK=yes` | existing | **fail** |
| 2 | `restore_rc0` | `FT_RESTORED_PID` set & `FT_SUB_RC=0` & `FT_RESTORED_NETNS==FT_B_NETNS` | existing | **fail** |
| 3 | `netns_routable` | `FT_NETNS_IFACES>=1` & `FT_NETNS_DEFROUTE=yes` (Fix A) | existing | skip |
| 4 | `cni_confirmed` | `FT_CNI_CONFIRMED=yes` — **non-gating localizer** (ok/skip, never fail) | existing | skip |
| 5 | `rehydrate_acks` | `FT_REHYDRATE_GB_ACK=ok` & `FT_REHYDRATE_COOP_ACK=ok` | **new** | both absent → skip; one → fail |
| 6 | `work_intact` | `FT_WORK_TOKEN=intact` | **new** | skip |
| 7 | `ram_intact` | `FT_RAM_RESUMED=yes` | **new** | skip |

The `absent` column is the verdict when a rung's sentinel(s) are not present in
the stream. Rungs 3/4/6/7 each read signals that may legitimately be unmeasured
(legacy node script, or a harness that did not probe that signal), so absence is
`skip`, not a failure. Rung 5 is **fail-closed per consumer**, mirroring the
controller's ack gate: only when BOTH ack sentinels are absent (rehydration not
exercised at all) does it `skip`; emitting exactly one is a partial-measurement
`fail`. **Harness contract: emit both `FT_REHYDRATE_*_ACK` sentinels together, or
neither — never exactly one.** Rung-5/6/7 values are matched case-sensitively
against the exact lowercase positive token (`ok` / `intact` / `yes`); any other
value is treated as a failure.

## Two correctness subtleties (verified against source)

1. **The `FT_ROOT_RC` overload.** On a lo-only netns the node script emits
   `FT_ROOT_RC=0` (barrier succeeded), prints `FT_RESTORED_PID`, then RE-emits
   `FT_ROOT_RC=128` on the fail-close — so a last-wins parse sees `128` on a cycle
   whose restore actually succeeded. Rung 2 therefore keys on **`FT_RESTORED_PID`
   presence**, not `FT_ROOT_RC`, so a lo-only fault classifies as `netns_routable`
   (rung 3), not `restore_rc0` (rung 2). A genuine barrier failure has NO
   `FT_RESTORED_PID` (the script exits before `[6]`).
2. **`cni_confirmed` is non-gating.** `FT_CNI_CONFIRMED` is best-effort and never
   asserted by the controller — a thaw can be genuinely routable with
   `FT_CNI_CONFIRMED=no` (Fix-C self-IPAM populated the netns). Since the ladder
   orders `netns_routable` BEFORE `cni_confirmed`, rung 4 is only reached when the
   netns is already routable, so `cni=no` there is a healthy outcome. Rung 4 is
   ok/skip only and never the classified mode; its localizers are also embedded in
   rung 3's reason.

## New sentinels the canary harness (R4) must emit

Rungs 5–7 have **no existing `FT_` sentinel** — `freeze-thaw-rungs.sh` DEFINES
the contract; the unified canary harness must emit, from its post-thaw
observations:

- `FT_REHYDRATE_GB_ACK=ok|fail` / `FT_REHYDRATE_COOP_ACK=ok|fail` — one per
  required `thawepoch` consumer (`gb`, `coop`), epoch-scoped, from the
  per-consumer ack verdict (`freezethaw_rehydrate.go` `waitForAcks`).
- `FT_WORK_TOKEN=intact|lost` — the `/work/state.bin` token read post-thaw vs the
  pre-freeze sentinel.
- `FT_RAM_RESUMED=yes|no` — the in-RAM monotonic counter post-thaw is strictly
  greater than its freeze-time value (true memory continuity, not a cold restart).

## Consumers

- **Rig R2** ([ka-0000006948]) captures the full `FT_RUNG_*` set into a `gb
  bundle` on the first failing rung.
- **Rig R3** ([ka-0000004124], `file-next-layer-bug.sh`) files a deduped
  next-layer bug keyed on `FT_FIRST_FAILED_RUNG` + the reason signature.

[ka-0000005639]: https://beads-viewer.e2e.example.com/bead/ka-0000005639
[ka-0000000867]: https://beads-viewer.e2e.example.com/bead/ka-0000000867
[ka-0000001355]: https://beads-viewer.e2e.example.com/bead/ka-0000001355
[ka-0000006948]: https://beads-viewer.e2e.example.com/bead/ka-0000006948
[ka-0000004124]: https://beads-viewer.e2e.example.com/bead/ka-0000004124
