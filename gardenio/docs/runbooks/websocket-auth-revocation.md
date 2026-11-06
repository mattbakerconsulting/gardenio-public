# WebSocket auth lifetime + operator revocation guidance

> **Audit reference:** F18 (LOW) in `docs/audits/ingress-tls-static.md` (not in this repo — the audits tree was excluded from the 2026-09-16 export).
> **Bead:** ka-0000001864 (parent audit ka-0000007645).

> **⚠️ IdP update (T6 Keycloak permadelete, ka-0000006953).** The in-chart
> Keycloak realm this runbook's revocation steps target has been removed from the
> fleet chart (`templates/keycloak/*`). The live fleet browser front door is now
> oauth2-proxy against the **environment OIDC issuer** — GitLab (`gitlab.com`) on
> `gardenio-rwx`/CI and staging, Entra/GitLab in prod — not an in-chart Keycloak
> realm. The **core lesson below is IdP-agnostic and unchanged**: long-lived
> streams authenticate once at upgrade and survive an upstream session/token
> revocation, so you still force revocation at the infra layer (roll the pod /
> rotate the secret). But when a step says "revoke in Keycloak" / "Keycloak admin
> UI" / regenerate the `keycloak.forwardAuth` client secret, apply it to the
> **environment's actual IdP** (the GitLab OAuth app or Entra app registration and
> the oauth2-proxy client/cookie secrets), not to a Keycloak realm the fleet no
> longer runs. The in-box (Gasbox) IdP still uses Keycloak; its migration is
> tracked separately.

## TL;DR for on-call

Long-lived streaming connections (WebSocket, SSE, gRPC) on the gardenio platform are
authenticated **once at the upgrade / initial request**, then run for the lifetime of
the TCP connection. Revoking a Keycloak session, expiring an OIDC token, or disabling a
user account does **NOT** terminate connections that are already open. To force a
revocation to take effect on existing streams, you must break the connection at the
infrastructure layer (roll a pod or rotate a secret).

## Affected endpoints

| Endpoint | Service | Type | Auth gate |
|---|---|---|---|
| `/ws/` | coopmux | WebSocket | Traefik forwardAuth (oauth2-proxy → Keycloak) at upgrade |
| `/api/v1/sessions` (subset) | coopmux | HTTP/long-poll | Traefik forwardAuth at request |
| `/grpc` (incl. server-stream) | beads daemon | gRPC over HTTP/2 | daemon-enforced bearer / OIDC at stream start |
| `/api/v1/events` | beads daemon | SSE | daemon-enforced bearer / OIDC at request |
| `/sse` (and any long-poll) | beads-viewer | SSE | Traefik forwardAuth at request |

All routes above complete authentication **before** the response stream begins. Once
the stream is open, no further auth check happens for the lifetime of the connection.

## Why this happens

Traefik's `forwardAuth` middleware runs once per HTTP request. For a WebSocket upgrade,
the *upgrade request* is one request — Traefik calls oauth2-proxy, oauth2-proxy
validates the session cookie / OIDC token, and on success Traefik proxies the upgrade
through. After the 101 Switching Protocols response, the connection is a raw byte
stream between Traefik and the upstream service (coopmux, beads, beads-viewer); Traefik
does not re-invoke the auth middleware on subsequent frames.

The same applies to SSE (`text/event-stream`) and to server-streaming gRPC: the auth
check is on the initial HTTP request, not on individual frames or messages.

oauth2-proxy itself is **not** in the data path — it only sits in front of Traefik for
the auth decision. Even if the user's Keycloak session is revoked at t+30s, the open
WebSocket continues to stream until the client disconnects, the upstream pod restarts,
or the TCP connection is otherwise broken.

This is consistent with how every reverse-proxy + forwardAuth deployment works
(Traefik, NGINX `auth_request`, Envoy `ext_authz` in per-request mode). It is not a
gardenio-specific bug — but operators need to know that **revoking access in Keycloak is
not sufficient to lock out a user with an active WS/SSE/gRPC stream.**

## When this matters

