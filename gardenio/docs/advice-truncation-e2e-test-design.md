# Advice truncation — failure-mode catalog & e2e test design

**Epic:** ka-0000003108 (Research: better e2e coverage for advice truncation)
**Task:** ka-0000007757 (Catalog truncation failure modes and write e2e test design doc)
**Depends on audit:** ka-0000006611 → doc ka-0000006445 (`/bundle/ka-0000000763`)
**Origin:** Slack thread ka-0000005078 — Matt Baker:
> "need better e2e tests to make sure our advice isn't getting truncated; it's
> hurting instruction following and autonomy and it comes up a lot."

This doc is the bridge between the render-path audit and the implementation
task (ka-0000003643). It (1) catalogs the failure modes the byte-budget cap can
introduce, (2) names one e2e scenario per mode with a mechanism sketch, (3) maps
each against current coverage so the implementer only writes what's missing, and
(4) recommends which scenarios belong in shell scripts under
`gardenio/tests/e2e/scripts/` versus claudeless TOMLs under
`gardenio/tests/e2e/claudeless/`.

---

## 0. Current mechanics (re-verified against `main` HEAD, 2026-08-03)

The audit (ka-0000006445) is accurate on the *shape* of the pipeline but its
**budget numbers and pre-budget filters are stale**. Re-read from source before
writing tests. Deltas that change test design:

| Fact | Audit said | Current source (`main` HEAD) |
|---|---|---|
| Main-path abbrev budget | `primeAdviceByteBudget = 4096` | **`60000`** (`prime_advice.go:31`) — raised to ~20k tokens by operator policy ka-0000006677 |
| Main-path title budget | `primeAdviceTitleByteBudget = 3072` | **`20000`** (`prime_advice.go:43`) |
| `4096 / 3072` | main-path defaults | now the **diff-path** defaults: `primeAdviceDiffByteBudget = 4096`, `primeAdviceDiffTitleByteBudget = 3072` (`prime_advice.go:52-53`) |
| Confidence filter | `FilterAndSortByConfidence`, `DefaultConfidenceThreshold = 0.2`, sort by confidence desc | **removed** — `ListAdviceForAgent` (`advice.go:44`) no longer sorts or filters by confidence. Within-group order is now just `ListOpenAdvice` (server) order. **This is central to F1.** |
| Server fetch | (not mentioned) | `ListOpenAdvice` fetches `type=advice status=open` with a hard **`Limit: 500`** (`advice.go:102`). A silent pre-budget truncation surface — see **F10**. |
| Bare-project drop | (not mentioned) | `IsDeniedBroadcast` (`advice.go:59`) drops any advice labelled only `project:X` with no `role:`/`agent:` targeting and no `scope:project-broadcast` opt-in. Relevant to **F6**. |

Live snapshot on this crew pod today: `gb prime --size` →
`total_bytes=34468 advice_bytes=22316 advice_items=38`. So on the **default**
budget the real corpus does *not* truncate — every scenario below must force the
cascade with `GB_PRIME_ADVICE_BUDGET_BYTES` / `GB_PRIME_ADVICE_TITLE_BUDGET_BYTES`
overrides, exactly as `test-advice-budget.sh` already does. **Do not** write a
test that asserts truncation on the default budget; it will pass today and rot
the moment the corpus shrinks.

### The cascade, precisely (`renderAdviceItemsWithLimits`, `prime_advice.go:238`)

Per item, in matched order (groups sorted by `GroupSortKey`: global→project→role→agent):

1. **inline** (`inline:true` label) → `renderInlineAdviceItem`, full text, **bypasses both budgets**.
2. **abbreviated** (`title + ≤500-rune summary + pointer`) → accumulate while
   `regularBuf.Len() + item.Len() ≤ budget` (bytes). First item that would
   exceed spills to overflow.
3. **overflow → title-only** (`**[Header]** Title — gb advice show ka-X`) →
   accumulate while `titleBuf.Len() + item.Len() ≤ titleBudget` (bytes).
