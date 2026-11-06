# V-R2: Default-filter UX & override semantics for `operated_by` in beads-viewer

**Status:** research only — no implementation, no test code, no wire-format changes.
**Parent epic:** ka-0000002754 (research-only).
**Sibling docs:**

- `viewer-default-operated-by-r1-inventory.md` (V-R1) — surface inventory; says **only `/active` (and its `?project=` variant) is default-on**.
- `viewer-default-operated-by-r3-server.md` (V-R3, pending) — server-side default-injection + cache implications.
- `viewer-default-operated-by-r4-migration.md` (V-R4, pending) — sunset condition for the migration banner.
- `human-attribution-ux.md` (R8, repo-root `docs/`) — chip rendering states, `me` alias, "my beads" hero link.
- `operated-by-transitivity-r3-dibs.md` — operator class lattice (`human` / `sentinel` / `agent`); the viewer's OIDC email canonicalises to the `oidc:<email>` form.

**Code touchpoints (read-only references — V-R2 is design-only):**

- `gardenio/controller/cmd/beads-viewer/operator_chip.go::resolveOperatedByParam` (line 118) — alias machinery already maps `me` / `null` / `(none)` and returns an `isMe` flag. R2 reuses this verbatim; the only change R3 will introduce is *defaulting* to `me` when the raw param is empty.
- `gardenio/controller/cmd/beads-viewer/active.go::handleActive` (line 72) — sole default-on surface. Already sets `MyWorkSummaryMode` when `isMe`.
- `gardenio/controller/cmd/beads-viewer/operator_chip.go::operatorIDsMatchViewer` (line 142) — already understands the email vs `oidc:<email>` equivalence we depend on.

## Why this exists

V-R1 settled *where* the default applies (one surface: `/active`). V-R2 settles *what the user actually sees and can do*:

- What does the URL look like the first time an OIDC user lands on `/active`?
- How do they "show all" again? How do they get back to "show only mine" after that?
- Do we remember their last choice? Across tabs? Across devices?
- How do we tell them — without making them feel watched — that the page is currently scoped to them?
- Once we ship default-on, the corpus is overwhelmingly Matt-owned. How do we signal that to a non-Matt user so they don't conclude the system is empty?
- What about the static-token / unauthenticated viewer that exists in dev and (briefly) in CI?

The output is wireframe-level: ASCII mockups, a sequence diagram, and a decision table. No code, no template diffs.

## 1. Cold-load default semantics

### 1.1 The contract

| Request shape (on a default-on surface)         | Effective filter                                                          |
|-------------------------------------------------|---------------------------------------------------------------------------|
| OIDC viewer, no `?operated_by=` at all          | `operated_by = <viewerEmail>` (server-applied; URL is *rewritten* to `?operated_by=me` on response — see §1.3) |
| OIDC viewer, `?operated_by=me`                  | `operated_by = <viewerEmail>` (alias resolves; URL kept as-is)            |
| OIDC viewer, `?operated_by=` (empty present)    | **no filter** ("show all") — the empty value is an *explicit* opt-out     |
| OIDC viewer, `?operated_by=null` or `(none)`    | `operated_by IS NULL` ("unowned" / autonomous / pre-backfill)             |
| OIDC viewer, `?operated_by=<other id>`          | literal match on that id (e.g. another teammate, or a `slack:U…` raw id)  |
| Anonymous / static-token viewer, no param       | **no filter** — no email to default to (see §6)                           |
| Anonymous viewer, any explicit value            | passed through verbatim (`me` is a no-op for an anonymous viewer; folds to "no filter" with a sign-in CTA) |

Two lines of this table are the load-bearing ones:

