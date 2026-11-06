# Agent Container Image — Security Audit

**Date:** 2026-04-18
**Bead:** ka-0000000248
**Auditor:** gardenio-audit-agent-container-xsq
**Scope:**
`gardenio/images/agent/Dockerfile`, `entrypoint.sh`, `resolve-auth.sh` (+ `resolve-auth_test.sh`), `install/**`, `hooks/**`, `scenarios/**`, `channel/**`, and the K8s pod/RBAC/network policy artefacts that define how this image is actually deployed
(`gardenio/controller/internal/podmanager/manager.go`, `helm/gardenio/templates/controller/agent-{,cluster-}rbac.yaml`, `helm/gardenio/templates/networkpolicies/networkpolicies.yaml`).

The environment prevented live image pulls, so the `trivy`/`dive` dynamic evidence is stubbed out (see "Dynamic checks not run" below). Everything below is from static review of the sources that produce the image plus the K8s manifests that run it.

---

## Executive summary

The agent image is a **very fat, very privileged, internet-connected Ubuntu 24.04 workload** that
runs arbitrary LLM-generated code on behalf of users. The most pressing issues are not in
`entrypoint.sh` or `resolve-auth.sh` themselves — those are mostly well-written — but in the
**K8s privilege boundary around the pod**:

| # | Severity | Finding |
|---|---|---|
| C1 | **Critical** | Agent ServiceAccount is optionally bound to `cluster-admin` (`agent-cluster-rbac.yaml`); any tool-call RCE in any agent pod yields cluster-admin. |
| C2 | **Critical** | Agent ServiceAccount role is `apiGroups:["*"] resources:["*"] verbs:["*"]` across the whole namespace (`agent-rbac.yaml`) — agents can read every secret in the release namespace including other tenants' API keys. |
| C3 | **Critical** | `automountServiceAccountToken` is never set to `false` on the pod spec (`podmanager/manager.go:488-523`); `kubectl` is baked into the image; the two combine with C1/C2 to a trivial K8s takeover. |
| H1 | High | Pod has `NOPASSWD: ALL` sudoers + `AllowPrivilegeEscalation:true` + `Capabilities.Add:[SETUID,SETGID]` — agent can become root inside the container at will. |
| H2 | High | `jira-status-reset.sh` builds a `nohup bash -c "..."` string with unquoted shell-interpolated `${saved_status}`, `${ticket}`, `${transition_id}` coming from `/tmp/jira-pre-status/` files and from grepped command text. Command-injection sink. |
| H3 | High | Image install scripts pull ~20 binaries over HTTPS with **no checksum pinning** (coop, kd, kubectl, helm, gh, glab, terraform, docker, bun, uv, poetry, hadolint, rwx, k6, claudeless, quench, yq, stern, whisper-cpp source, Google Chrome via apt, playwright chromium). Any one takeover = image takeover. |
| H4 | High | `resolve-auth.sh._resolve_cred_secret` builds `/var/run/secrets/${secret_name}/token` from an account-bead field with no path-sanitisation. A beads-write-capable actor can point another project's agent at `.../token/../../another-secret/token`. |
| H5 | High | Prompt-injection: `check-mail.sh` / `check-jira.sh` emit `<system-reminder>` tags whose body embeds attacker-influenced bead titles. Any actor who can file a mail bead can inject arbitrary system-reminder content into a target agent's context. |
| M1 | Medium | `chmod -R 777 /ms-playwright` and `chmod -R a+rw $RUSTUP_HOME $CARGO_HOME`: world-writable toolchains persisted in the image. A co-tenant with shell access can patch `rustc`/`cargo`/playwright binaries for the next run. |
| M2 | Medium | `.credentials.json` is written to `$CREDS_PVC` (the PVC is mounted at 0755) without an explicit `chmod 0600`. |
| M3 | Medium | `gardenio-channel.ts` silently disables auth when `GARDENIO_CHANNEL_TOKEN` and `COOP_BROKER_TOKEN` are both unset (`TOKEN = "" → if (TOKEN) { ... }` skipped). Fail-open. |
| M4 | Medium | Agent NetworkPolicy egress is `0.0.0.0/0:443,80` except RFC1918 — the agent can still exfiltrate to arbitrary internet endpoints including link-local (`169.254.169.254` IMDS is NOT excluded). |
| M5 | Medium | Toolbelt contains `kubectl`, `aws`, `gh`/`glab`, `terraform`, `psql`, `docker`, `curl`, `ssh-client`, `ffmpeg` — far more than any single task requires. Drastically widens post-exploitation. |
| L1 | Low | `ssh-keyscan` at build-time trusts whatever DNS answered during `docker build`. No TOFU-style out-of-band verification of github.com/gitlab.com host keys. |
| L2 | Low | `entrypoint.sh` matches `"${BOAT_COMMAND:-}" \| grep -q "claudeless"` — substring match enables `--mock` flag on any command containing that word. Low exploit value (mock mode is less capable). |
| L3 | Low | `stop-gate.sh` uses `set -uo pipefail` without `-e`. Reads `$COOLDOWN_FILE` and `$BLOCK_COUNT_FILE` via `cat` without validating contents before arithmetic comparison; a corrupted file causes log spam but not RCE. |

---

## Per-file walkthrough

