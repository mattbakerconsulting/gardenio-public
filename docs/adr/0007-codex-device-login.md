# ADR 0007 — Codex ChatGPT-plan login without the manual mint

> Status: **Accepted: option A, with D as stopgap/fallback** (operator decision gd-SFUfHizFdK). Built in this PR; see §9 for what landed and what is still open.
> Bead: gd-CKJxLse4Mx. Parent: gd-J9Wkw5HjF6 (Codex rollout). Extends [ADR 0004](0004-codex-harness.md) §5.
> Baseline: the credential contract on gd-3gTWiNEhg1 and the keeper runbook [`gardenio/docs/codex-subscription-keeper.md`](../../gardenio/docs/codex-subscription-keeper.md).
> The first login was minted by an operator on a laptop (escalation gd-6MspYn8R0l).

Evidence tags as in ADR 0004: **[PROBE]** observed in this task, in a live agent pod with `codex-cli 0.162.1`. **[BIN]** read from the pinned binary's strings, never executed. **[REPO]** read in this repo. **[UNVERIFIED]** inference; §6 names the step that closes it.

## 1. Problem

Two situations need a ChatGPT-plan `auth.json` to land in `gardenio-codex-auth`:

1. **Bootstrap** of a login (the first one, or an additional `gardenio-codex-auth-<name>`).
2. **Re-login** after the keeper sets `probe_auth_status=rejected` (refresh token reused, expired or revoked) and files its re-login escalation.

Both are a laptop step today: run `codex login --device-auth` with a throwaway `CODEX_HOME`, approve in a browser, store the file with `--from-file` without displaying it. The goal is to make the browser approval the only human step, and to keep every ADR 0004 §5 property: the refresh token never reaches a general agent pod, the keeper stays the single writer, `auth.json` is never printed, logged or committed.

## 2. What the probe showed

- **[PROBE]** `codex login --device-auth` runs headless in an agent pod with no TTY, no browser, a throwaway `CODEX_HOME` and no `auth.json` anywhere. It prints the verification URL (`https://auth.openai.com/codex/device`) and a one-time code with a **15 minute** expiry to stdout, then blocks polling. Killed before approval, it wrote nothing under `CODEX_HOME` except `log/` and `tmp/`. So the browser half is entirely separable from the process: any human can complete it from any device, and the process needs only outbound HTTPS.
- **[BIN]** The flow is implemented in `login/src/device_code_auth.rs` against `/deviceauth/usercode` (request a code), `/deviceauth/token` (poll) and `/deviceauth/callback`, then the same `auth.openai.com/oauth/token` the keeper already speaks, with the same public client id (`codexauth.ClientID`). The poll handles `authorization_pending`-style waiting and `slow_down`. Request and response bodies are not read from the binary (§6 R-A).
- **[BIN]** `codex login` also has `--with-access-token`, and app-server has `account/login/start`. Neither mints a refresh-token-bearing login; they consume one. They do not help here.
- **[REPO]** The controller already has everything on the receiving end: `internal/codexauth` (OAuth client, JWT claim parsing, `Parse`), the keeper's Secret CAS write path with `resourceVersion`, name-scoped `update` on `gardenio-codex-auth*` bound to the controller ServiceAccount only, an operator escalation filed on rejection, and rejected-fingerprint logic that clears the rejection by itself when a different refresh token appears.

Consequence: running the login inside a pod is technically fine, which is exactly why the constraint must come from policy, not capability.

## 3. Options

