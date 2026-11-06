# Canonical-prod gardenio cluster — Round 2 planning refinement

**Status:** plan-of-record
**Scope:** refinement pass over the 12 child epics of umbrella **ka-0000005972** (canonical production gardenio cluster + reusable Terraform module).
**Predecessor:** Round 1 structure locked in decision **ka-0000000374** (12 epics + 43 research/design tasks, no-implementation guardrail per advice ka-0000003177).
**Tracking bead:** ka-0000003355.
**Mode:** research + scoping only. No AWS resources, no new state backends, no `feat/canonical-prod/*` branches in `site-deployment` until a human green-lights each step via `gb decision`.

Closes: summarizes (a) which epics have committed design docs, (b) which still need a survey/design pass and in what order, (c) open decisions that block downstream epics.

---

## (a) Epics with committed design docs

Five committed artifacts currently live under `docs/research/` (and will continue to live there; `gardenio/plans/` is reserved for plan-of-record docs like this one).

| Epic | ID | Committed doc(s) | Closing bead | Notes |
|---|---|---|---|---|
| 2 — TF module architecture | ka-0000005315 | `docs/research/canonical-prod-tf-module-architecture.md` (MR !1308) | ka-0000003215 | Prior-art survey only. Three Epic-2 forks deferred into sibling tasks by decision ka-0000005769 (resolved `merge_close`). |
| 10 — Migration / cutover | ka-0000001529 | `docs/research/canonical-prod-live-dependency-inventory.md` (MR !1402) | ka-0000000843 | 8 inbound + 12 outbound edges with owner + cutover method. 6 open questions flagged — see §(c) `ka-0000002168`. |
| 11 — Cost + FinOps | ka-0000007484 | `docs/research/canonical-prod-cost-estimate.md` | ka-0000005372 | Refreshed from original ka-0000005845 estimate with canonical-prod assumptions. Delta-vs-generic table + FinOps plan still open. |
| 12 — Batteries-included | ka-0000007371 | `docs/research/canonical-prod-batteries-included.md`, `docs/research/canonical-prod-operator-placement-matrix.md` | ka-0000007446, ka-0000001278, ka-0000003112, ka-0000003430 | Four of five children closed: tenant-system inventory, operator placement matrix, multi-tenant story, greenfield consumer checklist. Portable chart proposal (ka-0000003395) remains open. |

**Interpretation.** Epic 12 is the most mature child: four of five tasks closed and two committed docs. Epic 2 has a prior-art doc but no architectural ADR yet (the three forks from the survey were pushed into siblings). Epics 10 and 11 each have one committed doc and outstanding follow-ups.

## (b) Epics still requiring a survey/design pass

Ordered by dependency depth: foundation first, consumers last. Within each tier epics can progress in parallel, but later tiers should not start drafting until their prerequisites land (or are explicitly unblocked by decision).

### Tier 1 — Foundation (unblock everything downstream)

Pick these up first. Everything else quietly assumes these decisions are made.

1. **Epic 1 — AWS account & Organizations baseline (ka-0000001315).** 3/3 tasks open: org-structure inventory (ka-0000003641), account-provisioning runbook (ka-0000001214), tagging + naming policy (ka-0000000951). The inventory task almost certainly needs read-only AWS access (canonical-prod role has none by default) — escalate via `gb decision` before shelling out. Output: `docs/research/canonical-prod-aws-account-baseline.md`.
2. **Epic 2 completion (ka-0000005315).** 3/4 remaining: module directory skeleton (ka-0000007733), state backend + versioning + test tooling (ka-0000003471), module I/O contract (ka-0000002543). The three forks deferred from the prior-art survey (cross-repo module versioning, test depth, TF version floor) must resolve inside these tasks — flag them as explicit decisions, not prose.
3. **Epic 3 — Networking foundation (ka-0000008154).** 3/3 open: CIDR + AZ plan (ka-0000000283), NAT + VPC endpoints cost-benefit (ka-0000002147), DNS + ingress edge plan (ka-0000000441). Gates EKS subnetting and ingress cutover design.

