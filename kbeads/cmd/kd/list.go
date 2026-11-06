package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var (
	listProjectFlag     string
	listAllProjectsFlag bool
	listFleetFlag       string
	listAllFleetsFlag   bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List beads",
	Long: `List beads. By default only issue-kind beads are shown (task, feature, bug, epic, chore). Use --all-types to include infrastructure beads, or --type/--kind to filter explicitly.

Open escalation and decision beads are kind=data and so are hidden by the
default issue-kind filter. Surface them with --include-operational (which also
skips project scoping, since decisions carry no project label). The ad-hoc
equivalent is -t escalation -t decision. A plain interactive 'kd list' prints a
one-line stderr nudge when open escalation/decision beads were hidden.

--json emits each bead's full body, including a "description" field that is
ALWAYS present (an empty string when the bead has none), so a content grep like
'kd list --json | jq -r ".[] | select(.description | test(\"term\")) | .id"'
searches descriptions as well as titles and never hard-errors on an
empty-description bead. For full-text ranked find across all fields use
'kd search'.

JSON output: 'kd list --json' is a BARE ARRAY (jq '.[]'), and results are
capped by --limit (default 20) with no total in the array — so a scripted
caller cannot tell a limit-capped page from the whole set. To learn the true
count without paging, use 'kd count' (same filter flags, prints just the
integer) — including FIELDED counts, e.g. 'kd count --all-types -f
spawned_by=*' for "how many beads carry this field" or '-f key=value' for a
specific value; no jq needed. To get the page AND its pagination metadata in
one call, use
--json-envelope: it emits {beads,count,total,has_more,offset,limit} instead of
the bare array, so has_more/total reveal whether more beads exist beyond this
page. The bare-array default is unchanged (--json-envelope is strictly opt-in).`,
	GroupID: "beads",
	RunE: func(cmd *cobra.Command, args []string) error {
		// --json-envelope is a machine-readable shape, so it implies --json
		// (mirroring how --jq forces jsonOutput in the root PersistentPreRunE):
		// a caller who asks for the enveloped shape need not also pass --json.
		if envelope, _ := cmd.Flags().GetBool("json-envelope"); envelope {
			jsonOutput = true
		}

		req, projectScopeApplied, err := buildListRequest(cmd)
		if err != nil {
			return err
		}

		resp, err := beadsClient.ListBeads(context.Background(), req.ListBeadsRequest)
		if err != nil {
			return fmt.Errorf("listing beads: %w", err)
		}
		recordSeenBeadIDs(listedBeadIDs(resp.Beads))

		// Version-skew guard (ka-0000000945). A daemon that predates
		// with_open_blockers ignores the unknown query param and returns rows
		// with open_blockers ABSENT — which decodes to nil and reads exactly
		// like "I checked; nothing is blocked". That false all-clear, handed
		// to the backlog-triage callers this flag exists to serve, is worse
		// than no feature at all, so fail LOUDLY instead. A daemon that DID
		// honor the flag always sends a present-but-empty list for an
		// unblocked bead, so this can never fire against a current daemon.
		//
		// Deliberately scoped to a NON-EMPTY page. A zero-row result carries
		// no blocker claim about any bead, so there is nothing to be falsely
		// reassured about; and with no row to inspect, an old daemon is
		// indistinguishable from a new one on this path. The guard protects
		// the case that can actually mislead — rows present, all unanswered.
		if req.WithOpenBlockers && len(resp.Beads) > 0 {
			answered := false
			for _, b := range resp.Beads {
				if b.OpenBlockers != nil {
					answered = true
					break
				}
			}
			if !answered {
				return fmt.Errorf("--with-blockers: daemon returned no blocker data for any of the %d rows — it is too old to support this flag. "+
					"Refusing to print an unblocked-looking list that was never actually checked; re-run without --with-blockers, or upgrade the daemon", len(resp.Beads))
			}
		}

		if jsonOutput {
			// The default JSON output is a bare array (printBeadListJSON), so
			// there is no envelope to carry a project_scope field without
			// breaking consumers that index resp[i] directly. Leave the default
			// --json untouched; the scope note is a human-footer affordance only
			// (ka-0000007254). The truncation total is suppressed under --json
			// (it broke `kd list --json 2>&1 | jq`); `kd count` is the scriptable
			// total (ka-0000008174).
			//
			// --json-envelope is the OPT-IN escape hatch for scripted callers
			// who need to tell a --limit-capped page from the whole set in one
			// call: it emits {beads,count,total,has_more,offset,limit} instead
			// of the bare array (ka-0000001033). It is off by default precisely
			// so the bare-array contract above is never silently broken.
			if envelope, _ := cmd.Flags().GetBool("json-envelope"); envelope {
				printBeadListJSONEnvelope(resp.Beads, resp.Total, req.Offset, req.Limit)
			} else {
				printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
			}
		} else {
			printBeadListTable(resp.Beads, resp.Total,
				projectScopeDescriptor(projectScopeApplied, listProjectFlag))
			// Self-revealing nudge: when a plain `kd list` hid open
			// escalation/decision beads behind the default kind=issue filter,
			// tell the reader they exist and how to see them (design
			// ka-0000000863 §4). Interactive/TTY only — a piped stdout is
			// either an agent poll loop (whose extra count round-trip the
			// design flags as a cost) or a script, and neither should pay for
			// or see the hint. jsonOutput is already excluded by the branch.
			if req.implicitIssueKind && isStdoutTTY() {
				maybePrintOperationalNudge(cmd.Context(), projectScopeApplied, req.Fleet)
			}
		}
		// The scope-kind nudge closes two false-answer footguns: an empty `-f`
		// result whose beads the default kind=issue clause hid (ka-0000004030),
		// and a `--created-by`/`--assignee` UNDERCOUNT where the actor's
		// non-issue-kind beads are silently dropped (ka-0000008078) — an empty or
		// short list is exactly where the footer total gives the reader nothing
		// to notice the scoping from. It runs in EVERY output mode, --json
		// included (ka-0000005220): the note is stderr-only, so it never pollutes
		// the bare-array or enveloped JSON on stdout, and --json is the mode
		// agents script against — precisely where the false-empty/undercount had
		// no other safety net. The probe is gated and deadline-bounded (see the
		// function's cost note).
		maybePrintScopeKindNudge(cmd.Context(), req, resp.Total)
		return nil
	},
}