| | Where the device flow runs | Where the document lands | Verdict |
|---|---|---|---|
| A | **The leader controller** speaks the device flow itself (`codexauth` grows `DeviceLogin`). | Straight into `gardenio-codex-auth` through the keeper's existing CAS write. No file, no pod. | **Recommended.** |
| B | A short-lived login pod/Job running the real `codex login --device-auth`. | The file starts in a pod filesystem; getting it out needs Secret-write RBAC for that pod, or stdout capture by something that can read pod logs. | Rejected: the refresh token lives in a pod, and the exfiltration path (RBAC or logs) is the weakest link in the design. |
| C | An agent in its own pod runs `codex login --device-auth` and relays the URL and code. | Same as B, in a general agent pod. | Rejected: violates ADR 0004 §5 ("never reaches an agent pod"), and puts a live device code, which phishes the owner into granting their plan, into bead data any agent can read. |
| D | Keep the laptop flow; ship a `gb codex login` helper that wraps the throwaway home + `--from-file` steps. | Same as today, fewer typos. | **Keep as the fallback** and the stopgap while A is built. It is also what bootstraps an additional account (§4.6). |

Why A over B: it needs no new RBAC, no new pod type, no shared file; the only new surface is outbound HTTPS from the controller to `auth.openai.com`, which the keeper already uses. Cost of A: the controller reimplements an undocumented-to-us protocol that the pinned binary otherwise owns. That risk is bounded by R-A (§6) and by D remaining available.

## 4. Proposed design (option A)

### 4.1 Flow

1. **Trigger (explicit operator act).** The keeper's re-login escalation (and a bootstrap request, §4.6) carries `operation_spec.action = "codex_device_login"` with `account`, `secret` and the rejected fingerprint. The operator presses **Start device login** on that escalation (Slack card button or `gb escalation approve`). Nothing starts the flow automatically: a code is only requested when a human is ready to use it inside the 15 minute window.
2. **Request.** The controller (leader only) calls `/deviceauth/usercode`, holds the `device_auth_id` and the poll interval **in memory only**, and returns `{verification_url, user_code, expires_at}` to the *operator-authenticated* surface that pressed the button (the escalation card for the operator, a private operator message). It is never written to a bead field, comment or log line, and never sent to an agent.
3. **Approve.** The operator opens the URL in a browser signed in as the plan owner, enters the code, approves. This is the explicit operator action the brief requires.
4. **Poll and exchange.** The controller polls `/deviceauth/token` at the server's interval (honouring `slow_down`), stops at expiry, then does the code exchange and receives `id_token`, `access_token`, `refresh_token`.
5. **Validate before writing.** Parse the id token. Require `chatgpt_account_id` to equal the existing Secret's `account_id` unless the escalation says `allow_account_change: true` (so a wrong browser profile cannot silently swap the cluster onto another person's plan). Reject a missing `refresh_token`.
6. **Persist (keeper stays the single writer).** The login runs under the keeper's per-account mutex and writes with `resourceVersion` CAS to `gardenio-codex-auth`, then the keeper derives `-session` and pushes coopmux exactly as after any rotation. It writes only when the Secret's current refresh fingerprint still equals the `rejected_fingerprint` in the escalation (or the Secret is empty/missing for a bootstrap), so a concurrent re-login by another route is never overwritten. The next keeper pass sees a different fingerprint and clears `probe_auth_status` itself, unchanged.
7. **Close.** The controller records only non-secret facts on the escalation (account, HTTP outcome class, new fingerprint, `last_refresh`), closes it as executed, and the bead that was waiting resumes by the existing close-on-execute path.

### 4.2 Security properties

- **No refresh token outside the controller and the Secret.** Not in a pod, a log, an error string, a bead, a comment, a squawk or an escalation field. Same hygiene rules as `codex_keeper.go`. The document is held in memory between exchange and persist; on persist failure it is dropped and the flow is simply run again (a fresh login is cheap, unlike a mid-chain rotation).
- **Phishing resistance.** The code is shown only on an operator-authenticated surface and is bound to a flow the controller started on a human's explicit button press. No agent can start a flow or read a code (the endpoint is operator-authorised and not routed to agent pods). An attacker who sees a code gains nothing by itself (tokens go to the controller's poll, not to them); the realistic abuse is social engineering the owner into approving someone else's code, which is why the card tells the owner to approve only a code they just requested here.
- **One flow per account at a time**, in memory, with a TTL equal to the code's expiry. A controller restart or lost lease drops it; the operator presses the button again.
- **Scope.** The login is the cluster-dedicated one for that Secret. The design never reads a laptop `~/.codex/auth.json` and never pulls one in (a shared refresh-token family would make the cluster and the laptop knock each other out; ADR 0004 §5).
- **Audit.** The escalation is the paper trail: who pressed start, when, the account, the outcome and the new fingerprint.