Most of the time, this is fine. WebSocket sessions are typically short-lived (minutes
to a few hours), and the next reconnect re-runs the full auth path. The behavior
becomes a problem in these scenarios:

1. **Compromised user / urgent revocation.** A user's account has been compromised, or
   an employee was just terminated. Revoking the Keycloak session does not kick their
   open coopmux terminal session — they can continue to interact with running agent
   pods until the WS connection drops.
2. **Permission downgrade.** A user's group memberships changed (e.g. removed from
   `gardenio-admins`). The forwardAuth headers (`X-Auth-Request-Groups`) were captured
   at upgrade time; the open WS continues to carry the old group claims to coopmux
   until the connection closes.
3. **OIDC client secret leak.** The forwardauth `clientSecretRef` was leaked. Rotating
   the secret in Keycloak invalidates new logins but does not affect cookies / sessions
   that were already exchanged for an access token before rotation.
4. **Forced logout for compliance.** Audit / compliance requires "kick all sessions
   now". The Keycloak admin UI's "logout all sessions" only invalidates refresh tokens
   and cookies — already-open WS connections are unaffected.

## Operator workarounds — how to actually break the session

Pick the smallest hammer that works. All of these will momentarily disrupt other users
on the same service; use them deliberately.

### A. Roll the upstream pod (preferred for a single user / a single service)

Restarting the upstream pod tears down every WS / SSE / gRPC connection terminated on
that pod. Clients reconnect through the ingress, and the reconnect re-runs forwardAuth
— which fails for the revoked user.

```bash
# Identify the namespace (gardenio-prod / gardenio-rwx / per-tenant fleet ns)
NS=gardenio-prod

# coopmux (covers /ws/, /api/v1/sessions)
kubectl -n "$NS" rollout restart deployment/coopmux
kubectl -n "$NS" rollout status deployment/coopmux --timeout=2m

# beads daemon (covers /grpc and /api/v1/events SSE)
kubectl -n "$NS" rollout restart deployment/beads
kubectl -n "$NS" rollout status deployment/beads --timeout=2m

# beads-viewer (covers /sse)
kubectl -n "$NS" rollout restart deployment/beads-viewer
kubectl -n "$NS" rollout status deployment/beads-viewer --timeout=2m
```

**Blast radius:** every active WS/SSE/gRPC stream against the rolled deployment is
disconnected and forced to reconnect. coopmux clients (mux UI tabs, agents posting
output) reconnect within a few seconds; beads-viewer SPA reconnects on the next event.
Agent pods talking to the beads daemon over gRPC will retry.

**Use this when:** you need to cut off a specific user's access *now* and a few seconds
of cluster-wide reconnect churn is acceptable.

### B. Revoke the OIDC client secret (when the secret itself is compromised)

If the leaked credential is the forwardauth oauth2-proxy → Keycloak client secret, not
just a single user's session:

1. In Keycloak admin: regenerate the client secret for the gardenio oauth2-proxy
   client.
2. Update the K8s Secret referenced by `keycloak.forwardAuth.clientSecretRef`:

   ```bash
   kubectl -n "$NS" edit secret <secret-name>   # or: kubectl create secret ... --dry-run=client -o yaml | kubectl replace -f -
   # Secrets: create or replace, never apply (secrets-create-not-apply.md)
   ```

3. Restart forwardauth so it picks up the new secret AND so all in-flight cookies are
   invalidated (the cookie HMAC is keyed on the cookie secret, not the OIDC secret —
   see C if you want to nuke cookies too):

   ```bash
   kubectl -n "$NS" rollout restart deployment/forwardauth
   kubectl -n "$NS" rollout status deployment/forwardauth --timeout=2m
   ```

4. Roll the upstream services per (A) to break any open WS that was authed before the
   rotation.

**Blast radius:** any new login flow until step 2 completes will fail. Existing user
sessions whose access tokens have not yet expired remain valid against the IdP, but
their next refresh through forwardauth will fail and they will be bounced to login.

### C. Rotate the oauth2-proxy cookie secret (forces re-login of every user)

