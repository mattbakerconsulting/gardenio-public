# Air-gap image path: `ctr images import` vs the in-box zot registry (eval)

**Bead**: ka-0000003734 (Phase 4 air-gap follow-up)
**Parent**: ka-0000007881 (Phase 4: air-gap image distribution — **closed**, all 3 children merged)
**Mega epic**: ka-0000001295 (Gardenio in a Box: single-EC2, in-box kube, zero external cloud deps)
**Decision context**: ka-0000003249 (Phase 4 complete)
**Predecessor research**: `docs/research/inbox-image-distribution.md` (ka-0000005069, open-question 1) — not in this repo, the research tree was excluded from the 2026-09-16 export
**Unblocking change**: ka-0000007177 (P4.3 — agent pull policy configurable, `global.airgap`)

This doc answers exactly **open-question 1** of the image-distribution research: now
that the agent pull policy is configurable (P4.3) and first-party images are
digest-pinned (ka-0000006693), is a registry-free `ctr images import` path —
loading the image bundle straight into containerd at install and never running a
runtime registry — viable, and should we adopt it, keep zot, or document it as an
option? It is an **evaluation**, not a code change to the pull path.

---

## TL;DR — recommendation

**Keep zot as the default air-gap path. Document `ctr images import` as a
supported, lower-footprint *option* for the strict-offline / minimal-RAM box —
but do not make it the default, and do not rip out zot.**

The single fact that made zot *mandatory* in the original research — the agent
container's hardcoded `PullAlways` — **is gone** (P4.3, `manager.go:558`). With
`agentPullPolicy: IfNotPresent` + digest-pinned first-party images +
`latestMode` hard-disabled under `global.airgap`, a pure containerd-import path
is now **technically viable**: every gardenio image ref resolves against the local
containerd content store with no registry round-trip. The bootstrap *already*
ships the mechanism — `import_images()` runs `k3s ctr images import` at stage 4,
before the zot `airgap_setup()` at stage 4b.

