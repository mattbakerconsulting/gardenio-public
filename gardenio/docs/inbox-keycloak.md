# In-box Keycloak bring-up

How to stand up the **in-cluster** Keycloak identity server that the chart ships
but no environment has ever run. Until now `keycloak.runtime.enabled` was `false`
in every overlay and the only Keycloak in play was the external corporate ACME
Keycloak (`auth.e2e.dev.acme.example`, on ECS). This is the Path A bring-up from the
SSO research (`docs/research/sso-identity-gardenio-in-a-box.md` §0/§7, epic
`ka-0000005376`, task `ka-0000004234`).

> **First-time bring-up, not a toggle.** Flipping `keycloak.runtime.enabled=true`
> stands up a StatefulSet + Service + IngressRoute + a new `ClusterKeycloak` CR
> and binds the `KeycloakRealm` to it. The §3 first-admin bootstrap and §5 MFA
> gaps from the research still apply and are tracked as separate epic children.

## Quick start

Layer the in-box overlay on top of a base env overlay:

```sh
helm upgrade --install gardenio gardenio/helm/gardenio \
  --namespace <ns> --create-namespace \
  --values deploy-values/gardenio-rwx.yaml \
  --values deploy-values/gardenio-inbox.yaml
```

Per environment, set in `deploy-values/gardenio-inbox.yaml` (or a copy):

- `keycloak.runtime.host` — the auth hostname Traefik routes for.
- `keycloak.realm.name` — a stable, **cluster-unique** realm name (the
  `vkeycloakrealm-v1.kb.io` webhook enforces uniqueness; prod owns `gardenio`).
- Leave `keycloak.realm.issuerURL` **empty** — it is auto-derived (see §7.4).

## What the chart now does for you

| § | Concern | Mechanism |
|---|---|---|
| 7.1 | Postgres DB wiring | `postgres.enabled` + `keycloak.runtime.db.autoWire=true` points `KC_DB_*` at the in-cluster postgres Service and a `create-db` init container creates Keycloak's own `keycloak` database on it. |
| 7.2 | TLS without Route53 | `tls.existingSecretName=<secret>` makes the chart skip the cert-manager `Certificate` + external-dns `DNSEndpoint`; the keycloak-runtime IngressRoute still references the operator's TLS secret. |
| 7.3 | Admin secret seeding | `keycloak.runtime.admin.generate=true` (default) generates `gardenio-keycloak-runtime-admin` with a stable random password (`helm.sh/resource-policy: keep` + `lookup`). |
| 7.4 | Issuer-URL derivation | When `realm.issuerURL` is empty and the runtime is on, the issuer is derived as `https://<runtime.host><httpRelativePath>/realms/<realm>` and fed to beads / coopmux / landing / beads-viewer. |
| 7.5 | ClusterKeycloak ref | `realm.clusterKeycloakRef: gardenio-keycloak-runtime` binds the realm to the in-box CR instead of the external `acme-keycloak`. |

## What the operator still does

### TLS secret (§7.2)

Pre-create a `kubernetes.io/tls` Secret in the release namespace and set
`tls.existingSecretName` to its name. A self-signed cert works for a private box:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout tls.key -out tls.crt -subj "/CN=*.<your-domain>" \
  -addext "subjectAltName=DNS:*.<your-domain>,DNS:<auth-host>"
kubectl create secret tls gardenio-wildcard-tls \
  --cert=tls.crt --key=tls.key -n <release-namespace>
