# ses-support-inbound

Mail to `support@lands.clonk.tv` reaches the operator's mailbox (Lands Track B §6.1,
gd-UBeJ3kVicQ). It is item 3 of the mailer rollout escalation gd-o4fbw9qUJc, which the laptop
deferred to terraform. It is for people who cannot sign in; everyone else asks in their land's
`#support`. The operator's handling is in
[`docs/lands/support-runbook.md`](../../../docs/lands/support-runbook.md#mail-to-support).
The composition instance is `module.ses_support_inbound`.

```diagram
sender ──MX──▶ SES inbound (us-east-1) ── rule "support" (support@ only, spam + virus scan)
                 ├─ 1. S3 s3://gardenio-support-mail-<acct>/support/<message-id>  (expires 30 d)
                 └─ 2. Lambda gardenio-support-forward
                        └─ ses:SendRawEmail as support@lands.clonk.tv ──▶ operator mailbox
                           From "<sender> via support", Reply-To the sender
```

## What it creates

| Resource | Notes |
|---|---|
| `aws_route53_record.mx` | `lands.clonk.tv` MX `10 inbound-smtp.us-east-1.amazonaws.com` |
| `aws_ses_receipt_rule_set.this` + `aws_ses_active_receipt_rule_set.this` | `gardenio-inbound`, made **the** active rule set (SES has one per region) |
| `aws_ses_receipt_rule.support` | recipient `support@lands.clonk.tv` only; `scan_enabled`; S3 action, then Lambda action (`Event`) |
| `aws_s3_bucket.mail` (+ public access block, owner-enforced, SSE-S3, lifecycle) | `gardenio-support-mail-<account>`; raw mail expires after `retention_days` (30); policy lets only `ses.amazonaws.com` put under `support/`, for this account's rule; TLS only |
| `aws_lambda_function.forward` | `scripts/support-forward/support_forward.py`, python3.13, no VPC |
| `aws_iam_role_policy.forward` | logs; `s3:GetObject` on `support/*`; `ses:SendRawEmail` on the one identity, `ses:FromAddress` = `support@lands.clonk.tv` |
| `aws_lambda_permission.ses` | only this account's support rule may invoke it |
| `aws_cloudwatch_metric_alarm.errors` | Lambda `Errors` ≥ 1 in 5 min → the edge-probe SNS topic (the operator's email) |

What the forwarder does with one message:

- **Clean or spam:** re-sent to the mailbox. Spam is tagged `[spam?]` in the subject, not
  dropped: a tenant's first mail from a new address is often scored badly.
- **Virus:** not re-sent. The operator gets a short notice (sender, subject, checks, S3 key).
- **SES refuses it** (over 10 MB, a malformed header): the same notice, so nothing is lost.
- **Our own forward coming back** (a loop): dropped.

`X-Gardenio-Support-Verdicts` on each forward carries SES's spam, virus, SPF, DKIM and DMARC
verdicts. A failing SPF/DMARC means the From may be forged: check before acting on it.

## OPERATOR steps (verbatim order)

Prerequisite: `module.ses_lands` is applied and `lands.clonk.tv` is verified (its README,
steps 1-2).

```bash
# 1. Nothing else may own the active rule set: applying this one replaces it.
aws ses describe-active-receipt-rule-set --profile gardenio --region us-east-1
# want: empty output (no active set), or Metadata.Name == gardenio-inbound

# 2. Nothing else may receive mail for lands.clonk.tv.
dig +short MX lands.clonk.tv       # want: empty

# 3. The forward mailbox. It is lands_support_forward_to in the untracked terraform.tfvars,
#    or, when that is unset, edge_probe_alert_email. While SES is in the sandbox it must be
#    a verified identity, or every forward is refused:
aws sesv2 create-email-identity --profile gardenio --email-identity <mailbox>
#    then click the link AWS mails to it.

# 4. apply from a tree equal to origin/main
./scripts/tf-gardenio.sh plan  -target=module.ses_support_inbound   # expect 16 to add, 0 to change, 0 to destroy
./scripts/tf-gardenio.sh apply -target=module.ses_support_inbound
```

Acceptance:

1. From an outside address, mail `support@lands.clonk.tv` with the subject `test 1`. Within a
   minute the mailbox gets `[support] test 1` from `<you> via support <support@lands.clonk.tv>`,
   and replying to it addresses the outside sender.
2. Mail `nobody@lands.clonk.tv`: it bounces (no rule takes it).
3. `aws s3 ls s3://gardenio-support-mail-<account>/support/ --profile gardenio` lists the test
   message.
4. The alarm `gardenio-support-forward-errors` is `OK`.

## Rollback

`./scripts/tf-gardenio.sh apply -destroy -target=module.ses_support_inbound`. The MX goes with
it, so mail to `support@` bounces again. Destroying the bucket fails while it holds mail:
empty it first (`aws s3 rm --recursive s3://gardenio-support-mail-<account>/`) if you mean to
drop what was received.

## Tests

`terraform test` (mock provider, plan only; no credentials): the MX target; the rule takes only
`support@`, scans, and stores before it forwards; the bucket is private, TLS-only, expiring, and
writable by SES only for this account's rule; the forwarder role has exactly the listed actions,
no wildcard action, sends only as `support@` on the one identity and reads only `support/*`; only
the rule may invoke it; the alarm exists only with a topic. The forwarder's own tests:
`python3 scripts/support-forward/support_forward_test.py`. CI: `checks.yml` terraform and
script-test jobs.
