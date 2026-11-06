package server

import (
	"slices"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildListFilter_StatusInMerged(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		Status:   []string{"open"},
		StatusIn: []string{"in_progress", "open"}, // duplicate "open" should dedupe
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Status) != 2 {
		t.Fatalf("expected 2 unique statuses, got %d (%v)", len(f.Status), f.Status)
	}
	if !slices.Contains(f.Status, model.StatusOpen) || !slices.Contains(f.Status, model.StatusInProgress) {
		t.Fatalf("expected open+in_progress, got %v", f.Status)
	}
}

func TestBuildListFilter_TypeInAliasResolved(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		TypeIn: []string{"template"},
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(f.Type, model.TypeFormula) {
		t.Fatalf("expected formula via template alias, got %v", f.Type)
	}
}

// TestBuildListFilter_LabelsAnyRoutesToUser locks in the ka-0000003307
// invariant: the user-facing labels_any proto field maps to
// model.BeadFilter.LabelsAnyUser (its own AND-ed EXISTS clause), NOT the
// internal LabelsAny field the server reserves for OIDC project scoping. If a
// future refactor naively routes labels_any into LabelsAny, a user any-of
// would OR-merge with the OIDC project labels and silently widen project
// isolation — this test fails loudly if that happens.
func TestBuildListFilter_LabelsAnyRoutesToUser(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		LabelsAny: []string{"release-stuck", "preflight-broken", "release-pipeline-broken"},
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.LabelsAnyUser) != 3 {
		t.Fatalf("expected labels_any to populate LabelsAnyUser (3 values), got %v", f.LabelsAnyUser)
	}
	if len(f.LabelsAny) != 0 {
		t.Fatalf("labels_any must NOT touch the OIDC-scoping LabelsAny field; got %v", f.LabelsAny)
	}
}

func TestBuildListFilter_AssigneeOwnerCreatedByIn(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		AssigneeIn:  []string{"alice", "bob"},
		OwnerIn:     []string{"carol"},
		CreatedByIn: []string{"dave", "eve"},
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Assignees) != 2 || f.Assignees[0] != "alice" || f.Assignees[1] != "bob" {
		t.Fatalf("Assignees mismatch: %v", f.Assignees)
	}
	if len(f.Owners) != 1 || f.Owners[0] != "carol" {
		t.Fatalf("Owners mismatch: %v", f.Owners)
	}
	if len(f.CreatedBys) != 2 {
		t.Fatalf("CreatedBys mismatch: %v", f.CreatedBys)
	}
}

func TestBuildListFilter_DecisionPendingDefaultsStatus(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{DecisionPending: true}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(f.Type, model.TypeDecision) {
		t.Fatalf("expected type=decision, got %v", f.Type)
	}
	if len(f.Status) != 2 ||
		!slices.Contains(f.Status, model.StatusOpen) ||
		!slices.Contains(f.Status, model.StatusInProgress) {
		t.Fatalf("expected status defaulted to open+in_progress, got %v", f.Status)
	}
}

func TestBuildListFilter_DecisionPendingHonorsExplicitStatusSubset(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		DecisionPending: true,
		Status:          []string{"open"},
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Status) != 1 || f.Status[0] != model.StatusOpen {
		t.Fatalf("expected only open, got %v", f.Status)
	}
}

