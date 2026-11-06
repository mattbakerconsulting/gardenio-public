package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// countCmd is the scriptable sibling of `kd list`: it takes the identical
// filter flags but prints ONLY the matching total (a bare integer) to stdout
// and transfers no bead bodies. It exists to close the residual
// machine-readability gap that `kd list --json` left open (ka-0000008174 /
// design ka-0000000863 §4):
//
//   - `kd list --json | jq length` reports only the page (default --limit 20),
//     silently undercounting — the 31-child-epic "0 children" bug (ka-0000003137).
//   - The truncation-total hint that once rode stderr broke `kd list --json
//     2>&1 | jq` (a non-JSON line in the pipe → jq parse error, exit 5), so it
//     is now suppressed under --json (see printBeadListJSONWithTotal).
//
// `kd count` is the answer to "how many match?" without paging or a full fetch:
//
//	N=$(kd count --status=open --type=bug)
//
// A JSON-envelope alternative for `kd list` (`{"beads":[...],"total":N}`) was
// explicitly REJECTED — it breaks every `kd list --json | jq '.[]'` consumer and
// the `kd children` JSON schema contract (ka-0000007254). A dedicated count
// command sidesteps that entirely: the output is a bare integer (itself valid
// JSON), so `kd count` and `kd count --json` both emit a jq-parseable number.
//
// Implementation: it reuses buildListRequest verbatim — the single source of
// truth for how `kd list`'s flags map onto the wire request — then flips
// CountOnly=true. Sharing the builder guarantees the count can never drift from
// the total `kd list`'s footer would show for the same flags: they derive the
// identical WHERE clause, differing only in whether row bodies come back. The
// server's count_only fast path issues a bare `SELECT COUNT(*)` (no ORDER BY, no
// projections, no LIMIT), so a broad count avoids the full materialize+sort the
// COUNT(*) OVER() list path pays (queries.go:920-934).
var countCmd = &cobra.Command{
	Use:   "count",
	Short: "Count beads matching kd list's filters (prints just the integer)",
	Long: `Count beads matching the same filters as 'kd list', printing ONLY the total
integer to stdout — no bead bodies are transferred.

Use it wherever you would otherwise pipe 'kd list --json' to 'jq length': that
idiom silently returns only the default page size (--limit 20), whereas 'kd
count' returns the true total. Scriptable:

    N=$(kd count --status=open --type=bug)

FIELDED COUNTS — '-f/--field' answers "how many beads carry this field / this
value" without paging or jq:

    kd count --all-types -f spawned_by=*        # field present and non-empty
    kd count --all-types -f role=gardener        # exact value
    kd count --all-types -f spawned_by=         # field absent or empty

Note the --all-types: like 'kd list', a plain 'kd count' is scoped to issue-kind
beads AND (unless --all-projects) to your project, so a '-f' predicate matches 0
whenever the beads carrying that field fall outside either scope — e.g.
spawned_by/agent/role live on kind=config agent beads (need --all-types), and a
field may exist only on beads in another project (need --all-projects). When that
happens 'kd count' prints a one-line stderr note naming WHICH scoping hid them
and the total without it, so a bare '0' never reads as "none exist". stdout
stays a bare integer regardless, so 'N=$(kd count …)' is
unaffected.

It accepts kd list's filter flags (status, type, --exclude-type, kind,
label/--label-any, --exclude-label, assignee, --unassigned, created-by, field,
--project/--all-projects,
--fleet/--all-fleets, --parent, --include-operational, --no-blockers,
--no-parent, mentions, has-report, blocked-on-decision, …). Pagination and
ordering flags (--limit/--offset/--sort) are omitted: they do not affect a
count. The output is a bare integer (valid JSON), so 'kd count' and 'kd count
--json' both emit a jq-parseable number.`,
	GroupID: "beads",
	RunE: func(cmd *cobra.Command, args []string) error {
		req, _, err := buildListRequest(cmd)
		if err != nil {
			return err
		}
		// Flip to the count-only fast path: the server returns just the total
		// (empty Beads) via a bare SELECT COUNT(*), skipping the materialize +
		// sort the default list path pays. Total is identical to what the same
		// filters would surface on `kd list` — CountOnly changes only WHETHER
		// row bodies come back, not WHICH rows match.
		req.CountOnly = true
		// context.Background(), matching `kd list` (list.go) rather than
		// cmd.Context(): Command.Context() returns the raw ctx field, which only
		// ExecuteC/ExecuteContext populates, so any caller reaching RunE directly
		// gets nil — and a nil ctx reaches http.NewRequestWithContext and fails
		// the whole command with "net/http: nil Context". The nudge below can
		// safely take cmd.Context() because it guards nil itself and is
		// best-effort; the PRIMARY count must never fail for that reason.
		resp, err := beadsClient.ListBeads(context.Background(), req.ListBeadsRequest)
		if err != nil {
			return fmt.Errorf("counting beads: %w", err)
		}
		fmt.Println(resp.Total)
		// A `-f` predicate that answers 0 only because the default kind=issue
		// clause hid the beads carrying that field is the silent-zero footgun
		// ka-0000004030; a `--created-by`/`--assignee` count that reports the
		// in-scope subset as if it were the whole is the undercount footgun
		// ka-0000008078. Either way stdout's bare integer reads as complete. The
		// note goes to stderr AFTER the integer, so `N=$(kd count …)` is
		// byte-identical while the operator still learns to add --all-types.
		maybePrintScopeKindNudge(cmd.Context(), req, resp.Total)
		return nil
	},
}

func init() {
	// Same filter surface as `kd list`, so a count and a list agree by
	// construction (both flow through buildListRequest).
	registerListFilterFlags(countCmd)
}
