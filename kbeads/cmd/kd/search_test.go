package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func TestResolveStatuses_Positive(t *testing.T) {
	got, err := resolveStatuses([]string{"open", "in_progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "open" || got[1] != "in_progress" {
		t.Errorf("resolveStatuses = %v, want [open in_progress]", got)
	}
}

func TestResolveStatuses_Negated(t *testing.T) {
	got, err := resolveStatuses([]string{"!closed"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "!closed" must expand to every valid non-closed status — including
	// blocked and partial, which a triage query most needs. See ka-0000006476.
	expected := map[string]bool{"open": true, "in_progress": true, "deferred": true, "blocked": true, "partial": true}
	if len(got) != len(expected) {
		t.Fatalf("resolveStatuses = %v, want %d statuses", got, len(expected))
	}
	for _, s := range got {
		if !expected[s] {
			t.Errorf("unexpected status %q in result", s)
		}
	}
	for want := range expected {
		found := false
		for _, s := range got {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing status %q in !closed result %v", want, got)
		}
	}
}

// TestAllStatuses_CoversValidStatuses guards the completeness basis for status
// negation: allStatuses must list every status model.Status.IsValid accepts,
// or "!status" negation silently drops beads in an omitted status (ka-0000006476).
func TestAllStatuses_CoversValidStatuses(t *testing.T) {
	for _, s := range allStatuses {
		if !model.Status(s).IsValid() {
			t.Errorf("allStatuses contains invalid status %q", s)
		}
	}
	valid := []model.Status{
		model.StatusOpen, model.StatusInProgress, model.StatusDeferred,
		model.StatusBlocked, model.StatusPartial, model.StatusClosed,
	}
	for _, want := range valid {
		found := false
		for _, s := range allStatuses {
			if s == want.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allStatuses missing valid status %q", want)
		}
	}
}

func TestResolveStatuses_MultipleNegated(t *testing.T) {
	got, err := resolveStatuses([]string{"!closed", "!deferred"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := map[string]bool{"open": true, "in_progress": true, "blocked": true, "partial": true}
	if len(got) != len(expected) {
		t.Fatalf("resolveStatuses = %v, want %d statuses", got, len(expected))
	}
	for _, s := range got {
		if !expected[s] {
			t.Errorf("unexpected status %q in result", s)
		}
	}
}

func TestResolveStatuses_MixedError(t *testing.T) {
	_, err := resolveStatuses([]string{"open", "!closed"})
	if err == nil {
		t.Error("expected error for mixed positive and negated statuses")
	}
}

func TestResolveStatuses_Empty(t *testing.T) {
	got, err := resolveStatuses(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("resolveStatuses(nil) = %v, want empty", got)
	}
}

func TestParseWhereFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"nil", nil, nil},
		{"single", []string{"project=gardenio"}, map[string]string{"project": "gardenio"}},
		{"empty_value", []string{"project="}, map[string]string{"project": ""}},
		{"multiple", []string{"project=gardenio", "role=crew"}, map[string]string{"project": "gardenio", "role": "crew"}},
		{"value_with_equals", []string{"url=https://example.com/path?a=1"}, map[string]string{"url": "https://example.com/path?a=1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWhereFlags(tt.flags)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseWhereFlags = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseWhereFlags = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseWhereFlags[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestBeadFieldValue(t *testing.T) {
	b := &beadForFieldTest{
		fields: `{"project":"gardenio","count":42,"nested":{"a":"b"}}`,
	}
	bead := b.toBead()

	if got := beadFieldValue(bead, "project"); got != "gardenio" {
		t.Errorf("beadFieldValue(project) = %q, want %q", got, "gardenio")
	}
	if got := beadFieldValue(bead, "count"); got != "42" {
		t.Errorf("beadFieldValue(count) = %q, want %q", got, "42")
	}
	if got := beadFieldValue(bead, "missing"); got != "" {
		t.Errorf("beadFieldValue(missing) = %q, want empty", got)
	}
}

// TestBeadColumnValue_Score covers the relevance-score custom column added for
// Option 2 (ka-0000003832). Both the `score` alias and the `relevance_score`
// long form render the float to three decimals.
func TestBeadColumnValue_Score(t *testing.T) {
	b := &model.Bead{RelevanceScore: 2.5}
	for _, col := range []string{"score", "relevance_score"} {
		if got := beadColumnValue(b, col); got != "2.500" {
			t.Errorf("beadColumnValue(%q) = %q, want %q", col, got, "2.500")
		}
	}
}

// TestPrintBeadListTableWithScore_RendersScoreColumn verifies the search table
// renderer prepends a SCORE column and formats each row's relevance score.
func TestPrintBeadListTableWithScore_RendersScoreColumn(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-a", Status: model.StatusOpen, Type: model.TypeBug, Title: "first", RelevanceScore: 3.25},
		{ID: "gd-b", Status: model.StatusOpen, Type: model.TypeTask, Title: "second", RelevanceScore: 1.0},
	}
	out := captureStdout(t, func() {
		printBeadListTableWithScore(beads, 2)
	})
	for _, want := range []string{"SCORE", "3.250", "gd-a", "1.000", "gd-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected table output to contain %q, got:\n%s", want, out)
		}
	}
}

// beadForFieldTest is a helper to create test beads with JSON fields.
type beadForFieldTest struct {
	fields string
}

func (b *beadForFieldTest) toBead() *model.Bead {
	return &model.Bead{
		Fields: json.RawMessage(b.fields),
	}
}

// --- --semantic flag path ---

// setSearchFlags sets flags on searchCmd for a single test, restoring defaults
// on cleanup so the global cobra command is not mutated across tests.
// resetSearchFlags returns searchCmd's flags to their defaults. searchCmd is a
// package-global cobra command shared across every test, so a filter flag left
// set by one test would otherwise leak into the next (ka-0000003751) — the
// empty-result hint in particular branches on which filters are present, so a
// stale --where silently changes another test's stderr. Called both at the
// START of setSearchFlags (defend against leak-in) and via Cleanup (leak-out).
func resetSearchFlags() {
	// A plain Flags().Set(name, "") does NOT clear a StringSlice flag (--where,
	// --status, …): pflag's stringSliceValue keeps its OWN private `changed`
	// bool (distinct from pflag.Flag.Changed), and once set it APPENDS on every
	// later Set — so Set("") is a no-op append and the stale value survives into
	// the next test. That leak was harmless until the empty-result hint began
	// branching on which filters are present (ka-0000003751). The only reliable
	// reset is pflag's SliceValue.Replace, reached here via an inline interface
	// to avoid importing pflag in the test.
	scalarDefaults := map[string]string{
		"semantic": "false", "min-score": "0", "limit": "20",
		"assignee": "", "fields": "", "sort": "", "mentions": "",
	}
	sliceFlags := []string{"status", "type", "kind", "label", "label-any", "where"}
	for name, def := range scalarDefaults {
		if f := searchCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
			_ = f.Value.Set(def)
		}
	}
	for _, name := range sliceFlags {
		f := searchCmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		f.Changed = false
		if sv, ok := f.Value.(interface{ Replace([]string) error }); ok {
			_ = sv.Replace(nil)
		}
	}
	// --json is a rootCmd persistent flag bound to the package-global jsonOutput
	// (main.go). A prior test leaving it true suppresses the empty-result hint
	// (the hint is gated on !jsonOutput), so reset the global directly.
	jsonOutput = false
}

func setSearchFlags(t *testing.T, flags map[string]string) {
	t.Helper()
	resetSearchFlags()
	t.Cleanup(resetSearchFlags)
	for k, v := range flags {
		if err := searchCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s=%s: %v", k, v, err)
		}
	}
}

