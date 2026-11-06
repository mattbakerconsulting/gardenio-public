# R3 — Single-Host-Knob → Derived Link Vars (helm template-var design)

**Task:** ka-0000003816 · **Epic:** ka-0000006413 (URL-unification research/design)
**Inputs:** R1 code inventory (bundle ka-0000001712, doc ka-0000000719) ·
R2 helm inventory (bundle ka-0000002618, doc ka-0000005460 / `docs/url-unify/R2-helm-template-var-inventory.md`)
**Scope:** RESEARCH / DESIGN ONLY. Specifies the chart helper(s) + precedence rule
that let each env set **one** unified-host knob and have the chart derive every
link var. **No prod flips. No `legacyRedirects`/default-URL changes** — those are
operator-gated under ka-0000004747 / ka-0000007283 (hard gates in runbook
ka-0000000026).

Line numbers are as of `main` @ commit base of 2026-06-05. Chart paths are relative
to `gardenio/helm/gardenio/` in this monorepo. Env deploy-values live in the
**separate** `acme-helm-chart` repo (`charts/gardenio/values/*.yaml`); the PoC
composes a third layer via terraform `values-aws-poc.yaml.tftpl`.

---

## 0. Executive summary

- **The goal:** an operator sets `unifiedHost.host` (+ `enabled`) once per env, and
  the chart derives `COOPMUX_PUBLIC_URL`, `BEADS_VIEWER_URL`, `BEADS_WEB_URL`,
  `GRAFANA_BASE_URL`, viewer `BASE_PATH`, and (new) `GARDENIO_DEFAULT_HOST` +
  `ESCALATION_REVIEW_URL` — eliminating the 5+ per-app host knobs and explicit
  per-var override lists that envs hand-set today.
- **The code is already done (R1).** Every link-former reads an env-knob base and
  appends a path. The single behaviour-affecting default is `cmd/gb/main.go:126`,
  already retargetable via `$GARDENIO_DEFAULT_HOST` — a wiring change, not a code
  change.
- **The gap is in helm (R1 §2, R2 §5).** In `slack-bridge/deployment.yaml` and
  `beads-viewer/deployment.yaml`, the link knobs derive from **per-app ingress
  hosts** (`coopmux.ingress.host`, `beadsViewer.avaIngress.host`,
  `beads3d.ingress.host`), with a hardcoded literal `/mux` suffix. **Only**
  `GRAFANA_BASE_URL` (`beads-viewer/deployment.yaml:186–204`) already derives from
  `unifiedHost.host` + `paths.grafana`. **That printf block is the reference
  pattern** this design replicates for the other knobs.
- **The design = 6 named helpers in `_helpers.tpl`**, each implementing the same
  three-tier precedence: *(1) explicit per-var override wins → (2) derive from
  `unifiedHost` when fully enabled → (3) legacy per-app host (unchanged)*. The
  deployments call the helper instead of inlining the URL.
- **R2 BLOCKER carried forward:** `unifiedHost.enabled=true` is set in **zero**
  committed envs (`acme-helm-chart` deploy-values). The epic's "prod already unified"
  ground-truth is a **live-vs-committed drift** that must be resolved before any
  cutover (§7, flagged to epic).
- **Safety split:** the 6 helpers are **SAFE to land now** because each preserves
  the existing legacy fallback when `unifiedHost.enabled=false` (zero behaviour
  change in every committed env). The *activation* (setting `unifiedHost.enabled=true`
  + `host` per env) is **COUPLED to the gated cutover** and is **not** part of this
  epic. §6 marks each derivation SAFE-now vs COUPLED.

---

## 1. Design principle: one knob in, every link out

