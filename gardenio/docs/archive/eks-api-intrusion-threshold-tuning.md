# EKS-API intrusion-detection threshold tuning (Phase 3)

> Task `ka-0000002508` — *"Tune thresholds against a 7-day baseline of normal
> fleet+CI traffic to reach zero false positives"* (success criterion of the IDP
> design-of-record `ka-0000003550`, Phase 3 epic `ka-0000000495`).

This document records **how each detection's threshold was tuned toward zero
false positives**, the evidence it was tuned against, and the exact procedure to
**re-derive every threshold from the live 7-day baseline** once the
CloudWatch→exporter→vmsingle pipeline is emitting the contract series. It is the
companion to the runbook (`eks-api-intrusion-runbook.md` §*Silence / tuning*):
the runbook says *tune in Phase 3*; this doc *is* that tuning.

All tuning is **values / tfvars only** — never the PromQL, the LogsQL, or the
metric-filter template. The knobs are `eksApiIntrusionAlerts.*` (Phase 1),
`eksApiIntrusionLogs.*` (Phase 2), and the terraform
`eks_audit_*` variables (the metric-filter producer).

---

## The constraint that shaped this tuning

At the time of this pass the **audit-metric pipeline is not yet emitting a
baseline**: the in-chart `cloudwatch-exporter` is off, `gardenio-vmsingle` carries
**zero** `eks_audit_*` series, and the agent role is IAM-denied on `logs:*` /
`cloudwatch:*` over the audit log group (the Phase-0 IAM grant, MR !6444, had not
landed). So a naive "read 7 days of the counters and pick the 99.9th percentile"
was not yet possible — the same wall the red-team validation (`ka-0000006556`)
hit for detection #3.

Two things are nonetheless true and let this tuning be **substantive, not
deferred**:

1. **One class of false positive is *structural*, not statistical** — it is
   provable from the cluster's RBAC + running controllers *today*, without any
   audit-counter history. That is the detection-#4 firehose below, and it is
   fixed here.