1. **"no param" ≠ "empty param".** Cold load with no param means the user has not made a choice; the server gets to apply the default. An empty param string means the user (or a "show all" link) has *explicitly* opted out; the server must respect that. R8 already documented `?operated_by=` as the empty-string shape; V-R2 promotes it to the canonical "show all" override.
2. **`me` is the new default; `<viewerEmail>` is its expansion.** The URL the user keeps in their browser bar after the cold-load redirect is `?operated_by=me`, not `?operated_by=alice@example.com`. The alias-form survives shared bookmarks (Alice's `?operated_by=me` URL DM'd to Bob renders Bob's view of `/active`, which is what people expect from a "show me my work" link).

### 1.2 Why "URL wins"

Two reasons:

- **Shareable links must be self-describing.** If a user pastes `?operated_by=null` into a Slack message, the recipient must see autonomous beads, not the recipient's own beads. The URL is the contract, the default is a fallback.
- **It composes with the existing `?project=`, `?status=`, `?type=` machinery.** Those params today are URL-only and have no per-user default; making `operated_by` follow the same pattern keeps mental load low.

### 1.3 Cold-load redirect: rewrite the URL or just render?

Two options for the cold-load case (no `?operated_by=` at all):

| Option | Behaviour | Pros | Cons |
|---|---|---|---|
| **A. 302 to `/active?operated_by=me`** | Server redirects on first hit | URL bar always reflects the active filter; bookmarks are honest; chip state = URL state, no template-only state to track | Adds a round-trip to every cold-load; messes up the `Referer` header; potentially noisy in access logs |
| **B. Render in place, leave URL bare** | `/active` renders default-on with no URL change | One round-trip; cleanest URL when typed by hand | URL bar lies (says "no filter" but page is filtered); refresh-as-bookmark has the wrong contract; chip and URL diverge — every "Show all" link must be careful to produce `?operated_by=` not bare `/active` |

**Recommendation: Option A — 302 redirect on cold-load.** The URL-honest behaviour is more important than saving one round-trip on the page that humans hit hardest. The redirect is local (same host, same path, query-only delta) so it's cheap, and HSTS/keep-alive amortise it. The chip's "Show all" link then unambiguously points at `/active?operated_by=` (empty string), which is *also* URL-honest and round-trips correctly through bookmarks.

Edge case to call out for V-R3: the redirect must only fire when the request had **no `operated_by` key at all**, not when it had `operated_by=` empty. That distinction lives in `r.URL.Query()["operated_by"]` (slice presence, not value). V-R3 owns the implementation; V-R2 just locks the contract.

## 2. Clearing semantics (the precise meaning of each value)

The `resolveOperatedByParam` function (`operator_chip.go:118`) already implements three of these; V-R2's contribution is to lock the **fourth** (the no-param case) to "apply default", and to lock the URLs the chip emits.

| URL fragment              | Resolved filter                  | Chip label (header, see §4)            | Chip secondary actions          |
|---------------------------|----------------------------------|----------------------------------------|---------------------------------|
| (no key)                  | `operated_by = <viewerEmail>` (default) | "Showing your work"               | [Show all] [Unowned] [Other…]   |
| `?operated_by=me`         | same as above                    | "Showing your work"                    | [Show all] [Unowned] [Other…]   |
| `?operated_by=`           | no filter                        | "Showing all work"                     | [Just mine] [Unowned] [Other…]  |
| `?operated_by=null`       | `operated_by IS NULL`            | "Showing unowned work"                 | [Just mine] [Show all] [Other…] |
| `?operated_by=(none)`     | same as `null` (alias)           | "Showing unowned work"                 | [Just mine] [Show all] [Other…] |
| `?operated_by=<other>`    | literal match                    | "Showing work operated by `<display>`" | [Just mine] [Show all] [Unowned]|

**Rules locked by this doc:**

- Empty (`?operated_by=`) is an explicit "all" — never auto-promoted to `me` regardless of how the user got there. This is the single most common opt-out and must be a stable URL.
- `me` is a server-side alias for `<viewerEmail>`. The viewer's identity for purposes of expansion is the OIDC email (`oidcauth.EmailFromRequest`). The expansion uses the canonical `oidc:<email>` form when comparing against `beads.operated_by`, but the *URL* keeps the alias `me` (cf. §1.1 line 2).
- `null` and `(none)` are aliases for IS-NULL. R8 §5.2 already treats them as equivalent and we keep that.
- For any other value, the viewer renders the chip with the resolved display name (via the existing `OperatedByDisplay` resolution path) and never tries to interpret the string further.

### 2.1 The "Show all" link contract

Every place in the viewer that today emits an unfiltered link to `/active` (e.g. nav header, project crumbs, "Active" tab) must, after V-R3 ships, emit `/active?operated_by=` *if and only if* the page is default-on. On default-off surfaces (V-R1: everything else) the bare `/active` link is fine — there's no default to clear.

This is a small but real footgun: a future contributor who adds a new "Active" link without the trailing query will silently re-engage the default for any user who clicks it. The chip + the V-R1 default-on table together are the documentation; V-R3 needs to thread the explicit empty value through every emitter and add a comment at the emit site.

## 3. Per-user persistence

The question: when Alice clicks "Show all" on Monday, what does she see when she opens `/active` on Tuesday?

### 3.1 Three viable storage shapes

| Shape | Storage | Cross-device? | Privacy footprint | Where the read happens |
|---|---|---|---|---|
| **None (always default-on)** | n/a | n/a | minimal | nowhere; the URL is the only state |
| **HttpOnly cookie** | browser cookie set by viewer; e.g. `gardenio_operated_by_pref=all` (90d) | no | low — cookie is same-origin, HttpOnly, scoped to `/active` | request handler reads cookie before applying default |
| **`user_pref` config bead** | a config-shaped bead keyed by `oidc:<email>`; field `operated_by_default`: one of `me|all|null|<id>` | yes | medium — the choice is queryable via beads HTTP API, visible to other users with read access; bead history records changes | request handler does a small daemon call (cached per-request) |

### 3.2 Recommendation: **None for V1; cookie for V2; bead for V3 (and never automatic)**

**V1 (ship-with-the-feature): no persistence.** The URL is the only state. Every cold load applies the default; every explicit choice lives in the URL bar, the browser back-stack, and bookmarks. If Alice wants "always show all", she bookmarks `/active?operated_by=` and uses that. This is the cheapest correct thing and matches how `?project=` and `?status=` already behave.

Reasons this is the right V1:

- Matt's framing is "default to your work". Persistence works *against* the default in the case the user has already escaped — which makes the default progressively weaker over time. A user who hits "Show all" once never sees their personal scope again unless they remember the URL trick. That's worse than the URL-only design.
- A persistence layer means the chip has to *also* tell the user "this is your remembered preference, not the global default." That's a new piece of UI complexity to land alongside the default itself. Pay for one new affordance at a time.
- We can always *add* persistence in a later epic without breaking URLs (URL-given values still win — see §3.3).
- It dodges every CSRF / privacy question (cookie shape) and every cross-tenant question (bead shape) for now.

**V2 (later, if telemetry shows users are escaping the default repeatedly): a cookie.** Scoped to `/active`, HttpOnly, SameSite=Lax, 90-day TTL. Set only when the user makes an explicit choice from the chip's dropdown; never set on cold-load (we don't want to "freeze" the default before the user has expressed an opinion). Read on cold-load *after* checking the URL — URL still wins.

