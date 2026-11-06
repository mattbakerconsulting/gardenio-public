# MR proof packets — claim-matched evidence in MR descriptions

**Task:** ka-0000004403 (W2, plan ka-0000001183) · **Template:** [`.gitlab/merge_request_templates/Default.md`](../../.gitlab/merge_request_templates/Default.md) · **Gate:** `scripts/lint-mr-proof.sh` (CI job `lint-mr-proof`)

## The norm

Green CI proves the change compiles and its internal assertions hold. It does
**not** prove the user-visible claim: a viewer change backed only by unit tests
sails through CI without anyone ever having seen the pixels. The proof packet
closes that gap — every gardenio MR description states:

1. **Claim** — the user/operator-visible behavior this MR delivers, one sentence.
2. **Surface** — where that behavior lives (routes the evidence expectation).
3. **Evidence** — links/output that exercise *the claim itself*, not just
   internal assertions.
4. **Not established** — what the evidence does NOT prove (untested paths,
   environments, scales). Naming the boundary is what makes the evidence honest.

The block lives between `<!-- PROOF:BEGIN -->` / `<!-- PROOF:END -->` markers
so it is machine-checkable. Keep it near the top of the description — GitLab
truncates the CI snapshot of long descriptions (~2700 chars).

In CI the lint reads the **untruncated** description from the MR API
(`GET /projects/:id/merge_requests/:iid`), so truncation normally cannot hide any
part of the packet; the job log names the source it used. When that read is
unavailable (no token, no `curl`/`jq`, API error) the lint falls back to the
truncated `CI_MERGE_REQUEST_DESCRIPTION` and **fails closed** rather than
skipping (ka-0000001603 — the old skip left the gate silently unenforced). A
failed read is never silent: the log names each credential it tried and the HTTP
code it got back, e.g.
`MR API description read failed: GITLAB_CLONE_TOKEN→HTTP 401, CI_JOB_TOKEN→HTTP 401.`
The two truncation-driven failures are:

- Block entirely below the cut → `no PROOF block found`, with truncation named as
  the likely cause. Move the block up rather than re-verifying a block that is
  genuinely there (ka-0000004000).
- `PROOF:BEGIN` visible but `PROOF:END` past the cut → **partial validation**. A
  required field that is not visible before the cut counts as missing and fails
  the job; the shape heuristics a mid-value cut could false-fail (Surface enum,
  per-surface evidence match) soften to warnings, and the pass is labelled
  `PARTIAL validation`. The remedy is the same: move the block to the top.

## Evidence by surface

| Surface | Claim shape | Evidence that matches | Evidence that does NOT |
|---|---|---|---|
| `viewer-ui` | a page/element renders or behaves differently | screenshot of the change (`gb image upload <png>` → proxied URL), before/after pair, Playwright artifact | "unit tests pass", "the Go template compiles" |
| `pipeline-ci` | a pipeline/job behaves differently | link to a pipeline/job/CI run where the new behavior is visible | "the YAML parses", "rules read correctly" |
| `daemon-api` | an endpoint/CLI returns something different | before/after query or command output (fenced block), reproducible command | handler unit test alone (fine as *support*, not as the proof) |
| `agent-behavior` | agents act differently (hooks, prompts, triggers, advice) | session/bead link showing an agent exercising the new path, transcript excerpt | "the prompt text says so" |
| `chart-infra` | deployed topology/config changes | `helm template`/`terraform plan` output (fenced) or a deploy run link | "helm lint passes" |
| `docs-tooling` | humans/agents read or run something new | often the diff itself — say so explicitly; for scripts, the test-suite run | — |

Uploading evidence: `gb image upload <file>` pushes to the whitelisted S3
bucket and prints the viewer-proxied URL (the only human-openable link — the
raw bucket is private). Paste that URL into the Evidence field.

## What the lint enforces vs. what the reviewer judges

`scripts/lint-mr-proof.sh` enforces the **mechanical** part on every MR
pipeline: block present, all four fields filled with non-placeholder values,
Surface is a known value, and the two unambiguous shape checks (`viewer-ui`
evidence contains an image/screenshot link; `pipeline-ci` evidence contains a
run link).

A field's value is the **whole bullet**, not just its first line: the
`- **Evidence:**` line plus every continuation line under it — blank lines,
indented prose, nested sub-bullets, fenced before/after output — all count
toward both the filled-in check and the per-surface shape check. So an
Evidence bullet may open with a sentence of framing and put its run links,
transcripts and comparisons below; there is no need to cram a URL into the
lead sentence. The value ends at the next `- **<Field>:**` bullet, so evidence
that lives in a *sibling* field does not satisfy Evidence (ka-0000006457).

The **semantic** part — does the evidence actually exercise the claim? — is
the reviewing/merging agent's job. Gardenio agents self-merge (ka-0000002230),
so whoever merges is the reviewer of record. Before merging any gardenio MR,
check:

- [ ] Does the Evidence show the *Claim*, or only something adjacent to it?
      (Claim says "plan cards render a grid"; a screenshot of a different page
      doesn't count.)
- [ ] Is the evidence from *this* branch/build, not from main or an older run?
- [ ] Is **Not established** honest — does it name the obvious untested
      dimension (mobile, prod scale, concurrent writes), or is it a throwaway?
- [ ] If the MR is `Proof-Exempt`, is the reason real? (Docs typos yes;
      "small change" no — small changes have claims too.)

A claim/evidence mismatch is a reason to fix the packet (capture the missing
evidence), not to merge with a note.

## Off-ramps (all audited)

- **Draft MRs** (`Draft:`/`WIP:` title) — skipped; the packet is required
  before marking ready.
- **Trickle MRs** (real `<!-- PROVENANCE:BEGIN -->` markers) — carry their own
  receipt, enforced by `scripts/lint-trickle-mr.sh`.
- **`Proof-Exempt: <reason>`** — a single line replacing the block, for MRs
  where a packet genuinely does not apply. Exemptions stay visible in the
  description and are counted by the KPI pass; a reason is mandatory.

## KPI

**Post-merge regression beads per 100 merged MRs.** A regression bead is a
`type=bug` whose description/comments attribute the defect to a previously
merged MR (the "worked before MR X" shape). Measurement pass (periodic, e.g.
the CED/groomer loop):

- numerator: bugs filed in the window that reference a merged MR as cause
  (search: `kd search "regression" --type=bug` + MR-URL mentions, then confirm
  attribution by reading);
- denominator: MRs merged in the window (`gb mr list` / GitLab API);
- also track: `Proof-Exempt` rate (exemptions per 100 MRs) — if it climbs,
  the norm is being routed around, not satisfied.

Baseline starts at the merge of this norm; the plan's retirement condition
(ka-0000001183) asks for one fresh before/after comparison.

## Rollout notes

- The gate applies to MR pipelines run after this lands. In-flight MRs go red
  on their next pipeline until their description gains a PROOF block (30
  seconds of work) or a `Proof-Exempt` line.
- GitLab only auto-applies `Default.md` to MRs created in the web UI. Agents
  creating MRs via `glab`/API must include the block in the description they
  pass — the advice bead for this norm carries a copy-paste skeleton.