```
                         ┌─────────────────────────────────────────┐
   ONE per-env knob ───► │ unifiedHost.enabled = true               │
                         │ unifiedHost.host    = gardenio.<env>.…     │
                         │ unifiedHost.paths.{beads,mux,grafana,…}  │ (chart defaults)
                         └───────────────────┬─────────────────────┘
                                             │  _helpers.tpl derivations
        ┌────────────────────────────────────┼────────────────────────────────────┐
        ▼                ▼                     ▼                ▼                    ▼
 COOPMUX_PUBLIC_URL  BEADS_VIEWER_URL   BEADS_WEB_URL   GRAFANA_BASE_URL    GARDENIO_DEFAULT_HOST
 =host+paths.mux     =host+paths.beads  =host+paths.beads =host+paths.grafana =host (bare)
 (slack-bridge +     (slack-bridge +    (slack-bridge)   (beads-viewer —      (agent pods +
  beads-viewer)       gb via bridge)                      ALREADY DONE)        ESCALATION_REVIEW_URL
                                                                               =host+/escalations)
        │                                                                           │
        └──────────  viewer BASE_PATH = paths.beads (coopmux BASE_PATH = paths.mux) ┘
```

**Invariant tie-in (from R2 §1 / values.yaml L209–215):** when `unifiedHost.enabled=true`,
the upstream `BASE_PATH`-equiv on each app MUST equal the Traefik strip-prefix
(`coopmux.basePath == paths.mux`, `beadsViewer.ingress.pathPrefix == paths.beads`,
`jiraBridge.basePath == paths.jira`). Today these are operator-set by hand and the
chart does **not** enforce the tie. **This design also makes those `BASE_PATH` envs
derive from `paths.*` when unified is on** (helper `gardenio.unified.beadsBasePath` /
`gardenio.unified.muxBasePath`), so the invariant holds automatically and the manual
MUST-set list (R2 §4.3) collapses to a single `host` line.

---

## 2. The reference pattern (already in the tree)

`beads-viewer/deployment.yaml:186–204` is the proven shape — explicit override wins,
else derive from `unifiedHost` **only when fully enabled**, else stay empty:

```text
{{- $grafanaBaseURL := .Values.beadsViewer.grafanaBaseURL | default "" -}}
{{- if eq $grafanaBaseURL "" -}}
{{-   $grafanaUnifiedPath := (.Values.unifiedHost.paths | default dict).grafana | default "" -}}
{{-   if and .Values.grafana.enabled .Values.unifiedHost.enabled .Values.unifiedHost.host (ne $grafanaUnifiedPath "") -}}
{{-     $grafanaBaseURL = printf "https://%s%s" .Values.unifiedHost.host $grafanaUnifiedPath -}}
{{-   end -}}
{{- end }}
{{- if ne $grafanaBaseURL "" }}
- name: GRAFANA_BASE_URL
  value: {{ $grafanaBaseURL | quote }}
{{- end }}
```

Every helper below is this block, generalized into a named `define` so both
deployments share one implementation and the legacy fallback is explicit.

---

## 3. `_helpers.tpl` helpers (the design)

Six new `define`s. Each takes the root context (`.`), applies the **three-tier
precedence**, and returns a string (empty string ⇒ caller omits the env var, exactly
as the Grafana block does). Naming follows the existing `gardenio.<area>.<thing>`
convention in `_helpers.tpl`.

### 3.1 Shared precedence contract

For every link var:

| Tier | Source | Wins when |
|---|---|---|
| **1. explicit override** | the existing per-var value (`slackBridge.beadsViewerURL`, `beadsViewer.grafanaBaseURL`, etc.) | non-empty |
| **2. unified-derived** | `printf "https://%s%s" unifiedHost.host <paths.X>` | override empty **AND** `unifiedHost.enabled` **AND** `unifiedHost.host != ""` **AND** the relevant `paths.X != ""` (and component `.enabled` where the link only makes sense if the app is deployed) |
| **3. legacy fallback** | today's per-app-host expression (`https://<coopmux.ingress.host>/mux`, `https://<beadsViewer.avaIngress.host>`, …) | both above empty |

