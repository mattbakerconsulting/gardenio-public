# ADR: `git-blob` Bead Type — Security and Secret Redaction

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p3-gitblob-adr-xnu
**Phase**: P3 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000007332
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (§ 8 — security)
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks `secrets_redacted` and `secrets_summary` as fields on the type, and the `project_scoped: true, escalatable: true` write policy
**Resolves decisions**: **D-6** (secret-scanner choice and policy on hit)
**Defers**: D-1 (P1), D-3 (P1), D-4 (P2), D-7 (P5), D-8 (P6)

## TL;DR

Every `git-blob` registration runs through a server-side secret scanner before persistence. The scanner is **gitleaks**, configured to extend the existing repo-root `.gitleaks.toml` (already used by the pre-commit hook and the `gitleaks` CI task in `.rwx/ci.yml`). On any finding the server **redacts the matched span in place**, sets `secrets_redacted=true`, and records the rule, line number, byte range, and a fingerprint in `secrets_summary`. The original secret bytes are never persisted, never logged, never emitted in events. Plain rejection is rejected as the default policy because it loses the snapshot and pushes operators toward `--no-redact`.

A narrow **`--no-redact` admin override** exists for the rare case (compliance evidence, postmortem reproduction of a leak) where the unredacted blob has to land in the store. It requires `min_role: gardenio-admin`, a `--reason` string, and emits a mandatory `audit`-kind bead linked to the resulting `git-blob`. The override flag does not skip scanning — the scan still runs, populates `secrets_summary`, and sets `secrets_redacted=false` so the audit trail captures *what* was knowingly retained.

Read access is unchanged from the default bead read path (fleet-bounded, project-scoped). A per-fleet write rate limit on `git-blob` registrations bounds blast radius from a compromised agent token. Binary blobs and `encoding=omitted` blobs are scanned only on inputs the scanner can read; the policy matrix is in § 5.2.

This ADR ships **no code**. It is the spec the eventual P9 implementation will register at the create handler for `type:git-blob` and the `audit`-kind hooks. The operator-facing fork resolved here (D-6) is surfaced in a single `gb decision` so the operator either accepts the recommended defaults (gitleaks, redact-not-reject, admin `--no-redact` with audit) or sends back a specific override.

## 1. Context

### 1.1 What the plan-of-record asks

`kbeads/docs/research-git-blob-bead-type.md` § 8 (Security) frames three concerns:

