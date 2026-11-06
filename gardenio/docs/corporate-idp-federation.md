# Corporate IdP Federation

Federate gardenio's Keycloak realm with Acme's corporate SSO (Google Workspace,
Azure AD, or a generic OIDC provider). When enabled, employees log in with their
corporate credentials. When someone leaves Acme, their corporate SSO is disabled
and they immediately lose gardenio access.

## Architecture

```
User → Keycloak Login Page → "Login with Acme SSO" button
                                    ↓
                            Corporate IdP (Google/Azure)
                                    ↓
                            OIDC callback to Keycloak
                                    ↓
                            First-broker-login flow
                            (auto-creates local user)
                                    ↓
                            Keycloak session + JWT issued
                                    ↓
                            oauth2-proxy validates JWT
                                    ↓
                            Beads API validates JWT + email domain
```

**Three layers of email domain enforcement:**

| Layer | Config | What it does |
|---|---|---|
| Corporate IdP | Google: `hd` claim / Azure: tenant restriction | Only org members can authenticate |
| oauth2-proxy | `keycloak.forwardAuth.emailDomains` | Rejects tokens with non-matching email |
| Beads API | `keycloak.oidcAllowedEmailDomains` | Rejects API requests with non-matching email |

## Prerequisites

- Keycloak is enabled (`keycloak.enabled: true`) and the realm is created
- Admin access to the corporate IdP (Google Workspace Admin Console or Azure Portal)
- A secret store configured for ExternalSecrets (recommended for production)

## Setup: Google Workspace

### 1. Create OAuth app in Google Cloud Console