Tier-3 is **byte-for-byte the current template output**, so with
`unifiedHost.enabled=false` (every committed env) the rendered manifest is
**unchanged**. That is what makes the helpers SAFE to land before any cutover.

### 3.2 The helpers

```text
{{/*
gardenio.unified.host — the single per-env knob, or "" when unified is off.
Guard used by every derivation below.
*/}}
{{- define "gardenio.unified.host" -}}
{{- if and .Values.unifiedHost.enabled .Values.unifiedHost.host -}}
{{- .Values.unifiedHost.host -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.unified.url — derive https://<unifiedHost.host><path> when unified is on
AND the given path is non-empty; else "". Args: dict "ctx" . "path" <pathStr>.
*/}}
{{- define "gardenio.unified.url" -}}
{{- $ctx := .ctx -}}
{{- $h := include "gardenio.unified.host" $ctx -}}
{{- if and (ne $h "") (ne .path "") -}}
{{- printf "https://%s%s" $h .path -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.coopmuxPublicURL — COOPMUX_PUBLIC_URL.
1) explicit slackBridge.coopmuxPublicURL (NEW optional override; add to values)
2) unified: https://<host><paths.mux>   (only when coopmux.enabled)
3) legacy:  https://<coopmux.ingress.host>/mux  (only when coopmux.enabled+ingress.host)
*/}}
{{- define "gardenio.link.coopmuxPublicURL" -}}
{{- $o := .Values.slackBridge.coopmuxPublicURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else if .Values.coopmux.enabled -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).mux | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}{{- else if .Values.coopmux.ingress.host -}}
{{-     printf "https://%s/mux" .Values.coopmux.ingress.host -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.beadsViewerURL — BEADS_VIEWER_URL.
1) explicit slackBridge.beadsViewerURL
2) unified: https://<host><paths.beads>
3) legacy:  avaIngress.host → else ingress.host+pathPrefix  (current chain)
*/}}
{{- define "gardenio.link.beadsViewerURL" -}}
{{- $o := .Values.slackBridge.beadsViewerURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).beads | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}
{{-   else if and .Values.beadsViewer.avaIngress .Values.beadsViewer.avaIngress.enabled .Values.beadsViewer.avaIngress.host -}}
{{-     printf "https://%s" .Values.beadsViewer.avaIngress.host -}}
{{-   else if and .Values.beadsViewer.ingress.enabled .Values.beadsViewer.ingress.host -}}
{{-     printf "https://%s%s" .Values.beadsViewer.ingress.host .Values.beadsViewer.ingress.pathPrefix -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.beadsWebURL — BEADS_WEB_URL (escalation deep links).
1) explicit slackBridge.beadsWebURL
2) unified: https://<host><paths.beads>   (beads3d deprecated → repoint at /beads)
3) legacy:  https://<beads3d.ingress.host>  (only when beads3d.enabled)
NOTE: tier-2 is COUPLED-to-gated — see §6 note A (escalations HTML must be served
under /beads on the unified host first).
*/}}
{{- define "gardenio.link.beadsWebURL" -}}
{{- $o := .Values.slackBridge.beadsWebURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).beads | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}
{{-   else if and .Values.beads3d.enabled .Values.beads3d.ingress.host -}}
{{-     printf "https://%s" .Values.beads3d.ingress.host -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.grafanaBaseURL — GRAFANA_BASE_URL. Extracts the EXISTING block at
beads-viewer/deployment.yaml:186–204 verbatim. Already correct; lifting it into a
helper just lets the design treat all six uniformly.
*/}}
{{- define "gardenio.link.grafanaBaseURL" -}}
{{- $o := .Values.beadsViewer.grafanaBaseURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $p := (.Values.unifiedHost.paths | default dict).grafana | default "" -}}
{{-   if and .Values.grafana.enabled .Values.unifiedHost.enabled .Values.unifiedHost.host (ne $p "") -}}
{{-     printf "https://%s%s" .Values.unifiedHost.host $p -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.defaultGardenioHost — GARDENIO_DEFAULT_HOST for agent pods + gb CLI.
Bare host (no scheme/path); gb derives /beads + cli.<host> from it (R1 #17–19).
1) explicit agents.defaultGardenioHost (NEW optional override)
2) unified: <unifiedHost.host>
3) legacy:  "" → gb falls back to compiled default (R1 #17). NOT set in helm today.
COUPLED-to-gated — see §6 note D (couples ka-0000004747 default-URL switch).
*/}}
{{- define "gardenio.link.defaultGardenioHost" -}}
{{- $o := (.Values.agents.defaultGardenioHost | default "") -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}{{ include "gardenio.unified.host" . }}{{- end -}}
{{- end -}}

{{/*
gardenio.link.escalationReviewURL — ESCALATION_REVIEW_URL (viewer "Review →" deep
link, R1 #13). Unset in every env today → link silently disabled. Additive win.
1) explicit beadsViewer.escalationReviewURL (NEW optional override)
2) unified: https://<host>/escalations
3) legacy:  "" (stays disabled)
*/}}
{{- define "gardenio.link.escalationReviewURL" -}}
{{- $o := (.Values.beadsViewer.escalationReviewURL | default "") -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $h := include "gardenio.unified.host" . -}}
{{-   if ne $h "" -}}{{ printf "https://%s/escalations" $h }}{{- end -}}
{{- end -}}
{{- end -}}
```

