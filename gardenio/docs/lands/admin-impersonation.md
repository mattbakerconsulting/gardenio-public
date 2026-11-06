# Admin impersonation: Become

A gardenio admin can become a Lands tenant from `/garden` with one click. The
session really is the tenant's, with the same scope, purse, deed and writes,
because it is Keycloak's own impersonation. A banner says so on every page,
**Stop** ends it, and every record of the session names the admin as well as
the tenant. Feature: gd-NJkG7x9wb7. Plan: gd-0SUIdKXWV3. Chart task:
gd-X2xwS9MoBP.

The cost: the whole browser becomes the tenant on `gardenio.clonk.tv` (every
tab) until you press Stop, and Stop asks you to sign in again as yourself.

## Become a tenant

1. Signed in to `https://gardenio.clonk.tv/garden` as yourself, find the
   tenant. There are two places:
   - In the Commons, open a channel of the tenant's land and press
     **People**. The lightbox's **Members** list has **Become** on every row
     but your own.
   - Open the tenant land's panel (`/garden?project=<land>`). Beside
     "Owner: <name>" is **Become**.
2. Press **Become** and confirm: "Become <name>? Your browser signs in as them
   until you press Stop. Writes you make are marked as made by you on their
   behalf."
3. The browser goes to `https://auth.clonk.tv/gardenio/become/?email=<tenant>`
   (the page described below). It signs you in to Keycloak silently, refuses
   an unknown email, yourself or another admin, starts the impersonation, and
   signs you out of `gardenio.clonk.tv` so that `/garden` signs in again as
   the tenant.
4. You land on `/garden` as the tenant.

Only an admin sees Become. beads-viewer writes `data-admin` on `<body>` only
for a session with the admin role (`viewerCanAdmin`), and `become.js` draws
no button without it. Keycloak is the real gate: without the `impersonation`
role its admin API answers 403, and the page says "Keycloak refused".

### While you are the tenant: the banner

Every `/garden` view (the farm, the Commons band, the phone Commons sheet, a
panel) shows a strip at the top: **You are <tenant>, impersonated by
<admin>**, with **Stop**. The server draws it from the `impersonator` claim of
the token it verified, so it shows with JS off, and no request header can
fake it or hide it. A normal session has no banner.

### Stop

**Stop** goes to `https://gardenio.clonk.tv/oauth2/sign_out`, then to
Keycloak's end-session for the `gardenio-traefik-forwardauth` client, then
to `/garden`. That clears the gardenio.clonk.tv cookie and the tenant's
Keycloak session, so `/garden` asks for your own sign-in. Keycloak may ask
"Do you want to log out?" first, because no `id_token_hint` is sent: say yes.

The end-session only accepts `/garden` as the return address because the
forwardauth client's `post.logout.redirect.uris` lists it
(`deploy-values/gardenio-k3s.yaml`, guarded by
`TestKeycloakClients_K3sOverlay_ForwardAuthAllowsStopBecome`). If Keycloak
says "Invalid redirect uri", that value has not rolled out, and the tenant's
Keycloak session is still open: clear the site data for `auth.clonk.tv` and
`gardenio.clonk.tv` (or close the private window), then sign in as yourself.
`BEADS_VIEWER_STOP_BECOME_URL` overrides the Stop URL.

## What records an impersonated session

Writes still belong to the tenant: `created_by`, `author` and `operated_by`
name them. The admin is recorded alongside, from the verified `impersonator`
claim only (kbeads `ImpersonationExtractor`; a header never counts, and only
OIDC callers can be impersonated):

| Record | Where | What it says |
|---|---|---|
| `fields.impersonated_by=<admin>` | a bead the session creates, updates or closes, or writes through the field endpoints | the last impersonated write (it stays after later writes) |
| `fields.created_impersonated_by=<admin>` | a bead the session creates; never on a later write | who really wrote the bead |
| `impersonated_by` on a comment | `GET /v1/beads/{id}/comments` (migration 061) | the admin who added the comment |
| **by <admin> as <tenant>** | beside the author of a Commons post or reply (`created_impersonated_by`) | the visible mark. An admin closing or reacting to a tenant's own message does not relabel it. |
| One audit bead per session | `type=audit`, `action=impersonate`, label `auto:impersonation-audit`; `resource` is the tenant's user bead; also `actor`, `impersonated_by`, `subject`, `client_id`, `source_ip`, `user_agent`, `session_id` | that the session happened. Written on the session's first read or write, keyed `impersonation-session:<sid>`. |
| Keycloak events, 90 days | admin console, **Events** (see Audit below) | that the admin started it, with time and IP |