func TestSearchCommand_SemanticFlag_RoutesToSemanticSearch(t *testing.T) {
	mc := newMockClient()
	mc.SemanticSearchResp = &client.SemanticSearchResponse{
		Model: "test-embed",
		Results: []*client.SemanticSearchResult{
			{Bead: &model.Bead{ID: "gd-AAA", Title: "match", Type: model.TypeTask, Status: model.StatusOpen}, Score: 0.9},
		},
	}
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{
		"semantic":  "true",
		"limit":     "5",
		"min-score": "0.25",
		"status":    "open",
		"type":      "task",
		"label":     "project:gardenio",
		"assignee":  "alice",
	})

	if err := searchCmd.RunE(searchCmd, []string{"failed", "login"}); err != nil {
		t.Fatalf("searchCmd.RunE: %v", err)
	}

	// ListBeads must NOT be called when --semantic is set.
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("ListBeads called %d times, want 0 when --semantic is set", len(mc.ListBeadsCalls))
	}
	if len(mc.SemanticSearchCalls) != 1 {
		t.Fatalf("SemanticSearch calls = %d, want 1", len(mc.SemanticSearchCalls))
	}
	req := mc.SemanticSearchCalls[0]
	if req.Query != "failed login" {
		t.Errorf("Query = %q, want 'failed login'", req.Query)
	}
	if req.Limit != 5 {
		t.Errorf("Limit = %d, want 5", req.Limit)
	}
	if req.MinScore != 0.25 {
		t.Errorf("MinScore = %v, want 0.25", req.MinScore)
	}
	if len(req.Status) != 1 || req.Status[0] != "open" {
		t.Errorf("Status = %v, want [open]", req.Status)
	}
	if len(req.Type) != 1 || req.Type[0] != "task" {
		t.Errorf("Type = %v, want [task]", req.Type)
	}
	if len(req.Labels) != 1 || req.Labels[0] != "project:gardenio" {
		t.Errorf("Labels = %v, want [project:gardenio]", req.Labels)
	}
	if req.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice", req.Assignee)
	}
}

