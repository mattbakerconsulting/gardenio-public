# Tarboat (Gardenio-in-a-Tar) — Gap Analysis

> **⚠️ REFRESHED 2026-08-02.** This is the *original* gap map (2026-06-10). Much of it has since
> landed on `main`. For the current-state register + next steps, read
> [`tarboat/tarboat-gap-refresh-2026-08.md`](tarboat/tarboat-gap-refresh-2026-08.md) alongside
> this doc — this one is kept for the original framing and the operator's Q1–Q4 decisions.
>
> **Tracking bead:** ka-0000002628 · **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Thread agent:** thread-1781113307-279539 · **Date:** 2026-06-10
>
> **The ask (Matt Baker, Slack):** a special distribution — *"gardenio in a tar (tarboat)"* —
> with **all air-gapped resources** and a **full working setup guide**. *"The most important
> thing about this project will be making sure that the tarball is completely validated from
> scratch using inference. The whole point is that a raw Claude Code instance should be able to
> drive the set up of Gardenio with a single prompt. This is the path we want to test and prove
> before we can release the tarball to our friends — something we'd like to do today if possible."*
>
> **This doc identifies the gaps first** (per the operator's "identify gaps first" instruction),
> then became the input to a phased Tarboat backlog now being executed. **Build status as of
> 2026-06-10** (all on branch `feat/tarboat-gap-analysis` / MR !4070 unless noted):
>
> **T1 (self-contained tarball) is COMPLETE — all four pieces merged to the branch:**
> - **G1.1 offline-asset staging — DONE** (`--offline-assets` in `bootstrap-ec2.sh` +
>   `install-registry.sh`; hard-fail under `--airgap-strict`; 9 unit tests).
> - **G1.2 carry repo tree — DONE** (MR !4072; `detect_repo_dir()` re-keyed on the
>   `deploy-byo.sh` sentinel so a tarball-staged tree with no `.git` is accepted).
> - **G1.3 tarball assembly — DONE** (MR !4074; `scripts/tarboat/assemble-tarboat.sh`
>   builds the single `gardenio-tarboat-<calver>.tar.zst` + MANIFEST/SHA256SUMS/LICENSES).
> - **G1.4 local-CA TLS — DONE** (`tls.mode=localCA` chart chain + trust-import runbook
>   `tarboat-local-ca-tls.md`; 3 helm tests).
>
> **T2 (inference-driven validation) — skeleton done; in-box inference now landed:**
> - **G2.2/G2.5 single-prompt setup guide — DONE on main** (`gardenio/docs/tarboat/TARBOAT-SETUP.md`
>   + `TARBOAT-AGENT-PROMPT.md`, landed by a parallel agent against the same task ka-0000004969).
> - **G2.3 from-scratch validation harness (the release gate) — DRAFTED + TESTED**
>   (`scripts/inbox/tarboat-validate.sh` + 13 unit tests; gate 4 wires to the now-merged in-box LiteLLM).
>   CI/cron-runnable against an ephemeral box: `--results-json PATH` emits a stable
>   `tarboat-validate/v1` per-gate result doc (status pass/fail/skip + the failing
>   assertion + a `release_grade` flag that is false whenever any gate was skipped),
>   for the E2 regression-bug filer (epic ka-0000003645, E1=ka-0000001107).
> - **T0 in-box inference (ka-0000007994) — MERGED to main** (in-box LiteLLM chart + seed script).
>   The last blocker on a release-grade proof run is now in place; gate 4 of the harness can be
>   made real and the proof run driven.

---

## TL;DR

Gardenio-in-a-Box is **built** — the mega-epic ka-0000001295 and all six phase epics (P0–P5)
are closed. We have a bootstrap script, two values overlays, an air-gap OCI image bundle, a
host-hardening path, an install runbook, and an acceptance smoke. **Tarboat is not a new
platform — it is two thin-but-real layers on top of that built platform:**

1. **A genuinely self-contained tarball.** Today's "air-gap" is *image-air-gapped only*. The
   bootstrap still reaches the public internet for k3s, helm, cert-manager, the ServiceMonitor
   CRD, the EDP keycloak-operator chart, zot, and crane — and it requires cloning the **private**
   `gardenio` repo. A friend with a tarball and no GitLab access cannot install today.
2. **An inference-driven, from-scratch validation path** — the headline ask. There is **no**
   single-prompt agent runbook, no agent-executable acceptance harness, and no proof that a raw
   Claude Code instance can drive the install end-to-end. This is the gate before release.

The single biggest **technical** blocker behind the headline ask is **in-box inference**
(epic ka-0000007994, open): the box has no self-contained inference path, so the very agent
we want to "drive the setup" has nothing to think with once external provider egress is blocked
or throttled.

---

## What already exists (do NOT rebuild)

| Capability | Artifact | Bead |
|---|---|---|
| Single-EC2 k3s substrate + scripted bootstrap | `scripts/bootstrap-ec2.sh`, `scripts/k3s/*`, `scripts/cloud-config/*` | ka-0000007690 (P0, closed) |
| local-path storage, in-box-seeded secrets, pg_dumpall DR | `values-inbox.yaml`, `scripts/deploy-byo.sh` | ka-0000007539 (P1, closed) |
| In-box object store (MinIO, S3-free) | `templates/minio/*`, `inBox.objectStore.*` | ka-0000004955 (P2, closed) |
| Public-safe edge: forwardAuth-on-every-route, NetworkPolicy default-deny, CrowdSec, sslip.io + HTTP-01 TLS, host firewall + IMDSv2 | `deploy-values/gardenio-inbox.yaml`, `scripts/k3s/install-host-hardening.sh` | ka-0000005904 (P3, closed) |
| Air-gap **image** distribution: OCI bundle (zot + ctr-import shapes), containerd mirror | `.rwx/bundle.yml`, `scripts/inbox/*`, `global.airgap` | ka-0000007881 (P4, closed) |
| Documented one-shot install + acceptance smoke | `gardenio-in-a-box-install.md`, `scripts/inbox/acceptance-smoke.sh` | ka-0000006354 (P5, closed) |
| In-box Keycloak identity (Path A) | epic ka-0000005376 (closed) | — |

The architecture/substitution synthesis is `gardenio-in-a-box-synthesis.md`. **Tarboat reuses all
of it.** The gaps below are deltas, not redesigns.

---

## Gap bucket 1 — A genuinely self-contained tarball

The operator said *"all air-gapped resources."* Today's bundle air-gaps the **container images**
and nothing else. Verified against the actual scripts:

### G1.1 — Bootstrap still pulls binaries/manifests from the public internet

Even in `--airgap` mode, `scripts/bootstrap-ec2.sh` fetches at install time:

| What | Source (verified) | Line |
|---|---|---|
| k3s installer + binary | `https://get.k3s.io` | `bootstrap-ec2.sh:368` |
| helm | `https://raw.githubusercontent.com/helm/helm/.../get-helm-3` | `bootstrap-ec2.sh:578` |
| crane | `https://github.com/google/go-containerregistry/releases/...` | `bootstrap-ec2.sh:592` |
| cert-manager full manifest | `https://github.com/cert-manager/.../cert-manager.yaml` | `bootstrap-ec2.sh:682` |
| ServiceMonitor CRD | `https://raw.githubusercontent.com/prometheus-operator/...` | `bootstrap-ec2.sh:698` |
| EDP keycloak-operator chart | `helm repo add https://epam.github.io/edp-helm-charts/stable` | `bootstrap-ec2.sh:710` |
| zot (`--airgap`) | `https://github.com/project-zot/zot/releases/...` | `inbox/install-registry.sh:89` |

**Gap:** a true tarball must stage *all* of these offline — k3s binary + airgap images tarball,
the helm/crane/zot binaries, the cert-manager + ServiceMonitor manifests, and the
keycloak-operator chart (`helm pull`-ed into the bundle) — and the bootstrap must consume the
staged copies with a network fetch only as a fallback (or never, under `--airgap-strict`). This
is the difference between "air-gapped images" and "air-gapped install."

### G1.2 — The private `gardenio` repo is a hard install prerequisite

`gardenio-in-a-box-install.md` §A.0 item 5: cloud-init clones the **private** `gardenio` repo to
`/opt/gardenio/repo`, and the whole bootstrap aborts if it can't (ka-0000001441). The documented
escapes are AMI-bake, a clone token, or "air-gap bundle stages repo content." **A friend cannot
`git clone` our private repo.** Tarboat's defining property is that the tarball *carries* the
repo content (scripts + chart + overlays) so there is no clone step at all — the `.git`
short-circuit already exists; we just need the bundle to actually contain the tree and the
guide to make "untar, run" the only path.

**Resolved (box-side, G1.2 ka-0000008214).** The clone short-circuit was the trap: it keyed on
`/opt/gardenio/repo/.git`, but a tarball-staged tree has no `.git`, so the box still reached the
private clone and aborted. The fix re-keys both code paths on the sentinel
`scripts/deploy-byo.sh` (never `.git`): (1) `bootstrap-ec2.sh:detect_repo_dir()` gained a probe
for `$OFFLINE_ASSETS/repo/scripts/deploy-byo.sh`; (2) the cloud-init template detects the staged
tree on that sentinel, symlinks `$OFFLINE_ASSETS/repo` → `/opt/gardenio/repo`, and `exit 0`s
before the clone is ever reached — the true "untar, no clone" path. A new `--offline-assets`
placeholder threads the dir (default `/opt/gardenio/tarboat`) through `render-user-data.sh`. The
remaining half — actually *staging* the gardenio subset (`scripts/` + `gardenio/helm/gardenio/` +
`deploy-values/` + install docs) into the tarball's `repo/` directory — is the build-side work
in **G1.3** (ka-0000000192); the layout contract is recorded on that bead.

### G1.3 — No single tarball artifact / no manifest+checksum/provenance for the whole thing

`.rwx/bundle.yml` emits only the **image** artifacts (`*.oci.tar.zst`,
`*.images.oci-archive.tar.zst`). There is no `gardenio-tarboat-<calver>.tar.zst` that bundles
images + binaries + manifests + repo tree + the setup guide + a top-level checksum/SBOM. **Gap:**
define the tarball layout, build it in CI (extend `bundle.yml` or a new `.rwx/tarboat.yml`), and
ship one signed artifact with a `MANIFEST` + `SHA256SUMS` + provenance so a friend can verify
what they received.

### G1.4 — TLS for a real air-gapped box (no public internet at all)

The turnkey default is sslip.io + Let's Encrypt HTTP-01, which **needs outbound :443 to LE and
inbound :80 for the challenge** — fine for an internet-connected single-EC2, but not for a true
air-gap. The local-CA fallback exists in `values-inbox.yaml` (self-signed CA → leaf, comment at
line ~370) but is **not wired into the one-shot path** and its trust-distribution runbook
(browser/iOS/CLI import) is referenced, not written. **Gap:** make the local-CA TLS mode a
first-class, scripted Tarboat option with a documented trust-import guide, so "no internet"
actually works.

### G1.5 — `--airgap-import-only` GC/reschedule caveat needs to be the Tarboat default story

The registry-free `--airgap-import-only` mode (zero runtime registry) is exactly the
strict-offline appliance shape friends will want, but it has the documented "breaks on
reschedule / after GC" caveat (install doc Part D). **Gap:** decide whether Tarboat's default is
`--airgap` (zot, recovers automatically, ~80 MB resident) or import-only (lighter, fragile), and
make the guide opinionated rather than presenting both.

---

## Gap bucket 2 — Inference-driven, from-scratch validation (THE HEADLINE)

> *"A raw Claude Code instance should be able to drive the set up of Gardenio with a single
> prompt … test and prove this path before we release."*

This is the part with **the least existing coverage** and the highest operator priority.

### G2.1 — In-box inference does not exist yet (the hard technical blocker)

Epic **ka-0000007994** ("In-box LiteLLM") is **open**; child ka-0000006789 is in_progress on
another thread. Today the box depends on the corp LiteLLM proxy / direct Anthropic, whose
WAF/IP-throttle rejects a fresh EC2 egress IP (proven on the live box, ka-0000004450). **Until
the box has an in-cluster inference chokepoint, an agent running *on* the box (or *driving* it)
has no reliable, self-contained way to think.** This is the foundation the rest of bucket 2
stands on. Recommend: treat ka-0000007994 as a **Tarboat blocker**, not a parallel nicety.

> **UPDATE (ka-0000007708, 2026-06-11):** The credential half of this blocker is resolved, and
> not the way ka-0000004450 assumed. There is **no `sk-ant-api…` key anywhere in this org** —
> every Anthropic credential is a Claude OAuth token (`sk-ant-oat01…`). So the "in-box LiteLLM
> holds a static provider key" plan is dead. The in-box LiteLLM now runs with
> `forward_client_headers_to_llm_api: true` and **forwards the on-box agent's OAuth bearer token**
> to `api.anthropic.com` (same mechanism as the shared devops LiteLLM). A direct OAuth
> `/v1/messages` round-trip from the live box EIP was verified working (**HTTP 200**), so the
> ka-0000004450 IP-throttle did **not** materialize for a single call — though it remains a
> known risk under sustained load, mitigated by an EIP allowlist request (there is no static key
> to fall back to). Chart + runbook updated on branch `agent/tarboat-oauth-inference-ka-0000007708`.

> **Scope nuance to settle with the operator:** "a raw Claude Code instance drives the setup
> with a single prompt" — does that agent run **on the box** (needs in-box inference + a staged
> agent image, which the bundle already carries) or **on the friend's laptop** driving the box
> over SSH (needs the friend's own Claude Code + network to the box)? The single-prompt UX and
> the air-gap story pull in different directions; the answer picks the validation harness shape.
> Captured as open question Q1 below.

### G2.2 — There is no single-prompt setup runbook for an agent to execute

`gardenio-in-a-box-install.md` is written **for a human operator**: it has interactive SSH steps,
"do A.4 by hand then re-run the deploy stage," and prose tradeoffs. **Gap:** a deterministic,
agent-targeted `TARBOAT-SETUP.md` (or the single prompt itself) — an ordered, copy-pasteable,
idempotent command sequence with explicit success/failure assertions after each step, no "decide
based on your judgment" forks, and every input either staged in the tarball or named up front.
This *is* "the full working setup guide" the operator asked for, but shaped for inference.

### G2.3 — The acceptance smoke is operator-run, not agent-run, and asserts the wrong layer

`scripts/inbox/acceptance-smoke.sh` is good but (a) is invoked by a human with flags, and (b)
proves the **box's runtime posture** (cert trust, route gating, ports, IMDS block). It does
**not** prove the **inference-driven install succeeded** — i.e. "an agent, given only the
tarball and one prompt, reached a green box with no human intervention." **Gap:** an
agent-executable validation harness that wraps the smoke and adds the from-scratch assertions:
fresh instance → untar → single prompt → agent drives bootstrap → all pods Running → acceptance
smoke green → an agent on the box completes one inference round-trip. The exit code of *that* is
the release gate.

### G2.4 — No "fresh-from-scratch" proof run on record

P5.2 acceptance (ka-0000005704) was run against a hand-bootstrapped box; the live-box inference
proof (ka-0000004450) used a tunnel/scaffold, not the self-contained path. **There is no recorded
run of: blank EC2 + tarball + one prompt → working gardenio, agent-driven.** The operator wants
exactly this proof **today**. **Gap:** execute one real validation run end-to-end and capture the
transcript/artifacts as the release evidence. This is a *doing* gap, not a *building* gap — but it
depends on G2.1/G2.2/G2.3 existing.

### G2.5 — Single-prompt bootstrapping of the agent itself

For a raw Claude Code instance to "drive the setup," it needs, at minute zero: the tarball
location, the four operator-supplied inputs (instance, EIP, SSH key, and — per Q1 — an inference
credential or the in-box-LiteLLM provider key), and a prompt that points it at `TARBOAT-SETUP.md`.
**Gap:** write that prompt, and decide how the credential reaches the driving agent without
baking a secret into the tarball (it must not — see G1.3 provenance).

---

## Cross-cutting / smaller gaps

- **G3.1 Sealing the egress allowlist.** The threat-model sign-off asserts default-deny egress,
  but Let's Encrypt (G1.4) and the LiteLLM→provider hop (G2.1) are deliberate holes. Tarboat
  needs one documented, minimal egress allowlist (or zero, under strict air-gap + local-CA).
- **G3.2 Versioned compatibility.** The tarball pins one calver across images + chart + scripts.
  We need a `--app-version`-equivalent that is implicit in the tarball (the friend shouldn't have
  to pick a tag — install doc §A.2a shows how easily that goes wrong) and a stated support window.
- **G3.3 Licensing in a redistributed artifact.** MinIO is AGPL-v3 (decision ka-0000004293
  resolved for in-cluster self-host). **Redistributing** a tarball to "our friends" is a
  different posture than self-hosting — confirm the AGPL-v3 (and every bundled image's license)
  is acceptable for outbound distribution, and ship a `LICENSES/` / NOTICE in the tarball.
- **G3.4 Friend-grade docs.** Current docs assume gardenio-internal context (beads, gb, RWX). A
  friend needs a plain-English "what is this, what do I need, what will it cost (r7i sizing),
  how do I get help" front page.

---

## Operator decisions (RESOLVED — Matt Baker, Slack 2026-06-10, decision ka-0000002491)

- **Q1 — Where does the "driving" agent run? → ON THE BOX.** The agent runs on the box, so the
  validation harness (G2.3) and single prompt (G2.5) target an on-box agent, and in-box inference
  (G2.1) is firmly on the critical path.
- **Q2 — Internet-connected or true air-gap? → TRUE AIR-GAP.** No public internet at install.
  This forces local-CA TLS (G1.4) into T1's critical path — sslip.io+LE is internet-dependent, so
  local-CA + a trust-import runbook is the air-gap default, not a later fallback.
- **Q3 — zot vs import-only default?** Not explicitly answered; default to `--airgap` (zot) for
  auto-recovery unless the strict-offline appliance shape argues otherwise (revisit in T1).
- **Q4 — AGPL-v3 MinIO in a redistributed tarball? → OK FOR NOW.** Ship MinIO; carry its AGPL-v3
  license in the tarball `LICENSES/`/NOTICE (G3.3). SeaweedFS stays the documented fallback.

### New scope added by the operator (same message)

- **A "test-in-the-box" e2e suite** that *separately* validates the boxed gardenio, **kept alive
  over time** to catch regressions as we discover them. This is distinct from the from-scratch
  install proof (G2.4): it is an ongoing regression suite for the boxed artifact itself. → new
  epic (T-EBOX below).
- **The minimum-viable config/prompt subset is "the essence of the OSS release."** Production
  gardenio carries a huge amount of configuration and prompting; finding the minimal viable subset
  for a friend-grade box is a long, human-stakeholder-driven discovery process. → new epic
  (T-MVP below), long-lived and human-in-the-loop.

---

## Recommended phasing (proposed, not yet filed)

Filing the backlog is the next step *after* the operator confirms Q1–Q4. Proposed shape:

- **T0 — In-box inference (ka-0000007994) lands & is proven on the box.** Hard blocker for the
  headline ask (confirmed by Q1=on-box). Already in flight on another thread.
- **T1 — Self-contained tarball** *(STARTED THIS CYCLE per operator pick).* Stage all bootstrap
  binaries/manifests offline (G1.1), carry the repo tree (G1.2), define + build the single signed
  tarball artifact (G1.3), wire **local-CA TLS into the critical path** (G1.4, forced by Q2=true
  air-gap), default to zot (G1.5, Q3 pending). Independent of T0 — builds in parallel.
- **T2 — Inference-driven setup + validation.** `TARBOAT-SETUP.md` + the single prompt for an
  **on-box** agent (G2.2, G2.5), agent-executable from-scratch validation harness (G2.3).
- **T3 — Prove it.** One recorded fresh-from-scratch, on-box-agent-driven run as the release gate
  (G2.4). Depends on T0+T1+T2.
- **T4 — Friend-grade polish.** Tarball `LICENSES/`/NOTICE incl. AGPL-v3 MinIO (G3.3, Q4=ok),
  egress allowlist doc (G3.1), implicit version pin/support window (G3.2), plain-English front
  page (G3.4).
- **T-EBOX — "Test-in-the-box" e2e regression suite** *(new operator scope).* A separate,
  long-lived suite that validates the boxed gardenio and is kept alive to catch regressions over
  time. Distinct from the one-shot T3 proof.
- **T-MVP — Minimum-viable config/prompt subset** *(new operator scope, "the essence of the OSS
  release").* Long, human-stakeholder-driven discovery of the minimal config+prompting a
  friend-grade box needs. Long-lived, human-in-the-loop.

T3 is the operator's "test and prove before release" gate. T1+T2 are its prerequisites; T0 is the
foundation under all of it. T-EBOX and T-MVP are ongoing tracks that outlive the initial release.

---

Operator: Matt Baker
