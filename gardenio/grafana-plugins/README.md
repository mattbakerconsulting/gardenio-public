# Grafana plugins

In-repo home for custom gardenio Grafana plugins.

## `gardenio-assistant-app`

A custom self-signed Grafana **app plugin** — the owned UI layer of the OSS
Grafana chat frontend (Path C). It will host a gardenio-branded chat box backed
by `gardenio-litellm` (Bedrock via IRSA), consuming `@grafana/llm` +
`mcp-grafana` exposed by the Apache-2.0 `grafana-llm-app` foundation.

- **Plugin id:** `gardenio-assistant-app`
- **Type:** app (React/TS frontend, frontend-only to start; Go backend optional later)
- **Scaffolded with:** `npx @grafana/create-plugin@7.8.0`
  (`--plugin-type app --plugin-name assistant --org-name gardenio --no-backend`)

This directory is the bare scaffold (C2, bead `ka-0000002566`). The actual
chat UX is wired in follow-up beads:

| Bead | Phase C step | What |
|---|---|---|
| `ka-0000002566` | C2 | **this scaffold** |
| `ka-0000006039` | C3 | wire `llm.streamChatCompletions` + MCP client + gardenio chat UX |
| `ka-0000007568` | C4 | sign + distribute (image / pinned-URL / init-container) |
| `ka-0000002263` | C5 | non-prod acceptance |
| `ka-0000001659` | C6 | prod-rollout operator GO + RBAC decision |

Epic: `ka-0000002011` · Plan: `ka-0000005715` (Phase C).

### Build / lint (from `gardenio-assistant-app/`)

```sh
npm install        # frontend deps (node_modules/ is gitignored)
npm run build      # webpack production build -> dist/
npm run dev        # watch build
npm run lint       # eslint
npm run typecheck  # tsc --noEmit
npm run test:ci    # jest
```

### Notes for reviewers / follow-up crew

- The scaffold ships standalone-repo machinery that is **inert in this GitLab
  monorepo**: the nested `.github/workflows/` (GitHub Actions only run from a
  GitHub repo root) and `docker-compose.yaml` (local dev server). They are left
  as-generated; CI/distribution is reconciled in C4 (`ka-0000007568`).
- `dist/` and `node_modules/` are gitignored — only source is committed.
