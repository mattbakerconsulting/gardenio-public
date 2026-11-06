# kd CLI: typed mentions

Reference for the `kd` surface that exposes typed mentions
(`bead_references.relationship_type`). For the bead-side syntax —
`parent(ka-X)` / `parent: ka-X` etc. — see
[`research-typed-mention-syntax.md`](research-typed-mention-syntax.md).
For the broader plan see
[`gardenio/plans/typed-mentions-graph-edges-and-cli.md`](../../gardenio/plans/typed-mentions-graph-edges-and-cli.md).

## Listing beads by what they mention

```sh
kd list --mentions ka-Target001                       # any reference to target
kd list --mentions ka-Target001 --mentions-type blocks   # only typed `blocks`
kd list --mentions ka-Target001 --mentions-type=        # only untyped mentions
```

Three-state semantics for `--mentions-type` (matches `kd refs --type`):

| Flag form                          | Filter applied                              |
|------------------------------------|---------------------------------------------|
| omitted                            | none — any relationship type, including untyped |
| `--mentions-type=`                 | only rows with `relationship_type IS NULL`  |
| `--mentions-type=mentions`         | alias for `--mentions-type=` (untyped only) |
| `--mentions-type=<verb>`           | exact match on `<verb>` (e.g. `blocks`, `parent`) |

Composes with all other `kd list` filters (`--status`, `--label`, etc.).

HTTP only at the moment — gRPC parity tracked in Phase 2 of the plan.

## Showing a bead's mention graph

`kd show <id>` now appends two sections after `Comments:` when the bead
participates in any reference edges:

```text
Mentions:
  blocks: 2 (ka-AAA, ka-BBB)
  parent: 1 (ka-CCC)
  untyped: 4 (ka-DDD, ka-EEE, ...)

Mentioned by:
  blocked-by: 1 (ka-XXX)
  untyped: 2 (ka-YYY, ka-ZZZ)
```

- One line per relationship type.
- Typed verbs are listed alphabetically; `untyped` is always last.
- Each line shows the count followed by up to five exemplar IDs;
  larger groups get a trailing `...`. Run `kd refs mentions <id>
  --type=<verb>` (or `--type=` for untyped) to see the rest.
- Beads with no references render nothing — no empty section headers.

When the daemon predates the typed-mentions API the section is
suppressed silently; if exactly one direction errors, a one-line hint
is printed so the operator knows the output is partial.

### `--mentions-only` — pipe-friendly mention view

```sh
kd show ka-Target001 --mentions-only        # only the two mention sections
kd show ka-Target001 --mentions-only | grep '^  parent:'
```

Suppresses the header table, `Depends On:`, and `Comments:`. Skips the
`GetBead` round-trip entirely, so the call is cheap when scripted. The
output format is identical to the embedded sections above, suitable for
`grep` / `awk` pipelines.

### TTY linkification

When stdout is a terminal *and* `BEADS_VIEWER_URL` (or `--viewer-url`)
is set, ka-IDs in the Mentions / Mentioned-by sections are wrapped in
[OSC 8](https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda)
hyperlinks pointing at `<viewer-url>/<ka-id>`. Modern terminals
(iTerm2, kitty, WezTerm, gnome-terminal) render the IDs as clickable
links. Non-TTY callers (pipes, redirected files, CI logs) get plain
text — no escape sequences leak through. `NO_COLOR=1` disables links
just like color.

```sh
export BEADS_VIEWER_URL=https://beads.gardenio.e2e.example.com
kd show ka-Target001                        # IDs are clickable in a TTY
kd show ka-Target001 | less                 # plain text — no escapes
```

## `kd mention <id>` — convenience alias

```sh
kd mention ka-Target001                     # equivalent to `kd refs mentions ka-Target001`
kd mention ka-Target001 --type blocks       # only typed `blocks`
kd mention ka-Target001 --type=             # only untyped mentions
```

Pure alias of `kd refs mentions` — same flags, same output, same JSON
when `--json` is set. Added because operators consistently reach for
`kd … mentions` rather than the longer `kd refs mentions …`. Keep `kd
refs` for the precise crowd; both will be supported indefinitely.

## See also

- [`kd refs <id>` / `kd refs mentioned-by <id>`](research-typed-mention-syntax.md#query-surface) — full
  per-edge listing with `relationship_type` column.
- [`/v1/graph`](design-bead-references.md) — graph endpoint that emits
  typed-mention edges as `dep_type=<verb>` distinct from untyped
  `mentions` edges, consumed by beads-viewer.
