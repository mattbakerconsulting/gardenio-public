<!-- Bead: ka-0000004533 (R4 de-risking) · Epic: ka-0000006064 (claudemore) · Operator: Matt Baker -->
<!-- Author: gardenio-r4-derisking-cost-n8u · Adversarially-verified workflow synthesis · 2026-06-14 -->

# claudemore R4 — De-risking: Risk Register + Go/No-Go

## 0. Executive summary

Real Claude Code in e2e (claudemore) is **de-riskable to a yellow GO-WITH-GUARDRAILS** — every dominant risk has a feasible mitigation grounded in code that already exists in this repo, and no risk is a hard blocker. The two dominant risks are **(1) unbounded model spend** — because litellm per-key budget enforcement is documented but *not wired* into the controller (`MaxBudget` is verbatim an "informational" hint, no `/key/generate` write path exists anywhere in gardenio/kbeads/coop), and **(2) non-determinism/flake** layered onto a harness already strained by a 15-min shard wall and a documented 5.5h golden stall. The dominant mitigations are, respectively, **wiring a per-CI scoped litellm virtual key with `max_budget`** (one `/key/update` write reusing existing read-path fields) and a **record/replay cassette architecture** (the coop S3 transcript substrate already exists) that restores real reasoning while keeping CI at claudeless-grade determinism. Critically, claudemore must **augment, never replace** claudeless: claudeless owns the deterministic CLI/hook/bead *contract*; claudemore owns *reasoning/integration* — and must never become a required per-PR gate. The single correction that reshapes the secrets story: production already authenticates agents with a **real Anthropic OAuth token** (`CLAUDE_CODE_OAUTH_TOKEN`) forwarded upstream for billing — the litellm virtual key is a spend-tracking sidecar, not a substitute — so the de-risk is about **scoping, capping, revoking, and redacting** a real model credential, not avoiding one.

## 1. Risk register

