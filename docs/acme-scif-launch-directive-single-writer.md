# acme-scif launch directive — the single-writer invariant

> **Status:** documented invariant of record (B2 gap 3, task ka-0000005319);
> §5 updated with the shipped non-credential launch-time re-check (follow-up
> task ka-0000000388).
> **2026-10-31 (gd-Y4ZCGr2m2t):** the in-repo scif-portal
> (`gardenio/controller/cmd/scif-portal`) and its `internal/scif` and
> `internal/scifportal` packages were deleted (off in prod; operator decision
> gd-rZJdGewUse). The code paths cited below are now in git history only. The IAM
> invariant in §1 and the checklist in §4 still bind any `_launch/*` grant that
> remains in `site-deployment` TF.
> **Reading:** attribution-and-gate (Reading A). Decision ka-0000003143 resolved
> `defer`: proceed under Reading A for an end-to-end devel proof, revisit
> Reading-A-vs-B after the seam is live past devel acme. This doc describes the
> invariant Reading A relies on; it is **not** credential delegation (Reading B).
> **See also:** `docs/adr/human-attribution-propagation.md` (the `operated_by`
> attribution model this seam propagates), the `scif` package doc
> (`gardenio/controller/internal/scif`, deleted 2026-10-31 — see git history),
> and the audit design of record
> (bundle ka-0000002439 / doc ka-0000001260).

## 1. What the invariant is

> **The launcher trusts the launch directive's `approved_by` blindly on the
> entitlement dimension ONLY because the cross-account IAM grant makes the portal
> IRSA role the SOLE principal permitted to `PutObject` under `_launch/*`. Any
> widening of that write permission — a second principal, a wildcard, a broader
> resource — silently breaks the Gate-1 launch gate.**

Concretely, on a Gate-1 approve the scif-portal writes a launch directive to
`s3://<reports-bucket>/_launch/<request_id>.json` (SSE-KMS). The B2 launcher is a
consumer of that prefix: it reads each directive and calls `ecs:RunTask` on the
acme-scif task-def, injecting the directive's `approved_by` as the `APPROVED_BY`
container override so it lands in `meta.json.launch.approved_by` and, ultimately,
as the report bead's `operated_by`.

The launcher does **not** re-verify that `approved_by` names a human entitled to
launch. Its only entitlement check is structural: the directive exists under
`_launch/`, has a recognised `directive_version`, and carries a non-empty
`approved_by`. The authorization that "this human may launch this investigation"
was decided **upstream in the portal** — AVA-verified identity + tenant-scoped
group authz + four-eyes, in `decideRequest`
(`gardenio/controller/cmd/scif-portal/handlers_request.go`) →
`internal/scif/authz*.go`. The directive is the fail-closed **record** of that
decision, not a re-litigation of it.

That blind trust is sound iff nothing else can put a directive under `_launch/`.
If a second writer could forge a directive with an arbitrary `approved_by`, it
would launch a PHI-reading job that never passed a human — the launch gate would
fail **open**. The single-writer grant is therefore not a convenience; it is the
load-bearing enforcement of the entire invocation gate.

## 2. Why this design instead of credential delegation

The operator phrasing "launch acme-scif USING THE APPROVING USER'S CI
PERMISSIONS" (epic ka-0000000104, comment 121708) admits two readings:

- **Reading A — attribution-and-gate (adopted here).** The run is *authorized* by
  the portal gate and *attributed* to the approver end-to-end (`operated_by`
  flows approve → directive → ECS override → `meta.json` → report bead), while
  `ecs:RunTask` stays a single least-privilege launcher role inside the acme-scif
  boundary. This mirrors how CI attributes a pipeline to its triggerer while
  executing under a service identity.
- **Reading B — true per-user credential delegation.** `ecs:RunTask` executes
  under credentials literally scoped to each approver's own IAM/CI permissions
  (per-user roles or OIDC→STS federation keyed on the approver).

