# kd / gb CLI cookbook — querying beads, and the jq/shell footguns

Consolidated reference for the recurring `kd`/`gb` CLI-usage and jq/shell traps that were
previously nine separate `global` advice beads. Each bead now carries a one-line hook plus a
pointer here (advice-pool paydown, ka-0000003207) so the rules stay discoverable without
paying ~19 KB of fleet-wide context on every session.

Reach for this doc when: tracing what an agent created/touched, projecting `--json` output
with jq, defaulting a boolean field, filtering by time, or writing a comment/description from
a file.

## Contents

- [Query the SERVER, not a jq scan](#1-query-the-server-not-a-jq-scan)
- [`--json` output shapes: bare array vs envelope vs object](#2-json-output-shapes)
- [`kd show --json` nested keys use their own vocabulary](#3-ka-show---json-nested-keys)
- [Time-window queries: use the `*-after` params](#4-time-window-queries)
- [Fetch-once, project-many](#5-fetch-once-project-many)
- [jq `//` treats `false` as empty](#6-jq--treats-false-as-empty)
- [Inline jq string literals are fragile — use `-rf` or single-quotes](#7-inline-jq-string-literals)
- [Writes from a file: `@file` support is NOT uniform; read back](#8-writes-from-a-file)

---

## 1. Query the SERVER, not a jq scan

To answer "what did agent X create / touch / own?", let the SERVER filter. A client-side jq
scan over `kd list --json` is not merely slow — it returns a **silent false empty**, because
`kd list --json` is a 20-row PAGE of a 44k-bead table, scoped to open issue-kind beads in the
current project.

| Question | Use |
|---|---|
| what did agent X create? | `kd list --created-by <agent> --all-types --json` |
| …in the last N hours? | add `--created-after 6h` |
| what's assigned to X? | `kd list --assignee <agent> --all-types --json` |
| find X's agent/config bead | `kd list --all-types -f agent=<agent> --json` |
| just the count | `kd count --created-by <agent> --all-types` |

`--created-by`/`--assignee` are columns (bare value). `-f key=value` is for custom fields
(`agent`, `spawned_by`, `role`, `task_id`); `-f key=*` = present and non-empty.

### The brute-force form is WRONG, not just wasteful

Tracing an agent whose real footprint is **6 beads** (measured 2026-08-03):

    kd list --created-by <agent> --all-types --json  ->  9,895 B, 6 beads  ✅
    kd list --json | jq 'select(.created_by==…)'     -> 45,974 B, 0 beads  ❌ FALSE EMPTY
    kd list --limit 500 --all-types --json | jq …    ->   1.6 MB, 0 beads  ❌ STILL empty

Three scopes must be defeated before a client-side scan can even be right, and `--limit`
defeats only one:

1. **Pagination** — bare `--json` is capped by `--limit` (default 20) with no total in it.
   `--json-envelope` reports `count`, `total`, `has_more`.
2. **Kind scope** — issue-kind only. Agent/session/message/config beads are never issue-kind,
   so even a targeted query without `--all-types` silently drops them.
3. **Project scope** — defaults to `$KD_PROJECT`; cross-project needs `--all-projects`.

Server-side predicates apply to the whole table *before* the limit; a jq filter applies only
to what the limit already truncated. That gap is correctness, not performance.

Don't count on a warning: `-f` warns in table mode but prints nothing in `--json`
(ka-0000005220); `--created-by`/`--assignee` never warn (ka-0000008078). Cross-check a
suspicious `0` with `kd count … --all-types`. Before hand-rolling jq over a full list, re-read
`kd list --help` — it has grown `--unassigned`, `--no-parent`, `--exclude-type`,
`--exclude-label`, `--label-any`, `--mentions`, `--parent` and six time-window params.

## 2. `--json` output shapes

CLI `--json` output is NOT uniformly enveloped. There is NO `.beads` key on any kd/gb CLI
`--json` output. Pick the iterator by shape:

- `kd list --json`, `gb ready --json`, `kd search --json` → **bare JSON array**. Iterate with
  `jq '.[]'` (e.g. `gb ready --all-projects --json | jq -r '.[].id'`).
- `kd show <id> --json` → a **single object**. Index fields directly: `jq -r '.status'`.
- `gb news --json` → an **object envelope**. Iterate the named arrays: `jq '.agents[]'`,
  `.in_progress[]`, `.stale[]`, `.closed[]`, `.unassigned[]`, `.open_decisions[]`.

Separate surface — the beads **HTTP API** (`GET /v1/beads`) DOES return an envelope
`{beads:[...], total}`, so `jq '.beads[]'` is correct THERE. The trap is only the CLI: CLI
list/ready/search = bare array; HTTP `/v1/beads` = enveloped.

The built-in `--jq` flag now catches the classic mistake **at runtime**: `--jq '.beads[]'` on
a bare array prints a hint naming the right iterator (`.[]`) instead of a raw error. The table
above still applies to external jq over `--json` and to the HTTP API.

## 3. `kd show --json` nested keys

The nested sub-objects `kd show <id> --json` emits do NOT use the same key names as the
top-level bead object, and jq returns null (not an error) for an absent key — so a
conventional guess yields plausible-looking output with silent holes.

`dependencies[]` (and its `depends_on[]` alias) — the row names the EDGE, not the neighbour:
  - `bead_id` = THIS bead (the dependent / child side)
  - `depends_on_id` / `depends_on_status` / `depends_on_title` = the NEIGHBOUR
  - `type` = edge type (blocks, parent-child, …)

`comments[]`:
  - `author` = who wrote it
  - `text` = the body
  - `operated_by_display` = resolver-populated operator display name (NOT an author fallback)

As of kd > 2026.218.25 (MR !6921) strictly-additive **unprefixed aliases** also work:
`dependencies[]` gains `id`/`status`/`title`; `comments[]` gains `body`. The canonical
prefixed keys are unchanged — prefer them in durable scripts since they work on every binary.

The habit that saves the round-trip: when a jq projection nulls, dump keys once instead of
guessing again — `kd show <id> --json | jq '.dependencies[0] | keys'`. A half-populated
projection (author prints, body nulls) is a schema signal, not bad data.

## 4. Time-window queries

`/v1/beads` and `kd list` honor six time-window params — `created_after`, `created_before`,
`updated_after`, `updated_before`, `closed_after`, `closed_before` — all RFC3339 (MR 5670).
Do NOT offset-walk the list endpoint and date-filter client-side with jq.

`kd list` flags: `--created-after` / `--created-before` / `--updated-after` /
`--updated-before` / `--closed-after` / `--closed-before`. Each accepts an RFC3339 timestamp
(`2026-07-18T09:00:00Z`) OR a duration back from now (`4h`, `30m`, `2d`).

    kd list -s closed --closed-after 24h          # closed in the last day
    kd list --created-after 2026-07-18T12:00:00Z  # created since noon
    GET /v1/beads?kind=issue&created_after=2026-07-18T12:00:00Z

Notes: `closed_*` windows the nullable `closed_at` column, so `-s closed` is implied by the
window. HTTP transport only (the kd default); over gRPC these filters error loudly. The
server silently ignores unparseable param values; the kd CLI fails loudly on a malformed flag.

## 5. Fetch-once, project-many

Never re-run the identical server query for a different projection. Since the agent image
shipped the built-in `--jq` flag (MR !5731), one server call feeds many projections
**in-process** — no external jq, no re-fetch, no temp file. Combine every projection into a
single filter:

    kd list --type=bug --status=open -l project:gardenio \
      --jq '{count: length, ids: [.[].id], first: .[0]}'

`--jq` implies `--json` and projects the response before it leaves the CLI, so a
count-then-detail / progressive-slice / double-show pattern collapses to one call.

Capture to a FILE only when you must re-read the SAME snapshot across a mutation (or share it
between shell steps):

    S=$(mktemp -d)
    kd list --type=bug --status=open -l project:gardenio --json > "$S/open.json"
    jq length "$S/open.json"; jq -r ".[].id" "$S/open.json"   # zero extra round-trips

Recurring leaks this replaces: count-then-detail (`| jq length` then re-fetch for detail),
progressive comment slicing (`.[-5:]` then `.[-3:]` then full), same-block double-show
(`jq .fieldA` then `jq .fieldB` on the same id). On a cron gardener running 48 cycles/day this
multiplies into hundreds of pointless round-trips.

When a fresh re-fetch IS correct: just-before-mutate staleness checks, or when you
performed/observed a mutation in between. Even then: ONE fresh capture, all projections off it.

## 6. jq `//` treats `false` as empty

jq's alternative operator `a // b` yields `b` whenever `a` is `null` OR `false` (jq's two
falsy values). So `.fields.enabled // "true"` reads a clean boolean `false` as `"true"` —
manufacturing phantom "enabled" rows that then drive redundant re-queries to disprove
(CED bug ka-0000001763: 5 disabled triggers flagged enabled, 7× identical re-runs).

    echo '{"fields":{"enabled":false}}' | jq -r '.fields.enabled // "true"'   # prints: true (WRONG)

THE RULE: never use `//` to default a field whose legitimate values include `false`, `null`,
`0`, or `""`. Correct patterns default only on absent/null and preserve real `false`:

    jq -r 'if .fields.enabled == null then "true" else (.fields.enabled|tostring) end'
    jq 'select(.fields.enabled != false)'     # keep unless explicitly disabled

## 7. Inline jq string literals

Any jq projection with a quoted string literal — `select(.type=="blocks")`, a quoted object
key — is fragile as a single-line inline shell argument: to embed the `"` inside a
double-quoted bash string you escape it (`\"`), and one dropped backslash yields
`jq: error: syntax error … (Unix shell quoting issues?)`. The data plane is fine; the fault is
100% client-side shell quoting.

THE RULE — the moment a jq program contains a `"`, do NOT hand-escape it inline. In order of
preference:

1. **jq file + `-rf`** (best for anything non-trivial or reused). No shell sees the `"`:

       cat > /tmp/proj.jq <<'EOF'
       .[] | select(.type=="blocks") | .id
       EOF
       kd show <ids> --json | jq -rf /tmp/proj.jq

   The **quoted** heredoc delimiter `<<'EOF'` is what disables `$`/backtick/backslash
   expansion — `<<EOF` (unquoted) does NOT protect you.

2. **Single-quote the jq program** — bash does no expansion inside `'...'`:

       kd show <ids> --json | jq -r '.[] | select(.type=="blocks") | .id'

3. **Prefer a fielded server-side filter** over a client-side jq scan when one exists (see §1).

Reject on sight: escaped double-quotes inside a double-quoted command string —
`jq -r "select(.type==\"blocks\")"`.

## 8. Writes from a file

`@file` support is NOT uniform across write commands, and the failure mode is a silent
success — so a **read-back is mandatory** on any write whose payload came from a file.

- **Supports `@file`** (reads the file's bytes verbatim, no shell): `gb advice add -d @file`,
  `gb escalation create --context @file`, `kd create -d @file`, `kd update -d @file`,
  `kd update --title @file` / `--notes @file`.
- **Does NOT support `@file`**: `kd comment add --body` (and its `--content`/`--message`
  aliases, and positional text). `--body "@report.md"` durably writes the ten literal
  characters `@report.md` — exit 0, no error (this destroyed two merge-train scoreboard rows,
  ka-0000002095).

Two ways a file-sourced comment gets silently corrupted:

1. `--body "@file.md"` → stores the literal path (no `@` expansion).
2. `--body "$(cat file.md)"` → runs **command substitution** on every backtick in the file.
   Markdown is full of inline code spans, so backticked words are executed and their output
   (or a `command not found`) is substituted into your prose. Exit 0 either way.

Do this instead for a comment body:

    # quoted heredoc disables backtick/$ expansion:
    kd comment add <id> --body "$(cat <<'EOF'
    …markdown with `backticks` and $vars…
    EOF
    )"
    # or skip the shell entirely:
    #   subprocess.run(["kd","comment","add",BEAD,"--body",open("f.md").read()])

Then read back and grep for a distinctive phrase from your source:

    kd show <id> --json | jq -r '.comments[-1].body' | head -1   # must be your TEXT, not a path
