package bridge

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func schemaFlagTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initConfigsDriftDiff is the unified diff of MR !7647 (f8dab085f), restated in
// init_configs.yaml form (the type:agent fields moved there from init.go): it
// adds two type:agent fields (resolved_by, owner) and touches no config.go.
// This is the exact incident shape the bot must flag.
const initConfigsDriftDiff = `@@ -157,6 +157,10 @@ "type:agent":
     - {name: schedule_cron, type: string}
     - {name: schedule_slack_channel, type: string}
     - {name: schedule_name, type: string}
+    # Live-card footer navigability (ka-0000003131): the resolved
+    # gardener that spawned this agent and the human owner.
+    - {name: resolved_by, type: string}
+    - {name: owner, type: string}
     # Scheduled-loop-context fields.
     - {name: schedule_generation, type: integer}`

// initConfigsCommentOnlyDiff touches init_configs.yaml but adds no field — only
// a comment. This must NOT flag.
const initConfigsCommentOnlyDiff = `@@ -157,6 +157,7 @@ "type:agent":
     - {name: schedule_name, type: string}
+    # A clarifying comment, no new field.
     - {name: schedule_generation, type: integer}`

// configGoDiff is a stand-in daemon config.go diff (contents irrelevant — the
// bot only cares that the path was touched).
const configGoDiff = `@@ -1,3 +1,4 @@
+	"resolved_by": {Type: "string"},`

// --- fake GitLab client ---

// fakeSchemaFlagGitLab serves a canned open-MR list, per-MR changes, and records
// the comments + labels the bot writes. It also honors a re-verify fetch.
type fakeSchemaFlagGitLab struct {
	openMRs   []GitLabMR
	listErr   error
	changes   map[int][]GitLabDiff // mr iid -> changes
	changeErr map[int]error        // mr iid -> ListMergeRequestChanges error

	// verifyLabels overrides the labels returned by GetMergeRequestByPath (the
	// re-verify fetch), keyed by iid; falls back to the listing's labels.
	verifyLabels map[int][]string
	verifyState  map[int]string // iid -> state on re-verify (default "opened")

	comments   map[int]string // iid -> posted comment body
	labels     map[int][]string
	postErr    map[int]error
	labelErr   map[int]error
	postCalls  int
	labelCalls int
}

func newFakeSchemaFlagGitLab() *fakeSchemaFlagGitLab {
	return &fakeSchemaFlagGitLab{
		changes:      map[int][]GitLabDiff{},
		changeErr:    map[int]error{},
		verifyLabels: map[int][]string{},
		verifyState:  map[int]string{},
		comments:     map[int]string{},
		labels:       map[int][]string{},
		postErr:      map[int]error{},
		labelErr:     map[int]error{},
	}
}

func (f *fakeSchemaFlagGitLab) ListGroupMergeRequests(_ context.Context, _ int, state string, _ time.Time) ([]GitLabMR, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if state != "opened" {
		return nil, nil
	}
	return f.openMRs, nil
}

func (f *fakeSchemaFlagGitLab) ListMergeRequestChanges(_ context.Context, _ string, iid int) ([]GitLabDiff, error) {
	if err := f.changeErr[iid]; err != nil {
		return nil, err
	}
	return f.changes[iid], nil
}

func (f *fakeSchemaFlagGitLab) GetMergeRequestByPath(_ context.Context, projectPath string, iid int) (*GitLabMR, error) {
	state := "opened"
	if s, ok := f.verifyState[iid]; ok {
		state = s
	}
	labels := f.labels[iid]
	if l, ok := f.verifyLabels[iid]; ok {
		labels = l
	}
	url := "https://gitlab.com/" + projectPath + "/-/merge_requests/" + itoa(iid)
	return &GitLabMR{WebURL: url, State: state, IID: iid, Labels: labels}, nil
}

func (f *fakeSchemaFlagGitLab) PostMRNote(_ context.Context, _ string, iid int, body string) (*GitLabNote, error) {
	f.postCalls++
	if err := f.postErr[iid]; err != nil {
		return nil, err
	}
	f.comments[iid] = body
	return &GitLabNote{ID: 1}, nil
}

func (f *fakeSchemaFlagGitLab) AddMergeRequestLabels(_ context.Context, _ string, iid int, labels []string) error {
	f.labelCalls++
	if err := f.labelErr[iid]; err != nil {
		return err
	}
	f.labels[iid] = append(f.labels[iid], labels...)
	return nil
}

func mkMR(iid int) GitLabMR {
	return GitLabMR{
		WebURL: "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/" + itoa(iid),
		State:  "opened",
		IID:    iid,
	}
}

