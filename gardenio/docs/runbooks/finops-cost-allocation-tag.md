# FinOps cost-allocation tag activation — operator runbook

**Bead:** ka-0000002963 (wiring) · ka-0000006441 (DR + cost posture) ·
decision ka-0000007709 (chosen: `activate-tag`)
**Module:** `gardenio/terraform/modules/finops`
**Composition:** `gardenio/terraform/composition/canonical-prod/`
**Tag key:** `gardenio:component`
**Operator:** Matt Baker (slack:U0OPERATOR1)

## What this is

`modules/finops` provisions per-component AWS Budgets and a per-component Cost
Anomaly Detection monitor, both scoped by the `gardenio:component`
cost-allocation tag. AWS will **not** populate a tag-scoped budget until that
tag key is **activated as a cost-allocation tag** in the Billing console — an
account-root action with no Terraform resource.

Two things must both be true for per-component budgets to show real numbers:

1. **Resources carry the tag.** ✅ Done in code (ka-0000002963). The
   composition now assigns `gardenio:component` per module via
   `local.component_tags` in `locals.tf`. Before this fix, every resource was
   tagged only `gardenio:module`, which does **not** match the finops cost
   filters — so the component budgets would have read **$0 even after the
   operator activated the tag.** That gap is closed.
2. **The tag is activated in Billing.** ⬜ **This is the operator step below.**

The **account**, **data-transfer**, and the **services** anomaly monitor work
**without** any activation — they filter on built-in AWS dimensions, not on a
user tag. So you are protected on the global ceiling from day one; tag
activation only lights up the per-component breakdown.

## Operator checklist

> Cost-allocation tags can only be activated by the **management/payer account**
> (or a member account with delegated Billing access). They surface in the
> console **only after at least one resource carrying the tag exists** — so run
> the canonical-prod `terraform apply` first, then activate.

- [ ] **1. Confirm resources are tagged.** After `terraform apply`, spot-check
      that a resource carries the tag:
      ```sh
      aws resourcegroupstaggingapi get-resources \
        --tag-filters Key=gardenio:component \
        --query 'ResourceTagMappingList[].ResourceARN' --output text | head
      ```
      Expect ARNs across RDS, VPC/NAT, KMS, S3, EKS, etc. (Empty = apply hasn't
      run, or you're in the wrong account/region.)

- [ ] **2. Activate the tag in Billing.** Console:
      **Billing and Cost Management → Cost allocation tags → User-defined cost
      allocation tags →** select **`gardenio:component`** → **Activate**.
      (Also worth activating the other low-cardinality keys for Cost Explorer
      readability: `gardenio:env`, `gardenio:owner`, `gardenio:cost-center`,
      `gardenio:module`. Do **not** activate `gardenio:git-ref` — high
      cardinality, identification not billing.)

      The tag key only appears in this list **after** AWS has seen a resource
      using it (can lag a few hours past apply).

- [ ] **3. Wait for backfill.** Activation is **not retroactive to before the
      tag existed**, and Cost Explorer / Budgets data lags **up to ~24h**.
      A freshly-activated tag shows `$0` for the first day even when resources
      are correctly tagged — this is expected, not a wiring bug. Re-check after
      a full UTC billing day before concluding anything is wrong.

- [ ] **4. Confirm the budgets resolved.** After ~24h:
      **Billing → Budgets** → the `canonical-prod-gardenio-component-*` budgets
      should show non-zero actuals matching the cost model
      (`docs/research/canonical-prod-cost-delta.md`). Cross-check in
      **Cost Explorer → Group by → Tag: gardenio:component**.

- [ ] **5. Confirm SNS email subscriptions.** Each `budget_emails` /
      `operator_email` recipient must **click the confirmation link** AWS sends
      when the `<env>-gardenio-finops` SNS topic is created — until confirmed
      they receive nothing. Check **SNS → Subscriptions** for any
      `PendingConfirmation`.

## Component → cost bucket mapping

`local.component_tags` (in `locals.tf`) assigns each module's resources to a
budget bucket. These match the keys in `var.finops_component_budgets`:

| `gardenio:component` | Modules tagged | Default budget |
|---|---|---|
| `networking` | network, route53, acm (HA NAT + VPC endpoints + DNS/cert) | $270/mo |
| `rds` | rds (the beads DB line; incl. its PI/Enhanced-Monitoring spend) | $350/mo |
| `security` | kms-cmks, secrets-manager, edge-auth | $200/mo |
| `platform` | eks, platform addons, s3-sessions | $400/mo |
| `agent` | karpenter — propagates to launched Spot nodes; feeds `agent_spot` | $500 ceiling |
| `observability` | *(no dedicated resource — see note)* | $120/mo |

**`observability` is expected to read ~$0.** AWS-native observability spend
(CloudWatch ingest, RDS Performance Insights / Enhanced Monitoring) bills
against the *emitting* resource, so it lands under that resource's component
(mostly `rds` + `platform`). The `observability` budget is kept as a tripwire:
if a future stand-alone observability resource is added and tagged
`gardenio:component=observability`, the budget catches it automatically. If you
want zero noise, set `finops_component_budgets` without the `observability` key.

## Why the tag is assigned in the composition, not the modules

The base modules (`modules/network`, `modules/rds`, …) deliberately do **not**
bake in the canonical-prod cost taxonomy — they stay portable for any consumer.
The composition is the right layer to express "this env's `rds` module rolls up
to the `rds` cost bucket." Mechanically: every base module merges
`var.common_tags` into its own `local.module_tags`, so passing a per-module
`common_tags = local.component_tags.<bucket>` lands `gardenio:component` on all
of that module's taggable resources without touching any module's API.

`local.component_tag_key` **must stay in sync** with
`modules/finops/var.component_tag_key` (both default `gardenio:component`). If
you rename the key, change it in both places or the budgets silently stop
matching.

## If a component budget stays $0 after 24h

1. **Tag present on resources?** Run step 1's `get-resources` query filtered to
   the specific value, e.g. `Key=gardenio:component,Values=rds`. Empty → the
   module block isn't passing `local.component_tags.<bucket>` (check the
   relevant `*.tf`), or apply didn't run.
2. **Tag activated?** Billing → Cost allocation tags shows `gardenio:component`
   as **Active**, not Inactive.
3. **Backfill window?** Activation isn't retroactive and data lags ~24h — give
   it a full UTC billing day.
4. **Right account?** Cost-allocation tags activate in the **payer** account;
   if canonical-prod is a member account, activate from the management account
   (or via delegated Billing access).
