package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// TestBuildReadyQuerySortsPriorityFirst pins the fix for ka-0000001517: ready
// beads must be ordered by priority first (P0 to the top), with -created_at as
// the tiebreaker, so high-priority work cannot sink below the default Limit.
func TestBuildReadyQuerySortsPriorityFirst(t *testing.T) {
	q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})

	if q.Sort != "priority,-created_at" {
		t.Errorf("ready sort = %q, want %q (priority primary, -created_at tiebreaker)", q.Sort, "priority,-created_at")
	}
}

// TestBuildReadyQueryRequestsParent pins that `gb ready` always opts into the
// daemon's parent projection so `gb ready --json` carries parent_id per row —
// letting a dispatcher tell a leaf task under an epic from a floating one, and
// group by epic, without an N+1 kd show (ka-0000006083).
func TestBuildReadyQueryRequestsParent(t *testing.T) {
	q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
	if !q.WithParent {
		t.Error("WithParent = false, want true — gb ready must surface each row's parent-epic id")
	}
}

func TestBuildReadyQueryFilters(t *testing.T) {
	t.Run("default project scope and scheduled exclusion", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Types: []string{"task"}, Assignee: "alice", Project: "gardenio", Limit: 20})

		if !slices.Equal(q.Statuses, []string{"open"}) {
			t.Errorf("statuses = %v, want [open]", q.Statuses)
		}
		if !slices.Equal(q.Kinds, []string{"issue"}) {
			t.Errorf("kinds = %v, want [issue]", q.Kinds)
		}
		if !q.NoOpenDeps {
			t.Error("NoOpenDeps = false, want true")
		}
		if !q.ExcludeScheduled {
			t.Error("ExcludeScheduled = false, want true (scheduled beads hidden by default)")
		}
		if q.Assignee != "alice" {
			t.Errorf("assignee = %q, want alice", q.Assignee)
		}
		if !slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want to contain project:gardenio", q.Labels)
		}
		// This is a project-scoped view, so the daemon narrows to beads
		// exclusive to this project server-side via ExclusiveProject — the SQL
		// LIMIT accounts for the narrowing, so the query asks for the BARE limit
		// with no client-side over-fetch (ka-0000003411).
		if q.Limit != 20 {
			t.Errorf("limit = %d, want 20 (bare limit; server-side ExclusiveProject removed the over-fetch)", q.Limit)
		}
		if q.ExclusiveProject != "gardenio" {
			t.Errorf("ExclusiveProject = %q, want gardenio (server-side project narrowing)", q.ExclusiveProject)
		}
	})

	t.Run("all-projects drops project label", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AllProjects: true})
		if slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want no project label under --all-projects", q.Labels)
		}
	})

	t.Run("include-scheduled disables scheduled exclusion", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, IncludeScheduled: true})
		if q.ExcludeScheduled {
			t.Error("ExcludeScheduled = true, want false under --include-scheduled")
		}
	})

	t.Run("-l/--label ANDs and preserves the project label", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Labels: []string{"needs-intake"}})
		// Project scoping must survive alongside the user label; the project
		// label is appended first so both are ANDed.
		if !slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want to contain project:gardenio", q.Labels)
		}
		if !slices.Contains(q.Labels, "needs-intake") {
			t.Errorf("labels = %v, want to contain needs-intake", q.Labels)
		}
		// project label ordered before the user label.
		if q.Labels[0] != "project:gardenio" {
			t.Errorf("labels[0] = %q, want project:gardenio (project scoping first)", q.Labels[0])
		}
	})

	t.Run("--label-any maps to LabelsAny (OR)", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, LabelsAny: []string{"release-stuck", "preflight-broken"}})
		if !slices.Equal(q.LabelsAny, []string{"release-stuck", "preflight-broken"}) {
			t.Errorf("LabelsAny = %v, want [release-stuck preflight-broken]", q.LabelsAny)
		}
	})

	t.Run("--exclude-label maps to ExcludeLabels", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, ExcludeLabels: []string{"needs-intake"}})
		// The implicit `thread` (ka-0000003760), `sentinel` (ka-0000000675), and
		// `kind:doc` (ka-0000000054) exclusions ride alongside the user's own
		// --exclude-label rather than replacing it.
		if !slices.Equal(q.ExcludeLabels, []string{readyThreadLabel, readySentinelLabel, readyDocLabel, "needs-intake"}) {
			t.Errorf("ExcludeLabels = %v, want [%s %s %s needs-intake]", q.ExcludeLabels, readyThreadLabel, readySentinelLabel, readyDocLabel)
		}
	})

	t.Run("label filters combine with project scoping under all combos", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Labels: []string{"kind:ced-audit"}, LabelsAny: []string{"a", "b"}, ExcludeLabels: []string{"needs-intake"}})
		if !slices.Contains(q.Labels, "project:gardenio") || !slices.Contains(q.Labels, "kind:ced-audit") {
			t.Errorf("Labels = %v, want project:gardenio + kind:ced-audit", q.Labels)
		}
		if !slices.Equal(q.LabelsAny, []string{"a", "b"}) {
			t.Errorf("LabelsAny = %v, want [a b]", q.LabelsAny)
		}
		if !slices.Equal(q.ExcludeLabels, []string{readyThreadLabel, readySentinelLabel, readyDocLabel, "needs-intake"}) {
			t.Errorf("ExcludeLabels = %v, want [%s %s %s needs-intake]", q.ExcludeLabels, readyThreadLabel, readySentinelLabel, readyDocLabel)
		}
	})

	// --plan is the ka-0000001367 ergonomics fix: scope the ready set to a plan's
	// transitive parent-child subtree so a plan-gardener's own open, unblocked
	// steps surface instead of sinking below the limit under project-wide work.
	t.Run("--plan sets Parent (subtree scope)", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"})
		if q.Parent != "ka-0000004689" {
			t.Errorf("Parent = %q, want ka-0000004689", q.Parent)
		}
	})

	t.Run("--plan overrides the implicit project label (ID-anchored)", func(t *testing.T) {
		// A subtree query is already scoped to one plan; ANDing the caller's
		// project label would drop steps that legitimately carry a different
		// project label (mirrors kd list --parent, ka-0000008241).
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"})
		if slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want no project label under --plan (ID-anchored subtree)", q.Labels)
		}
	})

	t.Run("--plan still preserves readiness invariants", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"})
		if !slices.Equal(q.Statuses, []string{"open"}) {
			t.Errorf("statuses = %v, want [open]", q.Statuses)
		}
		if !q.NoOpenDeps {
			t.Error("NoOpenDeps = false, want true — --plan must not surface blocked steps")
		}
		if !q.ExcludeScheduled {
			t.Error("ExcludeScheduled = false, want true under --plan")
		}
	})
}