```

The cert SAN **must** cover `keycloak.runtime.host`. If the base overlay already
sets `tls.existingSecretName`, leave the in-box overlay's `tls:` block commented
out so it does not clobber the base value.

### Encryption-at-rest for the Keycloak PVC (§7.6)

The Keycloak runtime StatefulSet provisions a persistent volume via
`volumeClaimTemplates` (`keycloak.runtime.persistence`). This volume can hold
bootstrap admin state and cached realm material, so it **must** be backed by
encrypted storage. The chart does not enforce this.

Before enabling `keycloak.runtime.enabled` in an environment:

1. Determine which StorageClass the PVC will use, in precedence order:
   `keycloak.runtime.persistence.storageClass` → `global.storageClass` →
   cluster default StorageClass.
2. Verify it is encrypted:
   `kubectl get storageclass <name> -o yaml` and confirm the provider encryption
   parameter (AWS EBS: `parameters.encrypted: "true"`), or that the
   account/cluster enforces default volume encryption.
   Note: out of the box `global.storageClass` is `gp2`, which is **not**
   encrypted; treat "inherit" as "unencrypted unless you have verified otherwise."
3. If the inherited class is not encrypted, set
   `keycloak.runtime.persistence.storageClass` to an encrypted class in the
   per-env overlay.

This is a one-time per-cluster check; the PVC is retained across upgrades.

### External-RDS variant

If the environment uses an external RDS instead of the in-cluster postgres, set
`keycloak.runtime.db.autoWire=false` and pre-create the
`gardenio-keycloak-runtime-db` Secret with keys `url` (a full JDBC URL),
`username`, and `password`. The `create-db` init container is not rendered in
this mode — provisioning the database is the RDS task's responsibility.

## Edge migration — humans + CLI onto the in-box Keycloak (ka-0000006087)

Standing up the Keycloak server is only half the job: the **edge** that
authenticates real users still points at the cloud (GitLab-direct OIDC for the
browser, AVA + GitLab-PAT for the CLI). Neither works air-gapped. The
`deploy-values/gardenio-inbox.yaml` overlay flips both onto the in-box Keycloak.
See the research doc §6 (`docs/research/sso-identity-gardenio-in-a-box.md`).

**Browser edge (oauth2-proxy / forwardauth).** The base overlays set the front
door to `--provider=gitlab` against `gitlab.com`, gated on GitLab group
`example-org`. The inbox overlay overrides:

- `keycloak.forwardAuth.provider: oidc` — standard OIDC code flow.
- `keycloak.forwardAuth.oidcIssuerUrl: ""` — **leave empty**. The chart
  auto-derives the realm issuer from `runtime.host` + `httpRelativePath` +
  `realm.name` via the `gardenio.forwardAuth.issuerURL` helper (the forwardauth
  analogue of the §7.4 `realm.issuerURL` derivation), e.g.
  `https://auth.<domain>/auth/realms/gardenio-inbox`. An explicit value still
  wins, so the cloud path is untouched; when neither resolves to an `https://`
  issuer the deployment fail-fasts (no empty/plaintext issuer ever reaches
  oauth2-proxy).
- `keycloak.forwardAuth.gitlabGroups: []` — the GitLab group gate is a no-op
  with `provider=oidc`; realm membership is the authz model on the box.

**CLI / agent edge (edge-auth shim).** The cloud uses AVA + a GitLab-PAT
fallback. The overlay replaces both with the **oauth2-proxy cookie** path:

- `edgeAuth.ava.trustSigner: ""` — AVA off (AWS Verified Access does not exist
  off-cloud; an empty signer compiles the AVA branch out).
- `edgeAuth.patDisabled: true` — turns the GitLab-PAT path **off at the
  source**. The shim builds no GitLab validator, makes **no `gitlab.com` API
  calls**, and rejects any PAT-bearing request, so a PAT minted from stale org
  membership cannot authenticate even on a box with internet egress.
  > Note: just blanking `edgeAuth.gitlabGroup` is **not** enough — both the
  > chart and the shim re-default an empty group to `gardenio`, and a nil
  > validator would otherwise flip the readiness probe to NotReady and take the
  > whole edge (cookie auth included) down. `patDisabled` makes the readiness
  > gate accept cookie/AVA mode in the validator's place.
- `edgeAuth.cookieAuth.enabled: true` (+ `cookieName: _gardenio_oauth2`) — a
  request carrying the oauth2-proxy session cookie (from a browser, or from a
  `gb login` older than gd-5FThTGbOOP, which no longer captures it) is sub-requested to oauth2-proxy's `/oauth2/auth`; on `200` the
  shim stamps the attested email into the **same HMAC envelope**
  (`X-Operator` / `X-Edge-Auth-Sig` / `X-Edge-Auth-Ts`) that PAT/AVA produced.
  The daemon HMAC validator (`DAEMON_HMAC_KEY`) is unchanged.

Net: the in-box Keycloak realm becomes the single identity source for both the
browser and the CLI; the box makes no outbound auth calls.

## First-admin lock-out fallback

With `realm.login.userRegistration=false` and `defaultGroups: [gardenio-viewers]`,
the first OIDC user lands as a read-only viewer and there is no self-service
escape. The **first-admin bootstrap Job** (`ka-0000007117`) solves this
automatically; if it is disabled or fails, recover by port-forwarding to the
Keycloak admin console using the generated master-realm admin credentials:

```sh
kubectl -n <ns> get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.password}' | base64 -d
kubectl -n <ns> port-forward svc/<release>-keycloak-runtime 8080:8080
# open http://localhost:8080/auth/ and create a gardenio-realm admin user
```

> **Full runbook:** the happy path (enabling/verifying the Job, retrieving the
> one-time password) and the complete step-by-step lock-out fallback live in
> [`first-admin-bootstrap-runbook.md`](first-admin-bootstrap-runbook.md).
