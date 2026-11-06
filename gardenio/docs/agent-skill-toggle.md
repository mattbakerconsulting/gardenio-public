# Per-Role Skill & Sub-Agent Toggles (`enabled_skills` / `disabled_skills`, `enabled_agents` / `disabled_agents`)

**Status:** Reference (grounded in live code, file:line cited). Documents the
role-bead skill toggle shipped for epic ka-0000004989 ("Adam feedback #1"),
E2E-validated in ka-0000004292, and the sub-agent toggle added in ka-0000006908.
Backfills the `gardenio/docs` gap recorded in ka-0000003972 — before this doc,
the fields existed in code and the beads-viewer UI but were undocumented.

**What it is:** two pairs of optional fields on a `type:role` config bead —
`enabled_skills` / `disabled_skills` and `enabled_agents` / `disabled_agents`
(allow-list / deny-list each) — that decide **which Claude Code skills and
sub-agents get linked into an agent's workspace** for that role. Entries a
toggle drops are simply not present under `.claude/skills/` /
`.claude/agents/`, so the agent never sees them. The default (fields empty /
absent) links **everything** discovered, exactly as before the toggles existed.

> **Scope.** The skill toggle governs the `skills` subdir, the agent toggle
> the `agents` subdir. Slash commands (`.claude/commands/`) **always link** —
> there is no command toggle (`linkClaudeExtensions`,
> `controller/cmd/gb/agent_k8s_workspace.go`; proven by
> `TestLinkClaudeExtensionsFilterSkillsOnly` and
> `TestLinkClaudeExtensionsAgentToggle`). The toggles are independent: a name
> in `disabled_skills` never drops an agent, and vice versa.
>
> **Codex pods** honour the same `enabled_skills` / `disabled_skills`: `gb setup codex`
> links the same entries into `$CODEX_HOME/skills/` (`setup_codex_skills.go`). The
> agent toggle has no Codex effect.

> **The agent toggle gates only repo-provided sub-agents** — the ones gardenio
> symlinks from the primary/sibling repos' `.claude/agents/`. The harness
> built-in agent types (Explore, Plan, general-purpose, …) are not linked
> files and are unaffected. It lets a terminal Bash-only role (e.g. a
> mechanical cron gardener) shed repo sub-agents it never spawns and trim the
> corresponding `agent_listing` bytes those linked files add.

Everything below is written for the skill toggle; the agent toggle behaves
identically with `enabled_agents` / `disabled_agents` and LINKED agent file
names (primary entries bare, e.g. `reviewer.md`; sibling entries prefixed
`{repo}-`). Its multi-role union is built by the same helper
(`unionExtensionFilter`, `controller/cmd/gb/setup.go`).

---

## Semantics