func TestSearchCommand_SemanticFlag_RejectsEmptyQuery(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{"semantic": "true"})

	err := searchCmd.RunE(searchCmd, nil)
	if err == nil {
		t.Fatal("expected error for empty --semantic query, got nil")
	}
	if len(mc.SemanticSearchCalls) != 0 {
		t.Errorf("SemanticSearch called %d times, want 0 when query is empty", len(mc.SemanticSearchCalls))
	}
}

func TestSearchCommand_SemanticFlag_PropagatesClientError(t *testing.T) {
	mc := newMockClient()
	mc.SemanticSearchErr = errors.New("injected failure")
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{"semantic": "true"})

	err := searchCmd.RunE(searchCmd, []string{"anything"})
	if err == nil {
		t.Fatal("expected error from client, got nil")
	}
}

func TestSearchCommand_NoSemanticFlag_UsesListBeads(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setSearchFlags(t, nil)

	if err := searchCmd.RunE(searchCmd, []string{"anything"}); err != nil {
		t.Fatalf("searchCmd.RunE: %v", err)
	}
	if len(mc.SemanticSearchCalls) != 0 {
		t.Errorf("SemanticSearch called %d times, want 0 without --semantic", len(mc.SemanticSearchCalls))
	}
	if len(mc.ListBeadsCalls) != 1 {
		t.Errorf("ListBeads calls = %d, want 1", len(mc.ListBeadsCalls))
	}
}

// TestSearchCommand_EmptyLexicalResult_HintsSemantic verifies that a non-JSON
// lexical search with a query but 0 rows emits the --semantic recovery hint on
// stderr (ka-0000007281). Lexical search ANDs every token, so a long NL query
// can silently intersect to empty; the hint converts that dead-end into a lane.
func TestSearchCommand_EmptyLexicalResult_HintsSemantic(t *testing.T) {
	mc := newMockClient() // ListBeadsResult nil → empty response
	withMockClient(t, mc)
	setSearchFlags(t, nil)

	stderr := captureStderr(t, func() {
		if err := searchCmd.RunE(searchCmd, []string{"long natural language query with many terms"}); err != nil {
			t.Fatalf("searchCmd.RunE: %v", err)
		}
	})
	if !strings.Contains(stderr, "--semantic") {
		t.Errorf("expected empty-result hint to mention --semantic, got stderr:\n%s", stderr)
	}
	// A multi-token query is the only case where "requires ALL terms" is the
	// correct diagnosis (ka-0000003751).
	if !strings.Contains(stderr, "requires ALL terms") {
		t.Errorf("expected multi-token hint to explain the AND-of-terms footgun, got stderr:\n%s", stderr)
	}
}