A client cannot set either field itself: create, update, every field merge,
the field endpoints, the escalation result, the close body and the comment
body all refuse it (403). An unreadable claim is recorded as
`unknown-impersonator`, never as the tenant.

Not every write is stamped: decision respond and cancel, escalation approve,
reject and result, delete, labels and deps are not. On those, the mutation
history still names the tenant, and the session's audit bead records that it
was impersonated. To find a session's writes, start from its audit bead:

    kd list --type audit -l auto:impersonation-audit

## Fallback: Become from the Keycloak console

Use this if `/garden` or the Become page is down, or to become a user who is
not a land member (Become lists only members). The session is the same
Keycloak impersonation, so the banner, the stamps and the audit bead appear
just as they do after Become.

1. Open a **private window**, so your own admin session stays separate. Do
   not open `gardenio.clonk.tv` in it yet: its sign-in cookie would still be
   yours after step 3.
2. Open the gardenio realm's admin console,
   `https://auth.clonk.tv/auth/admin/gardenio/console/`. The edge asks you to
   sign in first (oauth2-proxy, `/gardenio-admins` only), and the console then
   signs you in again silently with the same Keycloak session.
3. Go to **Users > <user> > Action > Impersonate**. Keycloak ends your own
   session in this window and gives it an SSO session as that user. The tab
   moves to that user's account page.
4. In the same window, open `https://gardenio.clonk.tv/garden`. oauth2-proxy
   finds the tenant's SSO session and mints the tenant's cookie.
