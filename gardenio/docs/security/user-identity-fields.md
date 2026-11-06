# User identity fields are admin-only

Tracking: gd-uAvleAZUvV (epic gd-00NASU92b2). Applies to the beads daemon (`kbeads`).

## Why

A `type:user` bead says who a person is: its `email`,
`slack_id`, `gitlab_username`, `github_username` and `jira_user_id` become
`operator_aliases` rows (the controller bridge's alias resync), and
`allowed_roles`, `allowed_projects`, `capabilities` and `auth_methods` are the
person's access grants. (`email_aliases` is **not** an identity field any more: it
is a display label, still admin-written, and nothing derives an alias, a sign-in,
an approver match or a link from it. gd-L55f9EYFjN: one person is one identity, an
email is a label, not a credential. An admin who needs a second address attributed
to the same operator links it explicitly with `link-aliases`; that row has its own
source and is not pruned with the user-bead ones.) The shared static token is held by every agent pod, and
fleet tokens by every bridge. If those could write these fields, any agent could
attach its own identity to the owner's operator and act as them.

## The rules

1. **Identity fields need an OIDC `gardenio-admin`.** The field set is
   `userIdentityFields` in `kbeads/internal/server/user_identity_authz.go`.
   Static and fleet tokens get **403** when they change one. They keep the
   allow-listed non-identity writes on a user bead (`status_message`,
   `profile_prefs`, inbox last-visit stamp, `display_name`, ...), which still
   return 200.
2. **One gate, every write path.** `createBead`/`updateBead` (PATCH, POST,
   gRPC, undo, escalation replay), the single-field endpoints (`increment`,
   `fields/{f}/append`, `reorder`, `edit`), the extra fields on `close`, and
   `mergeBeadFields` all funnel into `denyUserIdentityFields`.
   `TestFieldMutationEndpoints_UserIdentityGate` drives every one over HTTP as the
   static token, and `TestStoreFieldWriters_AreGated` fails if a new store
   field-write call site appears that is not routed through the gate.
3. **Machine tokens do not choose aliases.** `POST /v1/admin/operators/link-aliases`
   takes an explicit `aliases` list only from an OIDC `gardenio-admin` (who may
   also merge operators). A static or fleet token sends `{"resync_bead_id": "<user bead>"}`;
   the daemon reads the aliases from that bead (so they are exactly what an admin
   wrote), always in no-merge mode (409 `alias_collision` if the aliases already
   belong to different operators; `MergeOperators` is never called).
4. **Revocation is computed server-side.** `POST /v1/admin/operators/prune-aliases`
   for `source=user-bead-bridge` builds its keep-set from the user beads; the caller
   cannot supply one (403 for a machine token, 400 for an admin). An empty
   computed set prunes nothing. The bridge calls it once per complete scan.

## Terms acceptance fields

`terms_version` and `terms_accepted_at` (Lands step 7.4, gd-eKqTeoVQO1) record
which version of the terms a person accepted and when. They are **not** identity
fields: no alias, sign-in or grant derives from them. They are evidence of what a
person agreed to, so they share the identity fields' write gate (`userTermsFields`
joins `isGatedUserField`): a generic write needs an OIDC `gardenio-admin`, and a
static or fleet token gets 403.

The person records their own acceptance with `POST /v1/users/me/terms` and body
`{"terms_version": "<id>"}`. The route needs no role (a Lands applicant holds
none), refuses every non-OIDC caller, writes only those two keys on the user bead
the caller's own token resolves to, and stamps `terms_accepted_at` itself. The
request names no bead, so it cannot be aimed at someone else's. `GET /v1/users/me/terms`
reads the same two keys back under the same caller rule (empty for someone who never
accepted), which is how the viewer's `/lands/accept` page knows whether the current
version is accepted.

## Read cursors

`read_cursors` (Lands Commons step 4.5, gd-q20kp0MRzh) maps each Commons channel
to the last message the person read there. It is not an identity field either,
but it shares the same write gate (it joins `isGatedUserField`): a static or fleet
token gets 403 on PATCH, gRPC `UpdateBead` and the single-field endpoints, so an
agent pod cannot mark someone else's channels read. The person writes their own
through `POST /v1/users/me/read-cursors`, which resolves the caller's own bead,
caps the map and checks every id. The key is also skipped when references are
extracted from `fields`, so a message's mentioned-by never shows who has read it.

## Standing consent to push

`consent_push_to_mr_auto` (gd-QV2WuUjely) is a person's standing consent to have
prepared CI fixes pushed to their MRs without a `gb consent-to-push` prompt. It was
a key inside the free-form `preferences` blob, which the static token may write, and
the agent CLI itself closed the decision with `push-to-my-mr` chosen, so any agent
could consent for anyone and answer as them. Now:

- It is a top-level boolean field in `userStandingConsentFields`, which joins
  `isGatedUserField`: a generic write needs an OIDC `gardenio-admin`
  (`gb consent-to-push-manage grant|revoke`), and a static or fleet token gets 403.
  The person sets their own with `POST /v1/users/me/consent-push` and body
  `{"auto": true|false}` (`gb consent-to-push-manage opt-in|opt-out`), which follows
  the read-cursors caller rule: own OIDC token, a gardenio role or a land, own bead.
  A `consent_push_to_mr_auto` key inside `preferences` is ignored.
- The daemon answers. When a decision carrying `consent_push: "true"` and a
  `push-to-my-mr` option is created addressed (`operated_by`) to exactly one open
  user bead holding the consent, `createBead` closes it with `chosen: push-to-my-mr`,
  `responded_by` and `resolver_subject` `system:standing-consent`, `resolver_admin:
  false` and the server-stamped `standing_consent_user`. A `system:*` responder is
  never a human answer, so it unlocks no privileged continuation respawn.

## Welcome tour

`welcome_tour_seen_at` (Lands Track B step 9.14b, gd-aEIUgpyxpi) is when the person
finished or dismissed the `/garden` welcome tour, so a second device does not run it
again. It joins `isGatedUserField` too, so an agent's token cannot mark someone's tour
seen (or clear it so it runs again). The person reads it with `GET` and stamps it with
`POST /v1/users/me/welcome-tour` (no body; the daemon writes the time, and only the
first time), under the read-cursors caller rule: own OIDC token, a gardenio role or a
land, own bead. beads-viewer makes both calls on the person's bearer alone
(`garden_welcome.go`).

## What changed for callers

| Caller | Before | Now |
|---|---|---|
| bridge `ResolveBySlackEmail` slack_id backfill (`user_directory.go`) | wrote `slack_id` on the user bead | **Removed** (gd-W4FkF80BlX). The Slack profile email is only a hint (`SlackEmailHint`) for the mention-gate denial message; it never maps a Slack id to a user bead, in memory or on the bead. The person links (`@gardenio link`) or an admin sets `slack_id`. |
| `gb identity add` / `gb identity seed` | created user beads with any identity field | **403** for static/fleet tokens. Run them with an OIDC `gardenio-admin` login, or ask an admin to set the fields (the denial is marked escalatable, so an operator can approve the write). |
| bridge alias resync | sent the alias list it extracted | sends the bead id; the daemon extracts |
| `inviteservice.BeadsOperatorLinker` | static token, explicit `[oidc, ziti]` aliases | **Deleted** with the inviteservice (2026-10-31, gd-Y4ZCGr2m2t); sign-in invites go through Keycloak and beads-viewer (gd-ycE484kLyk). |