The decision helper is `extensionFilter.allows`
(`controller/cmd/gb/agent_k8s_workspace.go`). For a given skill's
**linked name** (see [Skill names](#skill-names--the-post-namespacing-linked-name-is-what-you-list) below):

| Field | Kind | Rule |
|---|---|---|
| `enabled_skills` | **allow-list** | When **non-empty**, ONLY skills whose linked name is in the list are linked. **Empty ⇒ no restriction** (every discovered skill is enabled). Mirrors `account_whitelist`'s empty-means-unrestricted convention. |
| `disabled_skills` | **deny-list** | A skill whose linked name is in the list is **never** linked — **even if it also appears in `enabled_skills`**. |

**Deny wins over allow.** A skill listed in both is dropped. The decision logic,
verbatim from `agent_k8s_workspace.go` (inline comments added here for
clarity — they are not in the source):

```go
func (f extensionFilter) allows(linkedName string) bool {
    if f.disabled[linkedName] {        // deny wins
        return false
    }
    if len(f.enabled) > 0 && !f.enabled[linkedName] {  // allow-list gates only when non-empty
        return false
    }
    return true
}
```

Truth table (rows drawn from `TestSkillFilterAllows`):

| enabled_skills | disabled_skills | skill | linked? |
|---|---|---|---|
| *(empty)* | *(empty)* | anything | ✅ yes (default: all on) |
| `a,b` | *(empty)* | `a` | ✅ yes |
| `a,b` | *(empty)* | `c` | ❌ no (not in allow-list) |
| *(empty)* | `x` | `x` | ❌ no |
| *(empty)* | `x` | `y` | ✅ yes |
| `a` | `a` | `a` | ❌ no (**deny wins**) |
| `a` | `z` | `b` | ❌ no (not in allow-list) |

---

## Skill names — the post-namespacing ("linked") name is what you list

The names in `enabled_skills` / `disabled_skills` must match the **linked skill
name that Claude Code actually discovers** under `.claude/skills/`, which is set
by the multi-repo merge in `linkExtensionSubdir`
(`controller/cmd/gb/agent_k8s_workspace.go`, `linkExtensionSubdir`):

- **Primary repo** — cloned to `/home/agent/bot/{project}/work`. Its skills link
  under their **bare** directory name: a skill dir `code-review/` → `code-review`.
- **Sibling reference repos** — cloned to `/home/agent/bot/{project}/repos/{name}`
  (siblings of your working dir; run `ls ../repos/`). Their skills are
  **namespaced** with a `{repoBase}-` prefix, where `repoBase` is the checkout
  directory's base name (`filepath.Base(repo)`,
  `agent_k8s_workspace.go`): a skill dir `deploy/` in sibling repo
  `otherrepo` → `otherrepo-deploy`.

Namespacing avoids collisions when the primary and a sibling ship a skill of the
same base name. **Primary wins** on a collision, and an earlier sibling (siblings
are sorted by name, `siblingRepoDirs` → `sort.Strings`) wins over a later one
(`agent_k8s_workspace.go`).

> **Gotcha — you must use the NAMESPACED name to target a sibling skill.**
> Denying the bare basename of a sibling skill is a **no-op**: `disabled_skills:
> sibling` does NOT drop `otherrepo-sibling` (`TestLinkExtensionSubdirSkillDenyUnnamespaced`,
> `TestLinkExtensionSubdirSkillDenyBareKeepsSibling`). Conversely, `disabled_skills:
> deploy` drops the **primary** `deploy` but leaves every sibling's
> `{repo}-deploy` untouched.

> **Gotcha — a typo in an allow-list fails CLOSED.** An `enabled_skills` naming
> only skills that do not exist links **zero** skills (`TestLinkExtensionSubdirSkillPhantomNames`).
> Denying a non-existent skill, by contrast, is a harmless no-op. When in doubt,
> check the linked names against what the viewer shows for the role, or against
> the agent's `.claude/skills/` after spawn.

---

## Multi-role agents — union semantics

A single agent can wear several hats (comma-separated `BOAT_ROLE`, e.g.
`crew,triage`). The effective filter is the **union of the roles' effective skill
sets**, built by `fetchRoleExtensionFilters` → `unionExtensionFilter`
(`controller/cmd/gb/setup.go`) — an agent that wears N hats gets what any
one of those hats would grant on its own:

- **DENY side — deny wins across roles.** A skill is disabled if **any**
  participating role disables it. Deny-lists union.
- **ALLOW side — an unrestricted role collapses the allow-list.** A role with no
  allow-list (empty `enabled_skills`, *or no role bead at all*) grants **every**
  skill, so its effective allow-set is the universe. The union with a universe is
  the universe — so **if any participating role is unrestricted, the combined
  allow-list is cleared and only the deny-list gates.** Otherwise the allow-list
  is the union of every restricting role's `enabled_skills`.

Worked example (`TestFetchRoleSkillFilterMixedAllowList`), roles:

```
scoped:  enabled_skills=[x]      disabled_skills=[bad]
broad:   enabled_skills=[]       disabled_skills=[danger]     # unrestricted allow
```

- Spawned as `scoped` alone → allow-list restricts to `{x}`; `y` is blocked.
- Spawned as `scoped,broad` → `broad` is unrestricted, so the allow-list
  **collapses**: every skill is allowed **except** the unioned deny-list
  (`bad`, `danger`). `x`, `y`, `anything-else` all link; `bad` and `danger` do not.

> **How this compares to `account_whitelist`.** Both combine multi-role sets by
> **unioning** the raw entries — `resolveRoleAccountWhitelist`
> (`controller/cmd/controller/podspec.go:2092-2125`) returns the union of every
> role's `account_whitelist`, just as the skill toggle unions each field. The
> difference is what a **non-empty** list then means. For accounts, a non-empty
> combined whitelist stays **fail-closed**: `accountAllowedByRoleWhitelist`
> (`podspec.go:2127-2142`) permits ONLY the listed accounts — right for a
> credential boundary. The skill toggle deliberately does the opposite: it is a
> **curation** feature, so an unrestricted co-role (empty `enabled_skills`)
> **collapses** the combined allow-list to allow-all (`unionExtensionFilter`, `setup.go`), failing
> toward **granting** the skills an operator explicitly gave a co-role rather than
> stripping them when an unrelated restrictive role joins the same agent
> (`setup.go`). Deny-lists on both sides always union and win.

**Best-effort / fail-open.** If the role string is empty, the daemon is
unreachable, or a named role has no bead, the effective filter is empty
(allow-all) — the same default as before the toggle existed. An empty role
string, a nil lister, or a daemon error early-returns the empty filter
(`setup.go`); an unknown role is treated as unrestricted and collapses
the allow-list to allow-all (`setup.go`). Either way the toggle never
blocks a spawn; at worst it links everything (`TestFetchRoleSkillFilterDegrades`).

---

## How to set it

### `gb role scaffold` (recommended)

Both fields are repeatable / comma-separated flags on `gb role scaffold`
(`controller/cmd/gb/role.go`):

```sh
# Allow-list: this role links ONLY these skills (bare = primary, prefixed = sibling)
gb role scaffold my-role --enabled-skills code-review,verify,otherrepo-deploy

# Deny-list: this role links everything EXCEPT these
gb role scaffold my-role --disabled-skills video,deep-research

# Both (deny wins on overlap)
gb role scaffold my-role --enabled-skills a,b,c --disabled-skills b

# Sub-agent toggle — same shapes, agent FILE names (ka-0000006908)
gb role scaffold my-role --disabled-agents reviewer.md,otherrepo-deployer.md
```

- Flags accept repeats (`--enabled-skills a --enabled-skills b`) or a single
  comma-separated value; both land as a comma-separated string field.
- Passing the flag with an **empty value** clears a value cloned via `--from`
  (`setOrDelete` deletes on empty, `role.go`) — the same idiom as
  `--model ""`.
- Omitting the flag entirely leaves any existing value untouched (the write is
  gated on `cmd.Flags().Changed(...)`).

### Editing the field directly

`enabled_skills` / `disabled_skills` / `enabled_agents` / `disabled_agents` are
ordinary role-bead fields. The parser
(`parseStringList`, `controller/internal/beadsapi/roles.go`) reads **either**
a JSON array **or** a comma-separated string, trimming whitespace and dropping
empty entries — so both of these are equivalent:

```sh
kd update <role-bead-id> -f enabled_skills='code-review,verify'
kd update <role-bead-id> -f enabled_skills='["code-review","verify"]'
```

> Do **not** use `kd edit` — it opens `$EDITOR` and hangs an agent. Use
> `kd update -f`.

### Where the fields are consumed

The toggle is read **in-pod by `gb setup claude`** (`symlinkClaudeExtensions` →
`fetchRoleExtensionFilters`), which fetches role beads directly from the daemon at
workspace setup. It is **NOT** propagated through the controller's pod spec —
`config.RoleInfo` intentionally omits it (`roles.go`). Practical upshot:
a change to a role's skill fields takes effect on the **next spawn / workspace
setup**, no controller release required.

---

## Where to see it

The beads-viewer **role hub** page renders the toggles when set
(`controller/cmd/beads-viewer/templates/role_hub.html`):

- **"Skills enabled (allow-list)"** / **"Skills disabled (deny-list)"** —
  shown only when the corresponding field is non-empty.
- **"Sub-agents enabled (allow-list)"** / **"Sub-agents disabled (deny-list)"**
  — likewise for the agent toggle.

An empty toggle renders **no panel** — absence of the panels means "everything
on" (the default), not a bug.

At spawn time, `gb setup claude` logs each decision:
`[gb agent start] linked <dst> -> <src> (<source-label>)` for entries it links
(the trailing label is the source: `primary`, or a sibling repo's base name),
`[gb agent start] skipping skills <name> (role skill toggle)` for skills the
filter drops, and `[gb agent start] skipping agents <name> (role agent toggle)`
for dropped sub-agents (`agent_k8s_workspace.go`). When a filter is active it
also prints a one-line summary `[setup] role skill toggle: N enabled-allow, M
disabled` (or `role agent toggle: …`, `setup.go`). Grep the pod's setup output
to confirm what actually happened.

---

## Quick reference

```
enabled_skills   = allow-list; empty ⇒ ALL skills on; non-empty ⇒ ONLY these
disabled_skills  = deny-list;  deny WINS over allow
enabled_agents   = allow-list for .claude/agents/ entries (same semantics)
disabled_agents  = deny-list for .claude/agents/ entries (deny wins)
names            = the LINKED name: primary entries BARE, sibling entries {repo}-{name};
                   agent entries are FILE names (reviewer.md), skill entries dir names
scope            = skills toggle → skills subdir; agents toggle → agents subdir;
                   commands always link; harness BUILT-IN agent types unaffected
multi-role       = deny unions (any role denies ⇒ denied);
                   allow unions, BUT any unrestricted role collapses the allow-list
failure          = best-effort allow-all (daemon down / unknown role ⇒ everything links)
set via          = gb role scaffold --enabled-skills/--disabled-skills/
                   --enabled-agents/--disabled-agents, or kd update -f
takes effect     = next spawn / workspace setup (read in-pod, not via controller pod spec)
see it           = beads-viewer role hub panels; pod setup log lines
```

## See also

- [`onboarding-new-project-and-role.md`](onboarding-new-project-and-role.md) — Step 4
  (Create a custom role) is where you'd apply this toggle when standing up a role.
- Source: epic ka-0000004989 (skill toggle), task ka-0000004292 (E2E validation),
  task ka-0000003972 (this doc), task ka-0000006908 (sub-agent toggle, epic
  ka-0000007580). Tests: `controller/cmd/gb/agent_k8s_skillfilter_test.go`.
