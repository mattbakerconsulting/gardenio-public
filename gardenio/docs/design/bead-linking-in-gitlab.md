# Bead linking & hover-preview in GitLab MRs

Status: design / proposal
Bead: ka-0000006104 (parent thread ka-0000002923)
Author: prewarmed-1781015786-1 (thread agent)
Operator ask (Matt Baker): *"think how to address bead linking in MRs and
potentially mouse-over preview in GitLab."*

## TL;DR

We already maintain a gardenio-managed "Agent Context" block inside every
linked MR's description, and we already have a polished hover-preview popover
in **beads-viewer**. The two gaps Matt is pointing at:

1. **Bead IDs in GitLab MRs are plain text, not links.** The `- **Bead:**
   ka-XXX` line in the MR description (and the bead IDs an agent types into
   MR descriptions / comments by hand) render as inert text in GitLab. A
   reviewer reading the MR cannot click through to the bead.
2. **GitLab can't run our popover JS,** so the in-app hover preview we built
   for beads-viewer does not exist when you read the same bead reference on
   gitlab.com. Reproducing it natively requires a different mechanism.

**Recommendation:** ship (1) immediately — it's a one-line change to the
managed MR-description section plus a small linkifier for the rest of the
body, low-risk and high-value. Treat (2) as a follow-up with a clear
preferred path (GitLab's own native cross-link unfurling via a backlink note,
not a custom hover overlay).

---

## What exists today

### Forward path: bead → MR (controller/internal/bridge)

- **`gitlab_mrdesc.go`** — `buildGardenioSection` renders a managed block,
  delimited by `<!-- gardenio:start … -->` / `<!-- gardenio:end -->` markers,
  spliced into the MR description. It already surfaces Bead, JIRA, MR state,
  pipeline status, approvals. `syncMRDescription` is idempotent (no-op when
  the section content is unchanged).
- **`gitlab_sync.go` `handleDescriptionSync`** — on `beads.bead.updated`,
  builds the section from bead fields and writes it via the GitLab API.

The bead line is built here (`gitlab_mrdesc.go:32`):

```go
b.WriteString(fmt.Sprintf("- **Bead:** %s\n", s.BeadID))
```

`s.BeadID` is the bare ID string — **no Markdown link wrapping**. GitLab
renders it as literal text.

### Reverse path: MR → bead

- **`gitlab_mr_backlink.go`** — periodic reconciler that scans recently
  updated group MRs and recovers the owning bead from a PE-/FSM-/kd- key in
  the branch/title/description, then sets `mr_url` (what `gb mr link` does).
- **`gitlab_comment_mrlink.go`** — watches `beads.comment.added`; if a comment
  carries an MR URL, back-fills the parent bead's `mr_url`.

These keep the bead↔MR association populated. They are *not* about rendering
links inside GitLab — they're about the data join.

### Hover preview (beads-viewer only)

- **`bead_preview.go`** — `GET /b/{id}/preview` returns a small HTML fragment
  (type, status, id, title, ~280-char description excerpt; a richer
  `bead_preview_mr` variant for MR beads).
- **`templates/static/preview_popover.js`** — singleton floating `<aside>`,
  150 ms hover-intent, per-page cache, viewport-aware positioning. Fires for
  any `<a>` whose path is `ka-XXX` (or `a/{name}`).
- Plan-of-record: `docs/design/beads-viewer/popover-preview.md`.

This is **same-origin only** — the JS, the endpoint, and the OIDC session all
live under the beads-viewer domain. None of it runs on gitlab.com.

### Linkifier (beads-viewer only)

- **`linkify.go`** already does the inverse of what we want for GitLab: inside
  beads-viewer it wraps bare `ka-XXX`, `!N` MR refs, and `FSM-/PE-/DEVOPS-`
  JIRA keys in anchors. The bead-href and JIRA-base-URL conventions there are
  the reference for what a GitLab-side link should point at.

---

## Gap 1 — Make bead references clickable in GitLab MRs

### 1a. The managed "Agent Context" block (highest value, lowest risk)

Change the one line in `buildGardenioSection` to emit a Markdown link when a
viewer base URL is configured:

```
- **Bead:** [ka-XXX](https://<viewer>/b/ka-XXX)
```

Design notes:

- **Plumb a viewer base URL into the section builder.** `MRDescriptionSection`
  gains a `ViewerBaseURL` field; `GitLabSync` gains the URL via
  `GitLabSyncConfig` (the bot already carries `beadsViewerURL`, e.g.
  `https://gardenio.e2e.example.com/beads` — `bot.go:85`). When the URL is
  empty, fall back to the current plain-text rendering so nothing breaks in
  configs without a public viewer.
- **Path shape.** beads-viewer serves bead detail at `{base}/b/{id}` (see the
  popover `previewURLFor` and the `/b/{id}` routes in `server.go`). Use that,
  not a bare `{base}/{id}`.
- **Idempotency is preserved for free.** `syncMRDescription` compares rendered
  section content; the link text changes once and then stabilizes.
- **JIRA already deep-links?** No — the JIRA line is also plain text today.
  Same treatment: `[FSM-371](https://acme.atlassian.net/browse/FSM-371)`,
  reusing the `jiraBaseURL()` convention from `linkify.go`. Low extra cost.

Estimated change: ~15 lines in `gitlab_mrdesc.go` + field plumbing in
`gitlab_sync.go` + a couple of test updates in `gitlab_mrdesc_test.go`.