// TestBuildReadyQueryExcludeType pins the ka-0000007209 fix: `gb ready
// --exclude-type` forwards the negative type filter to the daemon's server-side
// `type NOT IN (...)` clause — the same one `kd list --exclude-type` already
// used. Before this, dropping the container types (epic/molecule) from the
// ready queue required `gb ready --json | jq 'map(select(.type != …))'`, a
// client-side post-filter over a full page pull whose one wrong jq field name
// silently returns an empty set indistinguishable from "no work".
func TestBuildReadyQueryExcludeType(t *testing.T) {
	t.Run("forwards to ExcludeTypes", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, ExcludeTypes: []string{"epic", "molecule"}})
		if !slices.Equal(q.ExcludeTypes, []string{"epic", "molecule"}) {
			t.Errorf("ExcludeTypes = %v, want [epic molecule]", q.ExcludeTypes)
		}
	})

	t.Run("unset leaves ExcludeTypes empty", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
		if len(q.ExcludeTypes) != 0 {
			t.Errorf("ExcludeTypes = %v, want empty when --exclude-type is unset", q.ExcludeTypes)
		}
	})

	// --exclude-type must compose with --type rather than replace it: the
	// motivating query is "task/bug/feature, minus the containers".
	t.Run("composes with --type", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{
			Project: "gardenio", Limit: 20,
			Types:        []string{"task", "bug"},
			ExcludeTypes: []string{"epic"},
		})
		if !slices.Equal(q.Types, []string{"task", "bug"}) {
			t.Errorf("Types = %v, want [task bug]", q.Types)
		}
		if !slices.Equal(q.ExcludeTypes, []string{"epic"}) {
			t.Errorf("ExcludeTypes = %v, want [epic]", q.ExcludeTypes)
		}
	})

	// It is orthogonal to scoping and readiness: neither the label scope
	// rewrite nor the no-open-deps invariant may be disturbed.
	t.Run("preserves readiness invariants and project scope", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, ExcludeTypes: []string{"epic"}})
		if !q.NoOpenDeps || !q.ExcludeScheduled {
			t.Error("--exclude-type must not disturb NoOpenDeps / ExcludeScheduled")
		}
		if !slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want to still contain project:gardenio", q.Labels)
		}
	})
}

// TestBuildReadyQueryUnassigned pins the other half of ka-0000007209: `gb ready
// --unassigned` maps onto the daemon's `assignee IS NULL OR assignee = <empty>`
// predicate. This is the clause `gb ready` exists to serve — every claim-then-
// work agent wants "open, unblocked, nobody on it yet" — and it had no
// server-side spelling at all: `--assignee ""` reads as "no assignee filter" and
// hands back beads other agents are already driving.
func TestBuildReadyQueryUnassigned(t *testing.T) {
	t.Run("sets Unassigned", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Unassigned: true})
		if !q.Unassigned {
			t.Error("Unassigned = false, want true under --unassigned")
		}
		// The scalar assignee filter must stay EMPTY — a stray assignee here
		// would AND with the unassigned predicate and match nothing.
		if q.Assignee != "" {
			t.Errorf("Assignee = %q, want empty under --unassigned", q.Assignee)
		}
	})

	t.Run("unset leaves Unassigned false", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
		if q.Unassigned {
			t.Error("Unassigned = true, want false when the flag is unset — it would hide every claimed bead")
		}
	})

	// The composite work-seeking query the bug report named: open + unblocked +
	// unclaimed + in-lane, minus the containers, all server-side.
	t.Run("composes with type/exclude-type into the full work-seeking query", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{
			Project: "gardenio", Limit: 20,
			Types:        []string{"task", "bug", "feature"},
			ExcludeTypes: []string{"epic", "molecule"},
			Unassigned:   true,
		})
		if !q.Unassigned {
			t.Error("Unassigned = false, want true")
		}
		if !slices.Equal(q.ExcludeTypes, []string{"epic", "molecule"}) {
			t.Errorf("ExcludeTypes = %v, want [epic molecule]", q.ExcludeTypes)
		}
		if !slices.Equal(q.Statuses, []string{"open"}) || !q.NoOpenDeps {
			t.Error("the composite query must keep the open + unblocked readiness invariants")
		}
		if !slices.Contains(q.Labels, "project:gardenio") {
			t.Errorf("labels = %v, want to contain project:gardenio", q.Labels)
		}
	})
}

