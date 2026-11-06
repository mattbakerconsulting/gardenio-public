# Project infra escalation chain

**Task:** gd-CfFb732AHr (P10) · **Plan:** gd-xiV5hyhPrG (agent self-service infra
access) · **Builds on:** `project-infra-role-design.md` (P8, §7) and
`infra-admin-role-design.md` (P6) · **Status:** the process in §2-§5 is how requests are
handled today; the Keycloak "clients as code" path in §3 depends on PR #297, which is a
draft until it merges (checked 2026-10-14).

Plan decision D2: a project's infra agent owns one namespace, and "can escalate to
gardenio infra for e.g. connecting to our auth". This doc is that escalation: one
documented path, and it always ends at a human.

## 1. When this applies

A `<project>-infra` agent (P8) holds a Role in its own namespace and nothing else. Some
things sit outside it. All of these go up this chain:

| Need | Why the project role cannot do it |
|---|---|
| A Keycloak client or redirect URI in realm `gardenio` | Keycloak is a shared platform piece in ns `gardenio`; the role has no rights there and never touches Keycloak |
| A public hostname (DNS record, certificate, public-edge vhost) | `public-edge` and the Route 53 records belong to the gardenio tier |
| A Secret copied between namespaces | the role cannot read Secrets (P8 §3) |
| A wider Role, a new quota size, a new namespace label | a chart PR, not a runtime grant (P8 §7) |

Rule that holds for all of them: **never ask for a wider Role to get around this.** If
the request is frequent enough to be annoying, change the pattern by a reviewed PR to
`project-infra-role-design.md`.

## 2. The chain

```text
project infra agent                       gardenio tier                       humans
(<project>-infra, own namespace)
        │
        │ 1. writes the request in a bead comment
        │ 2. opens the PR(s) it can: code, never live writes
        ▼
 gb escalation create ───────────────────────────────────────────►  OPERATOR (laptop-claude)
   (+ merge_window escalation for any gated PR)                       reviews, merges in a window,
                                                                      runs the OPERATOR steps
                                                                         │
                                                  only if the change     │
                                                  cannot be code:        ▼
                                                  `infra-admin` agent ◄─ OIDC human approves
                                                  (every spawn)          that spawn
```

1. **The project agent never writes outside its namespace.** No `kubectl`, no Keycloak
   admin API, no Route 53, no edit of another namespace's objects. It prepares a PR and an
   escalation.
2. **Prefer code over a live write.** Keycloak clients are chart values
   (`keycloak.clients:` in `deploy-values/gardenio-k3s.yaml`, rendered into
   `KeycloakClient` CRs by `templates/keycloak/clients.yaml`), and the public edge is a
   manifest (`gardenio/helm/gardenio-ziti/public-edge.yaml`) plus a Terraform list. So the
   request is "a PR to those files plus a merge-window escalation", not "please click
   in the Keycloak console".
3. **The operator executes.** The OPERATOR merges the PR in a merge window and runs the
   few steps that cannot be code (copy a Secret, `kubectl apply` the edge manifest,
   `terraform apply`).
4. **`infra-admin` only when no PR can express the change.** It is spawned the way P6
   describes: a human approves every spawn, with no `auto` rule, ever. A project infra
   agent does not spawn it itself; it names the need in the escalation and the operator
   decides.

## 3. What the request must carry

Put this in the escalation `--operation-spec` (JSON) and repeat it in the bead comment, so
the operator never has to read prose to act. Names only: never a client secret, cookie
secret, token or kubeconfig.

| Field | Meaning | ClaimLayer value |
|---|---|---|
| `project` | the requesting project | `claimlayer` |
| `client_id` | Keycloak `clientId`, exactly as the proxy is configured | `claimlayer-demo` |
| `redirect_uris` | exact URLs, no wildcards | `https://claimlayer.clonk.tv/oauth2/callback` |
| `web_origins` | exact origins | `https://claimlayer.clonk.tv` |
| `public_host` | the hostname wanted | `claimlayer.clonk.tv` |
| `upstream_namespace` / `upstream_service` / `upstream_port` | where the edge routes | `claimlayer-demo` / `claimlayer-demo-auth` / `80` |
| `client_secret_ref` | Secret name and key holding the client secret (names only) | `claimlayer-demo-oidc` / `client-secret` |
| `client_settings` | confidential or public, PKCE method, audience mapper | confidential, `S256`, audience mapper `claimlayer-demo-audience` |
| `pr` | the PR(s) the agent opened | URL |

The same list also says which gates apply: `client_id` and hosts are exact strings, and a
host the gardenio tier does not already serve is a new public name (§5).

## 4. Who approves what