| ID | Risk | Dimension | Likelihood | Impact | Inherent severity | Mitigation(s) | Residual severity | Owner-hint |
|---|---|---|---|---|---|---|---|---|
| R4-COST-1 | Unbounded per-run spend; no per-key litellm budget enforcement wired (`MaxBudget` informational-only, no `/key/generate`/`/key/update` write path) | Cost | H | H | 🔴 | Wire ephemeral per-CI virtual key minted with `max_budget`+`budget_duration` (reuse read-path fields `usage.go:41-42`); litellm 429s key at cap | 🟡 | controller |
| R4-COST-2 | No prompt cache → repeated e2e system prompts pay full input price every run (`litellm.extraSettings: {}`, `values.yaml:6176`) | Cost | M | M | 🟡 | Add cache TTL via `extraSettings` merge point; stable harness prompt becomes cache-read | 🟢 | infra/litellm |
| R4-COST-3 | Opus-tier reasoning per agent-session is expensive (~$3–$12/agent est.) | Cost | M | M | 🟡 | Model-tier e2e agents to Haiku 4.5 (already in `model_list`, config-only) → ~$0.20–$0.80/agent | 🟢 | infra/litellm |
| R4-COST-4 | Full suite spawns many agents → high per-run burn if real-CC runs everywhere | Cost | M | H | 🟡 | Scenario sampling: claudeless for bulk, real-CC whitelisted (sampled scheduled only) | 🟢 | e2e harness |
| R4-COST-5 | Non-terminating agent burns budget up to 15-min shard wall (timeout caps wall-clock, not cost) | Cost | M | M | 🟡 | Bound agent turn-count (max-turns kill) in harness; litellm 429 backstop | 🟢 | e2e harness |
| R4-DET-1 | Output-text non-determinism — assertions on `say`/transcript substrings flake | Determinism | H | M | 🟡 | Assert only on outcomes/side-effects (mirror `$GB_SCENARIO_RESULT_FILE` pattern); never on model text | 🟢 | e2e harness |
| R4-DET-2 | Tool-choice/ordering non-determinism — agent may squawk before claim, skip/batch steps | Determinism | H | M | 🟡 | Outcome-only assertions + record/replay cassette | 🟢 | e2e harness |
| R4-DET-3 | Latency variance vs 15-min shard wall + 180s per-scenario cap (`test-claudeless.sh`) | Determinism | H | H | 🔴 | Record/replay (instant in CI); live runs scheduled-only with longer-timeout dedicated shard | 🟡 | e2e harness |
| R4-DET-4 | Model-version drift — no opus-4-8 default; `claudeModel: ""` → Claude Code's built-in default (Sonnet), Anthropic can change silently | Determinism | M | H | 🔴 | **Load-bearing:** explicit per-account model pin in e2e account bead (`fields.model`→`CLAUDE_MODEL`, `resolve-auth.sh:122`); scheduled live canary as drift tripwire | 🟡 | e2e/platform |
| R4-DET-5 | Stochastic flake amplifies an already-flaky harness (5.5h golden stall ka-0000005791; 1/102 race ka-0000004111) | Determinism | M | H | 🔴 | Record/replay gates CI; live canary non-gating; per-module `flaky-attempts:2`, don't raise global `flaky-attempts:3` | 🟡 | e2e harness |
| R4-DET-6 | Non-reproducible live failures (exact tokens gone) | Determinism | M | M | 🟡 | Cassette = byte-for-byte reproducible; live canary degrades to noise (non-gating) | 🟢 | e2e harness |
| R4-SEC-1 | First real model credential (OAuth token) in CI trust boundary; can spend against Anthropic | Secrets | M | H | 🔴 | Scope to short-lived budget-capped path; ephemeral-namespace containment; prefer revocable virtual key for tracking, treat OAuth token as the sensitive auth cred | 🟡 | platform/security |
| R4-SEC-2 | F2: `buildCoopEnv` starts from `os.Environ()` wholesale; only `CLAUDE_CODE_*` filtered — model cred + `BD_DAEMON_TOKEN`/AWS inherit into coop child (`init_agent_loop.go:565`) | Secrets | M | H | 🔴 | Switch `buildCoopEnv` to an explicit allowlist (synthesis T2) | 🟡 | controller |
| R4-SEC-3 | F4: real reasoning agent can echo a key into S3 terminal recordings (`record.rs`/`s3.rs`); inert under claudeless | Secrets | M | H | 🔴 | Redaction layer F16/F17 on recordings (patterns: `sk-ant-`, litellm `sk-`/`vk-`) | 🟡 | coop |
| R4-SEC-4 | F5: agent writes a key into durable bead notes (ka-0000000804 pattern); inert under claudeless | Secrets | L | H | 🟡 | Redaction layer F17 on bead writes | 🟡 | controller/kbeads |
| R4-SEC-5 | F7: RWX-vault referenced-but-not-provisioned → empty string, silent until 401/403 | Secrets | M | M | 🟡 | Empty-string guard at dispatch mirroring `.rwx/e2e.yml` POC-cred check; fail loud | 🟢 | e2e pipeline |
| R4-SEC-6 | F1: coop plaintext credential persistence (fails **closed** by default; opt-in `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1`) | Secrets | L | M | 🟡 | Assert `COOP_ALLOW_PLAINTEXT_BOOTSTRAP` is NOT set in CI namespace (verify-a-flag, not build-a-guard) | 🟢 | e2e/helm |
| R4-ISO-1 | AWS reachable via IRSA with prod-named role `gardenio-agents-s3` on shared bucket; prefix isolation best-effort, not IAM-enforced (`values.yaml:2059-2060`); IRSA web-identity bypasses IMDS egress block | Isolation | M | H | 🔴 | Swap to dedicated low-priv CI role scoped to instance prefix, or drop `agents.s3.bucket` for claudemore runs | 🟡 | platform/IAM |
| R4-ISO-2 | Broad egress `0.0.0.0/0:443,80` (RFC1918/IMDS/CGNAT excepted) — agent reaches arbitrary internet/APIs | Isolation | M | H | 🔴 | claudemore-specific NetworkPolicy: allow only proxy+kbeads+coopmux+kube-dns (infra exists in `networkpolicies.yaml`) — highest-leverage control | 🟡 | platform/network |
| R4-ISO-3 | cluster-admin SA present in ephemeral namespace (`clusterAdmin.enabled: true`) bound to built-in `cluster-admin` ClusterRole | Isolation | L | H | 🟡 | Set `clusterAdmin.enabled: false` and/or `useRestrictedRole: true` in claudemore values | 🟢 | platform/RBAC |
| R4-ISO-4 | Un-sandboxed arbitrary Bash — gVisor default-OFF in ephemerals; real agent's self-chosen `bash -c` hits default runtime | Isolation | M | M | 🟡 | Flip `gvisor.enabled=true`+`runtime_class: runsc` for claudemore pods (plumbing exists, `gvisor.yaml`) | 🟢 | platform |
| R4-ISO-5 | Git push / real MRs reachable IF git creds added (absent in ephemeral template today; egress would permit gitlab.com:443) | Isolation | L | H | 🟡 | Codify no-prod-creds invariant: never project `GitCredentialsSecret`/`GitlabTokenSecret` into claudemore pods (assert the existing absence) | 🟢 | e2e/helm |
| R4-COEX-1 | Divergence — two suites assert contradictory things about the same surface | Coexistence | M | M | 🟡 | Single source of truth: claudemore asserts outcomes, claudeless owns contract; no surface asserted at both altitudes | 🟢 | e2e governance |
| R4-COEX-2 | Operators delete claudeless scenarios because "claudemore covers it" | Coexistence | M | H | 🟡 | Anti-pattern policy (§3): a surface leaves claudeless only if on the parity-doc DROP list | 🟢 | e2e governance |
| R4-COEX-3 | claudemore becomes a required per-PR/main-push hard gate (cost + flake trains "merge anyway") | Coexistence | M | H | 🟡 | Policy: claudemore non-blocking on scheduled first; soft-gate at tag only after flake-rate proven; never per-PR hard | 🟢 | e2e governance |
| R4-COEX-4 | CI-tier confusion — green main-push misread as "claudeless ran" (it didn't; smoke subset excludes claudeless module) | Coexistence | M | M | 🟡 | Document the tier matrix (§3) in `release-pipeline.md`; unambiguous module names | 🟢 | docs |

## 2. Per-dimension de-risking (the detail)

### 2.1 Cost

**What exists today.** Every e2e run uses claudeless (a scripted mock, zero LLM tokens) → token cost is **$0** today. Real-CC burn would hit (a) **every main push** via the one `agent-crew-lifecycle` smoke module (`run-shard.sh:593`, the smoke subset excludes the full `claudeless` module), and (b) **every tag/scheduled full E2E** across the ~113 `test-*.sh` scripts (`.gitlab-ci.yml:555-556`). The litellm cost pipeline is reusable as-is: per-session authoritative cost via baseline-at-spawn-minus-close on the `agent:<name>` tag (`reconcile_session_spend.go:177-310`) driving `gardenio_agent_spend_usd_total`; a 5-min account loop on `/key/info`+`/spend/logs` (`spend_sync.go:50-54`). A **$0-on-no-spend guarantee already holds** — the per-session reconcile delta is dropped when non-positive (`spend.go` non-positive guard); the counter is incremented once per session at close, so a no-reasoning run is provably $0. Opus 4.8/4.7/4.6, Sonnet 4.6, and **Haiku 4.5** are all in `model_list` (`configmap.yaml:46-58`), so tiering is config-only.

**Risks.** No per-key budget enforcement is wired (R4-COST-1, the decisive gap); no prompt cache (R4-COST-2); long agent lifecycles = many Opus turns (R4-COST-3); full suite spawns many agents (R4-COST-4); the 15-min shard caps wall-clock not cost (R4-COST-5). Order-of-magnitude *estimate* (Opus list price, no cache, honestly unmeasured): ~$3–$12/agent-session; a full E2E spawning ~10–20 agents ≈ $30–$240/run. The spend pipeline measures the real number on first run.

**Mitigations (cheap→structural).** Haiku tiering (config-only, ~1/15–1/20 Opus price); scenario sampling (the ~42 free TOML scenarios stay claudeless); prompt cache via `extraSettings`; **per-run hard `max_budget` ceiling wired via `/key/update` (structural, closes the gap)** — the fields exist in the read path, this is a write, not new schema; bound agent turn-count. Note `accounts.go:20-21` shows a `Fallback`/`exhausted`/`cooldown` rotation concept exists in the data model — a partial softener, but it relies on external enforcement, not litellm `max_budget`.

**Residual.** With Haiku + sampled + per-key `max_budget`: ~$0.20–$0.80/main-push, a few dollars/scheduled-run, hard-capped by litellm 429. Residual: (a) the `max_budget` write path must be built; (b) the metric is lagging (5-min reconcile), so the in-run kill must come from litellm's 429, not the metric; (c) Haiku fidelity < Opus.

**Verdict: 🟡** — fully measurable + cheaply reducible today, but the structural per-key budget ceiling that makes runaway spend *impossible* does not exist yet and must be built before real-CC runs on every main push.

### 2.2 Determinism / flake

**What exists today.** claudeless is deterministic by construction — it contains no model; every "turn" is a table lookup of `[[responses]]` blocks matched by `on`, first-match-wins, with `[tools] mode = "live"` driving real `gb`/`kd` side-effects (`local-lifecycle.toml`). Identical input ⇒ identical branch ⇒ identical Bash. The only non-determinism is the real infra the commands hit, which `lib-retry.sh` (3 attempts/2s) and Captain absorb. Flake machinery is mature: Captain `attempts:1`/`flaky-attempts:3`/`max-tests:5%`/`fail-fast:true` with targeted `captain-retry.sh`; a quarantine file (`gvisor-rehydrate-ack`, `trigger-system`); `SKIP_SCENARIOS` quarantines **10 reconcile-bound scenarios (~150–240s, ka-0000005245) plus 1 SIGTERM-bound** (`local-sigterm`) that blew the 15-min wall. Two history flakes occurred **with a fully deterministic harness**: a 5.5h golden stall across 7 tags (ka-0000005791) and a 1/102 cross-shard race (ka-0000004111). The coop S3 substrate already persists transcripts + `recording.jsonl` + workflow journals and rehydrates on resume (`s3.rs`, `record.rs`) — the record/replay substrate exists.

**Risks.** Output-text non-determinism (R4-DET-1); tool-choice/ordering (R4-DET-2); latency vs the 15-min shard wall **and the additional 180s per-scenario cap** (`test-claudeless.sh:127`) (R4-DET-3); **model drift — there is no opus-4-8 default; `claudeModel: ""` (`values.yaml:2035`) → Claude Code's built-in default (Sonnet)**, which Anthropic can change silently (R4-DET-4); stochastic-flake amplification of an already-flaky harness (R4-DET-5); non-reproducible failures (R4-DET-6).

**Mitigations.** *Cheap:* assert only on outcomes/side-effects (claudeless's `$GB_SCENARIO_RESULT_FILE` proves every node has a terminal-state probe); **explicit per-account model pin — load-bearing, not belt-and-suspenders, because there is no safe default to inherit** (`fields.model`→`CLAUDE_MODEL`, `resolve-auth.sh:122`); set temperature=0/seed (GAP: not present today, needs a one-line add); bounded polling under the walls. *Medium:* keep claudeless gating, run claudemore as a separate non-gating module; per-module `flaky-attempts:2`, never raise the global `3`. *Structural (the right answer):* **record/replay** — record a real CC session per scenario into a committed cassette, replay byte-for-byte in CI (instant, zero flake, zero cost, reproducible, generated by real reasoning); a scheduled **live canary** is the drift tripwire — when its outcomes diverge from the cassette, re-record. Live-every-run is RED (continuous latency+cost+flake, non-reproducible failures).

**Residual.** Cassette staleness between canary runs (bounded by ≤24h cadence); re-record review burden; first-recording bias (mitigate by N-sample outcome-invariance); outcome-only assertions under-specify (acceptable, matches claudeless posture).

**Verdict: 🟢 (conditional on BOTH record/replay AND an explicit per-account model pin)** — claudemore reaches claudeless-grade CI determinism while restoring reasoning. Live-every-run is 🔴 and must not be the default. The pin is conditional, not optional: without it the e2e model is undefined (Sonnet default) and drift is live today.

### 2.3 Secrets

**What exists today (corrected).** Account beads carry `litellm_key`, `base_url`, `max_budget` (`accounts.go`, `usage.go:41-42`). At pod creation the controller injects **both** a **real Anthropic OAuth token** (`CLAUDE_CODE_OAUTH_TOKEN`, from a K8s secret — the actual auth credential, forwarded by LiteLLM to Anthropic's direct API for Max-subscription billing, `podspec.go`) **and** a litellm virtual key as `x-litellm-api-key` header (for spend tracking/tagging, *not* authentication). The virtual key rides alongside a real model credential — it is not a substitute. The controller authenticates to litellm with the **master key** (`spend_sync.go:170/221`) and only **reads** keys (`/key/info`) — it never mints them; `/key/generate` is documented (`litellm-production-cutover.md`) but **not wired in Go** (grep: zero hits). CI today carries **zero** model creds — only AWS, registry, and a beads token (`.rwx/e2e.yml`). `buildCoopEnv` was partially hardened (strips non-allowlisted `CLAUDE_CODE_*`) but **still starts from `os.Environ()` wholesale** (`init_agent_loop.go:565`) — F2 defect remains. coop credential persistence **fails closed**: `bail!` unless `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` is explicitly set (`persist.rs:146-153`).

**Risks.** First real model credential in CI (R4-SEC-1); F2 env inheritance now carries a model cred (R4-SEC-2); F4 real agent echoes key into S3 recordings (R4-SEC-3); F5 bead-note leak (R4-SEC-4); F7 vault-not-provisioned trap (R4-SEC-5); F1 plaintext persistence (R4-SEC-6, smaller than first thought — fails closed by default).

**Mitigations.** Route real agent through the proxy so the *trackable/revocable* surface is a scoped virtual key with `max_budget` ~$1–5/run minted via the documented-but-unwired `/key/generate` (mint at namespace create, delete at reap); but **be honest: prod auth is the OAuth token, so a faithful run likely needs a real OAuth-style credential** — the de-risk is scoping/revoking/redacting it, not avoiding it. Add the empty-string vault guard (R4-SEC-5); assert `COOP_ALLOW_PLAINTEXT_BOOTSTRAP` unset (R4-SEC-6); close F2 with an allowlist; ship redaction F16/F17 covering `sk-ant-` and litellm `sk-`/`vk-` shapes; keep the **master key controller-side only** — it must never enter the CI agent path.

**Residual.** A leak exposes at most one short-lived, low-budget, revocable virtual key for tracking — but the OAuth token is the credential whose leak matters; bound it via budget, namespace lifetime, and redaction. R4-SEC-3/4 remain yellow until F16/F17 ship.

**Verdict: 🟡 (true yellow)** — de-riskable, and most mechanism-level grounding is solid, but the "scoped-virtual-key-only, never a raw model cred" narrative is undercut by prod's actual OAuth-token auth. The de-risk is about budget-scoping/revoking/redacting a real model credential, not avoiding one.

### 2.4 Isolation / blast-radius

**What exists today.** A real agent picks its own tool calls inside an agent pod — blast radius is "what an agent pod can reach," built for production agents. Primitives: PSA (enforced default **`baseline`**, `values.yaml:46`; `restricted` only audited/warned, `:49`); gVisor flag-gated **default OFF** (`values.yaml:896`); NetworkPolicy default-deny-ingress + agent-egress (enabled in ephemerals) allowing coopmux/beads/NATS/litellm/kube-dns **plus `0.0.0.0/0:443,80`** with only RFC1918/IMDS/CGNAT excepted; per-instance namespaces + reaper (45m/maxConcurrent=10). Already neutralized in ephemerals: Slack mocked (`slackBridge.enabled: false` + slackmock `lib-slackmock.sh`), git creds **absent** (template doesn't set `GitCredentialsSecret`/`GitlabTokenSecret`; materialization gated at `initclone.go:64`), default-deny ingress. **Live concerns:** prod-named IRSA role `gardenio-agents-s3` reused on a shared bucket with best-effort (not IAM-enforced) prefix isolation (`values.yaml:2059-2060`; IRSA web-identity bypasses the IMDS egress block); `clusterAdmin.enabled: true` binding the built-in `cluster-admin` ClusterRole (`agent-cluster-rbac.yaml:104-108`, `useRestrictedRole` default `false`); un-sandboxed Bash. (Note: `claudemore` does not yet exist in the codebase; mitigations are proposed work. The existing **litellm** in-cluster proxy already demonstrates the "proxy holds the single provider egress, agents stay default-deny to internet" pattern, so the egress-allowlist mitigation is well-grounded.)

**Risks.** AWS reachability with prod-named role (R4-ISO-1, highest-value live surface); broad internet egress (R4-ISO-2); cluster-admin SA present (R4-ISO-3); un-sandboxed Bash (R4-ISO-4); git push reachable if creds ever added (R4-ISO-5).

**Mitigations.** *Cheap config:* codify no-prod-creds invariant (assert git-cred absence); swap IRSA to a dedicated low-priv prefix-scoped CI role or drop `agents.s3.bucket`; set `clusterAdmin.enabled: false`/`useRestrictedRole: true`; extend slackmock to other external surfaces. *Structural:* claudemore-specific egress allowlist (proxy+kbeads+coopmux+kube-dns only) — single highest-leverage control, infra exists; gVisor mandatory for real-CC pods; turn/tool-budget kill-switch at the proxy.

**Residual.** Proxy becomes a trusted choke point (needs own hardening); S3 prefix isolation best-effort until IAM scoped (out-of-chart); shared-cluster blast from a gVisor escape (low-prob, defense-in-depth). All bounded; none are prod-write paths once creds removed.

**Verdict: 🟡** — primitives exist and several prod surfaces are already off in ephemerals, but the namespace ships a prod-named IRSA role, a cluster-admin SA, broad `0.0.0.0/0` egress, and no gVisor — fine for a scripted runner, unsafe for an autonomous agent until the config-only mitigations land. (The baseline-not-restricted PSA correction marginally strengthens the yellow.)

### 2.5 Coexistence

**What exists today (corrected).** claudeless is the upstream scripted mock (`claude` + `--scenario/--capture/--claude-version/--failure`), **42 TOML scenarios** in `gardenio/tests/e2e/claudeless/` (matching the parity doc), zero real LLM reasoning. It runs as the `claudeless` e2e **module** pinned to **shard 0** (`run-shard.sh:503`), gated to **tag (`/^20/`) + scheduled** pipelines only (`.gitlab-ci.yml:549-557`) — **not per-PR**. Main-push runs the `rwx-main` DAG whose smoke subset (`run-shard.sh:593`, **6 modules** in code despite a stale "5-module" comment, and `smoke-rwx.yml` still says 5 — real doc-drift) **excludes the claudeless module**; only `agent-crew-lifecycle` (which embeds claudeless) runs there. Active claudeless scenarios = 42 − 11 skipped = **31**. The parity doc's §3.1 "DROP — not claudeless candidates" list (parity doc 281-302: real prompt interpretation, tool-choice, compaction, model-drift, plan-mode) is verbatim claudemore's charter — the two suites are disjoint by construction.

**The boundary.** claudeless uniquely provides zero cost, zero flake, speed at scale, exact CLI/hook/bead **contract** pinning (stop-hook exit 0/2 in `stop-gate-cooldown.toml`; decision round-trip in `local-yield-close.toml`), and failure injection a model can't be coerced into. claudemore uniquely provides real prompt interpretation, real tool-choice, real compaction, model-drift detection, plan-mode — things claudeless structurally cannot do.

**Risks + mitigations.** Divergence (R4-COEX-1 → outcomes-only for claudemore, contract is claudeless-only); scenario deletion (R4-COEX-2 → DROP-list rule); flake-in-a-gate / per-PR hard gate (R4-COEX-3 → non-blocking scheduled first); tier confusion (R4-COEX-4 → document the matrix). Maintenance burden mitigated by reusing the `run-shard.sh` module pattern, not a parallel harness.

**Residual.** Authoring drift (periodic review against outcomes-only rule); sampling gaps (accepted — claudemore is the fidelity net, not the fast net; claudeless pins everything contractual on every tag); two-oracle triage confusion (bounded by the documented surface map).

**Verdict: 🟡** — the *policy argument* is green-worthy and sound (the DROP-list/charter disjointness is real, the contract-vs-behavior boundary is coherent, the tier facts are accurate), but I am not grading it green: the underlying researcher prose rested on a fabricated count (54/43 vs the real 42/31) and two phantom scenario anchors (`squawk-roundtrip`, prime-after-compaction are *proposed* backlog items P1/P11, not shipped). With the count corrected to 42/31 and examples regrounded on scenarios that exist (`stop-gate-cooldown.toml`, `local-yield-close.toml`), the policy is defensible — and it is the binding governance for the whole epic.

## 3. The coexistence policy (claudemore AUGMENTS claudeless)

> **claudeless is the fast, deterministic, tag-and-scheduled gate on PLUMBING — the literal `gb`/`kd` CLI contract, hook exit-code protocol, and bead-field shapes. claudemore is the slow, stochastic, *sampled and non-blocking* gate on REASONING and integration. A surface belongs to claudeless if its failure mode is "the contract changed"; to claudemore if its failure mode is "the model behaved differently." Neither suite may assert the other's territory. claudemore augments; it never replaces.**

**Division of labor (surface → owner):**

| Surface class | claudeless | claudemore |
|---|---|---|
| CLI flag/exit-code/bead-field contract (`gb squawk`, `kd claim`, `gb decision create`) | **OWNS** | no |
| Hook exit-code protocol (Stop/PreToolUse gate; `stop-gate-cooldown.toml`) | **OWNS** | no |
| Trigger/scheduler engine shape (cron, conditions, dedup) | **OWNS** | no |
| Failure injection (network/auth/rate-limit/out-of-credits) | **OWNS** | no |
| "Does the agent pick the right bead from `gb ready`" | no | **OWNS** |
| Real tool-choice / multi-step reasoning to completion | no | **OWNS** |
| Compaction + post-compaction prime re-injection (behavior) | contract only | **OWNS** behavior |
| Model-drift / tool-use shape regressions | no | **OWNS** |
| End-to-end lifecycle integration with a real agent | smoke embed only (`agent-crew-lifecycle`) | **OWNS** the real-reasoning version |

**Forbidden anti-patterns:**
1. **Do not delete a claudeless scenario because "claudemore covers it."** A claudemore pass proves the model *can* do it once; it does not pin the contract. A surface may leave claudeless only if it was never claudeless's to begin with (i.e., on the parity-doc DROP list).
2. **Do not gate every PR (or main-push) on real-CC.** It costs real spend and is inherently flaky; a flaky *required* gate trains operators to "merge anyway," destroying signal. claudemore must never be a required per-PR hard gate.
3. **Do not let claudemore re-test pure plumbing.** Asserting `kd claim` exit codes in claudemore pays LLM cost for what claudeless does free and creates a second, flakier oracle for the same fact — the seed of divergence. claudemore assertions target reasoning/integration *outcomes*, never CLI shapes.
4. **Do not let claudeless grow into a slow suite chasing model behavior.** The 11-scenario quarantine is the warning: claudeless's value *is* its speed. Adding model-shaped scenarios to claudeless is doubly wrong — it can't do them, and it'd blow the 15-min shard.

**Recommended CI-tier placement:**

| Tier | claudeless | claudemore |
|---|---|---|
| **main-push** (`rwx-main`, every commit; 6-module smoke) | smoke embed only (`agent-crew-lifecycle`) — keep as-is | **NEVER** (cost + flake) |
| **tag / pre-release** (`rwx-e2e` on `/^20/`) | **full suite, hard gate** — its home today | **sampled subset, advisory → soft-gate** only after flake-rate proven |
| **scheduled / nightly** (`schedule`) | full suite, hard gate | **full reasoning suite, non-blocking** (or the live cassette canary) — the home for slow/stochastic/expensive runs |

Concrete: keep claudeless exactly where it is; place claudemore on the **scheduled tier first, non-blocking**, as a new `run-shard.sh` module on its own shard with a longer timeout; promote a small sampled subset to a tag-tier *soft* gate only after measured flake rate is acceptable and spend is budgeted.

## 4. Cost model

**Measurement substrate (reuse as-is, no new pipeline):** the litellm spend reconcile — per-session baseline-at-spawn-minus-close on the `agent:<name>` tag (`reconcile_session_spend.go:177-310`) → `total_cost_usd` → `gardenio_agent_spend_usd_total`, plus the 5-min account loop (`spend_sync.go:50-54`). The metric is observable/alert-capable (a hard *cap* is the new `/key/update` work, not the metric). The $0-on-no-spend guarantee already holds.

**Assumptions (stated explicitly):**
- Model: **Haiku 4.5** for e2e agents (already in `model_list`), ~1/15–1/20 Opus token price.
- Sampling: claudeless carries the ~42-scenario bulk free; real-CC runs only `agent-crew-lifecycle`-class on main-push (if ever promoted) and a small sampled subset on scheduled.
- Per-agent estimate (Haiku, no cache): ~**$0.20–$0.80/agent-session** (derived from the Opus ~$3–$12 estimate ÷ ~15). With prompt cache on a stable harness prompt, the dominant repeated input cost drops further. These are estimates; the spend pipeline measures the real number on first run.

**Envelope under recommended mitigations:**

| Cadence | Composition | Per-run | Notes |
|---|---|---|---|
| **main-push** | claudeless smoke only (recommended: NO real-CC) | **$0** | If a single Haiku crew module were ever added: ~$0.20–$0.80/run |
| **scheduled / nightly** | sampled real-CC subset, e.g. ~5–10 Haiku agents | **~$1–$8/run** | Hard-capped by litellm `max_budget` 429 |
| **tag / pre-release** | sampled soft-gate subset, ~3–5 Haiku agents | **~$0.60–$4/run** | Only after flake-rate proven |

**Per-day / per-month:** with real-CC kept off main-push (recommended), daily spend is driven by scheduled cadence: at one nightly run ≈ **~$1–$8/day → ~$30–$240/month**, hard-capped. If a Haiku crew module were promoted to main-push at ~30 pushes/day, add ~**$6–$24/day**. **Budget ceiling (must-build):** mint the per-CI virtual key with `max_budget` per-run + `budget_duration` so litellm 429s at the cap — this makes the worst case bounded regardless of agent behavior, and is the structural guardrail that turns R4-COST-1 from red to yellow.

## 5. Go / No-Go risk summary

**Verdict: 🟡 GO-WITH-GUARDRAILS.**

Real-CC-in-e2e is de-riskable. Cost is measurable and cheaply reducible; determinism reaches claudeless-grade via record/replay; secrets, isolation, and coexistence are all addressable with edits to chart values, pipeline config, and a small amount of controller wiring — no new subsystems. No dimension is red; four are yellow and one (determinism) is green-conditional. Nothing blocks measurement or de-risking — but several structural guardrails do not exist yet and must be built before real creds touch CI.

**The 3–5 must-have guardrails that turn the yellows green:**
1. **Per-CI scoped litellm virtual key with `max_budget` + `budget_duration`, minted at namespace-create and deleted at reap** (wire `/key/generate`/`/key/update`, reusing existing read-path fields). Caps spend (R4-COST-1) and makes a leaked tracking key revocable/bounded (R4-SEC-1).
2. **Record/replay cassette gating CI + explicit per-account model pin in the e2e account bead; live runs scheduled-only, non-blocking.** Restores determinism (R4-DET-1/2/3/5) and removes the silent-default-model drift (R4-DET-4).
3. **claudemore-specific NetworkPolicy egress allowlist (proxy + kbeads + coopmux + kube-dns only), dedicated low-priv prefix-scoped IRSA role, `clusterAdmin.enabled: false`, gVisor on for real-CC pods.** Collapses the isolation surface (R4-ISO-1/2/3/4).
4. **Close F2 (`buildCoopEnv` allowlist) + ship redaction F16/F17 on coop recordings and bead writes; keep the litellm master key controller-side only.** Neutralizes the leak vectors a *reasoning* agent activates (R4-SEC-2/3/4).
5. **Codify the coexistence policy (§3) as governance: claudemore non-blocking/sampled, never a per-PR hard gate, never asserting CLI contract.** Prevents flake-in-a-gate and divergence (R4-COEX-1/2/3).

**Explicit pre-conditions a follow-on build epic MUST satisfy before any real creds touch CI:**
- Per-key `max_budget` enforcement wired and verified to 429 at cap (does not exist today).
- Empty-string vault guard at dispatch for any model-cred reference; `COOP_ALLOW_PLAINTEXT_BOOTSTRAP` asserted unset in the CI namespace.
- No-prod-creds invariant asserted (git creds absent; dedicated CI IRSA role; clusterAdmin off).
- Redaction layer live for `sk-ant-`/litellm key shapes on recordings and bead writes.
- claudemore wired as a non-gating module on the scheduled tier only — never on the main-push/per-PR hard path.

**The operator decision checkpoint should ask:**
- Is the per-CI virtual key actually minting with an enforced `max_budget`, and has a deliberate runaway test confirmed litellm 429s at the cap (not just that the metric eventually shows spend)?
- Does the e2e account bead set an explicit `fields.model` (no reliance on the Sonnet built-in default), and is the cassette/canary drift-detection cadence ≤24h?
- Is the claudemore egress NetworkPolicy proven to block `0.0.0.0/0`, and is the IRSA role a dedicated low-priv one (not `gardenio-agents-s3`)?
- Has F2 + redaction shipped, and is the master key confirmed absent from the CI agent path?
- Is claudemore guaranteed non-blocking and sampled, with a flake-rate threshold and a measured per-run spend ceiling before any promotion toward a gate?

## 6. Phase-0 gate attestation (HARD GATE — CLEARED 2026-08-11)

> Bead: **ka-0000000278** (Phase 0 — claudemore guardrails & preconditions). This
> section supersedes the §5 "safe to *build*, not yet safe to point real creds at CI"
> posture for the five must-build guardrails: all five landed on `main` and were
> re-verified green on `main@29187f6ee` (2026-08-11). It does **not** by itself
> authorize pointing real creds at CI — that remains an explicit operator go-decision
> per the coexistence policy (§3) and the still-open gated follow-up ka-0000001706
> (wiring the per-CI key into the namespace lifecycle). What it attests is that the
> structural guardrails a real cred would *land against* now exist and hold.

Each Phase-0 acceptance subtask, the MR that landed it, and the artifact re-run this
cycle to confirm the guard actually fires (not just that a file exists):

| Subtask | Req | Landed | Re-verified on `main` (2026-08-11) |
|---|---|---|---|
| 0.1 per-CI budget-capped virtual key | R4-COST-1 / SEC-1 | !6555 (ka-0000000823) | `go test ./gardenio/controller/cmd/gb -run TestBudgetCap429AtCap` → **PASS** (deliberate runaway 429s at cap); e2e `tests/e2e/scripts/test-litellm-budget-cap.sh` present |
| 0.2 explicit per-account model pin | R4-DET-4 | !6432 (ka-0000004991) | `go test ./gardenio/controller/cmd/controller -run TestAccountModelPin` → **PASS** (4 cases: pinned→exact, unpinned→global fallback, pin beats global, no-global→unset) |
| 0.3 isolation guardrails | R4-ISO-1/2/3/4 | !6536 (ka-0000005906) | `helm template` with `claudemore.isolation.egressAllowlist=true` **drops** the two internet-egress NetworkPolicies (8→6); `validate-claudemore-isolation.yaml` **fail-closes** on clusterAdmin-on / gvisor-off / shared-IRSA-role / empty-s3-prefix |
| 0.4 F2 + redaction + key/vault guards | R4-SEC-2..6 | !6470 (ka-0000005576) | `go test ./kbeads/internal/server -run Redact` → **PASS** (F17 bead-write cred-shape redaction); coop `record.rs` F16 `sk-ant-`/`sk-`/`vk-` shape redaction present; `LITELLM_MASTER_KEY` used only controller-side (`main.go` spend sync), never in `podspec.go`; coopmux R4-SEC-6 render guard fail-closes `COOP_ALLOW_PLAINTEXT_BOOTSTRAP` under the production posture |
| 0.5 coexistence governance | R4-COEX-1/2/3 | !4301 (ka-0000000768) | Governance section present at `docs/release-pipeline.md` (tier matrix + 4 anti-patterns + 3 principles) |

**The operator checkpoint (§5), answered.**
- **Budget 429 at cap** — yes; `TestBudgetCap429AtCap` drives a deliberate runaway and
  asserts LiteLLM refuses with HTTP 429 once spend ≥ `max_budget`, not merely that spend
  eventually shows.
- **Explicit `fields.model` pin** — yes; the pin is regression-locked (`podspec_account_model_pin_test.go`),
  no reliance on the mutable Sonnet built-in default. (Cassette/canary drift cadence is a
  Phase-1+ concern, tracked separately.)
- **Egress blocks broad internet + dedicated IRSA** — yes; the allowlist structurally drops
  the broad internet-egress policies, and the render guard **refuses** the shared
  `gardenio-agents-s3` role under isolation.
- **F2 + redaction shipped, master key absent from agent path** — yes; redaction is live on
  both coop recordings (F16) and bead writes (F17), and the master key is controller-side
  only.
- **Non-blocking / sampled** — codified in the coexistence governance section; claudemore
  is never a per-PR or main-push hard gate.

**Verdict: 🟢 Phase-0 guardrails GREEN.** The structural preconditions the R4 register
called "must exist before real creds touch CI" now exist and are verified. Enabling real
creds in a CI run remains a deliberate operator go-decision gated on ka-0000001706
(per-CI key wired into the namespace lifecycle) and the coexistence policy — the guardrails
are built; the switch is intentionally still off.

If all five checkpoint answers are yes, the yellows are green and real-CC e2e is GO. Until they are, the honest posture is GO-WITH-GUARDRAILS: safe to *build* the de-risking, not yet safe to point real creds at CI.