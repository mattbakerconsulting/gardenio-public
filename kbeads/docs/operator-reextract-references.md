# Operator runbook: `kd admin reextract-references`

This is the one-time backfill that walks every bead and re-extracts ka-id
references from `title`, `description`, `notes`, and (when
`BEADS_REFERENCES_EXTRACT_COMMENTS=1`) every comment body, then writes the
results into `bead_references`. As of T4 of the typed-mention syntax epic
(`ka-0000003840`), the same pass also populates the new
`bead_references.relationship_type` column for every typed mention
(e.g. `parent(ka-X)`, `blocks: ka-Y`).

## When to run

Run once after deploying a release that includes:

1. **Migration `013_bead_references_relationship_type`** (T1) — adds the
   column and the extended unique index
   `(source_bead_id, target_bead_id, source_field, COALESCE(relationship_type,''))`.
2. **Typed parser `internal/references/typed.go`** (T2) — recognizes the
   canonical verb vocabulary.
3. **Write-path wiring** (T3) — emits typed rows on every new
   bead/comment going forward.

Without (3), inserts after the migration won't write typed rows; the
backfill catches up the historical data and any rows that went in
between (1) and (3). With (3), the backfill is still useful: it is the
authoritative way to bring rows that pre-existed (3) up to the typed
state.

The backfill is **idempotent** — re-running it after a transient failure
(or simply for paranoia) produces no diff. Each per-bead transaction
deletes the rows for that bead's text source_fields and re-inserts the
current extraction; typed rows use `ON CONFLICT DO NOTHING` against the
extended unique index.

## How to run

```sh
kd admin reextract-references
```

Optional flags:

- `--batch-size N` (default 100) — beads per server-side page. The server
  enforces a ~30s per-request deadline, and a 500-bead page (which also
  re-reads every comment on those beads) measured 25–30s on prod — sitting
  *just under* the ceiling, so any heavier page tipped over and the whole
  walk died with an opaque HTTP 500 (bug ka-0000001700). 100 completes
  cleanly with headroom. If a page still overruns the deadline the walk
  **halves the batch and retries the same cursor automatically** (down to a
  floor of 25) — you no longer need to hand-copy the cursor and re-invoke
  with `--after` on each timeout.
- `--after ka-XXX` — resume cursor; skips beads with `id <= ka-XXX`. The
  command prints a `cursor=<lastId>` per page; record the most recent
  one if you need to resume.
- `--type <type>` — narrow the walk to a single bead type (index-backed
  by `idx_beads_type`); empty (the default) covers every type. Use it to
  reindex `type=decision` **first**: the `parked_on_decision` badge
  (`gb ready` / `gb news`) resolves parking purely through open decisions'
  references, and a full ~677k-bead backfill orders decision ids near the
  END of the keyset — so an unfiltered run leaves the badge rendering
  silent false negatives on parked rows for hours (bug ka-0000005898). A
  targeted `--type decision` pass is seconds and makes the badge sound for
  the one type it queries. Run the targeted pass, then the full pass:

  ```sh
  kd admin reextract-references --type decision   # badge sound in seconds
  kd admin reextract-references                    # finish the long tail
  ```

Output is one line per page:

```
page 1: beads=500 comments=820 refs=312 cursor=ka-LAST_ID_OF_PAGE
page 2: beads=500 comments=741 refs=287 cursor=ka-LAST_ID_OF_PAGE
...
done in 4m12s: pages=18 beads_scanned=8742 comments_scanned=14201 references_written=4138 total_references=4138
```

`references_written` counts both flat rows (one per unique ka-id per
source_field) and typed rows (one per recognized canonical verb per
unique target). Unknown verbs (e.g. `closes(ka-X)`, `fixes(ka-X)`) emit
only a flat row — the `Verb`-empty `RawVerb` shape is intentionally not
materialized into `relationship_type`.

## Expected duration

The backfill is dominated by per-bead transactions (DELETE + small
INSERT). A modest production database (10⁴–10⁵ beads, comment extraction
flag on) typically completes in **a few minutes** on a warm cache.

Rule of thumb (gardenio-prod sizing as of 2026-05):

| Beads (`COUNT(*) FROM beads`) | Comments | Wall-clock estimate |
|---|---|---|
| ~5,000 | ~10,000 | < 1 minute |
| ~50,000 | ~150,000 | 3–5 minutes |
| ~500,000 | ~2M | 30–45 minutes |

If you blow past these by more than 2× something is wrong — most likely
a hot lock from a parallel writer. Stop the run, investigate, and resume
from the printed cursor.

## Operational considerations

- **Concurrency-safe with live writes.** Pagination is keyset-based
  (`id > $afterID ORDER BY id ASC`); rows inserted during the backfill
  are picked up on a later page rather than shifting earlier pages.
  Per-bead transactions keep the bead's references atomic; live writes
  to other beads are unaffected.
- **Soft-deleted beads are included.** Their references rows are owned
  by the row and would otherwise stay stale until hard-delete.
- **Comment extraction is gated by `BEADS_REFERENCES_EXTRACT_COMMENTS`.**
  When `0` (default), the backfill skips the `comments` iteration and
  leaves `comment:<id>` source_fields untouched. Flip the env var on,
  redeploy, then re-run the backfill if you want comment-text references
  populated.
- **Failure handling.** A per-bead failure aborts the page. The CLI
  surfaces the cursor **and the batch-size** in the error message; pass the
  cursor to `--after` to resume. A page that overruns the server's ~30s
  deadline is handled automatically — the walk halves the batch and retries
  the same cursor (floor 25) rather than aborting — and the server now
  surfaces a distinct `reextract page exceeded the request time budget`
  body for that case instead of the opaque generic 500.

## Verification after running

After a clean `done` line, the table cardinality should match the printed
`total_references`. Spot-check a known typed-mention bead:

```sql
SELECT source_bead_id, target_bead_id, source_field, relationship_type
FROM bead_references
WHERE source_bead_id = 'ka-some-bead-with-parent-mention';
```

You should see one row per (target, field) for flat references and one
additional row per (target, field, canonical_verb) for typed ones.

## See also

- `kbeads/docs/design-bead-references.md` — overall bead-references design.
- `kbeads/docs/research-typed-mention-syntax.md` § 10.1 — schema shape and
  rollout.
- `kbeads/internal/store/postgres/queries_references.go` —
  `queryReextractReferencesPage` (server-side handler) and
  `bulkInsertTypedReferences` (typed rows insert).
- `kbeads/internal/references/typed.go` — typed parser surface.