// resetReadyFlags clears the readyCmd flags a subtest sets. readyCmd is a
// package-level cobra command, so a value left behind (especially the
// --unassigned bool, which HIDES every claimed bead) leaks into later tests.
func resetReadyFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		readyCmd.Flags().VisitAll(func(f *pflag.Flag) {
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
				return
			}
			_ = f.Value.Set(f.DefValue)
		})
	}
	clear()
	t.Cleanup(clear)
}

// TestReadyOptionsFromFlags pins the flag→readyOptions mapping. buildReadyQuery's
// tests construct readyOptions directly, so they would stay green if a flag were
// read into the WRONG field (the failure mode the struct refactor exists to
// prevent) or not read at all. This is the test that actually parses argv.
func TestReadyOptionsFromFlags(t *testing.T) {
	t.Run("maps every filter flag to its field", func(t *testing.T) {
		resetReadyFlags(t)
		args := []string{
			"--type", "task,bug",
			"--exclude-type", "epic,molecule",
			"--unassigned",
			"--limit", "42",
			"--project", "gardenio",
			"--label", "kind:ced-audit",
			"--label-any", "a,b",
			"--exclude-label", "needs-intake",
			"--include-scheduled",
		}
		if err := readyCmd.Flags().Parse(args); err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}

		if !slices.Equal(opts.Types, []string{"task", "bug"}) {
			t.Errorf("Types = %v, want [task bug]", opts.Types)
		}
		if !slices.Equal(opts.ExcludeTypes, []string{"epic", "molecule"}) {
			t.Errorf("ExcludeTypes = %v, want [epic molecule]", opts.ExcludeTypes)
		}
		if !opts.Unassigned {
			t.Error("Unassigned = false, want true")
		}
		if opts.Limit != 42 {
			t.Errorf("Limit = %d, want 42", opts.Limit)
		}
		if opts.Project != "gardenio" {
			t.Errorf("Project = %q, want gardenio", opts.Project)
		}
		if !slices.Equal(opts.Labels, []string{"kind:ced-audit"}) {
			t.Errorf("Labels = %v, want [kind:ced-audit]", opts.Labels)
		}
		if !slices.Equal(opts.LabelsAny, []string{"a", "b"}) {
			t.Errorf("LabelsAny = %v, want [a b]", opts.LabelsAny)
		}
		if !slices.Equal(opts.ExcludeLabels, []string{"needs-intake"}) {
			t.Errorf("ExcludeLabels = %v, want [needs-intake]", opts.ExcludeLabels)
		}
		if !opts.IncludeScheduled {
			t.Error("IncludeScheduled = false, want true")
		}
	})

	// The defaults path: --unassigned and --exclude-type must be OFF unless
	// asked for. A default-on --unassigned would silently hide claimed beads
	// from every bare `gb ready`.
	t.Run("defaults leave the new filters off", func(t *testing.T) {
		resetReadyFlags(t)
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Unassigned {
			t.Error("Unassigned must default to false")
		}
		if len(opts.ExcludeTypes) != 0 {
			t.Errorf("ExcludeTypes = %v, want empty by default", opts.ExcludeTypes)
		}
	})

	t.Run("--unassigned with --assignee is rejected", func(t *testing.T) {
		resetReadyFlags(t)
		if err := readyCmd.Flags().Parse([]string{"--unassigned", "--assignee", "alice"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err := readyOptionsFromFlags(readyCmd)
		if err == nil {
			t.Fatal("want an error for --unassigned with --assignee, got nil")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("error %q should say the flags are mutually exclusive", err)
		}
	})

	// --assignee alone must still work: the guard fires only on the pair.
	t.Run("--assignee alone is accepted", func(t *testing.T) {
		resetReadyFlags(t)
		if err := readyCmd.Flags().Parse([]string{"--assignee", "alice"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("--assignee alone must be accepted, got %v", err)
		}
		if opts.Assignee != "alice" {
			t.Errorf("Assignee = %q, want alice", opts.Assignee)
		}
	})

	// The --plan/--parent conflict is surfaced through this path too.
	t.Run("conflicting --plan and --parent is rejected", func(t *testing.T) {
		resetReadyFlags(t)
		if err := readyCmd.Flags().Parse([]string{"--plan", "gd-m0thf9jx5j", "--parent", "gd-parent22222"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if _, err := readyOptionsFromFlags(readyCmd); err == nil {
			t.Fatal("want an error for conflicting --plan/--parent, got nil")
		}
	})

	// --parent must resolve into Plan (the alias), not be silently dropped.
	t.Run("--parent resolves into Plan", func(t *testing.T) {
		resetReadyFlags(t)
		if err := readyCmd.Flags().Parse([]string{"--parent", "ka-0000004689"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Plan != "ka-0000004689" {
			t.Errorf("Plan = %q, want ka-0000004689 (from --parent alias)", opts.Plan)
		}
	})
}

// TestReadyFlagsRegistered guards that the two new flags are actually registered
// on readyCmd. buildReadyQuery is unit-tested through the readyOptions struct, so
// a missing Flags() registration would leave every test above green while
// `gb ready --unassigned` failed as an unknown flag at the command line.
// TestBuildReadyQueryLeafDefault pins the core of epic ka-0000000542 / audit
// ka-0000001484: a work-seeking agent's default `gb ready` scopes to claimable
// LEAF WORK — category=intermediate AND no epic/molecule containers — while a
// human/local caller (no agent identity) and the escape hatches keep the full
// view. buildReadyQuery is a pure daemon-free unit, so AgentContext is passed in
// the options rather than read from env here.
func TestBuildReadyQueryLeafDefault(t *testing.T) {
	t.Run("agent context scopes to intermediate leaf work + excludes containers", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true})
		if !slices.Contains(q.Categories, leafDefaultCategory) {
			t.Errorf("Categories = %v, want to contain %q", q.Categories, leafDefaultCategory)
		}
		for _, ct := range readyContainerTypes {
			if !slices.Contains(q.ExcludeTypes, ct) {
				t.Errorf("ExcludeTypes = %v, want to contain container type %q", q.ExcludeTypes, ct)
			}
		}
	})

	t.Run("no agent identity fails open to the full view", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: false})
		if len(q.Categories) != 0 {
			t.Errorf("Categories = %v, want empty for a human/no-identity caller (fail open)", q.Categories)
		}
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want no implicit container exclusion for a human caller", q.ExcludeTypes)
		}
	})

	t.Run("--all restores the flat view even in an agent context", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, All: true})
		if len(q.Categories) != 0 {
			t.Errorf("Categories = %v, want empty under --all", q.Categories)
		}
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want no container exclusion under --all", q.ExcludeTypes)
		}
	})

	t.Run("explicit --category overrides the implicit leaf default", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, Categories: []string{"intake"}})
		if !slices.Equal(q.Categories, []string{"intake"}) {
			t.Errorf("Categories = %v, want [intake] (explicit --category wins)", q.Categories)
		}
		// An explicit category means the caller stated the scope: the implicit
		// container exclusion is NOT applied.
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want no implicit container exclusion when --category is explicit", q.ExcludeTypes)
		}
	})

	t.Run("--include-epics keeps containers but retains the category scope", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, IncludeEpics: true})
		if !slices.Contains(q.Categories, leafDefaultCategory) {
			t.Errorf("Categories = %v, want to still contain %q under --include-epics", q.Categories, leafDefaultCategory)
		}
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want epics retained under --include-epics", q.ExcludeTypes)
		}
	})

	t.Run("--plan subtree suppresses the leaf default", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, Plan: "ka-0000004689"})
		if len(q.Categories) != 0 {
			t.Errorf("Categories = %v, want empty under --plan (subtree is already the scope)", q.Categories)
		}
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want no container exclusion under --plan (steps may be epics)", q.ExcludeTypes)
		}
	})

	t.Run("container exclusion composes with an explicit --exclude-type without duplicating", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, ExcludeTypes: []string{"epic"}})
		got := 0
		for _, ct := range q.ExcludeTypes {
			if ct == "epic" {
				got++
			}
		}
		if got != 1 {
			t.Errorf("ExcludeTypes = %v, want exactly one 'epic' (no duplicate from the leaf default)", q.ExcludeTypes)
		}
		if !slices.Contains(q.ExcludeTypes, "molecule") {
			t.Errorf("ExcludeTypes = %v, want to also contain molecule", q.ExcludeTypes)
		}
	})

	// ka-0000004790 (step 4): published-deliverable types (bundle/dashboard/
	// rich-report/mr/merge-request) get their own `artifact` category from
	// DeriveCategory (migration 043, MR !7580 / ka-0000007274), so the
	// category=intermediate leaf scope excludes them BY CONSTRUCTION. The former
	// readyDeliverableTypes ExcludeTypes backstop (ka-0000006718) covered legacy
	// rows carrying a stale `intermediate` stamp before the post-deploy
	// `kd admin backfill-category` sweep; that sweep has run and 0
	// `intermediate`-stamped deliverables remain fleet-wide, so the backstop is
	// retired and the leaf default now relies on the category scope alone.
	deliverableTypes := []string{"bundle", "dashboard", "rich-report", "mr", "merge-request"}

	t.Run("agent context scopes category=intermediate to exclude deliverables (no type backstop)", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true})
		if !slices.Contains(q.Categories, "intermediate") {
			t.Errorf("Categories = %v, want to contain intermediate (excludes artifact deliverables)", q.Categories)
		}
		for _, dt := range deliverableTypes {
			if slices.Contains(q.ExcludeTypes, dt) {
				t.Errorf("ExcludeTypes = %v, want NO type backstop for deliverable %q (retired; category scope excludes it)", q.ExcludeTypes, dt)
			}
		}
	})

	// Deliverables stay out under --include-epics too: that flag re-includes only
	// epic/molecule containers, and the category=intermediate scope (not a type
	// filter) is what keeps `artifact` deliverables out.
	t.Run("--include-epics keeps category=intermediate so deliverables stay excluded", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, IncludeEpics: true})
		if !slices.Contains(q.Categories, "intermediate") {
			t.Errorf("Categories = %v, want to contain intermediate under --include-epics", q.Categories)
		}
		for _, dt := range deliverableTypes {
			if slices.Contains(q.ExcludeTypes, dt) {
				t.Errorf("ExcludeTypes = %v, want NO type backstop for deliverable %q under --include-epics", q.ExcludeTypes, dt)
			}
		}
		if slices.Contains(q.ExcludeTypes, "epic") {
			t.Errorf("ExcludeTypes = %v, want epics retained under --include-epics", q.ExcludeTypes)
		}
	})

	// A human/no-identity caller keeps the full view: no category scope and no
	// deliverable exclusion, so deliverables are visible.
	t.Run("no agent identity keeps deliverables (no category scope, no exclusion)", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: false})
		if slices.Contains(q.Categories, "intermediate") {
			t.Errorf("Categories = %v, want no implicit category scope for a human caller", q.Categories)
		}
		if slices.Contains(q.ExcludeTypes, "bundle") {
			t.Errorf("ExcludeTypes = %v, want no implicit deliverable exclusion for a human caller", q.ExcludeTypes)
		}
	})

	// --all restores the full view even in an agent context: no category scope,
	// no deliverable exclusion.
	t.Run("--all restores deliverables even in an agent context", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, All: true})
		if slices.Contains(q.Categories, "intermediate") {
			t.Errorf("Categories = %v, want no category scope under --all", q.Categories)
		}
		if slices.Contains(q.ExcludeTypes, "bundle") {
			t.Errorf("ExcludeTypes = %v, want no deliverable exclusion under --all", q.ExcludeTypes)
		}
	})

	// An explicit --exclude-type composes with the leaf default's container-type
	// exclusion without duplicating: the leaf default adds epic/molecule, so an
	// already-present 'epic' must not be added a second time.
	t.Run("container exclusion composes with an explicit --exclude-type without duplicating", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AgentContext: true, ExcludeTypes: []string{"epic"}})
		got := 0
		for _, ct := range q.ExcludeTypes {
			if ct == "epic" {
				got++
			}
		}
		if got != 1 {
			t.Errorf("ExcludeTypes = %v, want exactly one 'epic' (no duplicate from the leaf default)", q.ExcludeTypes)
		}
	})
}