### Tier 2 — Cluster core

Pre-req: Tier 1. Stateful services rely on networking; EKS IAM depends on org baseline.

4. **Epic 4 — EKS cluster design (ka-0000005565).** 4/4 open: control-plane + node groups (ka-0000006546), Karpenter vs Cluster Autoscaler (ka-0000001568), IRSA vs Pod Identity + role inventory (ka-0000000573), addon pinning strategy (ka-0000001559).
5. **Epic 5 — Stateful services (ka-0000008037).** 4/4 open: RDS engine/class/HA (ka-0000001656), S3 + KMS layout (ka-0000002303), Secrets Manager inventory + path convention (ka-0000001947), **beads DB schema + pgvector migration dry-run (ka-0000004215)**. Beads-DB is load-bearing per canonical-prod core rule 5 — treat it as highest-risk item in this tier.

### Tier 3 — Bootstrap, identity, observability

Pre-req: Tier 2. These tasks produce per-workload contracts that assume an EKS cluster and stateful backends.

6. **Epic 6 — Platform addons / cluster bootstrap (ka-0000001447).** 3/3 open: addon install order + ownership (ka-0000001545), IRSA policies per addon (ka-0000001736), NetworkPolicy + storage class baseline (ka-0000002150). Keep addon defaults portable — acme-specific knobs belong in the site-deployment caller (core rule 3).
7. **Epic 7 — Identity (ka-0000007440).** Keycloak placement (ka-0000002096) was **resolved on 2026-05-21 by operator decree on ka-0000006859**: Keycloak is **not** in the canonical stack; canonical edge-auth = **GitLab AWS VPN SSO validated connection** (production approach). See `docs/research/customer-poc-edge-auth-canonical.md`. Remaining open: Slack app cutover (ka-0000006653), GitLab OAuth + webhook cutover (ka-0000006074). Feeds Epic 10 directly — keep cutover sequencing compatible with the live-dependency inventory.
8. **Epic 8 — Observability (ka-0000007067).** 3/3 open: log pipeline (ka-0000005391), metrics + dashboards (ka-0000003865), baseline alerting catalog (ka-0000006737). Mezmo cross-account ingest is the likely fork.

### Tier 4 — Invocation, cutover, cost, greenfield finish

Pre-req: Tiers 1–3. These epics stitch the module, the caller, and the migration together.

9. **Epic 9 — site-deployment invocation + apply pipeline (ka-0000005269).** 4/4 open: caller layout survey (ka-0000001703), per-env directory skeleton (ka-0000007778), apply pipeline + plan review (ka-0000003917), secret-injection strategy for TF (ka-0000001934). Study existing `site-deployment/terraform/live/<env>/common/services/` + `cicd-templates` TF component before inventing conventions (core rule 6).
10. **Epic 10 remaining (ka-0000001529).** 3/4 open: staged migration phase runbook (ka-0000002179), DNS + TLS cutover choreography (ka-0000005105), beads DB cutover design (ka-0000007634). Unblocked by the committed inventory, but will need answers to the 6 open questions first (§c).
11. **Epic 11 remaining (ka-0000007484).** 2/3 open: FinOps plan draft (ka-0000008238), side-by-side delta vs generic estimate (ka-0000004726). Both need Tier 1/2 decisions (NAT HA, RDS class, node-group shape) to produce meaningful numbers.
12. **Epic 12 finish (ka-0000007371).** 1/5 open: portable `gardenio-tenant-system` chart proposal (ka-0000003395). Depends on the operator placement matrix (already landed) and Epic 6 addon decisions.

## (c) Open decisions that block downstream epics

Only decisions currently *blocking* the research path are listed — informational decisions and other roles' decisions are out of scope.