| Step | Approval | Why |
|---|---|---|
| The escalation itself | **none, ever**, by policy | A `spawn_policy` `auto` rule is only evaluated for a spawn escalation (`operation_type: create` with a `spawn_role`; `kbeads/internal/server/spawn_policy.go`). A Keycloak, edge or merge-window escalation is never one, so it is never auto-approved. Do not add a rule for these operation types. |
| The PR to `deploy-values/**` or `gardenio/helm/**` | merge-desk review, then the OPERATOR | those paths are DRAFT + label `merge-desk:hold`, merged only in a window (T8 and the org move are past) |
| `infra-admin` spawn, if needed | an OIDC human, every time | P6; never auto-approved |

A project agent opens the PR as a **DRAFT with label `merge-desk:hold`** and files **one**
merge-window escalation:

```bash
gb escalation create \
  --operation-type merge_window \
  --requesting-task <your-task-id> \
  --title "Merge window: <PR title>" \
  --context "<PR URL(s)>; rollout order; OPERATOR steps verbatim"
```

For work the operator must do by hand (copy a Secret, apply a manifest), file a second
escalation with `--operation-type infrastructure_change` and the §3 spec, or put those
steps in the merge-window context if they are one rollout. After filing, comment on your
task and `gb stop`. The escalation closing respawns you.

## 5. Worked example: how `claimlayer.clonk.tv` was wired

ClaimLayer's demo is a static nginx pod behind oauth2-proxy, sign-in through Keycloak
realm `gardenio`, reachable publicly at `https://claimlayer.clonk.tv`. Written as the
steps a project agent would request today. Sources: the ClaimLayer repo
(`deploy/demo/k8s/*.yaml`), `gardenio/helm/gardenio-ziti/public-edge.yaml`,
`gardenio/terraform/composition/gardenio/main.tf`, and the live Keycloak client export on
gd-DAy1iWvO4y (2026-10-14). **Checked against the live cluster on 2026-10-14** by a
read-only `infra` agent: the proxy image and every arg below, the Secret and env var
names, the namespace PodSecurity labels, and the `public-edge` vhost routing
(`claimlayer.clonk.tv` to `claimlayer-demo-auth:80`) all matched. Not verified live:
NetworkPolicies (the read-only SA cannot list them in `claimlayer-demo`), so those are
from the ClaimLayer repo only. The namespace has no `gardenio.io/project` label yet
(P8 precondition, an OPERATOR step).

**Inside the namespace (the project's own, no escalation needed).** The project deploys
into `claimlayer-demo`: the demo `Deployment`, the `oauth2-proxy` `Deployment` and its
`Service` `claimlayer-demo-auth` (port 80, target 4180), NetworkPolicies
(`claimlayer-demo-from-oauth2-proxy`, `oauth2-proxy-from-public-edge`), and the Secret
`claimlayer-demo-oidc` with keys `client-secret` and `cookie-secret`. The namespace
enforces PodSecurity `restricted`.

**Steps the project agent asks the gardenio tier for:**

1. **Keycloak client** in realm `gardenio`. Live client `claimlayer-demo`:
   confidential (`publicClient: false`), standard flow only, direct access grants off,
   PKCE `S256`, redirect `https://claimlayer.clonk.tv/oauth2/callback`, web origin
   `https://claimlayer.clonk.tv`, `post.logout.redirect.uris`
   `https://claimlayer.clonk.tv/`, an audience mapper `claimlayer-demo-audience`
   (`oidc-audience-mapper`, audience `claimlayer-demo`), default scopes
   `web-origins, acr, roles, profile, basic, email`. The clientId is `claimlayer-demo`,
   not `claimlayer`; the proxy's `--client-id` must match it character for character.
   - *Today:* this client was made by hand and exists only in the live Keycloak DB
     (gd-389GoDw1cw: a realm restore loses it).
   - *Target:* PR #297 (gd-DAy1iWvO4y) declares it in `keycloak.clients:` with
     `secret: "$claimlayer-keycloak-client:client-secret"`. A `KeycloakClient` resolves
     `$name:key` in its own namespace, so the OPERATOR first copies the client secret from
     `claimlayer-demo/claimlayer-demo-oidc` into `gardenio/claimlayer-keycloak-client`
     (`--from-file=client-secret=/dev/stdin`, never printed). Until #297 merges and rolls,
     a new client is a request for the operator, with the same fields. Once it has, a new
     project's client is one more entry under `keycloak.clients:` plus the Secret copy.
2. **Public hostname.** Three files, all gardenio-tier:
   - `gardenio/terraform/composition/gardenio/main.tf`: add the host to
     `public_edge_fqdns` (A record to the edge node's EIP; the module's security group
     already allows 443);
   - `gardenio/helm/gardenio-ziti/public-edge.yaml`: add the host to the `Certificate`
     `dnsNames`, and a `server { server_name <host>; }` block. ClaimLayer's block proxies
     `location /` to
     `claimlayer-demo-auth.claimlayer-demo.svc.cluster.local:80`, sets
     `X-Forwarded-Proto/Host/For`, and refuses ambiguous paths (`$public_edge_bad_path`)
     with 400. An unknown SNI is refused at the handshake and an unknown `Host` gets 421,
     so a host does nothing until it has a block;
   - `scripts/edge-probe/expectations.json`: add the host to `edge_hosts` so the daily
     external probe covers it.

   The nginx pod runs on the ziti edge node with `hostPort: 443`, in namespace
   `public-edge`, which is `privileged` for that reason. That is why the vhost is
   gardenio's, not the project's.
3. **Keycloak reachability.** The proxy redeems codes inside the cluster at
   `gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080`. The project's egress
   policy allows that Service on 8080 (P8 §5); the project agent does not ask for it.

**What the project's oauth2-proxy runs with** (ClaimLayer
`deploy/demo/k8s/oauth2-proxy.yaml`, image `quay.io/oauth2-proxy/oauth2-proxy:v7.7.1`).
The client and cookie secrets come from `claimlayer-demo-oidc` as the env vars
`OAUTH2_PROXY_CLIENT_SECRET` and `OAUTH2_PROXY_COOKIE_SECRET`, never as args.