4. **remainder → footer**: `... N more advice items not shown (budget exceeded).
   Run gb advice list --for <agent> to see all.`

Key invariants the tests defend:
- **Whole items only.** The cascade compares *whole rendered-item* byte lengths
  and moves entire items between tiers. It never slices a rendered item, so it
  can never cut mid-rune (this constrains F7 — see there).
- **Truncate, don't drop.** Everything either renders (some tier) or is counted
  in the footer. Silent disappearance is always a bug.
- **Render-time only.** Truncation never mutates the store; every displaced bead
  stays retrievable via `gb advice list` / `gb advice show`.

---

## 1. Failure-mode catalog

Each mode: the defect, why it silently hurts autonomy, the observable symptom a
test can assert on, and current coverage. Coverage legend: ✅ covered · 🟡 partial
· ❌ gap.

### F1 — High-priority advice evicted by low-priority advice (ordering bug)

- **Defect:** the order feeding the cascade puts a low-value item ahead of a
  high-value one, so under budget pressure the important item is the one that
  spills to title-only or the footer.
- **Why it bites:** the agent silently loses the instruction that mattered most
  while keeping noise. This is the exact autonomy-regression the epic exists for.
- **Current reality (FIXED — ka-0000008175):** the cross-group axis is still the
  **scope group order** from `GroupSortKey` (global→project→role→agent). *Within*
  a scope group, order is **no longer** the arbitrary `ListOpenAdvice` server
  order: `advice.SortByPriorityRecency` now sorts each group before the budget
  cascade by **(priority asc [P0 first], UpdatedAt desc, CreatedAt desc, ID asc)**.
  The ID tiebreak makes the order total, so an identical corpus always yields an
  identical retained/evicted set regardless of the order it arrived in — the
  intra-group order that used to be unspecified is now a pinned, reproducible
  contract. Applied in both `outputAdvice` (main path) and `outputAdviceDiffItems`
  (role-diff path).
- **Symptom to assert:** (a, cross-group) seed a high-scope (global) and a
  low-scope (agent) item; under a budget that fits exactly one abbreviated item,
  the group whose `GroupSortKey` sorts first renders abbreviated and the other is
  pushed to overflow/footer. (b, intra-group) seed a high-priority recent bead
  and a low-priority stale bead in the *same* group; under a budget that fits one
  abbreviated item, the high-priority recent bead renders abbreviated and the
  low-priority stale one spills to title-only.
- **Coverage:** ✅ intra-group ordering + determinism pinned by
  `advice.TestSortByPriorityRecency_*` (unit, priority/recency/ID axes +
  permutation-invariance) and `main.TestAdvicePriorityRecency_*` (end-to-end
  sort → budget eviction render, incl. determinism across input permutations).

### F2 — Project advice that supersedes a global is itself dropped under budget

- **Defect:** a project advice names `supersedes kd-<global>` in its body; the
  global is correctly removed from the pool, but the *superseder* then spills
  past the budget — so the agent sees **neither** the global nor its override.
- **Why it bites:** worst case of F1 — the deliberate project override vanishes
  and its global was already dropped, so the topic goes dark entirely.
- **Symptom to assert:** seed global G + project P where P supersedes G. Under a
  budget large enough for exactly one abbreviated item: G is absent (superseded)
  **and** P renders (at least title-only) — P must never be evicted while G was
  dropped on its behalf. This is the ka-0000007289 invariant, but *under budget
  pressure* and end-to-end.
- **Coverage:** 🟡 unit-covered (`supersession_test.go:153`
  `TestApplySupersessions_ProjectUnderBudgetCutoff`) and supersession is e2e-covered
  on the **default** budget (`test-advice-supersession.sh` S2), but **no e2e ties
  supersession to budget pressure**. Gap = the combination.

### F3 — Title-only overflow so aggressive the title alone misleads