Reading A vs B is **deferred** (decision ka-0000003143). Gap 3 does **not** add
credential delegation. Reading A deliberately keeps credential-minting off the
PHI edge: there are no per-user IAM roles, no OIDC→STS federation, and the
`aws_iam_role.launcher` trust model (`lambda.amazonaws.com`) is unchanged. The
price of that simplicity is precisely this invariant — the gate's soundness rests
on the single-writer grant rather than on per-user credentials.

## 3. How the invariant is enforced (the T3 devel grant)

Enforcement is an IAM grant, not application logic — belt-and-suspenders with the
write-side prefix guard below:

1. **The cross-account grant (primary enforcement).** In the devel reports
   bucket policy + CMK key policy (`site-deployment` devel TF, task
   ka-0000002964, shipped in site-deployment MR !1159), **exactly** the portal
   IRSA role ARN is granted `s3:PutObject` + `kms:Encrypt`/`GenerateDataKey` on
   `_launch/*`; the launcher role is granted only `s3:GetObject` +
   `kms:Decrypt` on `_launch/*`. No wildcard principals, no second writer. This
   is the sole principal that can arm a launch, and it is CloudTrail-audited
   (mirrors OQ-D). The acme-scif ECS task role can read/write `report.md` /
   `meta.json` under the reports subtree but has **no** `_launch/*` write grant —
   the runner cannot arm its own next launch.

2. **The write-side prefix guard (defense in depth).** `withinLaunchPrefix` in
   `internal/scif/store.go` refuses any `WriteLaunchDirective` whose key is not
   under `_launch/` before any S3 call — so a bug or a crafted key in the portal
   itself can never write a directive outside the launch prefix (and, symmetric
   to the reports `withinPrefix` guard, never into the reports subtree).

3. **Idempotent, fail-closed arming (no strand, no duplicate).**
   `WriteLaunchDirective` PutObjects a deterministic key (`_launch/<id>.json`), so
   arming is naturally last-writer-wins. `decideRequest` writes the directive
   AFTER the bead flip and BEFORE the 200; if the flip succeeded but the write
   failed, the bead is `approved` with no directive — a state that would 409
   forever on retry. The narrow re-arm path (`launchDirectiveMissing` +
   `armLaunch`) re-writes the missing directive on a re-approve instead of 409ing,
   so a half-armed approve can never permanently strand the run. This keeps the
   invariant honest under failure: a directive that exists was written by the
   portal on a real approve, and no approve can leave the gate half-open.

## 4. What breaks the invariant (review checklist)

Treat any of the following as breaking the Gate-1 launch gate. Reviewers of
`site-deployment` devel/prod TF and of `internal/scif` must reject them unless the
Reading-A-vs-B decision (ka-0000003143) has been explicitly revisited:

- Granting `s3:PutObject` on `_launch/*` (or on the whole bucket via `*`) to any
  principal other than the single portal IRSA role — a second `Allow`
  statement, a wildcard principal, or a resource that widens past `_launch/*`.
- Granting the acme-scif ECS task role, the launcher role, or any agent/CI role
  write access under `_launch/`.
- Relaxing `withinLaunchPrefix` (or removing the guard) so the portal can write a
  directive outside `_launch/`.
- Making the launcher act on a directive without the structural checks
  (recognised `directive_version`, non-empty `approved_by`), or making it accept
  a directive from any prefix other than `_launch/`.
- Adding a code path that writes a launch directive from anywhere other than a
  passing `decideRequest` approve (e.g. a "manual re-launch" endpoint that skips
  the authz gate).

Because the launcher performs **no** entitlement re-check by design, none of the
above is caught at launch time — they fail **open** silently. The IAM grant and
the prefix guard are the only things standing between "a human approved this" and
"a PHI job ran." That is why widening the writer set is a security regression, not
a config tweak.