func TestBuildListFilter_DecisionPendingRejectsConflictingType(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		DecisionPending: true,
		Type:            []string{"task"},
	}
	_, err := buildListFilter(req)
	if err == nil {
		t.Fatal("expected error for decision_pending + non-decision type")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestBuildListFilter_DecisionPendingRejectsConflictingStatus(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		DecisionPending: true,
		Status:          []string{"closed"},
	}
	_, err := buildListFilter(req)
	if err == nil {
		t.Fatal("expected error for decision_pending + closed status")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestBuildListFilter_LastActiveBefore(t *testing.T) {
	cutoff := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	req := &beadsv1.ListBeadsRequest{
		LastActiveBefore: timestamppb.New(cutoff),
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.UpdatedBefore == nil || !f.UpdatedBefore.Equal(cutoff) {
		t.Fatalf("expected UpdatedBefore=%v, got %v", cutoff, f.UpdatedBefore)
	}
}

func TestBuildListFilter_StaleForMinutes(t *testing.T) {
	fixedNow := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	prev := nowFn
	nowFn = func() time.Time { return fixedNow }
	defer func() { nowFn = prev }()

	req := &beadsv1.ListBeadsRequest{StaleForMinutes: 30}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fixedNow.Add(-30 * time.Minute)
	if f.UpdatedBefore == nil || !f.UpdatedBefore.Equal(want) {
		t.Fatalf("expected UpdatedBefore=%v, got %v", want, f.UpdatedBefore)
	}
}

func TestBuildListFilter_StaleAndLastActiveTakesEarlier(t *testing.T) {
	fixedNow := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	prev := nowFn
	nowFn = func() time.Time { return fixedNow }
	defer func() { nowFn = prev }()

	// stale_for_minutes=10 → cutoff = 11:50 (less restrictive / later)
	// last_active_before  = 11:00              (more restrictive / earlier)
	earlier := time.Date(2026, 4, 27, 11, 0, 0, 0, time.UTC)
	req := &beadsv1.ListBeadsRequest{
		StaleForMinutes:  10,
		LastActiveBefore: timestamppb.New(earlier),
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.UpdatedBefore == nil || !f.UpdatedBefore.Equal(earlier) {
		t.Fatalf("expected earlier bound=%v, got %v", earlier, f.UpdatedBefore)
	}

	// reversed: stale_for_minutes=120 → 10:00 (earlier), last_active_before=11:30 (later)
	later := time.Date(2026, 4, 27, 11, 30, 0, 0, time.UTC)
	req2 := &beadsv1.ListBeadsRequest{
		StaleForMinutes:  120,
		LastActiveBefore: timestamppb.New(later),
	}
	f2, err := buildListFilter(req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantStale := fixedNow.Add(-120 * time.Minute)
	if f2.UpdatedBefore == nil || !f2.UpdatedBefore.Equal(wantStale) {
		t.Fatalf("expected stale bound=%v, got %v", wantStale, f2.UpdatedBefore)
	}
}

// ka-0000006652: exclude_scheduled passes through to BeadFilter so the
// SQL layer can suppress cron-spawned beads.
func TestBuildListFilter_ExcludeScheduled(t *testing.T) {
	t.Run("default false", func(t *testing.T) {
		req := &beadsv1.ListBeadsRequest{}
		f, err := buildListFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.ExcludeScheduled {
			t.Fatal("expected ExcludeScheduled=false by default (backward compat)")
		}
	})
	t.Run("set true", func(t *testing.T) {
		req := &beadsv1.ListBeadsRequest{ExcludeScheduled: true}
		f, err := buildListFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.ExcludeScheduled {
			t.Fatal("expected ExcludeScheduled=true")
		}
	})
}

func TestBuildListFilter_LegacyScalarsPassthrough(t *testing.T) {
	req := &beadsv1.ListBeadsRequest{
		Assignee: "alice",
		Labels:   []string{"urgent"},
		Search:   "login",
		Limit:    25,
		Offset:   50,
		Sort:     "-priority",
	}
	f, err := buildListFilter(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Assignee != "alice" || f.Search != "login" || f.Sort != "-priority" {
		t.Fatalf("scalar passthrough failed: %+v", f)
	}
	if f.Limit != 25 || f.Offset != 50 {
		t.Fatalf("limit/offset mismatch: %d/%d", f.Limit, f.Offset)
	}
	if len(f.Labels) != 1 || f.Labels[0] != "urgent" {
		t.Fatalf("labels mismatch: %v", f.Labels)
	}
}

// gd-9ephUcYMIy: ListBeadsRequest.mentions / mentions_type must propagate
// into BeadFilter with the same three-state semantics as the HTTP path
// (TestHandleListBeads_MentionsTypeQueryParam):
//   - field absent     → MentionsType nil
//   - field present "" → non-nil pointer to "" (untyped only)
//   - field "<verb>"   → non-nil pointer to "<verb>"
func TestBuildListFilter_MentionsTypeThreeState(t *testing.T) {
	mentionsTarget := "gd-Target001"
	cases := []struct {
		name     string
		mentions *string
		mtype    *string
		wantM    string
		wantMT   *string
	}{
		{"omitted", &mentionsTarget, nil, "gd-Target001", nil},
		{"empty value", &mentionsTarget, strPtr(""), "gd-Target001", strPtr("")},
		{"verb", &mentionsTarget, strPtr("blocks"), "gd-Target001", strPtr("blocks")},
		{"no mentions filter", nil, nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &beadsv1.ListBeadsRequest{
				Mentions:     tc.mentions,
				MentionsType: tc.mtype,
			}
			f, err := buildListFilter(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.Mentions != tc.wantM {
				t.Errorf("Mentions = %q, want %q", f.Mentions, tc.wantM)
			}
			switch {
			case tc.wantMT == nil && f.MentionsType != nil:
				t.Errorf("MentionsType = %q, want nil", *f.MentionsType)
			case tc.wantMT != nil && f.MentionsType == nil:
				t.Errorf("MentionsType = nil, want %q", *tc.wantMT)
			case tc.wantMT != nil && f.MentionsType != nil && *tc.wantMT != *f.MentionsType:
				t.Errorf("MentionsType = %q, want %q", *f.MentionsType, *tc.wantMT)
			}
		})
	}
}

// ka-0000007923 (R1): ListBeadsRequest.has_report must propagate into
// BeadFilter.HasReport with three-state semantics matching the HTTP path
// (TestHandleListBeads_HasReportQueryParam):
//   - field absent → HasReport nil
//   - field true   → *true
//   - field false  → *false
func TestBuildListFilter_HasReportThreeState(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name      string
		hasReport *bool
		want      *bool
	}{
		{"omitted", nil, nil},
		{"true", &tru, &tru},
		{"false", &fal, &fal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &beadsv1.ListBeadsRequest{
				HasReport: tc.hasReport,
			}
			f, err := buildListFilter(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch {
			case tc.want == nil && f.HasReport != nil:
				t.Errorf("HasReport = %v, want nil", *f.HasReport)
			case tc.want != nil && f.HasReport == nil:
				t.Errorf("HasReport = nil, want %v", *tc.want)
			case tc.want != nil && f.HasReport != nil && *tc.want != *f.HasReport:
				t.Errorf("HasReport = %v, want %v", *f.HasReport, *tc.want)
			}
		})
	}
}

// TestBuildListFilter_LimitClamped asserts the gRPC list path shares the same
// OOM choke point as the HTTP handler: an unset/zero limit defaults, and a
// limit above the cap is clamped. Regression guard for the unbounded-gRPC-
// ListBeads finding (ka-0000003195; HTTP-side rationale ka-0000006889).
func TestBuildListFilter_LimitClamped(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want int
	}{
		{"zero defaults", 0, defaultListBeadsLimit},
		{"negative defaults", -1, defaultListBeadsLimit},
		{"under cap preserved", 500, 500},
		{"at cap preserved", maxListBeadsLimit, maxListBeadsLimit},
		{"over cap clamped", maxListBeadsLimit + 1, maxListBeadsLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := buildListFilter(&beadsv1.ListBeadsRequest{Limit: tc.in})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.Limit != tc.want {
				t.Fatalf("limit=%d: got clamped %d, want %d", tc.in, f.Limit, tc.want)
			}
		})
	}
}