### 4.3 Re-login for `probe_auth_status=rejected`

The existing escalation `Codex login rejected, re-login needed: <account>` becomes the surface for 4.1 step 1. Its prose changes from "run this on a laptop" to "press Start device login, then approve at the URL"; the laptop procedure (D) stays in the text as the fallback. No change to the keeper's rejection or fingerprint logic.

### 4.4 Interfaces to build

- `codexauth.DeviceLogin` (request, poll with backoff and `slow_down`, exchange) with an injectable base URL for hermetic tests, same allowlist discipline as `SafeOverrideURL`.
- A controller-side `codexLoginBroker` sharing the keeper's per-account lock and Secret writer; one in-flight flow per account.
- An operator-only endpoint (and the Slack action that calls it) to start a flow and to return the URL and code to that caller. Wired through the escalation executor path rather than a new public route if the escalation machinery can carry the response (§6 R-C).
- `gb codex login --account <name>` (option D) as the laptop helper and fallback: throwaway `CODEX_HOME`, `codex login --device-auth`, `--from-file` into the Secret without printing it, remove the throwaway home.

### 4.5 RBAC

None new for re-login: the controller already holds name-scoped `update` on `gardenio-codex-auth*` and the keeper already refuses to act without it. Any chart change is a privileged-lane `helm_values_change` escalation (advice gd-3wArIbnnWn), not part of this PR.

### 4.6 Bootstrap of an additional login

`resourceNames` cannot scope `create`, and the keeper deliberately has no `create`. So a *new* `gardenio-codex-auth-<name>` still needs the Secret to exist (empty `auth.json` key) before the broker can fill it. Either the chart pre-creates empty Secrets from `agents.codexAuth.secretNames` (a helm change, escalation), or bootstrap stays on option D. Re-login of an existing login, the common recurring case, needs neither.

## 5. Why not build on the Codex process in the pod

Technically it works [PROBE]; the cost is the whole point of ADR 0004 §5. A login performed by `codex` in a pod hands the long-lived refresh token to a filesystem that runs model-driven code, and the only ways to get it into the Secret are to grant that pod Secret-write or to scrape it from logs. Option A makes the controller the only place the document ever exists, which is also where the keeper already is.

## 6. Open risks and what closes each

| # | Risk | Closed by |
|---|---|---|
| R-A | Request and response bodies of `/deviceauth/usercode`, `/deviceauth/token` and the final exchange (is a `code_verifier` returned by the poll; field names; error codes; poll interval) are not in this note. Only the paths and the existence of the flow are observed. | Build step 1: implement `DeviceLogin` against a local mock matching the binary's behaviour, then run it **once against real `auth.openai.com` into a throwaway Secret name**, operator approving, before it ever touches `gardenio-codex-auth`. If the real protocol resists, fall back to D and stop. |
| R-B | Whether OpenAI treats a controller-originated device flow differently from the CLI's (client id, user-agent, `originator` header). It uses the CLI's public client id; if it is rejected, that is a hard stop for A. | Same real run as R-A. |
| R-C | Whether the escalation/Slack machinery can carry an operator-only response (the URL and code) back to the pressing human without writing it to a bead. | Build step 2: read `bridge/escalations.go` and the Slack interaction path; if it cannot, return the code through an ephemeral Slack response or an operator-authenticated `gb` command and leave the escalation holding only status. |
| R-D | A login for a different plan/account than the existing Secret. | The `account_id` check in 4.1 step 5. |
| R-E | Controller loses the lease mid-flow. | The flow is in memory and short-lived; the operator re-presses start. Nothing was written. |

