# Secrets & Credentials Synthesis

Cross-cutting synthesis of the three secrets audits filed under research epic
[ka-0000001193](https://beads-viewer.e2e.example.com/bead/ka-0000001193).

Source audits (closed):

- **beads-viewer** — [ka-0000002836](https://beads-viewer.e2e.example.com/bead/ka-0000002836)
- **kbeads + gardenio** (controller, bridges, agent images, helm, coop) — [ka-0000002746](https://beads-viewer.e2e.example.com/bead/ka-0000002746)
- **Incident pattern analysis** (7 clusters across ~200 beads) — [ka-0000001794](https://beads-viewer.e2e.example.com/bead/ka-0000001794)

Related open epics (cross-linked):

- [ka-0000006809](https://beads-viewer.e2e.example.com/bead/ka-0000006809) — operated_by mandatory on every write path
- [ka-0000006932](https://beads-viewer.e2e.example.com/bead/ka-0000006932) — gb-daemon threat model
- [ka-0000004678](https://beads-viewer.e2e.example.com/bead/ka-0000004678) — permissions model

---

## Executive summary

The gardenio stack has a **sound baseline credential architecture** — single
source of truth in the controller, AES-256-GCM encryption at rest in coopmux,
mandatory HMAC on all bridge webhooks, automated `BEADS_DAEMON_TOKEN` rotation,
IRSA in production, no tokens logged at any level in beads-viewer. Where it
fails, it fails in five recurring shapes:

1. **Encryption / hardening is optional, not mandatory.** Defaults silently
   downgrade — coop persists plaintext when `COOP_CREDENTIAL_KEY` is unset,
   helm allows raw `Secret` fallbacks for Slack/JIRA/NATS, cookie `Secure`
   flag is missing on viewer-set cookies. Operators can deploy "working but
   insecure" with no startup error.
2. **Secrets leak as a side effect of telemetry.** Components that aren't
   supposed to handle credentials end up handling them: terminal recordings
   capture `aws sts` paste-buffers, `statusreporter` wrote coop bearers into
   bead notes, `buildCoopEnv` inherits `os.Environ()` wholesale. The audit
   model is "where do credentials enter?" but the leak surface is "where can
   anything write to durable storage?"
3. **Referenced ≠ provisioned.** `K6_PERF_BASTION_PRIVATE_KEY` is documented
   in advice but has no `AgentPodSpec` field; `BEADS_TOKEN` is referenced by
   RWX coverage-ratchet but missing from the default vault. Same shape: a
   capability documented in one layer and never threaded through to another.
4. **Identity propagation is treated as a UX feature, not a security
   property.** `operated_by` enforcement (ka-0000005772, ka-0000006809)
   shipped server-side before clients migrated and red-lit 67 E2E modules.
   Async paths (NATS, slack-bridge, JIRA webhook) still bypass it. The
   forwardauth / oauth2-proxy cookie bugs are the same family at the browser
   layer — who is acting for whom is not first-class.
5. **Rotation is reactive, not rehearsed.** The 2026-03 coopmux bearer leak
   (ka-0000000684) was discovered by audit, not by drill. No `git filter-repo`
   rewrite was performed (token is still in history). No periodic fire-drill
   exists for Slack / JIRA / GitLab / Keycloak admin / AWS SM-unreachable.

The W1-W10 short-term list from ka-0000002746 addresses items in shapes (1),
(3), and parts of (5). The structural recommendations below address (2) and
(4).

---

## Cross-cutting themes

### Theme A — Encryption by default, fail-closed at startup

Three audit findings share the same root cause: optional encryption with
silent fallback.

- **W1**: `coop/crates/cli/.../persist.rs:234` writes plaintext
  `credentials.json` when `COOP_CREDENTIAL_KEY` is unset (warn-only).
- **W5**: ~~NATS agent token is plaintext env-var via raw `Secret` fallback
  (`agent-creds-secret.yaml:42`) when ExternalSecrets aren't enabled.~~ —
  **landed (ka-0000006585)**: token is now projected as a tmpfs-backed file
  at `/etc/nats-token/token` and consumed via `$COOP_NATS_TOKEN_FILE`; no
  longer present in `/proc/self/environ` or `kubectl describe pod`. See
  [`nats-token-hardening.md`](nats-token-hardening.md).
- **viewer A**: cookie `Secure: false` (literal absence) on viewer-set
  cookies (`agents.go:95-102`, `migration_banner.go:103-110`).
- Cluster 1 (ka-0000007475): chart "allows plaintext for Slack /
  JIRA-webhook / NATS / Claude-OAuth / autodestruct".

**Pattern**: each is a per-component knob that defaults open.

**Recommendation**: enforce **fail-closed** at startup. Each component should
refuse to start if its credential path is plaintext-eligible:

- coopmux: error (not warn) when `COOP_CREDENTIAL_KEY*` is unset; remove the
  plaintext migration code path or gate it behind an explicit
  `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` that logs WARN at every credential read.
- helm: `values.schema.json` should require `externalSecrets.enabled=true`
  when the deployment target is `production`; raw `Secret` fallbacks remain
  only for `env=dev`.
- viewer: `Secure: r.TLS != nil || isDevMode()` — the existing pattern
  already used elsewhere in the codebase.

### Theme B — Telemetry as a credential exfil channel

Three closed bugs and one open audit gap describe the same failure mode:
non-credential components writing credentials to durable storage.

- **ka-0000003272** — coop recorder uploads full screen contents (pasted
  `aws sts`, `gh auth status`) to S3 without redaction.
- **ka-0000000804** — `statusreporter` persisted coop bearer in bead notes
  (gosec HIGH).
- **ka-0000007438** — `buildCoopEnv` inherits `os.Environ()` wholesale
  (`init_agent_loop.go`), leaking `BD_DAEMON_TOKEN`, `BEADS_*`, etc., into
  every agent pod's env.
- Implicit gap from ka-0000001794 — no bead on telemetry/metrics proving
  secrets never appear in Mezmo log streams (gitleaks scans disk only).

**Pattern**: the perimeter is drawn at "credential entry points" but
secrets exit through general-purpose write paths (recordings, status bead
notes, env-var inheritance, log forwarding).

**Recommendation**: add a **redaction layer at the egress boundary**, not at
each writer. Three concrete pieces:

1. **Allowlist `buildCoopEnv`** (already proposed in ka-0000007438): replace
   "inherit os.Environ() and filter by deny-list" with "start empty and add
   only known-safe keys".
2. **Secret-pattern scrubber on coop recorder output** before S3 upload —
   share the gitleaks rule set; scrub matched ranges with `[REDACTED]`.
3. **Mezmo-side secret detector**: a periodic job that scans the last 24h
   of log volume for `(token|key|secret).*[A-Za-z0-9+/]{32,}` patterns and
   files a P0 bead on any hit. This catches the leak even if (1) and (2)
   miss; "no hits" becomes a live signal we can ratchet on.

### Theme C — Referenced vs. provisioned (the "missing field" pattern)

Two open bugs and one advice item match this shape, plus the historical
forwardauth bugs (ka-0000006657 CSRF cookie limit) where a flag was
referenced in one config layer but not provisioned in another.

- **W2** — advice [ka-0000001600](https://beads-viewer.e2e.example.com/bead/ka-0000001600)
  references `K6_PERF_BASTION_PRIVATE_KEY` for SSH to the perf bastion;
  there is no field in `AgentPodSpec` to materialize it. Operators do it
  out-of-band.
- **ka-0000006417 / ka-0000005501** — RWX coverage-ratchet references
  `BEADS_TOKEN`; missing from RWX "default" vault → 100% red.

**Pattern**: a capability is documented in advice or referenced by code in
one component, but the credential plumbing is not threaded through to the
component that actually injects it.

**Recommendation**: every credential reference (env var name, secret key,
file path) should be **declared in exactly one schema** that all consumers
import. For agent pods, that schema is `AgentPodSpec`. Add:

- A `SshKeys map[string]SecretRef` field (or generic `ExtraSecretFiles`) so
  per-bastion keys can be declared once, materialized into
  `/etc/ssh-keys/<name>` with mode 0400.
- A lint/validation step that walks all `kd advice` records and verifies any
  env-var-name token they reference exists either in `AgentPodSpec` or in
  the documented "operator out-of-band" allowlist.

### Theme D — Identity as a security property, not a UX nicety

Cluster 3 (operated_by) and Cluster 4 (forwardauth / oauth2-proxy / iOS
WebKit) are the same family viewed from server and browser ends. The
operated_by work (epic ka-0000006809) made the server-side first-class but
async paths still bypass it.

- **R3** (ka-0000002887) — open: resolve `operated_by` on every write path
  (R1 inventory exists, R3 server changes partial).
- **Implicit gap** — NATS, slack-bridge inbound webhook, JIRA webhook all
  carry their own identity context that isn't normalized into `operated_by`.

**Recommendation**: treat `operated_by` propagation as a **W-track** in
parallel to the W1-W10 short-term fixes — concrete tasks per async path
filed as children of [ka-0000006809](https://beads-viewer.e2e.example.com/bead/ka-0000006809).
Not duplicated here; this synthesis adds the cross-link to the secrets epic.

### Theme E — Rotation must be a drill, not a fire

- **Cluster 1**: 2026-03 coopmux leak (ka-0000000684) discovered by audit,
  not by drill.
- **Implicit gap**: no bead tracks `git filter-repo` to purge the leaked
  token from history (ka-0000001794) — token is still readable in any
  clone.
- **Implicit gap**: no bead on Slack / JIRA / GitLab / Keycloak admin
  rotation cadence audit (ka-0000007475 flagged "Manual" with no follow-up).
- **Implicit gap**: no bead exercises "AWS SM unreachable" failure mode of
  ExternalSecret 1h refresh.

**Recommendation**: file three explicit follow-on tasks (see W-list below
for the operational ones; structural ones below):

1. **Quarterly rotation fire-drill epic**: walk Slack, JIRA, GitLab,
   Keycloak admin, coopmux bearer, `BEADS_DAEMON_TOKEN`, AWS IRSA, NATS
   token. Each gets a documented rotation runbook + last-rotated timestamp
   field in the secret's ExternalSecret manifest.
2. **History rewrite of leaked tokens**: file a separate (gated) task to
   `git filter-repo` the coopmux token from history. This requires operator
   approval because it rewrites all clones — escalation, not autonomous.
3. **ExternalSecrets failure-mode integration test**: simulate SM
   unreachability for > `refreshInterval`; assert agents stay up on cached
   creds and a single, deduplicated alert fires.

---

## Short-term fix backlog (W1–W10)

These are the items from the kbeads + gardenio audit (ka-0000002746 §8/§9).
Filed as individual task beads as parent-children of ka-0000001193 so
they're claimable by future gardener-delivery cycles. IDs are listed in the
epic; cross-reference there.

| W   | Severity | Summary                                                              | Theme |
| --- | -------- | -------------------------------------------------------------------- | ----- |
| W1  | Moderate | Make `COOP_CREDENTIAL_KEY` mandatory (no plaintext fallback)         | A     |
| W2  | High*    | `K6_PERF_BASTION_PRIVATE_KEY` field in `AgentPodSpec`                 | C     |
| W3  | Moderate | Beads-token revocation-list HA / fallback path                       | E     |
| W4  | Low      | ProjectScoped secret reconciliation race (pod sched < ES exists)    | A     |
| W5  | Low      | NATS agent token env-var hardening (mount/tmpfs over env)            | A     |
| W6  | Moderate | Git creds via credential helper, not init-container env              | B     |
| W7  | Low      | Narrow gitleaks `_test.go` blanket allowlist                          | B     |
| W8  | Low      | Document ExternalSecret `refreshInterval` guidance per secret type   | A/E   |
| W9  | Low      | Document/require mTLS between controller and coopmux                 | A     |
| W10 | Moderate | Coop credential file mode/tmpfs hardening                            | A     |

\* "High if used" — perf bastion path may be live or dormant.

### Beads-viewer short-term (from ka-0000002836 §6)

Not duplicated into separate W beads (smaller surface, three of four are
one-line fixes); roll up into a single "viewer hardening" task child of
this epic:

- Conditional `Secure` cookie flag (`r.TLS != nil || isDevMode()`)
- Remove redundant `BEADS_DAEMON_TOKEN` copy (`main.go:47` arg unused)
- Document cookie posture in helm chart values
- Confirm CSRF posture: daemon validates `operated_by` / origin on writes

---

## Structural recommendations (multi-week, file as own epics)

These are larger than W-list items. Each warrants its own epic if pursued
(do not file the epics in this pass — surface as decision options for the
operator).

1. **Credential-helper sidecar pattern** (replaces env-var injection for
   git, NATS, project-secrets). Each pod gets a sidecar with `/var/run/cred`
   socket; broker is the only process that holds raw creds. Closes W5, W6,
   W10, and reduces the egress surface from Theme B.
2. **Per-credential audit log** — every read/write of a managed credential
   emits a structured event (`credential.read`, `.write`, `.rotate`,
   `.refresh`) to a dedicated Mezmo stream. Pairs with the secret-pattern
   scrubber from Theme B as the positive signal.
3. **Rotation integration tests** — automated drills (mid-session refresh,
   rolling restart, revocation overlap, SM unreachable). Closes Theme E
   gaps.
4. **Schema-enforced production posture** — `values.schema.json` rejects
   raw `Secret` fallbacks when `env=production`. Closes Theme A.
5. **Secret-pattern redaction layer** on pod stdout/stderr before Slack /
   kubectl surfaces (and S3 recorder uploads). Closes Theme B at the
   egress.
6. **`operated_by` async propagation** — already an open thread under
   ka-0000006809 / ka-0000002887. Cross-linked from this epic; no new epic
   needed.

---

## Hand-off / what to do next

1. **W1–W10 tasks** are filed as children of ka-0000001193 (see epic
   "Mentioned by" list). Gardener-delivery cycles can claim them
   individually.
2. **Decision needed from operator**: which structural recommendations
   (1–5 above) become epics now vs. defer. Gardener-intake should surface
   this as a decision in the next cycle (out of scope for this docs-only
   pass).
3. **Cross-links filed** to ka-0000006809, ka-0000006932, ka-0000004678
   via `kd dep add` so this epic appears in the graph neighborhood when
   any of those is opened in beads-viewer.
4. **History rewrite** of the leaked coopmux token (ka-0000000684) is
   **not** filed autonomously — it needs operator authorization because
   it rewrites all clones. Surface via decision in next gardener cycle.
