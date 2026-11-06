package bridge

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// issue is a tiny constructor for a JiraIssue carrying the fields the picker
// reads (key, labels, created).
func issue(key, created string, labels ...string) JiraIssue {
	return JiraIssue{
		Key: key,
		Fields: JiraIssueFields{
			Created: created,
			Labels:  labels,
		},
	}
}

func TestParseWaveKeys_AbbreviatedRun(t *testing.T) {
	// The wave titles abbreviate after the first key: "FSM-360,361,395,398".
	title := "Wave B — crf-display (9): FSM-360,361,395,398,428,436,437,458,461"
	got := ParseWaveKeys(title)
	want := []string{
		"FSM-360", "FSM-361", "FSM-395", "FSM-398", "FSM-428",
		"FSM-436", "FSM-437", "FSM-458", "FSM-461",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWaveKeys abbreviated run:\n got  %v\n want %v", got, want)
	}
}

func TestParseWaveKeys_SpacedAndFullyPrefixed(t *testing.T) {
	// Description form spells every key with the FSM- prefix and spaces.
	body := "WAVE C — tmf (8): FSM-355, FSM-366, FSM-372, FSM-373, FSM-394, FSM-420, FSM-425, FSM-465."
	got := ParseWaveKeys(body)
	want := []string{
		"FSM-355", "FSM-366", "FSM-372", "FSM-373",
		"FSM-394", "FSM-420", "FSM-425", "FSM-465",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWaveKeys spaced+prefixed:\n got  %v\n want %v", got, want)
	}
}

func TestParseWaveKeys_StopsAtProse(t *testing.T) {
	// A run must stop when prose follows a key — the "(9)" count and trailing
	// theme text must NOT be captured as keys, and numbers embedded in prose
	// after a key must not extend the run.
	title := "Wave G — perf (4): FSM-399,400,401,410 — likely one shared root cause, triage as a set"
	got := ParseWaveKeys(title)
	want := []string{"FSM-399", "FSM-400", "FSM-401", "FSM-410"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWaveKeys stops at prose:\n got  %v\n want %v", got, want)
	}
}

func TestParseWaveKeys_MultipleRunsAndDedup(t *testing.T) {
	// Title names the keys; body repeats them with a stray key. Order of first
	// appearance is preserved and duplicates collapse.
	text := "Wave F — audit-trail (4): FSM-310,456,462,463\n\nWAVE F — audit-trail (4): FSM-310, FSM-456, FSM-462, FSM-463. See FSM-999 for context."
	got := ParseWaveKeys(text)
	want := []string{"FSM-310", "FSM-456", "FSM-462", "FSM-463", "FSM-999"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWaveKeys multi-run dedup:\n got  %v\n want %v", got, want)
	}
}

func TestParseWaveKeys_CaseInsensitive(t *testing.T) {
	got := ParseWaveKeys("please triage fsm-186 and Fsm-190")
	want := []string{"FSM-186", "FSM-190"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWaveKeys case-insensitive:\n got  %v\n want %v", got, want)
	}
}

func TestParseWaveKeys_None(t *testing.T) {
	if got := ParseWaveKeys("no keys here, just prose about tickets"); got != nil {
		t.Fatalf("ParseWaveKeys with no keys: got %v, want nil", got)
	}
}

func TestParseWaveKeys_LargeMiscBatch(t *testing.T) {
	// The Waves I-K misc step names 22 keys in one abbreviated run.
	title := "Waves I-K — misc (22, unclustered): FSM-220,247,319,336,345,347,356,359,362,363,364,402,405,411,417,431,434,442,444,446,449,459"
	got := ParseWaveKeys(title)
	if len(got) != 22 {
		t.Fatalf("ParseWaveKeys misc batch: got %d keys, want 22: %v", len(got), got)
	}
	if got[0] != "FSM-220" || got[21] != "FSM-459" {
		t.Fatalf("ParseWaveKeys misc batch bounds: first=%s last=%s", got[0], got[21])
	}
}

