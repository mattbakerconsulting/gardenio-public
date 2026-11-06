# LOL Public-Repo Sync Runbook

**Bead:** ka-0000007574 · **Parent epic:** ka-0000006835
**Pairs with:** [lol-provisioning-minimal-port.md](../../docs/archive/rwx/docs/gardenio-docs/lol-provisioning-minimal-port.md) (archived) ·
[lol-release-port.md](../../docs/archive/rwx/docs/gardenio-docs/lol-release-port.md) (archived) · `scripts/sanitize/README.md`

How to keep the **public LOL gardenio repo** in step with the **internal gardenio
monorepo** without ever leaking internal references. The public repo is a
**sanitized projection** of internal `main`, re-derived by replaying the sanitize
pipeline — it is **not** a plain `git push` mirror and must never be one.

The public repo also carries **fresh history**: it never receives internal
`main`'s ~10k commits. Operator decision `ka-0000005246` chose the
**squash-at-export** model — an explicit, tooling-enforced step
(`scripts/sanitize/squash-export.sh`) collapses the sanitized tree onto a
zero-parent orphan commit and *proves* it shares no ancestry with internal `main`
before anything is pushed. Internal `main` is never rewritten.

## Why a sanitize step (not a raw mirror)

Internal `main` carries internal hosts, registry paths, GitLab/Jira/Slack ids, AWS
account ids / IAM ARNs / IPs / EC2 instance ids, operator PII, and ACME-specific
CLAUDE.md/advice/formula prose. A raw mirror would publish all of it. The
`scripts/sanitize/` pipeline removes every catalogued internal reference while keeping
the tree buildable, and gates the result behind a regex check, an adversarial
LLM-judge sweep, and **one** human merge decision. The sync cadence is just **re-running
that pipeline** whenever internal `main` advances.

## Direction & invariants

- **Direction:** internal `gitlab.com/example-org/gardenio` (`main`) → public LOL
  repo. **One-way.** Public-repo changes (external PRs) are cherry-picked back into
  internal `main` by hand; they are never force-synced over internal history.
- **Never** `git push` internal `main` straight to the public remote.
- **Never** push the *sanitized branch itself* to the public remote — it still
  carries internal `main`'s full commit history. Publish only the **squash-export
  ref** (`refs/heads/export/<version>`), which is fresh history by construction.
- **Never** auto-merge sanitized output — the single `gb decision` human gate is
  mandatory (`run-sanitize.sh decide`).
- Both gate halves (regex `no-internal-strings.sh` **and** LLM-judge
  `verify-pass.workflow.js` → `clean:true`) must be green before the decision is filed.

## Each sync cycle

Run from a Claude Code session in an up-to-date internal checkout (the workflow steps are
Claude Code dynamic workflows — the prompt must contain the word "workflow", or use
ultracode). `scripts/sanitize/run-sanitize.sh plan` prints the authoritative ordered
runbook; the steps below summarize it.