### `Dockerfile` (280 lines)

Multi-stage BuildKit build. Final stage `FROM base AS agent` installs the full toolchain.

**What's right**
- Multi-stage isolation of `gb-builder`, `go-tools`, `rust-tools`, `cli-tools`, `playwright-builder`, `whisper-builder`, `terraform-providers` so the final image doesn't carry all the build-time cruft in a single layer. ✓
- `USER 1000` at end of both `base` and `agent` stages. ✓
- `DISABLE_AUTOUPDATER=1` (documented in `node.sh`) prevents drift of the critical `claude-code` npm package at runtime. ✓
- TLS CA bundle env vars are set (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`). ✓
- `--no-install-recommends` used consistently. ✓

**What's wrong**
- Line 164: `echo 'agent ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/agent` — the UID-1000 agent is effectively root inside the container. Combined with `AllowPrivilegeEscalation:true` in the K8s pod spec, this is a hard no-go for a workload that runs LLM-authored shell. **[H1]**
- Line 171: `ssh-keyscan -t ed25519,ecdsa,rsa github.com gitlab.com > /home/agent/.ssh/known_hosts 2>/dev/null` — build-time DNS trust. If a build runner's DNS is poisoned, the fingerprints get baked into the image. **[L1]**
- Line 186: `KUBECTL_VERSION=$(curl -fsSL https://dl.k8s.io/release/stable.txt)` — always-latest, unpinned. Combined with no checksum check (**[H3]**), kubectl changes silently under you.
- The `agent` stage inherits `USER root` to install system packages, then `USER 1000`. Ok, but **every `COPY --from=<builder>` lands as root-owned into `/usr/local/...`**, which is fine. Problem is that `COPY --from=playwright-builder /ms-playwright/` is later `chmod -R 777` (see `playwright.sh` line 29) — that makes the browser binaries world-writable inside the image. **[M1]**
- `curl -fsSL https://install.python-poetry.org | ... python3 -` and `curl -fsSL https://bun.sh/install | ... bash` and `curl -LsSf https://astral.sh/uv/install.sh | ... sh` are classic "curl | bash" supply chain foot-guns. There is **no GPG signature or SHA256 check** on any of them. **[H3]**
- `gpg --dearmor -o /etc/apt/keyrings/google-chrome.gpg` followed by adding the Chrome repo: trust-on-first-fetch; the key itself is fetched by HTTPS from `dl.google.com` without pinning. Acceptable risk for Google, but we should document the SHA256.
- No `HEALTHCHECK`. The K8s manifest uses HTTP probes so this is informational only.

### `entrypoint.sh` (38 lines)

- `set -euo pipefail` ✓
- All expansions are quoted. ✓
- `COOP_MUX_SESSION_ID` and `COOP_LOG_LEVEL` default correctly.
- `rm -f /tmp/standby_active /tmp/standby_expired /tmp/standby_done` — idempotent cleanup. ✓
- `echo "${BOAT_COMMAND:-}" | grep -q "claudeless"` — substring match. If a non-mock command happens to contain the word "claudeless" in an argument, `--mock` is appended. Misuse case only. **[L2]**
- `exec gb init "${GB_INIT_FLAGS[@]}"` hands off cleanly to the Go binary for PID 1.

Verdict: thin and clean. No high-severity issues in this script.

### `resolve-auth.sh` (229 lines) & `resolve-auth_test.sh`

Sources OAuth/API credentials from three backends (coopmux HTTP, K8s secret mount, env var).

**What's right**
- Uses `set -euo pipefail` via `entrypoint.sh` when sourced (the script itself is sourceable).
- All env-var expansions quoted.
- Doesn't `eval` or `source` untrusted content — all parsing is through `jq -r`. ✓
- `_resolve_cred_env` uses `local env_val="${!var_name:-}"` (bash indirect expansion). `var_name` comes from the account bead's `credential` field which is a trust-boundary issue, not a shell-escape issue — `${!var_name}` only reads variables, cannot execute. OK.
- Test coverage covers env / coopmux / base_url paths. ✓

**What's wrong**
- `_resolve_cred_secret`: `local secret_path="/var/run/secrets/${secret_name}/token"`. `secret_name` arrives from `credential="secret:..."` on the account bead. Path traversal (`secret:foo/../../bar`) is not filtered. **[H4]** Mitigation today: only trusted actors can write account beads — but the audit's threat model lists "beads-write-capable inner attacker" so this is in scope. **Fix:** `[[ "$secret_name" =~ ^[a-z0-9-]+$ ]] || return 0`.
- `coopmux` path builds a JSON request body with the shell-interpolated `HOSTNAME` and `account`. Both are trusted today, but `jq -nc --arg ...` would be safer than string concatenation.
- `.credentials.json` is written by `echo "${mux_creds}" > "${CREDS_PVC}"` with no explicit `chmod 0600`. The path lives on a per-agent PVC that no other pod can mount, so blast radius is contained — but it's a missed hardening. **[M2]**
- `ACCOUNT_CREDS_SET=0` at module scope: if this file is sourced twice with different outcomes, the second failure doesn't re-zero the flag until line 24 runs. Low-severity ordering bug, not a security issue.

### `install/*.sh`

All 6 install scripts use `curl -fsSL | ... untar/bash` patterns with **zero checksum or GPG verification**:

| Script | Unverified downloads |
|---|---|
| `clis.sh` | kubectl, gh, glab, docker, docker-compose, AWS CLI zip, helm + plugins, terraform, terragrunt, uv, poetry, bun, yq, stern, hadolint, claudeless, rwx, k6, whisper.cpp source |
| `node.sh` | Node.js tarball + `claude-code` npm (npm uses its own SHA512, ok) |
| `go-tools.sh` | Go tarball, gopls (`go install`), golangci-lint (sha-verified release tarball), Task CLI |
| `rust-tools.sh` | archived rustup-init binary (sha-verified, per-release), quench tarball |
| `playwright.sh` | Playwright npm (ok), Chromium via `playwright install`, Google Chrome via apt |
| `terraform-providers.sh` | Terraform providers (terraform verifies provider signatures — ok) |

**This is the single largest attack surface of the image.** Any one compromise among ~15 vendors yields full code execution at build time, which becomes agent-side code execution everywhere. **[H3]**

Minimum fix: add `curl ... && echo "<sha256>  <file>" | sha256sum -c -` for every single download, with the expected hash pinned in `versions.env`.

### `hooks/*.sh`

| Hook | Issues |
|---|---|
| `check-mail.sh` | Reads mail beads via `kd list`, injects as `<system-reminder>` tag. `jq -r` unwraps raw bead titles/IDs into the reminder body. A malicious mail sender can embed `</system-reminder>` followed by their own forged reminder — **prompt-injection trust boundary break**. **[H5]** Fix: enforce `bullets=$(echo "$mail_json" \| jq -r '.[] \| "- \(.id) from \(.sender // "unknown"): \(.title \| gsub("[\\r\\n<>]"; "_"))"')`. |
| `check-jira.sh` | Same pattern, same vulnerability, same fix. **[H5]** |
| `drain-queue.sh` | Reads JSONL from `/tmp/inject-queue.jsonl`, echoes `.content` verbatim. If `check-mail.sh` is fixed, this inherits the fix. |
| `prime.sh` | Calls `gb prime` and `kd show`. Safe — those are the audited programs' own outputs. |
| `stop-gate.sh` | Complex but not injection-prone. Uses `cat "$COOLDOWN_FILE" 2>/dev/null \|\| echo 0` then `[ "$elapsed" -lt "$cooldown" ]`. If a co-tenant can write to `/tmp/stop-gate-last-block`, they can force the gate to exit 0 on every stop. Co-tenant write isn't possible today (per-pod `/tmp`), but **`/tmp` is shared between sidecars** — the DinD sidecar runs as root and could poison these files. **[L3]** |
| `wiki-sync.sh` | Uses `$GITLAB_TOKEN` via `PRIVATE-TOKEN` header. Correctly uses `--data-urlencode`. Project ID is hardcoded. OK. |
| `jira-status-save.sh` | Uses `/usr/bin/curl -sf -k` (`-k` = disable TLS cert verification). **Every JIRA API call skips TLS verification.** Needs justification or removal. |
| `jira-status-reset.sh` | `nohup bash -c "` (line 86) embeds `${JIRA_EMAIL}`, `${JIRA_API_TOKEN}`, `${JIRA_BASE_URL}`, `${ticket}`, `${saved_status}`, `${transition_id}` into a single-quoted shell string via shell interpolation. `ticket` is derived from `basename` of files in `/tmp/jira-pre-status/`, which match glob `PE-*` — today filenames are safe, but any future code path that writes arbitrary filenames there becomes an RCE. Also uses `-k` again. **[H2]** Fix: pass values as environment vars to the subshell (`JIRA_TICKET=$ticket JIRA_STATUS=$saved_status bash -c '...'`). |

### `scenarios/*.toml`

Claudeless test fixtures. All use `permission_mode = "accept-edits"` which, in **production**, would be catastrophic — but these files are mounted into `/scenarios/` and only executed when `BOAT_COMMAND` contains `claudeless`. In that mode the "agent" is the scenario runner itself, not Claude. Acceptable.

### `channel/gardenio-channel.ts`

MCP channel server bridging Slack↔Claude. Binds `0.0.0.0:8789`.

- **M3:** `const TOKEN = process.env.GARDENIO_CHANNEL_TOKEN || process.env.COOP_BROKER_TOKEN || ""` and `if (TOKEN) { ... }` means when *both* env vars are unset the server silently accepts anonymous POST to `/permission`. Any cluster-local pod that can reach `podIP:8789` can send permission verdicts and channel notifications. Today the NetworkPolicy restricts ingress to the controller, so blast radius is limited, but the code should **fail closed**: `if (!TOKEN) { throw new Error("GARDENIO_CHANNEL_TOKEN required") }`.
- Otherwise uses a proper bearer-token check: `if (auth !== `Bearer ${TOKEN}`) return 401`. Not constant-time, but for a per-pod token the timing-oracle risk is minimal.
- JSON parsing wrapped in try/catch. ✓
- The forwarded `CALLBACK_URL` is trusted from env (not user-supplied per-request), so no SSRF. ✓

### `install/versions.env`

Hardcodes versions for Go 1.25, Node 24, Python 3.12, Docker 27.5.1, Terraform 1.11.3, Claude Code 2.1.111, etc. Package lists are explicit. The file's own tracking is sound. It does **not** pin checksums — the biggest gap.

---

## Privilege boundary (K8s pod)

From `gardenio/controller/internal/podmanager/manager.go`:

```go
SecurityContext: &corev1.PodSecurityContext{
  RunAsUser: 1000, RunAsGroup: 1000, RunAsNonRoot: true, FSGroup: 1000,
}
// container:
SecurityContext: &corev1.SecurityContext{
  AllowPrivilegeEscalation: boolPtr(true),   // ← required for sudo
  ReadOnlyRootFilesystem:   boolPtr(false),
  Capabilities: { Drop: ALL, Add: [SETUID, SETGID] },
}
```

Plus:

- `automountServiceAccountToken` is **not** set. Default is `true`. **[C3]**
- No `seccompProfile` declared. Default on most clusters is unconfined or RuntimeDefault depending on PSA level; gardenio chart does not opt-in to `Restricted`.
- No `AppArmor` annotation.
- `TerminationGracePeriodSeconds: 45`.

From `agent-rbac.yaml`:

```yaml
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]
```

**Namespace-wide `*/*/*`** — the SA can read every Secret in the namespace (including every other project's API keys and database credentials), modify every Deployment, delete every Pod, etc. **[C2]**

From `agent-cluster-rbac.yaml` (when `agents.clusterAdmin.enabled=true`, which `values-fleet.yaml` sets for fleet operators):

```yaml
kind: ClusterRoleBinding
roleRef: { kind: ClusterRole, name: cluster-admin }
subjects: [ the agent SA ]
```

**Cluster-admin on the agent ServiceAccount.** Any RCE in any agent pod using that SA owns the whole cluster. **[C1]** Even the default `agent-cluster-rbac.yaml` still binds the non-cluster-admin agent SA into the cluster-admin binding's `subjects` when `agentServiceAccount.create` is true (see lines 28-32). **That re-escalates all agents to cluster-admin even when only the fleet manager was meant to get it.** This looks like a bug, not just a posture concern.

### Network policies

`networkpolicies.yaml` restricts:
- Ingress to agents: controller + vmagent only. ✓
- Egress from agents: coopmux/beads/NATS/slack-bridge + internet 0.0.0.0/0:443,80 except RFC1918. ✓ but **does not exclude 169.254.169.254/32** (AWS IMDS, GKE metadata). Any agent-side RCE can hit `http://169.254.169.254/latest/meta-data/iam/security-credentials/` if the worker node has an instance profile. **[M4]**

---

## Trivy / dive / SUID — dynamic checks (2026-04-22)

**Bead:** ka-0000008201 (follow-up to ka-0000000248)
**Scanned tag:** `<registry>/gardenio/agent:2026.111.3-rc`
(digest `sha256:737c32cac42811b96bc798768fea42a9e6469faed0c8d6d9c2e9f3fb3fdd4a25`)

The current head of `main` is calver `2026.112.6`, but that tag had not yet been published
to the registry when this audit ran (release pipeline in flight). `2026.111.3-rc` is the
most recent calver variant available in the registry at the time of this scan and is the
tag currently pinned by `acme-helm-chart/charts/gardenio/Chart.yaml` per advice ka-0000007348
("pin `-rc` tag until E2E golden-verified"). Re-run against the golden `2026.112.6` tag
once it lands.

Raw outputs live under `gardenio/images/agent/audit/` (text, not JSON — the trivy db pull
and dive extraction both worked, and table/CI output is what the AUDIT.md story needs).

### Commands run

```sh
IMG=<registry>/gardenio/agent:2026.111.3-rc

# 1. trivy — HIGH/CRITICAL, ignore-unfixed, table output
docker run --rm --network host -e DOCKER_HOST="$DOCKER_HOST" \
  aquasec/trivy:latest image \
    --severity HIGH,CRITICAL --ignore-unfixed --format table \
    "$IMG" > audit/trivy-2026.111.3-rc.txt

# 2. dive --ci
docker run --rm --network host -e DOCKER_HOST="$DOCKER_HOST" -e CI=true \
  wagoodman/dive:latest "$IMG" --ci > audit/dive-2026.111.3-rc.txt

# 3. SUID/SGID sweep
docker run --rm --entrypoint find "$IMG" / -xdev -perm /6000 -type f \
  > audit/suid-2026.111.3-rc.txt
```

### Trivy — summary

`--severity HIGH,CRITICAL --ignore-unfixed` across all detected package managers:

| Layer | Totals |
|---|---|
| Ubuntu 24.04 OS packages (92 pkgs) | **0 HIGH, 0 CRITICAL** (everything fixed-unreleased or patched in base) |
| Node.js (node-pkg) | 1 HIGH, 0 CRITICAL |
| Python (python-pkg) | 3 HIGH, 0 CRITICAL |
| Go binaries + terraform providers (18 targets) | **219 HIGH, 38 CRITICAL** across 47 unique CVEs |

**The entire HIGH/CRITICAL tail is in bundled Go binaries and terraform providers**, not in the Ubuntu base or in the language-package managers. The Ubuntu base is clean w.r.t. `--ignore-unfixed`.

Top targets by CRITICAL count (see `audit/trivy-2026.111.3-rc.txt` for per-CVE detail):

| Target | HIGH | CRITICAL |
|---|---:|---:|
| `opt/terraform-plugin-cache/.../mrparkers/keycloak/4.3.1` (gobinary) | 18 | 4 |
| `opt/terraform-plugin-cache/.../cyrilgdn/postgresql/1.22.0` (gobinary) | 16 | 4 |
| `usr/local/lib/docker/cli-plugins/docker-compose` (gobinary) | 19 | 2 |
| `usr/local/bin/rwx` (gobinary) | 17 | 2 |
| `usr/local/bin/terraform` | 14 | 2 |
| `usr/local/bin/terragrunt` | 14 | 2 |
| `usr/local/bin/stern` | 12 | 2 |
| `usr/bin/git-lfs` | 11 | 2 |
| `opt/terraform-plugin-cache/.../hashicorp/aws/5.81.0` | 10 | 2 |
| `opt/terraform-plugin-cache/.../hashicorp/local/2.5.1` | 11 | 3 |
| `opt/terraform-plugin-cache/.../hashicorp/random/3.6.3` | 10 | 3 |
| `usr/local/bin/helm` | 0 | 1 |
| `usr/local/bin/docker` (cli) | 9 | 1 |
| `usr/local/bin/golangci-lint` | 10 | 1 |
| `usr/local/bin/rwx` | 17 | 2 |
| `usr/local/share/helm/plugins/helm-push/bin/helm-cm-push` | 6 | 2 |
| `usr/local/share/helm/plugins/helm-unittest/untt` | 6 | 1 |
| `usr/local/bin/glab` | 4 | 0 |
| `usr/local/bin/gb` | 1 | 0 |
| `usr/local/bin/k6` | 5 | 0 |

Most-frequent CVEs (appearances across targets):

| CVE | Hits | Notes |
|---|---:|---|
| CVE-2026-32283 | 19 | Go stdlib |
| CVE-2026-32281 | 19 | Go stdlib |
| CVE-2026-32280 | 19 | Go stdlib |
| CVE-2026-25679 | 17 | Go stdlib |
| CVE-2025-68121 | 17 | Go stdlib |
| CVE-2025-61728 | 16 | Go stdlib / net/http |
| CVE-2025-61726 | 16 | Go stdlib / net/http |
| CVE-2025-61729 | 15 | Go stdlib / net/http |
| CVE-2025-58183 | 15 | Go stdlib |
| CVE-2025-47907 | 15 | Go stdlib |
| CVE-2026-33186 | 13 | Go stdlib |
| CVE-2024-34156 | 5 | Go encoding/gob DoS |
| CVE-2024-24790 | 5 | Go net (IPv4-in-IPv6) |
| CVE-2025-22869 | 4 | golang.org/x/crypto SSH DoS |
| CVE-2025-22868 | 3 | golang.org/x/oauth2 |
| CVE-2024-45337 | 3 | golang.org/x/crypto authz bypass |
| CVE-2023-45288 | 3 | net/http HTTP/2 CONTINUATION flood |

**Key findings**
- The image picks up the full HIGH/CRITICAL tail from pinned third-party Go binaries that haven't been rebuilt against the latest Go toolchain. A handful of very-recent Go stdlib CVEs (the `CVE-2026-3xxxx` cluster, fixed in recent Go point releases) are present in almost every bundled Go binary, because the binaries were built before the fix.
- **The `terraform-plugin-cache` providers are the single largest contribution.** Seven pinned providers account for 103 of the 257 HIGH+CRITICAL findings. These providers are never executed by the agent pod itself — they exist in `/opt/terraform-plugin-cache` so `terraform init` is fast offline. Purging or re-pinning them, or delivering them via a sidecar mount instead of baking into the image, would immediately cut the HIGH/CRITICAL surface by ~40%.
- `helm` alone has 1 CRITICAL and 0 HIGH. Our own `gb` binary has only 1 HIGH — it's built from source in the same repo so `make preflight` catches new stdlib CVEs at the next release.
- `usr/local/bin/rwx` carries 17 HIGH / 2 CRITICAL — upstream RWX CLI release cadence is the bottleneck; worth raising with the RWX vendor.
- Every finding has `--ignore-unfixed` already applied, so every number above represents a **fix that exists upstream but has not been picked up in the image build yet**. See the "Recommended hardening" section below for P0/P1 follow-ups filed as beads.

### Dive — summary

```
efficiency: 99.5601 %
wastedBytes: 40325765 bytes (40 MB)
userWastedPercent: 0.6414 %

Result: PASS [Total:3] [Passed:2] [Failed:0] [Warn:0] [Skipped:1]
```

The image is 99.56% efficient; total wasted bytes (~40 MB) are dominated by duplicated
shared libs across stages (`libcrypto.so.3` 11 MB, `libc.so.6` 4.3 MB, `libdb-5.3.so` 3.5 MB,
`libstdc++.so.6` 2.6 MB, `libgnutls.so.30` 2.1 MB, etc.) — this is from `COPY --from=<builder>`
layers dragging in a second copy of glibc/OpenSSL that the final base already has.

Also duplicated: `/etc/passwd`, `/etc/group`, `/etc/shadow` (2.0 kB + 1.1 kB + 1.1 kB) —
each builder stage edits these files, so the final image carries both the base and the
override. Not a security issue (the final layer wins at runtime) but a small hardening win
would be to normalise user/group creation in a single stage.

**No `dive --ci` rule failures.** No credentials or `.env`/`.pem`/`.p12` files surfaced in
the inefficient-files list.

### SUID / SGID sweep

```
/usr/bin/chage
/usr/bin/chfn
/usr/bin/chsh
/usr/bin/expiry
/usr/bin/gpasswd
/usr/bin/mount
/usr/bin/newgrp
/usr/bin/passwd
/usr/bin/su
/usr/bin/umount
/usr/bin/sudo
/usr/sbin/pam_extrausers_chkpwd
/usr/sbin/unix_chkpwd
```

13 SUID/SGID binaries total — **all standard Ubuntu `base-files`/`login`/`util-linux`/`sudo` binaries**. No unexpected SUID bits (no toolchain binaries, no installed CLIs, nothing under `/usr/local/` or `/opt/`).

This confirms the audit's [H1] finding: `sudo` is SUID by design, and combined with the
`NOPASSWD: ALL` sudoers entry baked in at `Dockerfile:164`, UID 1000 can become root at will.
The fix is removing `sudo` from the image, not stripping the SUID bit (doing the latter
breaks `sudo`). Tracked via the P1 "Drop `NOPASSWD: ALL`" item in Recommended hardening.

Beyond `sudo`, the rest of the SUID set is unavoidable for a functioning `login`/`passwd`
shell — keeping them is acceptable as long as `AllowPrivilegeEscalation:false` eventually
lands in the pod spec (P0 RBAC item). With `AllowPrivilegeEscalation:false` the SUID bits
are ignored by the kernel, which makes the image harmless even with `sudo` present.

### Follow-ups filed from dynamic scan

No new severity class was discovered — every finding maps to an existing follow-up or
to the "bundled-Go-binary CVE tail" supply-chain problem already tracked by **H3** in this
audit. Concrete follow-ups worth filing:

1. **Rebuild or re-pin bundled Go binaries** (helm, terraform, terragrunt, stern, git-lfs,
   docker-compose, rwx, golangci-lint, helm plugins) to pick up the Go 1.25+ stdlib fixes
   for CVE-2026-32280/-32281/-32283, CVE-2025-61726/-61728/-61729, CVE-2025-58183,
   CVE-2025-47907, CVE-2026-33186. Extension of H3 — implicitly requires the SHA-256 pinning
   recommendation so a re-pin = a deliberate supply-chain decision.
2. **Remove or externalise `/opt/terraform-plugin-cache`.** These providers are never run
   by Claude-authored code; baking them into the image is pure CVE surface. Either ship
   them via a sidecar PVC, fetch-on-first-use, or drop to only the providers actively used
   by agent tasks (`aws`, `local`, `random` today).
3. **Dedupe `/usr/lib/x86_64-linux-gnu/lib*.so.*`** across stages (dive finding). Moderate
   hardening + smaller image.

No SUID-specific follow-up beyond what H1 already tracks.

---

## Recommended hardening (ordered by ROI)

### P0 — do before the next release

1. **Fix the RBAC escalation (C1/C2).**
   - `agent-rbac.yaml`: replace the namespace admin role with a least-privilege role that only
     lists what agents legitimately need (arguably: nothing — agents should not talk to the K8s
     API at all; the controller should do it on their behalf).
   - `agent-cluster-rbac.yaml`: remove the `- kind: ServiceAccount name: agent` block from the
     `cluster-admin` binding (lines 28-32). Only the `clusterAdmin` SA should be there.
2. **Disable SA token mount (C3).** In `podmanager/manager.go`, default
   `spec.AutomountServiceAccountToken = boolPtr(false)` unless the project bead opts in.
3. **Remove `kubectl` from the base image.** Only agents that legitimately need it can install it
   at runtime via their own mount. Paired with (2), this closes C1/C2/C3 defense-in-depth.
4. **Pin every install script download.** Add `<sha256>` per binary in `versions.env`, verify via
   `sha256sum -c`. Move "curl | bash" installers (poetry, uv, bun, rustup) to GPG-verified
   tarballs or accept the supply-chain risk explicitly in a risk-register bead.
5. **Fix `jira-status-reset.sh` command injection (H2).** Pass variables via env, not
   interpolation:
   ```sh
   JIRA_TICKET="$ticket" JIRA_STATUS="$saved_status" JIRA_TRANS="$transition_id" \
     JIRA_EMAIL="$JIRA_EMAIL" JIRA_API_TOKEN="$JIRA_API_TOKEN" \
     JIRA_BASE_URL="$JIRA_BASE_URL" nohup bash -c '
       sleep 15
       current=$(curl -sf -u "$JIRA_EMAIL:$JIRA_API_TOKEN" \
         "$JIRA_BASE_URL/rest/api/2/issue/$JIRA_TICKET?fields=status" | jq -r .fields.status.name)
       [ "$current" = "$JIRA_STATUS" ] && exit 0
       curl -sf -u "$JIRA_EMAIL:$JIRA_API_TOKEN" -X POST \
         -H "Content-Type: application/json" \
         -d "{\"transition\":{\"id\":\"$JIRA_TRANS\"}}" \
         "$JIRA_BASE_URL/rest/api/2/issue/$JIRA_TICKET/transitions"
     ' >/dev/null 2>&1 &
   ```
   Remove `-k` unless there's a documented self-signed JIRA in staging — and if so, scope it to
   that env only.

### P1 — high

6. **Prompt-injection sanitisation (H5).** In `check-mail.sh` / `check-jira.sh`, strip `<`, `>`,
   and newlines from `.title` before embedding. Better yet, stop using the `<system-reminder>`
   tag convention for untrusted content — use a distinct `<mail>` tag and document that it is
   untrusted in `CLAUDE.md`.
7. **Account-bead path-traversal (H4).** Validate `secret_name` in `_resolve_cred_secret` with
   `[[ "$secret_name" =~ ^[A-Za-z0-9._-]+$ ]]`.
8. **Drop `NOPASSWD: ALL`.** Either ship a locked-down sudoers allowlist (e.g. `apt-get install`
   only) or drop sudo entirely and make the image work for the 1000-uid user directly. Paired
   with `AllowPrivilegeEscalation:false` + `Capabilities: Drop:ALL`.
9. **Remove `-k` from JIRA curl calls** (`jira-status-save.sh`, `jira-status-reset.sh`) or scope
   via `JIRA_INSECURE_TLS` env.

### P2 — medium

10. Fail-closed on the channel server token (M3).
11. Exclude `169.254.0.0/16` from the agent egress CIDR (M4).
12. `chmod 0600 $CREDS_PVC` after writing (M2).
13. Drop `chmod -R 777 /ms-playwright` and `a+rw` on rustup/cargo; the agent user owns them
    already (M1).
14. Trim the toolbelt (M5). Split into a `thin-agent` image (git, claude, gb, kd, coop) and a
    `full-agent` image (the current kitchen sink). Projects that need the kitchen sink opt in via
    `project.image`.

### P3 — low

15. Pre-pinned ssh host keys in the repo (`known_hosts.tpl`) instead of build-time `ssh-keyscan`
    (L1).
16. Replace substring match in `entrypoint.sh` with `[ "$BOAT_COMMAND" = "claudeless" ] \|\| [[ "$BOAT_COMMAND" == claudeless\ * ]]` (L2).
17. Validate `$COOLDOWN_FILE` contents before arithmetic in `stop-gate.sh` (L3).

---

## Hardening diff (representative subset)

This is intentionally a narrow sample — the full set is tracked in follow-up beads. Each
individual change should ship as its own small MR.

```text
--- a/gardenio/helm/gardenio/templates/controller/agent-rbac.yaml
+++ b/gardenio/helm/gardenio/templates/controller/agent-rbac.yaml
@@
 rules:
-  # Full namespace-scoped admin: agents can self-modify all resources
-  # within their own namespace (deployments, services, secrets, etc.)
-  - apiGroups: ["*"]
-    resources: ["*"]
-    verbs: ["*"]
+  # Agents should talk to the controller, not the K8s API. Nothing granted by
+  # default. Per-project beads opt-in to narrow rules via `rbacExtensions`.
+  []
```

```diff
--- a/gardenio/helm/gardenio/templates/controller/agent-cluster-rbac.yaml
+++ b/gardenio/helm/gardenio/templates/controller/agent-cluster-rbac.yaml
@@
 subjects:
   - kind: ServiceAccount
     name: {{ include "gardenio.coop.clusterAdminServiceAccountName" . }}
     namespace: {{ .Release.Namespace }}
-  {{- if .Values.agents.agentServiceAccount.create }}
-  - kind: ServiceAccount
-    name: {{ include "gardenio.coop.serviceAccountName" . }}
-    namespace: {{ .Release.Namespace }}
-  {{- end }}
```

```text
--- a/gardenio/controller/internal/podmanager/manager.go
+++ b/gardenio/controller/internal/podmanager/manager.go
@@
        podSpec := corev1.PodSpec{
                InitContainers: initContainers,
                Containers:     containers,
                Volumes:        volumes,
                RestartPolicy:  restartPolicyForMode(spec.Mode),
+               AutomountServiceAccountToken: boolPtr(spec.NeedsK8sAPI),
                SecurityContext: &corev1.PodSecurityContext{
                        RunAsUser:    intPtr(AgentUID),
                        RunAsGroup:   intPtr(AgentGID),
                        RunAsNonRoot: boolPtr(true),
                        FSGroup:      intPtr(AgentGID),
+                       SeccompProfile: &corev1.SeccompProfile{
+                               Type: corev1.SeccompProfileTypeRuntimeDefault,
+                       },
                },
        }
```

```text
--- a/gardenio/images/agent/hooks/jira-status-reset.sh
+++ b/gardenio/images/agent/hooks/jira-status-reset.sh
@@
-    nohup bash -c "
-        sleep 15
-        # Check if status actually changed
-        current_status=\$(/usr/bin/curl -sf -k \
-            -u '${JIRA_EMAIL}:${JIRA_API_TOKEN}' \
-            '${JIRA_BASE_URL}/rest/api/2/issue/${ticket}?fields=status' 2>/dev/null \
-            | jq -r '.fields.status.name // empty' 2>/dev/null) || exit 0
-
-        if [ \"\$current_status\" = '${saved_status}' ]; then
-            exit 0
-        fi
-
-        /usr/bin/curl -sf -k \
-            -u '${JIRA_EMAIL}:${JIRA_API_TOKEN}' \
-            -X POST \
-            -H 'Content-Type: application/json' \
-            -d '{\"transition\":{\"id\":\"${transition_id}\"}}' \
-            '${JIRA_BASE_URL}/rest/api/2/issue/${ticket}/transitions' >/dev/null 2>&1 || true
-    " >/dev/null 2>&1 &
+    JIRA_EMAIL="$JIRA_EMAIL" JIRA_API_TOKEN="$JIRA_API_TOKEN" \
+    JIRA_BASE_URL="$JIRA_BASE_URL" JIRA_TICKET="$ticket" \
+    JIRA_STATUS="$saved_status" JIRA_TRANS="$transition_id" \
+    nohup bash -c '
+        sleep 15
+        cur=$(/usr/bin/curl -sf \
+            -u "$JIRA_EMAIL:$JIRA_API_TOKEN" \
+            "$JIRA_BASE_URL/rest/api/2/issue/$JIRA_TICKET?fields=status" \
+            | jq -r ".fields.status.name // empty") || exit 0
+        [ "$cur" = "$JIRA_STATUS" ] && exit 0
+        /usr/bin/curl -sf \
+            -u "$JIRA_EMAIL:$JIRA_API_TOKEN" -X POST \
+            -H "Content-Type: application/json" \
+            -d "{\"transition\":{\"id\":\"$JIRA_TRANS\"}}" \
+            "$JIRA_BASE_URL/rest/api/2/issue/$JIRA_TICKET/transitions" >/dev/null 2>&1 || true
+    ' >/dev/null 2>&1 &
```

```text
--- a/gardenio/images/agent/resolve-auth.sh
+++ b/gardenio/images/agent/resolve-auth.sh
@@
 _resolve_cred_secret() {
     local secret_name="$1"
+    if ! [[ "$secret_name" =~ ^[A-Za-z0-9._-]+$ ]]; then
+        echo "[resolve-auth] WARNING: rejecting secret_name '$secret_name' (unsafe chars)"
+        return 0
+    fi
     local secret_path="/var/run/secrets/${secret_name}/token"
```

```diff
--- a/gardenio/images/agent/channel/gardenio-channel.ts
+++ b/gardenio/images/agent/channel/gardenio-channel.ts
@@
-const TOKEN = process.env.GARDENIO_CHANNEL_TOKEN || process.env.COOP_BROKER_TOKEN || "";
+const TOKEN = process.env.GARDENIO_CHANNEL_TOKEN || process.env.COOP_BROKER_TOKEN || "";
+if (!TOKEN) {
+  console.error("[gardenio-channel] GARDENIO_CHANNEL_TOKEN (or COOP_BROKER_TOKEN) required");
+  process.exit(1);
+}
```

```diff
--- a/gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml
+++ b/gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml
@@ agent-egress internet rule
     - to:
         - ipBlock:
             cidr: 0.0.0.0/0
             except:
               - 10.0.0.0/8
               - 172.16.0.0/12
               - 192.168.0.0/16
+              - 169.254.0.0/16      # cloud metadata / link-local
+              - 100.64.0.0/10       # CGNAT (EKS secondary CIDRs)
```

---

## Checklist accountability

| Bead checklist item | Status |
|---|---|
| Final USER in Dockerfile — not root | ✓ `USER 1000` (but NOPASSWD sudo reverses this) |
| Image minimisation, no SUID binaries | ✗ image is huge; SUID scan not run but `sudo` will be SUID by design |
| `resolve-auth.sh`: credentials / eval / source / env injection | Reviewed — one path-traversal (H4), no `eval`, no `source` of untrusted input |
| `entrypoint.sh`: quoting, command injection, `set -euo pipefail` | ✓ clean |
| Tools installed that shouldn't be | ✗ kubectl, aws, helm, terraform, docker, ssh, nc-equiv all present (M5) |
| Kube SA token mounted? | ✗ yes (C3), SA permits `*/*/*` in namespace (C2) and sometimes cluster-admin (C1) |
| Egress scoped | ✗ IMDS not excluded (M4) |
| Trivy scan | Pending — see "Dynamic checks not run" |
| `dive` for layer secrets | Pending — see "Dynamic checks not run" |

---

## Follow-up beads to file

1. `fix(helm): drop namespace wildcard role from default agent SA` — P0, gates cluster takeover.
2. `fix(helm): remove agent SA from cluster-admin binding subjects` — P0.
3. `feat(podmanager): default automountServiceAccountToken=false` — P0.
4. `chore(install): pin sha256 on every downloaded binary` — P0, supply chain.
5. `fix(hooks/jira-status-reset): escape subshell interpolation` — P0.
6. `fix(hooks/check-mail,check-jira): strip tag-ish chars from bead titles` — P1.
7. `fix(resolve-auth): validate secret_name before path concat` — P1.
8. `fix(channel): fail-closed when no token set` — P1.
9. `fix(networkpolicy): block 169.254/16 + 100.64/10 in agent egress` — P1.
10. `chore(image): split thin-agent from full-agent` — P2, post-refactor.

Each file-able via `kd create --type task -l project:gardenio -l security -l security-audit`.
