# ADR: `git-blob` Bead Type — Ingestion CLI and API

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p4-gitblob-spec-is8
**Phase**: P4 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000004396
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (MR !1973), §7 (ingestion).
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks type name `git-blob`, `kind: data`, the six required fields, project-scoped write policy.
- P1 ADR `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044) — locks the `(repo, commit_sha, path)` natural key, server-side `repo` normalization (§2.4), random-nanoid IDs with a partial unique index, upsert-on-conflict semantics, and the `blob_sha` mismatch → 409 contract.
**Resolves**: no D-* number; spec for the ingestion surface that P9 will implement.
**Defers**: storage placement of bytes (P2 / D-4), secret-scanner & `--no-redact` mechanics (P3 / D-6), graph-edge wiring (P5 / D-7), embedding eligibility (P6 / D-8), bridge auto-registration (P8). This ADR refers to those phases when the CLI/API surface touches them but does not re-decide them.

## TL;DR

P4 specifies the **ingestion surface** for `git-blob` — the `kd git-blob add` command that agents and operators use to register one snapshot, plus the matching gRPC RPC and REST endpoint. Concretely:

- A new top-level CLI command group `kd git-blob` with one verb today (`add`) and room for `show`, `ls`, `diff` later. `add` takes `--repo`, `--commit`, `--path`, optional `--from-file` for offline/agent-local content, optional `--source` to pick a fetch strategy, optional provenance flags, and idempotence-control flags (`--if-absent`, `--force-content-mismatch`).
- Four **fetch strategies**, in this priority order when no explicit `--source` is given: (1) `--from-file` if the operator supplied a body; (2) **local clone** when the working directory is inside a clone of the named `repo` and the commit is reachable; (3) **GitLab Files API** when `repo` resolves to a GitLab project the daemon has a token for; (4) **GitHub Contents/Git Data API** when `repo` resolves to GitHub. The first strategy that returns bytes wins; later strategies are not consulted. Failures within a strategy fall through to the next strategy by default; `--source <name>` pins to a single strategy and surfaces its error.
- **Idempotence semantics** are inherited from P1 §3.4: register-by-triple is an upsert. The CLI exposes the dedup result as a `dedup` field in JSON output and a one-line stderr note in human output. A re-registration with a different `blob_sha` for the same triple returns **409 Conflict** by default (P1 §3.5). `--force-content-mismatch` (admin-only, audit-logged via P3 mechanics) tells the server to accept the new bytes anyway; that flag intentionally piggybacks on the same admin path P3 designs for `--no-redact`.
- A new **gRPC RPC** `RegisterGitBlob(RegisterGitBlobRequest) returns (RegisterGitBlobResponse)` is added next to `CreateBead` rather than overloading `CreateBead`, because it has materially different semantics (server-side fetch, server-side `blob_sha` computation, upsert-on-conflict, scanner integration) and the response includes a `dedup_status` field that does not belong on `CreateBead`. The matching REST endpoint is `POST /v1/git-blob:register` with a JSON body that mirrors the proto.
- The **request body never carries content** when the daemon can fetch it. `--from-file` is the only path that puts bytes on the wire. This keeps the audit log small, keeps the request idempotent without hashing bodies on every retry, and concentrates secret scanning on the daemon (which already has the gitleaks config from P3).
- This ADR ships **no code, no proto regeneration, no migration**. P9 is the only phase that ships any of those.

The single design fork P4 surfaced — whether the RPC is a new method or a `CreateBead` overlay (§4.5) — was resolved by the operator on 2026-05-03 via decision `ka-0000003600`: **dedicated RPC**.

## 1. Context

### 1.1 What the plan-of-record asks for

`kbeads/docs/research-git-blob-bead-type.md` §7 sketches the ingestion surface:

> ```sh
> kd git-blob add \
>   --repo gitlab.com/example-org/gardenio \
>   --commit deadbeef0123... \
>   --path internal/server/config.go \
>   [--from-file path/on/disk]                 # local content; else fetched
>   [--source gitlab|github|local]             # provider hint
>   [--origin-bead ka-X]                       # bead this blob derives from
>   [--mr ka-0000004786]                       # link to MR/issue
>   [--branch feat/xyz]                        # context
>   [--no-redact]                              # disable secret scan (admin)
> ```
>
> Fetch strategies:
> - **Local clone:** if `cwd` is a clone of the repo, read directly.
> - **GitLab API:** `GET /projects/{id}/repository/files/{path}/raw?ref={sha}` — no clone needed.
> - **GitHub API:** `GET /repos/{owner}/{repo}/contents/{path}?ref={sha}` (≤1MB) or git data API (larger).
> - **Provided file:** `--from-file` for offline registration.
>
> Output: bead ID + dedup status (`created` vs `existing`).

P4 turns that sketch into a spec: the exact flag set, exit codes, the precedence rule when more than one strategy could fire, the gRPC and HTTP shapes, and the contract for retries and idempotence.

### 1.2 What P0 and P1 already decided

This ADR builds on (and does not relitigate):

- **P0** locked the type name `git-blob`, `kind: data`, the six required fields (`repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`), and the project-scoped write policy. The CLI must elicit the first three and let the server compute the latter three.
- **P1** locked `repo` normalization rules (P1 §2.4), the optional `repo_id` field, the partial unique index on `(LOWER(repo), commit_sha, path) WHERE type='git-blob' AND deleted_at IS NULL`, and the upsert-on-conflict contract. The CLI/API must surface the **upsert result** to callers (`created` vs `existing`) and must propagate the **`blob_sha` mismatch → 409** contract from P1 §3.5.

### 1.3 What P3 already decided

P3 locked the secret-scan path: every registration runs through gitleaks server-side, redaction is in-place, and `--no-redact` is an admin-gated override that does not skip scanning. P4 adopts that flag verbatim — it is the only "admin" knob on `git-blob add`. P4 also reuses the same admin-override pattern for `--force-content-mismatch` (§5.3), so operators who hold `gardenio-admin` see one consistent override surface.

### 1.4 Existing CLI and RPC patterns this ADR matches

Looking at how comparable surfaces land in the codebase today:

- **CLI grouping**: top-level `kd <noun> <verb>` is the established shape (`kd advice add|list|show|remove`, `kd formula apply|cook|pour|wisp`, `kd admin reextract-references`). Each noun has a Cobra parent in `kbeads/cmd/kd/<noun>.go` plus one verb-file per sub-command. P4 places `git-blob.go` (parent) + `git_blob_add.go` (verb) in that pattern.
- **CreateBead RPC** (`kbeads/proto/beads/v1/beads.proto:10-29`) is a generic create with a `bytes fields` blob and a free-form `type` string. It is the right primitive for *most* type writes but the wrong primitive for `git-blob` because (a) `blob_sha`, `file_size`, and `encoding` are server-computed not client-supplied, (b) the daemon may need to fetch bytes from a provider, (c) the response must distinguish `created` vs `existing`, (d) a `blob_sha` mismatch must surface as 409. Wedging all of this into `CreateBead` would either pollute that RPC for every other type or require a dedicated `type: git-blob` branch inside the generic handler. A dedicated RPC is cleaner.
- **Admin-gated commands** (`kbeads/cmd/kd/admin_*.go`) demonstrate the pattern for `--no-redact` and `--force-content-mismatch`: a flag, a server-side role check via interceptor, and an `audit`-kind bead emitted on success. P4 reuses this pattern unchanged.

### 1.5 Why a single ADR for CLI + RPC

The CLI surface and the RPC surface are duals: the CLI flag set drives the RPC field set, and vice versa. Splitting them would force constant cross-references and create an opportunity for the two to drift (a flag that doesn't map to a field, or a field with no flag). Keeping them in one ADR is the cheaper read.

## 2. Decision: CLI surface

### 2.1 Command tree

```text
kd git-blob              — parent command (no-op; lists sub-commands)
kd git-blob add          — register one (repo, commit, path) snapshot
# Reserved for later phases (NOT in P4 scope):
#   kd git-blob show <id>           — pretty-print one blob (P5/P6 may add)
#   kd git-blob ls                  — convenience wrapper around `kd list --type=git-blob` (P5)
#   kd git-blob diff <a> <b>        — diff two blob beads (out of scope per plan-of-record §13)
```

P4 ships **only** the `add` verb. Reserved verbs are listed so the command tree's growth path is intentional, not so they ship now. The eventual P9 implementation registers the parent + `add` and stops.

### 2.2 `kd git-blob add` flag reference

| Flag | Type | Required | Default | Purpose |
|---|---|---|---|---|
| `--repo <string>` | string | yes | — | Repo identifier. Server normalizes per P1 §2.4 before lookup/insert. Accepts any of the input forms in P1 §2.4 (URL, SSH, normalized). |
| `--commit <sha>` | string | yes | — | 40-char lowercase hex git commit SHA. CLI rejects short SHAs locally with a clear message before any RPC. |
| `--path <posix>` | string | yes | — | POSIX path relative to repo root. CLI rejects leading `/`, `..` segments, and non-UTF-8 locally. |
| `--from-file <path>` | string | no | — | Read content from a local file instead of fetching. Mutually exclusive with `--source` values that fetch. When set, CLI hashes bytes locally for a sanity check but the server still recomputes `blob_sha` of record. |
| `--source <name>` | enum (`local`, `gitlab`, `github`, `from-file`) | no | unset (= auto, see §3.2) | Pin to one fetch strategy. When set, the daemon does **not** fall through to other strategies; it returns the strategy's error. |
| `--repo-id <provider:id>` | string | no | — | Optional rename-stable secondary identifier (P1 §2.6). Stored as the optional `repo_id` field. CLI does not auto-resolve; that is a future ergonomic. |
| `--branch <ref>` | string | no | — | Branch the snapshot was registered from. Stored as provenance only — not part of identity. |
| `--origin-bead <ka-id>` | string | no | — | Bead this blob derives from (typically the originating issue/MR bead). Stored as the `origin_bead` field. Used by P5 for graph wiring. |
| `--mr <ka-id-or-url>` | string | no | — | Convenience: when given a `kd-` ID, CLI sets `origin_bead` if not already set; when given a GitLab URL, CLI extracts the MR IID and stores it in `gitlab_mr_iid`. Mutually compatible with `--origin-bead`; conflicting `--origin-bead` takes precedence. |
| `--commit-message <text>` | string | no | — | Optional. CLI does not auto-resolve from a local clone; an agent registering many blobs at one commit is expected to set it explicitly to avoid N git-show calls. |
| `--commit-author <text>` | string | no | — | Optional, `Name <email>` form. Same posture as `--commit-message`. |
| `--source-tag <enum>` | enum (`agent-cli`, `manual`) | no | `agent-cli` | The `source` provenance field. The CLI cannot legitimately set `gitlab-bridge` or `backfill` (those are server-side identities), so the flag's domain is restricted. |
| `--label <key=value>` | string (repeatable) | no | — | Project / topic labels to attach to the bead. Same shape as `kd create --label`. The server enforces project-scoped write policy against these. |
| `--if-absent` | bool | no | `false` | When set, the command exits **0 and prints nothing** if the triple already exists, and exits **0 with the new bead ID** if it was created. Useful in scripts that want "ensure registered" semantics without parsing dedup output. |
| `--force-content-mismatch` | bool | no | `false` | Admin-only. When the server detects `blob_sha` differs from the previously-stored content for the same triple, accept the new content (mutating the bead). Without the flag, the server returns 409. Audit-logged. See §5.3. |
| `--no-redact` | bool | no | `false` | Admin-only. P3's existing override; P4 does not redefine it. Audit-logged. |
| `--reason <text>` | string | conditional | — | Required when `--force-content-mismatch` or `--no-redact` is set. Stored on the audit-kind bead. |
| `--output <fmt>` | enum (`human`, `json`, `id`) | no | `human` | Output format. `id` prints just the bead ID and a newline (script-friendly). |
| `--dry-run` | bool | no | `false` | Compute the full request, run client-side validation, but do not call the RPC. Print what would be sent. |
| `--server <addr>` | string | no | env `BEADS_SERVER` | Standard kbeads CLI flag. Mentioned for completeness; not new. |

### 2.3 Mutually-exclusive and conditional rules (CLI-side)

| Rule | Behavior |
|---|---|
| `--from-file` set ⇒ `--source` may only be unset or `from-file`. Any other `--source` value with `--from-file` is a CLI error before RPC. | Prevents the contradictory "fetch from GitLab AND from-file" call. |
| `--source from-file` ⇒ `--from-file` must be set. | Makes the explicit form symmetric with the implicit one. |
| `--force-content-mismatch` or `--no-redact` ⇒ `--reason` must be non-empty. | Audit-bead has somewhere to record intent. |
| `--commit` must match `^[a-f0-9]{40}$` client-side. | Avoids a round-trip to learn the server rejected a short SHA. |
| `--path` rejects `..`, leading `/`, NUL bytes, non-UTF-8. | Mirrors P1 §4.3 server-side checks; same message. |
| `--if-absent` and `--dry-run` are independent (both can be set). | No contradiction. |

### 2.4 Output contracts

#### 2.4.1 `--output human` (default)

```
ka-0000002859  created  blob_sha=ad5b…  size=4321  encoding=text  source=local
```

One line. Six fields: id, dedup status, blob_sha (12-char prefix), size in bytes, encoding, fetch-strategy actually used. On `existing`, the line is identical except for the dedup token; the bead ID is the existing bead's ID. On `--force-content-mismatch` accept, the dedup token is `mutated`.

If the scanner redacted secrets, a second line follows:

```
secrets_redacted=true  rules=aws-access-key,generic-api-key  count=2
```

If the daemon truncated content (P2), a similar `truncated=true bytes=…` line follows.

#### 2.4.2 `--output json`

```json
{
  "bead_id": "ka-0000002859",
  "dedup": "created",
  "blob_sha": "ad5b1f6a97b1e1b9c0c4a3a6a3e4e5b6c7d8e9f0",
  "file_size": 4321,
  "encoding": "text",
  "source_used": "local",
  "secrets_redacted": false,
  "secrets_summary": null,
  "truncated": false,
  "warnings": []
}
```

`dedup` is one of `created` | `existing` | `mutated`. `source_used` is the strategy the daemon picked. `warnings` is a string list (e.g. `"path-case mismatch with stored bead"` when a soft-warn fires).

#### 2.4.3 `--output id`

Just the bead ID, then newline. Empty stdout when `--if-absent` and the bead already existed and the operator only wanted "ensure"; in that case exit 0 with no stdout output.

### 2.5 Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (`created`, `existing`, or `mutated`). For `--if-absent`, 0 always when no error. |
| 2 | Client-side validation error (bad SHA, bad path, contradictory flags). |
| 3 | Triple-already-exists *and* server returned 409 because client supplied a different `blob_sha` and `--force-content-mismatch` was not set. (See §5.3.) |
| 4 | All fetch strategies failed (local clone unreachable, no GitLab/GitHub token, no `--from-file`). Stderr lists each strategy's reason. |
| 5 | Server returned a redaction-policy error (e.g. `--no-redact` requested without admin role). |
| 6 | Authorization error (project-scoped write policy denied). |
| 7 | Daemon unreachable or RPC transport error. |

A single dedicated exit code per "shape of failure" lets agent harnesses act on `add` failures without parsing stderr.

### 2.6 Examples

**Common case — agent registers a file from its own clone:**
```sh
kd git-blob add \
  --repo gitlab.com/example-org/gardenio \
  --commit 1234567890abcdef1234567890abcdef12345678 \
  --path kbeads/internal/server/config.go \
  --origin-bead ka-issue123 \
  --output id
