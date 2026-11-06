# Tarboat — The Single On-Box Agent Prompt (G2.5)

> **Tarboat T2 / G2.5 deliverable — task ka-0000004969, epic ka-0000007759.**
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
>
> This file holds **the single prompt** an operator pastes into a raw Claude Code instance
> running **on the box** to kick off the on-box driving agent. It is the operator's whole job:
> unpack the tarball, fill in three blanks, paste. The agent does the rest by executing
> [`TARBOAT-SETUP.md`](TARBOAT-SETUP.md) in order.
>
> The same text ships in the tarball as `SETUP-PROMPT.txt` (the G1.3 placeholder this fills).

---

## How the operator uses this (three steps, then paste)

1. **Unpack + verify the tarball on the box** (the prompt assumes this is done):

   ```sh
   cd /opt && zstd -d -c gardenio-tarboat-<calver>.tar.zst | tar -x
   cd gardenio-tarboat-<calver> && sha256sum -c SHA256SUMS    # every line must say "OK"
   ```

2. **Start a raw Claude Code instance on the box** with the in-box LiteLLM (or, for the very
   first bootstrap before inference exists, with whatever provider the operator's own Claude
   Code is configured for — see "Credential" below).

3. **Fill the three blanks** in the prompt below (`<BOX_IP>`, `<OAUTH_TOKEN>`,
   `<CALVER>` — or let the agent derive `<CALVER>` and `<BOX_IP>` itself) and **paste the whole
   block as the first message.** Nothing else is required.

---

## Credential: how the driving agent gets something to think with (G2.5)

This is the chicken-and-egg the gap analysis flagged: an agent driving the *first* install has
no in-box inference yet (the box it is about to build is what provides it). The deterministic
answer, baked into this prompt:

- The driving agent's **own** inference credential is supplied **to the Claude Code process**,
  not through the tarball. The operator launches Claude Code on the box with their Claude OAuth
  token (env `CLAUDE_CODE_OAUTH_TOKEN`), **or** the box has prior connectivity for that one
  session. **No provider secret is ever baked into the tarball** (gap G1.3 / G3.3).
- The **box's** inference — what on-box gardenio agents use after install — is a Claude OAuth
  token (`sk-ant-oat01…`) the operator pastes into the prompt as `<OAUTH_TOKEN>`. This org has
  **no** `sk-ant-api…` key, so the in-box LiteLLM holds no provider secret: it runs with
  `forward_client_headers_to_llm_api: true` and forwards the agent's OAuth bearer token upstream
  (TARBOAT-SETUP.md §7). Nothing is seeded as a Secret.

The two credentials are intentionally distinct: one bootstraps the *driver*, one provisions the
*box*. They are typically the **same** OAuth token, but the prompt keeps them separate so neither
is shipped in the tarball.

---

## THE PROMPT (copy everything between the lines, fill the three blanks, paste)