func TestReadyFlagsRegistered(t *testing.T) {
	for _, name := range []string{"exclude-type", "unassigned", "all", "category", "include-epics"} {
		if readyCmd.Flags().Lookup(name) == nil {
			t.Errorf("gb ready must register --%s", name)
		}
	}
}

// TestReadyLimitShorthand pins the fix for gd-AZe9H1awWQ: `gb ready -n 5` must
// work, matching the sibling queue command `gb news -n 5`. The two commands
// expose the same conceptual --limit flag, and an asymmetric shorthand surface
// meant `gb ready -n 0` died with "unknown shorthand flag: n in -n" for a caller
// carrying the habit over from gb news.
func TestReadyLimitShorthand(t *testing.T) {
	f := readyCmd.Flags().ShorthandLookup("n")
	if f == nil {
		t.Fatal("gb ready must register -n as the shorthand for --limit (parity with gb news)")
	}
	if f.Name != "limit" {
		t.Errorf("-n must resolve to --limit, got --%s", f.Name)
	}
}

// TestReadyOptionsJSONFullSet pins the ka-0000006944 fix: `gb ready --json` with
// no explicit --limit is treated as PROGRAMMATIC and returns the full ready set
// (readyJSONFullLimit = the daemon's max page size), not the 20-row human
// browsing default. The bare-array JSON carries no truncation marker (advice
// ka-0000003286), so a dispatch lane inheriting the 20 silently reasoned over
// 2.3% of an 868-bead queue and concluded "nothing dispatchable".
//
// jsonOutput is a package-level global (set by the root --json flag). These
// subtests drive it directly, restoring it after, so the flag→limit mapping is
// exercised without a live daemon.
func TestReadyOptionsJSONFullSet(t *testing.T) {
	setJSON := func(t *testing.T, v bool) {
		t.Helper()
		old := jsonOutput
		jsonOutput = v
		t.Cleanup(func() { jsonOutput = old })
	}

	t.Run("--json with no explicit --limit returns the full set", func(t *testing.T) {
		resetReadyFlags(t)
		setJSON(t, true)
		if err := readyCmd.Flags().Parse([]string{"--project", "gardenio"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Limit != readyJSONFullLimit {
			t.Errorf("Limit = %d, want %d (--json must fetch the full ready set when --limit is unset)", opts.Limit, readyJSONFullLimit)
		}
	})

	t.Run("--json with an explicit --limit is honored (explicit wins)", func(t *testing.T) {
		resetReadyFlags(t)
		setJSON(t, true)
		if err := readyCmd.Flags().Parse([]string{"--project", "gardenio", "--limit", "5"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Limit != 5 {
			t.Errorf("Limit = %d, want 5 (an explicit --limit must survive --json)", opts.Limit)
		}
	})

	// An explicitly-typed `--limit 20` must NOT be mistaken for the inherited
	// default: cmd.Flags().Changed is the discriminator, not the value.
	t.Run("--json --limit 20 explicit is not overridden to the full set", func(t *testing.T) {
		resetReadyFlags(t)
		setJSON(t, true)
		if err := readyCmd.Flags().Parse([]string{"--project", "gardenio", "--limit", "20"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Limit != 20 {
			t.Errorf("Limit = %d, want 20 (an explicit --limit 20 must be honored, not treated as the inherited default)", opts.Limit)
		}
	})

	// The human TTY path is unchanged: no --json means the 20-row browsing page.
	t.Run("without --json the human default of 20 is kept", func(t *testing.T) {
		resetReadyFlags(t)
		setJSON(t, false)
		if err := readyCmd.Flags().Parse([]string{"--project", "gardenio"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		opts, err := readyOptionsFromFlags(readyCmd)
		if err != nil {
			t.Fatalf("readyOptionsFromFlags: %v", err)
		}
		if opts.Limit != readyDefaultLimit {
			t.Errorf("Limit = %d, want %d (a human TTY view keeps the browsing page)", opts.Limit, readyDefaultLimit)
		}
	})

	// readyJSONFullLimit must reach the daemon as a real limit and pull the whole
	// set: it is the max page size, so a single round-trip returns everything the
	// daemon holds. A regression to a small value would silently restore the bug.
	t.Run("readyJSONFullLimit is the daemon max page size", func(t *testing.T) {
		if readyJSONFullLimit < 10000 {
			t.Errorf("readyJSONFullLimit = %d, want >= 10000 (the daemon maxListBeadsLimit) so --json returns the whole ready set", readyJSONFullLimit)
		}
	})
}

// TestResolvePlanParent pins the ka-0000006275 fix: --parent is a flag-parity
// alias of --plan (matching `kd list --parent`). The two resolve to the same
// effective subtree id, and only a genuine conflict (both set to different ids)
// is an error.
func TestResolvePlanParent(t *testing.T) {
	t.Run("--parent alone resolves to the parent id", func(t *testing.T) {
		got, err := resolvePlanParent("", "ka-0000004689")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "ka-0000004689" {
			t.Errorf("got %q, want ka-0000004689", got)
		}
	})

	t.Run("--plan alone resolves to the plan id", func(t *testing.T) {
		got, err := resolvePlanParent("ka-0000004689", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "ka-0000004689" {
			t.Errorf("got %q, want ka-0000004689", got)
		}
	})

	t.Run("--plan and --parent set to the same id is allowed", func(t *testing.T) {
		got, err := resolvePlanParent("ka-0000004689", "ka-0000004689")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "ka-0000004689" {
			t.Errorf("got %q, want ka-0000004689", got)
		}
	})

	t.Run("neither set resolves to empty (unscoped)", func(t *testing.T) {
		got, err := resolvePlanParent("", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("--plan and --parent set to different ids errors", func(t *testing.T) {
		_, err := resolvePlanParent("gd-m0thf9jx5j", "gd-parent22222")
		if err == nil {
			t.Fatal("want error when --plan and --parent set to conflicting ids, got nil")
		}
	})
}

// TestReadyParentAliasEquivalence pins that --parent and --plan drive the same
// underlying subtree query: whichever flag supplied the id, buildReadyQuery
// produces an identical ListBeadsQuery.
func TestReadyParentAliasEquivalence(t *testing.T) {
	const epic = "ka-0000004689"

	viaPlan, err := resolvePlanParent(epic, "")
	if err != nil {
		t.Fatalf("resolvePlanParent(plan): %v", err)
	}
	viaParent, err := resolvePlanParent("", epic)
	if err != nil {
		t.Fatalf("resolvePlanParent(parent): %v", err)
	}

	qPlan := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: viaPlan})
	qParent := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: viaParent})

	if qPlan.Parent != epic || qParent.Parent != epic {
		t.Errorf("Parent = plan:%q parent:%q, want both %q", qPlan.Parent, qParent.Parent, epic)
	}
	if !slices.Equal(qPlan.Labels, qParent.Labels) {
		t.Errorf("Labels diverge: plan=%v parent=%v", qPlan.Labels, qParent.Labels)
	}
	if qPlan.Parent != qParent.Parent {
		t.Errorf("--parent and --plan must yield the same subtree scope: %q vs %q", qParent.Parent, qPlan.Parent)
	}
}

// TestBuildReadyQueryExcludesThreadLabel pins the fix for ka-0000003760: Slack
// thread (wish) head beads are conversations, not claimable work, and must not
// occupy the ready queue.
//
// The failure this guards: thread heads were minted at P0 (Priority omitted on
// a CreateBeadRequest whose Priority field has no omitempty → an explicit
// "priority":0). 83 open thread beads then sorted above every real P1-P3 bead
// under `priority,-created_at`, so the default --limit 20 view was 20/20
// unactionable conversation beads and ZERO work. gardener-delivery dispatches
// from a bare `gb ready`, so its bug lane was starved by construction while 19
// gardenio bugs sat open and unassigned.
func TestBuildReadyQueryExcludesThreadLabel(t *testing.T) {
	t.Run("excluded by default", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
		if !slices.Contains(q.ExcludeLabels, readyThreadLabel) {
			t.Errorf("ExcludeLabels = %v, want to contain %q — thread beads must not fill the ready queue",
				q.ExcludeLabels, readyThreadLabel)
		}
	})

	// Excluding a label the caller explicitly asked FOR would make the query
	// self-contradictory and always return nothing. An explicit request wins.
	t.Run("-l thread opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Labels: []string{readyThreadLabel}})
		if slices.Contains(q.ExcludeLabels, readyThreadLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when -l %s was requested",
				q.ExcludeLabels, readyThreadLabel, readyThreadLabel)
		}
	})

	t.Run("--label-any thread opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, LabelsAny: []string{readyThreadLabel, "other"}})
		if slices.Contains(q.ExcludeLabels, readyThreadLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when --label-any included it",
				q.ExcludeLabels, readyThreadLabel)
		}
	})

	// The exclusion is orthogonal to scoping: it must survive --all-projects and
	// --plan, the two paths that rewrite the label/parent scope.
	t.Run("survives all-projects and plan scoping", func(t *testing.T) {
		for name, q := range map[string]struct{ ExcludeLabels []string }{
			"all-projects": {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AllProjects: true}).ExcludeLabels},
			"plan":         {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"}).ExcludeLabels},
		} {
			if !slices.Contains(q.ExcludeLabels, readyThreadLabel) {
				t.Errorf("%s: ExcludeLabels = %v, want to contain %q", name, q.ExcludeLabels, readyThreadLabel)
			}
		}
	})
}

// TestBuildReadyQueryExcludesSentinelLabel pins the fix for ka-0000000675: a
// wake-keeper SENTINEL bead is a permanently-open kind=issue child that exists
// only to keep a standing gardener's preflight gate lit (the gate counts >0 open
// issue-kind children of the gardener's epic, so the sentinel cannot become a
// kind=data doc that would leave ready by construction). It is not claimable
// work — its own body forbids closing it, and closing it would sever the
// gardener's wake path — so it must never render as a fake ready row.
//
// This mirrors TestBuildReadyQueryExcludesThreadLabel: same class of queue
// noise, same default-exclude-by-label mechanism, same opt-back-in escape hatch.
func TestBuildReadyQueryExcludesSentinelLabel(t *testing.T) {
	t.Run("excluded by default", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
		if !slices.Contains(q.ExcludeLabels, readySentinelLabel) {
			t.Errorf("ExcludeLabels = %v, want to contain %q — sentinel beads must not fill the ready queue",
				q.ExcludeLabels, readySentinelLabel)
		}
	})

	// Excluding a label the caller explicitly asked FOR would make the query
	// self-contradictory and always return nothing. An explicit request wins.
	t.Run("-l sentinel opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Labels: []string{readySentinelLabel}})
		if slices.Contains(q.ExcludeLabels, readySentinelLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when -l %s was requested",
				q.ExcludeLabels, readySentinelLabel, readySentinelLabel)
		}
	})

	t.Run("--label-any sentinel opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, LabelsAny: []string{readySentinelLabel, "other"}})
		if slices.Contains(q.ExcludeLabels, readySentinelLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when --label-any included it",
				q.ExcludeLabels, readySentinelLabel)
		}
	})

	// The exclusion is orthogonal to scoping: it must survive --all-projects and
	// --plan, the two paths that rewrite the label/parent scope.
	t.Run("survives all-projects and plan scoping", func(t *testing.T) {
		for name, q := range map[string]struct{ ExcludeLabels []string }{
			"all-projects": {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AllProjects: true}).ExcludeLabels},
			"plan":         {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"}).ExcludeLabels},
		} {
			if !slices.Contains(q.ExcludeLabels, readySentinelLabel) {
				t.Errorf("%s: ExcludeLabels = %v, want to contain %q", name, q.ExcludeLabels, readySentinelLabel)
			}
		}
	})
}

