# Jira / GitLab / WL Bridges Security Audit

**Date:** 2026-04-19
**Task:** ka-0000005263
**Scope:**
- `gardenio/controller/cmd/{jira-bridge,gitlab-bridge,wl-bridge}/main.go`
- `gardenio/controller/internal/bridge/{jira*.go,gitlab*.go,wasteland*.go,trigger_source_webhook.go}`
- `gardenio/images/{jira-bridge,gitlab-bridge,wl-bridge}/Dockerfile`
- `gardenio/helm/gardenio/templates/{jira-bridge,gitlab-bridge,wl-bridge}/**`
- `gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml`

**Prior audits:** `AUDIT.md` (2026-04-05), `AUDIT-2026-04-18.md` (Slack).

---

## 0. Attacker Model

Three concentric trust boundaries:

```
         ┌────────────── internet ────────────────┐
         │                                        │
  (a) atlassian.net      (b) gitlab.com      (c) anyone who
      webhook sender          webhook sender      reaches the
      (HMAC-signed)           (shared token)      public host
         │                       │                    │
         ▼                       ▼                    ▼
    ┌───────────────────────────────────────────────────────┐
    │          Traefik IngressRoute (public host)           │
    │     jira-webhook.host (Path=/webhook)                 │
    │     gitlab-bridge.host (PathPrefix=...)               │
    └───────────────────────────────────────────────────────┘
                              │
                              ▼
  ┌─────────────────┬─────────────────┬─────────────────┐
  │  jira-bridge    │ gitlab-bridge   │  wl-bridge      │
  │  :8091          │ :8092           │  :8092 (no web  │
  │  /webhook       │ /webhook        │   hook; health  │
  │  HMAC-SHA256    │ X-Gitlab-Token  │   only)         │
  │  fail-closed    │ bearer (==)     │                 │
  └─────────────────┴─────────────────┴─────────────────┘
                              │
                              ▼
               ┌─────────────────────────┐
               │ beads daemon            │
               │ (CreateBead, SpawnAgent)│
               └─────────────────────────┘
```

Primary attacker capabilities considered:

- **A1 — workspace external (no cluster access):** can POST to the public
  webhook host. Must defeat HMAC / bearer to reach bead mutations or agent
  spawn.
- **A2 — authenticated Jira/GitLab user (no secret):** can author comments,
  mentions, and issues. Webhook payload is authenticated, but author identity
  is transitively trusted.
- **A3 — in-cluster attacker (arbitrary namespace):** can reach bridge Service
  IPs directly (no HMAC gate because same as A1, but bypassing IP allowlist
  middleware).
- **A4 — operator with Helm values write:** can set webhook secrets, IP
  allowlists, and API tokens.

---

## 1. Findings Summary

| ID  | Severity | Title                                                                           |
|-----|----------|---------------------------------------------------------------------------------|
| J1  | HIGH     | GitLab webhook bearer-token compare is not constant-time                        |
| J2  | HIGH     | GitLab webhook does unbounded `io.ReadAll` before auth check                    |
| J3  | HIGH     | Generic webhook trigger source silently allows unsigned events                  |
| J4  | MEDIUM   | Generic webhook trigger source unbounded body read                              |
| J5  | MEDIUM   | No egress NetworkPolicies on jira/gitlab/wl-bridge pods                         |
| J6  | MEDIUM   | Plaintext `apiToken`/`webhookSecret` paths in Helm values                       |
| J7  | MEDIUM   | gitlab-bridge IngressRoute exposes entire pod via PathPrefix                    |
| J8  | MEDIUM   | wl-bridge Dockerfile installs `dolt` from `releases/latest` without checksum    |
| J9  | LOW      | JIRA mention allow/blocklists are hardcoded in source                           |
| J10 | LOW      | GitLab mention spawn has no author allowlist                                    |
| J11 | LOW      | `JIRA_CUSTOM_JQL` concatenated into search URL (operator-controlled only)       |
| J12 | INFO     | wasteland SQL string-concat (already tracked as H3 in prior audit)              |
| J13 | INFO     | Bridge-created beads carry `CreatedBy: "{bridge}"`, not webhook author          |
| J14 | INFO     | wl-bridge has no inbound webhook surface (poll-only)                            |

Status of checklist items:

| Checklist item                                      | Status                                        |
|-----------------------------------------------------|-----------------------------------------------|
| Jira webhook signature verification                 | ✅ HMAC-SHA256, fail-closed, constant-time     |
| GitLab webhook signature verification               | ⚠ Bearer only, timing-leakable (J1)           |
| WL webhook signature verification                   | N/A — no webhook endpoint (J14)               |
| Constant-time compare                                | Jira ✅ / GitLab ❌ (J1) / generic ✅           |
| Replay window / body integrity                       | ⚠ None on any bridge (no nonce, no timestamp) |
| Token scopes (JIRA PAT, GitLab PAT/OAuth)           | Deferred — runtime config, see §6              |
| Egress allow-list matches one SaaS host             | ❌ Missing (J5)                                |
| Input trust: markdown/html escaping                 | OK — comments passed as plain text to beads    |
| Input trust: command injection bridge→kbeads        | OK — typed HTTP API, no shell-out              |
| SSRF: follow-links-from-ticket-body                 | OK — bridges never fetch attacker URLs         |
| Reentrancy / `created_by` spoof                     | Mitigated — `CreatedBy` is bridge (J13)        |

---

## 2. HIGH Findings

### J1 — GitLab webhook token compared non-constant-time; no body HMAC

**File:** `gardenio/controller/internal/bridge/gitlab_sync.go:461`

```go
if r.Header.Get("X-Gitlab-Token") != webhookSecret {
    logger.Warn("webhook: invalid X-Gitlab-Token")
    http.Error(w, "Forbidden", http.StatusForbidden)
    return
}
```

Go's `string` `!=` compares byte-by-byte and short-circuits on first mismatch,
leaking a timing side channel on the **pre-auth** path. Combined with no
rate limit at the handler (only optional Traefik middleware; `rateLimit.enabled`
defaults to false in chart), an attacker who can reach the public ingress or
the Service IP can iteratively brute-force the secret one byte at a time.

Secondary problem: GitLab's `X-Gitlab-Token` is a **shared bearer**, not a
body HMAC. That means:

1. The payload body is unauthenticated; if the token leaks (logs, SIEM,
   nginx-ingress request captures, operator screenshots), an attacker can
   forge **any** MR / note / approval event and have the bridge write to
   arbitrary beads (`mr_merged=true`, spawn crew agents via @mentions).
2. There is no replay window — the same valid POST can be replayed
   indefinitely. Combined with `spawn-on-mention`, one captured request
   is a persistent agent-spawn oracle.

**Fix (minimum):** constant-time compare:

```go
if subtle.ConstantTimeCompare(
        []byte(r.Header.Get("X-Gitlab-Token")),
        []byte(webhookSecret)) != 1 { ... }
```

**Fix (ideal):** migrate to a body HMAC. GitLab does not natively sign
payloads, but a forward proxy (e.g. `smee.io`-style relay or a dedicated
ingestion Lambda) can wrap inbound events with HMAC-SHA256 of the raw body
under a rotating key; the bridge verifies that instead of `X-Gitlab-Token`.
Adds integrity + replay defense (timestamp + Nonce).

**Exploit sketch (A3, in-cluster):**
```bash
# Attacker in adjacent namespace
while read -r guess; do
  t0=$(date +%s%N)
  curl -s -o /dev/null -H "X-Gitlab-Token: $guess" \
       -H "Content-Type: application/json" \
       -d '{"object_kind":"merge_request"}' \
       http://gitlab-bridge.gardenio.svc.cluster.local:8092/webhook
  echo "$guess $(( $(date +%s%N) - t0 ))"
done < guesses.txt | sort -k2 -n
```

---

### J2 — GitLab webhook does `io.ReadAll(r.Body)` with no cap

**File:** `gardenio/controller/internal/bridge/gitlab_sync.go:473`

```go
body, err := io.ReadAll(r.Body)
```

Identical class to the Slack/Jira H2 finding that was closed yesterday
(commit `03589087`, ka-0000002162). No `http.MaxBytesReader`. An attacker
who passes the bearer check (or before the check, since read happens after
but with no cap) can post a multi-GB body and evict the pod.

Even **before** J1 is fixed, J2 is exploitable: the bearer check runs
*first*, but `r.Body` is buffered by the Go stdlib transport up to the
content-length — a slow-body attack (TCP write, `Transfer-Encoding:
chunked`) will tie up the goroutine before the check returns, multiplied
by concurrent connections. With default `MaxHeaderBytes` being the only
implicit guard, this is a clean resource-exhaustion vector.

**Fix:** mirror the Jira fix:

```go
const gitlabMaxBodyBytes = 1 << 20 // 1 MiB; MR note payloads ≤ few KB
r.Body = http.MaxBytesReader(w, r.Body, gitlabMaxBodyBytes)
body, err := io.ReadAll(r.Body)
```

Apply before `X-Gitlab-Token` check (the token check itself reads no body,
so order is immaterial, but put it first for defense-in-depth).

---

### J3 — Generic webhook trigger source: HMAC optional, silent no-auth path

**File:** `gardenio/controller/internal/bridge/trigger_source_webhook.go:251`

```go
if data.Secret != "" {
    if !verifyWebhookHMAC(body, r.Header, data.Secret) { /* deny */ }
}
// else: unauthenticated event buffered
```

When an operator or agent creates a trigger bead with `trigger:webhook`
and omits the `webhook_secret` field, the handler **accepts any body** for
the matching token. The path token (`/webhooks/{token}`) is the only
authentication.

The token is cryptographically random (`GenerateWebhookToken` uses
`crypto/rand`, 128 bits), but:

- it is rendered into the bead's fields and is visible to every agent
  with read access to the bead store;
- it is visible in referer/proxy/ingress logs;
- it routes into the trigger engine, which can fire `spawn_agent`,
  `apply_formula`, `create_bead` actions — i.e. **agent spawn**.

This is exactly the polarity inversion fixed in N4 for Slack
(ka-0000007471). The secure zero value is "HMAC required."

**Fix:** require a secret unless an explicit `allow_unsigned: true` field
is set on the trigger bead (and log prominently at load time). Refuse
`LoadTriggers` to register a trigger that lacks a secret *and* enables
action types `spawn_agent` / `apply_formula` / `create_bead`.

---

## 3. MEDIUM Findings

### J4 — Generic webhook trigger source: unbounded body read

**File:** `trigger_source_webhook.go:243`

```go
body, err := io.ReadAll(r.Body)
```

Same class as J2 / H2. Trigger engine allocates `map[string]any` over the
decoded payload plus flattens the entire tree into `TemplateVars` — an
attacker who can POST to `/webhooks/{token}` with a valid (or empty-secret)
trigger can drive the bridge into multi-GB allocation by supplying a deeply
nested JSON payload.

**Fix:** add `http.MaxBytesReader` (1 MiB is generous for generic trigger
payloads).

---

### J5 — No egress NetworkPolicies on jira / gitlab / wl-bridge

**File:** `gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml`

Grep shows ingress policies for all three bridges but **no egress policies**.
The chart restricts egress for agent pods, Grafana, vmagent, but leaves
bridges wide open to the internet.

Impact: if the JIRA API token leaks *and* an attacker gains code-exec on
the bridge (e.g. via a JSON unmarshaller bug), they can reach:

- any other internal service on ClusterIP (postgres, beads-gRPC, coopmux),
- any external host (exfil path).

The task checklist explicitly demands "Egress allow-list matches exactly
one SaaS host."

**Fix:** add per-bridge egress policies:

- **jira-bridge:** DNS + kube-dns, beads HTTP, kubernetes API for SSE, and
  one CIDR/FQDN for `*.atlassian.net` (443 only). Atlassian IPs rotate;
  prefer an `ipBlock` with maintained list or an egress gateway.
- **gitlab-bridge:** DNS, beads, and `gitlab.com` (or the configured
  `GITLAB_BASE_URL` host). Block RFC1918 egress except beads/coopmux.
- **wl-bridge:** DNS, beads, `doltremoteapi.dolthub.com`. `dolt pull`
  invokes `gcr.io`-equivalent over HTTPS.

Without this, the blast radius of any of the above findings is the whole
cluster, not just the bridge.

---

### J6 — Plaintext `apiToken` / `webhookSecret` paths in Helm values

**Files:**
- `gardenio/helm/gardenio/templates/jira-bridge/deployment.yaml:68-76, 123-132`
- `gardenio/helm/gardenio/templates/gitlab-bridge/deployment.yaml:61-80`

Both chart templates accept tokens inline:

```yaml
{{- if .Values.jiraBridge.jira.apiToken }}
- name: JIRA_API_TOKEN
  value: {{ .Values.jiraBridge.jira.apiToken | quote }}
{{- else if .Values.jiraBridge.jira.secretName }}
- name: JIRA_API_TOKEN
  valueFrom: ...
```

The `secretName` path is the safe one, but plaintext is checked *first* —
a `values.yaml` committed to Git with an operator-set token will silently
take precedence over a K8s Secret. Same for `jiraBridge.webhook.secret` and
`gitlabBridge.gitlab.webhookSecret`.