But "viable" is not "better by default". zot earns its keep on the two operations
that a pure-import box does badly: **pod reschedule after a containerd GC/prune**
(import has no server to re-pull from — the image is just *gone*), and **upgrade
hygiene** (zot does online dedup + GC; a raw content store accumulates every
calver's blobs until DiskPressure). zot is already built, tested, wired into
bootstrap, and reboot-safe. The marginal win of deleting it (≈one host systemd
unit, ≈80 MB RSS, one pinned binary) does not justify giving up the reschedule
and GC safety it provides on a single box whose only disk is the one we're
trying not to fill.

So: **zot stays the recommended default; ctr-import becomes a documented
`--airgap-import-only` option** for operators who want the absolute-minimum
footprint and accept the reschedule/GC caveats below.

---

## 1. How the current zot + containerd-mirror hot path works

The air-gap overlay (`global.airgap: true` in
[`values-inbox.yaml`](../helm/gardenio/values-inbox.yaml)) + `bootstrap-ec2.sh
--airgap` stands up three host-level pieces, all **before** the chart deploys:

1. **In-box zot registry** (`scripts/inbox/install-registry.sh`) — a host
   *systemd* unit (`zot.service`), not a k8s pod. Listens on `127.0.0.1:5000`,
   plain HTTP on loopback, data on a persistent host path (`/var/lib/zot`).
   Ordered `Before=k3s.service` with `Restart=always` so the registry is live
   before the first kubelet pull and survives reboots. `storage.dedupe=true` +
   online GC (`gcInterval=24h`).

2. **Seed from the crane bundle** (`scripts/inbox/seed-bundle.sh`) — decompresses
   the `gardenio-bundle-${CALVER}.oci.tar.zst` produced by `.rwx/bundle.yml`
   (one OCI layout per image + `manifest.tsv`), verifies its `.sha256`, and
   `crane push`es each image into zot under its **identical** upstream repo path.

3. **Containerd mirror** (`scripts/inbox/configure-mirror.sh`) — writes
   `/etc/rancher/k3s/registries.yaml` with a `mirrors:` entry for every distinct
   upstream host (`registry.gitlab.com`, `docker.io`, `ghcr.io`,
   `public.ecr.aws`, `quay.io`) → `http://127.0.0.1:5000`, capabilities
   `["pull","resolve"]`, `insecure_skip_verify`. `--airgap-strict` adds
   `--disable-default-registry-endpoint` so a missing blob fails fast instead of
   leaking an upstream pull.

**Why a node-level mirror and not `global.imageRegistry`:** the three hottest
runtime refs bypass Helm entirely — the agent image is `COOP_IMAGE` on the
controller env (`config.go:719`), and init-clone (`alpine/git`) + DinD
(`docker:dind`) are Go constants (`manager.go:235,250`). The containerd mirror
rewrites *every* pull regardless of who set the ref; pod manifests stay
byte-identical to prod.

**Is the registry actually hot-path today?** It *was* — the original research
flagged the agent container's hardcoded `PullAlways` (`manager.go:982`) as *the*
deciding factor forcing an always-on registry: every on-demand agent spawn
forced a registry round-trip. **P4.3 changed this.** `manager.go:558`
(`agentPullPolicy()`) now honours an override, and the overlay sets
`agents.agentPullPolicy: IfNotPresent` (also forced by `global.airgap`). So under
the air-gap overlay the registry is **no longer hit on every agent spawn** — a
seeded image is reused from the containerd store. The registry is still consulted
for the *first* pull of each image and for any image not yet in the containerd
store, but the per-spawn hot-path round-trip is already eliminated. This is the
crux of why import-only is now viable at all.

---

## 2. The `ctr images import` alternative

The mechanism: instead of running a registry + mirror, load the image bundle
**directly into containerd's content store** at install, and set pull policies so
the kubelet never tries to fetch what is already present.

Two delivery shapes, both already partially present in the repo:

- **k3s auto-load dir** *(preferred per [`inbox-ec2-sizing-bootstrap.md`](research/inbox-ec2-sizing-bootstrap.md) §3)* —
  drop `*.tar`/`*.tar.zst` image archives into
  `/var/lib/rancher/k3s/agent/images/` and k3s auto-imports them on start.
  Survives k3s restarts; no manual `ctr` invocation.
- **Explicit `k3s ctr images import <file>.tar`** — exactly what
  `bootstrap-ec2.sh` `import_images()` (stage 4) already does for any tarballs
  under `/opt/gardenio/images`. Today this is a *belt-and-suspenders* step that
  runs *before* the zot `airgap_setup()`; an import-only mode would make it the
  whole story.

Required config (all already shipped by P4.3 / the digest-pin follow-up):

| Knob | Value for import-only | Where | Status |
|---|---|---|---|
| agent pull policy | `IfNotPresent` (or `Never` for strict) | `agents.agentPullPolicy` → `AGENT_PULL_POLICY` → `manager.go:558` | ✅ shipped (P4.3) |
| first-party Deployments | `IfNotPresent` | `gardenio.imagePullPolicy` helper (`_helpers.tpl:60`) | ✅ default when `latestMode` off |
| `latestMode` | forced **off** | `global.airgap` → `_helpers.tpl:39` | ✅ shipped (P4.3) |
| first-party digests | pinned | `values-inbox.yaml` digest blocks | ✅ shipped (ka-0000006693) |
| DinD image | overridable | `agents.dindImage` → `DIND_IMAGE` → `dind.go` | ✅ shipped (P4.3) |
| no upstream fallback | drop the mirror; rely on store | (no `registries.yaml` needed) | n/a for import-only |

The bundle format question: the existing `.rwx/bundle.yml` emits a **crane
OCI-layout** bundle (per-image OCI layouts + `manifest.tsv`), tailored for
`crane push` into zot. `ctr images import` wants **docker/OCI image-archive
`.tar`** files (the `ctr export`/`docker save` shape). So import-only does **not**
get to reuse the existing bundle as-is — it needs either (a) a second bundle
artifact in the image-archive format, or (b) a tiny on-box `crane export` /
`skopeo copy oci:… docker-archive:…` conversion step from the existing OCI-layout
bundle. This is a real (if small) cost and is the main reason import-only is not
"free given what's already built".

**Net effect:** the registry becomes **install-only** (only needed, if at all, to
produce/convert the archive) and there is **no runtime server process** — the box
runs purely off the containerd content store.

---

## 3. Tradeoffs vs the zot + mirror approach

### 3.1 What import-only wins

- **No always-on server process.** No `zot.service`, no ≈80 MB resident
  registry, no pinned `zot` binary to track for CVEs, no
  `Before=k3s.service` ordering dependency. On a 2 vCPU / 4 GiB floor box
  (the documented minimum, [`inbox-ec2-sizing-bootstrap.md`](research/inbox-ec2-sizing-bootstrap.md) §3),
  shedding a host daemon is a real, if modest, RAM/attack-surface win.
- **Strictly fewer moving parts at runtime.** No `registries.yaml`, no
  loopback HTTP, no "is the mirror up?" failure mode. The image is either in the
  containerd store or it is not — one fewer indirection to debug.
- **True install-only registry.** Directly satisfies the task's stated goal:
  "make the registry install-only, not runtime."

### 3.2 What import-only loses — and why these matter on *this* box

- **Pod reschedule / eviction is the killer.** This is the decisive caveat.
  With zot + `IfNotPresent`, if a pod is rescheduled and its image was GC'd from
  the containerd store, the kubelet re-pulls from the loopback zot — **the image
  is still available on-box**. With import-only there is **no server to re-pull
  from**: once containerd GC reclaims a manually-imported image, it is *gone*
  until the next manual re-import. A rescheduled pod wedges
  `ImagePullBackOff` against a dead/absent endpoint. With
  `pullPolicy: Never` it fails even harder (`ErrImageNeverPull`) with no
  recovery path short of re-running `ctr import`.
- **Containerd GC vs registry GC.** k3s docs are explicit: *manually-imported
  images can be GC'd under DiskPressure* and must be **pinned** with
  `io.cri.containerd.pinned=pinned` / `io.cattle.k3s.pinned=pinned` labels to
  survive. So import-only quietly **adds** an ops burden (label every imported
  image, re-pin after every import) to *remove* a server. zot inverts this: it
  is itself the durable, GC-managed store, and the containerd store in front of
  it is free to evict because the loopback re-pull is cheap. zot also does
  **online layer dedup + GC** of stale calver blobs; a raw containerd store
  accumulates every release's ≈2.5–4 GB until DiskPressure bites — exactly the
  failure mode the single shared host disk is most sensitive to.
- **Upgrade flow is heavier.** zot upgrade = re-seed the new bundle into the
  *same* registry (crane is content-addressed; shared `agent-base` layers
  dedup; old blobs GC out). Import-only upgrade = `ctr import` the new archives
  **and** re-pin them **and** prune the old pinned images by hand (else they
  never GC) — a multi-step, error-prone host operation per upgrade, with no
  dedup safety net.
- **Bundle format mismatch / dual artifact.** As noted in §2, the existing
  `.rwx/bundle.yml` OCI-layout bundle is built for `crane push`, not
  `ctr import`. Import-only needs a second artifact or an on-box conversion —
  net-new pipeline or install work that partly offsets the "fewer parts" win.
- **`COOP_IMAGE` / Go-constant refs still need to be present, by digest.**
  Import-only relies entirely on the containerd store holding the *exact* refs
  (incl. the digest-pinned agent/controller and the Go-constant
  `alpine/git`/`docker:dind`). That is fine when freshly imported, but combined
  with GC it is the same reschedule trap: the moment any of those is reclaimed,
  the spawn path breaks with no recovery server.

### 3.3 What is a non-issue on a single box

- **Multi-node distribution — N/A.** The classic argument *for* a registry is
  "distribute to many nodes". gardenio-in-a-box is **single-EC2** by definition
  (mega epic ka-0000001295). There is exactly one containerd store, so the
  multi-node case that would force a registry simply does not exist here. This is
  the strongest point *in favour* of import-only — and is precisely why the
  decision is close rather than obvious.
- **Bundle size — equivalent.** Both paths ship the same ≈2.5–4 GB compressed
  image set; the bytes that land on disk are comparable. zot's `dedupe` only
  pulls ahead across *successive* calver bundles, not on a single install.
- **Digest safety — equivalent.** Both preserve content digests (`ctr import`
  and `crane push` are both content-addressed); `global.airgap` forcing
  `latestMode` off protects both.

### 3.4 Side-by-side

| Dimension | zot + mirror (current default) | `ctr images import` (import-only) |
|---|---|---|
| Runtime server process | host `zot.service` (≈80 MB) | **none** ✅ |
| Pod reschedule after GC | re-pull from loopback zot ✅ | **breaks** — image gone, no server ❌ |
| Image GC / disk hygiene | online dedup + GC ✅ | manual pin + manual prune ❌ |
| Upgrade flow | re-seed same registry (dedup) ✅ | re-import + re-pin + manual prune ❌ |
| Multi-node | supported (unused) | N/A (single box) — tie |
| Bundle size on disk | ≈2.5–4 GB | ≈2.5–4 GB — tie |
| Bundle artifact reuse | uses existing `.rwx/bundle.yml` ✅ | needs archive format / conversion ❌ |
| Moving parts at runtime | registry + mirror + store | store only ✅ |
| Reboot survival | systemd `Restart=always` ✅ | k3s auto-load dir ✅ (if used) |
| Already wired in bootstrap | `airgap_setup()` (4b) ✅ | `import_images()` (4) ✅ |
| Strict offline (no inbound :80 either) | works | works |

---

## 4. Recommendation

**Adopt: keep zot as the default; document `ctr images import` as a supported
option. Do not change the default pull path.**

Rationale, in priority order:

1. **The reschedule + GC trap outweighs the footprint win.** The one thing a
   single box must never do is wedge an un-recoverable `ImagePullBackOff` because
   containerd GC'd an image and there is no server to re-pull from. zot makes the
   containerd store a *cache* in front of a durable local registry; import-only
   makes the containerd store the *only* copy, GC-eligible, recoverable only by a
   manual host command. For an appliance meant to run unattended, that is the
   wrong trade. The footprint saved (one systemd unit, ≈80 MB, one binary) is
   small against it.
2. **zot is already built, tested, and shipped.** Phase 4 is closed; the three
   scripts + bootstrap wiring + the `airgap_overrides_test.go` coverage all
   exist. Deleting it is *negative* work with *negative* safety. There is no
   maintenance crisis pushing us off it.
3. **P4.3 already captured most of the prize.** The thing the task set out to
   improve — "remove the always-on hot-path dependency on the registry" — is
   *already largely done*: `agentPullPolicy: IfNotPresent` means the registry is
   no longer hit on every agent spawn. The registry is now consulted only on
   first-pull / cache-miss, i.e. it is already close to "install-time-ish" in
   practice. The remaining delta (deleting the server entirely) buys little and
   costs the reschedule safety net.
4. **import-only is the right tool for a narrow case, so expose it as one.** For
   an operator who genuinely wants the minimum-footprint, strict-offline box and
   accepts the "re-import on reschedule" caveat (e.g. a demo/appliance that is
   never rescheduled and is rebuilt rather than upgraded), import-only is
   legitimately leaner. The mechanism is *already in bootstrap*
   (`import_images()`). The right move is to make it a **documented, gated
   option** — not to inflict its caveats on every box by default.

### Suggested follow-up (small, non-blocking — not part of this eval)

If/when an operator wants the import-only option turned into a first-class path,
file a follow-up task for:

1. A `--airgap-import-only` (or `AIRGAP_MODE=import`) flag on `bootstrap-ec2.sh`
   that skips `airgap_setup()` (no zot, no mirror) and relies on
   `import_images()` + the k3s auto-load dir, with `agents.agentPullPolicy:
   Never` and a `pin-images` helper that labels every imported image
   `io.cri.containerd.pinned=pinned`.
2. A bundle artifact (or on-box `crane export`/`skopeo` conversion) in the
   docker/OCI image-archive shape `ctr import` consumes.
3. A documented "on reschedule / after GC, re-run `import_images`" recovery note,
   and an explicit warning that import-only is **not** recommended for boxes that
   reschedule pods or upgrade in place.

None of the above is warranted *now*: the default path is correct, complete, and
already minimises the runtime hot-path. This eval's conclusion is to **keep the
current default and document the alternative**, which this doc does.

---

## Key repo anchors

- `scripts/inbox/install-registry.sh`, `seed-bundle.sh`, `configure-mirror.sh` — the three zot pieces
- `scripts/bootstrap-ec2.sh` — `import_images()` (stage 4, `k3s ctr images import`), `airgap_setup()` (stage 4b, zot)
- `gardenio/controller/internal/podmanager/manager.go:558` (`agentPullPolicy()` override), `:1037` (agent `ImagePullPolicy`)
- `gardenio/controller/internal/config/config.go:87-99,834-835` (`DindImage`/`AgentPullPolicy` env)
- `gardenio/helm/gardenio/templates/_helpers.tpl:30-59` (`global.airgap` forces `latestMode` off → `IfNotPresent` + digest binds)
- `gardenio/helm/gardenio/values.yaml:767-769` (`global.airgap`), `:1550-1557` (`dindImage`/`agentPullPolicy`)
- `gardenio/helm/gardenio/values-inbox.yaml` — `global.airgap: true`, `agents.agentPullPolicy: IfNotPresent`, first-party digest pins
- `.rwx/bundle.yml` — crane OCI-layout bundle (built for `crane push`, **not** `ctr import` — format gap, §2)
- `gardenio/docs/research/inbox-image-distribution.md` — open-question 1 (this doc answers it)
- `gardenio/docs/research/inbox-ec2-sizing-bootstrap.md` §3 — k3s auto-load dir + containerd GC pin labels
- `gardenio/docs/gardenio-in-a-box-install.md` Part D — `--airgap` runbook