// TestBuildReadyQueryExcludesDocLabel pins the fix for ka-0000000054: a
// `kind:doc` STANDING PROCEDURE (a gardener cycle procedure, runbook, or other
// live reference text) is deliberately held open/in_progress forever so a
// perpetual gardener's wake path stays lit — NOT because there is a code feature
// to build against it. The delivery/build gardener dispatches from `gb ready`, so
// such a bead (ka-0000000327, the live gardener-release-owner cycle procedure —
// type=task, kind=issue, area:release, kind:doc) looked dispatchable and
// misrouted two crew onto a running gardener's live procedure within ~13 minutes.
//
// This mirrors TestBuildReadyQueryExcludesThreadLabel / ...ExcludesSentinelLabel:
// same class of queue noise, same default-exclude-by-label mechanism, same
// opt-back-in escape hatch.
func TestBuildReadyQueryExcludesDocLabel(t *testing.T) {
	t.Run("excluded by default", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
		if !slices.Contains(q.ExcludeLabels, readyDocLabel) {
			t.Errorf("ExcludeLabels = %v, want to contain %q — kind:doc standing procedures must not fill the ready queue",
				q.ExcludeLabels, readyDocLabel)
		}
	})

	// Excluding a label the caller explicitly asked FOR would make the query
	// self-contradictory and always return nothing. An explicit request wins.
	t.Run("-l kind:doc opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Labels: []string{readyDocLabel}})
		if slices.Contains(q.ExcludeLabels, readyDocLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when -l %s was requested",
				q.ExcludeLabels, readyDocLabel, readyDocLabel)
		}
	})

	t.Run("--label-any kind:doc opts back in", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, LabelsAny: []string{readyDocLabel, "other"}})
		if slices.Contains(q.ExcludeLabels, readyDocLabel) {
			t.Errorf("ExcludeLabels = %v, want NOT to contain %q when --label-any included it",
				q.ExcludeLabels, readyDocLabel)
		}
	})

	// The exclusion is orthogonal to scoping: it must survive --all-projects and
	// --plan, the two paths that rewrite the label/parent scope.
	t.Run("survives all-projects and plan scoping", func(t *testing.T) {
		for name, q := range map[string]struct{ ExcludeLabels []string }{
			"all-projects": {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, AllProjects: true}).ExcludeLabels},
			"plan":         {buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20, Plan: "ka-0000004689"}).ExcludeLabels},
		} {
			if !slices.Contains(q.ExcludeLabels, readyDocLabel) {
				t.Errorf("%s: ExcludeLabels = %v, want to contain %q", name, q.ExcludeLabels, readyDocLabel)
			}
		}
	})
}