## 5. Launch-time re-check (defense in depth, SHIPPED — non-credential)

Gap 3 permits an OPTIONAL cheap launch-time re-check in the launcher, as
defense in depth over the single-writer grant. This section documents what was
**shipped** (B2 gap-3 follow-up, task ka-0000000388) and, importantly, why the
shipped form is **not** the bead-read originally sketched.

### 5.1 Why NOT a bead re-read

The obvious re-check is "before `RunTask`, read the referenced
investigation-request bead and verify `approval_status == approved` and
`approved_by` matches the directive." That is **infeasible against the merged
launcher and would be a net security regression**, for one decisive reason:

> **The launcher holds NO beads token, by deliberate design.** The 2026-07-21
> transport re-scope (epic ka-0000000104, 18:04) put the entire runner group
> (B2 launcher / C1 egress / C2) on an **S3-only** transport: it holds no beads
> `/ci` token and makes no beads-API call. The approval app is the sole
> `/ci`-token holder; it writes the launch directive to S3 and the launcher
> polls S3. The launcher's IAM policy (`launcher.tf`) grants only `ecs:RunTask`
> / `iam:PassRole` on the acme-scif roles, `ecs:List/DescribeTasks`, S3 on the
> `_launch/`+`_launch_receipts/` prefixes, and KMS on the reports CMK — **no
> Secrets Manager beads-token read and no beads egress path.**

Adding a bead read would mean minting and mounting a beads credential inside the
acme-scif trust boundary — putting credential material back on the PHI edge,
exactly what Reading A deliberately avoids (§2). It would therefore **reverse a
merged security decision** and reopen the deferred Reading-A-vs-B question
(decision ka-0000003143), not merely add "defense in depth." So the bead-read
form is explicitly **rejected**, not deferred.

### 5.2 What was shipped: an in-directive consistency re-check

The re-check the launcher CAN perform with **no credential and no network
call** is to verify the directive is an internally COMPLETE and SELF-CONSISTENT
approval record before `RunTask`, in `validate_directive`
(`site-deployment` `terraform/new-modules/acme-scif/lambda/launcher/handler.py`):

- **Non-empty `approved_at`.** A genuine Gate-1 approve (`armLaunch` →
  `WriteLaunchDirective`) always stamps the approve timestamp. An empty one
  means the object is not a clean approval record — refuse (fail closed).
- **`approved_by_email` agrees with `approved_by`** (case-insensitive) when the
  directive carries the explicit-email form. Both fields are copied from the
  single AVA-verified identity in `decideRequest`, so a disagreement means a
  partial/tampered/corrupted write — refuse. (The field is not *required*: a
  directive that omits it still validates on the strength of `approved_by`, for
  forward/backward compatibility.)

These join the pre-existing fail-closed checks (recognised `directive_version`,
required fields present, non-empty `prompt`, non-empty `approved_by`,
well-typed `data_scope`). All are pure structural/consistency checks over
fields already in the S3 object.

### 5.3 What this does and does NOT change

It is **defense in depth over the single-writer grant, NOT a substitute for
it**. The IAM grant is what stops a *forged* directive (a second writer with an
arbitrary `approved_by`); these checks catch a *malformed* one (a portal bug, a
truncated object, disagreeing identity fields) that the grant alone would let
through to `RunTask`. It changes **neither** the trust model **nor** the
Reading-A-vs-B boundary (decision ka-0000003143): it adds no credential, reads
no bead, and trusts the directive's `approved_by` on the entitlement dimension
exactly as before — it just refuses to act on a directive that is not a
coherent approval record.

The checks are exercised by the launcher's pure-python unit suite
(`test_handler.py`: `test_empty_approved_at_refuses`,
`test_mismatched_approved_by_email_refuses`,
`test_matching_approved_by_email_passes_case_insensitively`,
`test_absent_approved_by_email_is_allowed`), which runs in the CI validate
stage with no AWS access.
