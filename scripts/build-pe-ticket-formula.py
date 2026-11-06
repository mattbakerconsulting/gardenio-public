#!/usr/bin/env python3
"""Generate gardenio/docs/pe-ticket-to-completion-formula.json.

This formula EXTENDS the golden bug-fix formula (ka-0000001189) and adds the two
things Matt named that the golden formula lacks: explicit adversarial CRITIC gates
and a per-ticket GARDENER that drives the loop to completion.

Authoring notes (why this is shaped the way it is):

* `extends` merge is WHOLESALE per step ID: kbeads `mergeSteps`
  (kbeads/cmd/kd/formula_apply.go:1388) replaces a parent step with the child
  step that shares its ID — it does NOT field-merge descriptions. So a step we
  override only to re-point `depends_on` LOSES the inherited golden body. We
  therefore restate those three steps minimally and point the agent back at the
  golden source step (`kd formula dump ka-0000001189`) for the full body, rather
  than copy-pasting the golden prose (which would rot).

* New step IDs are appended; same IDs override. The critic/gardener steps below
  are all new except e2e-test / draft-mr / tag-reviewer, which are re-pointed.
"""
import json

GOLDEN = "ka-0000001189"

# ---------------------------------------------------------------------------
# Re-pointed golden steps (same id => wholesale override). Body is intentionally
# a short pointer back to the golden source step + the single thing that changed
# (its dependency), so the critic sits in the chain without us copy-pasting the
# golden prose.
# ---------------------------------------------------------------------------

def inherited_pointer(step_id, new_dep, why):
    return (
        f"## (inherited from golden formula {GOLDEN}, dependency re-pointed)\n\n"
        f"This step's full instructions are unchanged from the golden Bug-Fix-MR formula. "
        f"Run the body of the **`{step_id}`** step exactly as defined there:\n\n"
        f"```bash\nkd formula dump {GOLDEN}\n```\n\n"
        f"The ONLY change in the PE-Ticket-to-Completion variant is the gate that "
        f"precedes it: this step now waits on **`{new_dep}`** instead of its original "
        f"predecessor.\n\n> Why: {why}\n"
    )

e2e_test = {
    "id": "e2e-test",
    "type": "task",
    "title": "{{TICKET}}: Write E2E test that captures the defect",
    "labels": ["project:monorepo"],
    "depends_on": ["fix-critic"],
    "description": inherited_pointer(
        "e2e-test",
        "fix-critic",
        "we do not invest in writing/regression-proofing a test until a critic has "
        "confirmed the diff actually fixes the ticket and is narrow in scope.",
    ),
}

draft_mr = {
    "id": "draft-mr",
    "type": "task",
    "title": "{{TICKET}}: Push branch and create draft MR",
    "labels": ["project:monorepo"],
    "depends_on": ["test-evidence-critic"],
    "description": inherited_pointer(
        "draft-mr",
        "test-evidence-critic",
        "we do not push a branch / open an MR until a critic has confirmed there is "
        "RED-before / GREEN-after proof the test genuinely captures the defect.",
    ),
}

tag_reviewer = {
    "id": "tag-reviewer",
    "type": "task",
    "title": "{{TICKET}}: Find and tag the best reviewer for this MR",
    "labels": ["project:monorepo"],
    "depends_on": ["gardener-signoff"],
    "description": inherited_pointer(
        "tag-reviewer",
        "gardener-signoff",
        "we do not pull a human reviewer in until the gardener has signed off that all "
        "three critics scored >=8 AND CI is green. gardener-signoff sits after "
        "mr-description-critic, so the description rubric is transitively satisfied too.",
    ),
}

# ---------------------------------------------------------------------------
# CRITIC GATES (new). Each is a measure -> judge -> bounce critic modelled on the
# CDQR audit judge/file-issues prose (ka-0000004730): an explicit 0-10 rubric, a
# written verdict, a >=8 threshold, and a concrete bounce-back action on <8.
# ---------------------------------------------------------------------------

