# beads-viewer image rendering — Option 3 (proxy) vs Option 4 (public mirror)

Research/plan doc for parent epic [ka-0000002532] — "beads-viewer image rendering:
fix S3 access denied (private bucket -> browser <img> 403)".

Task: [ka-0000001754]. Predecessor research: [ka-0000002002] (closed). Predecessor
implementation: [ka-0000006131] (closed — proxy route already merged).

## TL;DR

**Keep Option 3 (beads-viewer `/image/{key…}` proxy) as the default and only
render path.** It is already shipped and authz-parity with the rest of
beads-viewer. Do **not** stand up a separate public bucket (Option 4) at this
time — its only real win is Slack/external unfurl, and that need has not
materialised. File Option 4 as a deferred follow-up that we can take when a
concrete unfurl ask appears.

This doc exists to ratify that posture with the operator so we can close the
epic on a recorded decision rather than on "the closed implementation task
happens to imply it."

## Current state

Option 3 was picked on 2026-05-20 by the operator on Slack ("agreed, proxy
now") and shipped under [ka-0000006131]. The code today:

- Proxy handler — `gardenio/controller/cmd/beads-viewer/image_proxy.go`
  (`handleImage`, route `GET /image/{key...}` registered in
  `server.go:790`). Streams the S3 object using the controller's IRSA creds,
  pass-through `Content-Type` / `ETag`, `Cache-Control: private, max-age=300`,
  conditional GET via `If-None-Match → 304`.
- Authz — registered without an explicit role gate, in-parity with the
  bundle page itself (ka-0000007953). The viewer's OIDC chain still
  authenticates the request; an anonymous browser does not reach the
  handler.
- Key authz — only keys under `<COOP_S3_PREFIX>/images/` (default
  `coop/sessions/images/`) are served. Out-of-prefix requests get `403`.
  Defense in depth on top of the private bucket.
- Renderer rewrite — `bundle_renderers.go` rewrites
  `gardenio-sessions.s3.amazonaws.com/<key>` (virtual-hosted) and path-style
  S3 URLs to `<basePath>/image/<encoded-key>` at render time, both for the
  dedicated `format=image` doc renderer and inline markdown images. Non-S3
  hosts continue through the existing whitelist.
- Wiring — `main.go:143` wires `COOP_S3_BUCKET` + `COOP_S3_REGION` +
  `COOP_S3_PREFIX` into `Server.SetImageProxy`. When `COOP_S3_BUCKET` is
  unset (typical local dev without AWS creds) the handler returns
  `503 Service Unavailable` so the misconfiguration is loud.

Smoke status: the test bundle [ka-0000004889] now renders end-to-end in a
logged-in browser session.

## Option 3 — proxy via beads-viewer (current default)

**Mechanism.** `<img src>` points at the beads-viewer same-origin URL
`/image/<key>`. The controller fetches the S3 object with IRSA, streams it
to the browser, and applies a short private `Cache-Control`.

**Security.**
- Bucket stays private. S3 GET still requires IAM, and only the controller
  has IRSA-bound perms.
- Every request flows through the OIDC chain — anonymous viewers do not
  receive image bytes. Authz posture is exactly the bundle page's.
- Per-bead ACL is possible later by extending the handler (e.g.
  decode bead-id from key prefix → check viewer's project memberships).
  Not implemented today, but the architecture allows it without changing
  URL shape.
- Defense in depth: prefix check refuses anything outside
  `coop/sessions/images/`.

**Auth parity.** Same as the bundle HTML itself. A user who can read a
bundle can render its images; a user who cannot, cannot. No new authz
surface to think about.

**Ops cost.**
- Bandwidth: every image byte transits the controller pod. Today's payloads
  are screenshot-sized (median <500 KiB, capped at 10 MiB per upload), so
  this is comfortably absorbed by existing controller capacity. If/when we
  start uploading large diagrams or video, we revisit (see "Open future
  work" — CloudFront+OAC).
- Latency: one extra hop (browser → controller → S3 → controller →
  browser) vs. a direct S3 GET. Acceptable for in-browser image render
  because the OIDC chain already costs one hop.
- Cacheability: short private cache (`max-age=300`, `ETag` pass-through).
  Conditional GETs short-circuit the S3 fetch.

**Migration effort.** Zero. Already in production. Existing private S3
objects work without re-upload, without bucket-policy edits, without URL
churn in already-rendered HTML or Slack pastes (the rendered viewer URL
points at the proxy; the underlying S3 URL is rewritten at render time,
not stored).

**Failure modes.** Documented and tested:
- `503` when proxy is unwired (no `COOP_S3_BUCKET`).
- `403` for out-of-prefix keys (defense in depth).
- `404` for `NoSuchKey` / `NotFound`.
- `502` for transport errors, with AWS error body stripped.
- `304` on If-None-Match.

**External paste behaviour.** Slack will not unfurl `/image/<key>` URLs
because the proxy requires an authenticated OIDC session; the unfurl
request comes from Slack's bot, which has no session. **This is fine for
the bundle-render path** — bundles open in beads-viewer in a browser,
where the operator is already logged in. Pasting a viewer image URL into
Slack today shows a link, not an inline preview. That is a known
limitation, not a defect.

## Option 4 — separate public bucket (or public-prefix opt-in)

**Mechanism.** Either (a) a second bucket with a `public-read` ACL whose
contents are world-readable, or (b) an opt-in `gb image upload --public`
flag that writes under a tightly-scoped `coop/sessions/images/public/`
prefix with a bucket-policy carve-out. Default uploads still go through
Option 3.

**Security.**
- Anything written to the public path is world-readable forever. Hard to
  revoke per-object once it ships — the URL can be cached anywhere
  (Slack, Wayback, Google).
- Easy to misuse. An agent that defaults to `--public` to dodge a render
  bug will leak a screenshot containing internal hostnames, Slack
  handles, or env-specific error strings before code review catches it.
- Two paths means two threat models. We have to remember which is which
  every time we add a feature (renderer, retention, audit).

**Auth parity.** Breaks parity. Anonymous viewers (including Slack
unfurl, search engines, third parties with the URL) can read these
images. The operator must explicitly remember "public means really
public" at upload time.

**Ops cost.**
- Bandwidth/latency: lower than the proxy for the public hop (S3 →
  browser direct), but the default path stays on the proxy, so the
  controller pod load is unchanged for normal use.
- Bucket / policy footprint: a new bucket (or a new policy on the
  existing bucket) to maintain, document, and audit. Adds infra TF and
  a new env var (`COOP_S3_PUBLIC_BUCKET` or similar).
- Retention: a second lifecycle policy. Without one, public objects
  accumulate forever.
- Migration cost is asymmetric: shipping the bucket is cheap; undoing
  a leak that ended up in Slack history is impossible.

**Migration effort.** Moderate.
- Provision public bucket + bucket policy in TF (`gardenio/tf` /
  `terragrunt`).
- New env var (`COOP_S3_PUBLIC_BUCKET`).
- `gb image upload --public` flag, separate S3 client/writer, separate
  prefix derivation (probably its own `s3PublicPrefix()` rather than
  reusing `s3Prefix()`).
- Renderer: existing whitelist already permits `s3.amazonaws.com` so a
  public-bucket URL would render directly; no rewrite needed. But: the
  whitelist would need to learn the public-bucket hostname (or stay
  generic).
- Docs: explicit "public means really public" warning, plus an agent
  advice bead to deter unintentional use.
- Operator process: a review path for what is allowed to be public
  (probably none, in practice).

**What it would buy us.**
- Slack-native unfurl for shared screenshots/diagrams.
- Embedding in external docs that beads-viewer auth can't reach (e.g.
  GitLab MR descriptions opened by reviewers without OIDC).
- Render path that works in static-site exports (none planned today).

None of these are open asks. The Slack-unfurl ask in particular has
appeared zero times since the proxy shipped on 2026-05-20; the request
that originally drove the epic (operator opening a bundle in a browser)
is fully satisfied by Option 3.

## Trade-off matrix

| Axis                | Option 3 (proxy, today)                            | Option 4 (public bucket / prefix)                          |
| ------------------- | -------------------------------------------------- | ---------------------------------------------------------- |
| Security blast      | Low. Bucket private; OIDC-gated.                   | High. Public objects are forever; leak risk per upload.    |
| Auth parity         | Exact parity with bundle page.                     | Breaks parity. Anonymous can read.                         |
| Per-bead ACL        | Possible later, same URL shape.                    | Not possible — URL is the auth.                            |
| Slack/Web unfurl    | No (acceptable today).                             | Yes (the only real win).                                   |
| Ops surface         | One handler, already shipped.                      | New bucket/policy, env var, CLI flag, advice bead, audits. |
| Caching             | Private `max-age=300` + ETag, controller-shaped.   | Public CDN-cacheable, but adds CDN to threat model.        |
| Bandwidth           | Controller hop on every image.                     | Direct S3 (cheap) for public; proxy still on the default.  |
| Migration cost      | Zero — already in production.                      | Moderate (TF + CLI + docs + process).                      |
| Reversibility       | High. Toggle handler off, switch render path.      | Low. Leaked URLs can't be unleaked.                        |
| Open user demand    | Satisfied (bundle renders end-to-end).             | Zero confirmed ask.                                        |

## Recommendation

**Adopt Option 3 as the canonical and only image-render path.** Do not
ship Option 4 now. File Option 4 as a deferred follow-up
(`status=open`, no gardener assignment) under the parent epic; pick it up
only when:

1. A concrete user (operator, gardener, integration) asks for Slack/web
   unfurl with a stated use case, and
2. We have a written "what counts as publishable" policy that an agent
   can be required to follow before invoking `--public`.

Until both gates are met, the cost (security blast radius, dual code
paths, agent foot-guns) outweighs the benefit (a feature nobody has
asked for since the proxy shipped).

If/when image volume or payload size makes the controller a bottleneck,
the right next step is **not** Option 4 — it is the CloudFront+OAC+
CF-Function escape hatch already gestured at in [ka-0000006131]
("a separate follow-up epic will migrate to CloudFront+OAC+CF-Function
once we want CDN caching for larger media"). That preserves auth parity
while adding edge caching, and is strictly compatible with the proxy URL
shape.

## Open future work (file as children of ka-0000002532 only when needed)

These are deliberately **not** filed today; they are noted here so the
operator can authorise them at the same time as ratifying Option 3 if
desired.

- **Per-bead ACL on the proxy.** Extend `handleImage` to parse the bead
  id from the key prefix (`coop/sessions/images/<bead-id>/…`) and check
  viewer membership before streaming. Today the prefix check is the
  only key authz; OIDC-authenticated viewers can read any image whose
  URL they know.
- **CloudFront + OAC migration.** Add a CDN in front of the proxy URL,
  preserving the `/image/<key>` shape. Useful only when payloads or
  request rate make the controller pod the bottleneck.
- **Slack unfurl bridge.** If/when an unfurl ask materialises, the
  cheaper alternative to Option 4 is to mint a one-shot presigned URL
  from a Slack-side bridge using its own IAM identity, instead of
  exposing a public bucket.

## Acceptance for this research task

- This doc lands on a feature branch + MR (no main push). ✔ (this commit)
- A `gb decision` is filed on [ka-0000002532] asking the operator to
  ratify Option 3 as the canonical path and defer Option 4. (next step)
- A comment on [ka-0000002532] links the doc and the decision. (next step)
- No runtime code is modified. ✔ (docs-only)

[ka-0000002532]: https://beads-viewer.e2e.example.com/bead/ka-0000002532
[ka-0000001754]: https://beads-viewer.e2e.example.com/bead/ka-0000001754
[ka-0000002002]: https://beads-viewer.e2e.example.com/bead/ka-0000002002
[ka-0000006131]: https://beads-viewer.e2e.example.com/bead/ka-0000006131
[ka-0000004889]: https://beads-viewer.e2e.example.com/bundle/ka-0000004889