// TestSearchCommand_EmptyLexicalResult_SingleToken_NoAllTermsClaim verifies that
// a single-token empty query does NOT assert the "requires ALL terms" cause —
// there is no term to drop, so that advice would misattribute the zero
// (ka-0000003751). --semantic is still offered.
func TestSearchCommand_EmptyLexicalResult_SingleToken_NoAllTermsClaim(t *testing.T) {
	mc := newMockClient() // empty response
	withMockClient(t, mc)
	setSearchFlags(t, nil)

	stderr := captureStderr(t, func() {
		if err := searchCmd.RunE(searchCmd, []string{"zzzqqxunlikely"}); err != nil {
			t.Fatalf("searchCmd.RunE: %v", err)
		}
	})
	if !strings.Contains(stderr, "--semantic") {
		t.Errorf("expected single-token empty hint to still offer --semantic, got stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "requires ALL terms") {
		t.Errorf("single-token query has no terms to drop; hint must not claim ALL-terms cause, got stderr:\n%s", stderr)
	}
}

// TestSearchCommand_EmptyLexicalResult_FilterZeroed_BlamesFilter verifies that
// when a structured filter accompanies a non-empty query and the result is
// empty, the hint points at the FILTER rather than at term-dropping — the query
// alone might match plenty (ka-0000003751).
func TestSearchCommand_EmptyLexicalResult_FilterZeroed_BlamesFilter(t *testing.T) {
	mc := newMockClient() // empty response
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{"where": "project=nosuchproject_xyz"})

	stderr := captureStderr(t, func() {
		if err := searchCmd.RunE(searchCmd, []string{"migration"}); err != nil {
			t.Fatalf("searchCmd.RunE: %v", err)
		}
	})
	if !strings.Contains(stderr, "filter") {
		t.Errorf("expected filter-zeroed hint to blame the filter, got stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "requires ALL terms") {
		t.Errorf("a filter emptied this, not the token intersection; hint must not claim ALL-terms cause, got stderr:\n%s", stderr)
	}
}

// TestSearchCommand_EmptyResult_NoQuery_NoHint verifies the hint is suppressed
// for a pure field-filter listing (no text query) — that is not the AND-of-
// tokens footgun and --semantic would not help.
func TestSearchCommand_EmptyResult_NoQuery_NoHint(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{"assignee": "nobody"})

	stderr := captureStderr(t, func() {
		if err := searchCmd.RunE(searchCmd, nil); err != nil {
			t.Fatalf("searchCmd.RunE: %v", err)
		}
	})
	if strings.Contains(stderr, "--semantic") {
		t.Errorf("did not expect --semantic hint for a query-less listing, got stderr:\n%s", stderr)
	}
}

// TestSearchCommand_NonEmptyResult_NoHint verifies the hint is NOT emitted when
// the lexical search actually returns rows.
func TestSearchCommand_NonEmptyResult_NoHint(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Status: model.StatusOpen, Type: model.TypeBug, Title: "hit"},
	}
	withMockClient(t, mc)
	setSearchFlags(t, nil)

	stderr := captureStderr(t, func() {
		if err := searchCmd.RunE(searchCmd, []string{"dammed"}); err != nil {
			t.Fatalf("searchCmd.RunE: %v", err)
		}
	})
	if strings.Contains(stderr, "--semantic") {
		t.Errorf("did not expect --semantic hint when results were returned, got stderr:\n%s", stderr)
	}
}

func TestSearchCommand_MentionsFlag_PropagatesToListBeads(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setSearchFlags(t, map[string]string{
		"mentions": "ka-0000004973",
		"assignee": "alice",
	})

	if err := searchCmd.RunE(searchCmd, nil); err != nil {
		t.Fatalf("searchCmd.RunE: %v", err)
	}

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("ListBeads calls = %d, want 1", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if req.Mentions != "ka-0000004973" {
		t.Errorf("Mentions = %q, want ka-0000004973", req.Mentions)
	}
	// --mentions composes with the other structured filters: a sibling
	// scalar flag (assignee) reaches the request alongside Mentions.
	if req.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice", req.Assignee)
	}
}

func TestSearchCommand_MentionsFlag_DefaultsEmpty(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setSearchFlags(t, nil)

	if err := searchCmd.RunE(searchCmd, nil); err != nil {
		t.Fatalf("searchCmd.RunE: %v", err)
	}
	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("ListBeads calls = %d, want 1", len(mc.ListBeadsCalls))
	}
	if got := mc.ListBeadsCalls[0].Mentions; got != "" {
		t.Errorf("Mentions = %q, want empty when flag not set", got)
	}
}