CRITIC_CONTRACT = (
    "## How to run a critic gate\n\n"
    "You are an **adversarial reviewer**, not the author. Your default posture is "
    "skepticism: assume the work is *not* good enough until the evidence forces you to "
    "score it >=8. Produce a written verdict, score it 0-10 against the rubric below, "
    "and act on the threshold:\n\n"
    "1. **Measure** — gather the objective signals the rubric names (the diff, the test "
    "output, the MR body). Do not score from memory; read the actual artifact.\n"
    "2. **Judge** — assign a 0-10 score with a one-paragraph written verdict that cites "
    "specific evidence (file:line, a quoted assertion, a screenshot URL). Bands: "
    "0-3 broken, 4-5 weak, 6-7 borderline, 8-9 good, 10 excellent.\n"
    "3. **Bounce or pass** — record the verdict as a comment on THIS step bead, then:\n"
    "   - **score >= 8** → PASS. Comment `CRITIC PASS <n>/10` + verdict, close this step, "
    "let the chain proceed.\n"
    "   - **score < 8** → DO NOT PROCEED. Comment `CRITIC BOUNCE <n>/10` + the written "
    "verdict naming exactly what must change, then reopen/return to the named bounce "
    "target step. The downstream steps stay blocked until the bounce target is redone "
    "and this critic re-scores >=8.\n"
)

fix_critic = {
    "id": "fix-critic",
    "type": "task",
    "title": "{{TICKET}}: CRITIC — does the diff actually fix it, and is the scope narrow?",
    "labels": ["project:monorepo", "kind:critic", "critic:fix"],
    "depends_on": ["investigate-fix"],
    "description": CRITIC_CONTRACT + (
        "\n## Critic 1 of 3: Fix critic\n\n"
        "Adversarially review the committed diff for **{{TICKET}}** on branch "
        "`{{BRANCH}}`. Two independent questions, both must hold:\n\n"
        "### A. Does it actually fix the ticket?\n"
        "- Re-read the JIRA ticket's *reported behaviour* (`issue/{{TICKET}}/details.md`).\n"
        "- Trace the diff: does the changed code sit on the exact path that produces the "
        "bug? Could the bug still reproduce after this change (wrong layer, masked symptom, "
        "happy-path-only)?\n"
        "- Is the root cause addressed, or just a symptom papered over?\n\n"
        "### B. Is the scope NARROW?\n"
        "- `git diff --stat origin/main...HEAD` — every changed file must be justified by "
        "the ticket. Flag drive-by refactors, reformatting churn, unrelated files, "
        "dependency bumps, or 'while I was in here' edits.\n"
        "- A correct fix buried in unrelated changes still scores low — narrow scope is a "
        "first-class requirement, not a nicety.\n\n"
        "### Rubric (0-10)\n"
        "| Band | Meaning |\n"
        "|---|---|\n"
        "| 0-3 | Does not fix the ticket, OR fixes it but touches many unrelated files. |\n"
        "| 4-5 | Plausibly fixes it but root cause is unproven, or scope has avoidable creep. |\n"
        "| 6-7 | Fixes the reported behaviour; one or two unjustified hunks remain. |\n"
        "| 8-9 | Clearly fixes the root cause; every changed line is ticket-justified. |\n"
        "| 10  | Minimal, surgical, obviously-correct fix; zero collateral. |\n\n"
        "### Bounce target\n"
        "**If score < 8: bounce to `investigate-fix`.** Write a verdict naming the gap "
        "(\"still reproduces because X\" / \"files A,B,C are unrelated — revert them\") and "
        "do NOT let `e2e-test` start. The author re-does `investigate-fix`; you re-score."
    ),
}

test_evidence_critic = {
    "id": "test-evidence-critic",
    "type": "task",
    "title": "{{TICKET}}: CRITIC — RED-before / GREEN-after proof the test captures the defect",
    "labels": ["project:monorepo", "kind:critic", "critic:test-evidence"],
    "depends_on": ["e2e-test"],
    "description": CRITIC_CONTRACT + (
        "\n## Critic 2 of 3: Test-evidence critic\n\n"
        "Adversarially verify that the test written in `e2e-test` genuinely captures the "
        "**{{TICKET}}** defect — i.e. it is a real regression guard, not a test that would "
        "pass with or without the fix.\n\n"
        "### What you must see (the bisection proof)\n"
        "- **RED before:** captured output (terminal screenshot or `/tmp/*-failure.txt`) "
        "showing the SAME test FAILING on pre-fix code, with an assertion error that names "
        "the buggy value/state.\n"
        "- **GREEN after:** captured output showing the SAME test (same file + same test "
        "name) PASSING on post-fix code.\n"
        "- **Assertion specificity:** at least one assertion targets the specific behaviour "
        "the ticket reported — not a generic `assert no error` / `assert 200`.\n\n"
        "### Adversarial checks\n"
        "- Could this test pass on UNFIXED code? If the assertion is too loose, it proves "
        "nothing — score <= 4.\n"
        "- Are the failure and success outputs from the *same* test name? A different test "
        "in each => not a bisection proof => score <= 3.\n"
        "- If the step legitimately SKIPPED tests (pure-CSS bug, no e2e coverage), the skip "
        "justification must be documented; a skip with no justification scores <= 4.\n\n"
        "### Rubric (0-10)\n"
        "| Band | Meaning |\n"
        "|---|---|\n"
        "| 0-3 | No RED/GREEN evidence, or failure & success are different tests. |\n"
        "| 4-5 | Evidence exists but assertion is loose / could pass unfixed / skip unjustified. |\n"
        "| 6-7 | Real bisection proof; assertion specificity is thin. |\n"
        "| 8-9 | Clear RED->GREEN on the same test; assertion pins the exact defect. |\n"
        "| 10  | Above, plus the test is well-placed and would catch a future regression. |\n\n"
        "### Bounce target\n"
        "**If score < 8: bounce to `e2e-test`.** Write a verdict naming what is missing "
        "(\"no pre-fix failure capture\" / \"assertion only checks status 200\") and do NOT "
        "let `draft-mr` start. The author re-does `e2e-test`; you re-score."
    ),
}

