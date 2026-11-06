# Codex config injection (`gb setup codex`)

How a `harness=codex` agent gets its config and instructions. Design: [ADR 0004](../../docs/adr/0004-codex-harness.md) §E. Task: gd-kmduHGcH5z.

| Claude agent | Codex agent | Writer |
|---|---|---|
| `~/.claude/settings.json` | `$CODEX_HOME/config.toml` | `runSetupCodex` (`cmd/gb/setup_codex.go`) |
| `<ws>/.mcp.json` | `[mcp_servers.*]` in `config.toml` | same `finalizeMCPServers` source as `.mcp.json` |
| `--append-system-prompt-file` | `$CODEX_HOME/AGENTS.md` | `writeSystemPromptFile` → `installCodexAgentsMD`, every coop session start |
| `<ws>/CLAUDE.md` | unchanged; read via `project_doc_fallback_filenames`. With `--local`, a git-tracked one is left alone and its text is folded into AGENTS.md | `writeClaudeMD` |

`gb setup claude` is not run for a codex pod: `setupClaudeSettings` and the `gb agent start` k8s path branch on `GARDENIO_HARNESS=codex` (set by the controller). `--append-system-prompt-file` is never added to a codex argv (`systemPromptFlag`).

## What `config.toml` contains

Built-in defaults: `approval_policy = "never"`, `sandbox_mode = "danger-full-access"`, `[projects."<ws>"] trust_level = "trusted"`, `project_doc_fallback_filenames = ["CLAUDE.md"]`, `project_doc_max_bytes = 262144`, `check_for_update_on_startup = false`, `[history] persistence = "save-all"`, `[features] hooks = true`, `[shell_environment_policy] inherit = "all"`.

From the pod env: `CODEX_MODEL` -> `model` (omitted when unset, so Codex picks its default); `CODEX_REASONING_EFFORT` (`minimal|low|medium|high|xhigh`) -> `model_reasoning_effort`. `CODEX_LITELLM_URL` (+ `CODEX_LITELLM_TAGS`) adds the `litellm` provider with `wire_api = "responses"` and the `x-litellm-tags` header; without it Codex uses ChatGPT-subscription auth straight to OpenAI (ADR §C), so v1 pods write no provider block.

MCP translation: stdio `command/args/env` copy across; HTTP servers become `url`, and an `Authorization: Bearer ${VAR}` header becomes `bearer_token_env_var = "VAR"` so the token stays out of the file. SSE servers and `gardenio-channel` are dropped (nudges go through the driver).

## Config beads

Resolved global -> project -> role -> agent like `claude-*`.

- `codex-config`: JSON object deep-merged over the defaults (keys are `config.toml` keys). An optional `"toml"` string key holds a raw TOML fragment applied on top.
  `{"model_reasoning_effort": "high", "toml": "[tui]\nnotifications = false\n"}`
- `codex-instructions`: `{"agents_md": "..."}` appended to `AGENTS.md` after the `gb prime` output.
- `claude-instructions` still feeds `gb prime`, so role lifecycle text (`config:claude-instructions:role:*`) reaches Codex through `AGENTS.md`.

## Local mode

`gb setup codex --local --workspace <ws>` writes `<ws>/.codex/config.toml` and `<ws>/.codex/AGENTS.md` and never touches `~/.codex`. Point Codex at it with `CODEX_HOME=<ws>/.codex`.

## Ordering with the hook step

`config.toml` is regenerated wholesale at each pod start. The hook step (`hooks.json` plus the seeded `[hooks.state]` trust, ADR §E.1, [codex-hooks.md](codex-hooks.md)) runs inside `runSetupCodex` right after `writeCodexConfig`, because `config.toml` is rewritten wholesale and would drop its entries.

## Tests

`go test ./cmd/gb -run Codex`. The golden is `cmd/gb/testdata/codex_config_crew.golden.toml` (`-update-codex-golden` rewrites it). `TestGoldenConfig_CodexStrictConfig` loads it with `codex --strict-config` when the binary is on PATH and checks that an unknown key is rejected.