2. **The volume thresholds (#1/#2/#6) are the ones that genuinely need the live
   histogram.** For those we ship a **defensible conservative starting point + a
   CI-enforced "normal baseline stays silent" proof + the exact re-derivation
   procedure**, so the moment the pipeline emits data the final numbers are a
   mechanical read, not a redesign.

---

## Detection #4 — sensitive-verb page — the structural false positive (CLOSED BY SPLIT)

> **Status resolution (2026-08-08 Stage-2 split, `ka-0000008111`).** Detection #4
> was **split into two metric filters** so the crown-jewel signal can page at real
> fidelity without the noise floor:
>   - **`sensitive_verb_exec`** (`exec`/`attach`/`portforward`, NO principal
>     exclusion) → contract `eks_audit_sensitive_verb_exec_total`, alert
>     `EKSApiSensitiveVerbExec`, severity **page**. Measured alone this is 720
>     events/24h, 64% of 5m windows ZERO, max 27 — so it arms at `>30` with real
>     crown-jewel fidelity. This REFUTES the 2026-08-03 "option B alone is
>     insufficient" finding, which only held while exec was buried under the
>     secrets floor.
>   - **`sensitive_verb_secrets`** (secrets get|list + CRB write, WITH the benign
>     principal exclusion) → contract `eks_audit_sensitive_verb_secrets_total`,
>     alert `EKSApiSensitiveVerbSecrets`, severity **ticket** at a volume-anomaly
>     threshold. The exclusion still cannot enumerate the ephemeral
>     `mr-<slug>:setup-jobs` class (below), which is exactly why this branch
>     tickets rather than pages — the page-worthy secrets discriminator is the
>     source IP (detection #3), not this counter.
>
> **Earlier status correction (2026-08-03 go-live).** An earlier revision of this
> section was titled *"(FIXED)"* and claimed *zero false positives* for the
> combined detection #4. The **live producer went on in prod at 2026-08-03 20:44**
> (operator, escalation `ka-0000005309`) and the measured baseline **invalidated
> that claim**: the principal-exclusion mechanism below closes the *enumerable
> controller* class it was measured against on 2026-08-02, but the **dominant**
> residual is a **non-enumerable** principal class that exact-match exclusion
> structurally cannot express. That measurement is what motivated the Stage-2
> split above (interim Stage-1 arming at combined `>2000` per `ka-0000005309`,
> then the split at `ka-0000008111`). The control remains off-by-default and inert
> in the base chart; the prod overlay arms it.

**Detection #4 is the only detection that PAGES**, and its CloudWatch metric
filter (`SensitiveVerbRequests`) is **dimensionless at threshold `0`** — it
counts *every* principal. It matches `pods/exec|attach|portforward`, `secrets
get|list`, and `clusterrolebindings create|patch|update`.

On the live fleet, two of those are **continuous benign traffic**:

| Benign source (measured 2026-08-02, `prod-gardenio-eks`) | Sensitive verb it emits | Evidence |
|---|---|---|
| `external-secrets-controller` | `secrets get`/`list` (dominant) | ClusterRole `external-secrets-controller` grants `secrets [get,list,watch,create,update,delete,patch]`; **244** ExternalSecrets on-cluster, some `refreshInterval=10s` |
| `cert-manager` controller | `secrets get`/`list` | ClusterRole `cert-manager-controller-certificates` grants `secrets [get,list,watch,…]` |
| `acme-operators-reloader` | `secrets list`/`get`/`watch` | ClusterRole `acme-operators-reloader-role` grants `secrets [list,get,watch]` |
| gardenio controller (`gardenio-agents-sa`) | `clusterrolebindings create` | one per ephemeral `gardenio-rwx-<sha>` namespace spin-up — **220+** `*-agent-cluster-admin` CRBs observed, Helm-managed |

An un-scoped `sensitiveVerbs: 0` therefore pages **continuously on normal
traffic** — infinite false positives on the one detection that pages. No amount
of *threshold* raising fixes it without also blinding the real signal (a single
`secrets list` from a stolen key is exactly what must page), because the noise
and the signal share the same coarse counter. The fix is **scoping, not
thresholding**.

### The fix (covers the enumerable controller class only)

The metric-filter pattern subtracts a **measured** set of benign
controller/deploy principals from **only** the `secrets`-read and
`clusterrolebindings`-write sub-clauses — the `exec`/`attach`/`portforward`
clauses are **never** scoped (they page for any principal, always).

This closes the *enumerable, stable-SA* half of the noise. It does **not** close
the non-enumerable half the live producer later revealed — that is a structural
limit of exact-match exclusion, recorded in
*[The measured prod residual](#the-measured-prod-residual-non-enumerable-2026-08-03)*.

- Terraform: `var.eks_audit_sensitive_verb_excluded_principals`
  (`eks_audit_metric_filters.tf`), seeded in `prod-real.tfvars`:

  ```hcl
  eks_audit_sensitive_verb_excluded_principals = [
    "system:serviceaccount:external-secrets:external-secrets",
    "system:serviceaccount:cert-manager:cert-manager",
    "system:serviceaccount:operators:acme-operators-reloader",
    "system:serviceaccount:gardenio-system:gardenio-agents-sa",
  ]
  ```

- The exclusion is **factored** — AND-joined *once* onto the grouped
  secrets/CRB sub-clauses, not repeated per verb — so the pattern stays under
  CloudWatch's **1024-char** metric-filter limit as the list grows (4 principals
  ≈ 795 chars; per-clause repetition overflowed at ≈ 1248).

### Why this cannot open a blind spot (the safety argument)

A stolen credential still pages, three ways over:

1. **A different principal** doing `secrets get|list` or a CRB write is *not* in
   the exclusion set → still counted → pages. (`attack-stolen-key-secrets-list`,
   `attack-crb-create-by-nonexcluded` fixtures.)
2. **An excluded principal doing `exec`/`attach`/`portforward`** → the exclusion
   never touches those clauses → pages. A compromised `external-secrets` SA
   opening an exec is the highest-severity case and can *never* be silenced.
   (`attack-exec-by-excluded-principal`, `attack-attach-by-excluded-principal`.)
3. **An excluded principal from an *unknown source IP*** → caught independently
   by **detection #3** (`EKSApiFromUnknownIP`, the ★ page) — a wholly separate
   pipeline. (Detection #3's measure-two-ways cross-check
   `EKSApiCrossCheckDivergence` is a *pipeline-health* corroboration, not the
   discriminator here; its derivation-B producer is now built — the CloudTrail
   trail `ka-0000004765` (honesty parent `ka-0000004700`) — but off by default
   until the operator-gated derivation-B go-live, so on a Phase-1-only env it is
   still measure-*one*-way. Either way the page-worthy signal for this case is
   detection #3 itself.)

The excluded identities are **stable in-cluster controller/deploy SAs**, not
human or external identities, so the set is small and slow-changing.

### Fail-safe default

The terraform default is an **empty** exclusion list ⇒ *nothing* is subtracted ⇒
every sensitive verb counts. A brand-new env (or any env that has not yet
measured its benign principals) fails **toward the page**, never toward silence.
Seeding the list is a safe, deferrable refinement — a missing entry costs a
benign page, never a blind spot.

### Proof (CI-enforced)

`tests/eks_api_intrusion_sensitive_verb_test.go` reads the **actual** pattern out
of the terraform (so the proof cannot drift from what deploys), models the
CloudWatch filter's boolean semantics, and replays
`eks_api_intrusion_sensitive_verb_events.jsonl`:

- `TestSensitiveVerb_TunedFilterZeroFalsePositives` — every benign controller
  record is **silent**, every attack signature **pages**.
- `TestSensitiveVerb_ExecNeverExcluded` — even with *every* fixture principal
  excluded, `exec`/`attach`/`portforward` still page (crown-jewel invariant).
- `TestSensitiveVerb_FailSafeEmptyExclusionCountsEverything` — the empty-set
  default counts everything.
- `TestSensitiveVerb_ExcludedPrincipalsMatchProdTfvars` — the oracle's set stays
  in lockstep with `prod-real.tfvars`.
- `TestSensitiveVerb_PatternWithinCloudWatchLimit` — the rendered pattern is
  under 1024 chars.
- `TestSensitiveVerb_ExactMatchExclusionCannotCoverEphemeralNamespaces` — the
  **honest negative**: the exclusion mechanism *cannot* silence the measured
  `mr-<slug>:setup-jobs` residual, so the repo can never silently re-claim
  zero-FP-at-page for detection #4 (see the residual section below).

> **Scope of the CI proof.** These tests prove the exclusion *correctly subtracts
> the enumerable controllers it is given* and *never touches exec/attach/portforward*.
> They do **not** — and cannot — prove *zero false positives in prod*, because the
> dominant prod noise comes from a principal class the mechanism structurally
> cannot enumerate. That gap is the subject of the next section and is itself
> guarded by the negative test above.

### The measured prod residual (non-enumerable), 2026-08-03

When the operator applied the producer to prod and let it publish
(`GardenioSecurity/EKSAudit`, escalation `ka-0000005309`), the **live** baseline
showed detection #4 firing far above threshold `0` on **routine** traffic even
*with* the four exclusions above in effect:

| Metric (5-min window, `for: 5m`) | Shipped threshold / severity | Measured prod baseline |
|---|---|---|
| `eks_audit_sensitive_verb_requests_total` (#4, combined — since SPLIT `ka-0000008111`) | `0` / **page** | **~95–270 per window, continuously** (this is the noise that motivated the split; now `eks_audit_sensitive_verb_exec_total` max 27 / page-at-30, `eks_audit_sensitive_verb_secrets_total` carries the floor / ticket) |
| `eks_audit_unauthorized_responses_total` (#1) | `20` / ticket | 46 in the observed window |
| `eks_audit_anonymous_requests_total` (#1) | `0` / ticket | 0 (OK) |
| `eks_audit_auth_failures_total` (#2) | `10` / ticket | 0 (OK) |

Logs-Insights attribution over the residual `secrets get|list` stream (the four
excluded controllers already subtracted):

```text
system:serviceaccount:external-secrets:external-secrets     get  secrets   10184   (excluded ✓)
system:serviceaccount:external-secrets:external-secrets     list secrets    4961   (excluded ✓)
system:serviceaccount:gardenio-system:gardenio-agents-sa      get  secrets    1280   (excluded ✓)
system:serviceaccount:mr-repro-fsm-485-harness:setup-jobs   get  secrets     696   ← NOT excludable
system:serviceaccount:mr-repro-fsm-484-harness:setup-jobs   get  secrets     696   ← NOT excludable
system:serviceaccount:mr-agent-thread-330759-pe-1048:setup-jobs get secrets  696   ← NOT excludable
… 10+ more mr-<branch-slug>:setup-jobs namespaces at ~690/h each
```

Plus the crown-jewel verbs are themselves routine for the CI cluster-admin:

```
arn:aws:iam::123456789012:user/gardenio  get pods/portforward          70/h
arn:aws:iam::123456789012:user/gardenio  get pods/exec                 20/h
arn:aws:iam::123456789012:user/gardenio  create clusterrolebindings     4/h
system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin  get pods/portforward  1/h
```

**Why exact-match exclusion cannot cover this (structural, not a tuning knob):**

1. **The `mr-<slug>:setup-jobs` class is unbounded.** The namespace segment is a
   *per-MR branch slug*, new on every merge request. `var.eks_audit_sensitive_verb_excluded_principals`
   holds **exact, full** `system:serviceaccount:<ns>:<sa>` strings, and a
   CloudWatch metric-filter string comparison is a **full-value** match unless it
   contains a `*` — and a `*` here would widen into an unsafe prefix match
   (`eks_audit_metric_filters.tf` deliberately forbids it, see the local's header
   comment). An exact-match list can **never enumerate** a class whose members are
   generated per-MR. The terraform comment anticipated ephemeral
   `gardenio-rwx-<sha>` namespaces and excluded `gardenio-agents-sa`, but the class
   actually running in prod is `mr-<slug>:setup-jobs` — a *different*, non-enumerable
   principal set.
2. **Excluding `secrets`-read alone is insufficient anyway.** `user/gardenio` — the
   `ci_deploy` cluster-admin that the alert's own annotation calls *"the canonical
   stolen-key signature"* — `exec`/`portforward`s ~90×/h as normal operation. The
   Phase-1 dimensionless counter **cannot separate routine CI cluster-admin
   activity from a stolen key by verb alone.** The discriminator has to be the
   **source IP / deploy context**, which is **detection #3** (the Phase-2 LogsQL
   `EKSApiFromUnknownIP` ★ page over VictoriaLogs, `eksApiIntrusionLogs.*`), **not
   this counter**.

**Implication for arming (RESOLVED by the Stage-2 split, `ka-0000008111`).**
Re-scoping the *combined* metric-filter pattern was confirmed insufficient by the
exec/portforward numbers — but **splitting** it is not re-scoping. The operator
resolved the `ka-0000005309` fork in two stages (2026-08-08):

- **Stage 1** — arm the *combined* counter at an empirical volume-anomaly
  threshold (`sensitiveVerbs: 2000`, still `page`), so the control goes live
  without permanent pager noise while the split lands. (Shipped as
  `feat(security): arm the EKS-API intrusion detective control in prod`.)
- **Stage 2** (this split) — separate the clean exec signal (pages at `>30`) from
  the un-pageable secrets floor (tickets at a volume-anomaly threshold). The
  crown-jewel exec case now pages at real fidelity; the secrets case leans on the
  **Phase-2 source-IP plane (detection #3)** as its page-worthy discriminator,
  with `EKSApiSensitiveVerbSecrets` as the volume-anomaly ticket + attribution.

Design constraint of record: `ka-0000002508` (comment 2026-08-03); go-live fork:
`ka-0000005309`; Stage-2 split: `ka-0000008111`. **Re-baseline** the split
`sensitive_verb_secrets` series ~24-48h before tightening its threshold below the
carried-over interim `2000` (the secrets-only histogram has not been observed yet).

---

## Detections #1 / #2 — volume thresholds (starting points + live re-derivation)

These are the counters that genuinely need the live histogram. The endpoint is
**public (`0.0.0.0/0`)**, so a background of internet 401/403 scanning is
*expected* and the threshold must sit **above** that background but **below** a
real burst.

| Threshold (`eksApiIntrusionAlerts.thresholds.*`) | Value | Window | Meaning |
|---|---|---|---|
| `anonymousRequests` | `0` | 5m | any non-probe `system:anonymous` request (filter already excludes `/healthz` `/livez` `/readyz` `/version`) — a real anonymous reach is always noteworthy, keep at 0 |
| `unauthorizedResponses` | `20` | 5m | 401/403 burst — conservative floor above expected internet-scan noise on a public endpoint. **Chart default only; the prod overlay carries `300` after the `system:anonymous` split — see [Live prod re-derivation](#live-prod-re-derivation-of-unauthorizedresponses-ka-0000004977) below.** |
| `authFailures` | `10` | 5m | 401 authentication-failures — credential-probing floor |

**These 0 / 20 / 10 are deliberately conservative starting points, not the final
tuned numbers.** They are chosen so a *quiet* normal baseline is silent (proven
below) while any meaningful burst still fires. The final numbers are a mechanical
read off the live baseline:

### Re-derivation procedure (run once the pipeline emits `eks_audit_*`)

1. Confirm the contract series exist and the dead-man's-switch heartbeat is live:
   `count(eks_audit_events_total)` > 0 in `gardenio-vmsingle`.
2. Over a **7-day** window of *normal* fleet+CI traffic (no incident, no
   red-team), compute the per-5m-window distribution of each series. NOTE these
   series are non-monotonic windowed-Sum GAUGES, so read them with
   `max(max_over_time(<series>[5m]))`, NOT `sum(increase(...))` — increase()
   inflates them 1.3-2.5x by mis-reading each gauge fall as a counter reset
   (ka-0000005736):
   ```promql
   quantile_over_time(0.999, max(max_over_time(eks_audit_unauthorized_responses_total[5m]))[7d:5m])
   quantile_over_time(0.999, max(max_over_time(eks_audit_auth_failures_total[5m]))[7d:5m])
   ```
3. Set each threshold to **ceil(p99.9) + headroom** (headroom ≈ the max single
   legitimate CI burst you observe, so a heavy but benign deploy window does not
   page). Record the observed max and the chosen value in this table.
4. **Measure two ways** (advice `ka-0000005924`): cross-check the 401 rate
   against the authenticator-stream count and CloudTrail auth failures before
   trusting it — a divergence is a pipeline bug, not a baseline.
5. Land the new numbers in the **deploy-values overlay**, never the template, and
   re-run the promtool proof.

### Live prod re-derivation of `unauthorizedResponses` (`ka-0000004977`)

The procedure above was executed against `prod-gardenio-eks` on **2026-09-02**. It
produced a result the procedure itself does not anticipate, so it is recorded
here in full: **this series could not be tuned as one number, and the fix was to
split the series, not to pick a better threshold.**

**Step 2 — the raw 7-day distribution.** Read straight off `gardenio-vmsingle`
(`kubectl port-forward -n gardenio-system svc/gardenio-vmsingle 8428`) with the
rule's own reducer, 7d `2026-08-26T12:00Z .. 2026-09-02T12:00Z`, n=**2017**
complete 5-minute windows:

| series | min | p50 | p90 | p95 | p99 | p99.9 | max |
|---|---|---|---|---|---|---|---|
| `eks_audit_unauthorized_responses_total` (raw) | 262 | 299 | 326 | 371 | 641 | 841 | **3377** |

An 11x spread. `ceil(p99.9) + headroom` off that is ~900-1000 — looser than the
interim `500` it was meant to replace, and useless on a P0 detective control.

**Why: two different populations in one counter.** Two structural contributors
had to come out before the number meant anything.

1. **The strimzi noise floor (removed at source, not tuned around).** ~93% of the
   pre-fix floor was one broken RBAC binding — `strimzi-cluster-operator` 403ing
   on `get endpoints` in ns `kafka` ~250x/window, 24x7 (bug `ka-0000004947`).
   **Fixed 2026-09-02 12:55Z** (helm release `acme-operators` → chart
   `operators-0.1.12`). Measured live across the transition, 5-min windows:
   `302-305` steady → `86` / `25` → a new floor of **18-22**.

2. **The `system:anonymous` scanner population (the finding that mattered).** The
   open question was whether the *spikes* were real credential-probing worth
   paging. Settled by joining this counter window-for-window against
   `eks_audit_anonymous_requests_total` over the same 7 days — no CloudWatch
   access needed, because the anonymous population already has its own counter:

   | series, same 7d, n=2017 | p50 | p99 | p99.9 | max |
   |---|---|---|---|---|
   | `unauthorized_responses` (raw) | 299 | 641 | 841 | 3377 |
   | `unauthorized_responses` **minus** `anonymous_requests` | 296 | 381 | 489 | **508** |

   Subtracting one principal collapses a 3377 tail to 508 and an 11x spread to
   1.7x. The 3377 peak (`09-02 07:30Z`) was `3119` anonymous + the ~300 strimzi
   floor; its successor window (2753) was `2501` anonymous. Every window whose
   excursion over its local 1h baseline exceeded +400 was anonymous-dominated.
   This is **internet background scanning against the public `0.0.0.0/0`
   endpoint**, every request correctly answered 403 — the same benign population
   `anonymousRequests: 100` was already tuned against.

**The fix — split the filter (source-side), then tune.** Shipped in
`eks_audit_metric_filters.tf` + `prod-real.tfvars`: the `unauthorized_responses`
metric-filter pattern is split so `var.eks_audit_unauthorized_excluded_principals`
(prod: `["system:anonymous"]`) is AND-joined onto the **403 branch only**.

- **No coverage is lost.** `EKSApiAnonymousReach` is
  `anonymousRequests > 100 OR unauthorizedResponses > T`, and `100` sits far
  below any anonymous burst that could ever have moved `T` — clause 1 is
  *strictly the more sensitive* detector of exactly these events. This removes
  double-counting, not coverage. (Contrast the detection-#4 exclusions, which do
  trade a little coverage for tunability.) If detection #1's anonymous clause is
  ever removed or raised above ~500, revisit the exclusion in the same change.
- **The 401 branch stays unguarded** — the absent-field footgun in its sharpest
  form. A 401 is by definition a request the *authentication* layer rejected, so
  it often carries no `$.user.username`, and a CloudWatch `!=` on an absent field
  **drops the datapoint**. Guarding the whole group would silently stop counting
  401s, blinding the detection exactly where credential probing appears. Same
  structural remedy detection #7 uses for cluster-scoped `ingressclasses`.

**Step 3 — the derived distribution and the chosen value.** What remains after
the split is the authenticated-403 population. Its post-strimzi-fix distribution
is reconstructed over the full 7 days from measured quantities: take
`(unauthorized − anonymous)` per window, subtract the strimzi steady component
(implied **277**/window = pre-fix authenticated p50 `296` − measured post-fix
floor `19`; the bug's own audit-log attribution independently put strimzi at
~250/window), and clip at the measured post-fix floor.

| reconstructed post-split, post-fix, n=2017 | min | p50 | p90 | p95 | p99 | p99.9 | max |
|---|---|---|---|---|---|---|---|
| authenticated 401/403 | 18 | 19 | 35 | 48 | 104 | 212 | **231** |

Windows over 231: **0 / 2017** — zero false positives over 7 days.

**Measure two ways (step 4, advice `ka-0000005924`).** The reconstruction is
validated against the live post-fix data that *does* exist —
`2026-09-02 13:10Z..15:20Z`, n=27 windows, with `anonymous_requests ≈ 0`
throughout, so that window **is** the authenticated-only series directly:
measured `min 18 / p50 19 / max 43`, against a reconstruction predicting
`p50 19` / `p95 48`. The two independent derivations agree.

**Chosen: `unauthorizedResponses: 300`** in the prod overlay (was the interim
`500`). ~1.3x the reconstructed 7d max, ~15x the floor, 0/2017 false positives,
and it catches a genuine auth storm an order of magnitude sooner than 500 did.

**Refuted for the record: `20-40` is NOT safe**, despite earlier guidance in
`ka-0000004947` predicting the post-fix baseline would "collapse to ~11-20". The
post-fix floor is `18-22` — *at* the shipped default of 20 — and the
authenticated series alone exceeds 40 in ~7% of windows (~20/day) and 100 in
~1.2%. Even in the 2h10m directly measured after the strimzi fix, 2 of 27 windows
hit 43.

**Apply order — mandatory.** `300` is safe **only** with the metric-filter
exclusion live; with anonymous still counted the post-fix series would exceed 300
in ~2.4% of windows (~7/day). The exclusion is a **root-module** change
(`eks_audit_metric_filters.tf` + `prod-real.tfvars`); the overlay threshold is
applied via `-target=module.gardenio`. **Apply the root change first, or both in
one full apply — never the overlay alone.** `500`-with-old-filter and
`300`-with-new-filter are each safe; `300`-with-old-filter is not.

**Honest limits.**

- The distribution is a 7-day *derivation*, not 7 days of *elapsed* post-fix
  data (the strimzi fix is hours old). **Re-read the direct 7d distribution
  on/after 2026-09-09** and confirm `max ≲ 231`; the two numbers agreeing is the
  acceptance test. If the direct read comes in higher, **raise** 300 first.
- Per-principal attribution of the residual *floor* remains unavailable: the
  agent role is IAM-denied on both `logs:DescribeLogGroups` and `logs:StartQuery`
  over `/aws/eks/prod-gardenio-eks/cluster` (re-probed live 2026-09-02 — real
  denials), and the EKS audit log is not in VictoriaLogs. Spike attribution did
  not need it; floor composition still does.
- The `18-22` floor is believed to be mostly `eks:az-poller` 403ing on
  non-allowlisted `kube-system` leases (~15/window, derived from a 1,080/6h audit
  figure in `ka-0000004977`, **not** re-measured). Both its `Role` and
  `ClusterRoleBinding` are `eks:`-prefixed and reconciled by the AWS control
  plane, so it is not ours to fix. If the floor ever needs to go lower, exclude
  that principal on the 403 branch the same way — do **not** lower the threshold
  toward it.

### Proof — a normal baseline is silent (CI-enforced)

`tests/eks_api_intrusion_rules_test.yaml` (driven by
`TestEKSApiIntrusion_SyntheticDetectionsFire`) includes:

- `quiet_baseline_is_silent` — anonymous+sensitive flat at 0; 401/403 and
  auth-failures hold at a low benign event count (`max_over_time[5m] == 5`, under
  `>20` / `>10`) — **nothing fires**.
- a `normal_fleet_baseline_is_silent` case (added by this task) — a *heavier*,
  more realistic fleet shape (steady benign 401/403 + auth retries near, but
  under, the thresholds) that must still be silent, so the zero-false-positive
  contract is exercised at the tuning boundary, not just at zero.

---

## Detections #3 / #5 / #6 — Phase-2 LogsQL (VictoriaLogs)

These evaluate over VictoriaLogs, which only just came up; the transport
(`ka-0000007889`) is still landing, so there is no live LogsQL baseline yet.
Tuning knobs and starting points:

| Threshold (`eksApiIntrusionLogs.thresholds.*` / `.knownGoodCIDRs`) | Value | Meaning + tuning note |
|---|---|---|
| `unknownSourceIP` (#3) | `0` | any authenticated success from outside `knownGoodCIDRs` pages — the ★ rule; keep at 0. Tuning is **the allowlist, not the threshold** |
| `knownGoodCIDRs` (#3) | `[]` → seed per env | soft allowlist (`ka-0000002105`). Empty ⇒ every success alerts (fail-safe). Seed RWX/NAT/VPN/node egress before enabling on a busy cluster |
| `distinctAsns` (#5) | `1` | a 2nd source ASN for one principal in-window trips it. Needs the transport's ASN enrichment; inert until then |
| `perIpRequests` (#6) | `500` | per-IP list/watch volume floor — **this is the one most likely to need raising**, see the shared-NAT note |

### The shared-NAT caveat for #6 (per-IP enumeration)

Detection #6 counts requests **per source IP** and does **not** subtract
`knownGoodCIDRs`. The agent fleet egresses through a **small set of shared NAT
boxes** (`prod-real.tfvars` `nat_mode=instance`, per-AZ) *and* nodes/pods reach
the API from in-VPC — so **many** legitimate clients collapse onto **one** source
IP. The aggregate list/watch volume from behind a shared NAT can be large and
benign. When the live baseline lands, `perIpRequests` must be set above the
**observed peak per-NAT-IP list/watch rate**, or #6 will false-positive on the
fleet's own NAT egress. Re-derive exactly like #1/#2:

```promql
# once #6's source_ip breakdown is queryable in VictoriaLogs, take the p99.9 of
# per-IP list/watch counts over 7d and set perIpRequests above it + headroom.
```

Until then `500` is a conservative-but-untuned floor; treat a #6 alert as
"confirm it is not just NAT aggregation" per the runbook.

---

## Summary of what this task changed

| Surface | Change | File |
|---|---|---|
| Detection #4 SPLIT (`ka-0000008111`) | one filter → two: `sensitive_verb_exec` (no exclusion, pages at 30) + `sensitive_verb_secrets` (exclusion, tickets at volume anomaly) | `terraform/…/eks_audit_metric_filters.tf`, chart `values.yaml`, `templates/eks-api-intrusion-alerts.yaml` |
| Detection #4 scoping | benign-principal exclusion var, applied to the secrets/CRB filter only, factored under the 1024-char limit | `terraform/…/eks_audit_metric_filters.tf`, `variables.tf` |
| Detection #4 prod seed | measured 4-principal exclusion set + split thresholds (exec 30 / secrets 2000 interim) | `terraform/…/prod-real.tfvars`, overlay `eks-api-intrusion-values.yaml` |
| Detection #4 proof | oracle: benign silent / attack pages / exec never excluded (now structural — separate filter) / lockstep with tfvars / char-limit | `tests/eks_api_intrusion_sensitive_verb_{test.go,events.jsonl}` |
| #1/#2 baseline proof | `normal_fleet_baseline_is_silent` promtool case | `tests/eks_api_intrusion_rules_test.yaml` |
| Tuning method-of-record | this document + runbook cross-link | `docs/eks-api-intrusion-threshold-tuning.md`, `docs/eks-api-intrusion-runbook.md` |

The volume thresholds (#1/#2/#6) carry a documented, mechanical **re-derivation
procedure** so that when the 7-day live baseline exists the final numbers are a
read, not a redesign — closing the success criterion in two stages the same way
the rest of Phase 3 (red-team #3, dead-man's-switch) shipped its offline proof +
live-fire procedure ahead of the pipeline.