func newSchemaFlagBot(gl schemaFlagGitLab) *MRSchemaFlagReconciler {
	return NewMRSchemaFlagReconciler(MRSchemaFlagConfig{
		GitLab:  gl,
		Logger:  schemaFlagTestLogger(),
		GroupID: 42,
	})
}

// change is a small builder for a per-file GitLabDiff.
func change(path, diff string) GitLabDiff {
	return GitLabDiff{NewPath: path, OldPath: path, Diff: diff}
}

// --- pure trigger-logic tests ---

func TestEvalSchemaDrift(t *testing.T) {
	tests := []struct {
		name       string
		changes    []GitLabDiff
		wantAdded  []string
		wantInit   bool
		wantConfig bool
	}{
		{
			name:      "incident !7647 shape: init_configs.yaml adds two fields, no config.go",
			changes:   []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)},
			wantAdded: []string{"resolved_by", "owner"},
			wantInit:  true,
		},
		{
			name: "init_configs.yaml AND config.go both touched — correctly paired",
			changes: []GitLabDiff{
				change(bridgeInitConfigsPath, initConfigsDriftDiff),
				change(daemonConfigGoPath, configGoDiff),
			},
			wantAdded:  []string{"resolved_by", "owner"},
			wantInit:   true,
			wantConfig: true,
		},
		{
			name:     "init_configs.yaml comment-only change adds no field",
			changes:  []GitLabDiff{change(bridgeInitConfigsPath, initConfigsCommentOnlyDiff)},
			wantInit: true,
		},
		{
			name:       "config.go only — never flags",
			changes:    []GitLabDiff{change(daemonConfigGoPath, configGoDiff)},
			wantConfig: true,
		},
		{
			name:    "unrelated file",
			changes: []GitLabDiff{change("gardenio/controller/internal/bridge/gitlab.go", initConfigsDriftDiff)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, touchesInit, touchesConfig := evalSchemaDrift(tt.changes)
			if touchesInit != tt.wantInit {
				t.Errorf("touchesInit = %v, want %v", touchesInit, tt.wantInit)
			}
			if touchesConfig != tt.wantConfig {
				t.Errorf("touchesConfig = %v, want %v", touchesConfig, tt.wantConfig)
			}
			if !equalStrings(added, tt.wantAdded) {
				t.Errorf("added = %v, want %v", added, tt.wantAdded)
			}
		})
	}
}

func TestAddedSchemaFields_dedup(t *testing.T) {
	diff := "+    - {name: a, type: string}\n" +
		"+    - {name: a, type: string}\n" + // dup — collapsed
		"     - {name: context, type: string}\n" + // context line (leading space) — ignored
		"-    - {name: removed, type: string}\n" + // removed line — ignored
		"+    - {name: b, type: enum, values: [x]}\n" +
		"+    - {name: \"c\", type: string}\n" // quoted name
	got := addedSchemaFields(diff)
	want := []string{"a", "b", "c"}
	if !equalStrings(got, want) {
		t.Fatalf("addedSchemaFields = %v, want %v", got, want)
	}
}

func TestSchemaFieldAddPattern_ignoresDiffHeader(t *testing.T) {
	// The `+++ b/…` file header must never be mistaken for an added field.
	if schemaFieldAddPattern.MatchString("+++ b/gardenio/controller/internal/bridge/init_configs.yaml") {
		t.Error("pattern matched the +++ diff file header")
	}
}

// --- end-to-end scan tests ---

func TestScanOnce_flagsDriftingMR(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	mr := mkMR(7647)
	gl.openMRs = []GitLabMR{mr}
	gl.changes[7647] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)}

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 1 {
		t.Fatalf("flagged = %d, want 1 (stats=%+v)", stats.flagged, stats)
	}
	body, ok := gl.comments[7647]
	if !ok {
		t.Fatal("expected a comment on MR 7647, got none")
	}
	// The comment must name the incident IDs and the added fields.
	for _, want := range []string{"ka-0000001549", "ka-0000004509", "ka-0000000811", "ka-0000003673", "resolved_by", "owner", daemonConfigGoPath} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q\n---\n%s", want, body)
		}
	}
	if got := gl.labels[7647]; !equalStrings(got, []string{schemaFlagLabel}) {
		t.Errorf("labels = %v, want [%s]", got, schemaFlagLabel)
	}
}