// listRequest bundles a built ListBeadsRequest with the derived flags the
// callers (list, count) need for post-build decisions the request struct itself
// does not carry — projectScopeApplied (for the footer/scope descriptor) and
// implicitIssueKind (whether this invocation injected the default kind=issue
// clause rather than being given an explicit --type/--kind/--all-types). Kept
// out of client.ListBeadsRequest because they are CLI presentation concerns,
// not wire fields.
//
// implicitIssueKind gates BOTH self-revealing hints about what that implicit
// clause hid: the escalation/decision nudge (maybePrintOperationalNudge) and
// the hidden-kind scope nudge (maybePrintScopeKindNudge, covering -f field
// predicates and the --created-by/--assignee actor columns). One bool, one
// source of truth — an explicit kind/type filter is the reader's own choice and
// silences both.
//
// implicitProjectLabel records the project label this builder INJECTED (empty
// when it injected none), so a caller that needs to undo the injection subtracts
// the exact string that was added instead of re-deriving "project:"+global. The
// field-kind probe must drop it (--all-types suppresses project scoping), and a
// second independent derivation would silently stop matching the day the
// injection's spelling or source global changes — with no test able to catch it,
// since both sides would read from the same literal.
type listRequest struct {
	*client.ListBeadsRequest
	implicitIssueKind    bool
	implicitProjectLabel string
}

