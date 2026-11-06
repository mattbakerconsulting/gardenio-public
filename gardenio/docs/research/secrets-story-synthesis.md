# Secrets Story Synthesis

**Source epic:** [ka-0000001193](https://beads-viewer.e2e.example.com/bead/ka-0000001193) — *Research epic: improve secrets story across beads-viewer, beads, gardenio*
**Synthesized:** 2026-05-24 by `gardenio-research-epic-improve-mit` (crew)
**Inputs:**
- [ka-0000002836](https://beads-viewer.e2e.example.com/bead/ka-0000002836) — beads-viewer audit
- [ka-0000002746](https://beads-viewer.e2e.example.com/bead/ka-0000002746) — kbeads + gardenio (controller / bridges / coop / helm) audit
- [ka-0000001794](https://beads-viewer.e2e.example.com/bead/ka-0000001794) — pattern analysis of recent secrets-related issues

This is a **docs-only synthesis**. No fixes are implemented here. Each candidate
task below is a *proposed* child task under the parent epic; the operator (or a
gardener) is expected to ratify, prioritize, and dispatch them.

---

## 1. Executive Summary

The gardenio stack's secrets architecture has a sound spine: a single source of
truth in the controller, AES-256-GCM encryption-at-rest in the coopmux broker
(when keyed), mandatory HMAC verification on bridge webhooks, IRSA for AWS, and
an automated weekly beads-token rotation CronJob with a revocation-list
ConfigMap. **The problems are not architectural — they are coverage gaps.**

The three audits independently converged on the same five themes:

1. **Env-var leakage** — credentials propagate via `os.Environ()` into agent
   pods, init-containers, terminal recordings, and bead notes.
2. **Git-history leaks** — a real coopmux bearer was committed in 2026-03 and
   still lives in `git log -p`; gitleaks coverage is blanket-allowlisted in
   tests; no history-rewrite has been performed.
3. **Coop uploads & telemetry** — terminal recordings (`coop record`) and
   statusreporter persist unredacted screen / note content to S3 and the
   beads DB, where pasted creds, `aws sts` output, etc. survive forever.
4. **Credential rotation** — rotation is reactive, not drilled. The
   single revocation-list ConfigMap is itself a single point of failure during
   overlap; no fire-drill bead, no integration test exercising rotation under
   load.
5. **operated_by propagation** — the `X-Operated-By` cutover (ka-0000005772)
   broke ~67 E2E modules because async paths (NATS, slack-bridge webhook
   re-emits) were not migrated. Identity is fuzzy at the "who acts on whose
   behalf" boundary and asynchronously detached.

Beneath these, two structural opportunities recur:

- **Plaintext fallback paths exist where they should not.** `COOP_CREDENTIAL_KEY`
  unset → coop persists credentials in plaintext on disk (`persist.rs:234`).
  Several helm chart paths permit raw K8s Secret fallbacks for Slack / JIRA-
  webhook / NATS / Claude-OAuth / autodestruct credentials.
- **There is no central, auditable layer between credential producers
  (controller / coopmux) and consumers (agent pods, bridges, viewer).** Every
  consumer reads credentials its own way — env vars, file mounts, helper API,
  bearer headers — and every leak we've shipped has been a consumer side-effect
  (recorder, statusreporter, init-clone env, env inheritance).

---

## 2. Findings Grouped by Severity

Severity reflects the synthesis author's read across all three audits; it is
**not** a CVSS score. P0 = ship a fix in the next sprint; P1 = ship in the
next quarter; P2 = structural, schedule into roadmap.

### P0 — Ship now

| ID | Finding | Source | Theme |
|---|---|---|---|
| **F1** | **Coop plaintext credential fallback** — when `COOP_CREDENTIAL_KEY` is unset, coop persists Anthropic OAuth tokens, etc. as plaintext to `$COOP_MUX_STATE_DIR/credentials.json` with only a `warn` log. (`coop/crates/mux/src/credential/persist.rs:234`) | ka-0000002746 W1 | env-var leakage / coop uploads |
| **F2** | **`buildCoopEnv` inherits `os.Environ()` wholesale** into the coop child process, silently leaking `BD_DAEMON_TOKEN`, `BEADS_*`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY`, etc., to the agent pod. (`gardenio/controller/cmd/gb/init_agent_loop.go`) | ka-0000001794 Cluster 2 / ka-0000007438 | env-var leakage |
| **F3** | **Coop terminal recordings upload unredacted screen content to S3** — pasted credentials, `aws sts`, `gh auth status`, browser cookie copies, etc., persist forever and are readable by anyone with bucket access. (`coop/crates/cli/src/record.rs` + `s3.rs`) | ka-0000001794 Cluster 2 / ka-0000003272 | coop uploads |
| **F4** | **Leaked coopmux bearer still in git history** — ka-0000000684 rotated the token but no `git filter-repo` / BFG rewrite was performed. Any pre-rotation clone still contains it. | ka-0000001794 Cluster 1 / "implicit gaps" | git-history leaks |
| **F5** | **statusreporter persisted coop bearer in bead notes** (gosec HIGH) — same family as F3: a "normal telemetry" component wrote a secret to durable storage. Code-fixed in ka-0000000804 but the pattern needs a generic redaction layer. | ka-0000001794 Cluster 2 / ka-0000000804 | env-var leakage |

### P1 — Ship this quarter

| ID | Finding | Source | Theme |
|---|---|---|---|
| **F6** | **Git creds in init-container env** — `GIT_USERNAME` / `GIT_TOKEN` exposed via `/proc/self/environ`, `kubectl describe`, and any process listing inside the init-clone container. (`podmanager/initclone.go:97-132`) | ka-0000002746 W6 | env-var leakage |
| **F7** | **K6_PERF_BASTION_PRIVATE_KEY referenced in advice but not implemented in `AgentPodSpec`** — operators materialize the key out-of-band per advice ka-0000001600. Same "key referenced but not provisioned" shape as ka-0000006417 (BEADS_TOKEN missing from RWX vault). | ka-0000002746 W2 / ka-0000001794 Cluster 5 | credential rotation / provisioning |
| **F8** | **Gitleaks blanket `_test.go` allowlist** (`.gitleaks.toml:28`) — real secrets in test fixtures will be hidden. Add per-directory / pattern-based granularity and gardenio-specific patterns (e.g. `coop-token-` prefix). | ka-0000002746 W7 | git-history leaks |
| **F9** | **No credential-rotation fire-drill** — rotation only happens after a leak (ka-0000000684). The rolling-restart order `beads → coopmux → controller/agents → bridges → viewers` is undocumented institutional memory. | ka-0000001794 "implicit gaps" | credential rotation |
| **F10** | **`operated_by` propagation gaps on async paths** — sync-HTTP migration is done (ka-0000005772) but NATS messages and slack-bridge webhook re-emits do not carry operator identity end-to-end. ka-0000002887 still open. | ka-0000001794 Cluster 3 | operated_by propagation |
| **F11** | **Beads-viewer cookies missing `Secure: true`** — set by `agents.go:95-102` and `migration_banner.go:103-110`; mitigated by Traefik TLS but not explicit. Trivial fix; high signal. | ka-0000002836 §3 / A | session / cookies |
| **F12** | **Beads-token revocation-list ConfigMap is a single point of failure during rotation overlap** — if the ConfigMap reload races a rotation, in-flight requests with the old token can fail or with the new can fail to validate. | ka-0000002746 W3 | credential rotation |
| **F13** | **Coop credential file mode / tmpfs not hardened** — `$COOP_MUX_STATE_DIR/credentials.json` is readable by the coopmux pod user; should be `0600` + tmpfs-backed. | ka-0000002746 W10 | coop uploads |

### P2 — Structural / roadmap

| ID | Finding | Source | Theme |
|---|---|---|---|
| **F14** | **Replace env-var credential injection with a credential-helper sidecar pattern** (git creds, NATS token, Claude OAuth, etc.). Removes the entire `/proc/self/environ` / `kubectl describe` exposure class. | ka-0000002746 Structural / ka-0000001794 hot spots | env-var leakage |
| **F15** | **Per-credential audit log** — structured read/write events to Mezmo with `credential_id` + `operated_by`, so we can prove who-used-what after the fact. | ka-0000002746 Structural | credential rotation / operated_by |
| **F16** | **Secret-pattern redaction layer on pod stdout/stderr** before Slack / kubectl logs / S3 surfaces. Pairs with F3 (recorder) and F5 (statusreporter). | ka-0000002746 Structural | coop uploads / env-var leakage |
| **F17** | **Centralized credential-masking logging hook** in both Go (`slog` middleware) and Rust (`tracing` layer). Pairs with F16 but at log-emit time rather than pod-stdout time. | ka-0000002836 §6 | env-var leakage |
| **F18** | **Refresh-token rotation for OIDC in beads-viewer** — currently transparent 401→refresh; promoting to explicit refresh-token rotation aligns with industry guidance and surfaces stuck sessions. | ka-0000002836 §6 | session / cookies |
| **F19** | **HashiCorp Vault integration for token fetch** — optional path away from K8s-Secret + ExternalSecret for the highest-impact creds (Anthropic, GitLab PAT, Slack signing). | ka-0000002836 §6 | credential rotation |
| **F20** | **mTLS controller↔coopmux for credential push** — currently bearer-only; mTLS adds a second factor on the highest-value channel in the stack. | ka-0000002746 W9 / Short-term #5 | coop uploads |
| **F21** | **Enforce `externalSecrets.enabled=true` for prod via values schema validation; remove raw K8s Secret fallbacks** for Slack / JIRA-webhook / NATS / Claude-OAuth / autodestruct. | ka-0000002746 Structural #4 | credential rotation |
| **F22** | **Credential usage analytics** — in-flight / throttled / over-budget per account, surfaced in beads-viewer. Detects abnormal usage that today only shows up as a leaked-key incident. | ka-0000002746 Structural #5 | credential rotation |

---

## 3. Findings Grouped by Theme

This view is for the operator picking a workstream owner. F-IDs match §2.

### 3.1 Env-var leakage (F1, F2, F5, F6, F14, F15, F16, F17)
The largest cluster. Producers (controller, init-clone) and consumers
(`buildCoopEnv`, recorder, statusreporter) treat env vars as a normal IPC
channel, which means anything that reads env, dumps env, or proxies env can
exfiltrate. The structural fix is F14 (credential-helper sidecar) + F16/F17
(redaction at output boundary).

### 3.2 Git-history leaks (F4, F8)
F4 is the unfinished work from ka-0000000684: rotation happened, history
rewrite did not. F8 is the prevention: a too-broad allowlist that would have
hidden F4 if the leak had been in a `_test.go` file.

### 3.3 Coop uploads (F3, F13, F16, F20)
Terminal recordings + on-disk credential storage. F3 is the showcase incident
(ka-0000003272); F13 hardens the file path; F16 covers the upload path; F20
hardens the producer side.

### 3.4 Credential rotation (F7, F9, F12, F15, F19, F21, F22)
The umbrella of "we react to leaks, we don't drill rotation." F9 is the
human-process piece (fire-drill); F12 is the technical-debt piece (revocation
list SPoF); F21 closes the chart-fallback foot-gun.

### 3.5 operated_by propagation (F10)
A single P1 in this synthesis but it owns its own existing open epics
(ka-0000006809 — "operated_by mandatory", ka-0000002887 — "resolve operated_by
on every write path"). The work *exists*; the synthesis claim is that those
epics should be explicitly cross-linked under this epic and not allowed to
stall in isolation.

### 3.6 Session / cookies / OIDC (F11, F18)
Beads-viewer is the strongest baseline — tokens never logged, in-memory only,
TLS everywhere, solid OIDC integration. F11 closes the trivial `Secure` flag
gap; F18 modernizes the refresh model.

---

## 4. Proposed Improvement-Task Breakdown

Each entry below is a candidate child task under ka-0000001193. The gardener
should ratify, optionally bundle, and dispatch. Acceptance criteria are
written so each task is independently testable.

### T1 — Fail-fast on missing `COOP_CREDENTIAL_KEY` (covers F1)
**Scope:** `coop/crates/mux/src/credential/persist.rs` — replace the
plaintext-fallback `warn` branch with a `panic!` / `exit(2)` at coopmux
startup unless a `COOP_ALLOW_PLAINTEXT=1` dev escape hatch is set; emit a
loud structured log and a `coopmux_credential_plaintext_total` metric so we
can alert. Update helm chart to wire `COOP_CREDENTIAL_KEY` via
ExternalSecret in prod; document the dev escape hatch in `coop/CLAUDE.md`.
**Acceptance:**
- coopmux refuses to start in prod without the env var.
- Existing plaintext files auto-migrate to encrypted on first successful
  read (already implemented per `persist.rs`); add a test for the migration.
- Helm chart `helm lint` passes; values schema rejects prod values with no
  `COOP_CREDENTIAL_KEY` ExternalSecret reference.
- Metric exposed; alert wired to the gardenio-rwx Mezmo log monitor.

### T2 — Allowlist (not denylist) the coop child env (covers F2)
**Scope:** `gardenio/controller/cmd/gb/init_agent_loop.go::buildCoopEnv` —
replace the `append(os.Environ(), …)` with an explicit allowlist of env
vars (e.g. `PATH`, `HOME`, `USER`, `LANG`, `TERM`, plus the specific
`COOP_*` / `BEADS_DAEMON_TOKEN` keys the child needs). Add a regression
test that asserts `BD_DAEMON_TOKEN` and `CLAUDE_CODE_*` do **not** appear in
the child env unless explicitly whitelisted.
**Acceptance:**
- Unit test asserts the leak vector is closed.
- E2E `agent-lifecycle` module still green.
- Doc update in `gardenio/CLAUDE.md` or `gardenio/docs/secrets/` noting the
  allowlist policy.

### T3 — Redact terminal recordings before S3 upload (covers F3)
**Scope:** `coop/crates/cli/src/record.rs` + `s3.rs` — add a streaming
redaction filter applied to each frame before it lands on disk / in the
upload pipeline. Use the same pattern set as F16/F17 (credit-card-ish,
JWT-ish, `AKIA…`, `sk-ant-…`, `xoxb-…`, `glpat-…`, `xoxe.xoxp-…`, etc.).
Add a `--no-redact` opt-out for local debugging only.
**Acceptance:**
- New crate-level test: feeding a known secret pattern into a recorded
  session yields a redacted frame.
- Recordings already in S3 are *not* migrated by this task (file follow-on:
  T13 history-rewrite of S3 bucket).
- Telemetry: `coop_recorder_redactions_total{pattern=…}` counter.

### T4 — Rewrite git history to purge leaked coopmux bearer (covers F4)
**Scope:** Coordinate with operator (gb decision) for downtime window.
Use `git filter-repo` (preferred) or BFG to remove the leaked token from
`helm/values-gardenio.yaml` and `.rwx/deploy-values/gardenio-rwx.yaml`
across all branches. Force-push to GitLab; coordinate fresh clones.
**Acceptance:**
- `git log --all -p -S <leaked-token-prefix>` returns no hits.
- All open MRs are rebased or noted as needing rebase.
- Runbook captured at `gardenio/docs/secrets/git-history-rewrite.md`.
- This task **requires** operator authorization via `gb decision` before
  any force-push happens.

### T5 — Generic secret-redaction layer for bead writes (covers F5)
**Scope:** kbeads server — add a redaction interceptor between the gRPC
handler and the DB write for `notes`, `description`, and `comment` fields.
Same pattern catalog as T3. Configurable via `BEADS_REDACTION_PATTERNS`
env / config; defaults shipped in code.
**Acceptance:**
- Unit tests cover each baked-in pattern.
- Existing beads are **not** mutated.
- New beads containing a matched pattern have the match replaced with
  `[REDACTED:<pattern-name>]` and a structured log line emitted with the
  bead ID + pattern (no value).
- statusreporter regression test confirms the original ka-0000000804
  scenario now writes a redacted note.

### T6 — Move git creds from env to git credential helper (covers F6)
**Scope:** `gardenio/controller/internal/podmanager/initclone.go:97-132` —
mount the git credentials as a file (already a Secret) and configure
`git config credential.helper 'store --file=/path'` instead of passing
`GIT_USERNAME` / `GIT_TOKEN` as env vars to the init-container.
**Acceptance:**
- `kubectl describe pod <agent-pod>` shows no `GIT_*` env vars on the
  init-container.
- Init-clone E2E module green.
- Same approach documented for adoption by other consumers in a follow-on
  task under F14.

### T7 — Implement `K6_PERF_BASTION_PRIVATE_KEY` in `AgentPodSpec` (covers F7)
**Scope:** Add a typed `PerfBastionSSHKeySecret *corev1.SecretKeySelector`
field to `AgentPodSpec`; podmanager mounts it at `/home/agent/.ssh/perf-bastion`
with mode `0600`; remove the out-of-band materialization step from advice
ka-0000001600. Update the advice once shipped.
**Acceptance:**
- Helm values support `perfBastionSSHKeySecret.name/key`.
- Agent pod has the file at the expected path.
- Existing manual workflow in ka-0000001600 still works during transition.
- Advice ka-0000001600 updated or superseded.

### T8 — Narrow the gitleaks allowlist (covers F8)
**Scope:** `.gitleaks.toml` — replace blanket `.*_test\.go` with
per-directory `paths` clauses and per-pattern allow rules. Add gardenio-
specific patterns (`coop-token-` prefix, beads-daemon token shape).
Optionally add a pre-push hook (ka-0000002821 already shipped pre-commit).
**Acceptance:**
- `gitleaks detect --config .gitleaks.toml` against a synthetic fixture
  with a real-looking secret in `*_test.go` fails.
- CI gitleaks job remains green on `main`.

### T9 — Credential-rotation fire-drill runbook + scheduled exercise (covers F9)
**Scope:** New doc `gardenio/docs/secrets/rotation-fire-drill.md` capturing:
the rolling-restart order (beads → coopmux → controller/agents → bridges
→ viewers), per-credential AWS SM rotation procedure, expected SLO. Schedule
a quarterly drill via a gardener bead (`kd create … --type=schedule`).
**Acceptance:**
- Runbook merged.
- Schedule bead created and enabled.
- First drill executed within 90d of merge; result captured as a closed
  task bead with timing observations.

### T10 — Eliminate beads-token revocation-list SPoF (covers F12)
**Scope:** Replace the single revocation-list ConfigMap with a versioned,
hot-reloaded list (e.g. backed by NATS KV or the existing Postgres) so a
ConfigMap reload race does not strand requests. Alternative: dual-write the
list and serve from both for the overlap window.
**Acceptance:**
- Rotation E2E module (ka-0000004808 family) covers the overlap window.
- No request fails during a simulated mid-rotation reload.
- Doc update on rotation flow.

### T11 — Set `Secure: true` on beads-viewer cookies (covers F11)
**Scope:** `gardenio/controller/cmd/beads-viewer/agents.go:95-102` and
`migration_banner.go:103-110` — set `Secure: r.TLS != nil || isDevMode()`.
Also remove the redundant `BEADS_DAEMON_TOKEN` storage at `main.go:65`
(audit found it stored twice) and document the cookie posture in helm
chart values.
**Acceptance:**
- Cookies inspected from a real beads-viewer instance show `Secure`.
- `BEADS_DAEMON_TOKEN` is held in exactly one location in memory.
- Helm chart values doc references cookie posture section.

### T12 — Harden coop credential file mode + tmpfs (covers F13, F20)
**Scope:** Mount `$COOP_MUX_STATE_DIR` as `emptyDir.medium=Memory`
(tmpfs), set the credentials file to `0600`, and require mTLS on the
controller↔coopmux push (server cert from existing cluster issuer; bearer
remains as second factor).
**Acceptance:**
- Pod spec inspection shows tmpfs.
- File mode confirmed in chart test.
- mTLS test added (cert-rotation drill bonus content).

### T13 — operated_by on async paths (covers F10) — *cross-link*
**Scope:** **Coordinate** rather than re-do — the existing open epics
ka-0000006809 and ka-0000002887 own this work. The proposed task is to:
(a) link those epics as children of ka-0000001193, (b) write the
acceptance test that asserts NATS messages and slack-bridge re-emits carry
`operated_by` end-to-end, (c) close the gap left in ka-0000001794 Cluster 3.
**Acceptance:**
- Epic cross-links recorded.
- New E2E test under `gardenio/tests/e2e/scripts/test-operated-by-async.sh`
  passes against gardenio-rwx.

### T14 — Structural: credential-helper sidecar (covers F14, P2) — *new epic*
**Scope:** Too large for a task; the synthesis proposes spinning a child
**epic** with its own design doc. Target consumers in priority order: git,
NATS, Claude OAuth, GitLab PAT, Slack signing. The sidecar exposes a
Unix-socket API and reads creds via projected SA token / ExternalSecret.
**Acceptance (of the spin):**
- New epic created under ka-0000001193.
- Design doc at `gardenio/docs/secrets/credential-helper-sidecar.md`.
- First consumer (git) prototyped on a feature branch.

### T15 — Structural: per-credential audit log (covers F15, P2)
**Scope:** Add `credential_event` structured log emissions at every
read/write of a managed credential: controller, coopmux broker, beads
daemon. Fields: `credential_id`, `actor`, `operated_by`, `op` ∈ {read, set,
revoke}, `result`. Ship to Mezmo; build a beads-viewer panel.
**Acceptance:**
- Mezmo `app:coopmux credential_event:*` returns events.
- Beads-viewer credential-events panel renders.

### T16 — Structural: enforce `externalSecrets.enabled=true` in prod (covers F21)
**Scope:** `gardenio/helm/gardenio/values.schema.json` — add a conditional
that rejects prod (`global.environment=prod`) renders without
`externalSecrets.enabled=true` for: Slack, JIRA-webhook, NATS, Claude-OAuth,
autodestruct. Remove the raw K8s Secret fallbacks from those template
files after a one-release deprecation window.
**Acceptance:**
- `helm lint` rejects prod values with the fallback path.
- Dev / RWX values continue to render.

---

## 5. Cross-Links to Existing Open Epics

These epics are already tracked and should be parented under ka-0000001193
or formally cross-referenced:

- [ka-0000006809](https://beads-viewer.e2e.example.com/bead/ka-0000006809) — operated_by mandatory
- [ka-0000006932](https://beads-viewer.e2e.example.com/bead/ka-0000006932) — gb-daemon threat model
- [ka-0000004678](https://beads-viewer.e2e.example.com/bead/ka-0000004678) — permissions model
- [ka-0000002887](https://beads-viewer.e2e.example.com/bead/ka-0000002887) — resolve operated_by on every write path
- [ka-0000001739](https://beads-viewer.e2e.example.com/bead/ka-0000001739) — canonical IRSA usage (advice)
- [ka-0000007098](https://beads-viewer.e2e.example.com/bead/ka-0000007098) — provision long-lived AWS creds (tension with the above)
- [ka-0000000157](https://beads-viewer.e2e.example.com/bead/ka-0000000157) — rotate-out legacy AKIA
- [ka-0000006417](https://beads-viewer.e2e.example.com/bead/ka-0000006417) / [ka-0000005501](https://beads-viewer.e2e.example.com/bead/ka-0000005501) — BEADS_TOKEN missing from RWX vault (same shape as F7)

## 6. Out of Scope (for now)

The audits surfaced these but the synthesis explicitly defers them:

- **Telemetry/metrics proving secrets never appear in logs/exports** — gitleaks
  scans disk, not Mezmo streams. Reasonable but follow-on to F16/F17.
- **Slack/JIRA/GitLab PAT rotation cadence audit** — ka-0000007475 flagged
  "Manual"; the right answer is the fire-drill (T9) plus a quarterly
  cadence review. Not a standalone task in this breakdown.
- **"AWS SM unreachable" failure-mode test of ExternalSecret 1h refresh** —
  worthwhile but operational, not credential-leak-shaped.
- **Gardener-e2e role-loadout sibling leak** (ka-0000001489) — already closed;
  the pattern (advice = capability boundary) is interesting but is a kbeads
  permissions concern, not a secrets concern, and lives better under
  ka-0000004678.

---

## 7. Suggested Sequencing

If the operator dispatches one task per week, in priority order:

1. T1, T2, T3 (parallel, all P0 leak-stop)
2. T5 (P0, depends on pattern catalog from T3)
3. T11 (P1, trivial; can be done by any crew in <1d)
4. T4 (P0, needs operator authorization and a maintenance window)
5. T6, T7, T8 (P1, parallel)
6. T9 (P1, doc + schedule)
7. T10, T12 (P1, infra)
8. T13 (P1, cross-link only)
9. T14, T15, T16 (P2, schedule as epics)

T4 is sequenced after T1–T3 / T5 deliberately: by the time we do a
maintenance window for history rewrite, the new redaction layer ensures
the next leak (if any) is caught before it lands in history.

---

## Appendix: Audit Source Map

| Audit | Bead | Coverage |
|---|---|---|
| beads-viewer | ka-0000002836 | session cookies, OIDC, env vars, logging masking, CSRF posture |
| kbeads + gardenio | ka-0000002746 | controller, slack/jira/gitlab bridges, agent images, helm, coop credential manager |
| Pattern analysis | ka-0000001794 | 7 clusters across ~200 beads + 4mo git log; hot-spot files; implicit gaps |