**V3 (much later, if cross-device feedback comes in): the `user_pref` bead** as a server-side analogue. Same precedence (URL > bead > computed default). At this point we are running an OIDC-only fleet and the privacy/CSRF concerns of V2 are solved by the bead's existing access controls. This is also where R8's saved-view machinery starts to matter: a power user's "always show all" preference is just a particularly minimal saved view scoped to `/active`.

### 3.3 Precedence order (locked for all future V*N)

1. URL (`?operated_by=` value, including the empty string)
2. (Future, V2+) cookie or `user_pref` bead — only consulted when the URL has *no* `operated_by` key at all
3. Computed default (`me` for OIDC viewers on default-on surfaces, `none` otherwise)

The V1 implementation collapses (2) into a no-op; the framework still has the slot.

### 3.4 Why we are not shipping persistence now (decision matrix)

| Option            | CSRF / privacy | Cross-device | Cost to ship | Reverses cleanly? |
|-------------------|----------------|--------------|--------------|-------------------|
| No persistence    | n/a            | n/a          | smallest     | n/a               |
| Cookie            | small (HttpOnly + SameSite=Lax mitigates) | no | medium — new write path, expiry rules, "clear my preferences" affordance | yes (delete the cookie) |
| `user_pref` bead  | none for cookie; medium for visibility | yes | largest — new bead schema, read-modify-write, race semantics, GDPR-style "delete my prefs" | yes (delete the bead) |

