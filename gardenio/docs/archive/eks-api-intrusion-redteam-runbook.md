# EKS API intrusion detection — red-team validation runbook (detection #3)

Staging red-team validation for **IDP detection #3** — *successful call from an
UNKNOWN source IP* — the ★ highest-value rule of the intrusion-detection program
(IDP) that remediates audit finding **F1** (`ka-0000000578`). This runbook proves
the control actually fires: a **synthetic `kubectl` call made with a valid
credential from a NON-allowlisted egress must page the on-call team.**

- **Task:** `ka-0000006556` (Phase 3 — harden & validate, epic `ka-0000000495`).
- **Plan:** `ka-0000007128` (`/plans`). **Design of record:** `ka-0000003550`.
- **Detection #3 rule (Phase-2 LogsQL):** `ka-0000006485`.
- **Umbrella alert→triage→response runbook:**
  [`intrusion-detection-runbook.md`](../intrusion-detection-runbook.md) — read that
  for what a detection *means* and how to *respond*; read this one to **validate
  that #3 fires**.
- **Phase-1 alert-group card:**
  [`eks-api-intrusion-runbook.md`](./eks-api-intrusion-runbook.md).

---

## 1. What detection #3 is (and why validating it matters)

Detection #3 is the detective analog of the CIDR allowlist F1 could **not** narrow
(narrowing it froze golden promotion for three tags — RC1, `ka-0000004055`). Where
we cannot *prevent* reach to the public `prod-gardenio-eks` API, we *detect* abuse
of it. #3 is the single most valuable rule because it catches the exact residual
risk a public endpoint carries: **a leaked, working credential exercised from an
attacker's location.**

**The predicate (`EKSApiFromUnknownIP`, `severity: page`):**

> An **authenticated** (valid, non-anonymous credential), **successful**
> (`responseStatus.code` < 400 — a 2xx/3xx that the API server actually served)
> request to the EKS API whose **`sourceIP` is OUTSIDE the known-good egress set**
> (RWX static/NAT, operator VPN, agent NAT box, cluster node/pod CIDRs —
> `ka-0000002105`).

All three axes are load-bearing, and flipping any one of them turns the page off:

| Axis | Red-team (page) | Benign (silent) |
|---|---|---|
| **credential** | valid, authenticated | `system:anonymous` → detection **#1**, not #3 |
| **outcome** | successful (`< 400`) | denied (`4xx`) → detection **#1/#2**, not #3 |
| **egress** | source IP ∉ known-good set | source IP ∈ known-good set (a real CI/deploy job) |

**Why it fails safe.** The known-good egress set is a **soft alerting input**, not
a terraform-enforced firewall. A missing legitimate egress produces a **benign
false-positive page** (annoying — add the range, downgrade), *never* a locked-out
runner (an outage). That is the property that makes this control RC1-safe: no
network path is ever touched.

---

## 2. Deployment state — why this is offline-proven today, live-fire later

Detection #3 is a **Phase-2 LogsQL** rule over **VictoriaLogs**, which is **not yet
deployed** (only VictoriaMetrics + VictoriaTraces run; the CloudWatch → VictoriaLogs
transport and the LogsQL rule are the Phase-2 build, `ka-0000007666` /
`ka-0000006485`). So a live-fire test against a real cluster **cannot run today.**

This runbook therefore has two halves:

1. **§3 — Offline validation (available now, CI-enforced).** A synthetic
   kubectl-call fixture + an executable reference oracle that encodes detection
   #3's exact predicate and proves the red-team call classifies as a **page** and
   every benign control stays **silent**. This is the "verify it triggers the
   detection" acceptance proof that does *not* depend on the Phase-2 pipeline.
2. **§4 — Live-fire staging procedure (run when the pipeline lands).** The exact
   commands to inject the *same* synthetic audit record into staging and confirm a
   real Slack page, once VictoriaLogs + the LogsQL rule are deployed.

The synthetic record is **identical** across both halves, so the offline oracle is
a faithful stand-in until the live path exists, and the live-fire step is a
drop-in confirmation — not a fresh test to design under incident pressure.

---

## 3. Offline validation (available now)

### 3.1 The synthetic kubectl call

The red-team artifact is a synthetic EKS audit record for a `kubectl` call the
crown-jewel `ci_deploy` cluster-admin credential makes **from `203.0.113.44` — an
address outside the known-good egress set** — and which the API server **serves
(`200`)**:

```
verb=list  resource=secrets  namespace=gardenio-system
user.username=ci_deploy  (arn: …:role/ci_deploy, groups: system:masters)
sourceIPs=[203.0.113.44]           ← NOT in the known-good egress set
userAgent=kubectl/v1.30.0 …
responseStatus.code=200            ← the credential WORKED
```