- **Defect:** an item degraded to title-only loses the summary that carried its
  actual payload; the bare title (e.g. "Release process") reads as informational
  when the body was a hard "never do X" rule.
- **Why it bites:** the agent sees a title, assumes it's low-stakes, never runs
  `gb advice show`, and violates the rule.
- **Symptom to assert:** two-part. (a) **Mechanical** (regression-safe): under
  `budget=1, titleBudget=large`, the item renders as `**[H]** <title> — gb advice
  show <id>` with the summary body absent and **no footer** (already S6(b)). (b)
  **Advisory lint** (quality, not a hard e2e gate): a check that flags advice
  whose title, alone, is imperative-free / non-actionable — belongs to the
  advice-lint family (`internal/advice/lint.go`), not the budget e2e. Recommend
  filing (b) as a separate lint follow-up, not blocking this epic.
- **Coverage:** 🟡 mechanical degradation covered (S6(b) e2e +
  `TestOutputAdvice_OverflowRendersAsTitles`); the "misleading title" *quality*
  dimension is uncovered and should be a lint follow-up.

### F4 — inline-bypass bead silently loses inline status → joins budgeted pool

- **Defect:** a regression in `isInlineAdvice` (label typo, refactor, label
  stripped upstream) makes a policy-critical `inline:true` bead fall into the
  budgeted pool, where it can be truncated away.