```
You are the on-box driving agent for a gardenio "tarboat" air-gapped install. You are running
as root on a fresh single EC2 box. Your job: take this box from the already-unpacked,
signature-verified tarboat tarball to a running, auth-gated gardenio with working in-box
inference — by executing the deterministic runbook, in order, with no improvisation.

INPUTS (the only things not in the tarball):
  BOX_IP              = <BOX_IP>          # the box's reachable IP; if blank, derive it (runbook §2.1)
  CLAUDE_CODE_OAUTH_TOKEN = <OAUTH_TOKEN> # the sk-ant-oat01… Claude OAuth token the in-box LiteLLM forwards upstream
  CALVER              = <CALVER>          # tarball calver; if blank, read it from MANIFEST (runbook §1.2)

THE RUNBOOK IS LAW. Open and follow exactly: /opt/gardenio-tarboat-<CALVER>/TARBOAT-SETUP.md
(or wherever the tarball was unpacked — find it with:
  ls -d /opt/gardenio-tarboat-* ). Execute its sections in this exact order, and do NOT skip,
reorder, or invent steps:

  §1  read CALVER from the tarball (never choose a version)
  §2  capture inputs; derive BOX_IP/DASHED_IP if not supplied (§2.1)
  §3  run the preconditions block — it must print "PRECONDITIONS OK"
  §4  stage offline assets — it must print "STAGING OK"
  §6.0 generate the local-CA TLS overlay — it must print "TLS OVERLAY OK"
  §6.1 bootstrap pass 1 (k3s + registry + operators); a deploy fail-fast here is EXPECTED
  §5  seed the edge-auth HMAC + LiteLLM provider secrets — it must print "SECRETS OK"
  §6.2 bootstrap pass 2 (deploy completes) — it must print "DEPLOY PASS-2 EXIT: 0"
  §7  in-box inference — it must print "INFERENCE OK"
  §8  acceptance — every probe must print "OK" and §8.5 must print "ACCEPTANCE SMOKE: GREEN"

DETERMINISM RULES (non-negotiable):
  1. Every step has an explicit success assertion in the runbook. A step is DONE only when its
     assertion prints the expected token (e.g. "STAGING OK", "DEPLOY PASS-2 EXIT: 0",
     "INFERENCE OK"). Verify it before moving on.
  2. If ANY assertion fails: STOP IMMEDIATELY. Do not improvise, do not work around it, do not
     reach the public internet (the box is --airgap-strict; a missing asset is a hard fail by
     design). Capture the failing command's output plus the logs the runbook §10 names
     (/var/log/gardenio-bootstrap.log, the failing pod's describe/logs, or the helm stderr) and
     report exactly what failed and where. A failure is a report, not a fix-it-yourself.
  3. Never edit any tracked file in /opt/gardenio/repo. Never choose an image tag or a version —
     CALVER pins everything.
  4. Treat CLAUDE_CODE_OAUTH_TOKEN as a secret: it is exported for the inference probe and the
     account bead only; never echo it, never write it to a tracked file, never put it in shell
     history beyond the commands the runbook §7 names. No provider-key Secret is created.

SUCCESS CRITERION (report this verbatim when reached):
  "TARBOAT INSTALL GREEN: <CALVER> on <BOX_IP> — all pods Running, acceptance smoke GREEN,
   one inference round-trip through the in-box LiteLLM proxy succeeded."

Begin now with runbook §1. Narrate each section briefly (which step, its assertion, pass/fail),
but let the commands do the work. Do not ask me questions unless an assertion fails and the
runbook §10 does not tell you what to attach.
```

---

## Why this prompt is deterministic and self-contained

- **Self-contained:** every command the agent runs lives in `TARBOAT-SETUP.md`, which is in the
  tarball next to this prompt. The prompt carries only the three operator inputs and the
  execution order. No external doc, no network lookup, no "use your judgment."
- **Deterministic:** the prompt names the exact section order, the exact success token for each
  step, and a single fail rule (stop + report). There is no branch where the agent picks
  between options — the four forks (Q1–Q4) were resolved into the runbook before the agent ever
  reads it (TARBOAT-SETUP.md §0).
- **No baked secret:** the tarball ships zero credentials. The driver's inference credential is
  on the Claude Code process; the box's OAuth token is pasted into the prompt and forwarded
  upstream by the in-box LiteLLM (no Secret is seeded). Neither travels in the tarball (G1.3 / G3.3).
- **Idempotent:** if the agent is interrupted and re-pasted the prompt, every runbook step is
  safe to re-run — staging is copy-over, secrets are apply, the bootstrap stages are
  individually idempotent, and the assertions re-confirm state rather than mutate it.

---

## Operator quick-reference (the absolute minimum)

```sh
# on the box, as root, after unpacking + verifying the tarball:
cd /opt && ls -d gardenio-tarboat-*          # confirm the unpacked dir
# launch Claude Code on the box, then paste THE PROMPT above with:
#   <BOX_IP>       -> the box's reachable IP (or leave blank to auto-derive)
#   <OAUTH_TOKEN>  -> the sk-ant-oat01… Claude OAuth token the box's LiteLLM forwards upstream
#   <CALVER>       -> e.g. 2026.161.18 (or leave blank to read from MANIFEST)
# then wait for: "TARBOAT INSTALL GREEN: ..."
```

Operator: matthew.baker@acme.example
