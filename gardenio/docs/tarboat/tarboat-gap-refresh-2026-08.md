# Tarboat (Gardenio-in-a-Tar) — Gap Analysis Refresh, 2026-08-02

> **Tracking bead:** ka-0000002628 · **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Author:** gardenio-tarboat-gardenioinatar-gap-vt5 (crew) · **Date:** 2026-08-02
>
> **This doc refreshes** the original gap analysis (`gardenio/docs/tarboat-gap-analysis.md`,
> dated 2026-06-10). That doc is still an accurate record of the *original* gap map and the
> operator's Q1–Q4 decisions — read it first for the framing. **~2 months of work have landed
> since**, so several gaps it lists as open are now closed, and a handful of *new* gaps have
> surfaced that the June doc could not have known about (they were only discoverable once the
> tarball was actually assembled and the standing regression suite was built). This refresh
> restates the register against the **verified state of `main` at release 2026.214.11**.

---

## The ask (unchanged)

Matt Baker, Slack: a special distribution — *"gardenio in a tar (tarboat)"* — carrying **all
air-gapped resources** and a **full working setup guide**. *"The most important thing … is
making sure that the tarball is completely validated from scratch using inference. A raw
Claude Code instance should be able to drive the set up of Gardenio with a single prompt. This
is the path we want to test and prove before we can release the tarball to our friends."*

The June decisions still bind (decision ka-0000002491): **Q1 driving agent runs ON the box ·
Q2 true air-gap (local-CA TLS) · Q3 default to `--airgap` (zot) · Q4 AGPL-v3 MinIO is OK for
now, carry its LICENSE.** Two new operator scopes were added the same day: a long-lived
**test-in-the-box regression suite** (epic ka-0000003645) and a long-lived **minimum-viable
config/prompt subset** discovery (epic ka-0000000011).

---

## TL;DR (2026-08-02)

**Bucket 1 (self-contained tarball) and Bucket 2's *build* deliverables are essentially DONE
and on `main`.** The tarball assembler, the offline-staging bootstrap, local-CA TLS, in-box
LiteLLM (OAuth-forwarding), the agent-targeted setup runbook + single prompt, the 6-gate
release-gate validator, the regression-bug filer, and the full provision→validate→file→teardown
sweep with a reaper all exist, are unit-tested, and CI-wired. **This is a large, real body of
completed work** — Tarboat is no longer a design exercise.

**But the headline ask is still NOT met, and the standing suite has quietly stopped covering
the parts that matter:**

1. **No release-grade validation has ever been produced.** The validator emits a
   `release_grade` boolean that is true *only* when every gate ran and none was skipped. The
   standing sweep runs against a **BYO internet-clone box** with `--skip-bootstrap
   --skip-inference --box-shape byo`, so the three gates that prove the *tarball* claim
   (`staging`, `bootstrap` true-air-gap, `inference` round-trip) are **never exercised**. Every
   green sweep to date is `release_grade=false`. (§ Headline gap.)
2. **No recorded from-scratch, one-prompt, agent-driven run exists** — the operator's explicit
   release gate (ka-0000004567, still open). The one June attempt was killed mid-build and
   surfaced three real assembler/validator bugs that were never filed or fixed.
3. **The tarball does not actually carry the single prompt.** `assemble-tarboat.sh` still
   writes `SETUP-PROMPT.txt` as a **placeholder heredoc**, even though `TARBOAT-AGENT-PROMPT.md`
   claims "the same text ships in the tarball as SETUP-PROMPT.txt." The headline UX artifact is
   a stub.
