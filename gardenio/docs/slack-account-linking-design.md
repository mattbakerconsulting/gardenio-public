# Slack acts as the linked person: one-time account linking, no header relay

Status: **decided and built** (decision gd-uPHB7YcHuI resolved `a-approve` 2026-10-14: option A
plus the Slack approval door; see [§5](#5-the-choice-how-the-bridge-obtains-the-persons-token)
and [§10](#10-as-built-deviations-from-this-spec)). Epic gd-UdM4LnnaGE, thread
gd-OvrPW5UlHG. Owner direction 2026-10-13 (decision gd-ZsfxvXRiyv, "tight", amended): a human
acting through Slack must reach kbeads with a token **Keycloak issued for that person**. A
service-asserted identity (an `X-Operated-By` relay, or the HMAC edge envelope of
gd-bn2XZleUVw) is not acceptable for humans. This document is the "short design spec" that
direction asked for. It is docs only; the build follows the choice.

Sibling of [machine-principal-design.md](machine-principal-design.md) (the **machine door**: the
bridge's own autonomous writes stay on `machine:gardenio-slack-bridge`, Task 5 gd-Hc34EENbO9).
This is the **Slack human door**.

## 1. Goal

When a person acts in Slack (approves an escalation, answers a decision, replies to an agent,
runs a bot command that writes), slack-bridge sends kbeads an `Authorization: Bearer` access
token Keycloak minted for that person, through a dedicated `gardenio-slack-link` client. kbeads
authorizes and attributes the write exactly as it does for the same person in the viewer
(`oidc:<email>`, their realm roles). The bridge may act only for people who completed a one-time
account link; everyone else gets a "Link your gardenio account" prompt instead of a write.

Done when (live on k3s, from the task):

- an escalation approved from Slack by a linked owner shows `oidc:<owner>` as approver;
- an unlinked Slack user's write is refused with the link prompt;
- a forged or replayed link callback is refused;
- after unlink, the next write is refused;
- then gd-ScDm0mEFmq (Slack escalation approve) closes.

## 2. What exists today (verified in the tree)

| Fact | Where |
|---|---|
| Slack human writes are stamped with `X-Operated-By` and, when a `PersonRoleSource` is set, signed as a v2 HMAC edge envelope carrying roles the bridge read from the Keycloak Admin API. On k3s neither the HMAC key nor `slackBridge.keycloak` is configured, so writes go out on the bridge's fleet token with a `slack:U…` operator header. | `controller/internal/bridge/bot_person_identity.go`, `bridge/keycloak.go`, `deploy-values/gardenio-k3s.yaml` `slackBridge:` |
| The Slack-id to person binding is the `slack_id` field of an admin-written `type:user` bead (`write_policy.min_role: gardenio-admin`). The bridge resolves it through `UserDirectory().LookupBySlackID`. | `kbeads/internal/server/config.go` `type:user`; `bridge/bot_person_identity.go` |
| kbeads verifies every OIDC token against ONE audience, `keycloak.oidcAudience` = `gardenio-traefik-forwardauth`. Every other realm client (`gardenio-cli`, `gardenio-controller`, `gardenio-authz-canary`) gets there with an `audience-self` protocol mapper that adds that audience. | `kbeads/internal/server/oidc.go` `NewOIDCVerifierWithDiscovery`; `deploy-values/gardenio-k3s.yaml` `keycloak.clients` |
| For ordinary writes kbeads does **not** gate on `azp`. The token's `azp` is only consulted for (1) escalation approve (`approverClients`, default = the audience client) and (2) trusting `X-Auth-Request-Email/Groups` (`forwardAuthClients`). A human token from any realm client with the right `aud`, an `email` claim and `realm_roles` writes as that human. | `kbeads/cmd/kd/serve.go` lines ~305-325; `kbeads/internal/server/escalation_approval_authz.go`; `interceptors.go` (`ClientID` uses) |
| The daemon client already has a per-request user-bearer path (`WithUserBearer`, `SetRequireUserBearerForWrites`), live for the viewer behind the edge sign-in. The bridge can reuse it unchanged. | `controller/internal/beadsapi/user_bearer.go` |
| Keycloak on k3s is **26.0.7** (`keycloak.runtime.image.tag`), issuer `https://auth.clonk.tv/auth/realms/gardenio`, served by the public-edge nginx which exposes only `/auth/realms/gardenio/` and `/auth/resources/`. Realm: access tokens 5 min, offline session idle 30 d, offline max lifespan disabled, refresh-token rotation off. | `helm/gardenio/values.yaml` `keycloak.runtime.image`, `keycloak.realm.sessions`, `tokenSettings`; `helm/gardenio-ziti/public-edge.yaml` |
| The bridge has no public URL on k3s. `slackBridge.ingress.enabled` is false; Slack reaches it over Socket Mode (`appToken`). The only public host that fronts in-cluster services is `gardenio.clonk.tv` (viewer-authproxy nginx, `helm/gardenio-ziti/viewer-authproxy.yaml`), whose `location /` sits behind the oauth2-proxy `auth_request` restricted to `/gardenio-admins`. | `helm/gardenio/values.yaml` `slackBridge.ingress`; `viewer-authproxy.yaml` |
| The Slack role ceiling: a Slack interaction never carries `gardenio-admin` (decision gd-8PT2Qr2gEo, `slackRoleCeiling = gardenio-operator`). Realm roles are **not** composite: `gardenio-admin` does not include `gardenio-operator`. | `bridge/bot_person_identity.go`; `helm/gardenio/values.yaml` `keycloak.realm.roles` |
| kbeads already holds a Keycloak Admin client (`KeycloakAdminClient`: find user, list sessions, logout, disable) used by the revoke-a-person flow. | `kbeads/internal/server/keycloak_admin.go` |

## 3. The link flow (common to every option)

```
Slack user U presses a write button / runs a writing command
  bridge: no link for U  ->  ephemeral "Link your gardenio account" [button]
                              button url = https://gardenio.clonk.tv/slack/link/start?s=<state>
browser GET /slack/link/start?s=<state>
  bridge: state known, unexpired  ->  302 Keycloak /auth?client_id=gardenio-slack-link
                                      &redirect_uri=https://gardenio.clonk.tv/slack/link/callback
                                      &state=<state>&code_challenge=<PKCE>&scope=openid email [offline_access]
user signs in (realm gardenio, MFA as configured)
browser GET /slack/link/callback?code=...&state=<state>
  bridge: state known, unexpired, UNUSED -> consume it (single use), else 400 and nothing happens
          exchange code (client secret + PKCE verifier) -> id_token, access_token [, refresh_token]
          person := id_token.sub, id_token.email
          POST kbeads /v1/identity-links/slack  Authorization: Bearer <access_token for person>
               body {slack_user_id: U, slack_team_id, linked_via: "gardenio-slack-link"}
          kbeads: token verified as the person -> records the bind on the person's user bead
                  (identity_link.slack = {slack_id, subject, linked_at, client}), sets slack_id
                  if empty, refuses 409 if slack_id is set to a DIFFERENT id or if U is already
                  bound to another person (an admin resolves; never auto-merge)
          (gd-L55f9EYFjN) the callback does NOT bind or store: it parks the tokens in memory and
          DMs the Slack user from the state "Link this Slack account to <email>? Confirm / Cancel".
          Only that Slack user's Confirm (Socket Mode) does the POST above and stores the row.
          Cancel, a 10-minute expiry and a second press store nothing and revoke the offline token.
          bridge: stores whatever credential the chosen option needs (see §5)
  browser: "Almost done: confirm in Slack" ; Slack: "Linked as <email>. Retry your action." after Confirm
```

Properties:

- **The Slack user id never comes from the browser.** It is looked up from the `state` record
  the bridge created when it showed the button to exactly that Slack user (ephemeral message,
  visible to U alone). The callback cannot bind someone else's Slack id.
- **State**: 32 random bytes, bound to `{slack_user_id, team_id, created_at}`, TTL 10 minutes,
  deleted on first use (success or failure). A replayed callback finds no state and is refused;
  a forged one never had state. Held in bridge memory (the bridge runs 1 replica on k3s); if the
  bridge ever scales out, the state moves to the kbeads link store of §6 with the same semantics.
- **PKCE** (S256) on top of the confidential client secret, so an intercepted `code` is useless.
- **The bind write is the person's own write**: kbeads records `identity_link` under the
  person's own token, so the audit row for the link itself is `oidc:<email>`, and the bridge's
  credential never writes a user bead. `type:user` keeps `min_role: gardenio-admin`; the link
  endpoint is a server-side write of two whitelisted fields (`identity_link`, `slack_id`) on the
  caller's **own** user bead, like `/v1/operators/me/aliases` today.
- **Public path**: `viewer-authproxy.yaml` gains `location ^~ /slack/link/` proxied to
  `gardenio-slack-bridge.gardenio.svc.cluster.local:8090` with **no** `auth_request` (the
  oauth2-proxy gate is admin-group-only and would exclude every non-admin person; the state and
  the Keycloak sign-in are the authentication). Nothing else of the bridge is exposed. This is a
  `gardenio-ziti` chart change: DRAFT + `merge-desk:hold` + `merge_window` escalation, operator
  rolls.
- **Keycloak client `gardenio-slack-link`**: confidential, standard flow only (no direct access,
  no device grant, no service account in options A/B), redirect URI exactly the callback above,
  `audience-self` mapper to `gardenio-traefik-forwardauth`, the `realm-roles` mapper as the other
  clients, `access.token.lifespan` 300. Declared in `deploy-values/gardenio-k3s.yaml`
  `keycloak.clients`; secret `$gardenio-slack-link:clientSecret` created by the operator like the
  machine-principal secrets.

## 4. Acting as the person, per write

Every human-initiated daemon call in the bridge today goes through `withSlackActor`. It becomes:

```
withSlackActor(ctx, U):
  link := linkStore.Get(U)            -> none: refuse with the link prompt (no write is sent)
  tok  := tokenSource.AccessToken(ctx, link)  -> error: unlink + refuse with the link prompt
  ctx  = beadsapi.WithUserBearer(ctx, tok)    // the person's own token is the bearer
  ctx  = beadsapi.WithOperator(ctx, "oidc:"+link.Email)   // display hint only; kbeads ignores it for authz
```

`SetRequireUserBearerForWrites(true)` on the bridge's human-path client makes the fail-closed
guarantee structural: a human-path write without a user bearer is not sent (ErrUserBearerRequired),
the same mechanism the viewer uses. `WithEdgeAuthOperator` / `WithEdgeAuthIdentity` and the
`PersonRoleSource` envelope path are removed from the human path. Autonomous bridge writes
(config seed, DM delivery, thread bookkeeping) stay on `machine:gardenio-slack-bridge` (Task 5).

Access tokens are cached per person until 30 s before `exp` (5 min lifespan), so a burst of
button presses costs one Keycloak round trip.

**Role ceiling.** The person's token carries their real realm roles, including `gardenio-admin`
for the owner. The standing rule (gd-8PT2Qr2gEo) is that Slack never carries admin. Because the
realm roles are not composite, a Keycloak-side scope restriction on the client would strip admin
and leave the owner with **no** role, so the ceiling is enforced where it is today, in code, but
moved to the resource server: kbeads caps the roles of any token whose `azp` is
`gardenio-slack-link` at `gardenio-operator` (`BEADS_CLIENT_ROLE_CEILING=gardenio-slack-link=gardenio-operator`).
A stolen Slack-door credential is therefore at most an operator, never an admin, wherever kbeads
is the resource server. Making `gardenio-admin` composite over `gardenio-operator` and turning
off `fullScopeAllowed` on the client would move the ceiling into Keycloak itself; that is a
realm change with blast radius on every client and is left as a follow-up.

## 5. The choice: how the bridge obtains the person's token

Both options use the identical link flow of §3 and the identical per-write path of §4. They
differ only in **what the bridge holds after the link** and **how a fresh access token is minted**.

### Option A: offline refresh token, encrypted at rest (recommended)

The link sign-in requests `scope=openid email offline_access`. Keycloak returns an **offline
refresh token** bound to the person and to `gardenio-slack-link`. The bridge encrypts it
(AEAD, XChaCha20-Poly1305, key from a K8s Secret mounted only in the bridge) and stores the
ciphertext in a kbeads table (§6). Per write: `grant_type=refresh_token` at the token endpoint
with the client secret, which yields a 5-minute access token for that person.

### Option B: Keycloak token exchange (direct naked impersonation)

The bridge holds only its own service-account credential. Per write it calls the token endpoint
with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<its own
service-account token>`, `requested_subject=<person>`, and Keycloak mints an access token for the
person. The bridge checks the person is linked (§3) **before** asking; Keycloak itself does not
know which users are linked.

### Comparison

| | A: offline refresh token | B: token exchange (naked impersonation) |
|---|---|---|
| **Keycloak feature maturity on 26.0.7** | Standard OIDC (`offline_access` scope, refresh grant). GA for years. | Only the **legacy V1 exchange**, which is "Preview and Deprecated … will be removed in future versions". Needs `--features=token-exchange` **and** fine-grained admin permissions **v1** (FGAP v2 has no token-exchange permissions), plus the `impersonate` permission granted to the client. The standard V2 exchange (supported from 26.2) lists "Subject impersonation (including direct naked impersonation): Not implemented yet", requires a `subject_token` **of the person**, and never issues offline tokens, so it cannot serve a user who is not in a browser session with the bridge. |
| **Blast radius if slack-bridge is compromised** | Exactly the linked people, each capped at operator by the kbeads azp ceiling, for as long as each offline session lives. The attacker also needs the AEAD key (bridge Secret) plus the kbeads rows; the bridge pod has both, so assume full use of every linked token. Nothing beyond linked users. | **Every user in the realm**, including gardenio-admins who never linked, for as long as the client secret is valid. "Linked only" is a bridge-side check the attacker simply skips. The impersonate permission is realm-wide; Keycloak offers no per-user scoping of it. The kbeads ceiling still applies (operator), but the identity set is unbounded. |
| **Blast radius if kbeads DB / backups leak** | Ciphertext only (key is not in the DB). | Nothing stored. |
| **Revocation** | Three independent kill switches, all immediate: (1) the person or an admin revokes the `gardenio-slack-link` consent / offline session in Keycloak (account console or admin API); (2) an admin disables the user (already wired in kbeads `KeycloakAdminClient.RevokeUser`); (3) `unlink` in Slack revokes the refresh token at Keycloak's RFC 7009 revoke endpoint and deletes the row. The next refresh fails with `invalid_grant`, the bridge drops the link and refuses with the prompt. | (1) disable the user; (2) delete the link row (bridge-side only, does not bind Keycloak); (3) rotate the client secret or remove the impersonate permission, which kills Slack for **everyone**. No per-person Keycloak-side revocation of the bridge's power exists. |
| **Lifetime / re-link** | Offline session idle 30 d (realm setting). A person who has not acted through Slack for 30 days re-links (one click + sign-in). Max lifespan disabled, so an active linker never re-links. Rotation is off (`revokeRefreshToken: false`); if it is turned on the bridge must persist the rotated token on every refresh (handled). | None; works as long as the realm permission stands. |
| **Keycloak side setup (operator)** | Declare the client in `keycloak.clients` with `optionalClientScopes: [offline_access]`; create its secret. No feature flags. Realm already has the `offline_access` scope (Keycloak default) and offline session timeouts. | Add `--features=token-exchange,admin-fine-grained-authz` to the Keycloak StatefulSet args (chart change, restart of the IdP), enable FGAP v1 on the realm, grant the client `impersonate` under Users > Permissions, give the client a service account. Deprecated path to maintain across every Keycloak upgrade until it is removed. |
| **kbeads changes** | Link store table + endpoints with a machine grant (§6), `identity_link` endpoint, azp role ceiling, approver allowlist entry. | Same minus the ciphertext column. |
| **Bridge changes** | Link flow, AEAD store, refresh-grant token source, unlink. | Link flow, exchange-grant token source, unlink. |
| **Audit truth** | kbeads sees `azp=gardenio-slack-link`, `sub`/`email` = person. Keycloak's own event log shows a refresh for the person by that client. | kbeads sees the same. Keycloak logs an impersonation event per write. |

### Recommendation

**Option A.** It is the only option whose blast radius is bounded by the set of people who chose
to link, it has per-person revocation that Keycloak enforces without the bridge's cooperation,
and it uses no preview or deprecated Keycloak feature. Option B would make slack-bridge a
realm-wide impersonator on a code path Keycloak has already deprecated, with "linked users only"
enforced by the very component whose compromise we are defending against. The owner's own
criterion ("passing the user's own token, permissions and identity along") is met by A: the
offline token is literally the token Keycloak issued to that person for that client, obtained
by that person signing in.

### The second question in the same decision: is Slack an approval door?

Escalation approve is fail-closed on `azp` (`BEADS_ESCALATION_APPROVER_CLIENTS`, default the
browser SSO client; since P10.B, gd-4mEAhIE01U, only the page session approves and that list is
gone, see §10). Adding `gardenio-slack-link` makes a linked person's Slack approve count.
Arguments for: this is what gd-ScDm0mEFmq asks for, and the person proved possession of their
Keycloak account in a browser to link, at the same assurance (incl. MFA) as the viewer. Against:
a stolen offline token (bridge compromise) could approve escalations as the linked owner, which
the browser door cannot suffer. The operator ceiling already stops admin-only writes; approval
needs only operator, with one exception that kbeads enforces: a privileged-role spawn (infra-admin, infra, claimlayer-infra, never-auto and bypass roles) needs an uncapped `gardenio-admin` even when a spawn_policy rule names the approver, so the Slack door can never approve cluster-admin. The ceiling also fails closed: a token whose azp cannot be read is capped at the lowest configured ceiling. **Recommended: yes, add it**, because without it the Slack door has no
practical use for the owner, and the compromise scenario is bounded to linked people and is
revocable per person in Keycloak. The decision lists it as a separate option so the owner can
take A without the approval door.

## 6. kbeads changes (all options)

1. **Link store** (new table `slack_identity_links`: `slack_user_id` PK, `slack_team_id`,
   `subject`, `email`, `credential_ciphertext` (A only), `created_at`, `last_used_at`).
   Endpoints `GET/PUT/DELETE /v1/slack-links/{slack_user_id}`, authorized **only** for a machine
   principal holding the new capability `gardenio-machine-slack-link-store`
   (granted to `machine:gardenio-slack-bridge` alone). Never readable with the static/fleet token
   or by a human token.
2. **`POST /v1/identity-links/slack`** (OIDC human token whose azp is `gardenio-slack-link` only; the bridge only calls it after the Slack user confirms the link in Slack; email, aliases and pinned slack_id are never matched, a differing Slack profile email is just a warning on the prompt): records
   `identity_link.slack` on the caller's own user bead, sets `slack_id` if empty, 409 on
   conflict. **`DELETE`** of the same (human token, or the machine link-store capability for the
   unlink-after-revocation path) clears it. `type:user` schema gains `identity_link` (json).
3. **azp role ceiling**: `BEADS_CLIENT_ROLE_CEILING` map applied after `ExtractRoles` for OIDC
   tokens (`gardenio-slack-link=gardenio-operator` on k3s).
4. **Approver allowlist**: `beads.escalationApproverClients: "gardenio-traefik-forwardauth,gardenio-slack-link"`
   in deploy-values, if the owner accepts the approval door.
5. No change to audience verification: the client's `audience-self` mapper does that.

## 7. Unlink and revoke

- `@gardenio unlink` (and a button on the link-confirmation message): the bridge revokes the
  refresh token at `/protocol/openid-connect/revoke` (client-authenticated, RFC 7009), deletes
  the link row, and clears `identity_link` on the user bead. Next write: link prompt.
- Keycloak-side revoke without the bridge: admin console > Users > `<person>` > Consents > revoke
  `gardenio-slack-link` (or Sessions > offline > revoke), or disable the user. The bridge's next
  refresh fails with `invalid_grant`; the bridge deletes the row and clears `identity_link`
  (machine path), so the state converges without an operator touching kbeads.
- Re-link after either is the §3 flow again.

## 8. Rollout and acceptance (option A)

PR order, each runtime-auth / deploy-values PR as DRAFT + `merge-desk:hold` + `merge_window`
escalation per Task 5:

1. kbeads (§6) and bridge code (§3, §4, §7), behind `SLACK_LINK_ENABLED` (default off: today's
   behaviour byte-for-byte). Unit tests: state single-use and TTL, forged state, PKCE mismatch,
   409 on conflicting bind, refresh failure unlinks, azp ceiling, approver allowlist.
2. `deploy-values/gardenio-k3s.yaml`: `gardenio-slack-link` client, bridge env
   (`SLACK_LINK_PUBLIC_URL`, `SLACK_LINK_KEY` Secret ref, client secret ref), beads env
   (`escalationApproverClients`, `BEADS_CLIENT_ROLE_CEILING`, machine grant).
   Operator: create the two Secrets, roll.
3. `helm/gardenio-ziti/viewer-authproxy.yaml`: `/slack/link/` location. Operator rolls.
4. Live acceptance, in this order: unlinked write refused with prompt; forged callback 400;
   link; replayed callback 400; approve an escalation from Slack, approver = `oidc:<owner>`;
   decision answer from Slack attributed `oidc:<owner>`; `unlink`; write refused; re-link;
   Keycloak-side consent revoke; write refused.

## 9. Non-goals

- Any relay of a human identity in a header or envelope from the bridge (removed, not narrowed).
- Linking from the viewer or CLI (the viewer already acts with the person's own token).
- A second Slack workspace per person, or linking one Slack id to several people.
- Moving the role ceiling into Keycloak composites (follow-up, §4).

## 10. As built (deviations from this spec)

Code: `controller/internal/bridge/slack_link.go` (link flow, AEAD store, refresh-grant token
source, unlink), `bot_person_identity.go` (`withSlackActor` link branch, link prompt),
`bot_commands.go` / `bot_mentions.go` (slash commands and `@gardenio` mentions run as the
linked person; `@gardenio link | unlink | whoami`), `cmd/slack-bridge` (`SLACK_LINK_*` env,
`/slack/link/` mount); kbeads `client_role_ceiling.go` (`BEADS_CLIENT_ROLE_CEILING`),
`http_identity_link_slack.go` (`POST`/`DELETE /v1/identity-links/slack`), `type:user` gains
`identity_link`; chart `slackBridge.accountLink`, `beads.clientRoleCeiling`;
`helm/gardenio-ziti/public-edge.yaml` `location ^~ /slack/link/` (on viewer-authproxy's ziti
front door until ziti was switched off, gd-xNMLrsgajy).

- **Link store is a bridge-local file, not a kbeads table (§6.1).** The rows (ciphertext only)
  live in `SLACK_LINK_STORE_PATH` (`/data/slack-links.json`, the bridge's state PVC) behind the
  `SlackLinkStore` interface. The kbeads table gated to `machine:gardenio-slack-bridge` needs
  the bridge's own machine principal, which Task 5 (gd-Hc34EENbO9, component 4) has not built
  yet; swapping the store implementation is the follow-up. Blast radius is unchanged (the bridge
  pod holds key and ciphertext in both designs) and kbeads holds no credential at all. Losing
  the file means everyone re-links.
- **AEAD is AES-256-GCM** (standard library) rather than XChaCha20-Poly1305; the key is still
  32 bytes from a bridge-only Secret (`gardenio-slack-link-key`, key `key`). The Slack user id
  is the AEAD additional data, so a row copied under another id does not decrypt.
- **The per-write bearer is honoured without the fail-closed client gate for now.** The
  bridge's daemon client runs `SetPreferUserBearerForWrites(true)`: a human-path write carries
  the person's token, and the human path refuses an unlinked user before any write is built.
  `SetRequireUserBearerForWrites(true)` (the structural guarantee) is flipped when Task 5 moves
  the bridge's autonomous writes (config seed, DM delivery, thread bookkeeping) to
  `machine:gardenio-slack-bridge`; until then they stay on the bridge's own credential.
- **Bridge machine principal, as built (gd-Hc34EENbO9 component 4).**
  `machine:gardenio-slack-bridge` carries the bridge's background writes only, per call
  (`beadsapi.WithMachinePrincipal`): the startup config seed (`EnsureSlackBridgeConfigs`, role
  `gardenio-machine-slack-config-seed`) and the dm delivery writes (`DMDelivery`, role
  `gardenio-machine-slack-dm-delivery`, ownership typed to `dm`). It has no operated-by relay,
  no `slack-link-store` capability (the link store stays the bridge-local file above) and no
  other write; the bridge's fleet token still carries thread bookkeeping, decisions,
  escalations, agents and every read, so `SetRequireUserBearerForWrites` stays off and the
  paragraph above still describes the bridge's guarantee. Chart `slackBridge.machinePrincipal`.
- **Which Slack paths are person-bound**: interactions (buttons, modals), replies in a tracked
  decision thread, every slash command, and every `@gardenio` mention except `link`, `unlink`
  and `whoami`. Plain thread replies that are not mentions keep today's operator stamp.
- **Keycloak-side revoke** drops the bridge row on the next refresh (`invalid_grant`) but leaves
  `identity_link.slack` on the user bead; the bridge holds no usable credential, so the next
  write is refused with the link prompt and the stale record is only informational.
- **Approver allowlist** on k3s: `beads.escalationApproverClients:
  "gardenio-traefik-forwardauth,gardenio-slack-link"` (the owner accepted the approval door).
  **Withdrawn:** Slack became notify-only (D10), and P10.S (gd-RMmWllb8PS) dropped
  `gardenio-slack-link` from the list, so a slack-link token no longer approves. P10.B
  (gd-4mEAhIE01U) then removed the allowlist itself: only the page session (the forward-auth
  client) approves, and the chart knob and env var are gone (gd-WgHArryHIF).
- **Rollout**: one PR (kbeads + bridge + chart + deploy-values + authproxy), DRAFT +
  `merge-desk:hold` + `merge_window` escalation. Operator steps before the merge: create the
  two Secrets, apply the `gardenio-slack-link` Keycloak client (`keycloak.clients`), roll
  viewer-authproxy, roll beads (ceiling + approver allowlist), roll slack-bridge. Live acceptance
  per §8 step 4.


## 11. Live acceptance record (2026-10-15)

PR #374 merged (5ffb8d1f) and rolled in 2026.287.14 (rev 141); the #402 email-mismatch
fallback rolled in 2026.288.1.

| Done-when | Result |
|---|---|
| Forged or replayed link callback refused | Live: a forged `state` gets 400 on `/slack/link/start` and on the callback. |
| Another Slack user cannot link the owner | Live: the first attempts were refused by the bridge (Slack user U0C37URBQVD: Slack email did not match the Keycloak email) and by kbeads (U0C9806910D: the user bead was pinned to a different Slack id, 409). The owner linked only after an owner-approved admin repoint of `slack_id`, at 03:33:35Z. |
| Ceiling and approver env present | Live: beads shows `gardenio-slack-link=gardenio-operator` and approver clients `gardenio-traefik-forwardauth,gardenio-slack-link`. |
| A privileged spawn approval via the slack-link token gets 403 | **Not run live; covered by test.** `kbeads/internal/server/slack_link_ceiling_approval_test.go` asserts 403 for infra-admin, infra and claimlayer-infra with and without a spawn_policy rule naming the owner, 200 for the same person on the browser client, 403 for a listed non-admin, and 200 for an unprivileged spawn from Slack. A live run needs a privileged spawn escalation plus the owner pressing Approve in Slack with a real linked token, which no agent holds, and it would risk approving a real cluster-admin agent if the guard were wrong. |
