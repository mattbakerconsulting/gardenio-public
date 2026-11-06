# CI/CD Supply-Chain: Operator Verification Runbook (GAPS)

Source task: [ka-0000007025](https://beads.gardenio.e2e.example.com/i/ka-0000007025)
— *[GAPS] Operator: verify protected tags, push rule, CI var masking, GHCR robot scope.*
Parent audit: [ka-0000004158](https://beads.gardenio.e2e.example.com/i/ka-0000004158)
· Hardening backlog epic: [ka-0000005893](https://beads.gardenio.e2e.example.com/i/ka-0000005893)

## Why this doc exists

The CI/CD supply-chain audit (ka-0000004158) landed 10 findings, but **4 checks
could not be completed from an agent pod** because the GitLab settings they read
require **Maintainer** access and the fleet bot holds only **Developer**. Those
four became this GAPS task. This runbook:

1. Re-confirms the access boundary is real (not a transient 403), so nobody
   burns another cycle re-attempting the same calls from a Developer token.
2. Gives the operator (or any Maintainer-scoped session) the exact `glab`
   commands to run, and — for each — **what a PASS looks like** so the check is
   mechanical, not a judgment call.
3. Records the code-side facts we *could* verify without elevated creds (the
   GHCR fallback path), so item 4 is half-closed already.

> Per advice ka-0000001560, verification prose lives in beads/docs, not
> CI-minute-burning commits. This doc is committed because it is a durable,
> repo-local **runbook** (operator procedure), not a one-off finding write-up.

## The access boundary (measured, 2026-08-06)

The fleet bot's GitLab identity and project access were probed live:

| Fact | Value |
|---|---|
| Bot user | `group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa` (id `34915288`) |
| Project | `example-org/gardenio` (id `12345678`) |
| Group | `example-org` (id `14176856`) |
| **Project access level** | **30 (Developer)** |

The three GitLab-settings endpoints below **each require Maintainer (40+)** and
returned `403 Forbidden` to the Developer-scoped bot — same result the original
audit saw. This is a **permanent role boundary**, not a flake: do not re-attempt
from an agent pod.

```
$ glab api projects/12345678/protected_tags   → {"message":"403 Forbidden"}
$ glab api projects/12345678/push_rule         → {"message":"403 Forbidden"}
$ glab api projects/12345678/variables         → {"message":"403 Forbidden"}
$ glab api groups/14176856/variables           → {"message":"403 Forbidden"}
```

For contrast, endpoints readable at Developer level (used by finding F2) return
data fine — so the 403s above are authorization scoping, not connectivity:

```
$ glab api projects/12345678/protected_branches   → 200 (data)
$ glab api projects/12345678/approval_rules        → 200 ([] — no rules)
```

---

## Item 1 — Protected tags (who can push calver tags)

**Risk:** `scripts/release.sh` cuts a `git tag YYYY.DDD.N` (calver) on `main`
HEAD and `git push --atomic`es it; pushing a `20*` tag triggers the release +
prod-deploy dispatch chain (`.rwx/docker.yml` `tag-push if: starts-with(tag,
'20')`). If **any Developer** can push a matching tag, they can start a release.

**Operator command (Maintainer creds):**
```bash
glab api projects/12345678/protected_tags | jq '.[] | {name, create_access_levels: [.create_access_levels[].access_level_description]}'
```

**PASS criteria:**
- A protected-tag entry exists whose `name` pattern matches release calver tags
  — i.e. `20*` or `*` (must cover `YYYY.DDD.N`).
- Its `create_access_levels` is **Maintainers (40)** or **No one (0)** — **not**
  Developers (30). Developers must not be able to create release tags.
- If the list is **empty**, that is a **FAIL** (anyone with push can create any
  tag → can trigger a release). Track remediation under the F-series backlog.

---

## Item 2 — Push rules (signed-commit / member enforcement)

**Risk:** F4 (ka-0000005321) wants signed release tags/commits;
`reject_unsigned_commits` is the enforcement knob. `member_check` and
`commit_committer_check` prevent commits attributed to non-members / spoofed
authors from landing.

**Operator command (Maintainer creds):**
```bash
glab api projects/12345678/push_rule | jq '{reject_unsigned_commits, commit_committer_check, commit_committer_name_check, member_check, prevent_secrets, author_email_regex, deny_delete_tag}'
```

**PASS criteria (record current state; these are hardening targets, not all
mandatory today):**
- `prevent_secrets: true` — GitLab's built-in secret-push blocker on (defense in
  depth alongside the blocking `gitleaks` CI job in `.rwx/ci.yml`).
- `deny_delete_tag: true` — protects released calver tags from deletion.
- `reject_unsigned_commits` — currently expected **false**; flipping to `true` is
  the enforcement half of F4 and should be coordinated with tag/commit signing
  (release.sh uses `git tag`, not `git tag -s`).
- `commit_committer_check` / `member_check` — desirable; note current value.
- A **404 / empty** on this endpoint means **no push rules are configured** →
  FAIL for `prevent_secrets` and `deny_delete_tag`.

---

## Item 3 — CI variable masking & protection

**Risk:** The high-value secrets — `REGISTRY_TOKEN`, `RWX_ACCESS_TOKEN`,
`GITLAB_CLONE_TOKEN`, `PROD_AWS_*` — must be **masked** (redacted from job logs)
and **protected** (exposed only to protected branches/tags). Note: gardenio CI
sources most secrets from the **RWX vault** (`${{ secrets.* }}` in `.rwx/*.yml`),
not GitLab CI variables, so the GitLab-variable surface may be small — but any
that DO live in GitLab CI must be masked + protected.

**Operator command (Maintainer creds) — project then group:**
```bash
glab api projects/12345678/variables | jq -r '.[] | "\(.key)\tmasked=\(.masked)\tprotected=\(.protected)\tenv=\(.environment_scope)"'
glab api groups/14176856/variables   | jq -r '.[] | "\(.key)\tmasked=\(.masked)\tprotected=\(.protected)\tenv=\(.environment_scope)"'
```

**PASS criteria — for every secret-bearing key** (`*_TOKEN`, `*_SECRET`,
`*_AWS_*`, `*_PASSWORD`, `REGISTRY_*`, `RWX_*`, `GITLAB_CLONE_TOKEN`, `GHCR_TOKEN`):
- `masked: true` — value is redacted from job logs (complements F5
  ka-0000002171, which stopped `set -x` from tracing the token).
- `protected: true` — variable is only injected on protected branches/tags, so a
  fork/feature-branch pipeline can't exfiltrate it.
- A **non-secret** config var (e.g. a region name, a public URL) need not be
  masked; call those out explicitly so the audit trail shows they were reviewed,
  not missed.
- Any secret with `masked=false` **or** `protected=false` is a **FAIL** → file a
  follow-up under ka-0000005893.

---

## Item 4 — GHCR robot token scope (bot 403 → partial code-side confirmation)

**Context:** The audit flagged that `REGISTRY_TOKEN` **falls back to
`GHCR_TOKEN`** and asked whether the GHCR robot token is **write-only to the
gardenio org**, not cluster/org-wide.

**Code-side facts (verified from the repo, no elevated creds needed):**

- The **live registry** (`<registry>/gardenio`)
  is the default target everywhere (`docker.yml` `IMAGE_REGISTRY:-…` on ~40 push
  tasks). GHCR is a **legacy fallback**, not the primary path.
- Push auth fallback: `.rwx/docker.yml:665`
  ```
  REG_TOKEN="${REGISTRY_TOKEN:-$GHCR_TOKEN}"
  ```
  and the pull-secret fallback: `.rwx/deploy.yml:432` creates a `ghcr-registry`
  imagePullSecret **only if** `GHCR_TOKEN` + `GITHUB_USER` are both set (same
  shape in `.rwx/deploy-rwx.yml:817`, `coop/.rwx/push.yml:18`).
- Remaining hard `ghcr.io` references are **third-party base images** (kiwigrid
  k8s-sidecar, berriai litellm in `.rwx/bundle.yml`) and the **legacy
  `groblegark/gardenio` GitHub mirror** — which `docker.yml:80` explicitly
  dropped from the release path (`on.github.push` removed; the 12-day-stale
  mirror is no longer load-bearing).
- Per the task note: the release formula ka-0000000359's `ghcr.io/groblegark`
  refs are **STALE**. The live path does not depend on GHCR for gardenio images.

**What still needs operator eyes (GitHub side, not GitLab API):** the actual
**scope of the `GHCR_TOKEN` secret** — is the GitHub PAT / robot account
write-scoped to the gardenio packages only, or does it carry broader
org/repo/admin scope? That lives in the GitHub org's PAT/robot settings, not
reachable from the GitLab-scoped bot.

**Operator PASS criteria:**
- Given GHCR is now a **legacy fallback**, the strongest outcome is to confirm
  `GHCR_TOKEN` can be **retired** (nothing in the live release/deploy path
  requires it) — then delete the secret and the fallback lines. If it must stay,
  its GitHub scope must be **`write:packages` on the gardenio org only**, with no
  `repo`, `admin:org`, or `delete:packages`.

---

## How to run this verification

1. Assume a **Maintainer-or-higher** GitLab session on `example-org/gardenio`.
2. Run the four `glab api` command blocks above (items 1–3) + the GitHub GHCR
   scope check (item 4).
3. Record each result against its PASS criteria in a comment on ka-0000007025.
4. Any FAIL → file/route a follow-up under the hardening epic ka-0000005893
   (F4 ka-0000005321 already tracks the signed-commit / push-rule half).

Because these settings require Maintainer creds the fleet bot cannot hold, the
verification itself is an **operator escalation** — filed alongside this doc so
the four checks land on a privileged operator's queue.