4. **The assembled tarball stages only the import-only image archive, not the zot bundle** —
   but Q3 locked `--airgap` (zot) as the default. A friend following the default path has no
   images to seed. (This is exactly the June proof-run's GAP-1; never filed, never fixed.)
5. **The regression gardener lane is dark.** ~13 releases have shipped unvalidated
   (last sweep box-ref 2026.201.3 on 07-20; HEAD is now 2026.214.11). An operator decision to
   revive it is open (ka-0000006575).
6. **The "essence of the OSS release" (MVC config subset) is inventoried but not built.** The
   redaction/seed mechanism (`seed-config-dump.sh`) does not exist; the tarball ships no config
   seed.
7. **No durable publish.** The tarball is an ephemeral RWX artifact; gpg signing is off by
   default; the GitLab distribution home is an unanswered open question.

The net: **the machinery to prove the headline exists, but the proof has never been run
end-to-end, and the standing suite is not configured to run it.** Closing the headline is now
mostly a *doing/wiring* problem, not a *building* problem — plus two genuine code gaps (#3, #4)
that would make any real from-scratch run fail.

---

## What changed since 2026-06-10 (the delta)

| June status | Today (2026.214.11) | Evidence |
|---|---|---|
| G1.1 offline staging — DONE (on branch) | **DONE, on main** | `bootstrap-ec2.sh` `--offline-assets`/`--airgap-strict`; `offline_required()` hard-fails staged-asset misses |
| G1.2 carry repo tree — DONE (on branch) | **DONE, on main** | `detect_repo_dir()` probes `$OFFLINE_ASSETS/repo/scripts/deploy-byo.sh` sentinel |
| G1.3 tarball assembly — DONE (on branch) | **DONE, on main + CI** | `scripts/tarboat/assemble-tarboat.sh` + `.rwx/bundle.yml` `build-tarboat` task emits `gardenio-tarboat-<calver>.tar.zst` + sha256/MANIFEST/SHA256SUMS/LICENSES |
| G1.4 local-CA TLS — DONE (on branch) | **DONE, on main** | `tls.mode=localCA` chain; `gardenio/docs/tarboat/tarboat-local-ca-tls.md` |
| G2.1 in-box inference — epic OPEN | **DONE, on main** — OAuth-forwarding | `templates/litellm/*`; `forward_client_headers_to_llm_api: true`; no `sk-ant-api…` key exists in this org (MR !4106) |
| G2.2/G2.5 setup runbook + prompt — skeleton | **DONE, on main** (with stub, see #3 above) | `gardenio/docs/tarboat/TARBOAT-SETUP.md` + `TARBOAT-AGENT-PROMPT.md` |
| G2.3 validation harness — drafted | **DONE, on main** — 6 gates + `--results-json`/`release_grade` | `scripts/inbox/tarboat-validate.sh` |
| T-EBOX regression suite — proposed | **BUILT, but running degraded + lane dark** | `.rwx/tarboat-sweep.yml`, `tarboat-probe.yml`, `tarboat-file-regressions.sh`; gardener dark (ka-0000006575) |
| G2.4 recorded proof run — none | **STILL none** (ka-0000004567 open) | one June attempt killed mid-build |

**Net:** the June doc's "Bucket 1 complete / Bucket 2 skeleton" is now "Bucket 1 + Bucket 2
build complete; the *proof* and the *standing coverage of the proof* are the live gaps."

---

## Current gap register (verified against `main` @ 2026.214.11)

Legend: **CLOSED** (built + on main) · **OPEN** (real remaining gap) · **DEGRADED** (built but
not delivering its intended coverage).

### Bucket 1 — self-contained tarball

- **G1.1 offline staging — CLOSED.** `--offline-assets`, `--airgap`, `--airgap-strict`,
  `--airgap-import-only` all present in `bootstrap-ec2.sh`; strict mode hard-fails on missing
  staged assets. k3s/helm/crane/zot/cert-manager/ServiceMonitor-CRD/keycloak-operator are all
  staged.
- **G1.2 carry repo tree — CLOSED.** Sentinel re-key on `deploy-byo.sh`; the tarball carries a
  monorepo subset under `repo/`.
- **G1.3 single signed tarball — CLOSED (build + sign wired) / OPEN (publish; vault key).** The
  artifact is built with MANIFEST + SHA256SUMS + LICENSES. **gpg signing is now wired ON by
  default** in CI (ka-0000008173): `bundle.yml` build-tarboat references
  `TARBOAT_GPG_KEYID` + `TARBOAT_SIGNING_KEY` and imports the private key before assembly, so
  the assembler emits a detached `.asc` beside the tarball. Both secrets resolve to an empty
  string when unset (RWX semantics), so a release cut before the `gardenio-main` vault key is
  provisioned still builds on the sha256 + MANIFEST + SHA256SUMS floor — **so the one remaining
  step to make signing live is the operator provisioning the two vault secrets** (a GPG keypair
  + published public key for friends to verify). **Durable publish is still MISSING**
  (`TARBOAT-PUBLISH.md`): the tarball exists only as an ephemeral RWX artifact; the GitLab
  destination + distribution shape (Generic Package vs Release asset) are unanswered.
- **G1.4 local-CA TLS — CLOSED.**
- **G1.5 `--airgap` vs import-only default — RESOLVED (Q3=zot) but the assembler contradicts
  it — OPEN.** **NEW GAP (from the June proof run, never filed):** `assemble-tarboat.sh` stages
  only `gardenio-bundle-<cv>.images.oci-archive.tar.zst` (the import-only shape) under `images/`.
  The zot default path (`seed-bundle.sh`) needs `gardenio-bundle-<cv>.oci.tar.zst`, which the
  tarball does **not** carry. A friend on the locked-default `--airgap` (zot) path has no images
  to seed. Either stage both shapes, or flip the documented default to import-only. (See
  ka-0000005158 comments for the original three-bug cross-read: assemble/stage-dir/validator-gate.)

### Bucket 2 — inference-driven from-scratch validation (THE HEADLINE)

- **G2.1 in-box inference — CLOSED.** In-box LiteLLM forwards the on-box agent's OAuth bearer
  token upstream (no static provider key exists org-wide). Model-group aliasing (`model_group_alias`
  bare→`-oauth`) is load-bearing — the controller hard-codes `-oauth` targets.
- **G2.2 agent-targeted setup runbook — CLOSED.** `TARBOAT-SETUP.md` is deterministic and
  agent-shaped.
- **G2.3 agent-executable validation harness — CLOSED (built) / DEGRADED (as run).** The 6-gate
  validator with `release_grade` exists and is correct. **But the only thing that runs it — the
  standing sweep — runs it degraded (see Headline gap).** No caller has ever invoked it in a
  `release_grade=true` configuration.
- **G2.4 recorded from-scratch proof run — OPEN (the headline gate).** ka-0000004567. Still no
  recorded "blank EC2 + tarball + one prompt → green box, agent-driven, validate exit 0."
- **G2.5 single-prompt bootstrap of the driving agent — OPEN (code gap).** **NEW GAP:**
  `assemble-tarboat.sh:328-335` writes `SETUP-PROMPT.txt` as a placeholder heredoc that says
  *"Tarboat T2 fills this in … follow README.md,"* but `TARBOAT-AGENT-PROMPT.md` already exists
  and claims it ships as `SETUP-PROMPT.txt`. The assembler was never updated to copy the real
  prompt. The headline "single prompt" is not actually in the tarball.

### Cross-cutting

- **G3.1 egress allowlist — partially addressed.** The `inference` gate asserts a default-deny
  egress NetworkPolicy exists; a single documented allowlist (LiteLLM→provider + optional LE)
  is still not consolidated in one friend-facing place.
- **G3.2 implicit version pin — CLOSED-ish.** The tarball pins one calver across images + chart
  + scripts; `assemble-tarboat.sh` derives version pins from the consumer scripts so staged
  filenames byte-match. A stated support window is still absent.
- **G3.3 licensing in a redistributed artifact — CLOSED.** `LICENSES/` shipped; MinIO AGPL-v3
  accepted for now (Q4).
- **G3.4 friend-grade front page — CLOSED.** `tarboat-README.md` built into the tarball.

### New scope beyond the original buckets

- **T-EBOX test-in-the-box regression suite — BUILT but DEGRADED + lane DARK.** The pipeline
  (`provision → validate → file-regressions → teardown`), reaper, and gardener runbook all exist
  and worked through July (32 children closed, several real gate bugs found+fixed). **Two
  problems now:** (a) the sweep is BYO-only, so it never validates the tarball claim itself
  (only login + beads_data on an internet-clone box); (b) the gardener lane went dark 2026-07-20
  and ~13 releases have shipped unvalidated. Revival is an open operator decision (ka-0000006575,
  recommendation: revive-with-revisions; note the pre-`gardenerWallClockGrandfatherEpoch` expiry
  caveat).
- **T-MVP minimum-viable config subset — INVENTORIED, NOT BUILT.** M1 inventory done
  (`mvc-config-inventory.md`); Q2/Q3/Q4 resolved. **M3 (ka-0000003598) — the seed mechanism +
  redaction deny-list — is the last open child and is not built.** `scripts/inbox/seed-config-dump.sh`
  does not exist; the tarball ships no redacted config/advice seed. This is "the essence of the
  OSS release" and remains a long-lived, human-in-the-loop track.

---

## The headline gap, in detail: nothing has produced a `release_grade=true` result

This is the single most important finding of the refresh, because it is easy to mistake the
green standing sweeps for "the tarball is validated."

`tarboat-validate.sh` computes `release_grade = (overall==pass AND gates_skipped==0)`
(`emit_results()`). The gates that prove the *tarball / air-gap / inference* claims are:

- **gate 1 `staging`** — tarball unpacked + MANIFEST/SHA256SUMS integrity. **SKIPs** on a BYO box.
- **gate 2 `bootstrap`** — bootstrap in true `--airgap-strict`. Skipped via `--skip-bootstrap`.
- **gate 4 `inference`** — real `/v1/messages` round-trip via in-cluster LiteLLM
  (with `--inference-live`). Skipped via `--skip-inference`.

The standing sweep (`.rwx/tarboat-sweep.yml`) provisions a **bring-your-own internet-clone box**
and runs `tarboat-validate.sh --skip-bootstrap --skip-inference --box-shape byo`. So it only
ever exercises **gate 3 (acceptance posture), gate 5 (login), gate 6 (beads_data)** — none of
which is the tarball claim. **Consequence:** every sweep to date is `release_grade=false` by
construction, and the operator's headline ("validated from scratch using inference") has no
green evidence anywhere.

That is not a bug in the validator or the sweep — the BYO sweep is a legitimate cheap
regression signal for the *boxed runtime*. The gap is that **no second, higher-fidelity path
runs the validator release-grade**, and that path is exactly the operator's proof run
(ka-0000004567). Until it runs once, the release gate is unmet.

---

## Recommended next steps

Ordered by leverage. The first three are small and unblock the headline; they should land
before any proof run is attempted, because a real from-scratch run will fail on #1 and #2.

**S1 — Fix the two assembler gaps that break any real from-scratch run (code; small).**
File as bugs parented to the T1 lineage and fix:
  - S1a: copy the real `TARBOAT-AGENT-PROMPT.md` into `SETUP-PROMPT.txt` in
    `assemble-tarboat.sh` (replace the placeholder heredoc). Add an assembler test asserting the
    staged prompt is non-placeholder.
  - S1b: stage the zot `gardenio-bundle-<cv>.oci.tar.zst` under `images/` (Q3=zot default), or —
    if import-only is actually preferred for friends — flip the documented default and the
    validator's gate expectations to match. Decide explicitly; do not ship a tarball whose
    default path has no images.

**S2 — Reconcile the validator gates with the assembled tarball layout (code; small).**
The June cross-read found `TARBOAT-SETUP.md §4.3` copies the zot bundle to `/opt/gardenio/images`
while `seed-bundle.sh` reads `BUNDLE_DIR=/opt/gardenio/bundle`, and gate 1/2 assert the
import-only archive while gate 2 runs zot `--airgap-strict`. Make setup doc, seed script,
assembler, and validator agree on one bundle shape + one staging dir. (ka-0000005158 has the
receipts.)

**S3 — Drive the recorded from-scratch proof run (doing; the release gate).** ka-0000004567.
Needs one operator action: a fresh EC2 + an inference credential for the driving agent (the
operator provides the box + credential, not labor — ka-0000003508). Then: cut/assemble the
tarball at a promoted calver → stage on a blank box → untar → `sha256sum -c` → start a raw
Claude Code instance on the box → paste the single prompt → agent drives `TARBOAT-SETUP.md` →
`tarboat-validate.sh --inference-live` (no `--skip-*`) → **exit 0 with `release_grade=true`**
→ capture the transcript + results.json as the release evidence. **This is the operator's "prove
it before we release to friends" gate.** File a `gb decision`/`gb escalation` for the EC2 +
credential provisioning, since that is the one thing an agent cannot self-serve.

**S4 — Add a release-grade sweep path (wiring; medium).** Extend `.rwx/tarboat-sweep.yml` (or a
new job) with a `--box-shape tarball` variant that assembles the tarball, provisions a box with
**no** clone token, runs `bootstrap-ec2.sh --airgap --airgap-strict --offline-assets`, and runs
`tarboat-validate.sh --inference-live` with no skips — so `release_grade=true` becomes a
standing, re-runnable signal, not a one-time manual proof. Keep the cheap BYO sweep too; they
answer different questions.

**S5 — Resolve the dark regression-gardener decision (operator; small).** ka-0000006575 —
revive-with-revisions per its own recommendation, retiring the other 4 dark triggers so the lane
has exactly one live gardener, and mind the wall-clock-expiry caveat. Without it, the boxed
artifact (which gates prod deploy) keeps shipping unvalidated.

**S6 — Durable publish + signing (medium).** Answer `TARBOAT-PUBLISH.md`'s open question (GitLab
Generic Package vs Release asset), wire the `bundle-airgap`/`build-tarboat` output to that
destination on tag pipelines. gpg signing is now wired ON by default (ka-0000008173:
`TARBOAT_GPG_KEYID` + `TARBOAT_SIGNING_KEY` referenced + key imported before assembly); the
remaining step is the operator provisioning the two `gardenio-main` vault secrets (GPG keypair +
published public key). A friend must be able to fetch + verify.

**S7 — Build MVC M3 (the OSS essence; long-lived).** ka-0000003598 — the redaction/seed
mechanism (`seed-config-dump.sh` + deny-list guard) so the tarball ships only the honest minimal
config/advice subset. Human-in-the-loop by design (epic ka-0000000011); the M1 inventory is the
starting point.

---

## Open bead map (as of 2026-08-02)

| Bead | Type | Status | What it is |
|---|---|---|---|
| ka-0000002628 | task | in_progress | THIS gap-analysis refresh |
| ka-0000004567 | task | open | **T3 headline proof run** (the release gate; S3) |
| ka-0000005158 | task | open | Provision EC2 + drive the 4-gate proof (overlaps S3) |
| ka-0000007142 | task | open | "merge OAuth MR !4106 + drive proof" — MR **already merged**; this bead is now stale, close/rescope |
| ka-0000006575 | decision | open | Revive the dark regression-gardener lane (S5) |
| ka-0000003645 | epic | open | test-in-the-box regression suite (long-lived; S4) |
| ka-0000007759 | epic | open | T2 inference-driven setup (parent of the proof run) |
| ka-0000000011 | epic | open | MVC config subset (long-lived; S7) |
| ka-0000003598 | task | open | **MVC M3 seed mechanism** (not built; S7) |

**Note for the next agent:** ka-0000007142's own comment confirms MR !4106 is merged and the
stale blocker closed — it should be closed or rescoped to "drive the proof run" so it stops
reading as blocked build work. The two proof-run beads (ka-0000004567, ka-0000005158) overlap
and should be de-duped into one.

---

Operator: Matt Baker