```text
--provider=keycloak-oidc
--client-id=claimlayer-demo
--oidc-issuer-url=https://auth.clonk.tv/auth/realms/gardenio
--skip-oidc-discovery=true
--login-url=https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/auth
--redeem-url=http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/auth/realms/gardenio/protocol/openid-connect/token
--oidc-jwks-url=http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/auth/realms/gardenio/protocol/openid-connect/certs
--profile-url=…/userinfo        --validate-url=…/userinfo      (same in-cluster base)
--redirect-url=https://claimlayer.clonk.tv/oauth2/callback
--upstream=http://claimlayer-demo.claimlayer-demo.svc.cluster.local:80
--scope="openid email profile"  --code-challenge-method=S256   --email-domain=*
--cookie-name=__Host-claimlayer-demo  --cookie-secure=true  --cookie-httponly=true
--cookie-samesite=lax  --cookie-expire=8h
--reverse-proxy=true  --skip-provider-button=true
--skip-auth-route=GET=^/healthz$
```

Why the split between public and in-cluster URLs: the browser is sent to the public
`auth.clonk.tv` login URL, and Keycloak's issuer is the public URL, so the `iss` in the
ID token matches `--oidc-issuer-url`. Code redemption and key fetch stay on the
in-cluster Service. Discovery is skipped on purpose (the ClaimLayer manifest's own comment), so the
URLs can differ in this way. `auth.clonk.tv` itself only serves `/auth/realms/gardenio/`
and `/auth/resources/` from public-edge; the admin console and the master realm are 404
there, so a project never gets a Keycloak admin path from this chain.

**Order the operator runs it:** (1) Terraform DNS apply; (2) Secret copy into ns
`gardenio` (only if the client is code); (3) merge-window roll of the Keycloak client;
(4) `kubectl apply -f public-edge.yaml` (certificate issues by DNS-01, about 1-2
minutes); (5) the project agent's own proxy and `Service` already exist, so confirm
`https://<host>/` answers 302 to `auth.clonk.tv` and the edge probe passes. Rollback:
remove the vhost block (apply), then the client entry (`KeycloakClient` CRs carry
`edp.epam.com/preserve-resources-on-deletion: "true"`, so removing the entry leaves the
live client in place; delete it in the console only if wanted).

## 6. Template: the escalation a project agent files

```bash
gb escalation create \
  --operation-type infrastructure_change \
  --requesting-task <task-id> \
  --title "Connect <project> to auth: Keycloak client <client_id> + public host <host>" \
  --operation-spec '{
    "project": "<project>",
    "client_id": "<client_id>",
    "redirect_uris": ["https://<host>/oauth2/callback"],
    "web_origins": ["https://<host>"],
    "public_host": "<host>",
    "upstream_namespace": "<ns>",
    "upstream_service": "<service>",
    "upstream_port": 80,
    "client_secret_ref": {"namespace": "<ns>", "name": "<secret>", "key": "client-secret"},
    "client_settings": {"public": false, "pkce": "S256", "audience_mapper": true},
    "pr": "<PR URL>"
  }' \
  --context "PR is DRAFT + merge-desk:hold (see the merge_window escalation <id>). OPERATOR steps: copy the client secret into ns gardenio without printing it; apply the PR; apply public-edge.yaml; confirm https://<host>/ 302s to auth.clonk.tv. Nothing here asks for a wider Role."
```

Then comment on the task and `gb stop`. Do not poll.

## 7. What this chain does not do

- It does not let a project agent write to Keycloak, Route 53 or `public-edge`.
- It has no auto-approval path. A spawn-policy rule for these requests is a change to this
  doc first.
- It does not cover the project's own namespace; that is the `<project>-infra` Role
  (`project-infra-role-design.md`).
- It does not make a project host reachable without a Keycloak sign-in: the edge routes to
  the project's oauth2-proxy, never directly to the workload.