> **`viaShim`/forwardAuth note:** these helpers only form the *outbound link
> strings*. They do NOT touch routing, `viaShim`, or `forwardAuth` — those remain
> exactly as the chart renders them today and are part of the gated infra cutover
> (ka-0000006723), not this design.

### 3.3 `BASE_PATH` derivations (close the invariant by construction)

Two more tiny helpers make the MUST-set invariant automatic instead of manual:

```gotemplate
{{- define "gardenio.unified.beadsBasePath" -}}
{{- if include "gardenio.unified.host" . -}}{{ (.Values.unifiedHost.paths | default dict).beads | default "" }}
{{- else -}}{{ .Values.beadsViewer.ingress.pathPrefix }}{{- end -}}
{{- end -}}

{{- define "gardenio.unified.muxBasePath" -}}
{{- if include "gardenio.unified.host" . -}}{{ (.Values.unifiedHost.paths | default dict).mux | default "" }}
{{- else -}}{{ .Values.coopmux.basePath }}{{- end -}}
{{- end -}}
```

When unified is off, both return today's hand-set value (no change). When unified is
on, they return `paths.beads` / `paths.mux`, guaranteeing
`BASE_PATH == strip-prefix`. This is the mechanism that lets a deploy-values file
drop the `coopmux.basePath` / `beadsViewer.ingress.pathPrefix` lines entirely.

---

## 4. Deployment call sites (what changes in the two templates)

The deployments stop inlining URLs and call the helpers. The `if ne … ""` /
omit-when-empty wrapper is preserved exactly (matches the Grafana block).

### 4.1 `slack-bridge/deployment.yaml`

| Current (lines) | Replace with |
|---|---|
| L231–233 `COOPMUX_PUBLIC_URL` inline | `{{- $u := include "gardenio.link.coopmuxPublicURL" . }}{{- if ne $u "" }}` → `name: COOPMUX_PUBLIC_URL / value: {{ $u \| quote }}` |
| L242–247 `BEADS_WEB_URL` chain | `{{- $u := include "gardenio.link.beadsWebURL" . }}{{- if ne $u "" }}` → emit `BEADS_WEB_URL` |
| L255–264 `BEADS_VIEWER_URL` chain | `{{- $u := include "gardenio.link.beadsViewerURL" . }}{{- if ne $u "" }}` → emit `BEADS_VIEWER_URL` |