func TestScanOnce_pairedMR_notFlagged(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	gl.openMRs = []GitLabMR{mkMR(100)}
	gl.changes[100] = []GitLabDiff{
		change(bridgeInitConfigsPath, initConfigsDriftDiff),
		change(daemonConfigGoPath, configGoDiff),
	}

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 0 || stats.configTouched != 1 {
		t.Fatalf("expected 0 flagged / 1 configTouched, got %+v", stats)
	}
	if gl.postCalls != 0 {
		t.Errorf("posted a comment on a correctly-paired MR")
	}
}

func TestScanOnce_noInitChange_notFlagged(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	gl.openMRs = []GitLabMR{mkMR(101)}
	gl.changes[101] = []GitLabDiff{change("gardenio/controller/internal/bridge/gitlab.go", "+// unrelated\n")}

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 0 || stats.noInitChange != 1 {
		t.Fatalf("expected 0 flagged / 1 noInitChange, got %+v", stats)
	}
}

func TestScanOnce_initCommentOnly_notFlagged(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	gl.openMRs = []GitLabMR{mkMR(102)}
	gl.changes[102] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsCommentOnlyDiff)}

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 0 || stats.noFieldAdd != 1 {
		t.Fatalf("expected 0 flagged / 1 noFieldAdd, got %+v", stats)
	}
	if gl.postCalls != 0 {
		t.Errorf("posted a comment on a comment-only init_configs.yaml change")
	}
}

func TestScanOnce_alreadyLabeled_skippedCheaply(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	mr := mkMR(103)
	mr.Labels = []string{"some-other", schemaFlagLabel}
	gl.openMRs = []GitLabMR{mr}
	gl.changes[103] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)}

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 0 || stats.alreadyFlag != 1 {
		t.Fatalf("expected 0 flagged / 1 alreadyFlag, got %+v", stats)
	}
	if gl.postCalls != 0 {
		t.Errorf("re-commented on an already-labeled MR")
	}
}

// A second scan after the first flag must not re-comment: the label written by
// the first scan is the idempotency marker.
func TestScanOnce_idempotentAcrossScans(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	mr := mkMR(104)
	gl.openMRs = []GitLabMR{mr}
	gl.changes[104] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)}
	bot := newSchemaFlagBot(gl)

	bot.ScanOnce(context.Background())
	// Simulate the label landing on the listing (as GitLab would report next scan).
	gl.openMRs[0].Labels = gl.labels[104]

	stats2 := bot.ScanOnce(context.Background())
	if stats2.flagged != 0 || stats2.alreadyFlag != 1 {
		t.Fatalf("second scan re-flagged: %+v", stats2)
	}
	if gl.postCalls != 1 {
		t.Errorf("postCalls = %d, want 1 (no re-comment)", gl.postCalls)
	}
}

// The re-verify fetch guards against a concurrent labeling between the listing
// snapshot and the write: if the fresh MR is already labeled, no comment posts.
func TestScanOnce_reverifyLabelRace_noComment(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	gl.openMRs = []GitLabMR{mkMR(105)} // listing shows no label...
	gl.changes[105] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)}
	gl.verifyLabels[105] = []string{schemaFlagLabel} // ...but re-verify sees it labeled

	stats := newSchemaFlagBot(gl).ScanOnce(context.Background())

	if stats.flagged != 0 {
		t.Fatalf("flagged a labeled-on-reverify MR: %+v", stats)
	}
	if gl.postCalls != 0 {
		t.Errorf("posted despite re-verify seeing the label")
	}
}

func TestScanOnce_dryRun_noWrites(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	gl.openMRs = []GitLabMR{mkMR(106)}
	gl.changes[106] = []GitLabDiff{change(bridgeInitConfigsPath, initConfigsDriftDiff)}
	bot := NewMRSchemaFlagReconciler(MRSchemaFlagConfig{
		GitLab: gl, Logger: schemaFlagTestLogger(), GroupID: 42, DryRun: true,
	})

	stats := bot.ScanOnce(context.Background())

	if stats.flagged != 1 {
		t.Fatalf("dry-run should still count a would-flag: %+v", stats)
	}
	if gl.postCalls != 0 || gl.labelCalls != 0 {
		t.Errorf("dry-run wrote to GitLab: postCalls=%d labelCalls=%d", gl.postCalls, gl.labelCalls)
	}
}

func TestScanOnce_disabledOnZeroGroup(t *testing.T) {
	gl := newFakeSchemaFlagGitLab()
	bot := NewMRSchemaFlagReconciler(MRSchemaFlagConfig{
		GitLab: gl, Logger: schemaFlagTestLogger(), GroupID: 0,
	})
	if err := bot.Run(context.Background()); err != nil {
		t.Fatalf("Run with zero group should return nil, got %v", err)
	}
}
