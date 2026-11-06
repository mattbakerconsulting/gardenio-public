# Release commit + tag signing (git-level)

> **Superseded (2026-10-27)** by [`releases-design.md`](releases-design.md) (gd-hls6BkIEiO). Neither `release.sh` nor `release-cut.sh` signs anything or takes a lock today, and the GitLab push rules are gone. Kept for history.

**Task:** ka-0000005321 (F4) · **Audit:** ka-0000004158 (CI/CD supply chain) · **Backlog:** ka-0000005893

> This is the **git-level** trust root: the release *commit* and *tag* that
> `scripts/release.sh` cuts. It is a separate control from **image signing**
> (cosign keyless, F1 — see [`image-signing.md`](image-signing.md)) and from the
> **air-gap bundle** GPG signing (F9). All three are independent supply-chain
> layers; this one binds "who cut this release ref".

## What this closes

Before F4, `scripts/release.sh` cut the release commit with a plain `git commit`
and the release tag with a **lightweight** `git tag` — neither `-S` nor `-s`, no
GPG/SSH signing anywhere in the release path (`grep -n 'git commit\|git tag'
scripts/release.sh`). The release ref is the pipeline's trust root: a tag push
triggers image builds, staging deploy, E2E, and a terraform-driven **prod**
rollout. With no signature, nothing cryptographically binds "this tag was cut by
a release-owner" — a tag pushed by anyone with branch/tag write, pointing at
attacker code, is indistinguishable from a genuine cut.

F4 has two halves:

1. **Sign** (in-repo, done): `release.sh` signs the release commit (`git commit
   -S`) and the release tag (annotated `git tag -s`) with an **SSH signing key**
   when one is provisioned. Fail-open — an absent/broken key logs a warning and
   the cut proceeds unsigned rather than wedging the release lane.
2. **Enforce** (GitLab, operator/Maintainer): register the public key on the bot
   account so GitLab marks the refs **Verified**, and turn on the
   `reject_unsigned_commits` push rule so an unsigned commit is rejected at push
   time. These are Maintainer-only (the fleet bot is Developer/30 → 403).

## Why SSH signing, not GPG

The release-owner pod (`gardener-release-owner`, the same ephemeral pod shape as
any crew) ships **`ssh-keygen` and git 2.43 but no `gpg` binary**. Git's SSH
signing backend (`gpg.format=ssh`, git ≥ 2.34) is therefore the only signer
available in-pod. It needs no keyring/agent/daemon — just an unencrypted private
key file — which suits an ephemeral, headless pod far better than GPG. (Keyless
cosign, used for *images* in F1, cannot sign a git object; it signs registry
artifacts against Fulcio/Rekor.)

## How the script signs (`scripts/release.sh`)

`configure_release_signing` runs once, right after the single-flight lock is
held, and resolves the key from the environment:

| Env var | Meaning |
|---|---|
| `RELEASE_SIGNING_KEY` | Path to an **unencrypted** SSH private key. |
| `RELEASE_SIGNING_KEY_B64` | base64 of the private key; materialised to a `0600` temp file that is shredded on exit (`trap`). Takes precedence over the path form. |

It then sets `SIGN_COMMITS` / `SIGN_GIT_ARGS` / `SIGN_STATUS`, and the commit +
tag step runs:

```sh
git -c gpg.format=ssh -c user.signingkey=<key> commit -S -m "chore: release <ver>"
git -c gpg.format=ssh -c user.signingkey=<key> tag    -s -m "release <ver>" <ver>
```

**Fail-open, never-hang, by design.** If no key is configured, or the key is
missing / encrypted / malformed, the cut proceeds **unsigned** with a loud
`>&2` warning — it never aborts. Rationale: the release lane is the fleet's only
path to ship a fix; a signing misconfiguration must not become a new way to
wedge it (same discipline as the deploy-failure breaker and the RWX timeouts).
The key is validated with `ssh-keygen -y -f <key> </dev/null`, which fails
**fast** on an encrypted/bad key rather than prompting — so a bad key can never
hang the cut waiting on a passphrase. Once the operator turns on
`reject_unsigned_commits` (below), an unsigned commit is rejected at **push**
time by GitLab — a loud, recoverable failure at the perimeter — so fail-open
here does not defeat enforcement.

The annotated tag (`-s`, vs the old lightweight tag) also gives the tag a proper
tagger + message. The release pipeline triggers on the ref **name** (`20*.*.*`),
so annotated-vs-lightweight is transparent to it.

### The unsigned-warning is expected until the key is provisioned

Until an operator provisions the key (next section), every cut logs:

```
=== Release signing: unsigned (no signing key configured — set RELEASE_SIGNING_KEY[_B64]) ===
    Proceeding UNSIGNED (fail-open). Provision a key via RELEASE_SIGNING_KEY[_B64] ...
```