This is the canonical stolen-key signature: the deploy identity used **outside a
deploy job, from an attacker's location, successfully.** The full record — plus a
second red-team shape (`pods/exec`) and three benign controls — lives in
`gardenio/helm/gardenio/tests/eks_api_intrusion_redteam_events.jsonl` (deleted with the chart feature, gd-R4Ao2mKEwZ; see git history).
Each record is tagged with `_expect` (`page` | `silent`) and a `_why` rationale.

The synthetic known-good egress allowlist it is tested against is
`gardenio/helm/gardenio/tests/eks_api_intrusion_redteam_egress.txt` (deleted with the chart feature, gd-R4Ao2mKEwZ; see git history)
— RFC5737/RFC1918 placeholder ranges, deliberately never a real production egress.

### 3.2 The reference oracle & how to run it

`TestEKSApiIntrusion_Detection3_RedTeamSyntheticKubectlCall` (in
`gardenio/helm/gardenio/tests/eks_api_intrusion_redteam_test.go`, deleted with the chart feature, gd-R4Ao2mKEwZ; see git history)
encodes detection #3's predicate (`matchesDetection3`) as an executable oracle and
asserts every fixture record classifies as its `_expect`. A companion test,
`TestEKSApiIntrusion_Detection3_PredicateBoundaries`, mutates the canonical
red-team record one axis at a time and proves flipping **any** single axis
(egress → in-set, outcome → 403, credential → anonymous) turns the page off.

Run it:

```sh
cd gardenio/helm/gardenio/tests
GOWORK=off go test -count=1 -run TestEKSApiIntrusion_Detection3 -v ./...
```

Expected: both tests **PASS** — the synthetic non-allowlisted call pages, every
benign control is silent. This runs in CI on every MR + main push via the
`test-helm` task (`.rwx/ci.yml`), so a regression that weakened detection #3's
predicate would red the pipeline.

> **Keep the oracle in lockstep with the live rule.** `matchesDetection3` is the
> reference implementation of the same success/authenticated/egress predicate the
> LogsQL rule (`ka-0000006485`) encodes. If that rule's predicate changes, change
> the oracle to match — otherwise the offline proof drifts from the live control.

---

## 4. Live-fire staging procedure (run when the Phase-2 pipeline lands)

Once VictoriaLogs + the CloudWatch→VL transport + the LogsQL detection #3 rule are
deployed to staging (`ka-0000007666`, `ka-0000006485`), replay the **same**
synthetic record end-to-end and confirm a real Slack page. Prefer **injecting the
synthetic audit record** over making a genuine unauthorized API call: it exercises
the full detection → alert → Slack path without needing an attacker-controlled
egress or a real cluster-admin action.

### 4.1 Preconditions

- Detection #3 LogsQL rule deployed and enabled in the staging overlay
  (`eksApiIntrusionAlerts.enabled=true` for the metric path; the LogsQL rule wired
  to vmalert via the vlogs datasource).
- The known-good egress set (`ka-0000002105`) loaded in staging **and it does NOT
  contain `203.0.113.44`** (confirm — the whole test is that this IP is unknown).
- Slack routing to the security channel wired (`ka-0000002447`); confirm the
  Phase-1 alerts already deliver there.
- A staging-safe way to write into VictoriaLogs (the CloudWatch→VL transport, or a
  direct VictoriaLogs insert for the synthetic record).

### 4.2 Inject the synthetic record

Push the red-team record from the fixture into the staging VictoriaLogs stream the
LogsQL rule reads (adjust the endpoint / stream labels to the deployed transport):

```sh
# Extract just the red-team page case from the fixture.
RECORD=$(jq -c 'select(._case=="redteam-page")' \
  gardenio/helm/gardenio/tests/eks_api_intrusion_redteam_events.jsonl)

# Insert into staging VictoriaLogs (JSON line protocol). Stream labels must match
# what the CloudWatch→VL transport tags EKS audit records with so the LogsQL rule
# selects it. Example endpoint — replace with the staging vlsingle service.
echo "$RECORD" | curl -s -X POST \
  'http://gardenio-vlsingle.gardenio-rwx.svc:9428/insert/jsonline?_stream_fields=log_group&_time_field=requestReceivedTimestamp&_msg_field=_case' \
  -H 'Content-Type: application/stream+json' --data-binary @-
```

> Injecting the record proves the **detection → alert → Slack** path. To also
> exercise the **audit-capture → transport** path end-to-end, drive a genuine
> synthetic call in a throwaway staging cluster from a non-allowlisted egress (a
> temporary NAT / bastion whose IP is deliberately absent from the egress set) —
> but never against prod, and never with a real long-lived credential.

### 4.3 Expected result