5. Exit: press **Stop** on the banner, or close the private window.
   (`https://gardenio.clonk.tv/oauth2/sign_out` alone clears only the
   gardenio.clonk.tv cookie, not the tenant's Keycloak session.)

The console does not refuse an admin target or your own account; the Become
page does.

## What the chart sets

| Setting | Values key | Effect |
|---|---|---|
| `gardenio-admins` group gets the `realm-management` client roles `impersonation` and `view-users` | `keycloak.groups[gardenio-admins].clientRoles` (chart default; repeated in `deploy-values/gardenio-k3s.yaml`, which replaces the whole list) | Only gardenio admins can impersonate. Keycloak refuses everyone else. `view-users` lets the console list and open the user. |
| Realm admin events on, with details, plus user events for `IMPERSONATE` only, both kept 90 days | `keycloak.realm.events` -> `KeycloakRealm.spec.realmEventConfig` | Keycloak records each impersonation twice: an admin event with the admin's user id, time and IP, and an `IMPERSONATE` user event naming the impersonator. Logins are not recorded. |

The EDP 1.32.0 operator PUTs the whole `events/config` on every reconcile, and
Keycloak empties any field the body leaves null. So the chart renders every
field, and `eventsListeners` repeats Keycloak's default `jboss-logging`.
`enabledEventTypes` is `[IMPERSONATE]`; an empty list would record every login
too. `adminEventsExpiration` reaches the realm only while
`adminEventsEnabled` is true. `keycloak.realm.events.enabled: false` renders no
`realmEventConfig`, and the realm is then left as it is.

If the owner already set either one by hand in the console, the chart keeps it
that way, and a later `helm upgrade` does not revert it.

## Where the console and the page are served

The edge (`gardenio/helm/gardenio-ziti/public-edge.yaml`, owner decision
gd-papvgFfwMf) serves the gardenio realm's console on `auth.clonk.tv`. It
has to be that host: Keycloak sets the impersonated session's cookie on the
host the console was loaded from. A console opened through
`kubectl port-forward` puts it on localhost, and `/garden` then sends you to
a login page.

| Path on `auth.clonk.tv` | Edge answer |
|---|---|
| `/auth/admin/gardenio/console/...`, `/auth/admin/realms/gardenio[/...]` | sign-in check (`/gardenio-admins`), then Keycloak, which checks the console's own admin token |
| `GET /auth/admin/serverinfo`, `GET /auth/admin/realms` | the same; any other method is 403 |
| `/gardenio/become/` (the one-click Become page, below) | the same sign-in check, then the page's static files; GET and HEAD only |
| `/auth/admin/` (master console), `/auth/admin/master/...`, `/auth/admin/realms/<other realm>`, `/auth/realms/master/` | 404 |

The sign-in check's callback, `https://auth.clonk.tv/oauth2/callback`, is on
the `gardenio-traefik-forwardauth` client in `deploy-values/gardenio-k3s.yaml`.
The edge probe (`scripts/edge-probe`, E2) fails if the console is ever open
without the sign-in check, or if any master path stops returning 404.

The console's first load takes many asset requests, all on the
`auth_per_ip` limit (10/s, burst 40). If it shows a 429, reload.

## Audit

In the admin console, each impersonation leaves two rows, both kept 90 days:

- **Events > User events**, type `IMPERSONATE`: the tenant as the user, with
  `impersonator` (the admin's username) and `impersonator_realm` in the
  details.
- **Events > Admin events**, operation type `ACTION`, resource type `USER`,
  resource path `users/<tenant id>/impersonation`: the admin's user id, realm,
  client and IP.

## One-click Become: the Keycloak pieces

The first step of plan gd-0SUIdKXWV3 (gd-rbML2txvGG) adds the realm side:

| Piece | Where | Effect |
|---|---|---|
| Public client `gardenio-become`: standard flow, PKCE S256, no password grant, no service account; redirect `https://auth.clonk.tv/gardenio/become/*`, web origin `https://auth.clonk.tv` | `deploy-values/gardenio-k3s.yaml` `keycloak.clients` | The Become page signs the admin in through it and calls `POST /auth/admin/realms/gardenio/users/<id>/impersonation` with the token. |
| `fullScopeAllowed: false` plus a scope mapping for `realm-management` `impersonation`, `view-users`, `query-users` | the client's `scopeMappings`, written by the first-admin hook (step 2d) | Keycloak's admin API allows a call only when the user holds the role **and** the client has it in scope, so the token can do nothing else. A user who does not hold the roles (anyone outside `gardenio-admins`) gets nothing. |
| Mappers `impersonator` (session note `IMPERSONATOR_USERNAME`) and `impersonator-id` (`IMPERSONATOR_ID` to `impersonator_id`), access and ID tokens | the `gardenio-traefik-forwardauth` client (chart default and the k3s overlay) | Keycloak sets the notes only on an impersonated session, so the claims are absent otherwise. beads-viewer and kbeads already verify this client's access token and can read them with no oauth2-proxy change. |

**Scope mappings come from the hook.** The EDP 1.32.0 `KeycloakClient` has no
scope-mapping field, so `keycloak.clients[].scopeMappings` is chart input only.
The first-admin Job adds the listed roles with `kcadm` after the realm wait. It
is add-only (a role dropped from values stays mapped until removed in the
console) and non-fatal: if the operator has not made the client yet, the Job
waits a minute, warns and leaves it to the next upgrade. Until then the client's
token has no admin role, so a miss fails closed. The keycloak-theme harness
(`gardenio/tests/keycloak-theme/become-client.sh`) runs the shipped block
against a real Keycloak and checks that an impersonated session's tokens carry
`impersonator`.

**Limiting impersonation to `lands-tenants` (finding, 2026-10-31).** Keycloak
26.7's admin permissions v2 could do it: the realm switch
`adminPermissionsEnabled`, then a permission on the Groups resource type for
`lands-tenants` with the `impersonate-members` scope and a group policy for
`gardenio-admins`. It is not set, for two reasons:

1. The operator cannot express it. The 1.32.0 `KeycloakRealm` has no
   `adminPermissionsEnabled`, there is no CR for admin permissions, and
   `KeycloakClient.permission` is the v1 per-client form.
2. Permissions only add. A holder of the `realm-management` `impersonation`
   role can impersonate every user whatever the permissions say, so the limit
   also means taking that role off `gardenio-admins` and granting
   `impersonate-members` on `lands-tenants` in its place. That changes the
   console flow above and this client's scope mapping.

So Keycloak lets an admin impersonate any user, and the Become page refuses an
admin target (and the admin's own account) itself. The admin-permissions route
would go through the first-admin hook's admin API calls, like the scope mappings.

## One-click Become: the page

The plan's second step (gd-VIm9kMDswf) is the page that Become opens:
`https://auth.clonk.tv/gardenio/become/?email=<tenant>`. It is three static
files (`index.html`, `become.js`, `become.css`) in the ConfigMap
`public-edge-become` in `public-edge.yaml`, which the edge pod mounts at
`/usr/share/nginx/become`. It is on `auth.clonk.tv` for the console's reason:
Keycloak sets the impersonated session's cookie on the calling host, and the
calls are same-origin, so no CORS.

What it does, in order:

1. Sign in silently to `gardenio-become`: authorization code with PKCE,
   `prompt=none`, `response_mode=fragment` (the code never reaches a server
   log). The email, state and verifier wait in `sessionStorage`.
2. Trade the code for a token and look the tenant up:
   `GET /auth/admin/realms/gardenio/users?email=<e>&exact=true`. It refuses an
   unknown email, more than one account, your own account (the token's `sub`),
   and a member of `/gardenio-admins` (`GET .../users/<id>/groups`).
3. `POST /auth/admin/realms/gardenio/users/<id>/impersonation`. Keycloak ends
   your SSO session and starts the tenant's on this host.
4. Go to `https://gardenio.clonk.tv/oauth2/sign_out?rd=%2Fgarden`. That clears
   your gardenio.clonk.tv cookie and Redis session, so `/garden` signs in again
   and Keycloak answers with the tenant.

Every failure is a message with a link back to `/garden`, never a blank page:

| Case | Page says |
|---|---|
| The silent sign-in needs a login (`login_required` and the like) | You need to sign in first, with a **Sign in and try again** link (the same flow without `prompt=none`) |
| Keycloak answers 403 (no impersonation role, or the client's scope mapping is missing) | Keycloak refused |
| Unknown email, yourself, an admin, unreadable groups | No such tenant / That is you / That is an admin / Could not check |
| `auth_per_ip` 429, on the page itself or on a call | Too many requests |
| The edge session ran out mid-flow (401) | Your sign-in ran out |

The edge adds two guards. A start (`?email=`) opened from another site
(`Sec-Fetch-Site: cross-site`) does not run: it gets a page with **Continue**
(the same link, now same-origin) and **Back to /garden**, so a link elsewhere
cannot make your browser a tenant on a click. And a CSP lets the page run only
its own script and call only `auth.clonk.tv`.

The files are read per request, so an edit to `public-edge-become` alone goes
live within the kubelet's ConfigMap sync (about a minute) after
`kubectl apply`. A change to the `default.conf` locations still needs the edge
restart.

## Not done

- `kd login --as` / token exchange for the CLI.
- A mark on kbeads comments in the beads-viewer bead page. The data is in the
  comments JSON (`impersonated_by`); the visible mark is Commons-only.
- A 1h cap on an impersonated session. The realm SSO limits apply.
- Limiting Become to `lands-tenants` in Keycloak (see the finding above).

## Owner walk-through checklist

The live acceptance for the feature (gd-S3hjCWzudn). Use a test tenant that is
not an admin (phonobaloon, or one made for it).

1. **Become.** As yourself, open the test tenant's land channel in the
   Commons, press **People**, and press **Become** on their row. Confirm.
2. **Landing.** You arrive on `/garden` as them. The banner reads "You are
   <tenant>, impersonated by <you>" on the farm, the Commons band and a panel
   (and the Commons sheet on a phone).
3. **The mark.** Post one Commons message. It shows **by <you> as <tenant>**
   beside the author. On the laptop, its bead has
   `fields.impersonated_by=<you>`, and
   `kd list --type audit -l auto:impersonation-audit` shows one new audit bead
   for the session, whose `resource` is the tenant's user bead.
4. **Stop.** Press **Stop**. After Keycloak's logout (say yes if asked), you
   are back on `/garden`'s sign-in, and signing in makes you yourself again,
   with no banner.
5. **Keycloak Events.** In the gardenio realm console, **Events > User
   events** has an `IMPERSONATE` row for the tenant with `impersonator` =
   you, and **Events > Admin events** has the matching `ACTION` on
   `users/<tenant id>/impersonation`.
6. **A tenant sees no Become.** Signed in as a tenant, the People list and
   the land panel show no **Become** button. The Become session of step 2
   counts: check it there, before pressing Stop.