mr_description_critic = {
    "id": "mr-description-critic",
    "type": "task",
    "title": "{{TICKET}}: CRITIC — MR description rubric (root cause, fix, evidence, screenshots, scope, ticket)",
    "labels": ["project:monorepo", "kind:critic", "critic:mr-description"],
    "depends_on": ["mark-mr-ready"],
    "description": CRITIC_CONTRACT + (
        "\n## Critic 3 of 3: MR-description critic\n\n"
        "Adversarially review the finalized MR description for **{{TICKET}}** "
        "(MR !{{MR_IID}}). A reviewer who never saw the ticket should be able to understand "
        "the bug, the fix, and the proof from the description alone.\n\n"
        "### Six required elements — each is 0, 1, or 2 points (max 12, normalise to /10)\n"
        "1. **Root cause** — names the actual cause with a code reference (file:line), not "
        "just the symptom.\n"
        "2. **Fix summary** — 2+ sentences explaining what changed and why it resolves the "
        "root cause.\n"
        "3. **Test evidence** — the RED-before / GREEN-after outputs (or collapsible logs) "
        "from the test-evidence step are present and render.\n"
        "4. **Before/after screenshots** — both render inline (use the upload `url` field, "
        "not `full_path`) and are apples-to-apples.\n"
        "5. **Narrow-scope statement** — an explicit sentence asserting the change is "
        "scoped to the ticket, with the changed-file list.\n"
        "6. **Linked ticket** — the JIRA {{TICKET}} link is present and the ticket was "
        "commented with the MR link.\n\n"
        "Score = round(sum/12 * 10). Any element scoring 0 caps the overall at <= 6 "
        "regardless of the others (a missing required element is a fail, not an average).\n\n"
        "### Bounce target\n"
        "**If score < 8: bounce to `mark-mr-ready`.** Write a verdict listing each element "
        "and its 0/1/2 score plus what to add, and do NOT let `gardener-signoff` proceed. "
        "The author re-does `mark-mr-ready`; you re-score."
    ),
}

# ---------------------------------------------------------------------------
# GARDENER (new). gardener-setup creates a per-ticket gardener schedule bead scoped
# to this molecule's subtree (modelled on ephemeral-gardener ka-0000007901), with
# the run_budget + progress-aware + bead_query stop spec advice ka-0000000974
# requires. gardener-signoff is the gate where the gardener confirms all 3 critics
# are green and CI is green before a human reviewer is tagged.
# ---------------------------------------------------------------------------

