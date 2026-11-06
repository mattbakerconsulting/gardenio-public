# ses-identity

An SES sending domain with its DNS, plus the IAM user that Keycloak's realm SMTP signs in
with (gd-uBGCzwjlIh). The composition instance is `module.ses_lands`: domain
`lands.clonk.tv`, user `gardenio-keycloak-smtp`, From `lands@lands.clonk.tv`. It is the
prerequisite for the realm SMTP rollout (gd-ygZDTEs3Qt, chart PR #795), which public
GitHub registration needs: a GitHub registrant must verify their email through Keycloak's
VERIFY_EMAIL action, and that action needs realm SMTP.

## What it creates

| Resource | Notes |
|---|---|
| `aws_sesv2_email_identity.this` | domain identity, Easy DKIM, 2048-bit |
| `aws_route53_record.dkim[0..2]` | `<token>._domainkey.<domain>` CNAME `<token>.dkim.amazonses.com` |
| `aws_sesv2_email_identity_mail_from_attributes.this` | MAIL FROM `bounce.<domain>`; falls back to amazonses.com if the MX is missing |
| `aws_route53_record.mail_from_mx` | `bounce.<domain>` MX `10 feedback-smtp.<region>.amazonses.com` |
| `aws_route53_record.mail_from_spf` | `bounce.<domain>` TXT `v=spf1 include:amazonses.com ~all` |
| `aws_route53_record.dmarc` | `_dmarc.<domain>` TXT `v=DMARC1; p=quarantine` (plus `rua=mailto:` when `dmarc_rua` is set) |
| `aws_iam_user.smtp` | `/gardenio/<smtp_user_name>`; nothing attached but the policy below |
| `aws_iam_user_policy.smtp_send` | the ONE inline policy, below |

```json
{"Effect":"Allow","Action":"ses:SendRawEmail",
 "Resource":"arn:aws:ses:us-east-1:<acct>:identity/lands.clonk.tv",
 "Condition":{"StringEquals":{"ses:FromAddress":"lands@lands.clonk.tv"}}}
```

SES SMTP authorizes every message as `ses:SendRawEmail`, and SMTP auth takes an access key,
never a role, so this has to be a user (it is separate from the mailer's role,
gd-o4fbw9qUJc).

**Not here, on purpose:**

- `aws_iam_access_key`. Its secret and its `ses_smtp_password_v4` would sit in terraform
  state. The key is minted by hand (below).
- **SES production access.** It is an account-level AWS support request with a use-case
  statement, made by the owner. Until it is granted the account is in the sandbox
  (200/day), and mail reaches only addresses verified in SES.
- A configuration set and bounce/complaint SNS (gd-o4fbw9qUJc item 1). support@ inbound
  (item 3) is its own module, [`ses-support-inbound`](../ses-support-inbound/README.md). If a **default** configuration set is ever attached to this identity, SES also
  authorizes the send against the configuration-set ARN, so add that ARN to the policy's
  `Resource` in the same change or every send is AccessDenied.

## OPERATOR steps (verbatim order)

```
# 1. apply from a tree equal to origin/main (profile gardenio, us-east-1)
./scripts/tf-gardenio.sh plan  -target=module.ses_lands   # expect 10 to add, 0 to change, 0 to destroy
./scripts/tf-gardenio.sh apply -target=module.ses_lands

# 2. wait for the identity to verify (DKIM CNAMEs propagate in minutes, up to 72 h)
aws sesv2 get-email-identity --profile gardenio --email-identity lands.clonk.tv \
  --query '{verified:VerifiedForSendingStatus,dkim:DkimAttributes.Status,mailFrom:MailFromAttributes.MailFromDomainStatus}'
# want: verified true, dkim SUCCESS, mailFrom SUCCESS
```

3. Mint the key, derive the SMTP password, and create the Secret in one pipe, so neither
   the secret key nor the password touches argv, a file or the terminal. The SMTP password
   is **not** the secret key: it is derived with the SES SigV4 algorithm, and the region in
   the derivation must be the one Keycloak connects to. The username is the access key id.
   Create the Secret **before** the `keycloak.realm.smtp` values roll.

```
aws iam create-access-key --profile gardenio \
  --user-name gardenio-keycloak-smtp \
  --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text \
| python3 -c '
import base64, hashlib, hmac, json, sys
kid, sec = sys.stdin.read().split()
sign = lambda k, m: hmac.new(k, m.encode(), hashlib.sha256).digest()
k = sign(("AWS4" + sec).encode(), "11111111")
for m in ("us-east-1", "ses", "aws4_request", "SendRawEmail"):
    k = sign(k, m)
print(json.dumps({"apiVersion": "v1", "kind": "Secret",
  "metadata": {"name": "gardenio-keycloak-smtp", "namespace": "gardenio"},
  "stringData": {"username": kid,
                 "password": base64.b64encode(b"\x04" + k).decode()}}))' \
| kubectl create -f -
```

Use `kubectl create`, never `apply`: `apply` copies the whole object, `stringData`
included, into the `last-applied-configuration` annotation.

4. Roll `keycloak.realm.smtp` in `deploy-values/gardenio-k3s.yaml` (host
   `email-smtp.us-east-1.amazonaws.com`, port 587, STARTTLS, from `lands@lands.clonk.tv`,
   `auth.secretName: gardenio-keycloak-smtp`; the full block is on gd-ygZDTEs3Qt) with a
   chart that contains #795.

5. While still in the sandbox, verify the test recipient first
   (`aws sesv2 create-email-identity --email-identity <address>`), or wait for production
   access.

Acceptance: the realm's Email tab "Test connection" delivers; a send from any other From
address is rejected (`554 Access denied`).

## Rotation (every 90 days, or on any suspicion)

`aws iam list-access-keys --user-name gardenio-keycloak-smtp` shows key age. Create a second
key (a user holds at most two), run step 3 with `kubectl replace -f -` in place of
`kubectl create -f -`, let the Keycloak operator re-sync the realm (or restart it), check
"Test connection", then `aws iam update-access-key --status Inactive` on the old key. Delete
it once Keycloak has sent one mail on the new key; reactivate the old one if a send fails.

## Rollback

Set `keycloak.realm.smtp.enabled: false` and roll first. Then delete the access keys
(`aws iam list-access-keys` / `delete-access-key`; terraform does not own them), run
`./scripts/tf-gardenio.sh apply -destroy -target=module.ses_lands`, and
`kubectl -n gardenio delete secret gardenio-keycloak-smtp`.

## Tests

`terraform test` (mock provider, plan only; no credentials): the SMTP policy has one Allow
statement, action exactly `ses:SendRawEmail`, resource exactly the identity ARN, condition
exactly `ses:FromAddress` = the From address, and no wildcard anywhere (checked by
mutation); DKIM is 2048-bit with three CNAMEs; MAIL FROM, its MX and SPF, and DMARC (with
and without rua) are as above; a From address off the domain and a bad DMARC policy are
rejected. CI: `checks.yml` terraform job.