```sh
# 0. Fresh base — branch the sanitized output off up-to-date internal main.
git fetch origin && git checkout main && git pull --ff-only origin main
git checkout -b sanitize/<YYYY.DDD.N>            # sanitized branch for this cycle

# 1. Refresh the inventory IF the tree changed shape since the last cycle
#    (new files/areas, new internal-string categories). Otherwise reuse the
#    canonical inventory bead (ka-0000007121 / bundle ka-0000000494).
#    Launch: Workflow({ scriptPath: "scripts/sanitize/inventory-pass.workflow.js" })
#    then:   python3 scripts/sanitize/synthesize-inventory.py <out.json> '<baseline.json>' INVENTORY.md
#    Export the inventory Markdown the authors read:
#      kd show <inventory-doc-id> --json | python3 -c '...content...' > /tmp/INVENTORY.md

# 2. Propose redaction ops (per-area authors + adversarial per-area judge):
#    Workflow({ scriptPath: "scripts/sanitize/patch-pass.workflow.js",
#               args: { repoRoot: "<repo>", inventoryMd: "/tmp/INVENTORY.md" } })
#    -> save result JSON to /tmp/patch-pass.json

# 3-4. Apply ops, stage, run the deterministic regex gate:
scripts/sanitize/run-sanitize.sh apply /tmp/patch-pass.json     # --dry-run to preview first

# 5. Adversarial LLM-judge re-read of the STAGED diff (second gate half):
#    Workflow({ scriptPath: "scripts/sanitize/verify-pass.workflow.js",
#               args: { repoRoot: "<repo>" } })
#    -> save result JSON to /tmp/verify.json ; require result.clean == true

# 6. ONLY if regex gate exited 0 AND verify result.clean == true:
#    file the SINGLE human merge decision (do NOT push the sanitized branch — see step 7):
scripts/sanitize/run-sanitize.sh decide \
    --verify-result /tmp/verify.json --branch sanitize/<YYYY.DDD.N> --mr <public-mr-url>
#    -> files ONE gb decision (merge vs. another round) and yields. A human authorizes
#       the public merge. Nothing auto-merges.

# 7. Squash-at-export — turn the sanitized tree into FRESH public history. ONLY
#    after the merge decision is approved. This is the enforced "fresh history"
#    step: a zero-parent orphan commit, proven to share no ancestry with internal
#    main, so the ~10k internal commits never ship.
scripts/sanitize/run-sanitize.sh export \
    --from sanitize/<YYYY.DDD.N> --version <YYYY.DDD.N> \
    --public-remote <public-remote-or-url> [--onto export/<prior-version>]
#    -> writes refs/heads/export/<YYYY.DDD.N>, proves the no-internal-ancestry
#       invariants, and PRINTS the exact push command. It NEVER pushes.
#
#    Then publish the fresh history (only the export ref, never the sanitized branch):
git push --force-with-lease <public-remote-or-url> \
    refs/heads/export/<YYYY.DDD.N>:refs/heads/main
```

**Single-commit vs. per-release model.** Omit `--onto` and each export is one
fresh orphan commit (the public `main` is a single squashed snapshot — the
strongest "no internal history" guarantee). Pass `--onto export/<prior-version>`
to chain onto the previous export tip, giving the public repo a short
**per-release** linear history (one clean commit per public release) that *still*
carries zero internal commits — `squash-export.sh` verifies the parent itself has
no internal ancestry. Choose per-release if external contributors need to see
release boundaries; choose single-commit for the simplest possible public log.

`manual` ops (things the applier refuses to auto-apply) and `drop-file` non-grep
modalities (the root `controller` ELF, `docs/info-density/**` capture dirs, SVG mockups
with operator PII, minified bundles) are reported by `apply-patches.py` — handle them
before re-running the gate; they must not survive into the public branch.

## Keeping the catalog in sync (drift control)

The sanitize gate is only as good as its catalog. On every cycle, and especially when
internal code adds new internal-reference shapes:

- A new placeholder the authors introduce must be added to `scripts/sanitize/allowlist.txt`.
- A new category of internal string must have both a hunt pattern in
  `scripts/sanitize/patterns.txt` (tier `hard`/`soft` + ERE + label) and a replacement
  convention in `patch-pass.workflow.js`.
- New files that intentionally name internal strings (more tooling/catalog) go in
  `scripts/sanitize/allowlist-paths.txt`; everything else must pass the gate.

If the regex gate or the LLM-judge flags something the catalog didn't anticipate, fix the
**catalog** (so the next cycle catches it deterministically), not just the one line.

## Release cadence vs. sync cadence

- **Internal releases** continue exactly as today: `make release` on internal `main` →
  the GitLab+acme-RWX pipeline (see root `CLAUDE.md` → Release).
- **Public sync** is decoupled and run on demand (e.g. per public-facing release, or when
  enough internal change has accumulated). A public sync does **not** trigger an internal
  release and vice-versa.
- For the **pilot** the public build runs on the same acme-RWX + GitLab registry as
  internal (minimal-port; see provisioning doc §2). When the dedicated-LOL switch lands,
  the public repo flips `IMAGE_REGISTRY`/`CHART_REGISTRY`/`REGISTRY_*`/`RWX_ACCESS_TOKEN`
  to the LOL/GHCR values — the pipeline code already honors those vars (MR !3168), so the
  sync runbook itself does not change.

## CI on the public branch

Wire `scripts/sanitize/no-internal-strings.sh` as a **blocking** CI job on the public
repo's default branch and on every public MR. It is fast, deterministic, and LLM-free —
it is the always-on guard that a future change (internal cherry-pick, external PR, or a
missed sanitize op) never reintroduces an internal reference. The LLM-judge half runs at
sync time (step 5), not per-CI.