func TestParseWaveLabel(t *testing.T) {
	cases := map[string]string{
		"Wave B — crf-display (9): FSM-360":           "B",
		"Waves I-K — misc (22, unclustered): FSM-220": "IK",
		"Pilot wave A: FSM-220, 228":                  "A",
		"no wave here":                                "",
	}
	for title, want := range cases {
		if got := ParseWaveLabel(title); got != want {
			t.Errorf("ParseWaveLabel(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestSelectFromIssues_OldestFirstPick(t *testing.T) {
	keys := []string{"FSM-360", "FSM-361", "FSM-395"}
	issues := []JiraIssue{
		issue("FSM-360", "2026-05-01T10:00:00.000+0000"),
		issue("FSM-361", "2026-02-09T10:00:00.000+0000"), // oldest
		issue("FSM-395", "2026-06-15T10:00:00.000+0000"),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-361" {
		t.Fatalf("Pick = %q, want FSM-361 (oldest)", got.Pick)
	}
	wantOrder := []string{"FSM-361", "FSM-360", "FSM-395"}
	if !reflect.DeepEqual(got.Untriaged, wantOrder) {
		t.Fatalf("Untriaged order = %v, want %v", got.Untriaged, wantOrder)
	}
	if len(got.Skipped) != 0 || len(got.Missing) != 0 {
		t.Fatalf("expected no skipped/missing, got skipped=%v missing=%v", got.Skipped, got.Missing)
	}
}

func TestSelectFromIssues_SkipsTriaged(t *testing.T) {
	keys := []string{"FSM-360", "FSM-361", "FSM-395"}
	issues := []JiraIssue{
		// FSM-360 already triaged — must be skipped even though it is oldest.
		issue("FSM-360", "2026-02-01T10:00:00.000+0000", "gardenio-triaged", "gardenio-triaged-sev-2"),
		issue("FSM-361", "2026-05-09T10:00:00.000+0000"),
		issue("FSM-395", "2026-06-15T10:00:00.000+0000"),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-361" {
		t.Fatalf("Pick = %q, want FSM-361 (oldest UN-triaged; FSM-360 skipped)", got.Pick)
	}
	if !reflect.DeepEqual(got.Skipped, []string{"FSM-360"}) {
		t.Fatalf("Skipped = %v, want [FSM-360]", got.Skipped)
	}
	if !reflect.DeepEqual(got.Untriaged, []string{"FSM-361", "FSM-395"}) {
		t.Fatalf("Untriaged = %v, want [FSM-361 FSM-395]", got.Untriaged)
	}
}

func TestSelectFromIssues_SevLabelAloneIsNotTriaged(t *testing.T) {
	// A stray sev- refinement label WITHOUT the bare sentinel does NOT count as
	// triaged — the outbox writes the sentinel to mark a ticket done.
	keys := []string{"FSM-400"}
	issues := []JiraIssue{
		issue("FSM-400", "2026-06-01T00:00:00.000+0000", "gardenio-triaged-sev-1", "sev-1"),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-400" {
		t.Fatalf("Pick = %q, want FSM-400 (sev-label alone is not the sentinel)", got.Pick)
	}
	if len(got.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want empty", got.Skipped)
	}
}

func TestSelectFromIssues_FullyDrainedWaveEmptyPick(t *testing.T) {
	keys := []string{"FSM-360", "FSM-361"}
	issues := []JiraIssue{
		issue("FSM-360", "2026-02-01T00:00:00.000+0000", "gardenio-triaged"),
		issue("FSM-361", "2026-03-01T00:00:00.000+0000", "gardenio-triaged"),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "" {
		t.Fatalf("Pick = %q, want empty (wave fully drained)", got.Pick)
	}
	if len(got.Untriaged) != 0 {
		t.Fatalf("Untriaged = %v, want empty", got.Untriaged)
	}
	if !reflect.DeepEqual(got.Skipped, []string{"FSM-360", "FSM-361"}) {
		t.Fatalf("Skipped = %v, want both keys", got.Skipped)
	}
}

func TestSelectFromIssues_MissingKeySurfaced(t *testing.T) {
	// A named key with no issue in the fetch (typo / deleted ticket) is surfaced
	// as Missing, not silently dropped.
	keys := []string{"FSM-360", "FSM-99999"}
	issues := []JiraIssue{
		issue("FSM-360", "2026-05-01T00:00:00.000+0000"),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-360" {
		t.Fatalf("Pick = %q, want FSM-360", got.Pick)
	}
	if !reflect.DeepEqual(got.Missing, []string{"FSM-99999"}) {
		t.Fatalf("Missing = %v, want [FSM-99999]", got.Missing)
	}
}

func TestSelectFromIssues_TieBreakByNumericKey(t *testing.T) {
	// Equal created timestamps → deterministic numeric-key tie-break.
	keys := []string{"FSM-461", "FSM-360", "FSM-395"}
	ts := "2026-04-01T00:00:00.000+0000"
	issues := []JiraIssue{
		issue("FSM-461", ts),
		issue("FSM-360", ts),
		issue("FSM-395", ts),
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-360" {
		t.Fatalf("Pick = %q, want FSM-360 (lowest numeric key on tie)", got.Pick)
	}
	if !reflect.DeepEqual(got.Untriaged, []string{"FSM-360", "FSM-395", "FSM-461"}) {
		t.Fatalf("Untriaged tie order = %v, want [FSM-360 FSM-395 FSM-461]", got.Untriaged)
	}
}

func TestSelectFromIssues_MissingCreatedSortsLast(t *testing.T) {
	// A ticket with no created date must NOT pre-empt one with a real old date.
	keys := []string{"FSM-360", "FSM-361"}
	issues := []JiraIssue{
		issue("FSM-360", ""),                             // no created
		issue("FSM-361", "2026-02-09T00:00:00.000+0000"), // real old date
	}
	got := SelectFromIssues(keys, issues)
	if got.Pick != "FSM-361" {
		t.Fatalf("Pick = %q, want FSM-361 (dated ticket beats dateless)", got.Pick)
	}
	if !reflect.DeepEqual(got.Untriaged, []string{"FSM-361", "FSM-360"}) {
		t.Fatalf("Untriaged = %v, want [FSM-361 FSM-360]", got.Untriaged)
	}
}

func TestSelectFromIssues_DuplicateNamedKeysCollapse(t *testing.T) {
	keys := []string{"FSM-360", "fsm-360", "FSM-361"}
	issues := []JiraIssue{
		issue("FSM-360", "2026-05-01T00:00:00.000+0000"),
		issue("FSM-361", "2026-06-01T00:00:00.000+0000"),
	}
	got := SelectFromIssues(keys, issues)
	if !reflect.DeepEqual(got.Untriaged, []string{"FSM-360", "FSM-361"}) {
		t.Fatalf("Untriaged = %v, want [FSM-360 FSM-361] (dupes collapsed)", got.Untriaged)
	}
}

// fakeSearcher is a BacklogPickIssueSearcher backed by a fixed key→issue map. It
// records the JQL it was asked so tests can assert the picker never runs a
// created-window query.
type fakeSearcher struct {
	byKey map[string]JiraIssue
	jqls  []string
	err   error
}

func (f *fakeSearcher) SearchIssues(_ context.Context, jql string, _ []string, _ int) ([]JiraIssue, error) {
	f.jqls = append(f.jqls, jql)
	if f.err != nil {
		return nil, f.err
	}
	// Return an issue for every key the JQL names that we know about. The picker
	// builds `key IN ("FSM-360","FSM-361")`; extract via the shared key regex.
	var out []JiraIssue
	for _, k := range ParseWaveKeys(jql) {
		if iss, ok := f.byKey[k]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func TestBacklogPick_LiveWiring(t *testing.T) {
	f := &fakeSearcher{byKey: map[string]JiraIssue{
		"FSM-360": issue("FSM-360", "2026-05-01T00:00:00.000+0000", "gardenio-triaged"),
		"FSM-361": issue("FSM-361", "2026-02-09T00:00:00.000+0000"),
		"FSM-395": issue("FSM-395", "2026-06-15T00:00:00.000+0000"),
	}}
	keys := ParseWaveKeys("Wave B — crf-display: FSM-360,361,395")
	got, err := BacklogPick(context.Background(), f, "B", keys)
	if err != nil {
		t.Fatalf("BacklogPick error: %v", err)
	}
	if got.Wave != "B" {
		t.Errorf("Wave = %q, want B", got.Wave)
	}
	if got.Pick != "FSM-361" {
		t.Errorf("Pick = %q, want FSM-361", got.Pick)
	}
	if !reflect.DeepEqual(got.Skipped, []string{"FSM-360"}) {
		t.Errorf("Skipped = %v, want [FSM-360]", got.Skipped)
	}
	// The picker must query by KEY, never by a created-window clause.
	for _, jql := range f.jqls {
		if !strings.Contains(jql, "key IN (") || strings.Contains(jql, "created") {
			t.Errorf("unexpected JQL %q — backlog mode must be key-scoped, never a created window", jql)
		}
	}
}

func TestBacklogPick_EmptyKeysIsNoOp(t *testing.T) {
	f := &fakeSearcher{byKey: map[string]JiraIssue{}}
	got, err := BacklogPick(context.Background(), f, "Z", nil)
	if err != nil {
		t.Fatalf("BacklogPick empty error: %v", err)
	}
	if got.Pick != "" || len(f.jqls) != 0 {
		t.Fatalf("empty keys should be a no-op: pick=%q jqls=%v", got.Pick, f.jqls)
	}
}

func TestBacklogPick_SearchErrorPropagates(t *testing.T) {
	f := &fakeSearcher{err: errors.New("jira 500")}
	_, err := BacklogPick(context.Background(), f, "B", []string{"FSM-360"})
	if err == nil {
		t.Fatal("expected error to propagate from the searcher")
	}
}

func TestBacklogPick_NilSearcher(t *testing.T) {
	if _, err := BacklogPick(context.Background(), nil, "B", []string{"FSM-360"}); err == nil {
		t.Fatal("expected error for nil searcher")
	}
}

func TestBacklogPick_BatchesLargeKeySets(t *testing.T) {
	// More than one JQL batch's worth of keys must be walked completely, not
	// truncated at the batch cap.
	byKey := map[string]JiraIssue{}
	var keys []string
	for i := 0; i < backlogPickJQLBatch+10; i++ {
		k := "FSM-" + strconv.Itoa(1000+i)
		byKey[k] = issue(k, "2026-01-01T00:00:00.000+0000")
		keys = append(keys, k)
	}
	f := &fakeSearcher{byKey: byKey}
	got, err := BacklogPick(context.Background(), f, "X", keys)
	if err != nil {
		t.Fatalf("BacklogPick batch error: %v", err)
	}
	if len(got.Untriaged) != len(keys) {
		t.Fatalf("Untriaged count = %d, want %d (all keys walked across batches)", len(got.Untriaged), len(keys))
	}
	if len(f.jqls) != 2 {
		t.Fatalf("expected 2 JQL batches for %d keys, got %d", len(keys), len(f.jqls))
	}
}