**Fix:** remove the inline string options, or at minimum:
- reorder the `if` so `secretName` wins when both are set;
- add a `fail "both set"` at chart-lint time;
- emit a `NOTES.txt` warning when inline is used.

Related AUDIT-2026-04-18 §N4 pattern: the secure mode should be the zero
value.

---

### J7 — gitlab-bridge IngressRoute uses `PathPrefix`

**File:** `gardenio/helm/gardenio/templates/gitlab-bridge/ingressroute.yaml:19`

```yaml
match: Host(`{{ $host }}`){{ if $pathPrefix }} && PathPrefix(`{{ $pathPrefix }}`){{ end }}
```

When `pathPrefix` is empty (default), the route matches the bare host and
forwards **every path** on that host to the pod. The pod currently exposes
`/healthz`, `/readyz`, and `/webhook`. Jira's IngressRoute correctly pins
`Path('/webhook')`.

Low-severity today (the only risky endpoint is `/webhook`, which self-gates
via bearer), but:

- any endpoint added to the mux later is immediately public unless the
  operator remembered to set `pathPrefix`;
- `/healthz` and `/readyz` leak version info.

**Fix:** always match `Path('/webhook')` (or add both `/webhook` and
`/healthz`/`/readyz` explicitly if health checks are needed from outside
the cluster, which they shouldn't).

---

### J8 — wl-bridge Dockerfile pulls `dolt` from `releases/latest`

**File:** `gardenio/images/wl-bridge/Dockerfile:27`

```dockerfile
RUN curl -fsSL https://github.com/dolthub/dolt/releases/latest/download/install.sh | bash
```

Three problems, compounded:

1. **`curl | bash`** — no script integrity check. A compromised dolthub
   account or GitHub releases endpoint delivers arbitrary code as root
   during image build.
2. **`releases/latest`** — unpinned version. Supply-chain rollback / pinning
   is impossible without editing the Dockerfile. Non-reproducible builds.
3. **No checksum** — even the downloaded tarball is not verified against a
   known SHA-256.

**Fix:**
- pin a specific tag (e.g. `v1.45.7`) via build arg;
- `curl -o install.sh … && sha256sum -c dolt-install.sha256 && bash install.sh`;
- pull the binary directly instead of the install script; or
- switch to an upstream `dolthub/dolt:<tag>` base image if one is published.

---

## 4. LOW Findings

### J9 — JIRA mention allow/blocklists hardcoded in source

**File:** `gardenio/controller/internal/bridge/jira_mentions.go:75-83`

```go
var blockedJiraProjects = map[string]bool{"FSM": true}
var allowedJiraProjects = map[string]bool{"PE": true}
```

Hard-coded in source. Supporting a new JIRA project requires a code change
+ release. Operator config (env var or project bead field) would let the
same image serve multiple tenants. Safety wins today but flexibility suffers.

---

### J10 — GitLab mention has no author allowlist

**File:** `gardenio/controller/internal/bridge/gitlab_mentions.go:80`

`HandleMention` trusts any commenter with MR access to spawn a `crew`
role agent with an arbitrary prompt. If the GitLab project has community
contributors or bots with comment rights, they can spawn agents (consuming
Anthropic spend and potentially running code against the MR branch).

Mitigation today: `GITLAB_BOT_USERNAME` is filtered out to prevent loops;
`blockedJiraProjects` analogue does not exist on the GitLab side.

**Fix:** add an author allowlist (env `GITLAB_MENTION_AUTHORS` or a
`mention_authors` project bead field). Default to "project members with
Developer role or higher" via a GitLab API check (adds a fetch; worth it).

---

### J11 — `JIRA_CUSTOM_JQL` concatenation

**File:** `gardenio/controller/internal/bridge/jira_poller.go` (via `jiraCustomJQL`)

The poller config accepts an arbitrary JQL clause that is concatenated
into the search query. Operator-controlled (env var), so not a direct
attacker vector today, but if any code path ever populates it from a bead
field (e.g. a config-bead-driven JQL) it becomes injectable. Flag for
tests and code review discipline.

---

## 5. INFORMATIONAL

### J12 — Wasteland SQL: still tracked as H3

`escapeSQLString` only doubles `'`. No backslash / NUL / CR handling.
Dolt's SQL dialect does not interpret `\'` as an escape in string
literals, which is the sole reason this hasn't exploded. Inputs to
`ClaimWanted` and `SubmitCompletion` (e.g. `evidence = "Bead … Title: "
+ bead.Title`) include agent-editable bead titles. Replace with
parameterized dolt SQL via stdin or use `dolt sql-client` with bound
parameters. Tracked as H3 in AUDIT.md; repeating here because it lives
in the bridge scope.

### J13 — `CreatedBy: "{bridge}"`, not webhook author

`jira_webhook.go`, `jira_mentions.go`, `gitlab_mentions.go` all create
beads with `CreatedBy: "jira-bridge"` / `"gitlab-bridge"`. The actual
webhook author appears only in `fields["mention_author"]`. This is the
correct defensive choice — bead provenance traces to the bridge identity,
not spoofable external actor — but any downstream code that uses
`CreatedBy` for authorship attribution (audit logs, trust boundaries)
must be aware. Inline comment would help.

### J14 — wl-bridge: poll-only, no webhook surface

`cmd/wl-bridge/main.go` exposes only `/healthz` and `/readyz`. All dolt
interactions are outbound (pull / push). Scope checklist item "WL:
HMAC/sig header" is N/A. No attack surface beyond supply-chain (J8) and
wasteland dolt table integrity (J12).

---

## 6. Token Scope — Deferred

The audit checklist requests a review of:
- JIRA PAT scopes (`jiraBridge.jira.apiToken`)
- GitLab PAT/OAuth scopes (`gitlabBridge.gitlab.apiToken`)

Token scopes are runtime configuration, not in the repository. Required
API coverage by grep of `JiraClient` / `GitLabClient` methods:

| Bridge   | Operations observed                                   | Minimum required scope                     |
|----------|--------------------------------------------------------|--------------------------------------------|
| jira     | `search`, `get issue`, `transition`, `add comment`, `remote link` | `read:jira-work`, `write:jira-work`, `write:issue`, `read:issue.remote-link`, `write:issue.remote-link` |
| gitlab   | `list MRs`, `get MR`, `get pipeline`, `post note`, `post discussion` | `api` (GitLab bundles MR/note API under `api` for PATs) — or finer-grained OAuth scopes `read_api`, `write_repository` if migrated to OAuth |

Recommended follow-up task: verify in JIRA / GitLab admin that the live
tokens have **no admin** scopes and cannot modify project config,
permissions, or user accounts. Document the expected scope set in the
chart's `values.yaml` comments next to the `apiToken` / `secretName`
fields.

---

## 7. Suggested Remediation Order

1. **J2** (GitLab `MaxBytesReader`) — tiny diff, high-confidence; prevent
   easy DoS today. [~30 LOC + test]
2. **J1** (GitLab constant-time compare) — two-line diff; closes the
   timing side channel immediately. [~10 LOC + test]
3. **J3** (webhook-trigger fail-closed) — invert polarity; port the
   N4/N5 pattern exactly. [~40 LOC + test]
4. **J4** (webhook-trigger `MaxBytesReader`) — ride along with J3.
5. **J5** (egress NetworkPolicies) — chart work, no Go changes.
6. **J7** (gitlab IngressRoute `Path` pin) — one-line Helm fix.
7. **J8** (wl-bridge dolt pin + checksum) — supply-chain hardening,
   coordinate with a release bump.
8. **J6** (Helm plaintext secret paths) — remove or warn.
9. **J10** (GitLab mention author allowlist) — needs product discussion on
   trust model.
10. **J9, J11, J12** — tracked, no immediate fix.

---

## 8. Proposed Follow-Up Beads

| Title                                                                 | Labels                             |
|-----------------------------------------------------------------------|------------------------------------|
| GitLab bridge J1: constant-time compare X-Gitlab-Token                | project:gardenio, security, bug     |
| GitLab bridge J2: bound webhook body with MaxBytesReader              | project:gardenio, security, bug     |
| Trigger engine J3/J4: fail-closed + bounded body on webhook triggers  | project:gardenio, security, bug     |
| J5: add egress NetworkPolicies for jira/gitlab/wl-bridge              | project:gardenio, security          |
| J6: remove plaintext apiToken path in bridge charts                   | project:gardenio, security          |
| J7: pin gitlab-bridge IngressRoute to /webhook                        | project:gardenio, security          |
| J8: pin + checksum dolt install in wl-bridge Dockerfile               | project:gardenio, security, supply-chain |
| J10: GitLab mention author allowlist                                  | project:gardenio, security          |

These should be filed separately so each can be claimed and shipped
independently of the audit doc.