The most aggressive option short of a full chart redeploy. Rotating
`keycloak.forwardAuth.cookieSecretRef` invalidates every existing oauth2-proxy session
cookie — every user is bounced to the login page on their next request.

1. Generate a new 32-byte random:

   ```bash
   COOKIE_SECRET=$(openssl rand -base64 32 | tr -- '+/' '-_')
   ```

2. Update the K8s Secret and restart forwardauth:

   ```bash
   kubectl -n "$NS" edit secret <cookie-secret-name>
   kubectl -n "$NS" rollout restart deployment/forwardauth
   kubectl -n "$NS" rollout status deployment/forwardauth --timeout=2m
   ```

3. Roll upstream services per (A) to break open WS streams that were authed under the
   old cookie.

**Blast radius:** every authenticated user is logged out and must re-authenticate. Use
sparingly — typically only during a known-incident response or scheduled maintenance.

### D. Suspend the Keycloak user (for completeness)

Disabling the user account in Keycloak prevents future logins but, on its own, does
**not** terminate active sessions. Always pair with (A) or (C). The "Logout all
sessions" button in the Keycloak admin UI invalidates refresh tokens but, again, does
not break open WS.

### Summary of operator choices

| You want to... | Smallest workaround |
|---|---|
| Lock out a specific user immediately | Suspend in Keycloak + roll affected upstream pods (A) |
| Lock out everyone immediately | Rotate cookie secret (C) + roll all upstream pods (A) |
| Respond to a leaked OIDC client secret | Rotate client secret (B) + roll all upstream pods (A) |
| Apply a permission change to an active user | Roll the affected upstream pod (A) |
| Just clean up an idle session | Wait — the cookie expires per `--cookie-expire` (oauth2-proxy default 168h) |

## What NOT to do

- **Don't** assume a Keycloak "Logout all sessions" click is sufficient. It is not.
- **Don't** delete the Keycloak realm or client to "force everyone off" — this strands
  the chart in a broken state and is not reversible without re-issuing OIDC config.
- **Don't** `kubectl exec` into traefik or coopmux to manually kill TCP connections —
  rollout restart is cleaner, observable, and idempotent.
- **Don't** rely on cookie-expiry. The default `--cookie-expire` is 168h (7 days),
  which is fine for steady-state but inappropriate as an incident-response control.

## When to escalate to a code-level fix

The doc-only mitigations above are the standard for this class of platform. If your
operating environment requires *bounded* time-to-revocation on active streams (e.g. "a
revoked session must be kicked within 5 minutes without operator intervention"), the
runbook is no longer sufficient and a code change is warranted. Options:

1. **Periodic token re-check in coopmux.** Coopmux already terminates the WS upgrade
   and could implement a background refresh-token validation against the OIDC
   introspection endpoint on a fixed interval (e.g. every 5 minutes). On revocation,
   coopmux closes the WS. This is a coopmux-level concern, not a chart change.
2. **Per-frame auth on the WS protocol.** Higher-effort: each WS message carries a
   bearer token re-checked on each receive. Unusual for interactive terminals; suitable
   for high-security environments.
3. **Switch from forwardAuth to ext_authz on a per-stream interval.** Requires Envoy /
   service mesh in front of the upstreams. Out of scope for the current Traefik-based
   chart.

Implementing any of the above is **out of scope for the current MR** (audit finding
F18, doc-only). File a follow-up bead under the audit parent (ka-0000007645) if your
threat model requires it; reference this runbook so the design discussion has the
operator-side baseline.

## See also

- `docs/audits/ingress-tls-static.md` — full audit; F18 is the source of this
  runbook (not in this repo, see note above).
- [`docs/secret-rotation.md`](../secret-rotation.md) — secret-rotation procedures
  referenced by workarounds (B) and (C).
- oauth2-proxy docs:
  [`--cookie-secret`](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview),
  [`--cookie-refresh`](https://oauth2-proxy.github.io/oauth2-proxy/configuration/session_storage)
  for tuning the steady-state revocation window.