### 1b. Bead refs the agent types elsewhere in the MR body / comments

Inside the **managed block** we control the Markdown, so 1a covers it. But
agents and humans also write bare `ka-XXX` in the free-form part of the MR
description and in MR comments. Those stay inert.

Options:

- **(b-i) Do nothing outside the managed block.** GitLab will never know what
  `ka-XXX` means, and we must not rewrite user-authored prose outside our
  markers (the whole point of the markers is "content outside is never
  modified" — `spliceGardenioSection`). Lowest risk; partial coverage.
- **(b-ii) GitLab-native autolink via project settings.** GitLab supports
  *external issue trackers* and *custom autolink*-style references only in
  limited ways; the cleanest native hook is **a project/group-level
  description template + "External issue tracker" integration**, but that's
  built for a single tracker (and we already point JIRA there in some repos).
  Not a clean fit for `kd-` refs.
- **(b-iii) A bridge-authored "📎 Beads" comment** that lists every `ka-XXX`
  referenced anywhere in the MR as real Markdown links, refreshed like the
  managed description block. This gets clickability for free-text refs without
  ever mutating user prose. It's the same dual-write pattern we already use;
  the cost is one extra system comment per MR.

**Recommendation for Gap 1:** do **1a** now (managed block → links, both bead
and JIRA). Defer **1b**; if reviewers ask for clickable refs in free text,
prefer **(b-iii)** the managed comment over rewriting prose.

---

## Gap 2 — Hover preview in GitLab

Our beads-viewer popover cannot be reused on gitlab.com: GitLab won't load our
JS, and even if it did the `/b/{id}/preview` endpoint is OIDC-gated and
same-origin. So "mouse-over preview in GitLab" means one of:

### 2a. GitLab-native cross-reference unfurling (preferred)

GitLab already renders a **hover/preview card** for *GitLab* references —
issues, MRs, epics — when you mention them (e.g. `#123`, `!456`,
`group/proj#789`). The card shows title/state/assignee on hover, natively, no
JS from us.

We don't get that for `kd-` beads because beads aren't GitLab issues. But we
*can* lean into it: when a bead has a linked MR, **post a GitLab-side backlink
note** (or include in the managed block) a reference to the *GitLab* objects
the bead relates to (the MR itself, sibling MRs in a chained tree, the JIRA
issue if mirrored as a GitLab external ref). Those get native hover cards.

This is the **highest-leverage, lowest-maintenance** answer to "hover preview
in GitLab": instead of rebuilding our popover on a domain we don't control,
emit references in the form GitLab *already* unfurls.

Limitation: it previews the *MR/issue*, not the *bead's* description/status.
For most reviewers that's actually what they want (where's the work, what's
its state), and Gap 1a already puts the bead's status inline in the managed
block.

### 2b. Rich link unfurling via Open Graph / oEmbed

GitLab does **not** unfurl arbitrary external links into preview cards in MR
descriptions (unlike Slack). So adding OG tags to beads-viewer bead pages buys
us nothing *inside GitLab* (it would help Slack/Confluence/etc.). Worth doing
for those surfaces, but it does not satisfy "preview in GitLab." Out of scope
for this ask.

### 2c. Browser extension / userscript injecting our popover

A Tampermonkey-style userscript (or a thin org Chrome extension) could detect
`ka-XXX` on gitlab.com and call a CORS-enabled preview endpoint to render the
same card. Technically the *only* way to get the true beads popover on
gitlab.com.

Costs: an extension to distribute and maintain; CORS + auth on a public
preview endpoint (today `/b/{id}/preview` is OIDC-gated same-origin — exposing
a cross-origin preview means an auth story for gitlab.com → beads-viewer);
per-developer install friction. High effort, narrow audience.

**Recommendation for Gap 2:** pursue **2a** (emit GitLab-native references so
GitLab's own hover cards do the work). Skip 2b for GitLab (revisit only for
Slack/Confluence unfurl). Only consider **2c** if there's strong demand for
the *bead's own* content on hover and 2a proves insufficient — and gate it
behind the CORS/auth work it requires.

---

## Recommended sequencing

| Step | Scope | Risk | Value |
|---|---|---|---|
| **1a** Managed-block bead + JIRA → Markdown links | ~15 LOC + plumbing | Low (falls back to plain text when no viewer URL) | High — reviewers click straight to the bead |
| **2a** Emit GitLab-native refs (MR/issue) for native hover cards | small | Low | Medium-High — "preview in GitLab" with zero custom UI |
| **1b-(b-iii)** Managed "📎 Beads" comment for free-text refs | medium | Low-Med (extra system comment) | Medium — only if reviewers want free-text clickability |
| **2c** Userscript/extension w/ CORS preview endpoint | large | Med (auth/CORS, distribution) | Low-Med — true popover, narrow audience |

Steps 1a and 2a are independently shippable and together cover the bulk of the
ask. I'd start with **1a** as the first PR.

## Open questions for Matt

1. Is there a single public beads-viewer URL we can rely on for the deep-link
   base in MR descriptions (the `beadsViewerURL` the bot already knows), or
   does it vary per environment in a way that complicates the link?
2. For "preview," is seeing the *bead's* status/title enough (1a inline +
   2a native cards), or do you specifically want the floating popover card
   reproduced on gitlab.com (which pushes us toward 2c)?
3. Do we want clickable bead refs in *free-text* MR body/comments (1b), or is
   the managed "Agent Context" block sufficient?