V-R2 picks "no persistence" for V1, defers cookie/bead to a follow-on with its own decision checkpoint.

## 4. The indication chip (header)

### 4.1 Where it lives

A single chip in the page header strip on every default-on surface. Today's `/active` already has a header with project crumbs and the include-scheduled toggle; the operated-by chip slots into that strip, immediately to the right of the project crumb.

```
┌─ /active ─────────────────────────────────────────────────────────┐
│ Active   [project: gardenio ▾]   [☐ include scheduled]             │
│                                                                   │
│ ┌─ Showing your work ─────────────────────────────────[ × ]───┐  │
│ │ filter: operated_by = you                                    │  │
│ │ [ Show all ] [ Unowned ] [ Someone else… ]                   │  │
│ └──────────────────────────────────────────────────────────────┘  │
│                                                                   │
│ ── Daily-driver bead list below ──                                │
└───────────────────────────────────────────────────────────────────┘
```

### 4.2 Chip states (mirrors §2 table)

| Filter state                   | Chip headline                              | Primary actions                     |
|--------------------------------|--------------------------------------------|-------------------------------------|
| default-applied (cold or `me`) | **Showing your work**                      | [Show all] [Unowned] [Someone else…]|
| empty (explicit "all")         | **Showing all work**                       | [Just mine] [Unowned] [Someone else…]|
| `null`                         | **Showing unowned work** (`(scheduler)` and pre-backfill) | [Just mine] [Show all] [Someone else…]|
| literal `<other>`              | **Showing work operated by `<display>`** with raw id chip | [Just mine] [Show all] [Unowned]    |
| anonymous viewer               | **Showing all work — sign in to scope to yourself** | [Sign in] [Unowned] [Someone else…] |

Visual cues:

- The chip uses a calm tone (think "info" not "warning") — it informs, it does not alarm.
- The default-applied state shows a small **(default)** pill next to the headline so the user knows this is happening *because* of the cold-load default and not because they (or a colleague) set it.
- When the chip resolves to a real human (`<other>`), it reuses the operator-chip rendering from R8 — same `<strong>display</strong> <code>raw-id</code>` shape — for visual consistency with the bead-list rows below.

### 4.3 The "Someone else…" affordance

Reuses the typeahead/picker proposed in R8 §5.3 (`<input>` with server-side completion against distinct `operated_by` values). When selected, it sets `?operated_by=<their id>` and re-renders. No autocomplete results means the chip flips to "no matches" inline and leaves the URL alone.

### 4.4 Anti-pattern: what the chip is *not*

- It is **not** a banner. Banners imply "something is wrong"; the chip is a steady-state UI element that should fade into peripheral vision once a user is used to it.
- It is **not** modal. No confirm dialog, no toast on toggle. One click flips the URL and re-renders.
- It is **not** a place to surface the "all historic beads attributed to Matt" warning. That belongs to the migration banner (§5), which has a different lifecycle.

## 5. Migration banner

### 5.1 Why this is separate from the chip

V-R1 §1 and the parent epic both call out that the corpus today is overwhelmingly Matt-owned. A non-Matt OIDC user landing on `/active` for the first time will see an empty page with the chip saying "Showing your work" — which is technically correct but actively confusing in Phase 0. The chip cannot also explain the migration without bloating its primary job (showing current filter state).

Hence: a separate, dismissable migration banner, *above* the chip, *only for non-Matt OIDC users*, with a sunset condition.

### 5.2 Visual

```
┌─ Migration notice ─[ × dismiss ]──────────────────────────────────┐
│ ⓘ All historic beads were attributed to Matt during the migration.│
│   To see them, click "Show all" or visit                          │
│   /active?operated_by=. Your own beads will start populating as   │
│   you and your agents work.                                       │
│   (This banner sunsets once the corpus diversifies.)              │
└───────────────────────────────────────────────────────────────────┘
```