## 7. Order of work if agreed

1. This note merged.
2. `gb codex login` helper (option D): small, useful immediately, no design risk.
3. `codexauth.DeviceLogin` plus mock tests, then the single real throwaway run (R-A, R-B).
4. Broker, operator surface and escalation wording (R-C), tests, runbook update to `codex-subscription-keeper.md`.
5. Optional: chart pre-creation of empty Secrets for additional logins (§4.6), by escalation.

## 8. What the operator may overturn

- **A vs D only.** If the operator prefers not to have the controller own the protocol, D alone removes the typing but not the laptop; that is the whole delta.
- **Button press vs. automatic start on rejection.** Recommended: button press, because the code is live for 15 minutes and anyone can be phished with it. Automatic start would only be safe if the code stayed invisible until the operator acknowledged it, which is the same button.
- **Account-change guard** (`allow_account_change`) defaults to off.

## 9. Implementation status

Landed (bead gd-CKJxLse4Mx):

| Piece | Where |
|---|---|
| Device-flow client (`Start`, `Poll` with `slow_down`/429 back-off, code exchange, `NewAuth`, `ChatGPTAccountID`), hermetic mock tests | `internal/codexauth/devicelogin.go` |
| Wire shapes were checked against the pinned binary's strings (`UserCodeResp{device_auth_id,user_code,interval}`, `/api/accounts/deviceauth/{usercode,token}`, `/deviceauth/callback`, `/codex/device`); the request bodies are the usual OAuth device shapes and are confirmed only by the real run (R-A) | same |
| Controller broker: one flow per account, in-memory device handle, `account_id` guard, fingerprint precondition, resourceVersion-CAS write, gate-held persist | `cmd/controller/codex_login_broker.go` |
| Routes `POST /api/v1/codex/device-login`, `GET .../status` on :8091: **strict** bearer (`CONTROLLER_API_TOKEN` must be set, unlike the opt-in routes), leader-only (non-leader answers 503) | same |
| Keeper's re-login escalation now carries `operation_spec.action=codex_device_login` and says "press Start device login" with D as fallback | `cmd/controller/codex_keeper.go` |
| Slack **Start device login** button on that escalation; gated on the `approve:escalation` capability; the code returns as an **ephemeral reply to the presser only** (`response_url`), never a bead field, comment or log line | `internal/bridge/bot_codex_login.go` |
| `gb codex login` (option D laptop helper) and `gb codex device-probe` (one real run, result discarded) | `cmd/gb/codex_login.go`, `codex_device_probe.go` |

R-C is resolved by the Slack path: `postInteractionEphemeral` already exists, so the escalation card is the start surface and the code never travels through the escalation machinery. Authorisation is the same capability the web UI uses for approvals. No new RBAC or helm values are needed: the bridge and controller already share `CONTROLLER_API_TOKEN` in production.

Behaviour worth knowing:

- The broker refuses to replace a login the keeper has not found rejected (409), so a stray press cannot clobber a working login. It also refuses a Secret that does not exist (§4.6).
- On success the broker comments the account, Secret and refresh **fingerprint** on the escalation, closes it with `result_status=success`, and the keeper clears `probe_auth_status` on its next pass.
- The issuer can be overridden with `CODEX_AUTH_ISSUER_OVERRIDE` for hermetic tests (https and `auth.openai.com` only, like the other overrides).

Still open:

1. **R-A/R-B real run.** Run `gb codex device-probe` once on a laptop, approving as the plan owner. It prints only non-secret facts. If it fails, A is a hard stop and D stays the path. The broker should not be relied on before this passes.
2. An operator-visible surface outside Slack (beads-viewer button) is not built; the `gb codex login` fallback covers a Slack outage.
3. Bootstrap of an *additional* login still needs the empty Secret to exist (§4.6): a chart change by escalation, or D.
