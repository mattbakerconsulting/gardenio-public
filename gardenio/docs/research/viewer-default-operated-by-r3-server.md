# V-R3: Server-side propagation & caching for default `operated_by=me`

**Status:** research only — no implementation.
**Parent epic:** ka-0000002754 (research-only).
**Sibling docs:** R1 (surface inventory), R2 (UX/override semantics), R4 (migration sunset).
**Related prior work:**

- `gardenio/docs/research/viewer-default-operated-by-r1-inventory.md` — picks `/active` as the only default-on surface (Phase 1).
- `gardenio/docs/research/human-attribution-ux.md` (R8) — chip rendering + `MyWorkSummary` mode flag.
- `gardenio/controller/cmd/beads-viewer/active.go::handleActive` — reads `?operated_by=` today (active.go:75).
- `gardenio/controller/cmd/beads-viewer/search.go::handleSearch` — reads `?operated_by=` today (search.go:51).
- `gardenio/controller/cmd/beads-viewer/operator_chip.go::resolveOperatedByParam` — alias machinery for `me` / `null` / `(none)`.
- `gardenio/controller/internal/oidcauth/oidcauth.go::EmailFromRequest` — viewer-email resolution (oidcauth.go:48).
- `gardenio/controller/internal/beadsapi/client_query.go::ListBeadsQuery` — daemon list-query shape (client_query.go:12).

## Why this exists

R2 decides *what* the default-on UX looks like; R3 decides *where in the request path* the default actually gets stamped onto the daemon query, *how* it composes with the existing `?project=` / `?status=` / `?type=` filters, and *which* invariants must not break (chiefly `MyWorkSummaryMode` — the existing template branch that already scopes the active page to the viewer when `?operated_by=me` resolves).

R1 narrows the scope: only `/active` (and its `?project=` variant) is default-on in Phase 1. `/search`, `/decisions`, `/p/{project}` get an explicit `me` toggle, no default. Every other surface is default-off and not in scope here. This doc therefore concentrates the propagation design on `handleActive` and references `handleSearch` only for "shape parity" — even though `/search` is default-off, the **resolution helper** must work identically there so the existing `?operated_by=me` URL contract keeps behaving the same way.

## What changes vs. today

The existing two handlers already resolve `?operated_by=` against the viewer email via `resolveOperatedByParam(raw, email)`. The diff for R3 is a *single conditional flip*: when `raw == ""` and the surface is default-on and `email != ""`, treat the request as if `raw == "me"`.

That is the entire server-side mechanic. Everything below is the contract around it: where the flip lives, what it must read, what it must NOT touch, and the second-order consequences (cache key, saved views, MyWorkSummaryMode, decision banners).

## 1. Insertion point: BEFORE `ListBeadsFiltered`

The default must be applied in the **handler**, after URL parsing and before the daemon call. Two reasons:

1. **The default depends on the viewer email**, which is a per-request value derived from `oidcauth.EmailFromRequest(r)`. The daemon does not know who is asking — and must not, because the daemon is a multi-tenant service shared with bridges, controllers, and the kd CLI. Pushing default-injection into `beadsapi.Client` or into the daemon would conflate a viewer-presentation concern with a service-level filter contract.
2. **Per-surface policy lives at the handler.** R1 enumerated 13 surfaces; only one is default-on. The most natural way to express "this surface gets the default and that one does not" is per-handler, not via a shared client wrapper.

Concretely, in `handleActive` (`gardenio/controller/cmd/beads-viewer/active.go:72-105`):

```
   email := oidcauth.EmailFromRequest(r)
   operatedByRaw := strings.TrimSpace(r.URL.Query().Get("operated_by"))

   // R3 server-side default-injection (proposed):
   //   - param missing AND viewer authenticated AND surface is default-on
   //   - URL value of any kind (including the literal empty "?operated_by=") wins.
   if !r.URL.Query().Has("operated_by") && email != "" {
       operatedByRaw = "me"
   }

   operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)
   // …existing IS NULL branch and ListBeadsFiltered call unchanged…
```