// TestBuildReadyQuerySetsExclusiveProject pins the server-side migration of
// ka-0000004559 (ka-0000003411): the daemon narrows to beads exclusive to the
// scoped project via ExclusiveProject, so the SQL LIMIT accounts for the
// narrowing and the query asks for the BARE limit — no client-side over-fetch.
// Measured in prod before the fix: a bare `gb ready` printed "18 beads (909
// total)" against --limit 20 because 2 rows in the top-20 window carried both
// project:gardenio and project:monorepo.
//
// ExclusiveProject is set ONLY under the un-anchored, project-scoped view —
// narrowing when nothing scopes the result (all-projects / plan / no project)
// would wrongly hide cross-project rows the caller asked to see.
func TestBuildReadyQuerySetsExclusiveProject(t *testing.T) {
	t.Run("project-scoped view narrows server-side with a bare limit", func(t *testing.T) {
		const limit = 20
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: limit})

		if q.ExclusiveProject != "gardenio" {
			t.Errorf("ExclusiveProject = %q, want gardenio (server-side narrowing to the scoped project)", q.ExclusiveProject)
		}
		if q.Limit != limit {
			t.Errorf("Limit = %d, want %d (bare limit; the server-side filter removed the over-fetch)", q.Limit, limit)
		}
	})

	// --all-projects, --plan, and an un-scoped view all skip the narrowing, so
	// ExclusiveProject must be empty and the limit unchanged.
	for name, opts := range map[string]readyOptions{
		"all-projects skips narrowing": {Project: "gardenio", AllProjects: true, Limit: 20},
		"plan scope skips narrowing":   {Project: "gardenio", Plan: "gd-plan1", Limit: 20},
		"no project to narrow by":      {Limit: 20},
	} {
		t.Run(name, func(t *testing.T) {
			q := buildReadyQuery(opts)
			if q.ExclusiveProject != "" {
				t.Errorf("ExclusiveProject = %q, want empty (no project narrowing runs)", q.ExclusiveProject)
			}
			if q.Limit != 20 {
				t.Errorf("Limit = %d, want 20 (no over-fetch)", q.Limit)
			}
		})
	}

	// A limit of 0 means "no limit" — it must stay unlimited even when narrowing.
	t.Run("zero limit is left alone", func(t *testing.T) {
		q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 0})
		if q.Limit != 0 {
			t.Errorf("Limit = %d, want 0 (unlimited must stay unlimited)", q.Limit)
		}
		if q.ExclusiveProject != "gardenio" {
			t.Errorf("ExclusiveProject = %q, want gardenio (narrowing applies even when unlimited)", q.ExclusiveProject)
		}
	})
}

