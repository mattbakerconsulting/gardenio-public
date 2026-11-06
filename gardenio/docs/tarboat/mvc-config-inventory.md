# Tarboat MVC — Config/Prompt Inventory (M1, first pass)

> **Epic:** [ka-0000000011](https://gardenio.e2e.example.com/beads/ka-0000000011) — Minimum-viable config/prompt subset (the essence of the OSS release).
> **Task:** M1 inventory. **Operator:** Matt Baker (oidc:matthew.baker@acme.example).
> **Status:** first-pass agent recommendation. The `keep/drop/needs-human` column is a *starting point* for the
> M2 human-stakeholder pruning pass — not a final ruling.

## Why this exists

Production gardenio carries a huge amount of configuration and prompting. A friend's air-gapped box must inherit
only the **minimal honest subset** that keeps a boxed gardenio useful — never the ACME-internal firehose (Jira
tokens, perf bastion, RDS roles, monorepo-dark, Mezmo, internal hostnames). This table enumerates every
config/prompt artifact family prod seeds, with a first-pass `keep / drop / needs-human` recommendation per row.

The table feeds two downstream deliverables:
- **M2 (human pruning):** stakeholders lock keep/drop/redact per row. Long-lived; rows revisited as the box evolves.
- **M3 (seed mechanism + redaction guard):** installs ONLY the kept subset at bootstrap, with a deny-list guard
  (mirrors the gardenio-next per-env deny-list pattern, advice ka-0000007027).

## Legend

- **keep** — generic, useful on a friend's box as-is.
- **keep-redacted** — useful shape, but body names ACME-internal specifics that must be stripped/genericized.
- **drop** — ACME-internal or meaningless off-box; never ship.
- **needs-human** — genuine judgment call for M2.

---

## Family 1 — Advice beads (`type=advice`)

In-DB advice in this environment (20 rows). Note: a running prod session *also* injects role/global advice at
session start; those are the same beads, surfaced by label matching (advice ka-0000006660).

| id | scope | first-pass | why |
|---|---|---|---|
| ka-0000002418 | global | **keep** | "Link every MR to its bead with gb mr link" — generic beads hygiene. |
| ka-0000002435 | global | **keep** | tl;dr wrap-ups should describe real work — generic. |
| ka-0000006660 | global | **keep** | How advice label matching works — needed if friend authors advice. |
| ka-0000006688 | role:gardener | **keep-redacted** | CDQR rubric for quality-driver gardeners. Q1 resolved: v1 runs gardeners → keep, strip ACME examples. |
| ka-0000005707 | gardenio+gardener | **keep-redacted** | Opportunistic gardener wake-condition recipes. Q1 resolved → keep, genericize. |
| ka-0000001467 | gardenio+gardener | **keep-redacted** | Gardener wake-condition recipes. Q1 resolved → keep, genericize. |
| ka-0000000974 | project:gardenio | **keep-redacted** | "Every gardener MUST self-block" — good safety rule; strip ACME-specific examples. |
| ka-0000002635 | project:gardenio | **keep-redacted** | Gardener auto-block sentinel — generic safety; verify no internal refs. |
| ka-0000002866 | gardenio+gardener | **keep-redacted** | "Clear small blockers before escalating" — generic; check examples. |
| ka-0000007463 | project:gardenio | **needs-human** | dynamic-workflow vs pod-fanout — advanced orchestration; maybe drop for v1. |
| ka-0000003508 | project:gardenio | **keep-redacted** | "Operator isn't coming to save you" — good agent-autonomy framing; genericize away ACME AWS/repo refs. |
| ka-0000005439 | role:thread | **drop** | deploy-mr e2e preview env / DEVOPS-677 — ACME GitLab specific. |
| ka-0000004810 | project:gardenio | **drop** | resumer-watchdog gardener prompt — ACME-internal gardener. |
| ka-0000002875 | project:gardenio | **drop** | monorepo-dark gardener prompt — ACME-internal. |
| ka-0000003745 | monorepo+fsm-triage | **drop** | PE-epic gardener screenshot mandate — ACME PE workflow. |
| ka-0000004130 | project:monorepo | **drop** | monorepo coding conventions — not shipped. |
| ka-0000007974 | project:monorepo | **drop** | no-unnecessary-comments — monorepo-scoped. |
| ka-0000006428 | project:monorepo | **drop** | verify fix in customer PROD tenant — ACME customer ops. |
| ka-0000007878 | project:monorepo | **drop** | MR description scoring — monorepo-scoped. |
| ka-0000006856 | project:monorepo | **drop** | test/syntax-check stub — noise. |

**Plus (session-injected, ACME-internal — all drop):** customer-poc AWS creds (ka-0000007102), perf bastion+RDS
(ka-0000001600), Mezmo/observability (ka-0000001893), gardenio-next mirror (ka-0000007027, ka-0000003152),
Keycloak admin (ka-0000000349), acme-helm-chart deploy (ka-0000008182, ka-0000007064), RWX gotchas
(ka-0000003503, ka-0000000227, …), Jira write path (ka-0000001317). These name internal hosts/accounts/tokens
and must never ship.

**First-pass tally:** keep 3 · keep-redacted ~5 · needs-human ~4 · drop the rest (the large majority).

---

## Family 2 — Role-context / role nudges

| role | first-pass | why |
|---|---|---|
| thread | **keep-redacted** | The friend's primary interaction shape (Slack-or-equivalent thread agent). Strip Slack-bridge/ACME specifics. |
| crew | **keep-redacted** | Useful for multi-step work. Slim. |
| gardener | **keep-redacted** | Q1 RESOLVED (Matt, 2026-06-11): v1 boxes DO run autonomous gardeners. Keep the role-context; strip ACME-specific scheduling/escalation refs. |
| jira-mention | **drop** | ACME Jira integration. |
| triage / fsm-triage | **drop** | ACME intake workflow. |

---

## Family 3 — Custom bead types + allowed prefixes

| item | first-pass | why |
|---|---|---|
| core types (epic, task, bug, feature, decision, advice, schedule, trigger, account) | **keep** | The beads workflow itself. |
| ACME-specific custom types (if any) | **needs-human** | Enumerate against prod config; drop internal-only. |
| allowed prefixes | **keep-redacted** | Keep the bead-id prefix scheme; drop ACME-project-specific prefixes. |

---

## Family 4 — Decision templates

0 decision-template beads in this environment. **Q3 RESOLVED (Matt, 2026-06-11; enumeration run in the prod
gardenio namespace, DB `gardenio-beads.gardenio.svc`):** prod gardenio stores **zero** decision-template beads —
scanned `type=decision-template` (0), all 20 `type=config` beads (none define templates), and
`kd config get decision_templates` (empty). The decision *artifact_type* vocabulary
(`plan`/`report`/`checklist`/`diff-summary`/`epic`/`bug`/`tldr`) is **hardcoded in the `gb decision` binary**, not
DB-seeded config. So: nothing ACME-specific to redact, nothing to drop. **keep** the binary-baked enum (ships with
`gb` in the tarball); **seed nothing** in the M3 dump for this family. Re-run this enumeration before each tarboat
release in case a future env adds DB templates. (Decision ka-0000004062, checklist artifact ka-0000005101.)

---

## Family 5 — Skills surface (the `Skill` tool list)

| skill | first-pass | why |
|---|---|---|
| rwx:* (migrate, review, rwx) | **drop** | ACME CI is RWX+GitLab; not in a friend's box. |
| jira | **drop** | ACME Jira. |
| mezmo | **drop** | ACME observability. |
| schedule / loop | **keep** | Q1 resolved: v1 runs gardeners, which need scheduling. Keep. |
| deep-research, video, verify, code-review, simplify | **keep** | Generic dev/agent skills, useful anywhere. |
| update-config, keybindings-help, init, run | **keep** | Generic Claude Code harness skills. |

---

## Family 6 — The agent system prompt

The agent system prompt carries ACME endpoints, Slack/Jira/Mezmo MCP instructions, the beads workflow, and the
session-close protocol. **first-pass: keep-redacted** — a friend-grade variant must keep the beads workflow +
session-close + decision protocol, and strip: ACME hostnames (`*.e2e.dev.acme.example`), Slack bridge specifics, Jira
bridge, Mezmo MCP, the customer-poc AWS account, monorepo references, and internal SM secret names.

---

## The M3 seed mechanism (Q4 RESOLVED) + redaction deny-list

**Q4 RESOLVED (Matt, 2026-06-11; decision ka-0000002935, plan artifact ka-0000001836):** the box restores a
**pre-baked, already-redacted beads DB dump** wholesale at first boot (fastest install). The dump is a *generated*
artifact, not hand-authored: a release-time pipeline EXTRACTS only keep/keep-redacted config families → REDACTS via
the deny-list below → a HARD GUARD re-scan fails the build on any leaked internal ref → emits a reviewable
`seed-manifest.tsv` + `seed-redaction-report.md` (mitigates the "opaque blob" downside) → packages
`gardenio-seed-<calver>.dump` under `tarboat/seed/`. Install via a new `scripts/inbox/seed-config-dump.sh` (sibling
to `seed-bundle.sh`), idempotent, `--airgap-strict` hard-fails on a missing/failed dump. M3 children: M3.1 build
pipeline, M3.2 install+bootstrap wiring, M3.3 assemble + validate posture check. M3 build is blocked until Q2
(skill set) resolves; Q3 is already resolved (seed nothing — templates are binary-baked).

### Redaction deny-list (the guard)

Anything matching these patterns must be stripped by the M3 guard before seeding a friend's box (the guard re-scans
the dump post-redaction and FAILS the build on any match):