The placement matters in two subtle ways:

- **`r.URL.Query().Has("operated_by")` is the trigger, not `operatedByRaw == ""`**. The URL `?operated_by=` (R2's "show all" override) carries the key with an empty value, and `Has` returns true for it. Using `operatedByRaw == ""` would re-default on every "show all" link the user clicks — silently undoing their override on the next request and breaking R2's URL-is-the-source-of-truth contract. Lock this in tests in the implementation epic; it is the easiest knob to break.
- **The flip is *before* `resolveOperatedByParam`**, not after. We rewrite the *raw* param to `"me"` and let the existing alias resolver do its job. This keeps `isMe` honest (it stays the canonical "viewer effectively asked for self-scope" signal) and means `MyWorkSummaryMode` is set the same way for default-on as it is today for explicit `?operated_by=me` requests — no second code path.

## 2. Resolving the viewer email (`oidcauth.EmailFromRequest`)

`EmailFromRequest` is the right call site (oidcauth.go:48):

```
func EmailFromRequest(r *http.Request) string {
    if email := r.Header.Get("X-Auth-Request-Email"); email != "" {
        return email
    }
    return EmailFromContext(r.Context())
}
```

Two paths feed it:

1. **OIDC middleware** (`oidcauth.Middleware`, oidcauth.go:82-): on a verified token it stores `claims.Email` into the request context under `ctxEmail`. `EmailFromContext` reads it.
2. **ForwardAuth header**: the in-cluster `oauth2-proxy` / `forward-auth` upstream sets `X-Auth-Request-Email`. This is the production path for the viewer behind the auth-proxy.

For default-injection both paths must yield the same answer. Empirically the OIDC middleware path requires `IssuerURL` to be set; in dev (`make run-beads-viewer`, OIDC bypass) `IssuerURL` is empty and the middleware is a passthrough. So in dev the viewer email comes from the `X-Auth-Request-Email` header (or is empty).

Three operational consequences:

- **Empty email ⇒ no default.** `if email == "" { /* skip the flip */ }` is the explicit safety: anonymous viewers, static-token viewers, dev mode without the auth-proxy, and any future "service role" caller all keep the current "no filter" default. R1 §"Anonymous / static-token viewers" already records this expectation; R3 is the place where it becomes a single guard.
- **Header trust boundary.** `X-Auth-Request-Email` is trusted because the auth-proxy strips and re-sets it on every request before forwarding. The beads-viewer must never be exposed to the public internet without the auth-proxy in front of it (cf. `gardenio/docs/research/beads-viewer-public-internet-readiness.md`). R3 does not change that posture but it does *amplify* the cost of misconfiguration: a default-on filter that trusts a forged header would scope a malicious caller's view to "their" claimed email. The implementation epic's threat-model checklist must restate this.
- **Email vs. operator-id mismatch.** The default-injection writes `"me"` and lets `resolveOperatedByParam` resolve it to `viewerEmail`. The corresponding daemon column is `operated_by`, which can be raw email, `oidc:<email>`, or `slack:U…`. Today `resolveOperatedByParam` returns the bare email — so the daemon-side filter matches only rows whose `operated_by` is *exactly* that email (case-insensitive normalization is the daemon's job). The `oidc:<email>` rows that `operatorIDsMatchViewer` recognizes for the chip (operator_chip.go:142-155) are NOT matched by the list query. This is a pre-existing gap, not introduced by R3 — but default-on amplifies the "I see fewer rows than I expected" surprise. Flag it explicitly to R4 (migration) and to the implementation epic; resolution is either (a) daemon-side OR-match on the canonical id set, or (b) viewer-side multi-id filter via repeated `?operated_by=`. Out of scope for R3 to decide; in scope to call out.

## 3. AND-composition with `?project=` / `?status=` / `?type=`

The composition rule is **already AND**, structurally and unavoidably. The daemon translates `ListBeadsQuery` into URL-encoded `params` on `GET /v1/beads` (client_query.go:36-95):

| ListBeadsQuery field | URL param | Daemon semantics |
|---|---|---|
| `Statuses []string` | `status=<csv>` | Row matches if status ∈ set |
| `Types []string` | `type=<csv>` | Row matches if type ∈ set |
| `Labels []string` | `labels=<csv>` | Row matches if it carries ALL listed labels |
| `OperatedBy string` | `operated_by=<id>` | Row matches if `operated_by` == id |
| `OperatedByIsNull bool` | `operated_by_is_null=true` | Row matches if `operated_by IS NULL` |

All set fields are AND'd at the daemon. The handler does not control composition — it just sets the fields on a single `ListBeadsQuery` value. So:

- `/active?project=canonical-prod` (default-on) → `Labels=["project:canonical-prod"]` AND `OperatedBy=<viewer-email>`. The viewer sees their slice of canonical-prod, not the union of "canonical-prod beads" and "viewer's beads". This is the desired reading per R1 ("AND-composing `project` and `operated_by=me` is the expected reading").
- `/active?type=decision&status=open` (default-on) → daemon currently rejects `type` in `ListBeadsQuery` for the active page (the handler always sets `Statuses=["in_progress"]`; type is not parsed in `handleActive` at all). So a future `?type=` plumbing would compose AND with the default. No special case needed at the R3 layer — when the field is empty in the query value, the URL param is omitted (client_query.go:38, etc.), which is the daemon's "any" sentinel.
- `/active?operated_by=` (R2's empty-string "show all") → after the R3 flip-guard above, `operatedByRaw == ""` AND `r.URL.Query().Has("operated_by") == true`, so the flip is *skipped*. `resolveOperatedByParam("", email)` returns `("", false)`, and neither `OperatedBy` nor `OperatedByIsNull` is set on the query. The daemon returns the unfiltered set. Composition still works: `?project=foo&operated_by=` keeps the project filter and drops the operator filter. This is exactly what R2 wants.
- `/active?operated_by=null` (override → "unowned") → `resolveOperatedByParam` returns `("", false)`; the surrounding handler code sets `OperatedByIsNull=true` based on the literal-string check at active.go:101. The daemon then ANDs `operated_by IS NULL` with project / status. Same composition story.
- `/active?operated_by=other@example.com` (override → someone else's slice) → the URL value wins outright. AND'd with project / status. Default-injection is bypassed. This is the URL-as-source-of-truth contract.

There is **no OR path anywhere in the handler-→client-→daemon chain today**, and R3 introduces none. The `Labels []string` field is the only place the contract could surprise: it is "row carries ALL listed labels" (intersect), not "row carries ANY". `?project=` only ever appends a single `project:<name>` label, so this is moot for R3, but it is worth noting in the implementation epic in case a future `?label=` multi-value caller assumes OR.

## 4. `MyWorkSummaryMode` interaction (active.go:86)

The existing template flag is set from the resolver's `isMe` return:

```
operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)
…
"MyWorkSummaryMode": isMe,
```

`MyWorkSummaryMode` triggers the R8 §5.4 template branch that re-frames the page header from "Active" to "Your active work" (or similar; the exact copy is R2's call). Today it lights up only when the URL explicitly carries `?operated_by=me`.

With the R3 flip, the *raw* param is rewritten to `"me"` before the resolver is called, so `isMe` lights up on the cold-load default too. **This is the desired behavior**: a default-on cold load IS the "my work summary" mode by definition. The header chip ("showing your work — switch to all / unowned") that R2 specifies relies on this signal to render. No new flag is required; `MyWorkSummaryMode` continues to be the single source of truth for "the page is currently scoped to the viewer".

Two preservation requirements:

- **`isMe` must NOT light up when the URL carries an explicit non-`me` value.** `?operated_by=other@example.com` and `?operated_by=null` and `?operated_by=` (empty) all return `isMe=false` from `resolveOperatedByParam`. The R3 flip happens *only* on the missing-key path, so it cannot accidentally toggle `isMe` for those cases. This is enforced by the trigger condition (`!r.URL.Query().Has("operated_by")`) and is the second knob the implementation tests must cover.
- **The `OperatedByActive` flag (active.go:85) becomes always-true on default-on cold loads.** Today it is set from `operatedByRaw != ""`; after the flip rewrites raw to `"me"`, the cold-load default-on case has `OperatedByActive == true`. R2 already assumes this — the chip strip wants to render the `"showing your work"` indicator on the cold load. This is correct behavior; just call it out so the implementation epic does not regress on it.

The same wiring works for `handleSearch` if/when `/search` ever flips to default-on (R1 says no, but the composition is identical) — no per-handler divergence.

## 5. Cache-key implications

**This epic does not own caching strategy.** R3's job is to surface the implications so the team that owns the viewer's caching layer (today: nobody — the daemon list is per-request HTTP) can plan around them.

Today the relevant facts are:

1. The viewer is **stateless per request**. Every `/active` GET hits `s.daemon.ListBeadsFiltered`. There is no in-process LRU on the daemon URL.
2. The daemon is a Go HTTP server (`kbeads`) backed by PostgreSQL. There is no HTTP cache layer (no Varnish, no CDN). `Cache-Control` is not set on `/v1/beads` responses — the response is expected to be fresh.
3. Browser caching of `/active` HTML responses is governed by whatever `Cache-Control` (or absence of it) the viewer emits. The implementation epic should explicitly emit `Cache-Control: private, no-store` on default-on surfaces (see below) — viewer-scoped HTML must never land in a shared cache.

What changes with default-on:

- **The effective request becomes a function of the viewer.** Two users hitting the same URL `/active` get different responses. Today, with default-off and no `?operated_by=`, the response is *not* viewer-dependent. After R3, it is.
- **Any cache key that does not include the viewer is now incorrect.** Concretely:
  - **Browser cache.** A shared family laptop, a behind-NAT corporate proxy, a misconfigured CDN — anything that caches `/active` without keying on `Cookie` / `Authorization` / `X-Auth-Request-Email` would serve user A's view to user B. The mitigation is `Cache-Control: private, no-store` on the response; the implementation epic should make this explicit and the deployment team's CDN config (if any is ever added in front of the viewer) must respect it.
  - **In-process daemon-list cache (hypothetical future).** If anyone ever puts an LRU in front of `s.daemon.ListBeadsFiltered`, the cache key is today the URL params (`status`, `type`, `labels`, `operated_by`, …). Default-on means `operated_by` is now part of the key for the cold-load case — which is fine, *as long as the cache derives the key from the resolved `ListBeadsQuery`*, not from the inbound HTTP URL of the viewer request. If the cache keys off the inbound URL ("`/active`" with no operator) the cache will collapse different viewers' results onto a single key. Lock this in the implementation: cache keys are derived from the *outbound* daemon URL, not the *inbound* viewer URL.
  - **NATS / SSE invalidation.** The daemon emits bead-lifecycle events on NATS; the viewer is a polling reader, not a subscriber, so no per-viewer subscription churn is added. Default-on does not change this story.
- **Cardinality explosion is bounded.** The number of distinct viewer-emails is bounded by OIDC sign-ups (small — single-digit team today, dozens at most). Default-on does not produce a long-tail of synthetic keys.
- **The `?operated_by=` URL contract is the cache-friendly path.** Once the user clicks "show all" (`?operated_by=`) or "show me" (`?operated_by=me`), the URL is fully self-describing. Caches that key on URL+`Vary: Cookie` work correctly for the explicit-override case. The cold-load default is the ONLY case where the URL is viewer-dependent in a way the URL itself does not express. The implementation epic should consider whether to canonicalize the cold load by 301-redirecting `/active` → `/active?operated_by=me` for OIDC viewers — this would make the cold-load URL self-describing too, at the cost of a redirect on first hit and a slight UX regression (the URL bar reveals the default). My recommendation here is **do not redirect** — keep the cold URL clean and rely on `Cache-Control: private` — but the choice is the implementation epic's, not R3's.

## 6. Saved-view exception (cross-ref R1 §"Open questions")

`/views/{id}` (`handleViewShow`, views.go:287) calls `s.daemon.ListBeadsFiltered(r.Context(), spec.toQuery())`. The `savedViewSpec.toQuery()` builder (views.go:54) does NOT consult the viewer email or the URL query, by design — saved views are user-authored persisted queries.

**R3 must NOT inject the default into the saved-view path.** The flip lives in `handleActive` only. If a future `/active`-via-saved-view code path is added (e.g. "load my view named 'standup' on the active page"), the spec must be loaded *first* and only fall back to the cold-load default when the spec carries no `OperatedBy` field. R1 already records this; R3 ratifies the same boundary by placing the flip in the route handler, not in any shared `beadsapi.Client` wrapper.

If a future MR adds an `OperatedBy` field to `savedViewSpec`, the rule is: spec-set value wins, R3 default does nothing. This is the same precedence rule as URL-explicit-wins — the saved view IS an "explicit" choice, expressed once at save time.

## 7. Decision-banner / Queued-count side queries

`handleActive` issues two best-effort sibling daemon calls (active.go:116-130):

1. `decisions, err := s.daemon.ListBeadsFiltered(... Types: ["decision"], Statuses: ["open"] ...)` — populates the decision banner count.
2. `queuedCount := s.countQueuedBeads(r, projectFilter, includeScheduled)` — populates the "Queued" tile.

Neither call sets `OperatedBy`. They are **fleet-wide counts** today.

R3 question: should the default-on `me` filter scope these counts too, so the banner reads "you have N decisions" instead of "the system has N decisions"?

Recommendation: **no, keep them fleet-wide** (do not propagate the default into the decision/queued counts). The decision banner exists so an operator notices that *something* needs human input, regardless of who initiated it; scoping it to "decisions I requested" hides exactly the alerts the page exists to surface. Same logic applies to Queued. R1 already excludes `/decisions` from default-on for this reason — R3 just extends the same logic to `/active`'s sibling counts.

The implementation epic should:
- Leave the two sibling calls' `ListBeadsQuery` values untouched (no `OperatedBy` injection).
- If the team later wants per-viewer counts, add a *separate* tile (e.g. "decisions waiting on you") rather than rewriting the existing fleet-wide tile.

This is also the right place for the implementation tests to assert: "a default-on `/active` GET issues exactly one operator-scoped daemon call (the main list) and at least two non-operator-scoped sibling calls (decisions, queued)".

## 8. Putting it all together — handler-pseudocode reference

For `handleActive` (the only Phase 1 default-on surface):

```text
projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
includeScheduled := isTruthy(r.URL.Query().Get("include_scheduled"))
operatedByRaw := strings.TrimSpace(r.URL.Query().Get("operated_by"))

email := oidcauth.EmailFromRequest(r)

// R3 default-injection. Order matters:
//   1. URL key present (any value, incl. "") → URL wins, skip the flip.
//   2. URL key absent AND viewer authenticated → flip raw to "me".
//   3. URL key absent AND no viewer → leave raw empty (today's behavior).
if !r.URL.Query().Has("operated_by") && email != "" {
    operatedByRaw = "me"
}

operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)

// Existing IS-NULL branch is unchanged. After the flip the literal string
// "null" can only arrive via an explicit URL override, so this stays correct.
operatedByIsNull := operatedByRaw == "null" || strings.EqualFold(operatedByRaw, "(none)")

q := beadsapi.ListBeadsQuery{
    Statuses:         []string{"in_progress"},
    Limit:            activeListLimit,
    ExcludeScheduled: !includeScheduled,
}
if projectFilter != "" {
    q.Labels = []string{"project:" + projectFilter}
}
if operatedByIsNull {
    q.OperatedByIsNull = true
} else if operatedBy != "" {
    q.OperatedBy = operatedBy
}

// Decision/queued sibling calls remain fleet-wide; do NOT propagate
// operatedBy into them (see §7).
```

`MyWorkSummaryMode` and `OperatedByActive` continue to read from `isMe` and `operatedByRaw != ""` respectively — no template-side changes are required for R3 itself. R2's chip-strip changes ride on those existing flags.

For `handleSearch`: **no R3 change**. Search is default-off (R1). The existing `?operated_by=` resolution path is correct as-is. The implementation epic should still factor a tiny `applyDefaultOperatedBy(r, email, surface) string` helper out of `handleActive` so that if a future surface flips to default-on, the call site is two lines, not a copy-paste of the order-sensitive condition.

## 9. Feed-forward to the implementation epic

These are the items R3 surfaces but does not own:

1. **Test coverage for the flip-guard order.** `?operated_by=` (empty, present) MUST NOT re-default. `?operated_by=other` MUST override. Missing key + no viewer email MUST keep "all". Missing key + viewer email MUST flip to `me`. These four cases are the contract.
2. **`Cache-Control: private, no-store`** on default-on responses. R3 §5 explains why; the implementation epic should add the header in `s.render` (or the route mux) for `/active` only, and verify with a header assertion in the implementation tests (not R3's deliverable).
3. **`oidc:<email>` vs raw-email row-match gap** (§2). The viewer chip recognizes both; the daemon list filter only matches the bare email today. Default-on amplifies the surprise. Either widen the daemon match (preferred) or have the handler issue OR'd queries (not preferred — N round-trips). Bead a separate research/decision under R4 / the implementation epic.
4. **Helper extraction.** The flip is ten lines. Factor it (e.g. `defaultOperatedByForActive(r, email) string`) so the rule lives in one place even if `handleSearch` never adopts it.
5. **Saved-view `OperatedBy` field** (R1 follow-on). Out of scope for R3, but the implementation epic that adds it must update `parseViewSpec` / `toBeadFields` and the spec-edit form. The R3 boundary holds: if `spec.OperatedBy != ""`, no R3 default applies.
6. **Sibling-count semantics** (§7). Do not propagate the default into decision/queued counts. If a future "decisions for me" tile is wanted, add a new tile rather than rewriting the existing one.

## 10. What is explicitly out of scope

- **Caching strategy.** R3 only documents the cache-key implications of default-on. Whether to add an HTTP cache, a daemon-side LRU, or a `/v1/beads` ETag handshake is a separate epic.
- **Test plan.** Per the parent epic's hard rule, R3 produces no unit/e2e tests. The four-case contract in §9 item 1 is the test brief; the implementation epic owns the actual tests.
- **OIDC sign-in flow.** R3 assumes `EmailFromRequest` works. The auth-proxy / OIDC middleware are owned by `oidcauth` and the deployment chart.
- **Operator-id canonicalization.** Whether `operated_by` should be normalized to `oidc:<email>` at write time (vs. the current heterogeneous mix of bare email / `oidc:` / `slack:U…`) is a transitivity-epic concern, not a viewer concern.
- **Storage of a per-user "last filter choice".** R2 covers persistence (cookie vs. user_pref bead). R3 does not depend on it — the cold-load default applies whether or not persistence ships.

## Links

- Bead: ka-0000001699 (this task)
- Parent epic: ka-0000002754
- Sibling: ka-0000000145 (R1, closed; doc landed via MR !2005)
- Code touchpoints:
  - `gardenio/controller/cmd/beads-viewer/active.go` (handler — R3 flip lives here)
  - `gardenio/controller/cmd/beads-viewer/search.go` (handler — no R3 change, but shape parity)
  - `gardenio/controller/cmd/beads-viewer/operator_chip.go` (`resolveOperatedByParam` — unchanged)
  - `gardenio/controller/cmd/beads-viewer/views.go` (`savedViewSpec.toQuery` — must NOT pick up the default)
  - `gardenio/controller/internal/oidcauth/oidcauth.go` (`EmailFromRequest` — R3 calls it; no change)
  - `gardenio/controller/internal/beadsapi/client_query.go` (`ListBeadsQuery` shape — composition is AND-by-construction)
- R8 prior art: `docs/research/human-attribution-ux.md` (root `docs/`, not `gardenio/docs/`).