// TestReadyAssigneeNote pins the assignee-liveness annotation that closes
// ka-0000005196: `gb ready` returns only status=open beads, so a non-empty
// assignee is always anomalous, and the three shapes it takes (dead agent,
// human, sentinel) are none of them straightforwardly claimable. The note must
// surface that WITHOUT stripping the row — two of the three must not be reaped.
func TestReadyAssigneeNote(t *testing.T) {
	live := map[string]bool{"gardenio-live-agent-abc": true}

	t.Run("empty assignee is genuinely claimable — no note", func(t *testing.T) {
		if note := readyAssigneeNote("", live); note != "" {
			t.Errorf("note = %q, want empty for an unassigned (claimable) row", note)
		}
	})

	// The dead thread agent from the bug report: not in the live set, so it must
	// be flagged not-live and NOT presented as free to grab.
	t.Run("dead agent assignee is flagged not-live", func(t *testing.T) {
		note := readyAssigneeNote("thread-1783103092-785779", live)
		if note == "" {
			t.Fatal("note is empty, want a not-live-agent warning for a dead-agent assignee")
		}
		if !strings.Contains(note, "not a live agent") {
			t.Errorf("note = %q, want it to say the assignee is not a live agent", note)
		}
	})

	// A human operator (jordanlake) and a sentinel string are both absent from the
	// live set exactly like a dead agent; the note is deliberately conservative
	// rather than asserting a reap that would be wrong for these two.
	t.Run("human operator assignee is flagged not-live", func(t *testing.T) {
		if note := readyAssigneeNote("jordanlake", live); !strings.Contains(note, "not a live agent") {
			t.Errorf("note = %q, want a not-live-agent warning for a human assignee", note)
		}
	})
	t.Run("sentinel assignee is flagged not-live", func(t *testing.T) {
		if note := readyAssigneeNote("l2-gate-time-hold-until-2026-08-19", live); !strings.Contains(note, "not a live agent") {
			t.Errorf("note = %q, want a not-live-agent warning for a sentinel assignee", note)
		}
	})

	// Matching against the live set is case-insensitive (the roster keys are
	// lowercased, mirroring splitActiveStale in news.go).
	t.Run("live agent assignee is flagged as live, coordinate-first (case-insensitive)", func(t *testing.T) {
		note := readyAssigneeNote("Gardenio-Live-Agent-ABC", live)
		if !strings.Contains(note, "live agent") || strings.Contains(note, "not a live agent") {
			t.Errorf("note = %q, want a live-agent coordinate-first note", note)
		}
		if !strings.Contains(note, "coordinate") {
			t.Errorf("note = %q, want it to tell the reader to coordinate", note)
		}
	})
}
