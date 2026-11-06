# Box-config seeding — the air-gap equivalent of prod advice/type seeding

> **Task:** [ka-0000003598](https://gardenio.e2e.example.com/beads/ka-0000003598) (MVC M3).
> **Epic:** [ka-0000000011](https://gardenio.e2e.example.com/beads/ka-0000000011) — minimum-viable
> config/prompt subset for the boxed gardenio (the essence of the OSS release).
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example).
> **Locked inputs:** inventory + keep/drop rulings in
> [`gardenio/docs/tarboat/mvc-config-inventory.md`](tarboat/mvc-config-inventory.md);
> Q1–Q4 operator decisions (see that doc). This doc is the M3 **mechanism** — the
> HOW that installs the WHAT those rulings locked.

## Why this exists

Production gardenio carries a huge amount of configuration and prompting — ~74+ advice
items, role nudges, custom types, an agent system prompt — much of it ACME-internal (Jira
tokens, perf bastion, RDS roles, monorepo-dark, Mezmo, internal hostnames). A friend's
air-gapped box must inherit only the **minimal honest subset** that keeps a boxed gardenio
useful, and must **never** inherit the internal firehose.

Prod gets its advice seeded by living in the prod beads DB. A boxed gardenio starts with an
**empty** in-box beads DB — so the kept subset has to physically ride in the tarball and be
installed at first boot. That is what this mechanism does. It is the direct sibling of the
two seeds already in `scripts/inbox/`:

| seed | what it installs | script |
|---|---|---|
| image bundle | every runtime container image → in-box zot | `seed-bundle.sh` |
| model account | a `type=account` bead → in-box LiteLLM wiring | `seed-litellm-account.sh` |
| **box-config** | **the kept advice subset → in-box beads DB** | **`seed-config-dump.sh`** |

## Two defenses, two layers

The mechanism is defense-in-depth. An artifact reaches a friend's box only if it survives
**both**:

1. **Allow-list (selection).** [`scripts/inbox/seed-config-keeplist.tsv`](../../scripts/inbox/seed-config-keeplist.tsv)
   enumerates the exact kept subset — one row per artifact, `keep` or `keep-redacted`, with
   an audit note. The seed contains **only** these rows. Everything else prod carries is
   dropped **by omission** — never named, never shipped. This is the reviewable statement of
   *what ships*.

2. **Deny-list (redaction guard).** [`scripts/inbox/seed-redaction-denylist.txt`](../../scripts/inbox/seed-redaction-denylist.txt)
   is a fail-closed second defense: a list of internal-ref patterns (hostnames, AWS accounts,
   secret names, ACME system names, ACME workflow vocab). The build **re-scans every final
   body** against it and **fails on any match**. So even a mis-classified `keep` row that
   still names an internal ref can never leak — the guard drops it and fails the build.

This mirrors the gardenio-next config-mirror per-env deny-list (advice
[ka-0000007027](https://gardenio.e2e.example.com/beads/ka-0000007027)): *same defense —
"never sync a per-env / internal-only artifact" — different target* (a friend's box instead
of the gardenio-next mirror).

> **The guard earns its keep.** During M3 bring-up the guard caught two real leaks the M1
> first-pass had marked `keep` (ship verbatim): `ka-0000002418` still named a ACME ticket
> (`PE-10133`) and `ka-0000003508` named `JIRA_API_TOKEN`. Both were reclassified to
> `keep-redacted` so genericization strips the internal refs. A `keep` classification is a
> *hypothesis*; the guard is what makes it honest.

## The build pipeline (release-time)

[`scripts/inbox/build-seed-config.sh`](../../scripts/inbox/build-seed-config.sh) runs at
release time against a clean reference beads DB (`kd` pointed at it). It is
**extract → redact → guard → manifest → package**:

```
build-seed-config.sh --version <calver> [--out-dir DIR] [--allow-missing]
```

1. **Extract.** Read the keeplist. For each `advice` row, pull the bead via
   `kd show <id> --json` and reconstruct its body (`title` + `description`). The other
   families (`role-context`, `type`, `prefix`, `skill`) are metadata the box already carries
   in-binary / in-image — they ride the manifest for audit but seed no body. (Q3: prod seeds
   **zero** decision-template beads — the `gb decision` artifact-type vocabulary is
   binary-baked — so that family seeds nothing.)

2. **Redact.** For `keep-redacted` rows, `redact_text` genericizes the body: ACME
   hostnames → `example.internal`, the prod AWS account → `<your-aws-account-id>`,
   `gardenio-next` → `<your-config-mirror>`, `JIRA_API_TOKEN` → `<your-issue-tracker-token>`,
   etc. `keep` rows ship verbatim. `redact_text` and the deny-list are two views of the
   **same boundary** — every substitution must leave the body clean against the guard.

3. **Guard (hard gate).** `guard_scan` re-scans every final body against the deny-list,
   case-insensitively. **Any match fails the build.** The seed can never ship a leaked
   internal ref — this is the defense that makes Q4's chosen "pre-baked blob" honest.

4. **Manifest (reviewability).** Emit two reviewable sidecars so a human audits *what
   shipped* without cracking the seed:
   - `seed-manifest.tsv` — one row per artifact: `id · family · selector · disposition · status`.
   - `seed-redaction-report.md` — a human-readable table + tally (seeded / genericized /
     missing / dropped-leak).

5. **Package.** Write the seed as **JSON-lines** — `gardenio-seed-<calver>.jsonl`, one bead
   per line (`{seed_kind, id, type, title, body, labels}`) — plus a `.sha256`. JSON-lines,
   **not** a `pg_dump` binary: reviewable as a diff, restorable on any beads DB with
   `kd create`, no Postgres tooling on the box.

## The install (first boot, air-gapped)

[`scripts/inbox/seed-config-dump.sh`](../../scripts/inbox/seed-config-dump.sh) restores the
seed into the box's fresh beads DB, after the daemon is up and before the first agent spawn:

```
seed-config-dump.sh [--seed PATH] [--operated-by WHO] [--airgap-strict] [--force] [--dry-run]
```

- **Verify** the `.sha256` sidecar (offline integrity), then `kd create` one advice bead per
  JSON-lines record.
- **Idempotent.** A `type=config` **marker bead** (`box-config-seed applied <calver>`) records
  that the seed ran; a second invocation is a no-op (re-running bootstrap never double-seeds).
  `--force` re-seeds regardless (e.g. after wiping the DB).
- **`--airgap-strict`.** A missing or corrupt seed is a **hard fail** (same posture as the
  G1.1 staged-asset rule) — never a silent empty-config boot on a strict box. Without it, a
  missing seed is a soft skip so a non-strict box still boots.

## How it rides in the tarball (assembly)

[`scripts/tarboat/assemble-tarboat.sh`](../../scripts/tarboat/assemble-tarboat.sh) stages the
seed under `tarboat/seed/` via `--seed-archive`, alongside the OCI image bundle it already
carries:

```text
gardenio-tarboat-<calver>/
  images/   gardenio-bundle-<calver>.oci.tar.zst          (seed-bundle.sh consumes)
  seed/     gardenio-seed-<calver>.jsonl (+ .sha256)       (seed-config-dump.sh consumes)
            seed-manifest.tsv  seed-redaction-report.md    (reviewable sidecars)
  repo/     the gardenio tree (scripts + chart + values)
  ...
```

The reviewable sidecars are carried into `seed/` too, so a friend (or a reviewer) can read
*exactly what shipped* without cracking the seed. A tarball assembled without `--seed-archive`
simply boots with no seeded advice (fine for a non-strict box).

## Bootstrap wiring

[`scripts/bootstrap-ec2.sh`](../../scripts/bootstrap-ec2.sh) has an optional stage 6,
`seed_config()`, that runs **after deploy** (daemon up). It is **OFF by default**
(`SEED_CONFIG=1` to enable) because the restore needs `kd` reachable against the in-box
daemon, which a fully hands-off bootstrap can't assume. When on, `AIRGAP_STRICT` flows through
so a missing/corrupt seed hard-fails.

**Manual post-install** (the documented path — the box's daemon is reachable via port-forward):

```bash
# 1. point kd at the in-box beads daemon (e.g. via an SSH tunnel / port-forward)
k3s kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080 &

# 2. restore the seed (idempotent; --airgap-strict on a strict box)
scripts/inbox/seed-config-dump.sh \
  --seed /opt/gardenio/seed/gardenio-seed-<calver>.jsonl \
  --operated-by oidc:you@example.com
```

## Test / verify

Two unit test suites (wired into `make lint`), both hermetic — no kd, no daemon, no network:

- [`build-seed-config_test.sh`](../../scripts/inbox/build-seed-config_test.sh) — sources the
  build for its pure functions and exercises `redact_text` (every ACME specific genericizes
  **and** leaves the body clean against the shipped deny-list) + `guard_scan` (catches a
  planted leak, case-insensitive, skips comments, reports the matched pattern) + data-file
  integrity (deny-list EREs compile; keeplist shape; no DROP id smuggled into the allow-list).
- [`seed-config-dump_test.sh`](../../scripts/inbox/seed-config-dump_test.sh) — drives the real
  restore end-to-end against a **fake `kd` stub**, asserting: kept advice restored,
  idempotency (second run no-ops), `--force` re-seeds, `--airgap-strict` hard-fails on
  missing/corrupt seed, non-strict soft-skips, `--dry-run` creates nothing. This is the AC-3
  "all kept present, all redacted absent" check in miniature.

**End-to-end (AC-3), run during M3 bring-up against the live reference beads DB:**

```
$ build-seed-config.sh --version <cv> --out-dir seed/ --allow-missing
  done. 10 seeded / 10 genericized / 0 missing / 0 dropped.
$ grep -iE 'e2e\.dev\.acme\.ai|<prod-acct>|gardenio-next|mezmo|JIRA_API_TOKEN|DEVOPS-[0-9]' seed/*.jsonl
  → (no output) — zero deny-listed strings in the seed
$ sha256sum -c seed/*.sha256  → OK
```

All 10 kept advice present + genericized; every dropped/internal artifact absent by omission;
the guard blocked the two mis-classified leaks before they could ship.

## Maintenance

- **The keeplist is the contract.** M2 is long-lived — as stakeholders revisit keep/drop per
  row, edit `seed-config-keeplist.tsv`; the build re-derives the seed from it. Never
  hand-edit a generated `gardenio-seed-*.jsonl`.
- **The deny-list is fail-closed.** Add a pattern whenever a new internal-ref family appears;
  if a `keep-redacted` body legitimately names it, add the matching `redact_text` substitution
  in the same change (the `redact-clean-vs-deny` test enforces they stay in sync).
- **Re-run the Q3 enumeration each release** in case a future env adds DB-stored decision
  templates (today: zero).