gardener_setup = {
    "id": "gardener-setup",
    "type": "task",
    "title": "{{TICKET}}: Stand up the per-ticket gardener (schedule bead, enabled=false)",
    "labels": ["project:monorepo", "project:gardenio", "pattern:gardener"],
    "depends_on": ["check-existing-work"],
    "description": (
        "## Gardener setup — drive {{TICKET}} to a merge-ready, critic-green MR\n\n"
        "Create a per-ticket **gardener schedule bead** scoped to THIS molecule's subtree. "
        "The gardener wakes on a cadence, triages the molecule's open children, dispatches/"
        "nudges work, and drives the loop until {{TICKET}} reaches a merge-ready MR with all "
        "three critics green. Modelled on ephemeral-gardener `ka-0000007901`; satisfies the "
        "every-gardener-self-blocks rule (advice `ka-0000000974`).\n\n"
        "Runs EARLY (right after `check-existing-work`) so the gardener exists before the fix "
        "chain gets going. Created **disabled** — an operator/the gardener flips it on.\n\n"
        "### 1. Discover this molecule's root\n"
        "This step bead's parent IS the molecule root. Capture it:\n"
        "```bash\n"
        "THIS=<this step bead id>   # shown in your assignment\n"
        "MOLECULE_ROOT=$(kd show \"$THIS\" --json | python3 -c \"import sys,json; "
        "d=json.load(sys.stdin); print(next((x['id'] for x in d.get('depends_on',d.get('dependencies',[])) "
        "if x.get('type')=='parent-child'), ''))\")\n"
        "echo \"molecule root = $MOLECULE_ROOT\"   # fallback: kd mol list / kd show $THIS\n"
        "```\n\n"
        "### 2. Create the gardener schedule bead (enabled=false)\n"
        "```bash\n"
        "kd create \"gardener-pe-{{TICKET}}: drive {{TICKET}} to a critic-green merge-ready MR\" \\\n"
        "  --type=schedule --kind=config --priority=1 \\\n"
        "  --project gardenio -l project:gardenio -l pattern:gardener -l \"gardener-epic:$MOLECULE_ROOT\" \\\n"
        "  --description \"Per-ticket gardener for {{TICKET}}. Mission: drive the molecule rooted at \\\n"
        "$MOLECULE_ROOT to a merge-ready MR (!{{MR_IID}}) with fix-critic, test-evidence-critic and \\\n"
        "mr-description-critic all scored >=8 and CI green. Each cycle: load context (gb prime), triage \\\n"
        "open children of $MOLECULE_ROOT, unblock/nudge or dispatch crew, guard scope creep, check the \\\n"
        "agent budget, run the end-state check, squawk a one-line status, then gb stop.\" \\\n"
        "  -f \"role=gardener-pe-{{TICKET}}\" \\\n"
        "  -f \"cron=23,53 * * * *\" \\\n"
        "  -f \"timezone=America/Los_Angeles\" \\\n"
        "  -f \"enabled=false\" \\\n"
        "  -f \"assign_bead=$MOLECULE_ROOT\" \\\n"
        "  -f \"max_runs=24\" \\\n"
        "  -f \"terminal_goal=Molecule rooted at $MOLECULE_ROOT is closed: MR !{{MR_IID}} merge-ready with all 3 critics >=8 and CI green.\" \\\n"
        "  -f \"prompt=$(cat <<'PROMPT'\n"
        "Each wake-up: (1) gb prime + read the Loop Progress block. (2) kd list open/in_progress/blocked "
        "children of the molecule root; identify the critical-path step. (3) If a critic bounced (<8), make "
        "sure the bounce-target step is being redone; if a step is unowned, dispatch a crew agent on it; if "
        "blocked, resolve or escalate. (4) Guard scope: reject drive-by changes that would fail fix-critic. "
        "(5) End-state check: are all 3 critics >=8 AND is CI green AND is the MR marked ready? If yes, drive "
        "gardener-signoff and let tag-reviewer fire, then offer to self-disable. (6) gb squawk a one-line "
        "status. (7) gb stop with progress_score + progress_summary.\n"
        "PROMPT\n"
        ")\" \\\n"
        "  -f \"triggers=$(cat <<'TRIG'\n"
        "[{\"kind\":\"cron\",\"cron\":\"23,53 * * * *\",\"timezone\":\"America/Los_Angeles\",\n"
        "  \"preflight\":{\"all\":[{\"type\":\"bead_query\",\"op\":\">\",\"value\":\"0\",\n"
        "   \"types\":[\"task\",\"bug\",\"epic\"],\"parent\":\"PLACEHOLDER_MOLECULE_ROOT\",\n"
        "   \"status\":[\"open\",\"in_progress\",\"blocked\"]}]}},\n"
        " {\"kind\":\"bead_event\",\"events\":[\"closed\",\"updated\"],\n"
        "  \"match\":{\"all\":[{\"field\":\"parent\",\"op\":\"==\",\"value\":\"PLACEHOLDER_MOLECULE_ROOT\"}]},\"throttle\":\"60s\"},\n"
        " {\"kind\":\"manual\",\"label\":\"operator-fire\"}]\n"
        "TRIG\n"
        ")\" \\\n"
        "  -f \"stop=$(cat <<'STOP'\n"
        "{\"loop_end\":{\"any\":[\n"
        "  {\"kind\":\"bead_query\",\"op\":\"==\",\"value\":\"0\",\"types\":[\"task\",\"bug\",\"epic\"],\n"
        "   \"parent\":\"PLACEHOLDER_MOLECULE_ROOT\",\"status\":[\"open\",\"in_progress\",\"blocked\"]},\n"
        "  {\"kind\":\"specific_bead_closed\",\"bead_id\":\"PLACEHOLDER_MOLECULE_ROOT\"},\n"
        "  {\"kind\":\"max_runs\"},\n"
        "  {\"kind\":\"no_progress_for\",\"runs\":3},\n"
        "  {\"kind\":\"wall_clock\",\"duration\":\"168h\"}\n"
        "]},\n"
        "\"run_end\":{\"any\":[{\"kind\":\"yielded_then_woken_then_idle\"},{\"kind\":\"no_actionable_state\"},\n"
        "  {\"kind\":\"wall_clock\",\"duration\":\"30m\"}]},\n"
        "\"hard_stop\":{\"any\":[{\"kind\":\"consecutive_failures\",\"value\":5}]}}\n"
        "STOP\n"
        ")\" \\\n"
        "  -f \"progress=$(cat <<'PROG'\n"
        "{\"events\":[{\"kind\":\"child_closed\",\"parent\":\"PLACEHOLDER_MOLECULE_ROOT\"},\n"
        "  {\"kind\":\"crew_dispatched\",\"role\":\"crew\",\"parent\":\"PLACEHOLDER_MOLECULE_ROOT\"},\n"
        "  {\"kind\":\"mr_merged\",\"author_label\":\"gardener-pe-{{TICKET}}\"}],\n"
        "\"windows\":[{\"name\":\"this_run\",\"scope\":\"since_run_start\"},\n"
        "  {\"name\":\"last_3_runs\",\"scope\":\"last_n_runs\",\"n\":3}],\n"
        "\"surface\":{\"in_prime\":true,\"in_squawk_on_loop_end\":true,\"in_wrapup\":true}}\n"
        "PROG\n"
        ")\"\n"
        "```\n"
        "After creation, replace every `PLACEHOLDER_MOLECULE_ROOT` in the stored "
        "`triggers`/`stop`/`progress` fields with the real `$MOLECULE_ROOT` value (the "
        "heredocs above keep them literal so the shell does not mangle the JSON):\n"
        "```bash\n"
        "SCHED=<schedule bead id from kd create>\n"
        "for f in triggers stop progress; do\n"
        "  val=$(kd show \"$SCHED\" --json | python3 -c \"import sys,json;print(json.load(sys.stdin)['fields']['$f'])\" | sed \"s/PLACEHOLDER_MOLECULE_ROOT/$MOLECULE_ROOT/g\")\n"
        "  kd update \"$SCHED\" -f \"$f=$val\"\n"
        "done\n"
        "```\n\n"
        "### 3. Why this stop spec satisfies advice ka-0000000974\n"
        "The gardener self-blocks three ways: **run_budget** (`max_runs=24`), a "
        "**progress-aware** wedge guard (`no_progress_for: {runs:3}`, backed by the "
        "`progress` events above), and a **bead_query loop_end** that closes the loop when "
        "the molecule subtree has zero open/in_progress/blocked children "
        "(`{op:'==', value:'0', parent:<molecule root>}`), plus a 168h wall-clock backstop.\n\n"
        "> NOTE (live-schema caveat, runbook §5.1): the live `type:trigger` config schema "
        "may reject `assign_bead`/`stop`/`progress`/`triggers` keys today. If "
        "`kd create --type=schedule` rejects a field, fold the cadence + budget + end-state "
        "into the gardener `prompt` and create the bare firing shape "
        "(`type=trigger` / `trigger:schedule` / `action=spawn_agent` with the cron preflight "
        "+ `gardener-epic:<root>` label). The intent — self-blocking gardener scoped to the "
        "molecule — is unchanged.\n\n"
        "## Gate\n"
        "- [ ] Molecule root discovered and recorded as a comment on this step\n"
        "- [ ] Gardener schedule bead created with `enabled=false`\n"
        "- [ ] stop spec has all three self-blocks (max_runs + no_progress_for + bead_query loop_end on the molecule root)\n"
        "- [ ] PLACEHOLDER_MOLECULE_ROOT substituted with the real root in triggers/stop/progress\n"
        "- [ ] Schedule bead id recorded as a comment on this step (the gardener-signoff step references it)"
    ),
}