### 5.3 Visibility rules

- Render the banner **only** when:
  1. the viewer is OIDC-authenticated, **and**
  2. the resolved viewer email is not Matt's (`oidc:matt@…`), **and**
  3. the request is on a default-on surface (today: `/active`), **and**
  4. the migration banner has not been dismissed by this viewer (mechanism below), **and**
  5. the V-R4 sunset condition has not yet fired.
- Do *not* render for anonymous viewers. The sign-in CTA in the chip already addresses them.
- Do *not* render for Matt himself — the message is meaningless from his vantage.

### 5.4 Dismissal mechanism

Two options, parallel to §3:

- **Cookie** (preferred for V1 — same recipe as §3, but here we *do* set a cookie because dismissal is an explicit user action and the alternative is showing the banner forever). `gardenio_migration_banner_dismissed=1` (no value beyond presence), HttpOnly, 1-year TTL.
- **`user_pref` bead** (later; only if the cookie's per-device behaviour proves annoying).

The cookie shape is intentionally minimal — no email, no timestamp — so it carries no PII beyond presence. The banner content is the same for everyone who sees it; we don't need to remember *what* was dismissed, only *that* it was.

### 5.5 Sunset condition (forwarded to V-R4)

V-R4 owns the precise threshold; V-R2 just notes the shape. Three viable sunset triggers:

1. **Calendar date** — banner disappears after `2026-08-01` regardless. Cheap; brittle if dibs/transitivity slips.
2. **Cardinality threshold** — banner disappears once ≥ N distinct non-Matt `operated_by` values exist on beads created in the last 30 days. Self-tuning; needs a daemon-side count call.
3. **Config flag** — operator flips a boolean in viewer config or a `gardenio-config` bead. Manual; matches Matt's "we'll know when we see it" preference.

Recommendation forwarded to V-R4: **option 3 with option 1 as a backstop** (config flag wins, but if the flag is never flipped, the banner self-removes by date). Cardinality felt clever but risks being gamed by spawn churn.

## 6. Anonymous and static-token viewers

### 6.1 What "anonymous" means here

Two cases share a code path:

- The **dev-loop** viewer (`make run-beads-viewer`) runs without OIDC; `oidcauth.EmailFromRequest` returns empty.
- The **static-token** viewer (used by some CI / smoke flows) authenticates with a bearer token but carries no email identity.

Both are "no email available" cases. The default-injection logic must treat them identically: **no email, no default**.

### 6.2 What the user sees

```
┌─ /active ─────────────────────────────────────────────────────────┐
│ Active   [project: gardenio ▾]                                     │
│                                                                   │
│ ┌─ Showing all work ──────────────────────────────────────────┐   │
│ │ Sign in to scope this view to your own work.                │   │
│ │ [ Sign in ]   [ Unowned ]   [ Someone else… ]               │   │
│ └─────────────────────────────────────────────────────────────┘   │
│                                                                   │
│ ── Unfiltered bead list below ──                                  │
└───────────────────────────────────────────────────────────────────┘
```

Differences from the OIDC chip:

- Headline is "Showing all work" (no choice was applied — there is no `me` to default to).
- Primary CTA is **[ Sign in ]** instead of **[ Just mine ]** (clicking it routes to the OIDC sign-in path; on return the user lands back on `/active` and gets the cold-load default applied).
- "Someone else…" still works — anonymous users can still type a Slack ID or email to filter, because the daemon never required identity to filter.
- `?operated_by=me` from an anonymous viewer is a **no-op** that folds to "no filter". The chip explains why ("`me` requires sign-in") in a tooltip.

### 6.3 Why no `me` expansion for anonymous

R8 already documented this and `resolveOperatedByParam` already implements it (`viewerEmail == ""` → returns empty resolved value). V-R2 is just locking the *UX* contract: the chip must surface the sign-in path so the no-op isn't silent.

## 7. Sequence diagram: first-time visitor → URL override → cleared filter

The diagram covers three legs of one user's session:

1. Cold load (URL has no `operated_by` key). Server applies the default; redirects to the URL-honest form.
2. URL override (the user pastes a shared link with `?operated_by=null`). URL wins; no redirect.
3. Cleared filter (the user clicks "Show all" from the chip). URL becomes `?operated_by=` (empty); chip re-renders.

The `viewer` step is the `beads-viewer` HTTP handler (`handleActive`); the `daemon` step is the `kbeads` HTTP server it calls; `oidc proxy` is the forward-auth interceptor that injects `X-Auth-Request-Email`.

```text
Alice (browser)            oidc proxy             viewer (handleActive)        daemon (ListBeadsFiltered)
   |                            |                            |                            |
   | ─ GET /active ───────────► |                            |                            |     (1) cold load
   |                            | ─ GET /active ──────────► |                            |     (header injected)
   |                            |   X-Auth-Request-Email:    |                            |
   |                            |   alice@example.com        |                            |
   |                            |                            | ─ no operated_by key ────► |
   |                            |                            |   resolve viewerEmail      |
   |                            |                            |   compute default = "me"   |
   |                            |                            |                            |
   | ◄ 302 /active?operated_by=me ─────────────────────────  |                            |
   |                            |                            |                            |
   | ─ GET /active?operated_by=me ─────────────────────────► |                            |     (1b) honest URL
   |                            |   X-Auth-Request-Email:    |                            |
   |                            |   alice@example.com        |                            |
   |                            |                            | ─ resolveOperatedByParam ─►|
   |                            |                            |   resolved=oidc:alice@…    |
   |                            |                            |   isMe=true                |
   |                            |                            |                            |
   |                            |                            | ─ ListBeadsFiltered ─────► |
   |                            |                            |   OperatedBy=oidc:alice@…  |
   |                            |                            | ◄ result rows ──────────── |
   |                            |                            |                            |
   | ◄ 200 /active.html (chip = "Showing your work (default)")                            |
   |                            |                            |                            |
   |                            |                            |                            |
   | ─ click Slack-DM'd link ─► |                            |                            |     (2) URL override
   |   /active?operated_by=null |                            |                            |
   |                            | ─ GET /active?operated_by= |                            |
   |                            |   null ──────────────────► |                            |
   |                            |                            | ─ resolveOperatedByParam ─►|
   |                            |                            |   resolved="", isNull=true |
   |                            |                            | ─ ListBeadsFiltered ─────► |
   |                            |                            |   OperatedByIsNull=true    |
   |                            |                            | ◄ unowned beads ────────── |
   |                            |                            |                            |
   | ◄ 200 /active.html (chip = "Showing unowned work")                                    |
   |                            |                            |                            |
   |                            |                            |                            |
   | ─ click [Show all] in chip ►                            |                            |     (3) cleared filter
   |   GET /active?operated_by= |                            |                            |
   |                            | ─ GET /active?operated_by= |                            |
   |                            |   (empty) ───────────────► |                            |
   |                            |                            | ─ explicit empty: skip ───►|
   |                            |                            |   default; no filter       |
   |                            |                            | ─ ListBeadsFiltered ─────► |
   |                            |                            |   (no OperatedBy)          |
   |                            |                            | ◄ all rows ─────────────── |
   |                            |                            |                            |
   | ◄ 200 /active.html (chip = "Showing all work")                                        |
```

Three things to notice in the diagram:

- The **redirect happens once**, on the very first request. Once the URL bar reads `/active?operated_by=me`, every reload, refresh, or back-button uses the URL — no more redirects, no more "cold load" decisions.
- The handler **never blindly rewrites to `me`**. It only rewrites when the `operated_by` *key is absent from the query*. The empty-string case (leg 3) is preserved untouched — that's the explicit "all" contract from §1.1.
- The **daemon never knows about `me`**. The alias expansion happens in the viewer; the daemon receives the canonical `oidc:alice@example.com` (or `OperatedByIsNull=true`, or no filter at all). This keeps the daemon's filter API stable across UX changes.

## 8. Decision summary (table form)

| Decision                                              | V-R2 contract                                                                   | Locked here? | Forwarded to |
|-------------------------------------------------------|---------------------------------------------------------------------------------|--------------|--------------|
| Default surface(s)                                    | Only `/active` (and `?project=` variant) is default-on                          | (V-R1)       | —            |
| Cold-load behaviour                                   | OIDC viewer + no key → 302 to `?operated_by=me`; URL wins thereafter            | yes          | V-R3 impl    |
| `?operated_by=` empty                                 | Explicit "all"; **never** auto-promoted to `me`                                 | yes          | V-R3         |
| `?operated_by=null` / `(none)`                        | `IS NULL`; aliases preserved                                                    | yes (R8 too) | —            |
| `?operated_by=me`                                     | Resolves to `oidc:<viewerEmail>` server-side; URL keeps the alias               | yes (R8 too) | —            |
| `?operated_by=<other>`                                | Literal pass-through; chip uses display name                                    | yes (R8)     | —            |
| Per-user persistence                                  | **None** in V1; cookie in V2; `user_pref` bead in V3 — each gated by its own decision | yes (V1) | follow-on epic |
| Header chip                                           | Single calm chip on default-on surfaces; states per §4.2                        | yes          | V-R3 (template work) |
| Migration banner                                      | Separate, dismissable, non-Matt OIDC only; sunset condition forwarded            | yes          | V-R4         |
| Banner sunset trigger                                 | Config flag preferred; calendar backstop; cardinality rejected                  | recommended  | V-R4 owns final call |
| Anonymous / static-token viewer                       | No default; chip with "Sign in" CTA; `me` is a silent no-op (tooltip explains)  | yes          | V-R3         |
| Saved views                                           | Saved-view spec is verbatim — default-injection skips this code path            | (V-R1 §6)    | V-R3         |
| Daemon-side filter API change                         | None — alias expansion happens in viewer                                        | yes          | —            |

## 9. Open questions surfaced (for V-R3 / V-R4 / decision checkpoint)

1. **Redirect HTTP code (V-R3).** 302 (default) keeps the request method but most clients re-issue as GET; 303 is "see other GET" which is more semantically honest for our case. Either is fine; V-R3 picks one and writes it down.
2. **Chip dropdown UX (V-R3 + a future viewer-frontend bead).** "Someone else…" picker shape — inline input vs popover vs full menu — depends on the rendering primitives the team is comfortable with. R8's typeahead is the prior-art baseline; V-R3 may push this to a later UX bead.
3. **Mobile / narrow-viewport chip layout (V-R3).** The chip's three-action row will not fit on a 320px viewport; needs a collapsed "▾" menu form. Calling out so V-R3 doesn't ship a desktop-only chip.
4. **Banner copy approval (V-R4 + operator).** The banner text in §5.2 is a draft; final copy should land in V-R4 alongside the sunset trigger.
5. **Telemetry to drive V2 persistence decision (V-R3 or follow-on).** If we want to know whether users actually escape the default repeatedly, we need a counter — `/active` requests with explicit `?operated_by=` (non-default) value, grouped by viewer email. Deciding whether to add that counter is a small design question on its own; deferred.
6. **Interaction with R8's "My beads" hero link (R8 §5.4).** R8 proposed a separate `[ My beads ]` nav item that links to `/active?operated_by=me`. Once `/active` is default-on, that link is redundant — clicking it is the same as clicking `Active`. R8's nav item should be removed or repurposed (e.g. to deep-link into a different default-off surface like `/search?operated_by=me`). Calling this out so the implementation epic can clean it up.

## 10. Links

- This bead: ka-0000003485 (V-R2)
- Parent epic: ka-0000002754
- V-R1 inventory: `gardenio/docs/research/viewer-default-operated-by-r1-inventory.md` (ka-0000000145, merged via MR !1310 / closed)
- R8 prior art: `docs/research/human-attribution-ux.md`
- Dibs / class lattice: `gardenio/docs/research/operated-by-transitivity-r3-dibs.md` (ka-0000006715)
- Code touchpoints: `gardenio/controller/cmd/beads-viewer/{operator_chip.go, active.go, search.go}`
- Decision checkpoint: filed by epic owner once V-R1 → V-R4 land as merged docs.