// buildListRequest translates `kd list`'s flags into a ListBeadsRequest plus
// the projectScopeApplied bool. It is the single source of truth for how the
// list filter flags map onto the wire request, so `kd count` reuses it verbatim
// and its total can never drift from `kd list`'s footer total (they run the
// identical WHERE-clause derivation, differing only in CountOnly + output).
func buildListRequest(cmd *cobra.Command) (*listRequest, bool, error) {
	status, _ := cmd.Flags().GetStringSlice("status")
	beadType, _ := cmd.Flags().GetStringSlice("type")
	kind, _ := cmd.Flags().GetStringSlice("kind")

	// Validate --type against the known type registry so an unknown value
	// fails loud instead of silently matching nothing (exit 0) — mirroring
	// the create path's "unknown bead type" 400 (ka-0000006244). Reassign
	// beadType to the NORMALIZED (trimmed) values: the server matches the
	// type column verbatim, so querying with the raw `["task", " bug"]`
	// pflag splits would re-open the silent-empty footgun.
	beadType, err := validateTypeFilter(cmd.Context(), beadType)
	if err != nil {
		return nil, false, err
	}

	// --exclude-type is the negative type filter (drop beads whose type is
	// ANY of these; server-side `type NOT IN (...)`). Validate + normalize it
	// through the same registry as --type: an unknown --exclude-type value
	// would silently exclude nothing, and an untrimmed pflag split (`message,
	// event` → `["message", " event"]`) would fail to match the verbatim type
	// column so the noise it targets would leak back in — the same footgun
	// validateTypeFilter closes for --type.
	excludeType, _ := cmd.Flags().GetStringSlice("exclude-type")
	excludeType, err = validateTypeFilter(cmd.Context(), excludeType)
	if err != nil {
		return nil, false, err
	}

	limit, _ := cmd.Flags().GetInt("limit")
	assignee, _ := cmd.Flags().GetString("assignee")
	unassigned, _ := cmd.Flags().GetBool("unassigned")
	// --unassigned and --assignee are contradictory: no bead is both claimed by
	// X and claimed by nobody, so the combination can only ever return 0. Fail
	// loudly instead — a silent empty set here is indistinguishable from "no
	// work matches", the false-zero hazard this flag exists to remove
	// (ka-0000007209).
	if unassigned && assignee != "" {
		return nil, false, fmt.Errorf("--unassigned and --assignee are mutually exclusive (a bead cannot be both assigned to %q and unassigned); drop one", assignee)
	}
	createdBy, _ := cmd.Flags().GetString("created-by")
	offset, _ := cmd.Flags().GetInt("offset")
	fieldFlags, _ := cmd.Flags().GetStringArray("field")
	noBlockers, _ := cmd.Flags().GetBool("no-blockers")
	noParent, _ := cmd.Flags().GetBool("no-parent")
	missingShortTitle, _ := cmd.Flags().GetBool("missing-short-title")
	sort, _ := cmd.Flags().GetString("sort")
	allTypes, _ := cmd.Flags().GetBool("all-types")
	includeOperational, _ := cmd.Flags().GetBool("include-operational")
	labelFlags, _ := cmd.Flags().GetStringSlice("label")
	labelAnyFlags, _ := cmd.Flags().GetStringSlice("label-any")
	excludeLabelFlags, _ := cmd.Flags().GetStringSlice("exclude-label")

	// Warn (do not abort) when any label value carries a "|": labels match
	// literally, "|" is not an OR operator, so `-l "a|b|c"` silently matches
	// nothing and returns a false all-clear (ka-0000002308). Point the reader at
	// the shipped OR mechanism, --label-any. Exit semantics are unchanged.
	warnOnPipeLabels(os.Stderr, "-l/--label", labelFlags)
	warnOnPipeLabels(os.Stderr, "--label-any", labelAnyFlags)
	warnOnPipeLabels(os.Stderr, "--exclude-label", excludeLabelFlags)
	includeDeleted, _ := cmd.Flags().GetBool("include-deleted")
	onlyDeleted, _ := cmd.Flags().GetBool("only-deleted")
	includeScheduled, _ := cmd.Flags().GetBool("include-scheduled")
	mentions, _ := cmd.Flags().GetString("mentions")
	parent, _ := cmd.Flags().GetString("parent")
	operatedByAliases, _ := cmd.Flags().GetStringSlice("operated-by-alias")
	// --mentions-type uses three-state semantics matching `kd refs --type`:
	//   - flag unset       → nil (no relationship filter)
	//   - --mentions-type= → "" (only untyped/NULL)
	//   - --mentions-type=blocks → "blocks" (exact match)
	// The alias --mentions-type=mentions is also accepted for "untyped only"
	// to mirror the `kd refs --type=mentions` shorthand.
	var mentionsType *string
	if f := cmd.Flags().Lookup("mentions-type"); f != nil && f.Changed {
		v := f.Value.String()
		if v == "mentions" {
			v = ""
		}
		mentionsType = &v
	}

	// Time-window flags (ka-0000008195): each accepts an RFC3339 timestamp or
	// a duration back from now (4h, 2d) via the shared parseTimeCutoff helper.
	// A malformed value fails LOUDLY here — unlike the server's silent-ignore
	// of unparseable query params, a CLI typo must not widen the result set.
	now := time.Now()
	parseWindowFlag := func(name string) (*time.Time, error) {
		v, _ := cmd.Flags().GetString(name)
		if v == "" {
			return nil, nil
		}
		t, err := parseTimeCutoff("--"+name, v, now)
		if err != nil {
			return nil, err
		}
		return &t, nil
	}
	createdAfter, err := parseWindowFlag("created-after")
	if err != nil {
		return nil, false, err
	}
	createdBefore, err := parseWindowFlag("created-before")
	if err != nil {
		return nil, false, err
	}
	updatedAfter, err := parseWindowFlag("updated-after")
	if err != nil {
		return nil, false, err
	}
	updatedBefore, err := parseWindowFlag("updated-before")
	if err != nil {
		return nil, false, err
	}
	closedAfter, err := parseWindowFlag("closed-after")
	if err != nil {
		return nil, false, err
	}
	closedBefore, err := parseWindowFlag("closed-before")
	if err != nil {
		return nil, false, err
	}

	// --has-report and --blocked-on-decision are three-state bool flags:
	// unset = no filter, --flag (or =true) = require, =false = exclude.
	// Detect via f.Changed so the implicit zero value never leaks into
	// the request.
	var hasReport, blockedOnDecision *bool
	if f := cmd.Flags().Lookup("has-report"); f != nil && f.Changed {
		v, _ := cmd.Flags().GetBool("has-report")
		hasReport = &v
	}
	if f := cmd.Flags().Lookup("blocked-on-decision"); f != nil && f.Changed {
		v, _ := cmd.Flags().GetBool("blocked-on-decision")
		blockedOnDecision = &v
	}
	// --with-blockers is a plain opt-in enrichment (no three-state), and is
	// registered on `kd list` only — the Lookup guard keeps this builder
	// shared with `kd count`, where the flag does not exist.
	withOpenBlockers := false
	if f := cmd.Flags().Lookup("with-blockers"); f != nil {
		withOpenBlockers, _ = cmd.Flags().GetBool("with-blockers")
	}

	// Default to kind=issue when no explicit type, kind, or all-types flag is set.
	// The project label filter applies whenever the query is constrained to
	// issue-kind beads (default mode, --kind=issue, or --type listing only
	// issue types like task/bug/epic/feature/chore). Issue beads always carry
	// a project label; infrastructure (project, config, agent) and data
	// (formula, molecule) beads may not, so the filter is skipped when the
	// query could include them, to avoid silently excluding rows.
	defaultIssueMode := !allTypes && len(beadType) == 0 && len(kind) == 0

	// --include-operational surfaces open escalation + decision (kind=data)
	// beads that the default kind=issue filter hides — the exact beads no
	// agent may miss (design ka-0000000863 §4; remediates ka-0000002396).
	// Mirror --all-types: suppress the kind=issue injection and skip project
	// scoping. We implement it by APPENDING the two operational types to the
	// type filter — that makes isIssueOnlyQuery() false (so the implicit
	// project label is dropped, exactly as the working `-t escalation
	// -t decision` path does) and, with defaultIssueMode captured above,
	// keeps the kind=issue clause from being injected. Skip the append under
	// --all-types, where every type is already returned and adding a type
	// filter would instead NARROW to just these two.
	if includeOperational && !allTypes {
		beadType = append(beadType, "escalation", "decision")
	}

	// implicitIssueKind records whether this invocation is the plain default
	// path that injects kind=issue and thereby hides every non-issue-kind
	// bead. It gates the stderr nudges below. --include-operational (which
	// shows escalation/decision) and any explicit --type/--kind/--all-types
	// (defaultIssueMode false) both suppress them.
	implicitIssueKind := defaultIssueMode && !includeOperational
	if implicitIssueKind {
		kind = []string{"issue"}
	}

	// kd list --no-blockers defaults to hiding scheduled-task beads
	// (ka-0000006652): auto-spawned by the cron scheduler each tick,
	// they re-spawn before any human can claim them. Plain `kd list`
	// (no --no-blockers) keeps the legacy behavior so admins inspecting
	// scheduled beads directly are unaffected.
	excludeScheduled := noBlockers && !includeScheduled

	req := &client.ListBeadsRequest{
		Status:            status,
		Type:              beadType,
		ExcludeTypes:      excludeType,
		Kind:              kind,
		Limit:             limit,
		Assignee:          assignee,
		Unassigned:        unassigned,
		CreatedBy:         createdBy,
		Offset:            offset,
		NoOpenDeps:        noBlockers,
		NoParent:          noParent,
		ShortTitleMissing: missingShortTitle,
		Sort:              sort,
		IncludeDeleted:    includeDeleted,
		OnlyDeleted:       onlyDeleted,
		ExcludeScheduled:  excludeScheduled,
		Mentions:          mentions,
		MentionsType:      mentionsType,
		Parent:            parent,
		OperatedByAliases: operatedByAliases,
		HasReport:         hasReport,
		BlockedOnDecision: blockedOnDecision,
		WithOpenBlockers:  withOpenBlockers,
		CreatedAfter:      createdAfter,
		CreatedBefore:     createdBefore,
		UpdatedAfter:      updatedAfter,
		UpdatedBefore:     updatedBefore,
		ClosedAfter:       closedAfter,
		ClosedBefore:      closedBefore,
	}

	// An ID-anchored subtree query (`--parent <epic>`) must NOT be silently
	// intersected with the caller's implicit project label. Epics routinely
	// carry cross-project children (e.g. a gardenio plan with project:monorepo
	// child tasks); ANDing project:<caller> onto the subtree drops those
	// children so the epic reads as childless — the meta-gardener
	// stop-decision undercount bug (ka-0000008241): `kd list --parent <epic>`
	// returned 0 open children for plans whose children were project:monorepo,
	// producing a false "terminal goal met, stop the gardener" proposal on a
	// still-active plan. Suppress the implicit injection when --parent is set,
	// UNLESS the caller passed an explicit --project (Changed) — an explicit
	// scope is always honored, so `kd list --parent <epic> --project monorepo`
	// still narrows to that project's slice of the subtree.
	projectExplicit := cmd.Flags().Changed("project")
	implicitProjectSuppressed := parent != "" && !projectExplicit
	projectScopeApplied := !listAllProjectsFlag && listProjectFlag != "" && !allTypes &&
		isIssueOnlyQuery(beadType, kind) && !implicitProjectSuppressed
	// Remember the injected label verbatim so a caller undoing the injection
	// (maybePrintScopeKindNudge's probe) subtracts THIS string rather than
	// rebuilding it from the global — see listRequest.implicitProjectLabel.
	var implicitProjectLabel string
	if projectScopeApplied {
		implicitProjectLabel = "project:" + listProjectFlag
		req.Labels = append(req.Labels, implicitProjectLabel)
	}
	req.Labels = append(req.Labels, labelFlags...)
	// --label-any is the OR / any-of filter (a bead matches with at least
	// one of these labels), distinct from --label which ANDs. See
	// ka-0000003307: `kd list -l a,b,c` means "carries ALL of a,b,c", which
	// for distinct single-kind labels is always 0 — use --label-any for
	// "carries any of a,b,c".
	req.LabelsAny = append(req.LabelsAny, labelAnyFlags...)
	// --exclude-label is the negative filter (drop beads carrying ANY of
	// these labels; server-side `label NOT IN (...)`). It lets agents
	// express the NOT-clause of a lane query natively instead of
	// hand-rolling `kd list --json | jq 'select(...)'`, whose one wrong
	// field name silently returns an empty set indistinguishable from "no
	// work" (ka-0000007103).
	req.ExcludeLabels = append(req.ExcludeLabels, excludeLabelFlags...)

	// Fleet filtering: --all-fleets sends "*" (admin-only cross-fleet query),
	// --fleet sends a specific fleet name.
	if listAllFleetsFlag {
		req.Fleet = model.FleetAll
	} else if listFleetFlag != "" {
		req.Fleet = listFleetFlag
	}

	if len(fieldFlags) > 0 {
		req.FieldFilters = make(map[string]string, len(fieldFlags))
		for _, f := range fieldFlags {
			k, v, ok := splitField(f)
			if !ok {
				return nil, false, fmt.Errorf("invalid field filter %q (expected key=value)", f)
			}
			req.FieldFilters[k] = v
		}
	}

	return &listRequest{
		ListBeadsRequest:     req,
		implicitIssueKind:    implicitIssueKind,
		implicitProjectLabel: implicitProjectLabel,
	}, projectScopeApplied, nil
}