- **Why it bites:** inline is reserved for irreversible-action rules ("don't
  write prod without approval"). Losing one is the highest-severity truncation.
- **Symptom to assert:** two directions. (a) **Positive** (already S4): an
  `inline:true` bead renders in **full** even at `budget=1, titleBudget=1`. (b)
  **Negative guard** (gap): the *same* bead **without** the `inline:true` label,
  under the same budget=1, is truncated (footer/title-only) — proving the full
  render in (a) is *caused by* the label, not by the bead being short enough to
  fit anyway. Without (b), S4 can pass for the wrong reason.
- **Coverage:** 🟡 positive covered (S4 e2e + `TestOutputAdvice_InlineBypassesBudget`);
  negative/causal guard is a gap.

### F5 — env-var override (`GB_PRIME_ADVICE_BUDGET_BYTES`) ignored or capped

- **Defect:** the override is not read, is clamped to a floor/ceiling, or
  (subtle) `titleBudget=0` is rejected instead of honored.
- **Why it bites:** every other test *and* operator budget-tuning relies on the
  override actually taking effect; if it silently no-ops, the whole suite tests
  the default budget and the regression is invisible.
- **Symptom to assert:** three points on the curve, differential (bigger budget ⇒
  ≥ bytes rendered): a small override renders strictly less than a huge override
  over the same corpus; and the documented `>= 0` handling of the *title* budget
  (`adviceTitleBudgetBytes` accepts `0`; `adviceBudgetBytes` requires `> 0` and
  falls back to default on `0`/non-numeric) is honored.
- **Coverage:** ✅ unit (`TestAdviceBudgetBytes_DefaultAndOverride`,
  `TestOutputAdvice_BudgetWithEnvOverride`) + e2e differential (S1). One thin
  gap: no test pins the **asymmetry** that abbrev-budget=0 → *default* while
  title-budget=0 → *literal zero*. Worth one added assertion.

### F6 — per-role + per-project label combinatorics fail targeting under budget

- **Defect:** an advice targeted by an AND-group (`g1:role:crew`,
  `g1:project:gardenio`) or multiple OR groups either (a) fails to match a valid
  agent, or (b) matches but, because bare-`project:` beads are now dropped by
  `IsDeniedBroadcast`, silently never enters the pool — then the *targeting* miss
  looks identical to a *budget* drop.
- **Why it bites:** an operator writes correct-looking targeting, the advice
  never reaches the agent, and the failure is indistinguishable from truncation.
- **Symptom to assert:** matched-set vs rendered-set separation. Use
  `gb prime --json` (renders **every matched** bead, **no budget** — audit §1
  path #9) to prove the bead *matched*; then text `gb prime` under tight budget
  to prove it was *budgeted*, not *mis-targeted*. Specifically: (a) a correctly
  AND-targeted `g1:role:X + g1:project:gardenio` advice appears in `--json` for a
  matching agent and is absent for a non-matching agent; (b) a bare `project:X`
  advice (no role/agent, no `scope:project-broadcast`) is **absent from `--json`
  entirely** (dropped by `IsDeniedBroadcast`) — documenting that this is a
  targeting drop, not a budget drop.
- **Coverage:** 🟡 matching combinatorics unit-covered (`matching_test.go`
  AND/OR groups); the **matched-vs-budgeted disambiguation via `--json`** and the
  `IsDeniedBroadcast` drop are uncovered e2e. Gap.

### F7 — byte-vs-rune mismatch (UTF-8 multi-byte truncation mid-rune)

- **Defect:** budget accounting in bytes collides with a summary cap in runes,
  producing invalid UTF-8 (a `�` replacement char / broken sequence) in the
  rendered advice.
- **Current reality:** the cascade moves **whole rendered items** by byte length
  and never slices one, so it **cannot** cut mid-rune. The only rune-boundary
  logic is `adviceExcerpt` (`prime_advice.go:~320`), which caps the summary by
  `[]rune` and cuts on sentence/word boundaries — already rune-safe. So F7 today
  is a **regression guard**, not a live bug: a future change that switches
  `adviceExcerpt` to byte-slicing, or that begins slicing a rendered item to
  "fit" the budget, would introduce it.
- **Why it still matters:** advice increasingly contains emoji/CJK (operator
  names, non-ASCII rule text); a mid-rune cut corrupts the agent's context
  invisibly.
- **Symptom to assert:** seed an advice whose title+body is multi-byte-heavy
  (emoji + CJK) and long enough to force the ≤500-rune summary cap to bite;
  assert the rendered `gb prime` output (a) is **valid UTF-8** end-to-end
  (`iconv -f utf-8 -t utf-8`, or `python3 -c 'sys.stdin.buffer.read().decode("utf-8")'`),
  (b) contains **no** U+FFFD replacement char, and (c) the summary was cut on a
  rune/word boundary (last char is a whole rune, not a partial sequence). Also
  document the **unit mismatch**: `GB_PRIME_ADVICE_SUMMARY_CHARS` is in *runes*
  while `GB_PRIME_ADVICE_BUDGET_BYTES` is in *bytes* — a real footgun for anyone
  tuning them together.
- **Coverage:** ❌ no UTF-8 / rune-boundary test anywhere in the advice paths.

### F8 — footer count / grammar correctness at N=0, N=1, N=large

- **Defect:** off-by-one or singular/plural bug in
  `... %d more advice items not shown`: footer shown when N=0, or the count
  wrong, or "1 items".
- **Why it bites:** a footer that says "0 more" or is absent when items *were*
  dropped is the definition of silent truncation; a wrong count erodes trust in
  the "see all" escape hatch.
- **Symptom to assert:** three exact-boundary renders over a **known** corpus
  size: (N=0) budget fits everything → **no footer** substring at all; (N=1)
  budget fits all-but-one → footer present with the literal count `1`; (N=large)
  budget fits few → footer count == (seeded − rendered), verified by counting
  rendered tagged beads and subtracting. Optionally assert grammar if/when the
  footer is pluralized (today it is always "items" — a test that pins "1 more
  advice items" documents the current wording so a future grammar fix is a
  conscious change, not a surprise).
- **Coverage:** 🟡 N=0 (`TestOutputAdvice_SmallCatalogNoFooter`) and N=large
  (`TestOutputAdvice_BudgetTruncatesAndFooters`, e2e S2) covered; **N=1 exact
  boundary and exact-count arithmetic are not pinned**. Gap.

### F9 — post-cap prime still too large for the downstream consumer (context window)

- **Defect:** even with the advice section in-budget, the *total* prime (identity
  + threads + advice + role-diff + memory preamble) overruns the downstream
  ceiling, so the model silently drops the earliest-injected blocks (which
  include the system-prompt file that carries advice).
- **Why it bites:** advice can be perfectly in-budget locally yet still never
  reach the model because the *aggregate* prime blew the context window — the
  autonomy regression re-appears one layer up. Note the **uncapped role-diff
  path** (`outputAdviceRoleDiff`, audit §1 path #7) is the most likely aggregate
  overrun source and has its own bug bead (ka-0000004298).
- **Symptom to assert:** the **system-prompt path** (audit path #2 — the only
  advice channel that survives compaction) end-to-end:
  `gb prime --system-prompt` → assert total bytes under the enforced ceiling even
  when the advice corpus is large; and assert the role-diff supplement, when it
  fires, does not push the aggregate past the ceiling. This is the surface the
  model actually sees on turn 1.
- **Coverage:** 🟡 total-prime stress unit-covered
  (`TestOutputAdvice_StressTotalPrimeUnder100KB`, `TestPrimeOutputSizeBudget`,
  `TestSystemPromptBudget_WithPrerun`); **no e2e drives `gb prime --system-prompt`
  against a live over-budget corpus and asserts the written file stays under the
  ceiling.** Gap (and this is the highest-value e2e per the epic's framing).

### F10 — (extension, beyond the required minimum) server-side `Limit: 500` silently caps the candidate pool

- **Defect:** `ListOpenAdvice` fetches at most **500** open advice beads
  (`advice.go:102`) *before* matching/supersession/budget. If the global open-advice
  population exceeds 500, some advice never enters the pool at all — a truncation
  that happens **upstream of every budget knob** and is invisible to every
  budget-focused test.
- **Why it bites:** as the advice corpus grows, this becomes a real silent-drop
  surface that no budget tuning can fix, and it fails *closed* (advice missing)
  with zero footer.
- **Symptom to assert:** hard to force in e2e without seeding 500+ beads
  (expensive, pollutes the shared daemon). Recommend a **unit test** on
  `ListOpenAdvice` with a mock daemon returning >500, asserting either
  (a) pagination is used so nothing is dropped, or (b) a diagnostic/warn is
  emitted so the cap is observable. **Flag for the remediation task
  (ka-0000000650)** — this is arguably a design bug, not just a test gap.
- **Coverage:** ❌ nothing. Documented here so the remediation decision
  (ka-0000000650) accounts for it.

---

## 2. Scenario summary table

| Mode | Scenario name | Mechanism (one line) | Layer | Coverage today | Recommend |
|---|---|---|---|---|---|
| F1 | `advice_budget_eviction_order` | seed global + agent items; budget fits 1 abbrev; assert eviction follows `GroupSortKey` order (global kept, agent spilled) | shell (`gb prime` + daemon) | ❌ | **new** |
| F2 | `advice_supersession_under_budget` | global G + project P(supersedes G); tight budget; assert G absent AND P still rendered | shell | 🟡 (unit + default-budget e2e) | **new e2e** |
| F3 | `advice_titleonly_degradation` | budget=1, titleBudget=large; assert title kept, summary dropped, no footer | shell | 🟡 (S6(b)) | keep; lint follow-up for the quality half |
| F4 | `advice_inline_bypass_causal` | inline bead renders full at budget=1 **and** same bead sans label truncates | shell | 🟡 (positive only, S4) | **add negative guard** |
| F5 | `advice_env_override_curve` | small vs huge override differential + abbrev0→default / title0→literal-zero asymmetry | unit + shell | ✅ (mostly) | **add asymmetry assertion** |
| F6 | `advice_targeting_vs_budget_json` | `--json` proves matched-set; text proves budgeted-set; bare-`project:` dropped by `IsDeniedBroadcast` | shell (`--json` + text) | 🟡 (unit matching only) | **new e2e** |
| F7 | `advice_utf8_rune_safety` | multibyte-heavy body forces summary cap; assert valid UTF-8, no U+FFFD, rune-boundary cut | unit **and** shell | ❌ | **new (unit primary, shell secondary)** |
| F8 | `advice_footer_count_boundaries` | N=0 no footer, N=1 exact "1", N=large count == seeded−rendered | shell | 🟡 (N=0, N=large) | **add N=1 + exact-count** |
| F9 | `advice_systemprompt_aggregate_cap` | `gb prime --system-prompt` over large corpus stays under ceiling; role-diff doesn't overrun aggregate | claudeless TOML **+** shell | 🟡 (unit only) | **new e2e (highest value)** |
| F10 | `advice_pool_limit_500` | mock daemon >500 open advice; assert no silent pre-budget drop | unit | ❌ | **flag to remediation ka-0000000650** |

---

## 3. Shell-script vs claudeless-TOML recommendation

**Decision rule.** Choose by *what the test must observe*:

- **Budget cascade math and render shape** — everything from `gb prime` /
  `gb prime --json` / `gb prime --size` against a live daemon with env overrides.
  These need only a daemon + the `gb` binary; a mocked claude session adds cost
  and flake with **zero** signal. → **shell scripts**, extending the existing
  `test-advice-budget.sh` (its harness — seed corpus, differential bytes, footer
  assertions, cleanup, port-forward, shard wiring — is already the right rig).
  **F1, F2, F3, F4, F6, F8** land here.

- **Pure algorithm boundaries** (rune math, >500-pool behavior) — no daemon
  needed, deterministic, cheapest as Go unit tests next to the code. → **unit
  tests** in `cmd/gb/prime_advice_*_test.go` / `internal/advice/*_test.go`.
  **F7** (primary) and **F10** land here; F7 gets a thin shell companion so the
  *rendered* output (not just the function) is proven UTF-8-clean end-to-end.

- **End-to-end "does the truncated form actually reach the model's context?"** —
  this is the epic's core question and the *only* thing that justifies a
  claudeless TOML. The value-add of claudeless over a shell script is that it
  drives the **real coop launch path** that writes `.gb/system-prompt.md` and
  passes it via `--append-system-prompt-file`, then lets a mocked session
  *observe its own system prompt*. → **claudeless TOML** for **F9**: a scenario
  that boots a claudeless agent with a large seeded advice corpus and a tight
  budget, then has the mock session `Bash`-assert that
  `.gb/system-prompt.md` exists, is under the ceiling, and contains the footer
  (proving truncation is what landed in-context, not silent loss). A shell
  companion (`gb prime --system-prompt > f; wc -c f; grep footer f`) covers the
  same bytes more cheaply and should be the **gating** assertion; the claudeless
  TOML is the **integration proof** that the file is actually wired into the
  session.

**Concrete placement:**

| Scenario | File | Kind |
|---|---|---|
| F1, F2, F4-neg, F6, F8 | extend `gardenio/tests/e2e/scripts/test-advice-budget.sh` (Scenarios 7–11) | shell, live daemon |
| F5 asymmetry | add assertion to existing S1/S6 in the same script | shell |
| F7 (rune math) | new `TestAdviceExcerpt_RuneBoundaryMultibyte` in `prime_advice_test.go` | Go unit |
| F7 (rendered UTF-8) | Scenario 12 in `test-advice-budget.sh` | shell, live daemon |
| F9 (aggregate/system-prompt) | Scenario 13 (`gb prime --system-prompt`) in `test-advice-budget.sh` **+** new `advice-systemprompt-truncation.toml` claudeless | shell (gate) + claudeless (integration) |
| F10 (>500 pool) | new `TestListOpenAdvice_PoolLimit` in `internal/advice/advice_test.go`; **also** raise to remediation task ka-0000000650 | Go unit + decision input |

Rationale for leaning shell-heavy: `test-advice-budget.sh` already owns the
seed/override/cleanup/shard machinery and runs against the live daemon the audit
identified as the real surface (paths #1/#2). Claudeless TOMLs are reserved for
the single question a shell script *cannot* answer — "was the truncated system
prompt actually handed to the session?" — so we pay the mocked-session cost
exactly once, for F9.

---

## 4. Implementation notes for ka-0000003643 (the next task)

1. **Never assert truncation on the default budget.** The corpus is 22 KB / 38
   items today against a 60 KB/20 KB budget — nothing truncates. Every scenario
   forces the cascade with env overrides, as the existing script does.
2. **Seed under `agent:<unique>` scope** so scenarios are hermetic against
   ambient prod globals, and **always clean up** (the existing script tracks
   `CREATED_ADVICE`/`CREATED_BEADS` and deletes on exit — reuse it).
3. **Use `--json` to separate "matched" from "budgeted"** (F6). A missing bead in
   text output is ambiguous until `--json` proves it matched.
4. **Wire new shell scenarios into the existing module**, not a new one — they
   share the port-forward and corpus. `run-shard.sh` already runs
   `advice-budget`; adding scenarios needs no new `run_module` line (and avoids a
   shard-isolation-lint change).
5. **Refresh the stale budget constants** in any assertion that hard-codes a
   number; prefer differential/relative assertions (bigger budget ⇒ ≥ bytes) that
   survive future budget re-tuning — the audit's `4096/3072` are already wrong.
6. **F3(quality) and F10 are not e2e budget tests** — file F3-quality as an
   advice-lint follow-up and feed F10 into the remediation decision
   (ka-0000000650) rather than forcing them into `test-advice-budget.sh`.

---

## 5. Recommended new/changed test artifacts (checklist for the implementer)

**Shell (`gardenio/tests/e2e/scripts/test-advice-budget.sh`), new scenarios:**
- [ ] S7 `advice_budget_eviction_order` (F1) — cross-group eviction follows `GroupSortKey`.
- [ ] S8 `advice_supersession_under_budget` (F2) — supersede-then-spill invariant.
- [ ] S9 `advice_inline_bypass_causal` (F4) — negative guard: same bead sans `inline:true` truncates.
- [ ] S10 `advice_targeting_vs_budget_json` (F6) — matched (`--json`) vs budgeted (text) + `IsDeniedBroadcast` drop.
- [ ] S11 `advice_footer_count_boundaries` (F8) — N=1 exact + exact-count arithmetic.
- [ ] S12 `advice_utf8_rune_safety` (F7) — rendered output is valid UTF-8, no U+FFFD.
- [ ] S13 `advice_systemprompt_aggregate_cap` (F9) — `gb prime --system-prompt` under ceiling over big corpus (gating).
- [ ] Add F5 asymmetry assertion to S1/S6 (abbrev0→default vs title0→literal-zero).

**Go unit:**
- [ ] `TestAdviceExcerpt_RuneBoundaryMultibyte` (F7 primary) in `cmd/gb/prime_advice_test.go`.
- [ ] `TestListOpenAdvice_PoolLimit` (F10) in `internal/advice/advice_test.go`.

**Claudeless (`gardenio/tests/e2e/claudeless/`):**
- [ ] `advice-systemprompt-truncation.toml` (F9 integration) — boot mock agent, assert
      `.gb/system-prompt.md` exists, under ceiling, carries the footer.

**Follow-ups to file (not part of this epic's e2e):**
- [ ] Advice-lint rule for F3-quality (title-alone-is-misleading).
- [ ] Feed F10 (`Limit: 500` silent pre-budget cap) into remediation task ka-0000000650.
- [ ] Note the uncapped `outputAdviceRoleDiff` aggregate risk (existing bug ka-0000004298) as an F9 sub-case.