### 4.2 `beads-viewer/deployment.yaml`

| Current (lines) | Replace with |
|---|---|
| L116–118 `BASE_PATH` | `{{- $bp := include "gardenio.unified.beadsBasePath" . }}{{- if ne $bp "" }}` → emit `BASE_PATH` |
| L181–184 `COOPMUX_PUBLIC_URL` inline | `{{- $u := include "gardenio.link.coopmuxPublicURL" . }}{{- if ne $u "" }}` → emit |
| L186–204 `GRAFANA_BASE_URL` block | `{{- $u := include "gardenio.link.grafanaBaseURL" . }}{{- if ne $u "" }}` → emit (pure refactor, identical output) |
| *(new)* `ESCALATION_REVIEW_URL` | `{{- $u := include "gardenio.link.escalationReviewURL" . }}{{- if ne $u "" }}` → emit |

### 4.3 `coopmux/deployment.yaml`

| Current (L142–144) | Replace with |
|---|---|
| `BASE_PATH` ← `coopmux.basePath` | `{{- $bp := include "gardenio.unified.muxBasePath" . }}{{- if ne $bp "" }}` → emit |

### 4.4 Agent-pod env (`GARDENIO_DEFAULT_HOST`)

`GARDENIO_DEFAULT_HOST` is consumed by the **gb CLI inside agent pods** (R1 #17). Agent
pods are spawned by the controller (`internal/bridge/bot_agents.go`), not by a static
Deployment template, so this knob is injected via the controller's agent-env builder /
`agents.extraEnv` (`values.yaml:445`). **Recommended wiring (R4):** have the chart
render `agents.extraEnv` (or the controller's spawn env) to include
`GARDENIO_DEFAULT_HOST = {{ include "gardenio.link.defaultGardenioHost" . }}` when
non-empty. This is a controller/spawn-path change, larger than the two-deployment
refactor — split into its own R4 child (§8 task 4).

---

## 5. New `values.yaml` keys (optional overrides; all default to `""`/absent)

These keep tier-1 precedence working and are **additive** (no existing key changes):

```yaml
slackBridge:
  coopmuxPublicURL: ""      # NEW: explicit COOPMUX_PUBLIC_URL override (tier-1)
  # beadsViewerURL / beadsWebURL already exist (tier-1)
beadsViewer:
  escalationReviewURL: ""   # NEW: explicit ESCALATION_REVIEW_URL override (tier-1)
  # grafanaBaseURL already exists (tier-1)
agents:
  defaultGardenioHost: ""    # NEW: explicit GARDENIO_DEFAULT_HOST override (tier-1)
```

Everything else (`unifiedHost.host`, `unifiedHost.paths.*`) already exists. The
end-state per-env deploy-values shrink to **one meaningful line** (`unifiedHost.host`)
plus `enabled: true`, because `paths.*` come from chart defaults and `BASE_PATH`s are
derived (§3.3).

---

## 6. SAFE-now vs COUPLED-to-gated-cutover (per derivation)

| Helper / var | Land helper now? | Activation gating | Notes |
|---|---|---|---|
| `grafanaBaseURL` | **SAFE** (pure refactor — identical output) | n/a — already live where grafana on | Reference pattern; no behaviour change. |
| `coopmuxPublicURL` | **SAFE** to land | activation COUPLED (needs `unifiedHost.enabled` + `coopmux.basePath==paths.mux` + `/mux` route live) | tier-3 legacy = current output verbatim. |
| `beadsViewerURL` | **SAFE** to land | activation COUPLED (ka-0000007283 legacyRedirects + `/beads` route live) | tier-3 legacy = current avaIngress→ingress chain. |
| `beadsWebURL` | **SAFE** to land | tier-2 COUPLED — **note A** | beads3d deprecated; unified target needs escalations HTML under `/beads`. |
| `escalationReviewURL` | **SAFE + additive** (currently unset everywhere) | tier-2 needs `/escalations` route to exist on unified host | Until then stays "" (today's behaviour). Filing as a clean win. |
| `defaultGardenioHost` (agent env) | helper SAFE; **wiring is R4** | **COUPLED — note D** (ka-0000004747 default-URL switch) | Drive per-env via env knob; do NOT change compiled default. |
| `beadsBasePath` / `muxBasePath` | **SAFE** to land | activation COUPLED (only diverges when `unifiedHost.enabled`) | Makes MUST-set invariant automatic. |

**Why "land the helpers now" is safe:** with `unifiedHost.enabled=false` in every
committed env (R2 §4.1), `include "gardenio.unified.host"` returns `""`, every helper
falls to tier-3, and `helm template` output is **byte-identical** to today. The
helpers are inert until an operator sets `unifiedHost.enabled=true` + `host` — which
is the gated cutover step, **out of scope here**.

**Note A (BEADS_WEB_URL → unified):** R1 §4 note A. `bot_escalations.go:89` builds
`<BEADS_WEB_URL>/ui/escalations.html#<id>`. Confirm the escalations HTML is served
under `/beads/ui/…` on the unified host before relying on tier-2; until then keep an
explicit `slackBridge.beadsWebURL` override (tier-1) or leave on legacy beads3d.

**Note D (defaultGardenioHost):** R1 §4. Do not touch the compiled
`cmd/gb/main.go:126` default (couples ka-0000004747). Per-env retarget is purely via
`$GARDENIO_DEFAULT_HOST` in the agent-pod env. SAFE in the sense that setting it to the
correct per-env host only *corrects* gb's fallback; but because it changes the host gb
links to when no `BEADS_VIEWER_URL` is present, treat its activation as COUPLED to the
default-URL switch and gate it with the cutover.

**Hard gates honored:** no template here flips `unifiedHost.enabled`, no
`legacyRedirects` change, no default-URL switch. Those remain operator-only under
ka-0000004747 / ka-0000007283 / runbook ka-0000000026.

---

## 7. Per-env application table

Desired **end state** (post-cutover) — each env's deploy-values in `acme-helm-chart`
(or PoC tftpl) sets the knob, chart derives the rest:

| Env | Sets (deploy-values) | Derived `unifiedHost.host` | COOPMUX_PUBLIC_URL | BEADS_VIEWER_URL | GRAFANA_BASE_URL | GARDENIO_DEFAULT_HOST |
|---|---|---|---|---|---|---|
| **prod** (gardenio.yaml) | `unifiedHost.{enabled:true,host}` | `gardenio.e2e.example.com` | `…/mux` | `…/beads` | `…/grafana` (if grafana on) | `gardenio.e2e.example.com` |
| **gardenio-next** | same, host=`gardenio-next.…` | `gardenio-next.e2e.example.com` | `…/mux` | `…/beads` | `…/grafana`* | `gardenio-next.…` |
| **gardenio-rwx** | same, host=`gardenio-rwx.…` | `gardenio-rwx.e2e.example.com` | `…/mux` | `…/beads` | `…/grafana`* | `gardenio-rwx.…` |
| **customer-poc PoC** | **stays per-app** — `beadsViewer.avaIngress.{enabled,host,trustSigner}`; `unifiedHost` OFF | n/a (avaIngress) | legacy `…/mux` | `https://<ava host>` (tier-3) | (grafana off) | per-app AVA host via explicit `agents.defaultGardenioHost` override |

\* only if that env enables grafana under `/grafana`.

**PoC exception (important):** the customer-poc PoC uses `beadsViewer.avaIngress` on a
**per-app AVA host**, NOT `unifiedHost` (R2 §4.2; advice ka-0000007102 / values
`values-aws-poc.yaml.tftpl`). The three-tier precedence handles this **for free**:
PoC leaves `unifiedHost.enabled=false`, so every helper falls to tier-3 (legacy
avaIngress chain) — exactly today's behaviour. The PoC needs **no change** and must
**not** be put on `unifiedHost`. If the PoC later wants gb's default host corrected,
set the tier-1 `agents.defaultGardenioHost` override rather than enabling unifiedHost.

**Current state (R2 §4.1 — the drift):** `unifiedHost.enabled=true` is set in **NO**
committed env. So today every helper falls to tier-3 in every env. The end-state table
above is the *target*, reachable only via the gated cutover.

---

## 8. Remediation child tasks for R4 (file under epic ka-0000006413)

Actionable, non-gated work the helpers enable. Each links to — does not duplicate —
the gated cutover tasks (ka-0000004747, ka-0000007283).

1. **[SAFE] Land the 8 `_helpers.tpl` helpers + refactor 3 deployments to call them.**
   Pure refactor for `grafanaBaseURL`; legacy-preserving for the rest. `helm template`
   diff must be empty for all committed envs (`unifiedHost.enabled=false`). Adds the
   `unifiedHost`-aware tier-2 that activates on cutover. *(This is the SAFE helm MR
   this epic may open — see §9.)*
2. **[SAFE/additive] Wire `ESCALATION_REVIEW_URL`** in `beads-viewer/deployment.yaml`
   via `gardenio.link.escalationReviewURL`, plus `beadsViewer.escalationReviewURL`
   values key. Currently unset in every env ⇒ viewer "Review →" deep link is dead
   (R1 #13 / note B). Tier-2 needs the `/escalations` route to exist; until then it
   stays "" (no regression). Confirm route before enabling.
3. **[SAFE] Make legacy gallery anchors relative** (`incident_intel_gallery.html`,
   R1 #28 / note C): 8 hardcoded `beads-viewer.gardenio.app…/<id>` → relative
   `/<id>` under `BASE_PATH`. Code (not helm); low risk, no operator gate.
4. **[COUPLED→R4] Inject `GARDENIO_DEFAULT_HOST` into agent-pod env** from
   `gardenio.link.defaultGardenioHost` (controller spawn-path / `agents.extraEnv`).
   Larger than the deployment refactor; couples ka-0000004747 — keep gated.
5. **[BLOCKER/investigate] Resolve unifiedHost live-vs-committed drift** (R2 §6.1 /
   §7 here): epic says prod is unified; committed `acme-helm-chart` deploy-values set
   `unifiedHost` in zero envs. Reconcile before any cutover. Operator/infra item;
   links to ka-0000006723.

Tasks 1–3 are SAFE to implement and merge independently of the gated cutover. Tasks 4
and the activation of tiers-2 everywhere are COUPLED and gated.

---

## 9. SAFE helm MR (optional, this epic)

Per the task brief, this epic MAY open the SAFE helm derivation MR (task 8.1) that
replicates the GRAFANA_BASE_URL pattern, **clearly marked SAFE and NOT a cutover
flip**. Acceptance for that MR:

- `_helpers.tpl` gains the 8 `define`s above.
- The 3 deployments call the helpers; **`helm template` output is byte-identical for
  every committed env** (proof: render with each `acme-helm-chart` deploy-values, diff
  against `main`). This is the safety gate — empty diff ⇒ inert until cutover.
- New `values.yaml` keys default to `""`/absent.
- MR description states explicitly: **no `unifiedHost.enabled` flip, no
  `legacyRedirects` change, no default-URL switch**; activation is gated under
  ka-0000004747 / ka-0000007283 / ka-0000000026.

If the R3 agent does not land it, R4 picks it up as task 8.1.

---

*Generated for ka-0000003816 (R3) under epic ka-0000006413. Builds on R1
(ka-0000003583 / bundle ka-0000001712) and R2 (ka-0000002793 / bundle ka-0000002618).
Operator: Matt Baker.*