```
Daemon picks `local` (cwd is a clone). On success, prints `ka-0000002859`.

**Offline — agent has the file but no network:**
```sh
kd git-blob add \
  --repo gitlab.com/example-org/gardenio \
  --commit 1234567890abcdef1234567890abcdef12345678 \
  --path kbeads/internal/server/config.go \
  --from-file ./config.go \
  --source from-file \
  --origin-bead ka-issue123
```

**Forced re-write after a known-bad redaction (admin):**
```sh
kd git-blob add \
  --repo … --commit … --path … \
  --no-redact \
  --reason "compliance: legal needs original token for incident X-2026-04"
```

**Ensure-registered idempotent script:**
```sh
kd git-blob add --repo … --commit … --path … --if-absent --output id || exit $?
```

### 2.7 What the CLI does NOT do

- The CLI does **not** clone repos. If `--source local` is requested and the cwd isn't inside a clone of the named repo, fail fast with exit code 4 and a message pointing at `--from-file` or another `--source`.
- The CLI does **not** read or store provider tokens. Tokens for GitLab/GitHub fetch live on the *daemon*, not the client. The CLI only carries the request.
- The CLI does **not** auto-resolve `repo_id` from the GitLab API. Operators who need it pass `--repo-id`; future ergonomics may add a resolver but P4 does not.
- The CLI does **not** support batch input (e.g. `kd git-blob add --from-stdin manifest.json`). Bridge auto-registration (P8) is a different surface; the manual single-shot CLI is intentionally scoped narrow.
- The CLI does **not** invoke a local secret scan. The server is the only authoritative scanner (P3 §2). A local pre-flight scan would only add a false sense of safety; the daemon's scan is what matters.

## 3. Decision: fetch strategies

### 3.1 The four strategies (recap from plan-of-record §7.1)

| Strategy | Source of bytes | Where it runs |
|---|---|---|
| `from-file` | `--from-file` argument from the client. CLI reads the bytes and includes them in the request body. | Client (CLI). |
| `local` | The daemon shells out to `git cat-file -p <commit>:<path>` against a daemon-side clone of `repo`, when one exists. | Daemon. |
| `gitlab` | Daemon calls `GET /api/v4/projects/<urlencoded-path-or-id>/repository/files/<urlencoded-path>/raw?ref=<sha>` with its configured token. | Daemon. |
| `github` | Daemon calls `GET /repos/<owner>/<name>/contents/<path>?ref=<sha>` for content ≤1MB, or the git-data blob API for larger files. | Daemon. |

The plan-of-record §7.1 originally framed `local` as "if cwd is a clone of the repo, read directly". P4 moves that to the **daemon** rather than the CLI: only the daemon can vouch for what content was used (the audit trail is server-side), and only the daemon has the secret-scanner. A "client reads from cwd" path would either bypass the scanner or run two scans. Scanning once on the server is cleaner.

If a future use case really needs "client reads from its own checkout", `--from-file` already covers it: the CLI just runs `git show <sha>:<path>` itself and pipes into `--from-file <path>`. That pattern is documented in §3.5.

### 3.2 Auto-priority when `--source` is unset

When `--source` is not specified, the daemon tries strategies in this priority order:

1. **`from-file` if the client supplied bytes.** The client volunteered the content; it is authoritative for *this* registration. Other strategies are not consulted.
2. **`local` if a daemon-side clone of `repo` exists and `commit` is reachable.** Lowest latency, no API call. Skipped silently if no clone or commit not reachable.
3. **`gitlab` if `repo` host matches `gitlab.com` or a configured private GitLab instance, and the daemon has a token.** Skipped silently if no token configured for the host.
4. **`github` if `repo` host matches `github.com`, and the daemon has a token.** Same skip rule.

If all strategies are skipped or fail, the daemon returns a structured error listing each strategy's outcome (`skipped: no clone configured`, `failed: 401 from gitlab`, `failed: 404 from github`). The CLI surfaces this as exit code 4.

### 3.3 When `--source` is explicit

When `--source <name>` is supplied:

- The daemon tries **only** that strategy.
- If it skips (e.g. `--source github` but no GitHub token), the call fails with a structured error.
- If it fails (e.g. `--source gitlab` and the API returned 404), the daemon does not fall through; the call fails with the strategy's error.

This makes "try only this one path" possible for diagnosis and for scripts that don't want surprise fallbacks.

### 3.4 Configuration surface for daemon-side fetch

P4 does **not** invent a new config bead for fetch tokens. The daemon's existing config conventions apply:

- **GitLab token**: env `GITLAB_TOKEN` plus an optional config-bead pointer for per-host tokens (e.g. a `gitlab-fetch-token:<host>` config bead). The exact config-bead name is P9's call; this ADR only asserts that GitLab token storage **does not** invent a new mechanism.
- **GitHub token**: env `GITHUB_TOKEN` with the same per-host extension story.
- **Local clones**: a config bead `git-blob-fetch-clones` whose value is a JSON map `{"<repo>": "<absolute-path-on-daemon>"}`. The daemon checks the path is a clone of `<repo>` (via `git remote get-url origin`) on startup and again per-request. P9 designs the exact config bead shape; this ADR only asserts that **the daemon does not auto-discover clones** — the operator wires them explicitly.

If no fetch token / no clone is configured for a `repo`, the only path that works is `--from-file`. That is fine: the type is opt-in, and operators who don't wire tokens get a clear error pointing at `--from-file`.

### 3.5 Fetch-strategy error matrix

| Strategy | Common failure | How daemon handles | Operator remediation |
|---|---|---|---|
| `from-file` | Path missing / unreadable | CLI fails before RPC (exit 2). | Re-run with valid path. |
| `from-file` | File too large for size cap (P2) | Server rejects with 413. | Re-run without `--from-file`; let the daemon fetch (the daemon may serve a `content_ref` for >1MB via P2's tiering). |
| `local` | No clone configured for `repo` | Skipped silently in auto-priority; explicit `--source local` errors with "no clone configured for <repo> on the daemon" (exit 4). | Wire a clone or use a different strategy. |
| `local` | Commit not reachable in clone | Skipped silently in auto-priority; explicit errors with "commit <sha> not in daemon clone of <repo>; remote may need a fetch" (exit 4). | Operator-side fetch on the clone, or use `--source gitlab`. |
| `gitlab` | 401 from GitLab API | Auto-priority falls through to `github`; explicit errors with the API's reason. | Check token; check token's project access. |
| `gitlab` | 404 (file not at that commit) | Auto-priority **does not** fall through (a 404 means the file truly is missing, not that this strategy is unavailable). Returns 404 to client. | Check `commit`/`path`. |
| `gitlab` | 502/503 transient | Auto-priority falls through to `github`. | None; daemon will succeed on a retry. |
| `github` | 404 | Same as gitlab 404 — final, do not fall through. | Check `commit`/`path`. |
| `github` | 422 (file too large for Contents API) | Daemon transparently retries via the git-data blob API. | None. |

The "404 ⇒ stop" rule (rather than "404 ⇒ try the next strategy") matters: silently falling through on a 404 would mask a typo in `--commit` or `--path` by trying the next provider, which would also 404 and produce a much worse error message. Surface the first authoritative "this file is not here" instead.

### 3.6 What this ADR does NOT do for fetch

- Does **not** lock a maximum content size — that is P2's call (D-4). The CLI/API surface is content-size-agnostic; the daemon uses P2's tiering when storing, and may return `encoding=omitted` when content exceeds the agreed cap.
- Does **not** specify token rotation, secret-scanned tokens, or per-fleet token isolation — those are operator-deployment concerns shared with every other daemon-side credential.
- Does **not** add a `--depth=full` style "fetch the whole tree" flag. P4's CLI is one-blob-per-call.

## 4. Decision: gRPC RPC shape

### 4.1 New RPC, not a `CreateBead` overlay

P4 adds one new RPC to `BeadsService`:

```protobuf
// (sketch — actual proto lands in P9)
rpc RegisterGitBlob(RegisterGitBlobRequest) returns (RegisterGitBlobResponse);
```

### 4.2 `RegisterGitBlobRequest`

```protobuf
message RegisterGitBlobRequest {
  // Identity (P1 §2). Server normalizes `repo` per P1 §2.4 before lookup.
  string repo        = 1;
  string commit_sha  = 2;
  string path        = 3;

  // Optional rename-stable secondary identifier (P1 §2.6).
  string repo_id     = 4;

  // Optional client-supplied content. When set, server uses it as the source
  // of bytes regardless of `source`. Mutually exclusive with `source` values
  // other than "from-file" or unspecified.
  bytes  content     = 5;

  // Fetch strategy. One of "" (auto), "from-file", "local", "gitlab", "github".
  // When "from-file", `content` MUST be set.
  string source      = 6;

  // Provenance. All optional. Free-form on the wire; server applies
  // P0/P1/P3 validation.
  string branch          = 7;
  string commit_message  = 8;
  string commit_author   = 9;
  string commit_committer = 10;
  google.protobuf.Timestamp committed_at = 11;
  string parent_commit_sha = 12;
  string prior_blob_id     = 13;
  string gitlab_project_id = 14;
  string gitlab_mr_iid     = 15;
  string origin_bead       = 16;

  // Provenance: who is asking. Domain restricted to {"agent-cli", "manual"}
  // when supplied by clients; the bridge sets "gitlab-bridge" via the same RPC.
  // Server overrides if the caller's identity doesn't match the claim.
  string source_tag = 17;

  // Labels to attach (project scope, topic).
  repeated string labels = 18;

  // Idempotence / conflict knobs.
  bool   if_absent              = 19;  // see §2.4 / §4.4
  bool   force_content_mismatch = 20;  // admin-only; see §5.3
  bool   no_redact              = 21;  // admin-only; see P3
  string reason                 = 22;  // required when force_* / no_redact

  // Created-by override. Falls back to authenticated identity.
  string created_by = 23;
}
```

### 4.3 `RegisterGitBlobResponse`

```protobuf
message RegisterGitBlobResponse {
  // The bead — newly-created, existing, or (when force_content_mismatch=true)
  // mutated. Always populated on a 2xx / OK response.
  Bead bead = 1;

  // Dedup result.
  enum Dedup {
    DEDUP_UNSPECIFIED = 0;
    DEDUP_CREATED     = 1;  // new row; new ID
    DEDUP_EXISTING    = 2;  // triple already had a bead; same ID returned
    DEDUP_MUTATED     = 3;  // existing bead, content rewritten under force_content_mismatch
  }
  Dedup dedup = 2;

  // Fetch strategy actually used. One of from §3 plus "skipped" (only valid
  // with DEDUP_EXISTING when the daemon short-circuited because the triple
  // was already known). Servers may set this empty if not tracked.
  string source_used = 3;

  // Mirror of the bead's content fields, for clients that don't want to
  // re-parse the Bead.fields blob. Filled even on DEDUP_EXISTING.
  string blob_sha          = 4;
  int64  file_size         = 5;
  string encoding          = 6;
  bool   secrets_redacted  = 7;
  // secrets_summary is on Bead.fields; not re-mirrored here.
  bool   truncated         = 8;

  // Soft-warn list (path-case mismatch, provider-redirect, etc.). Non-fatal.
  repeated string warnings = 9;
}
```

### 4.4 Idempotence / conflict semantics on the wire

| Server-side state | Request shape | gRPC outcome |
|---|---|---|
| Triple does not exist | Any valid request | `OK`, `dedup=DEDUP_CREATED`. New bead. |
| Triple exists, fetched bytes match stored `blob_sha` | Any valid request | `OK`, `dedup=DEDUP_EXISTING`. Same bead returned. Provenance fields may be merged per P1 §3.5 (last-writer-wins on provenance only). |
| Triple exists, fetched bytes differ from stored `blob_sha`, `force_content_mismatch=false` | — | `FAILED_PRECONDITION` (HTTP 409). Response body carries existing bead's ID and stored `blob_sha` for diagnosis. |
| Triple exists, fetched bytes differ, `force_content_mismatch=true`, caller has admin | — | `OK`, `dedup=DEDUP_MUTATED`. Bead's content fields rewritten in place. Audit-kind bead emitted with `reason`. |
| Triple exists, `force_content_mismatch=true`, caller lacks admin | — | `PERMISSION_DENIED` (HTTP 403). |
| `if_absent=true`, triple exists | — | `OK`, `dedup=DEDUP_EXISTING`. CLI prints nothing for `--output id`. |
| `if_absent=true`, triple does not exist | — | `OK`, `dedup=DEDUP_CREATED`. CLI prints the new ID. |

`if_absent` is purely a CLI-output knob — it does not change RPC semantics, only how the CLI renders the `dedup` field. Including it as a request field (rather than a CLI-only thing) lets future clients (e.g. a hypothetical web UI) reproduce the behavior without reimplementing it.

### 4.5 Why a dedicated RPC and not a `CreateBead` overlay

Two designs were considered:

**Option A — dedicated RPC `RegisterGitBlob`.** ★ chosen.
- Pros: clean signature; response carries `dedup_status`, `source_used`, mirror fields without polluting `CreateBeadResponse`. The server-side handler can run the daemon-side fetch + scanner pipeline without "if type == 'git-blob'" branching inside `CreateBead`. Future operations (`UpdateGitBlobProvenance`?) live in the same dedicated namespace.
- Cons: one more proto message and handler to keep in sync with the type. Marginal proto growth.

**Option B — overlay on `CreateBead`.** Add `git_blob_options` (fetch source, force flags) to `CreateBeadRequest`; have the server detect `type == "git-blob"` and route into the dedicated handler internally; add `dedup_status` to `CreateBeadResponse` (defaulting to `DEDUP_UNSPECIFIED` for non-git-blob calls).
- Pros: one RPC. Looks "uniform".
- Cons: `CreateBeadRequest` accumulates type-specific fields it never had before (`from_file`, `force_content_mismatch`, `no_redact`); every other type-create has to ignore them. `CreateBeadResponse.dedup_status` is meaningless for every other type. The scanner / fetch pipeline becomes a hidden side-effect of `CreateBead`, surprising for any caller who expected `CreateBead` to be a thin write.

**Choice: Option A — confirmed by operator** (decision `ka-0000003600`, resolved 2026-05-03). The rest of the ADR assumes A. The CLI surface in §2 and the fetch behavior in §3 are unchanged either way because the CLI already abstracts the underlying RPC.

### 4.6 What this ADR does NOT do for the RPC

- Does not version the RPC behind a new package. It is added to the existing `beads.v1` service. P9 may decide to version-bump if the proto evolves; this ADR only asserts the *initial* shape.
- Does not specify streaming variants. A streaming "register many blobs" RPC is plan-of-record §7.2 / Phase P8 (bridge work), not P4.
- Does not specify a `DeregisterGitBlob` or content-erasure RPC. P7 (lifecycle) decides whether explicit content erasure is needed beyond soft-delete.

## 5. Decision: HTTP/REST shape

### 5.1 Endpoint

```
POST /v1/git-blob:register
Content-Type: application/json
```

The colon-suffix style mirrors gRPC HTTP transcoding conventions (and matches existing kbeads admin endpoints in pattern). The request body is the JSON form of `RegisterGitBlobRequest`; the response body is the JSON form of `RegisterGitBlobResponse`. Field names are snake_case, matching the rest of the kbeads HTTP surface.

### 5.2 HTTP status codes

| Status | Condition |
|---|---|
| 200 | `dedup ∈ {created, existing, mutated}`. |
| 400 | Client-side validation error reaching the server (e.g. malformed `commit_sha`). |
| 401 | No authenticated identity. |
| 403 | Project-scoped write denied; admin-only flag without admin role. |
| 404 | Fetch strategy got an authoritative "not found" from the upstream provider, OR (with `--source` pinned) the strategy is not available. |
| 409 | Triple exists, `blob_sha` mismatch, `force_content_mismatch=false`. Body includes existing bead ID + stored `blob_sha`. |
| 413 | Content too large per P2's cap. |
| 422 | Scanner ran, secret found, redact-not-allowed (e.g. some future "reject on secret" mode). For P3's redact-by-default the response is 200 with `secrets_redacted=true`. |
| 429 | Per-fleet rate limit (P3 §1.4). |
| 5xx | Server / fetch upstream error. |

The mapping back to CLI exit codes is in §2.5.

### 5.3 `--force-content-mismatch` audit story

This flag is the new admin override P4 introduces. Its rules:

- Required: `--force-content-mismatch` AND `--reason "<text>"` (≥10 chars, ≤500 chars).
- Caller must hold `gardenio-admin`. Server-side enforcement.
- On accept: `dedup=DEDUP_MUTATED`. The bead's content fields (`blob_sha`, `file_size`, `encoding`, `content` / `content_ref`, `truncated`, `language`, `mime_type`) are rewritten. **Identity fields stay immutable** — `repo`, `commit_sha`, `path` are not changed (you can't change identity by force; P1 §3.5 disallows it). Provenance fields do **not** automatically reset; the existing values remain unless the new request also supplies them.
- An `audit`-kind bead is emitted with: `actor` = caller, `action` = `git-blob.force-content-mismatch`, `subject` = the mutated bead's ID, `outcome` = `success`, `reason` = the supplied text, `prior_blob_sha` and `new_blob_sha` in fields. Same audit shape as P3's `--no-redact` path; P9 implements both off the same audit emitter.

Why an admin override at all: the 409-default protects against silent content drift (a provider returning different LF/CRLF, a scanner update changing what counts as a secret), but real-world drift does occur, and operators need a path to land the corrected snapshot without manually closing-and-recreating the bead. The flag's audit trail makes the override visible in the bead history.

### 5.4 What this ADR does NOT do for HTTP

- Does not specify content-type negotiation or protobuf-over-HTTP. JSON-over-HTTP is the existing kbeads convention; that holds.
- Does not specify webhook callbacks for "blob registered". Out of scope; bridge auto-registration (P8) is the place to design event surfaces.

## 6. Idempotence semantics — the full contract

This section consolidates the idempotence rules across CLI, gRPC, and HTTP into one place. It does not introduce new rules.

### 6.1 What a re-registration looks like

A "re-registration" is a `RegisterGitBlob` whose `(repo, commit_sha, path)` triple already maps to a bead.

| Caller-visible signal | When |
|---|---|
| Same bead ID returned, `dedup=existing`, no fields mutated. | The new request's content (after fetch + scan + canonical-encoding) hashes to the stored `blob_sha`. |
| Same bead ID, `dedup=existing`, provenance fields *may* merge (last-writer-wins). | Per P1 §3.5: only provenance fields (`commit_message`, `commit_author`, `branch`, `gitlab_mr_iid`, `origin_bead`, `source`) are upsert-friendly. |
| HTTP 409 / gRPC `FAILED_PRECONDITION`. | New content hashes differently AND `force_content_mismatch=false`. |
| Same bead ID, `dedup=mutated`, content fields rewritten, audit emitted. | New content hashes differently AND `force_content_mismatch=true` AND admin. |

### 6.2 What "the same content" means precisely

The server compares **`blob_sha`** values. `blob_sha` is the git blob SHA-1 of the **stored** content — i.e. after redaction (§ P3). Two registrations of the same triple with the same upstream bytes both produce the same `blob_sha` after the same scan, so they collapse to `existing`. A registration after a scanner-rule update may produce a *different* `blob_sha` because the redaction span changes; that surfaces as a mismatch (the operator's call whether to `--force` or to leave the older redacted snapshot in place).

P3 §6 may design a re-scan-on-rule-update job; that job's interaction with the 409 contract is P3's call, not P4's. P4 only asserts: the *client-driven* path treats `blob_sha` as the canonical content hash.

### 6.3 Concurrency

Two clients racing to register the same triple is fine: both reach the partial-unique-index check (P1 §4.1), one wins, the other gets the upsert path and `dedup=existing`. The losing client sees no error, sees the winning client's bead ID, and proceeds. The CLI tells the operator nothing changed.

### 6.4 Retries

`RegisterGitBlob` is **safe to retry** on transport error, on 5xx, and on `UNAVAILABLE`. A retry of the same request body either:
- Creates a bead (first attempt actually failed before the unique-index insert), or
- Returns `dedup=existing` for the bead the original attempt created (first attempt succeeded; the response was lost).

There is no idempotency token in the request — the natural-key triple **is** the idempotency token, by design.

### 6.5 Out-of-order registration

If two registrations interleave — e.g. client A starts a slow GitLab fetch, client B finishes a fast `--from-file` first — B's bead lands first; A's request finishes its fetch, sees the unique-violation, rereads B's bead, returns `dedup=existing`. A and B end up with the same `bead_id` and consistent state. The 409-on-mismatch only fires if A and B's *content* differs (which is then a real conflict, by the contract).

## 7. What this ADR does NOT decide

| Question | Owning phase |
|---|---|
| Where bytes physically live (inline / `git_blob_content` table / S3) | P2 / D-4 |
| Exact gitleaks rule set, redaction format, retro-scan policy | P3 / D-6 |
| `produces` / `derives-from` typed-mention verbs | P5 / D-7 |
| Whether `git-blob.content` is auto-embedded for pgvector | P6 / D-8 |
| Lifecycle / GC for orphan blobs | P7 |
| Bridge auto-registration on MR merge | P8 |
| Implementation patch (config.go entry, migration, handler wiring) | P9 |

P4 names the surfaces that touch each — the CLI flag for `--no-redact`, the `dedup_status` field, the `prior_blob_id` provenance — but does not relitigate the underlying decisions.

## 8. Consequences

### 8.1 For P9 implementation

P9 will:

1. Add `git-blob.go` and `git_blob_add.go` under `kbeads/cmd/kd/` matching the §2.2 flag set, with client-side validation matching §2.3.
2. Add `RegisterGitBlob` RPC + request/response messages in `kbeads/proto/beads/v1/service.proto` and `beads.proto`. Regenerate `gen/`.
3. Add HTTP route `POST /v1/git-blob:register` in `kbeads/internal/server/`.
4. Add a `git-blob` ingestion handler that:
   a. Normalizes `repo` per P1 §2.4.
   b. Validates `commit_sha`, `path`, `path` length / case per P1 §4.3.
   c. Picks a fetch strategy per §3.2 / §3.3.
   d. Runs the gitleaks scanner per P3 §2.
   e. Computes `blob_sha`, `file_size`, `encoding` from the (post-redaction) bytes.
   f. Routes content through P2's tiering for placement.
   g. Upserts via the partial unique index (P1 §4.1). On conflict-with-matching-`blob_sha`, returns the existing bead with provenance merged. On conflict-with-mismatching-`blob_sha`, returns 409 (or applies `force_content_mismatch` if admin).
   h. Emits an `audit`-kind bead when `force_content_mismatch` or `no_redact` was used.
   i. Records the standard mutation event (`store.RecordEvent` + `publisher.Publish`).
5. Add fetch-strategy clients:
   - `local` — `os/exec` against `git cat-file`.
   - `gitlab` — small `net/http` client with token.
   - `github` — small `net/http` client with token + the Contents/Git-Data dance for files >1MB.
6. Do **not** ship the partial unique index migration *here* — that lands as part of P9's P1-implementation deliverable. P4 only asserts the handler must obey the existing contract.

The CLI command tree, RPC name, and HTTP route are stable surfaces; P9 may rename internal helpers but should not rename any of the three.

### 8.2 For operators

- A `kd git-blob add` command with predictable flags. Auto-priority "just works" for the common case (agent in a clone, daemon has tokens). Explicit `--source` for diagnosis.
- One admin override (`--no-redact`) inherited from P3, plus one new admin override (`--force-content-mismatch`) for content drift, both audited the same way.
- `--if-absent` for "ensure registered" scripts; `--output id` for one-line bead ID extraction.
- No surprise: the daemon never registers a blob the operator did not ask for.

### 8.3 For agents

- One command per file the agent wants to attach to its work. The CLI is a thin wrapper around the RPC.
- Errors are exit-code-distinct, so an agent harness can branch on "fetch failed" vs "content drift" vs "auth denied" without parsing stderr.
- `--from-file` is always available as a fallback when the daemon can't reach the provider (offline agent, throttled token).

### 8.4 Cost / risk

- One new RPC and one new HTTP endpoint to maintain. Negligible.
- Risk: the daemon's fetch path becomes a credential consumer (GitLab/GitHub tokens). Mitigation: P4 reuses the daemon's existing token plumbing; no new secret-storage surface.
- Risk: auto-priority falls through silently on transient errors, giving operators a "sometimes I get GitLab, sometimes GitHub" experience. Mitigation: `source_used` is in every response; the CLI prints it; a script that wants determinism passes `--source`.
- Risk: `--force-content-mismatch` is misused (operator clicks past the 409). Mitigation: admin-only, requires `--reason`, audit-bead. Same shape as P3's `--no-redact`, so operators learn one pattern.
- Risk: the CLI grows. Mitigation: §2.1 explicitly reserves `kd git-blob ls|show|diff` for later phases and does not ship them now.

## 9. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The single fork in §4.5 (dedicated RPC vs `CreateBead` overlay) is resolved by the operator. **Resolved 2026-05-03 via decision `ka-0000003600`: dedicated RPC** (`RegisterGitBlob`).
3. P5, P6, P7, P8, P9 can begin (or continue) against the ingestion shape locked here.

This ADR is **not** the green light to ship the CLI, the RPC, or the HTTP endpoint. That is P9, after the operator authorizes the implementation epic per plan-of-record §11.

## 10. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (MR !1973, bead ka-0000003043). §7 (ingestion).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (MR !1976, bead ka-0000006442).
- P1 ADR: `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (bead ka-0000002044).
- P3 ADR: `kbeads/docs/adr/git-blob-P3-security-redaction.md` (bead ka-0000007332).
- gRPC service: `kbeads/proto/beads/v1/service.proto`, `kbeads/proto/beads/v1/beads.proto`.
- CLI patterns: `kbeads/cmd/kd/advice.go` (parent + verbs), `kbeads/cmd/kd/admin_*.go` (admin-gated commands).
- Type-config machinery: `kbeads/internal/model/type_config.go`. Built-in registrations: `kbeads/internal/server/config.go`.
- Audit-bead emission precedent: `kbeads/cmd/kd/admin_*.go` (uses the existing `audit`-kind type from `config.go`).
- GitLab Files API: `GET /api/v4/projects/:id/repository/files/:file_path/raw?ref=:ref`.
- GitHub Contents API: `GET /repos/:owner/:repo/contents/:path?ref=:ref`; Git Data Blob API: `GET /repos/:owner/:repo/git/blobs/:sha`.