gardener_signoff = {
    "id": "gardener-signoff",
    "type": "task",
    "title": "{{TICKET}}: GARDENER SIGN-OFF — all 3 critics >=8 and CI green before tagging a reviewer",
    "labels": ["project:monorepo", "pattern:gardener", "kind:gate"],
    "depends_on": ["mr-description-critic"],
    "gate": {"type": "manual", "timeout": "72h"},
    "description": (
        "## Gardener sign-off gate\n\n"
        "The per-ticket gardener (created in `gardener-setup`) confirms the ticket is truly "
        "merge-ready before a human reviewer is pulled in. This is a **manual gate**: it "
        "stays blocked until the gardener works the checklist and clears it. `tag-reviewer` "
        "depends on this step, so no reviewer is tagged until sign-off.\n\n"
        "### The gardener MUST verify ALL of the following\n"
        "- [ ] **fix-critic** closed with a recorded `CRITIC PASS` verdict scored **>= 8**.\n"
        "- [ ] **test-evidence-critic** closed with a recorded `CRITIC PASS` verdict scored **>= 8**.\n"
        "- [ ] **mr-description-critic** closed with a recorded `CRITIC PASS` verdict scored **>= 8**.\n"
        "- [ ] **CI is green** on MR !{{MR_IID}} — verify, do not assume:\n"
        "  ```bash\n  glab mr view {{MR_IID}} --json head_pipeline\n  ```\n"
        "  Ignore only the known-flaky jobs the golden formula lists; any real failure blocks sign-off.\n"
        "- [ ] The MR is marked **ready** (not draft).\n\n"
        "### Actions\n"
        "- **All boxes checked** → record a `GARDENER SIGN-OFF` comment on this bead citing the "
        "three critic scores + the green pipeline id, close this step (clearing the manual "
        "gate), and let `tag-reviewer` fire. If the gardener schedule is still enabled, the "
        "gardener should then offer to self-disable (its loop_end will also fire once the "
        "molecule children all close).\n"
        "- **Any box fails** → do NOT clear the gate. Comment what is missing and ensure the "
        "responsible critic has bounced to its target step. Sign-off waits for the redo.\n\n"
        "> A critic that merely *ran* is not a pass — only a recorded score >= 8 counts. "
        "If a critic verdict is missing, treat that critic as failing and block sign-off."
    ),
}