Within one evaluation interval + the rule's `for:` hold (≈ the `for:` window; the
Phase-1 groups evaluate at `30s`), the on-call team is **paged** in the security
Slack channel with `EKSApiFromUnknownIP` (`severity: page`), carrying the five
triage facts (principal `ci_deploy`, verb `list` on `secrets`, sourceIP
`203.0.113.44`, the kubectl userAgent, the timestamp) and the `runbook_url`
annotation pointing at the umbrella runbook.

**Confirm two ways** (advice `ka-0000005924`): the Slack page **and** the alert
showing active in vmalert / Alertmanager (`ALERTS{alertname="EKSApiFromUnknownIP"}`
or the vlogs-datasource query the rule runs). If they disagree, the delivery path
— not the detection — is the bug.

### 4.4 Negative control

Repeat §4.2 with the `benign-allowlisted` record (same successful call from
`198.51.100.17`, inside the RWX known-good range). Detection #3 must stay **silent**
— no page. This proves the control is high-signal, not a blanket alert on every
successful call. The `benign-unknown-but-denied` and `benign-node-in-vpc` records
are the other two silence controls.

### 4.5 Cleanup

- The synthetic record is a log entry; let it age out of VictoriaLogs retention, or
  delete it if the transport supports targeted deletion.
- If you drove a genuine call, tear down the throwaway cluster / temporary egress
  and rotate any credential that was used (even a synthetic one) per the umbrella
  runbook §5.3.
- Silence any alert you fired during validation once confirmed, and note the run in
  the Phase-3 tuning bead (`ka-0000002508`).

---

## 5. Re-run cadence

Re-run the **offline** oracle (§3) automatically on every MR (CI `test-helm`), and
the **live-fire** procedure (§4) after any change to:

- the detection #3 LogsQL rule (`ka-0000006485`) or its predicate,
- the known-good egress set (`ka-0000002105`),
- the CloudWatch→VictoriaLogs transport, or
- the Slack alert-routing path (`ka-0000002447`).

A detective control that is never exercised silently rots; this is the
dead-man's-switch's peer (`ka-0000003782`) for the *rule* rather than the
*pipeline*.

---

## 6. Caveats & assumptions

- **Field shape.** The synthetic record uses the `audit.k8s.io/v1` Event shape from
  the design of record. Confirm the field paths against a real EKS audit sample
  once the log-read IAM grant lands (`ka-0000002311`) and update the fixture +
  oracle if they differ.
- **IPv4 only.** The oracle and egress fixture handle IPv4 CIDRs. If the EKS ENIs
  ever carry IPv6 client addresses, extend both the known-good set and
  `matchesDetection3` — an unhandled IPv6 source would currently be treated as
  unparseable and therefore page (fail-safe, but noisy).
- **Multiple source IPs.** `sourceIPs` can carry a proxy chain. The oracle pages if
  **any** entry is outside the set (conservative). The live LogsQL rule now uses
  the SAME any-outside semantics: the audit-transport (`ka-0000006176`,
  `controller/internal/auditxform`) EXPANDS a multi-IP record to one row per source
  IP, each carrying a scalar `client_ip`, so `stats by (client_ip)` evaluates the
  allowlist independently per IP — a foreign IP anywhere in the chain fires
  regardless of its position. This is the same fix that makes `ipv4_range` work at
  all: VictoriaLogs stores the raw `sourceIPs` `[]string` as an opaque JSON-string
  that `ipv4_range` cannot filter (a no-op), so detection #3 queries the enriched
  scalar `client_ip` (chart default `eksApiIntrusionLogs.fields.sourceIP=client_ip`),
  proven end-to-end in `auditxform_e2e_test.go`.
- **Synthetic ≠ prod egress.** The fixture allowlist is placeholder ranges. Never
  copy it into a deploy overlay; the live set is `ka-0000002105`.

---

## 7. Related beads

- **Finding F1:** `ka-0000000578` · audit `ka-0000003522`.
- **Plan:** `ka-0000007128` — Phase 3 epic `ka-0000000495`.
- **Design of record:** `ka-0000003550` (bundle `ka-0000007676`).
- **Detection #3 LogsQL rule:** `ka-0000006485` · Phase-2 epic `ka-0000007666` ·
  VictoriaLogs deploy `ka-0000007261` · forensic surface `ka-0000007948`.
- **Known-good egress set:** `ka-0000002105`.
- **Slack routing:** `ka-0000002447` · **measure-two-ways:** `ka-0000002458`
  (advice `ka-0000005924`) · **dead-man's-switch:** `ka-0000003782` · **threshold
  tuning:** `ka-0000002508` · **audit-field grounding:** `ka-0000002311`.
- **Umbrella runbook:** [`intrusion-detection-runbook.md`](../intrusion-detection-runbook.md) ·
  **Phase-1 card:** [`eks-api-intrusion-runbook.md`](./eks-api-intrusion-runbook.md).
- **RC1 outage precedent:** `ka-0000004055` / `ka-0000002512`.