| Decision ID | Topic | Blocks | Recommended action |
|---|---|---|---|
| **ka-0000002168** | Epic 10 live-dep inventory: operator questions (Slack app reuse vs fork; GitLab webhook dual-subscribe window; `203.0.113.5/32` allow-list source; `gardenio-sessions` S3 size; ~~Keycloak issuer URL strategy~~ — moot per **ka-0000006859** decree (Keycloak removed from canonical stack); LiteLLM egress allow-list). | Epic 10 sibling tasks ka-0000002179 (phase runbook), ka-0000005105 (DNS/TLS), ka-0000007634 (DB cutover). Also Epic 7 identity cutovers. | Operator picks `answer-q` (preferred) or `fill-gaps`. Without answers, runbook prose will stall in `[operator to confirm]` placeholders. |

### Decisions that Round-2 design tasks *will* surface (not yet filed)

These are forks the Round 1 plan deferred; each should become a `gb decision` at the moment its owning task picks an option, so options stay visible instead of quietly chosen:

- **Epic 2 — cross-repo module versioning** (git tag vs OCI-registry consumption). Owning task: ka-0000003471.
- **Epic 2 — test depth** (validate + tflint only vs terratest with LocalStack vs full apply-in-sandbox). Owning task: ka-0000003471.
- **Epic 2 — Terraform / provider version floor**. Owning task: ka-0000003471.
- **Epic 4 — Karpenter vs Cluster Autoscaler**. Owning task: ka-0000001568.
- **Epic 4 — IRSA vs EKS Pod Identity** for the role inventory. Owning task: ka-0000000573.
- **Epic 5 — RDS Multi-AZ vs Aurora Postgres** (cost/HA tradeoff, interacts with beads-DB pgvector availability). Owning task: ka-0000001656.
- ~~**Epic 7 — Keycloak placement** (in-cluster vs separate hosted realm vs reuse america-e2e instance). Owning task: ka-0000002096.~~ **Resolved 2026-05-21 by operator decree on ka-0000006859**: Keycloak is removed from the canonical stack; canonical edge-auth = GitLab AWS VPN SSO validated connection. See `docs/research/customer-poc-edge-auth-canonical.md`.
- **Epic 9 — apply pipeline runner** (GitLab CI reusing `cicd-templates` TF component vs RWX). Owning task: ka-0000003917.
- **Epic 10 — Beads DB move mechanism** (logical replication vs snapshot+restore vs dump/load window). Owning task: ka-0000007634. Highest-risk single decision in the cutover; pgvector + schema + live data must all land intact within a bounded outage window (core rule 5).

### Access-shaped blockers

- **No AWS credentials in the canonical-prod role by default.** Epic 1's inventory task (ka-0000003641) will need to file a `gb decision` offering the operator options: (a) paste `aws orgs list-accounts` output, (b) scope short-lived read-only creds, (c) proceed from documentation alone. Do not shell out expecting creds (advice memory).

## Round 2 focus (this pass's recommendation)

If spawning child agents from this plan, prioritize in this order and keep concurrency ≤ 4 (canonical-prod core rule 10):

1. Resolve **ka-0000002168** with the operator so Epic 10 can resume runbook authoring.
2. Spawn Tier-1 survey/design agents for **Epic 1** and **Epic 3** in parallel (independent of Epic 2's skeleton work).
3. Drive **Epic 2** to committed skeleton + state-backend ADR; file the three deferred decisions explicitly when the owning task decides them.
4. Only after Tier 1 is substantially committed should Tier 2 (Epics 4 & 5) start — EKS and RDS designs branch off networking + org baselines.

No implementation. Each closure lands a committed doc (`docs/research/canonical-prod-<topic>.md`) + bead comment linking the MR.

## References

- Umbrella: ka-0000005972
- Round 1 structure lock: decision ka-0000000374
- No-implementation advice: ka-0000003177
- Prior-art doc (Epic 2): `docs/research/canonical-prod-tf-module-architecture.md` (MR !1308)
- Live-dep inventory (Epic 10): `docs/research/canonical-prod-live-dependency-inventory.md` (MR !1402)
- Cost estimate (Epic 11): `docs/research/canonical-prod-cost-estimate.md`
- Batteries-included (Epic 12): `docs/research/canonical-prod-batteries-included.md`, `docs/research/canonical-prod-operator-placement-matrix.md`
- Adjacent (not a replacement): gardenio-next root epic ka-0000005356