# ---------------------------------------------------------------------------
# Assemble. Order: gardener-setup early; critics + re-points interleaved; signoff
# before tag-reviewer. (Order in the list is cosmetic — deps define execution.)
# ---------------------------------------------------------------------------

formula = {
    "formula_type": "workflow",
    "extends": [GOLDEN],
    "phase": "liquid",
    "description": (
        "PE Ticket to Completion — drive a single PE-XXXX Jira ticket from triage to a "
        "merge-ready MR. Extends the golden Bug-Fix-MR formula and adds what makes it "
        "trustworthy unattended: three adversarial CRITIC gates (the fix actually fixes it "
        "and is narrow; the test has RED-before/GREEN-after proof; the MR description scores "
        ">=8 on root-cause/fix/evidence/screenshots/scope/ticket) plus a per-ticket GARDENER "
        "that drives the loop to completion and signs off that all three critics are green "
        "and CI is green before a human reviewer is tagged. Invoke from Slack: "
        "`/formula pour <id> --var TICKET=PE-XXXX`."
    ),
    "vars": [
        {"name": "TICKET", "required": True,
         "description": "JIRA ticket ID (e.g. PE-7289)"},
        {"name": "BRANCH",
         "description": "Git branch name (e.g. agent/my-agent/fix-pe-7289)"},
        {"name": "MR_IID",
         "description": "GitLab MR number if already created (e.g. 385), or 'TBD' if not yet"},
    ],
    "steps": [
        gardener_setup,
        fix_critic,
        e2e_test,
        test_evidence_critic,
        draft_mr,
        mr_description_critic,
        gardener_signoff,
        tag_reviewer,
    ],
}

OUT = "gardenio/docs/pe-ticket-to-completion-formula.json"
with open(OUT, "w") as fh:
    json.dump(formula, fh, indent=2)
    fh.write("\n")
print(f"wrote {OUT}")
