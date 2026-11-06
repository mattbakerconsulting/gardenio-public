# Advice Hygiene & Labelling Contract

`gb prime` injects every matching advice bead into every agent session. Bloat here steals
context from the code the agent is trying to reason about. This doc is the gardenio house
rule for **how to write** and **how to target** advice beads (`kd list --type advice`).

Enforced at runtime by the delivery pipeline (`internal/advice/`) and guarded in CI by the
real-catalog budget tests (`cmd/gb/real_catalog_budget_test.go`): `TestPrimeOutputSizeBudget`,
`TestRealCatalogPerArchetypeBudget`, `TestRealCatalogBroadcastCount`.

## Contents

- [Hygiene](#hygiene)
- [Labelling contract](#labelling-contract)

## Hygiene

### 1. Write at the length the advice actually needs

Every matching bead is injected into every session, so length has a real cost — but an
artificially-clipped rule that strips context to hit a char budget is worse than one that
takes an extra sentence to be actually actionable. No novellas, no truncated one-liners.

The first line of the description is the hook that surfaces in every session, so lead with
the rule or decision. The rest is only loaded when an agent runs `gb advice show <id>`, so
that's the right place for rationale, edge cases, and examples.

If an advice bead has grown into a doc — a reference guide, a long runbook, a multi-section
tutorial — write the doc (e.g. under `gardenio/docs/`) and replace the bead body with a
pointer: `see docs/<file>.md`.

The actual ceiling is runtime, enforced unconditionally in CI: `TestPrimeOutputSizeBudget`
fails if rendered `gb prime` output gets oversized, and `TestRealCatalogPerArchetypeBudget`
fails if the matched advice pool for any archetype busts its per-archetype token budget
against a snapshot of the real catalog. Those tests are the enforcement mechanism, not a
hand-rolled character limit.

Because those budget tests audit a **frozen fixture**
(`cmd/gb/testdata/advice_catalog_snapshot.json`) so CI stays hermetic, they are blind to
live corpus growth between refreshes. `TestRealCatalogSnapshotFreshness` bounds that drift:
a **soft** ceiling (7d) and a **hard** ceiling (14d) past which CI/Test reds on *every*
gardenio MR fleet-wide (ka-0000004418). The soft ceiling's `t.Logf` is invisible in a passing
test, so the observable early warning lives in a **scheduled** GitLab alarm job
(`advice-snapshot-freshness-alarm`, `scripts/check-advice-snapshot-freshness.py`) that
reddens a full week before the hard cliff (ka-0000004917). When you see that alarm — or the
hard test — refresh the fixture:

```sh
cd gardenio/controller && go test ./cmd/gb -run TestRealCatalog -update-advice-snapshot
```

(needs `BEADS_HTTP_ADDR` + `BEADS_AUTH_TOKEN`), then commit the regenerated `testdata/`. A
refresh that reddens the budget test is revealing **pre-existing** corpus drift
(ka-0000006923), not a regression you introduced — trim the corpus (`gb advice audit`) or
move the debt register, do not re-freeze a stale fixture to dodge the red.

### 2. Reserve `inline:true` for policy-critical, irreversible rules

The `inline:true` label forces full-text rendering and bypasses the byte budget. Use it
only when missing the rule would cause real, hard-to-reverse damage (credential leaks,
production writes to the wrong system, release-process violations). Workflow tips,
convenience patterns, and "how I did X last time" notes are NOT inline candidates.

### 3. Close instead of accumulating

Advice beads are not a knowledge base. Close any bead that is:

- **Redundant with CLAUDE.md** (root or component) — the agent already reads those.
- **A one-off how-to** (avatar setup, a specific debugging session) — document once, close the bead.
- **A historical ADR** (why we chose X over Y) — belongs in a commit message or doc.
- **A stale adoption plan / research note** — gets out of date fast, not session-relevant.
- **Superseded** by a newer bead, tool, or command.

Prefer `--ttl` at creation for time-bounded advice. Cycle summaries, release-window notes,
temporary workarounds, and "until MR X lands" hints should expire automatically
(`--ttl 7d` or `--expires-at 2026-05-01T00:00:00Z`) instead of relying on a human to close them.

When in doubt, close. A rediscovered truth re-surfaced as a fresh advice bead is cheaper
than thousands of sessions reading a stale one.

### 4. Prefer global pointers over per-project duplicates

If a piece of advice is useful across projects, use the `global` label and make sure the
content is project-agnostic. Do not duplicate global advice as `project:<name>` beads.

### 5. Review triggers

Audit `gb advice list -l project:gardenio` (and `gb advice audit` / `gb advice lint`) when:

- Adding a new advice bead (check for duplicates / overlap first).
- The prime size budget test (`TestPrimeOutputSizeBudget`) or the real-catalog guards
  (`TestRealCatalogPerArchetypeBudget`, `TestRealCatalogBroadcastCount`) fail.
- During any "shrink prime output" / advice-consolidation epic (see ka-0000008080 for the
  original reference pass and ka-0000003733 for the context-crisis cut).

## Labelling contract

Advice reaches an agent by **label matching**, not free text (full mechanics live in advice
bead ka-0000006660 — `gb advice show ka-0000006660` — and `gb advice --help`). This is the
gardenio house rule for *how to target* a bead so it lands on the right agents and nowhere
else. A bead mislabelled per this contract either reaches nobody or bloats every session,
and both regress the budget.

### Targeting labels (these decide who receives the advice)

| Label | Reaches | Matching rule |
|---|---|---|
| `global` | every agent, every project | reserve for **truly fleet-wide** rules |
| `project:<name>` | agents in that project | HARD requirement (all `project:` labels must match) |
| `role:<role>` | agents with that role | OR-matched among `role:` labels |
| `agent:<bead-id>` | exactly one agent bead | HARD requirement — scope a single gardener's playbook here, not `role:gardener` |

A **bare/unprefixed** label (`gardener-prompt`, `needs-intake`, `my-thing`) is **inert for
targeting** — it never scopes delivery, it is only a human-findable tag. `gN:` group
prefixes compound targeting: same group number → AND, different group numbers → OR
(`g0:project:gardenio, g0:role:crew` = gardenio AND crew).

### House rules

1. **`role:` is required unless you opt into a project broadcast.** A `project:<name>` bead
   with no `role:`, no `agent:`, and no `global` label is a *project broadcast* — it reaches
   **every** agent in the project regardless of role. Since Phase 3 of the context-crisis
   cut (ka-0000003733), such beads are **denied delivery** unless they carry the explicit
   `scope:project-broadcast` opt-in. Default to `role:` targeting; reach for the opt-in only
   when the rule genuinely applies to every role, and keep those beads short.

2. **`global` is reserved for truly fleet-wide rules.** If a rule only matters to one
   project, label it `project:<name>` (plus `role:`), not `global`. Global advice must be
   project-agnostic in its content, not just its label.

   **Never combine `global` with `role:` on the same bead — they contradict.** `global`
   means "every agent, every role"; a `role:` label means "only these roles". The delivery
   matcher (`MatchesSubscriptions`) has a role gate that vetoes any bead carrying `role:`
   labels the agent does not match — and it fires *before* the `global` OR-group, so the
   role set silently **wins**: the `global` label becomes inert and the bead reaches only
   the listed roles, never the whole fleet. Pick one: drop `global` if the bead is
   role-scoped, or drop the `role:` labels if it is truly fleet-wide. `gb advice lint`
   flags this shape as the `global-role-clash` **error** (added for ka-0000004354, where a
   `global, role:gardener, role:crew, role:jira-mention` bead never reached its load-bearing
   `role:thread` consumer — the fix was to drop the inert `global` and add `role:thread`).

3. **Canonical over duplicate.** If advice is useful across projects, write it **once** as
   `global` (see Hygiene rule 4) — do not fan it out into per-project copies. Per-project
   duplicates of a global rule are pure budget waste and drift out of sync.

4. **TTL for time-bounded advice.** Cycle summaries, release-window notes, "until MR X
   lands" hints, and temporary workarounds should expire on their own (`--ttl 7d` /
   `--expires-at …`) instead of accumulating (see Hygiene rule 3).

Check targeting with `gb advice lint` (flags denied broadcasts, dead/no-targeting beads,
junk `project:kd-…` labels, and `global`+`role:` contradictions) and per-archetype cost
with `gb advice audit` before adding or relabelling advice.
