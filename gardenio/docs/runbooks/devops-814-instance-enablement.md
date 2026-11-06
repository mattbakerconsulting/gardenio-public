# DEVOPS-814 — Enabling a Gas Boat instance on a new JIRA cluster (listen-only → SLA-with-friction)

> **Scope.** This runbook covers the *instance-enablement* slice of the staging-env
> effort: how to bring up a Gas Boat instance that **observes** a new JIRA cluster
> safely, how to graduate it from listen-only to acting, and how to manage SLA /
> escalation configuration with deliberate change-friction. It is the companion to
> [`devops-814` parent ticket](https://acme.atlassian.net/browse/DEVOPS-814)
> and deliberately does **not** repeat the broad provisioning steps already in
> [`devops-798-staging-env-setup.md`](./devops-798-staging-env-setup.md) (Atlassian
> sandbox creation, on-prem GitLab, Slack, ingestion-markdown decision). Read 798
> first for *which* components exist and *who* owns each; read this for *how the
> gardenio instance itself is gated* once those components are live.
>
> **Owners (from the 2026-06-16 AI Driven Support & Operations meeting):**
> Matthew Baker (Gas Boat instance + GitLab), Sky Bennett (JIRA admin / Slack),
> Lee Morgan (JIRA sandbox).

---

## 0. TL;DR — the four real knobs

Everything below reduces to four helm values on the new instance's `jiraBridge:`
block (`gardenio/helm/gardenio/values.yaml`, overridden in the deploy-values file for
the staging release). These are the *only* config that distinguishes a safe
listen-only instance from one that writes:

| Knob | Listen-only (initial) | Acting (after validation) | What it gates |
|---|---|---|---|
| `jiraBridge.jira.requiredLabels` | a label **no real ticket carries** (e.g. `gardenio-staging-go`) | `approved-to-work` (or drop, per project) | Whether the poller **spawns an agent** for a ticket at all |
| `jiraBridge.jira.disableTransitions` | `true` | `true` (keep) / `false` (only if humans want auto-transitions) | Whether bead lifecycle **writes status back** to JIRA |
| `jiraBridge.jira.boatProjects` | sandbox prefixes only | sandbox prefixes only | Which JIRA prefix → which boat/git project |
| `jiraBridge.webhook.enabled` | `false` until smoke-tested | `true` | Whether `@gardenio` **mentions** spawn agents |

The instance is "listen-only" when the poller *reads and classifies* tickets but
the `requiredLabels` gate prevents any spawn, and `disableTransitions` prevents any
status write. Nothing in the JIRA cluster changes. This is verified, not asserted —
see §2.

---

## 1. Provision the instance pointed at the new JIRA cluster

Use the **gardenio-in-a-box** install path (single EC2 + on-box k3s, AWS-poc account
`123456789012`) per 798 §3 — it is the most isolable and the easiest to rebuild.
The steps below assume that box is up and you are editing its release's
deploy-values file.

### 1.1 jira-bridge → the new cluster

The bridge reads its target entirely from helm values + a K8s secret — **no code
change, no image rebuild**. Set, on the staging deploy-values:

```yaml
jiraBridge:
  enabled: true
  jira:
    baseURL: "https://<new-cluster>.atlassian.net"   # the NEW cluster, never prod
    email: "gardenio@acme.example"
    secretName: "gardenio-jira-bridge-creds"           # token home site MUST be the new cluster
    projects: "DEVOPS,PE"                              # poll list (sandbox keys)
    statuses: "To Do,Refine,Active,Review Please"
    issueTypes: "Bug,Task,Story,Epic"
    pollInterval: "60s"
    boatProjects: "monorepo=<staging-git-url>:DEVOPS,monorepo=<staging-git-url>:PE"
    fixVersions: ""                                    # leave empty in sandbox unless replicating prod gating
```

Create the token secret out-of-band (tokens never go in git — see
[advice ka-0000001317](https://gardenio.e2e.example.com/beads/ka-0000001317)):

```sh
NS=gardenio
kubectl -n "$NS" create secret generic gardenio-jira-bridge-creds \
  --from-literal=api-token='<token-issued-on-the-NEW-cluster>'
```

> **The sandbox/prod boundary is the `baseURL` + the token's home site, nothing
> else.** A token minted on the new cluster physically cannot read or write
> `acme.atlassian.net`. Confirm the token's home site before applying — this is
> the single most important safety check in the whole runbook.

### 1.2 gitlab-bridge → the isolated GitLab

Point `gitlabBridge.gitlab.baseURL` + token at the staging GitLab (798 §2 / §3.2).
The bridge's own target and bot token are its config; `JIRA_BASE_URL` on the
gitlab-bridge is only for enrichment links. Wire the staging project's webhook to
`gitlabBridge.ingress.host` once the instance is up (§3 below).

---

## 2. Listen-only mode — what it is, how to set it, how to prove it

"Listen-only" is **not** a dedicated flag — it is the natural state of the bridge
when the spawn gate can never match and transitions are disabled. The poller still
GETs tickets every `pollInterval` and runs severity/classification, so you get the
full classification signal **without any side effect on the cluster**.

### 2.1 Set the gate so nothing spawns

The poller only spawns an agent for a ticket when the ticket carries **every** label
in `requiredLabels` (per-project when `FilterProjects` is set; otherwise globally —
see `jira_poller.go:buildJQL`). To guarantee zero spawns, require a label that no
real ticket will ever carry:

```yaml
jiraBridge:
  jira:
    requiredLabels: "gardenio-staging-go"   # sentinel label; absent from all real tickets
    disableTransitions: true               # bead lifecycle never writes status back to JIRA
```

With this set, the generated JQL includes `... AND labels = "gardenio-staging-go"`,
so the poll returns the classified tickets you want to *observe* — but because the
label is absent, the spawn path is never taken. To validate a **specific** ticket's
behavior end-to-end, add `gardenio-staging-go` to that one ticket in the sandbox; it
becomes the only ticket eligible to spawn.

> **Why a sentinel label, not `enabled: false`?** Disabling the bridge gives you no
> classification signal at all. The sentinel-label approach keeps the poller running
> and logging its severity judgments (the exact behavior DEVOPS-814 wants to
> validate) while structurally preventing any spawn or write. It is also a
> single-label flip to go live.

### 2.2 Prove it is actually listen-only

Do not trust the config — verify the running instance:

```sh
NS=gardenio

# (a) Bridge is polling the NEW cluster, not prod:
kubectl -n "$NS" logs deploy/gardenio-jira-bridge | grep -i 'baseURL\|polling' | tail
# expect the new-cluster host; MUST NOT contain acme.atlassian.net

# (b) The effective JQL carries the sentinel label (no spawn can fire):
kubectl -n "$NS" logs deploy/gardenio-jira-bridge | grep -i 'jql' | tail
# expect: ... AND labels = "gardenio-staging-go" ...

# (c) Transitions are disabled (no write-back path):
kubectl -n "$NS" logs deploy/gardenio-jira-bridge | grep -i 'disableTransitions\|transition' | tail

# (d) Zero spawns over a full poll window:
kubectl -n "$NS" logs deploy/gardenio-jira-bridge | grep -i 'spawn' | tail
# expect: nothing (until you label a ticket gardenio-staging-go on purpose)
```

A clean listen-only instance shows: new-cluster host in (a), sentinel label in (b),
transitions disabled in (c), and silence in (d).

### 2.3 Replay a week of real tickets (validation pass)

DEVOPS-814 asks to replay ~1 week of real tickets to validate severity
classification. Do this **in the sandbox cluster only** with sanitized copies — never
point the instance at prod to read the originals:

1. Export the week's tickets from prod (summary/description/type/labels only — strip
   PHI and customer identifiers per
   [advice ka-0000005335](https://gardenio.e2e.example.com/beads/ka-0000005335)).
2. Recreate them as throwaway tickets in the new cluster (manual create per the
   798 guardrail — no bulk prod sync).
3. Watch the jira-bridge logs for the classification each one receives. Because the
   instance is listen-only, classification runs with **no** spawn and **no**
   write-back — you are scoring the judgment, not exercising the action path.
4. Diff the staging classifications against how prod actually triaged them. File
   mismatches as bugs against the classifier.

---

## 3. Webhook (`@gardenio` mentions) — enable only after smoke test

Keep `jiraBridge.webhook.enabled: false` and `gitlabBridge` webhooks unwired during
the listen-only phase: a mention handler can spawn an agent regardless of the poll
gate, so it is a second, independent ingress that must stay closed until you trust
the instance.

When ready, enable per 798 §3 / the `jiraBridge.webhook` block (HMAC secret,
IngressRoute host, Atlassian webhook registration pointed at the **new cluster's**
webhook host). Verify a mention on a sandbox ticket spawns exactly one agent before
considering the instance "acting".

---

## 4. Graduating listen-only → acting

Flip one gate at a time and re-verify between flips:

1. **Spawn gate.** Change `requiredLabels` from the sentinel to the real gate
   (`approved-to-work`, or empty if the project should auto-spawn). Re-run the §2.2
   checks; now expect spawns for eligible tickets.
2. **Write-back (optional).** Leave `disableTransitions: true` unless the team
   explicitly wants Gas Boat to move JIRA status. If they do, set it to `false`;
   note that `enableClaimTransition` is the narrower option — it allows only the
   "In Progress on claim" transition while keeping all other transitions disabled
   (`jira_sync.go`). Prefer that over a blanket `false`.
3. **Webhook.** Enable mentions per §3.

Each flip is a deliberate, reviewable change to the deploy-values file — which is
exactly the change-friction the ticket asks for (§5).

---

## 5. SLA / escalation config with change-friction

DEVOPS-814: *"SLA settings should have controlled mutability — configurable but with
change friction."* Gas Boat has no separate SLA service; the timings that matter for
escalation live in two places, and the friction comes from **where** you store them:

### 5.1 The two SLA surfaces

- **JIRA-side SLA goals** — JSM SLA configuration on the new cluster (owned by the
  JIRA admin; compressed timings per 798 §1.2 / §3.3 for fast escalation testing).
- **Gas Boat-side timings** — gardener/escalation cadence: schedule cron expressions,
  decision-reflag windows, alert severities (e.g. the `prometheusRule.resumeLatency`
  block in the deploy-values). These are per-instance and set aggressively low in
  staging so escalations surface within one test session.

### 5.2 Where to store them so changes carry friction

Put **both** SLA surfaces under version control, not in a mutable runtime store:

- Gas Boat timings → the staging **deploy-values YAML** in this repo. Any change is a
  branch + MR + pipeline + review — config-as-code is the friction.
- JIRA SLA goals → capture the intended goals as a committed reference doc /
  `jira-sla-staging.md` next to this runbook, so a change to the JSM config is
  preceded by a doc MR describing the new target and why.

### 5.3 Recommended friction controls (config-as-code)

1. **No runtime mutation.** Forbid editing SLA timings via live `kubectl edit` /
   ad-hoc API calls; the source of truth is the committed deploy-values. A drift
   check (deployed values vs git) makes runtime edits visible.
2. **CODEOWNERS on the SLA surfaces.** This repo currently has **no** `CODEOWNERS`
   file. Add one that requires a named human reviewer (Matthew Baker / Sky Bennett)
   for the staging deploy-values file and the SLA reference doc, so SLA changes
   cannot merge without sign-off. This is the highest-leverage friction control and
   is a concrete follow-up (see §6).
3. **Two-surface consistency note.** When a JIRA-side goal changes, the same MR (or a
   linked one) must update the Gas Boat-side timing, and vice-versa — they are meant
   to stay consistent (798 §3.3). Encode this as a checklist item in the MR template
   for SLA changes.

> The intent of "controlled mutability" is met by making SLA values *editable by
> anyone via an MR* but *un-editable without one* — friction without a lockout.

---

## 6. Open items / follow-ups

| Item | Owner | Note |
|---|---|---|
| Issue the new-cluster JIRA token and create `gardenio-jira-bridge-creds` | Sky Bennett | token home site MUST be the new cluster (§1.1) |
| Stand up the gardenio-in-a-box instance (AWS-poc) | Matthew Baker | 798 §3 install path |
| Add `CODEOWNERS` requiring human sign-off on staging deploy-values + SLA doc | Matthew Baker | the core SLA friction control (§5.3) — none exists today |
| Sanitized 1-week ticket replay set | Lee Morgan | strip PHI/customer ids before recreating in sandbox |
| Decide write-back policy (`disableTransitions` / `enableClaimTransition`) | team | keep disabled until §2.3 replay validates classification |

---

### Cross-references

- [`devops-798-staging-env-setup.md`](./devops-798-staging-env-setup.md) — broad
  component provisioning (JIRA sandbox, on-prem GitLab, Slack, ingestion markdown).
- [`inbox-slack-enable-runbook.md`](../inbox-slack-enable-runbook.md) — Slack step.
- Real config: `gardenio/helm/gardenio/values.yaml` (`jiraBridge:` / `gitlabBridge:`),
  staging deploy-values YAML.
- Gating code: `gardenio/controller/internal/bridge/jira_poller.go` (`buildJQL`,
  `requiredLabels`), `jira_sync.go` (`disableTransitions`, `enableClaimTransition`).