That is the intended default-OFF posture (Audit-before-Enforce): the code is
live and ready; signing switches ON the moment the key secret exists.

## Operator setup (one-time, Maintainer required)

The bot holds **Developer (30)** on `example-org/gardenio` and gets a
**permanent 403** on `push_rule` and on registering account SSH keys via API —
these steps require a **Maintainer (40+)** session (see the sibling GAPS runbook
[`security/cicd-operator-verification-gaps.md`](security/cicd-operator-verification-gaps.md)).

### 1. Generate the release signing keypair

```sh
ssh-keygen -t ed25519 -C "gardenio-release-signing" -f ./gardenio-release -N ""
#   -> ./gardenio-release      (private — becomes the secret)
#   -> ./gardenio-release.pub  (public  — registered on GitLab, next step)
```

Use a **dedicated** key (not a login/deploy key) so it can be rotated
independently and its only capability is signing.

### 2. Provision the private key to release-owner pods (secret)

The private key is surfaced to `release.sh` via a project-bead secret. Add it to
the gardenio project bead's `secrets` field so it is injected into release-owner
pods as `RELEASE_SIGNING_KEY_B64` (base64 form — safe for a single-line env var):

```sh
# base64 the private key (one line)
base64 -w0 < ./gardenio-release        # -> <B64>

# declare the secret on the project bead (see docs/agent-operations.md §Secrets)
kd update <gardenio-project-bead> -f 'secrets=[{"env":"RELEASE_SIGNING_KEY_B64","secret":"gardenio-release-signing-key","key":"b64"}]'
```

(Store the key material in the backing secret store the `secret` name resolves
to; the bead only references it.) `release.sh` decodes it to a `0600` temp file
and shreds it on exit — the key never lands on disk unencrypted beyond the cut.

### 3. Register the PUBLIC key on the bot's GitLab account (Verified badge)

So GitLab shows the release commit/tag as **Verified**, add the **public** key as
a *signing* key on the account whose email the release commit uses
(`group_14176856_bot_…@noreply.gitlab.com`):

```sh
# Maintainer/owner session. usage_type=signing (NOT auth).
glab api -X POST user/keys \
  -f title="gardenio-release-signing" \
  -f usage_type="signing" \
  -f key="$(cat ./gardenio-release.pub)"
```

The commit email must match a verified email on that account for the Verified
badge to render; the SSH signing key binds the signature to the account.

### 4. Turn on the `reject_unsigned_commits` push rule (enforcement)

This is the enforcement half — it makes unsigned commits on the repo a **push
rejection**. Maintainer-only:

```sh
# project id 12345678 = example-org/gardenio
glab api -X PUT projects/12345678/push_rule -f reject_unsigned_commits=true
# (if no push_rule exists yet, POST instead of PUT)
```

Verify (part of the GAPS runbook, item 2):

```sh
glab api projects/12345678/push_rule | jq '{reject_unsigned_commits, commit_committer_check}'
# PASS: reject_unsigned_commits == true
```

**Sequencing matters.** Enable the push rule **only after** steps 2–3 are live
and at least one signed cut has landed green — otherwise the rule rejects the
next (still-unsigned) release commit and wedges the lane. Order: provision key →
confirm a cut signs (commit/tag show Verified) → then flip the push rule.

## Verifying a signed cut

After a signed release, confirm on GitLab (commit + tag show a **Verified**
badge) or locally:

```sh
# allowed-signers maps the signer email -> the public key
printf '%s namespaces="git" %s\n' \
  "group_14176856_bot_…@noreply.gitlab.com" "$(cat gardenio-release.pub)" > allowed_signers
git -c gpg.ssh.allowedSignersFile=allowed_signers verify-commit <release-sha>
git -c gpg.ssh.allowedSignersFile=allowed_signers verify-tag    <calver>
#   -> Good "git" signature for <email> with ED25519 key SHA256:...
```

## Rotation

1. Generate a new keypair (step 1).
2. Register the new **public** key (step 3) — keep the old one until all
   in-flight tags are verified, then remove it.
3. Update the secret (step 2) to the new **private** key.
4. Next cut signs with the new key automatically (fail-open means a rotation
   typo degrades to an unsigned cut + warning, not a broken lane).

## Tests

`scripts/release_test.sh` covers `configure_release_signing` in library mode
(no repo/network needed): a valid key signs; no key / missing path / encrypted
key / invalid base64 all fail **open** (unsigned, `SIGN_COMMITS=false`); and the
`RELEASE_SIGNING_KEY_B64` path is materialised and used. The commit/tag wiring is
exercised end-to-end against a throwaway repo (Good signature on both).