1. **Secrets** — source files routinely contain false-positive secret-shaped strings; the proposal is "run a server-side scanner on every registration. Default scanner: gitleaks. Behavior on hit: redact, set `secrets_redacted=true`, refuse to store the unredacted version. Override `--no-redact` requires `min_role: gardenio-admin`, audit-logged."
2. **Access control** — `project_scoped: true` write policy (locked in P0); fleet-bounded reads; private-repo blobs must not become world-readable by virtue of being a bead.
3. **PII / license / size DoS** — max size cap (16MB default; tier thresholds are P2's call), per-fleet rate limit, license text is fine, PII redaction is the operator's job before registration.

D-6 in plan-of-record § 10 asks the operator to confirm the scanner choice and the policy on hit. Default proposal: gitleaks; redact + `secrets_redacted=true`; override requires admin.

### 1.2 What P0 already locked

P0 ADR (ka-0000006442) made `secrets_redacted` (boolean) and `secrets_summary` (json) optional fields on `type:git-blob`. P0 also locked the write policy to `project_scoped: true, escalatable: true` with **no** `min_role`. P3 must therefore design the scanner and override path knowing:

- The write gate is project-scoped, not role-scoped. Role-gated behavior (e.g. `--no-redact`) is layered on top of the project-scoped default — the role check happens at the override flag, not at the type-write boundary.
- The two fields exist; this ADR fixes their **shape** and **lifecycle** (when set, what they contain, how they are produced).

### 1.3 What gitleaks gives us today

The repo already runs gitleaks in two places:
- Local pre-commit hook (`scripts/hooks/pre-commit` + `make hooks`).
- CI task `gitleaks` in `.rwx/ci.yml`.

`.gitleaks.toml` extends the upstream default ruleset (`useDefault = true`) — generic API keys, AWS, GCP, Slack, GitHub, JWT, etc. — and carries a `[allowlist]` of paths and regexes for known false positives in this repo (litellm aliases, public OAuth client ids, webpack bundles, `*_test.go`, the negative-path OIDC auth test, the gitleaks config itself, and a generic placeholder regex).

That config is curated, in-tree, and already operator-trusted. Reusing it for the `git-blob` server-side scan is the lowest-friction option and prevents drift between "what the pre-commit hook flags" and "what the bead store flags".

### 1.4 Threat model

The threats P3 must address:

| Threat | Channel | Mitigation locus |
|---|---|---|
| Agent commits a `.env` file by accident, registers it as a `git-blob`. | Write-time | Server-side scanner redacts before persistence. |
| Agent's token is compromised; attacker mass-registers blobs to exfiltrate scraped secrets. | Write-time | Scanner + per-fleet rate limit + `project_scoped`. |
| A blob is ingested that the scanner does not yet know about (novel rule). | Read-time | Re-scan on rule update; retro-redact. (§ 6.) |
| Operator deliberately stores an unredacted secret for compliance/postmortem. | Write-time | `--no-redact` admin override + mandatory audit bead. |
| Cross-fleet read leakage. | Read-time | Existing fleet-bound read path (no changes here). |
| DoS via 5GB binary registrations. | Write-time | Size cap (P2's tier policy) + per-fleet rate limit. |
| Sensitive content slips past gitleaks (custom token format, encrypted blob). | Write-time | Defense-in-depth: scanner is necessary, not sufficient; allowlist evolves; rule-update retro-scan; § 6. |

Threats explicitly **out of scope**:
- PII detection. The plan-of-record (§ 8.3) puts PII redaction on the operator before registration. We do not run a PII classifier.
- License-text "scrubbing". License files (LICENSE, NOTICE) are intentionally retainable; gitleaks does not flag them.
- Malware scanning of binary content. Binary blobs are stored as bytes, not executed; if a fleet wants antivirus on uploads, that is a separate ADR.

### 1.5 Why this ADR exists separately from P2 (storage)

P2 chooses where bytes live (inline vs `git_blob_content` table vs S3). P3 chooses what bytes are eligible to live anywhere at all, and what shape `secrets_summary` takes. The two interact (a redacted blob has a different `blob_sha` than the original; see § 4.4) but they decompose cleanly: P3 decides the *content gate*, P2 decides the *content placement*.

## 2. Decision: default scanner → gitleaks (resolves **D-6**, scanner choice)

### 2.1 Options considered

| Option | Pros | Cons |
|---|---|---|
| **gitleaks** ★ | Already in repo (`.gitleaks.toml`), already trusted by pre-commit + CI, single source of truth for the rule set, mature default ruleset, redaction-friendly (per-finding rule + line + byte-range output), extensible via TOML, can be invoked as a library or subprocess. | Go binary; if invoked as a subprocess on every write, latency/process overhead matters. Library embedding (`github.com/zricethezav/gitleaks/v8`) is idiomatic for kbeads. |
| **trufflehog** | Verifies findings (live HTTP probe) — fewer false positives. | Verification costs network and time per write; "live secret check" against third-party APIs from the bead-write hot path is a non-starter. Different ruleset means drift from existing pre-commit/CI. |
| **detect-secrets (Yelp)** | Python; pluggable detectors. | Operationally awkward for a Go server (subprocess + Python runtime), no presence in this repo today. |
| **Custom regex set** | Total control. | We would re-derive what gitleaks already curates, badly. Drift across teams. No reuse of `.gitleaks.toml`. |
| **No scanner; trust pre-commit** | Zero cost. | Pre-commit only fires for human pushes through a clone. Agents pushing via API, the gitlab-bridge auto-registering on merge (P8), and `--from-file` ingestion all bypass it. The bead store is a separate write path; it needs its own gate. |

### 2.2 Decision

Default scanner is **gitleaks**, embedded as a library (preferred) or invoked as a vendored binary (acceptable fallback). Configuration is loaded from the canonical `.gitleaks.toml` at server start, with override env var `BEADS_GITLEAKS_CONFIG` for fleets that need a different ruleset.

### 2.3 Rationale

1. **Single source of truth.** A `git-blob` rejected at the bead store should match what the pre-commit hook would have flagged. Two scanners with two configs guarantee disagreement; one scanner with one config is the only way to keep "what counts as a secret" coherent across the fleet.
2. **Already operator-trusted.** The `.gitleaks.toml` allowlist is a curated artifact this fleet has tuned for its real false-positive patterns. P3 inherits that work.
3. **No live-verification.** Any scanner that probes third-party APIs from the write hot path is unfit for the bead store. Gitleaks is regex-based; it is fast and offline.
4. **Library, not subprocess.** Embedding `github.com/zricethezav/gitleaks/v8` (or current canonical Go module) avoids fork/exec on every write. Implementation detail for P9 — flagged here so P9 doesn't default to subprocess.
5. **Extensible without forking.** The `[extend]` directive in `.gitleaks.toml` already gives us "default ruleset + our additions". A fleet that needs a tighter rule set adds it to its config; it doesn't have to vendor a different scanner.

### 2.4 What "extends `.gitleaks.toml`" means at the server

- The server loads the canonical config at start (path resolved from `BEADS_GITLEAKS_CONFIG`, defaulting to the `.gitleaks.toml` baked into the deployment image — kbeads in production runs from the gardenio monorepo image, so the same `.gitleaks.toml` ships with the binary).
- Loading is one-shot; reload requires SIGHUP or process restart. Hot-reload is out of scope for this ADR (could be added later if rule-tuning velocity demands it).
- Server emits a startup log line `gitleaks config loaded: <path> rules=<n> allowlist_paths=<n> allowlist_regexes=<n>` so operators can verify the right config is in effect.
- Server exposes a `/healthz?check=gitleaks` endpoint that returns the loaded rule count and config sha256 — for observability and "is the scanner actually loaded" debugging.

### 2.5 Falsifiers

If gitleaks is later judged inadequate (e.g. consistently misses a class of secrets that hits us in production), the in-process interface is small enough to swap. The interface is "given (path, bytes), return list of findings". Any scanner conforming to that interface is a drop-in. The default choice is gitleaks; the architecture does not require it.

## 3. Decision: policy on hit → redact, do not reject (resolves **D-6**, hit policy)

### 3.1 Options considered

| Option | Behavior on finding | Pros | Cons |
|---|---|---|---|
| **A — Redact in place** ★ | Replace matched bytes with `<REDACTED:<rule>:<fingerprint>>`. Persist redacted content. Set `secrets_redacted=true`. Record metadata in `secrets_summary`. | Snapshot is preserved (the rest of the file is intact and still useful for diff/embed/search). Operators see what was redacted and where. The "I tried to register a file with a secret" event is observable and recoverable. | Redacted content is no longer byte-identical to the file at `(repo, commit, path)`. The `blob_sha` we compute is over the redacted bytes; the *original* git blob SHA is recorded separately (§ 4.4). |
| **B — Reject the registration** | Return 422; no bead created; emit a warning event. | Simpler; the store never holds redacted-but-still-suspicious content. | The agent's `kd git-blob add` call fails, which means the snapshot is lost. Operators will work around it with `--no-redact` to "just get the registration to succeed", which is the worst outcome — every workaround uses the override. Loses the legitimate, redaction-safe content of the rest of the file. |
| **C — Reject only above N findings; redact otherwise** | Threshold-based. | Compromise. | The threshold is arbitrary and will be argued forever. No clear win over A. |
| **D — Persist original; refuse to return content on read** | Server-side filter at read time. | Most flexible. | Holding unredacted secrets in the store is exactly the threat we are mitigating. Every storage backup, every event payload, every audit log of the row exposes them. Hard veto. |

### 3.2 Decision

**Option A — redact in place.** The redacted bytes are what `git-blob.content` (or `content_ref`) stores. Original secret bytes are never written to disk, never emitted in events, never logged.

### 3.3 Rationale

1. **Preserve the snapshot.** A 500-line config file with one `aws_access_key = "..."` line is overwhelmingly useful with that one line redacted. Rejecting the whole registration throws away the other 499 lines of legitimate context.
2. **Avoid the "every workaround uses `--no-redact`" trap.** If the default behavior is "reject", agents and operators learn that the only way to get a registration to land is the admin override. That funnels every workaround through the audited override path, drowning the audit signal in noise. Redact-by-default keeps `--no-redact` as the rare, deliberate event it should be.
3. **Defense in depth.** Even when a redaction strips the matched bytes, the rule + line + fingerprint live in `secrets_summary`. Re-scanning later, retro-redacting on rule updates, and "did this file ever contain a leaked secret of class X" queries all become possible. None of those are possible if we reject.
4. **Observable.** The bead is created with `secrets_redacted=true`. Operators querying `kd list --type=git-blob -f secrets_redacted=true` see exactly which snapshots tripped the scanner. That visibility is itself a security feature.

### 3.4 The redaction format

Each finding's matched span is replaced with a deterministic placeholder:

```
<REDACTED:<rule-id>:<fingerprint>>
```

- `<rule-id>` — the gitleaks rule id (e.g. `aws-access-token`, `slack-bot-token`, `generic-api-key`). Operators can grep for redactions by rule.
- `<fingerprint>` — first 12 hex chars of `sha256(rule-id || ":" || matched-bytes || ":" || fleet-secret-pepper)`. The pepper is per-deployment, never logged. Two redactions of the same secret in the same fleet share a fingerprint; the same secret in two fleets has two different fingerprints (so cross-fleet correlation requires operator action). The fingerprint is **not** reversible — it cannot recover the secret — but lets operators ask "do these two redactions point at the same underlying secret?"

The placeholder is one token, no newlines, identifiable by literal prefix `<REDACTED:`. A scanner false-positive on the placeholder itself is allowlisted in the default config.

### 3.5 Why include a fingerprint at all

Without a fingerprint, two redactions look indistinguishable, and operators cannot answer questions like "is the same AWS key still showing up across many blobs?" without the original. The peppered hash is the minimum primitive that lets us answer the recurrence question without the underlying secret. If a fleet decides this is too much information leakage (e.g. compliance forbids any derivative of secret material), the implementation can fall back to a constant placeholder by setting an env var; default is fingerprinted.

### 3.6 Counter-arguments considered

- **"Redaction is irreversible; an operator who needed the original is stuck."** Correct, and intentional. The escape hatch is `--no-redact` (§ 5), which records the override decision in an audit bead. The expected workflow for "I really need to see what was there" is: re-fetch from git, run the scanner locally, decide whether to register with `--no-redact`.
- **"What about partial-line secrets that span the redaction boundary in weird ways (multi-line PEM blocks)?"** Gitleaks already handles multi-line patterns via `regex` rules; the redaction span is whatever gitleaks reports. We do not try to widen the span.
- **"Re-running the scanner on the redacted content might re-flag the placeholder."** Allowlisted by default-shipping a regex `<REDACTED:[a-z0-9-]+:[0-9a-f]+>` in the bundled gitleaks config. This is a one-line addition to the in-repo `.gitleaks.toml`; recorded as a follow-up below.

## 4. Decision: `secrets_summary` schema

### 4.1 Shape

`secrets_summary` is an array of finding records. P0 typed it as `json` (free-form). P3 fixes the shape.

```jsonc
{
  "scanner": "gitleaks",
  "scanner_version": "8.x.y",
  "config_sha256": "<sha256 of loaded gitleaks config>",
  "scanned_bytes": 12345,
  "findings": [
    {
      "rule_id": "aws-access-token",
      "rule_description": "AWS access key",
      "line": 42,
      "start_byte": 1024,
      "end_byte": 1044,
      "fingerprint": "9f2c1b7a4e80",
      "replacement": "<REDACTED:aws-access-token:9f2c1b7a4e80>"
    },
    ...
  ]
}
```

### 4.2 Why each field

- `scanner` / `scanner_version` / `config_sha256` — let the retro-redact pass (§ 6) ask "was this blob scanned with the current rule set, or do I need to re-scan?" Without these fields, every rule update would force a full re-scan of every blob.
- `scanned_bytes` — distinguishes "we scanned 12KB of text" from "we scanned 0 bytes because the blob was binary / omitted" (§ 5.2 matrix). Useful for audit and for spotting "scanner silently scanned nothing" bugs.
- `findings[]` — the per-finding records. `line` is 1-based for human display; `start_byte` / `end_byte` are 0-based and are the authoritative span (lines are derived). `replacement` is the literal string that was substituted, included so an operator can grep for it in `content` and confirm it lines up.
- The original secret bytes are **never** in this record, by construction.

### 4.3 What is NOT in `secrets_summary`

- Original matched bytes. Hard rule.
- Surrounding context bytes ("the 20 bytes before/after"). Even leading context can leak the secret prefix (`AKIA...`).
- A "reason" string the operator provided (that lives on the audit bead, § 5).

### 4.4 `blob_sha` semantics under redaction

P0 made `blob_sha` a required field, computed server-side. P3 fixes the meaning under redaction:

- `blob_sha` is the SHA-1 of the **stored** (post-redaction) bytes. This is what the bead store actually holds, and what P2's content-addressed dedup keys on.
- A new optional field `original_blob_sha` records the SHA-1 of the **pre-redaction** bytes, when known (i.e. when the scanner was given the original content). This lets retro-redact (§ 6) detect "this blob's stored content is the result of a redaction; the original blob in git matches `original_blob_sha`" without exposing the original bytes.
- When `secrets_redacted=false`, `original_blob_sha` is absent or equal to `blob_sha` (no redaction happened).
- This is a small additive field and falls inside the latitude P0 § 7.2 left to later phases ("P3 may revise `secrets_summary` shape"). P3 takes that latitude here. P9 will register it on the type-config alongside the others.

### 4.5 Why this matters

Without `original_blob_sha`, a P2 storage tier that dedups by `blob_sha` will silently dedup *redacted* content rows together (correct) and will treat a redacted row as a different content from the original (also correct), but cannot answer "is this redacted blob equivalent to the original git object?" — which is exactly the question retro-redact and audit need to answer.

## 5. Decision: `--no-redact` admin override

### 5.1 Shape

A registration carrying `--no-redact` (CLI) or `redact: false` (API) bypasses the *redaction step* (not the scan). Behavior:

1. The scan still runs. `secrets_summary` is still populated. `secrets_redacted` is set to **`false`** to flag "scanner found things; operator chose to retain them".
2. The original (unredacted) content is stored.
3. An `audit`-kind bead is created in the same transaction. The audit bead carries `actor` (the override-issuing operator), `action=git-blob.no_redact`, `target=<the git-blob bead id>`, `reason=<--reason argument, required>`, `findings_count=<n>`, and `outcome=success`. An audit bead also carries (per existing `type:audit`) `min_role: gardenio-admin` write policy — this is the precedent we are matching.
4. The `git-blob` bead carries `audit_bead_id=<kd-...>` linking back to the audit record.
5. If the audit-bead create fails, the whole registration fails (atomic). No `git-blob` lands without its audit record when `--no-redact` is in play.

### 5.2 Authorization

`--no-redact` requires `min_role: gardenio-admin`. This is an *additive* gate on top of P0's `project_scoped: true` write policy: an operator must be both authorized for the project label AND hold `gardenio-admin`. The check is implemented at the create handler (the type-config write policy stays project-scoped — role-gating one optional flag does not require changing the type-config write policy for the type as a whole).

The CLI rejects `--no-redact` early when the credential cannot satisfy the role check, so the request never reaches the server with mismatched expectations.

### 5.3 `--reason` is mandatory

`--no-redact` without `--reason "..."` is rejected (CLI and server). The reason is recorded in the audit bead and is **not** stored on the `git-blob` itself. Rationale: the reason is metadata about the *override decision*, not about the blob; it belongs with the audit row.

A short list of expected reasons (free-form, not enum):
- "compliance evidence for incident XXX"
- "postmortem reproduction of leak in MR YYY"
- "intentional reference example for runbook ZZZ"

Operators are free to write whatever, but empty / one-character / "test" reasons are the kind of thing the audit reviewer should flag — outside this ADR.

### 5.4 Discoverability

- `kd list --type=git-blob -f secrets_redacted=false -f secrets_summary:exists=true` returns "blobs that contained secrets and were knowingly retained". This is the primary review query.
- `kd list --type=audit -f action=git-blob.no_redact` returns the audit row stream.
- A periodic `audit-review` gardener (out of scope for P3, mentioned for P7/P8) could surface these for human review on a schedule.

### 5.5 Why not "always redact, full stop"

The honest answer is: redaction is sometimes wrong. A compliance evidence bead capturing "the leaked secret as it appeared in the merged commit" is the artifact, not a redaction of it. Trying to forbid the override pushes that workflow off-platform (operators screenshot the secret, paste it into a separate system) which is strictly worse for audit. A narrow, audited override is better than a silent escape hatch.

### 5.6 Why audit-bead and not just a log line

The audit bead is queryable, exportable, dependency-linkable, and survives log retention. A log line is grep-only and rotates out. The existing `audit` type (`type:audit`, `min_role: gardenio-admin`, required `actor`/`action`/`outcome` per P0 § 1) is exactly the primitive we want — we are not inventing a new audit format, only routing one specific override through it.

## 6. Decision: rule-update retro-redact

### 6.1 Problem

Gitleaks rules evolve. A blob registered today scanned with rule set v1 may later be in rule set v2 known to flag a token format v1 missed. Doing nothing means the bead store accumulates pre-rule-set-v2 unredacted content forever.

### 6.2 Decision

Provide an admin tool (P9 implementation) that re-scans existing `git-blob` beads with the current loaded ruleset and applies redaction to any new findings. Behavior:

- Tool: `kd admin git-blob retro-redact [--repo X] [--since T] [--dry-run]`.
- Scoped: `min_role: gardenio-admin`. Project filter optional.
- For each blob: if `secrets_summary.config_sha256 != current_config_sha256`, re-scan. If the re-scan turns up new findings, redact in place, update `content` (or the content-table row, per P2), update `blob_sha`, update `secrets_summary`, set `secrets_redacted=true`, append to a per-blob retro-redact event log on the bead.
- The pre-retro-redact `blob_sha` is preserved as `pre_retro_redact_blob_sha` (one-shot history; we do not keep N levels). Original (unredacted-at-original-ingest) bytes are **not** retained in the store — once redaction happens, even retro, they are gone.
- Dry-run prints what would change; does not write.
- `--no-redact` blobs are skipped (their override is intentional retention; retro-redact does not override the override).

### 6.3 Why a tool, not an automatic background job

A background "always retro-redact on rule update" pass is tempting but mutates content beads on a schedule that operators don't see, and risks redacting content during an active investigation. A manual tool with `--dry-run` is what auditors actually use. P7 (lifecycle / GC) can later add a scheduled wrapper if operators want one — out of scope here.

### 6.4 Cost

Re-scanning N blobs is O(total content bytes). With the tier policy (P2: ≤64KB inline) the inline tier rescans cheaply; the >1MB S3 tier requires presigned reads. A `--repo` filter scopes the cost. The default invocation is **never** "scan everything"; an operator must pick a scope.

## 7. Decision: scan eligibility matrix

### 7.1 What the scanner sees

| `encoding` value | Scan? | Notes |
|---|---|---|
| `text` | yes | Default path. |
| `truncated-text` | yes, on the truncated content. | The unscanned tail is unscanned by definition; flag as a known limitation in `secrets_summary` (`scanned_bytes` < `file_size`). |
| `base64` (binary) | no by default. | Gitleaks regex rules don't fire usefully on binary. Operator may flag a binary-scan add-on, out of scope here. |
| `truncated-base64` | no | Same as above. |
| `omitted` | no | No content stored; nothing to scan. `secrets_summary` is `null`. |

### 7.2 Why binary blobs are scan-skipped

- Regex scanners produce floods of false positives on binary.
- The interesting "secret in a binary" cases (embedded creds in a compiled binary) require specialist scanners (`trufflehog filesystem`, `semgrep` rules) — separate capability, separate ADR, opt-in per fleet.
- Skipping binary by default with an explicit `secrets_summary.scanned=false` reason is honest: "we did not scan; here is why".

### 7.3 Truncation

When a blob is truncated to the inline tier limit (P2's call, default 64KB), the scanner runs on the stored truncated content. The unscanned suffix is recorded as an explicit limitation in `secrets_summary`:

```jsonc
{
  "scanned_bytes": 65536,
  "skipped_bytes": 1048576,
  "skip_reason": "truncated-by-storage-tier",
  ...
}
```

This is intentionally pessimistic — an operator looking at `secrets_redacted=false` on a `truncated-text` blob should be able to see "we scanned the prefix and didn't find anything; we did not scan the suffix" and decide whether to chase it.

## 8. Decision: per-fleet write rate limit

### 8.1 Decision

A per-fleet rate limit on `git-blob` create RPCs:

- **Default budget**: 60 creates per minute per fleet (sliding window). Tunable via env var `BEADS_GIT_BLOB_RATE_PER_MINUTE`.
- **Burst allowance**: 120 creates in any 10-second window (allows bridge batch-registration of an MR with many files).
- **Enforcement**: server-side, before scan. A request over budget returns 429 with a `Retry-After` header.
- **Per-actor sub-limit**: 30 creates per minute per actor, to prevent one compromised token from monopolizing the fleet budget. Tunable via `BEADS_GIT_BLOB_RATE_PER_ACTOR_PER_MINUTE`.

### 8.2 Why these numbers

- A typical agent MR touches 1–20 files; bridge batch-registration of 20 files at once must not 429. The 120-in-10s burst covers it.
- 60/min sustained ≈ 86,400/day fleet-wide. Far above legitimate use (we currently see hundreds of agent MRs per day, each registering a handful of blobs).
- A compromised token capped at 30/min cannot exfiltrate at high speed; an attacker needs a long, observable, rate-limited tail.
- Per-fleet limit is the macro brake; per-actor is the micro brake. Both are necessary because either alone is gameable (one actor saturating the fleet vs many actors each within their per-actor budget).

### 8.3 What rate limits do not buy

Rate limits are not the secret-leak mitigation — the scanner is. Rate limits are the *blast-radius* mitigation: even if the scanner has a blind spot, an attacker cannot exploit it at scale. Designing the two as independent layers is deliberate.

### 8.4 Counter-arguments considered

- **"Rate limits will hurt legitimate batch backfill"** — a one-time backfill (per plan-of-record P8) needs a higher budget. Solution: backfill runs through an admin path that bypasses the rate limit, recorded as an audit. Out of scope to specify here; flag for P8.
- **"Why not per-IP?"** — kbeads does not consistently see the original client IP through gRPC + ingress; per-actor (token) is the meaningful unit. Per-IP is a possible later add.

## 9. Decision: read access (no change from existing path)

### 9.1 Decision

`git-blob` read access inherits the existing kbeads bead-read policy, unchanged:

- Fleet-bounded: a fleet sees its own beads.
- Project-scoped (when project labels are present and the reader's project authorization applies): same as any bead.
- No special "secrets" gate on `git-blob.content` — the *content* is the redacted content; if it is in the bead, it is fit for the same readers any other bead is fit for.

### 9.2 Rationale

If we could not promise that the content stored in a `git-blob` is appropriate for the bead's readers, we would have a different problem (the redaction is wrong). The redaction policy (§ 3, § 4) is what makes content readable at the existing access level. Adding a separate "secrets" read gate would imply the redaction is insufficient — which would be a P3 design failure, not something to mitigate at read time.

The sole exception is `--no-redact` blobs (§ 5). These are admin-authored, audit-linked, and visible to the same readers as any other bead in the project — the override decision was a deliberate operator action that made the unredacted content acceptable for the project's read scope. If a fleet wants stricter quarantine for `--no-redact` blobs, that is a fleet-policy add-on, not a default.

### 9.3 What is explicitly NOT done here

- A "redact at read time" code path. Read returns whatever is stored. Storing redacted content is the contract.
- A "decrypt-at-read" capability. We do not encrypt content at rest *as a redaction substitute*; encryption-at-rest at the database layer is orthogonal and is out of scope.
- A "second admin role to read `git-blob.content`". Not warranted — see § 9.1.

## 10. Audit trail summary

This ADR adds two streams of auditable signal:

1. **`secrets_redacted=true` on the `git-blob` itself**, with `secrets_summary` describing what was redacted. This is queryable and visible in any bead read. No special audit row is needed for the *normal* (redacted) case — the bead itself is the record.
2. **`audit`-kind beads for `--no-redact` overrides**, one per override, linked from the `git-blob` via `audit_bead_id`. This is the rare case; the existing `type:audit` machinery (P0 § 1) is the substrate. Required fields on the audit row: `actor`, `action=git-blob.no_redact`, `target=<git-blob-id>`, `reason`, `findings_count`, `outcome`.

Retro-redact actions (§ 6) append to a per-blob event log on the `git-blob` bead via the existing event-recording machinery (`store.RecordEvent` + publisher per `kbeads/CLAUDE.md` § "Landing the plane"). No separate audit bead per retro-redact event — the bead's event history is the audit. (If a fleet later wants per-retro-redact audit beads, that is a small extension.)

## 11. What this ADR does NOT decide

To keep this ADR small and reviewable, the following are explicitly out of scope and deferred to their owning phase:

| Decision / Concern | Owning phase | Reason |
|---|---|---|
| Storage tier thresholds and external-store choice (D-4) | P2 | Where bytes live ≠ what bytes are eligible. |
| Embedding policy for `git-blob.content` (D-8) | P6 | Embedding pipeline reads the *stored* (redacted) content; eligibility is set by `encoding`, not by `secrets_redacted`. |
| Bridge auto-registration on merge (P8) | P8 | Bridge writes go through the same scanner; backfill rate-limit relief is P8's call. |
| GC interaction with `secrets_redacted=true` blobs | P7 | Lifecycle phase. Default: redacted blobs are normal blobs for GC purposes. |
| Hot-reload of gitleaks config | follow-up | Restart-to-reload is fine for v1. |
| PII detection | not in scope | Operator's responsibility per plan-of-record § 8.3. |
| Encryption-at-rest at the database layer | not in scope | Orthogonal infra concern. |
| Antivirus / malware scanning of binary blobs | not in scope | Separate capability if a fleet needs it. |
| Live-verification scanners (trufflehog) | rejected by § 2 | Network-on-write hot path is unfit. |

## 12. Consequences

### 12.1 For the rest of the research epic

- **P2** can size the inline tier knowing scanning happens on the stored bytes (not on the original — the stored bytes are the redacted version). `blob_sha` keys content-addressed dedup on the *stored* (post-redaction) hash, so two redacted blobs with the same redaction pattern dedup correctly; an unredacted blob and its redacted twin do not.
- **P4** (CLI/API) inherits the `--no-redact` flag, the `--reason` requirement, and the role check at the CLI layer. The API contract is: client sends content, server scans, server writes redacted (default) or original (`--no-redact` + role + reason). The API never returns the original after redaction.
- **P6** (embedding) embeds `content` after redaction. Embeddings of `<REDACTED:rule:fp>` placeholders carry no secret information, so the embedding pipeline does not need a secrets gate.
- **P7** (lifecycle) carries `secrets_redacted=true` blobs through the same orphan GC pathway. No special purge.
- **P8** (bridge) writes through the scanner. Bridge batch-registration of an MR's files is rate-limit-eligible (§ 8); P8 specifies the bridge's authentication identity and any backfill exemption.
- **P9** (implementation) registers the additional fields locked here (`original_blob_sha`, `audit_bead_id`, `pre_retro_redact_blob_sha`) on `type:git-blob` alongside the P0 set, embeds the gitleaks library, wires the create handler, builds the `kd admin git-blob retro-redact` tool, and adds the rate-limit middleware.

### 12.2 For operators

- A new bead type whose `content` is guaranteed to have been scanned with the same gitleaks config the rest of the fleet trusts.
- A small new query surface: `kd list --type=git-blob -f secrets_redacted=true` and the audit-bead stream for `--no-redact` overrides.
- One new admin role check (`gardenio-admin` for `--no-redact`); no new admin role to provision.
- One new admin tool (`kd admin git-blob retro-redact`) for rule-update sweeps.

### 12.3 For the type registration

P0 § 7.2 sketched the `type:git-blob` field set. P3 adds (P9 will register them):

```jsonc
{"name": "original_blob_sha",          "type": "string"},  // pre-redaction SHA-1 when known
{"name": "audit_bead_id",              "type": "string"},  // links to type:audit row when --no-redact
{"name": "pre_retro_redact_blob_sha",  "type": "string"}   // one-shot history when retro-redact replaced content
```

`secrets_summary` (already on P0's list as `json`) is given the schema in § 4.1; the type-config registration does not need to change to encode the schema (`json` is free-form), but the create handler validates the shape on writes from the server side.

### 12.4 Cost / risk

- Gitleaks scan cost: bounded by file size; for a 64KB file, microseconds. Negligible per-write overhead.
- Pepper management: the per-deployment fingerprint pepper must be a stable secret, recorded in the deployment's secret store. Losing the pepper is non-fatal (only fingerprint correlation breaks; redactions still hold). This is a one-line deployment requirement; flagged for P9 / Helm chart.
- Scanner false negatives: the residual risk the threat model already acknowledges. Mitigated by retro-redact and the curated `.gitleaks.toml`. Not eliminable.
- Scanner false positives: a benign file that the scanner over-flags becomes a partly-redacted blob. The `[allowlist]` in `.gitleaks.toml` is the operator's tuning surface; same workflow as today.

## 13. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The accompanying `gb decision` (D-6) is resolved by the operator either accepting the recommended defaults (gitleaks; redact-not-reject; admin `--no-redact` with audit) or directing a specific override.
3. P4 (CLI/API) and P9 (implementation) can begin against the contract locked here.

This ADR is **not** the green light to embed gitleaks in kbeads or to add the create-handler scan logic. That is P9, after the operator authorizes the implementation epic per plan-of-record § 11.

## 14. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (§ 8 security, § 10 D-6).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442).
- Existing gitleaks config: `.gitleaks.toml` (repo root).
- Existing gitleaks invocations: `scripts/hooks/pre-commit`, `.rwx/ci.yml` (`gitleaks` task).
- Audit-type precedent: `kbeads/internal/server/config.go` (`type:audit`, `min_role: gardenio-admin`).
- Event-recording contract: `kbeads/CLAUDE.md` § "Landing the plane" (every mutation calls `store.RecordEvent` + publisher).
- Companion phases:
  - P1 (kd identity / dedup) — open, depends on P0.
  - P2 (ka-0000004323 storage tiering) — open, in progress.
  - P4–P8 — to be scheduled after P0–P3 land.
