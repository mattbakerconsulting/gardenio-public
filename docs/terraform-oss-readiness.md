# gardenio Terraform — OSS-readiness evaluation & next steps

**Status:** evaluation (2026-06-05). Tracking bead: `ka-0000002014`.
**Trigger:** Slack request from Matt Baker — *"evaluate the next steps on our
gardenio terraform stack; I'd like to prepare it for OSS release; perhaps a
setup guide?"*

This doc evaluates the current state of the gardenio Terraform stack against an
OSS-release bar, and proposes a concrete, ordered backlog. The companion
[`SETUP.md`](archive/terraform-eks-poc-setup.md) (archived with the EKS stack) is the from-zero adopter guide
this evaluation recommended.

---

## 1. What exists today

Two Terraform trees live in the repo:

| Tree | State | Verdict |
|---|---|---|
| `gardenio/terraform/` | **Live, as-built.** ~3k lines of real resource bodies across 15 modules. `composition/poc/` has been applied repeatedly against a real AWS account; the chart pin is bumped continuously by the currency gardener. | **This is the canonical stack.** Ship this. |
| ~~`terraform/customer-poc-aws-poc/`~~ | **Deleted.** Was an abandoned D-0 scaffold: networking / eks / platform-addons modules were empty stubs; only stateful + dns had bodies, and even those were only ever plan-tested against a local backend — never applied (backend `key` lines were commented out, so it owned no live state). | **Removed** so there is exactly one canonical tree. |

The canonical stack (`gardenio/terraform/`) is genuinely complete: VPC, EKS +
IRSA, Karpenter, RDS/S3/KMS/Secrets-Manager, the full platform-addon set,
Route53 + ACM DNS-01, AWS Verified Access edge auth, and the gardenio Helm
release — all real, all proven by repeated applies.

**So the headline is good news:** the stack is not a skeleton, it works, and
it's exercised daily. OSS-readiness is therefore mostly about *documentation,
genericization, and removing a confusing duplicate* — not about building
missing infrastructure.

---

## 2. OSS-release blockers & gaps

Ordered roughly by impact on a first-time external adopter.

### B1 — Stale "skeleton" README *(FIXED in this MR)*
`gardenio/terraform/README.md` said *"Skeleton — no real resources… Do not
consume yet."* That is flatly wrong now and would scare off any reader.
Rewritten to reflect as-built reality + point at the setup guide.

### B2 — No adopter-facing setup guide *(FIXED in this MR)*
There was an excellent *as-built runbook* (`plans/customer-poc-aws-poc.md`)
and a deep research-doc corpus, but nothing oriented at an external adopter
starting from an empty account. Added [`SETUP.md`](archive/terraform-eks-poc-setup.md) (archived with the EKS stack).

### B3 — Duplicate / abandoned tree *(DONE — deleted)*
`terraform/customer-poc-aws-poc/` has been deleted (operator decision
ka-0000005060 → `delete-stub`). It was a separate tier-per-state-file scaffold
whose networking/eks/platform-addons modules were empty stubs; its
stateful+dns modules were only ever plan-tested against a local backend (their
`backend.tf` `key` lines were commented out — never applied, owns no live
state). All live stateful resources (`gardenio-sessions-aws-poc` S3 bucket, RDS,
KMS CMKs, Secrets Manager) are owned by the canonical tree's
`composition/poc/stateful.tf`. There is now exactly one tree.

### B4 — Hardcoded gardenio-specific values
Real resource defaults are baked for gardenio's own environment. None are
*secrets*, but all leak org-specifics and need to be parameterized or clearly
flagged:
- `parent_zone_name = "gardenio.acme.example"`, `env_subdomain = "aws-poc"`.
- Account ID `123456789012` in comments, the `bootstrap` `gardenio:account`
  tag, and a `composition/poc/main.tf` comment.
- The manual `acme.example` apex NS-delegation runbook references a specific apex
  zone ID in account `456789012345`.
- Image/chart registry hardwired to
  `<registry>/gardenio/`.
- `modules/edge-auth` hardwires AWS Verified Access + `gitlab.com` as the
  OIDC IdP. Fine as gardenio's choice, but an adopter needs it documented as
  optional/swappable (the SETUP guide now does this).

### B5 — No published images / chart
The chart and images are in a private GitLab registry. True OSS consumption
needs either a public mirror or clear "bring your own registry" instructions.
SETUP §6 documents the override path; publishing publicly is a separate
decision.

### B6 — Backend config is implicit
`backend "s3" {}` is empty by design (config passed at `init`). Correct, but
undocumented for newcomers — now covered in SETUP §3.

### B7 — `composition/canonical-prod/` is a stale parallel root
It predates `poc/` and is a narrower composition. Either fold it into the
documented path or mark it clearly superseded to avoid a third "which root?"
question.

### B8 — No CI validation of the TF tree
There's no `terraform fmt -check` / `validate` gate in CI for these dirs, so
drift (like the stale README) goes unnoticed. A lightweight RWX/GitLab job
running `init -backend=false && validate` per root would catch a lot.

---

## 3. What is NOT a blocker

- **The modules themselves.** They're well-structured, tagged consistently,
  and follow good hygiene (no backends in modules, one state per root,
  exec-plugin auth). This is the strong part.
- **Secrets in git.** Scanned — no committed `*.tfvars` (only `.example`),
  no plaintext credentials, secrets are SM shells with `ignore_changes`.
  Clean.
- **The research corpus.** ~50 `docs/research/canonical-*.md` docs back every
  design decision. Great provenance; just not adopter-facing (don't ship them
  as the front door).

---

## 4. Recommended next steps (ordered)

| # | Step | Owner / gate | Done in this MR? |
|---|---|---|---|
| 1 | Fix stale README; add SETUP.md; add this evaluation | thread agent | ✅ |
| 2 | **Delete** `terraform/customer-poc-aws-poc/` (abandoned stubs) | operator approved (decision ka-0000005060 → delete-stub) | ✅ |
| 3 | Parameterize remaining hardcoded defaults (B4): make `parent_zone_name`, registry path, account-tag first-class vars with neutral example values | follow-up task | ❌ |
| 4 | Add a CI `terraform validate`/`fmt -check` gate for the TF roots (B8) | follow-up task | ❌ |
| 5 | Decide image/chart publication story for OSS (B5) | operator decision | ❌ |
| 6 | Resolve `composition/canonical-prod/` — fold or mark superseded (B7) | follow-up task | ❌ |
| 7 | Optional: extract a registry-neutral "examples/" minimal root | nice-to-have | ❌ |

Steps 3 (ka-0000000614), 4 (ka-0000002920), and 6 are tracked follow-up beads.
Step 2 is done (this change). Step 5 still wants an operator call.

---

## 5. Bottom line

The stack is **closer to OSS-ready than its own README suggests** — the
infrastructure is real and battle-tested. The work left is almost entirely
documentation + genericization + removing one confusing duplicate tree, not
new Terraform. This MR clears the two highest-impact items (accurate README +
a real setup guide) and lays out the rest as a tracked backlog.