1. Go to [Google Cloud Console](https://console.cloud.google.com/) > APIs & Services > Credentials
2. Create an **OAuth 2.0 Client ID** (type: Web application)
3. Set the authorized redirect URI to:
   ```
   https://{keycloak-host}/auth/realms/gardenio/broker/corporate/endpoint
   ```
   Replace `{keycloak-host}` with your Keycloak domain (e.g., `auth.e2e.dev.acme.example`).
4. Note the **Client ID** and **Client Secret**

### 2. Restrict to your Google Workspace domain

In the OAuth consent screen settings:
- Set "User type" to **Internal** (limits to your Google Workspace organization)
- This ensures only `@acme.example` (or your domain) users can authenticate

### 3. Configure Helm values

In your deployment repo values file (NOT in the chart defaults):

```yaml
keycloak:
  corporateIdp:
    enabled: true
    provider: "google"
    displayName: "Acme SSO"
    clientId: "YOUR_GOOGLE_CLIENT_ID.apps.googleusercontent.com"
    # clientSecret: set via ExternalSecrets (preferred) or direct value
    allowedEmailDomain: "acme.example"
    syncMode: "FORCE"
    # Optional: skip Keycloak login page, go straight to Google
    # authenticateByDefault: true

  # Lock down email domains at all layers
  oidcAllowedEmailDomains: "acme.example"

  forwardAuth:
    emailDomains:
      - "acme.example"
```

### 4. Store credentials via ExternalSecrets (recommended)

```yaml
externalSecrets:
  enabled: true
  corporateIdp:
    remoteRef: "path/to/corporate-idp-secret"
    secretName: "gardenio-corporate-idp-credentials"
```

The ExternalSecret creates a K8s Secret with `client-id` and `client-secret` keys.

### 5. Deploy

```bash
helm upgrade gardenio ./helm/gardenio -f your-values.yaml
```

Verify the `KeycloakRealmIdentityProvider` CR was created:
```bash
kubectl get keycloakrealmidentityprovider -n gardenio
```

## Setup: Azure AD (Entra ID)

### 1. Register an app in Azure Portal

1. Go to [Azure Portal](https://portal.azure.com/) > Azure Active Directory > App registrations
2. Click **New registration**
3. Set the redirect URI to:
   ```
   https://{keycloak-host}/auth/realms/gardenio/broker/corporate/endpoint
   ```
4. Under **Supported account types**, select:
   - **Accounts in this organizational directory only** (single tenant)
5. Note the **Application (client) ID** and **Directory (tenant) ID**
6. Under Certificates & Secrets, create a new **Client Secret** and note the value

### 2. Configure API permissions

Add the following Microsoft Graph permissions (delegated):
- `openid`
- `email`
- `profile`

Grant admin consent for these permissions.

### 3. Configure Helm values

```yaml
keycloak:
  corporateIdp:
    enabled: true
    provider: "azure"
    displayName: "Acme SSO"
    clientId: "YOUR_AZURE_APP_CLIENT_ID"
    azureTenantId: "YOUR_AZURE_TENANT_ID"
    allowedEmailDomain: "acme.example"
    syncMode: "FORCE"

  oidcAllowedEmailDomains: "acme.example"

  forwardAuth:
    emailDomains:
      - "acme.example"
```

### 4. Store credentials and deploy

Same as Google Workspace (steps 4-5 above).

## Setup: Generic OIDC Provider

For Okta, OneLogin, or other OIDC-compliant providers:

```yaml
keycloak:
  corporateIdp:
    enabled: true
    provider: "oidc"
    displayName: "Acme SSO"
    clientId: "YOUR_CLIENT_ID"
    authorizationUrl: "https://your-idp.example.com/authorize"
    tokenUrl: "https://your-idp.example.com/token"
    userInfoUrl: "https://your-idp.example.com/userinfo"
    jwksUrl: "https://your-idp.example.com/.well-known/jwks.json"
    issuer: "https://your-idp.example.com"
    allowedEmailDomain: "acme.example"
    syncMode: "FORCE"
```

All endpoint fields are required for generic OIDC (the template validates this).

## Disabling Local Registration

Local password registration is **already disabled by default** in the gardenio Keycloak realm:

```yaml
keycloak:
  realm:
    login:
      userRegistration: false   # ← already false
```

This means users cannot create accounts with a local password. They must authenticate
via an identity provider (GitLab, corporate SSO, etc.).

To fully lock down to corporate SSO only:

1. **Disable the GitLab IdP** (if no longer needed):
   ```yaml
   keycloak:
     identityProviders:
       - alias: gitlab
         enabled: false   # ← disable GitLab login
         # ... rest of config ...
   ```

2. **Make corporate SSO the default** (skip Keycloak login page):
   ```yaml
   keycloak:
     corporateIdp:
       authenticateByDefault: true
   ```

3. **Restrict email domains** at all layers (see examples above).

## User Lifecycle

### First Login (New User)

1. User clicks "Acme SSO" on the Keycloak login page
2. Redirected to corporate IdP, authenticates with corporate credentials
3. Keycloak's `first broker login` flow creates a local user account
4. User is assigned to the `gardenio-viewers` group (default group)
5. Admin can assign additional roles/groups as needed

### Subsequent Logins

1. User clicks "Acme SSO" on the Keycloak login page
2. Authenticates with corporate IdP
3. With `syncMode: "FORCE"`, Keycloak updates name/email from IdP on every login
4. User gets a Keycloak session (30min idle, 10h max by default)

### User Offboarding

1. Acme IT disables the user's corporate SSO account
2. User can no longer authenticate with the corporate IdP
3. Keycloak session expires naturally (or admin can force logout)
4. No gardenio access is possible without valid SSO

## Troubleshooting

### "Invalid redirect URI" error

Verify the redirect URI in the corporate IdP app registration matches exactly:
```
https://{keycloak-host}/auth/realms/gardenio/broker/corporate/endpoint
```

### Users can't log in after enabling corporate IdP

1. Check the `KeycloakRealmIdentityProvider` CR status:
   ```bash
   kubectl describe keycloakrealmidentityprovider gardenio-idp-corporate -n gardenio
   ```
2. Verify client credentials are correct (check ExternalSecret sync status)
3. Check Keycloak logs for OIDC discovery errors

### Existing users can't link corporate SSO

If a user already has a local Keycloak account with the same email, the
`first broker login` flow will prompt them to link accounts. They need to:
1. Click "Add to existing account"
2. Enter their existing Keycloak password
3. Future logins will use corporate SSO

### Email domain restriction not working

Email domain is enforced at three layers — check each:
1. **Corporate IdP**: Google (Internal OAuth app) / Azure (single-tenant app)
2. **oauth2-proxy**: `forwardAuth.emailDomains` in values
3. **Beads API**: `oidcAllowedEmailDomains` in values

## Helm Values Reference

| Value | Default | Description |
|---|---|---|
| `keycloak.corporateIdp.enabled` | `false` | Enable corporate IdP federation |
| `keycloak.corporateIdp.provider` | `"google"` | Provider type: `google`, `azure`, `oidc` |
| `keycloak.corporateIdp.displayName` | `"Acme SSO"` | Login button label |
| `keycloak.corporateIdp.trustEmail` | `true` | Skip email verification for SSO users |
| `keycloak.corporateIdp.firstBrokerLoginFlowAlias` | `"first broker login"` | Auth flow for first-time users |
| `keycloak.corporateIdp.clientId` | `""` | OIDC client ID from IdP app registration |
| `keycloak.corporateIdp.clientSecret` | `""` | OIDC client secret (prefer ExternalSecrets) |
| `keycloak.corporateIdp.azureTenantId` | `""` | Azure AD tenant ID (required for azure) |
| `keycloak.corporateIdp.allowedEmailDomain` | `""` | Email domain restriction |
| `keycloak.corporateIdp.syncMode` | `"FORCE"` | `IMPORT` or `FORCE` (sync on every login) |
| `keycloak.corporateIdp.authenticateByDefault` | `false` | Skip login page, go straight to IdP |
| `keycloak.corporateIdp.defaultScope` | `"openid email profile"` | OIDC scopes |
| `keycloak.corporateIdp.authorizationUrl` | auto | OIDC authorization endpoint |
| `keycloak.corporateIdp.tokenUrl` | auto | OIDC token endpoint |
| `keycloak.corporateIdp.userInfoUrl` | auto | OIDC userinfo endpoint |
| `keycloak.corporateIdp.jwksUrl` | auto | OIDC JWKS endpoint |
| `keycloak.corporateIdp.issuer` | auto | OIDC issuer |
| `externalSecrets.corporateIdp.remoteRef` | `""` | Secret store path for IdP credentials |
| `externalSecrets.corporateIdp.secretName` | `"gardenio-corporate-idp-credentials"` | K8s Secret name |