// registerListFilterFlags registers the filter flags shared by `kd list` and
// `kd count` on cmd, so both accept an identical filter surface and build the
// same wire request through buildListRequest — the count can never drift from
// the list footer's total. Pagination/ordering flags (--limit/--offset/--sort)
// are NOT registered here: they are list-only, meaningless to a count (the
// server's count_only fast path ignores Limit/Offset/Sort), so `kd list`
// registers them separately.
//
// The --project/--all-projects/--fleet/--all-fleets flags bind the same package
// globals for both commands; only one command runs per process, so the shared
// binding is safe.
func registerListFilterFlags(cmd *cobra.Command) {
	cmd.Flags().StringSliceP("status", "s", nil, "filter by status (repeatable)")
	cmd.Flags().StringSliceP("type", "t", nil, "filter by type (repeatable)")
	cmd.Flags().StringSlice("exclude-type", nil, "drop beads whose type is ANY of these (NOT IN; repeatable/comma; e.g. --exclude-type message). The type analogue of --exclude-label — e.g. kd list --mentions <epic> --all-types --exclude-type message drops the message narration that otherwise dominates a related-beads scan.")
	cmd.Flags().StringSliceP("kind", "k", nil, "filter by kind (repeatable; default: issue)")
	cmd.Flags().String("assignee", "", "filter by assignee")
	// --unassigned is the "nobody on it yet" predicate, evaluated server-side as
	// `assignee IS NULL OR assignee = <empty>`. It exists because there was NO way to
	// ask this: `--assignee ""` is dropped as "no filter" (so it returns every
	// bead, claimed ones included) and `-f assignee=` does not apply because
	// assignee is a first-class column, not a JSONB custom field. Work-seeking
	// therefore degraded to pulling the whole open set and post-filtering in jq,
	// where a wrong field name silently yields a false-zero "no work"
	// (ka-0000007209; same class as --exclude-label, ka-0000007103).
	cmd.Flags().Bool("unassigned", false, "only beads with no assignee (server-side assignee IS NULL OR assignee = ''). The work-seeking predicate: kd list -s open -t task --no-blockers --unassigned. Mutually exclusive with --assignee. HTTP transport only.")
	cmd.Flags().String("created-by", "", "filter by actor who created the bead")
	cmd.Flags().StringArrayP("field", "f", nil, "filter by custom field (key=value exact match; key=* field present and non-empty; key= field absent or empty; repeatable). SCOPE INTERACTION: a field carried only by beads the default scoping excludes matches 0 — non-issue-kind beads (e.g. spawned_by/agent/role on kind=config agent beads) need --all-types, and beads in other projects need --all-projects. Both cases print a stderr note naming which scoping hid them. Pair with kd count for a fielded count: kd count --all-types -f spawned_by=*")
	cmd.Flags().StringSliceP("label", "l", nil, "filter by label — ANDed: a bead must carry ALL given labels (repeatable/comma; e.g. -l source:jira -l jira-has-media). For OR/any-of use --label-any.")
	cmd.Flags().StringSlice("label-any", nil, "filter by ANY of these labels (OR; repeatable/comma; e.g. --label-any release-stuck,preflight-broken). Contrast -l/--label which ANDs.")
	cmd.Flags().StringSlice("exclude-label", nil, "drop beads carrying ANY of these labels (NOT IN; repeatable/comma; e.g. --exclude-label needs-intake --exclude-label release-regression). Lets you express a lane query's NOT-clause without hand-rolling jq.")
	// The status list here is the CANONICAL one (see FilterNoOpenDeps): it
	// includes 'blocked'. The help text used to omit it, and an agent
	// reconstructing the gate client-side copied that shorter list and missed
	// a blocker sitting in status 'blocked' (ka-0000000945). Keep it accurate
	// — and point readers at --with-blockers so they read the server's answer
	// instead of re-deriving one.
	cmd.Flags().Bool("no-blockers", false, "only show beads with no open/in_progress/blocked/deferred blocking dependencies (edge types: blocks, blocked-by, depends-on — parent-child is hierarchy, not a wait edge). To see WHICH blockers gate the rows instead of hiding them, use --with-blockers.")
	cmd.Flags().Bool("no-parent", false, "only show beads with no parent-child parent (orphans) — kills the triage-intake N+1 dep-list loop")
	cmd.Flags().Bool("missing-short-title", false, "only show beads with no short_title (set one with kd update <id> --short-title). HTTP transport only.")
	cmd.Flags().StringVar(&listProjectFlag, "project", defaultProject(), "filter by project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	cmd.Flags().BoolVar(&listAllProjectsFlag, "all-projects", false, "show beads from all projects (disables project filter)")
	cmd.Flags().Bool("all-types", false, "show all bead kinds including infrastructure (default: only issue-kind beads)")
	cmd.Flags().Bool("include-operational", false, "also show open escalation/decision beads (kind=data) that the default issue-kind filter hides; skips project scoping. Ad-hoc equivalent: -t escalation -t decision")
	cmd.Flags().StringVar(&listFleetFlag, "fleet", "", "filter by fleet name")
	cmd.Flags().BoolVar(&listAllFleetsFlag, "all-fleets", false, "query all fleets (requires admin token)")
	cmd.Flags().Bool("include-deleted", false, "include soft-deleted beads in results (admin-only)")
	cmd.Flags().Bool("only-deleted", false, "show only soft-deleted beads (admin-only)")
	cmd.Flags().Bool("include-scheduled", false, "include scheduled-task beads (cron-spawned) in --no-blockers results (default: hidden)")
	cmd.Flags().String("mentions", "", "only beads whose text references this bead id (e.g. ka-0000004973)")
	cmd.Flags().String("mentions-type", "", "narrow --mentions to a typed-mention verb (parent, blocks, related, …); 'mentions' or '' = untyped only")
	// --parent <epic> lists the REVERSE parent-child subtree (all descendants
	// at any depth whose parent-child edge points AT <epic>), via the same
	// cycle-safe recursive CTE the scheduler's bead_query {parent} gate uses.
	// This is the correct child-count query for a gardener's epic: it does NOT
	// read the epic's own forward depends_on edges (which point UP at a
	// grandparent and would 0-out the count — ka-0000000208). HTTP transport
	// only. Combine with -s/-t to scope, e.g. `kd list --parent <epic> -s open`.
	//
	// --parent SUPPRESSES the implicit project-label injection (ka-0000008241):
	// an ID-anchored subtree query must not be silently intersected with the
	// caller's project, or an epic with cross-project children reads as
	// childless. An explicit --project is still honored.
	cmd.Flags().String("parent", "", "list the transitive parent-child subtree (descendants) of this bead id — the reverse child query; suppresses the implicit project filter so cross-project children are counted (HTTP transport only)")
	// --operated-by-alias resolves `kind:subject` aliases through
	// operator_aliases to operated_by_id, recovering legacy Slack-driven
	// beads under the canonical alias (e.g. `oidc:matt@acme.example`
	// returns rows whose canonical operator also has a `slack:U…` alias
	// attached). The legacy TEXT-keyed --operated-by flag was removed
	// with migration 024 (P6); use --operated-by-alias instead.
	cmd.Flags().StringSlice("operated-by-alias", nil, "filter by canonical operator alias (kind:subject, resolved through operator_aliases; recovers legacy Slack-driven beads)")
	// Three-state predicates (R4, ka-0000001369): the Cobra default is
	// false, but the request only sets the field when the flag is changed
	// — `--has-report` requires a deliverable child, `--has-report=false`
	// excludes beads that have one, omitted = no filter. Same shape for
	// --blocked-on-decision.
	cmd.Flags().Bool("has-report", false, "filter by has_report predicate (set: only with deliverable child; =false: only without; unset: no filter)")
	cmd.Flags().Bool("blocked-on-decision", false, "filter by blocked_on_decision predicate (set: only blocked on a decision; =false: only not; unset: no filter)")
	// Time-window flags (ka-0000008195): server-side windows on created_at /
	// updated_at / closed_at, so "what closed in the last day" is one query
	// (kd list -s closed --closed-after 24h) instead of an offset-walk with
	// client-side jq date filtering. Values are RFC3339 or a duration back
	// from now. closed_* only ever matches closed beads (closed_at is NULL
	// until close). HTTP transport only — gRPC errors rather than silently
	// dropping the window.
	cmd.Flags().String("created-after", "", "only beads created after this cutoff (RFC3339 like 2026-07-18T09:00:00Z, or a duration back from now like 4h, 2d)")
	cmd.Flags().String("created-before", "", "only beads created before this cutoff (RFC3339 or duration back from now)")
	cmd.Flags().String("updated-after", "", "only beads updated after this cutoff (RFC3339 or duration back from now)")
	cmd.Flags().String("updated-before", "", "only beads updated before this cutoff (RFC3339 or duration back from now)")
	cmd.Flags().String("closed-after", "", "only beads closed after this cutoff (RFC3339 or duration back from now) — e.g. kd list -s closed --closed-after 24h")
	cmd.Flags().String("closed-before", "", "only beads closed before this cutoff (RFC3339 or duration back from now)")
	// Accept the pluralized --labels / --labels-any spellings as aliases of
	// --label / --label-any so the common agent typo no longer aborts with a
	// bare "unknown flag" that reads as a false all-clear (ka-0000000987).
	aliasLabelFlags(cmd)
}

func init() {
	registerListFilterFlags(listCmd)
	// Pagination/ordering flags are list-only (see registerListFilterFlags).
	listCmd.Flags().Int("limit", 20, "maximum number of beads to return")
	listCmd.Flags().Int("offset", 0, "offset for pagination")
	listCmd.Flags().String("sort", "", "sort column: priority, created_at, updated_at, title, status, type (prefix with - for descending, e.g. -priority)")
	// --with-blockers is list-only: it enriches ROWS, and a count has no rows.
	// Registering it here (not in registerListFilterFlags) keeps `kd count`
	// from advertising a flag that could not change its answer.
	listCmd.Flags().Bool("with-blockers", false, "add open_blockers[] to each row — the OPEN beads blocking it, each with {id,status,type,title,edge_type} — so you learn WHY rows are not ready without one kd show per bead. Uses the same blocking edge types and statuses as --no-blockers, so it explains EVERY row that flag hides. A blocker outside your project scope is still listed (with out_of_scope=true and its title redacted) so a gated row never reads as unblocked. Opt-in (one extra batched query per page); HTTP transport only.")
	// --json-envelope is an OPT-IN JSON shape for scripted callers (ka-0000001033).
	// It implies --json and wraps the page as {beads,count,total,has_more,offset,
	// limit} so a caller can distinguish a --limit-capped page from the complete
	// set in ONE call — the bare-array default (correctly, ka-0000007254) cannot.
	// It is list-only: `kd count` already returns the scriptable total, so the
	// envelope adds nothing to a count. Off by default so `kd list --json` stays
	// a bare array for every existing `jq '.[]'` consumer.
	listCmd.Flags().Bool("json-envelope", false, "output JSON as an object {beads,count,total,has_more,offset,limit} (implies --json) so scripted callers can tell a --limit-capped page from the whole set; default --json stays a bare array. For just the total, use kd count.")
}