- Internal hostnames: `*.e2e.dev.acme.example`, `acme.atlassian.net`, internal registry URLs.
- AWS account ids / IAM users / role ARNs (e.g. customer-poc 123456789012, gardenio-agents-*).
- Credential/secret names: Jira/Slack tokens, Mezmo, RWX_ACCESS_TOKEN, SM secret paths, Keycloak admin.
- ACME systems by name: monorepo, monorepo-dark, gardenio-next, perf bastion, acme-helm-chart, RDS master.
- ACME workflow vocabulary: DEVOPS-*, PE-epic, customer-PROD-tenant, jira-mention/triage roles.

This mirrors the gardenio-next config-mirror per-env deny-list (advice ka-0000007027) — same defense, different target.

---

## Open questions for M2 (human)

1. ~~Does a friend's box run **autonomous gardeners** in v1, or only interactive thread/crew agents?~~
   **RESOLVED (Matt Baker, 2026-06-11, Slack thread-1781113307): YES — autonomous gardeners are in v1.**
   Consequence for this inventory: the gardener cluster flips from `drop-for-v1` toward **keep-redacted**, not drop.
   Specifically — gardener role-context (Family 2) → **keep-redacted**; the gardener self-block safety advice
   (ka-0000000974, ka-0000002635) → **keep-redacted** (they were already); the `schedule`/`loop` skills (Family 5)
   → **keep**; and the gardener wake-condition recipe advice (ka-0000006688, ka-0000005707, ka-0000001467) move from
   `needs-human` toward **keep-redacted** (a v1 box runs gardeners, so it needs the recipes — strip ACME examples).
   The ACME-internal *named* gardeners (resumer-watchdog ka-0000004810, monorepo-dark ka-0000002875) stay **drop**.
2. What is the **minimal useful skill set** a friend expects out of the box? — *decision filed, see below.*
3. Is there a prod **decision-template** set to classify (none in this env)? — *decision filed, see below.*
4. Where does the seeded subset physically live in the tarball, and does M3 seed it at bootstrap (like the
   in-box account-bead seeding in `scripts/inbox/`) or ship it as a pre-baked beads dump? — *decision filed, see below.*

> Q2/Q3/Q4 are now live operator decisions (filed from Slack thread-1781113307). Their resolutions land back here and
> become the locked rulings that M3 (the seed mechanism + redaction guard) implements against.
