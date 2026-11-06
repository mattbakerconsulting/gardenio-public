package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const (
	ghPRURL = "https://github.com/joyousaversion/gardenio/pull/42"
	taskID  = "gd-BRp3is6Dpd"
	mrBead  = "gd-mrbead0001"
)

// fakeDaemon is an in-memory beadClient that records every write.
type fakeDaemon struct {
	mu sync.Mutex

	beads  map[string]*beadsapi.BeadDetail // GetBead by id
	linked []*beadsapi.BeadDetail          // ListTaskBeadsForMR result
	agents []*beadsapi.BeadDetail          // agent beads for the task

	upserts []upsertCall
	updates map[string]map[string]string
	deps    []string
	created []beadsapi.CreateBeadRequest
	closedN int

	upsertErr error
}

type upsertCall struct {
	f      beadsapi.MergeRequestBeadFields
	closed bool
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}, updates: map[string]map[string]string{}}
}

func (d *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := d.beads[id]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (d *fakeDaemon) ListTaskBeadsForMR(_ context.Context, _ string) ([]*beadsapi.BeadDetail, error) {
	return d.linked, nil
}

func (d *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if len(q.Types) == 1 && q.Types[0] == "agent" && q.Fields["task_id"] == taskID {
		return &beadsapi.ListBeadsResult{Beads: d.agents}, nil
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *fakeDaemon) UpdateBeadFields(_ context.Context, id string, f map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Merged like the daemon's field patch: a later write keeps earlier keys.
	if d.updates[id] == nil {
		d.updates[id] = map[string]string{}
	}
	for k, v := range f {
		d.updates[id][k] = v
	}
	return nil
}

func (d *fakeDaemon) UpsertMergeRequestBead(_ context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.upsertErr != nil {
		return "", d.upsertErr
	}
	d.upserts = append(d.upserts, upsertCall{f, closed})
	return mrBead, nil
}

func (d *fakeDaemon) AddDependency(_ context.Context, id, on, typ, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deps = append(d.deps, id+" "+typ+" "+on)
	return nil
}

func (d *fakeDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, req)
	return "gd-new" + string(rune('a'+len(d.created))), nil
}

func (d *fakeDaemon) CloseBead(context.Context, string, map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closedN++
	return nil
}

func (d *fakeDaemon) createdOfType(typ string) []beadsapi.CreateBeadRequest {
	var out []beadsapi.CreateBeadRequest
	for _, r := range d.created {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// daemonWithTask returns a daemon that knows the task bead and a live agent
// bound to a Slack thread.
func daemonWithTask(taskFields map[string]string) *fakeDaemon {
	d := newFakeDaemon()
	d.beads[taskID] = &beadsapi.BeadDetail{ID: taskID, Type: "task", Status: "in_progress", Fields: taskFields}
	d.agents = []*beadsapi.BeadDetail{{
		ID: "gd-agent0001", Type: "agent", Status: "in_progress", Title: "crew-pr-events",
		Fields: map[string]string{"agent": "crew-pr-events", "task_id": taskID},
	}}
	return d
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func deliver(t *testing.T, h *prHandler, fixtureName, delivery string) error {
	t.Helper()
	ev := Event{DeliveryID: delivery, Type: "pull_request", Payload: fixture(t, fixtureName)}
	fillEnvelope(&ev)
	return h.Handle(context.Background(), ev)
}

// threadText returns the text of the one squawk-shaped line filed for the
// agent. slack-bridge posts squawk message beads into the named agent's thread.
func threadText(t *testing.T, d *fakeDaemon) string {
	t.Helper()
	notices := d.createdOfType("message")
	if len(notices) != 1 {
		t.Fatalf("thread lines = %d, want 1: %+v", len(notices), notices)
	}
	var f map[string]string
	if err := json.Unmarshal(notices[0].Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["source_agent"] != "crew-pr-events" {
		t.Errorf("source_agent = %q, want the owning agent", f["source_agent"])
	}
	l := strings.Join(notices[0].Labels, ",")
	if !strings.Contains(l, "squawk") || !strings.Contains(l, "from:crew-pr-events") {
		t.Errorf("labels %v: slack-bridge only relays squawk beads from the agent", notices[0].Labels)
	}
	return f["text"]
}

func TestPR_OpenedMatchesBranchAndWritesFields(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_opened", "d-1"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(d.upserts))
	}
	got := d.upserts[0]
	want := beadsapi.MergeRequestBeadFields{
		Forge: "github", MRURL: ghPRURL, MRIID: "42", ProjectPath: "joyousaversion/gardenio",
		SourceBranch: "agent/launch/" + taskID, TargetBranch: "main", MRState: "opened",
		Title: "feat(github-bridge): PR events -> bead fields", Author: "gardenio-bot[bot]",
	}
	if got.f.Forge != want.Forge || got.f.MRURL != want.MRURL || got.f.MRIID != want.MRIID ||
		got.f.ProjectPath != want.ProjectPath || got.f.SourceBranch != want.SourceBranch ||
		got.f.TargetBranch != want.TargetBranch || got.f.MRState != want.MRState ||
		got.f.Title != want.Title || got.f.Author != want.Author || got.f.MROutcome != "" || got.closed {
		t.Errorf("upsert = %+v closed=%v, want %+v open", got.f, got.closed, want)
	}
	if u := d.updates[taskID]; u["mr_url"] != ghPRURL || u["forge"] != "github" {
		t.Errorf("task update = %v, want mr_url + forge=github", u)
	}
	wantDeps := []string{taskID + " produces " + mrBead, mrBead + " triggered-by " + taskID}
	if strings.Join(d.deps, "|") != strings.Join(wantDeps, "|") {
		t.Errorf("deps = %v, want %v", d.deps, wantDeps)
	}
	text := threadText(t, d)
	if !strings.Contains(text, "PR opened") || !strings.Contains(text, "<"+ghPRURL+"|#42 ") {
		t.Errorf("thread line = %q", text)
	}
	if len(d.createdOfType("mail")) != 0 {
		t.Errorf("opening a PR mailed the agent that opened it")
	}
}

func TestPR_SynchronizeUpdatesFieldsButStaysQuiet(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_synchronize", "d-2"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts) != 1 || d.upserts[0].f.MRState != "opened" {
		t.Fatalf("upserts = %+v", d.upserts)
	}
	if len(d.created) != 0 {
		t.Errorf("synchronize created beads %+v, want no Slack line or mail", d.created)
	}
	if _, ok := d.updates[taskID]; ok {
		t.Errorf("task bead already had mr_url, must not be rewritten: %v", d.updates[taskID])
	}
}

func TestPR_ClosedMergedClosesMRBeadAndTellsThreadAndAgent(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_closed_merged", "d-3"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts) != 1 {
		t.Fatalf("upserts = %d", len(d.upserts))
	}
	u := d.upserts[0]
	if u.f.MRState != "merged" || u.f.MROutcome != "merged" || !u.closed {
		t.Errorf("upsert = %+v closed=%v, want merged/merged/closed", u.f, u.closed)
	}
	text := threadText(t, d)
	if !strings.Contains(text, ":white_check_mark: PR merged by matthewbaker1110") {
		t.Errorf("thread line = %q", text)
	}
	mail := d.createdOfType("mail")
	if len(mail) != 1 || mail[0].Assignee != "crew-pr-events" || mail[0].Priority != 2 ||
		!strings.Contains(mail[0].Title, "merged") || !strings.Contains(mail[0].Description, ghPRURL) {
		t.Fatalf("mail = %+v", mail)
	}
	for _, l := range mail[0].Labels {
		if l == "delivery:interrupt" {
			t.Errorf("PR mail must not interrupt the agent")
		}
	}
}

// fakeFiles is a prFilesLister with a fixed answer.
type fakeFiles struct {
	paths []string
	err   error
	calls []prRef
}

func (f *fakeFiles) PRFiles(_ context.Context, pr prRef) ([]string, error) {
	f.calls = append(f.calls, pr)
	return f.paths, f.err
}

// mergedPRFiles is a merged PR with 3 test files and 2 goldens among others.
var mergedPRFiles = []string{
	"gardenio/controller/cmd/github-bridge/pullrequest.go",
	"gardenio/controller/cmd/github-bridge/pullrequest_test.go",
	"gardenio/controller/internal/gardenhub/testdata/cursor_browser.js",
	"gardenio/tests/gardenhub-browser/shoot.spec.ts",
	"gardenio/controller/internal/gardenhub/testdata/golden/cursorphase-phone-light.png",
	"gardenio/controller/internal/gardenhub/testdata/golden/cursorphase-desktop-dark.png",
	"gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md",
}

// At merge each producing task gets ci_state from the MR bead and the PR's
// test and golden counts (harvest spec §2.3).
func TestPR_MergedWritesHarvestFields(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	d.beads[mrBead] = &beadsapi.BeadDetail{ID: mrBead, Type: "merge-request", Fields: map[string]string{"mr_pipeline_status": "success"}}
	files := &fakeFiles{paths: mergedPRFiles}
	h := newPRHandler(d, quiet())
	h.files = files
	if err := deliver(t, h, "pull_request_closed_merged", "d-h1"); err != nil {
		t.Fatal(err)
	}
	u := d.updates[taskID]
	if u["ci_state"] != "success" || u["tests_touched"] != "3" || u["goldens_touched"] != "2" {
		t.Errorf("task harvest fields = %v, want ci_state=success tests_touched=3 goldens_touched=2", u)
	}
	if len(files.calls) != 1 || files.calls[0] != (prRef{Repo: "joyousaversion/gardenio", Number: 42}) {
		t.Errorf("files read = %+v, want one read of joyousaversion/gardenio#42", files.calls)
	}
}

// Zero is written as "0", so "no tests" reads apart from "not recorded".
func TestPR_MergedWithNoTestsWritesZero(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	h := newPRHandler(d, quiet())
	h.files = &fakeFiles{paths: []string{"README.md"}}
	if err := deliver(t, h, "pull_request_closed_merged", "d-h2"); err != nil {
		t.Fatal(err)
	}
	u := d.updates[taskID]
	if u["tests_touched"] != "0" || u["goldens_touched"] != "0" {
		t.Errorf("task harvest fields = %v, want both counts 0", u)
	}
	if _, ok := u["ci_state"]; ok {
		t.Errorf("ci_state = %q with no MR pipeline status, want absent", u["ci_state"])
	}
}

// A failed files read writes neither count, keeps ci_state, and never fails the
// merge handling.
func TestPR_MergedFilesReadFailureWritesNeitherCount(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	d.beads[mrBead] = &beadsapi.BeadDetail{ID: mrBead, Type: "merge-request", Fields: map[string]string{"mr_pipeline_status": "failed"}}
	h := newPRHandler(d, quiet())
	h.files = &fakeFiles{err: errors.New("HTTP 502")}
	if err := deliver(t, h, "pull_request_closed_merged", "d-h3"); err != nil {
		t.Fatalf("a files read failure failed the delivery: %v", err)
	}
	u := d.updates[taskID]
	if _, ok := u["tests_touched"]; ok {
		t.Errorf("tests_touched written after a failed read: %v", u)
	}
	if _, ok := u["goldens_touched"]; ok {
		t.Errorf("goldens_touched written after a failed read: %v", u)
	}
	if u["ci_state"] != "failed" {
		t.Errorf("ci_state = %q, want failed", u["ci_state"])
	}
	if len(d.createdOfType("message")) != 1 {
		t.Errorf("merge announcement lost after a files read failure")
	}
}

// Only a merge records the harvest: an open or an unmerged close reads no files.
func TestPR_NotMergedWritesNoHarvestFields(t *testing.T) {
	for _, fx := range []string{"pull_request_opened", "pull_request_closed_unmerged"} {
		d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
		files := &fakeFiles{paths: mergedPRFiles}
		h := newPRHandler(d, quiet())
		h.files = files
		if err := deliver(t, h, fx, "d-h4-"+fx); err != nil {
			t.Fatal(err)
		}
		if len(files.calls) != 0 {
			t.Errorf("%s read the PR files", fx)
		}
		for _, k := range []string{"ci_state", "tests_touched", "goldens_touched"} {
			if v, ok := d.updates[taskID][k]; ok {
				t.Errorf("%s wrote %s=%q", fx, k, v)
			}
		}
	}
}

func TestClassifyPRFile(t *testing.T) {
	for _, tc := range []struct {
		path         string
		test, golden bool
	}{
		{"kbeads/internal/server/evidence_test.go", true, false},
		{"coop/crates/mux/src/lib_test.rs", true, false},
		{".claude/skills/commons-ux-acceptance/scripts/test/probes.test.js", true, false},
		{"gardenio/tests/slack-e2e/thread.spec.ts", true, false},
		{"gardenio/controller/internal/gardenhub/testdata/golden_browser.js", true, false},
		{"gardenio/controller/internal/gardenhub/testdata/golden/bead-phone-dark.png", false, true},
		{"gardenio/helm/gardenio/tests/chart_test.go", true, false},
		{"gardenio/helm/gardenio/tests/values.yaml", true, false},
		{"gardenio/tests/gardenhub-browser/goldens/opening.png", false, true},
		{"gardenio/tests/gardenhub-browser/golden-notes/README.md", false, false},
		{"gardenio/controller/internal/gardenhub/web/art.js", false, false},
		{"gardenio/controller/internal/gardenhub/testdata/farm_fixture.json", false, false},
		{"Makefile", false, false},
	} {
		test, golden := classifyPRFile(tc.path)
		if test != tc.test || golden != tc.golden {
			t.Errorf("classifyPRFile(%q) = test %v golden %v, want %v %v", tc.path, test, golden, tc.test, tc.golden)
		}
	}
}

// PRFiles pages GET pulls/{n}/files until a short page.
func TestGitHubCommenter_PRFilesPages(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7/files" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		n := 0
		if page == "1" {
			n = prFilesPerPage
		} else if page == "2" {
			n = 3
		}
		out := make([]map[string]string, n)
		for i := range out {
			out[i] = map[string]string{"filename": "f" + page + "_" + string(rune('a'+i%26)) + "_test.go"}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}
	got, err := c.PRFiles(context.Background(), prRef{Repo: "o/r", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != prFilesPerPage+3 || strings.Join(pages, ",") != "1,2" {
		t.Errorf("got %d files over pages %v, want %d over 1,2", len(got), pages, prFilesPerPage+3)
	}
}

func TestPR_ClosedWithoutMergeIsClosedNotMerged(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_closed_unmerged", "d-4"); err != nil {
		t.Fatal(err)
	}
	u := d.upserts[0]
	if u.f.MRState != "closed" || u.f.MROutcome != "closed" || !u.closed {
		t.Errorf("upsert = %+v closed=%v, want closed/closed", u.f, u.closed)
	}
	if text := threadText(t, d); !strings.Contains(text, "closed without merging") {
		t.Errorf("thread line = %q", text)
	}
}

func TestPR_NoMatchingBeadWritesNothing(t *testing.T) {
	d := newFakeDaemon()
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_opened_unrelated", "d-5"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts)+len(d.created)+len(d.updates)+len(d.deps) != 0 {
		t.Errorf("unrelated PR wrote: upserts=%v created=%v updates=%v deps=%v", d.upserts, d.created, d.updates, d.deps)
	}
}

func TestPR_LocatesByBodyAndIgnoresUnknownIDs(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_opened_body_link", "d-6"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1 (body names %s)", len(d.upserts), taskID)
	}
	if len(d.deps) != 2 {
		t.Errorf("deps = %v, want the single known bead linked", d.deps)
	}
}

func TestPR_LocatesByExistingMRURLLink(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	linked := &beadsapi.BeadDetail{ID: taskID, Type: "task", Fields: map[string]string{"mr_url": ghPRURL}}
	d.linked = []*beadsapi.BeadDetail{linked}
	delete(d.beads, taskID)
	// Branch ref is not an agent branch, body has no id: only the mr_url link
	// can find it.
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_opened_unrelated", "d-7"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(d.upserts))
	}
}

func TestPR_UninterestingActionIsIgnored(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_labeled", "d-8"); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts)+len(d.created) != 0 {
		t.Errorf("labeled wrote beads")
	}
}

// With no agent bead the task's assignee still gets the thread line, but is
// not mailed: nothing says it is alive.
func TestPR_AssigneeFallbackSquawksButDoesNotMail(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	d.agents = nil
	d.beads[taskID].Assignee = "crew-pr-events"
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_closed_merged", "d-9"); err != nil {
		t.Fatal(err)
	}
	threadText(t, d)
	if n := len(d.createdOfType("mail")); n != 0 {
		t.Errorf("unverified assignee mailed %d times", n)
	}
}

// A task nobody ever worked has nobody to tell.
func TestPR_NoAgentNoSlackLine(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	d.agents = nil
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_closed_merged", "d-9b"); err != nil {
		t.Fatal(err)
	}
	if len(d.created) != 0 {
		t.Errorf("created %+v for an agentless task", d.created)
	}
}

func TestPR_ClosedAgentIsNotMailed(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	d.agents[0].Status = "closed"
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_closed_merged", "d-10"); err != nil {
		t.Fatal(err)
	}
	if n := len(d.createdOfType("mail")); n != 0 {
		t.Errorf("despawned agent mailed %d times", n)
	}
	if n := len(d.createdOfType("message")); n != 1 {
		t.Errorf("thread line still posts for a despawned agent: got %d", n)
	}
}

func TestPR_DaemonFailureMakesDeliveryRetryable(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	d.upsertErr = errors.New("daemon down")
	if err := deliver(t, newPRHandler(d, quiet()), "pull_request_opened", "d-11"); err == nil {
		t.Fatal("Handle returned nil on upsert failure; GitHub would never redeliver")
	}
}

func TestPR_NoticeIdempotencyKeyIsPerDeliveryAndTask(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	h := newPRHandler(d, quiet())
	for range 2 {
		if err := deliver(t, h, "pull_request_closed_merged", "same-delivery"); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]int{}
	for _, c := range d.created {
		keys[c.IdempotencyKey]++
	}
	// Two attempts of one delivery present identical keys, so the daemon
	// collapses them; both the notice and the mail are keyed.
	for k, n := range keys {
		if k == "" || n != 2 {
			t.Errorf("key %q presented %d times, want every create keyed and identical across retries", k, n)
		}
	}
}

func TestPR_IgnoresOtherEventTypes(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	ev := Event{DeliveryID: "x", Type: "check_suite", Payload: fixture(t, "pull_request_opened")}
	if err := newPRHandler(d, quiet()).Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(d.upserts)+len(d.created) != 0 {
		t.Errorf("handler acted on a check_suite event")
	}
}

func TestBeadIDsInBody(t *testing.T) {
	got := beadIDsInBody("Fixes gd-BRp3is6Dpd, ka-0000001299; not re-evaluating or pre-commit or gd-ABCDEFGHIJ.")
	want := []string{"gd-BRp3is6Dpd", "ka-0000001299"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStatusLine_EscapesSlackControlCharacters(t *testing.T) {
	var pe pullRequestEvent
	pe.Action = "opened"
	pe.PullRequest.Number = 7
	pe.PullRequest.HTMLURL = "https://github.com/o/r/pull/7"
	pe.PullRequest.Title = "fix <b>&</b> a|b"
	got := statusLine(&pe)
	if strings.Contains(got, "<b>") || strings.Contains(got, "a|b") {
		t.Errorf("unescaped control characters in %q", got)
	}
}

// A signed delivery travels the whole path: signature check, replay claim,
// router, handler, daemon. A redelivery of the same GUID is a duplicate and
// writes nothing more.
func TestWebhook_SignedPullRequestReachesBeadsOnce(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	r := fanoutSink{logSink{logger: quiet()}, newPRHandler(d, quiet())}
	h := newWebhookHandler(webhookConfig{
		Secret: testSecret, Sink: r, Logger: quiet(),
		Replay: newReplayCache(3600e9, 10, nil),
	})
	body := fixture(t, "pull_request_closed_merged")
	for i, want := range []int{http.StatusAccepted, http.StatusOK} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerSignature, sign(testSecret, body))
		req.Header.Set(headerDelivery, "e2e-guid")
		req.Header.Set(headerEvent, "pull_request")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("delivery %d = %d, want %d", i, rec.Code, want)
		}
	}
	if len(d.upserts) != 1 || len(d.createdOfType("message")) != 1 {
		t.Errorf("upserts=%d notices=%d, want 1 each", len(d.upserts), len(d.createdOfType("message")))
	}
}

// fakeDrafter records the convert and comment calls draftAgentPR makes.
type fakeDrafter struct {
	converts   []string // repo + " " + node id
	posts      []string
	convertErr error
	marked     bool // a draft notice is already on the PR
}

func (f *fakeDrafter) ConvertToDraft(_ context.Context, repo, nodeID string) error {
	f.converts = append(f.converts, repo+" "+nodeID)
	return f.convertErr
}

func (f *fakeDrafter) HasMarker(_ context.Context, _ prRef, marker string) (bool, error) {
	if marker != draftCommentMarker {
		return false, errors.New("unexpected marker " + marker)
	}
	return f.marked, nil
}

func (f *fakeDrafter) Post(_ context.Context, pr prRef, body string) error {
	f.posts = append(f.posts, pr.Repo+"#"+strconv.Itoa(pr.Number)+" "+body)
	return nil
}

// deliverOpened delivers the pull_request_opened fixture (an agent/ branch onto
// main, opened as ready) with a node id and the given edits to pull_request.
func deliverOpened(t *testing.T, h *prHandler, edit func(pr map[string]any)) error {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(fixture(t, "pull_request_opened"), &payload); err != nil {
		t.Fatal(err)
	}
	pr := payload["pull_request"].(map[string]any)
	pr["node_id"] = "PR_kwDOnode42"
	if edit != nil {
		edit(pr)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{DeliveryID: "d-draft", Type: "pull_request", Payload: raw}
	fillEnvelope(&ev)
	return h.Handle(context.Background(), ev)
}

func draftHandler(d *fakeDaemon, f *fakeDrafter) *prHandler {
	h := newPRHandler(d, quiet())
	h.drafter = f
	return h
}

// Positive control: the one shape the backstop exists for is converted, told
// why, and still mapped onto its bead.
func TestPR_AgentPROpenedReadyIsConvertedToDraft(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	f := &fakeDrafter{}
	if err := deliverOpened(t, draftHandler(d, f), nil); err != nil {
		t.Fatal(err)
	}
	if want := "joyousaversion/gardenio PR_kwDOnode42"; len(f.converts) != 1 || f.converts[0] != want {
		t.Errorf("converts = %v, want exactly [%s]", f.converts, want)
	}
	if len(f.posts) != 1 || !strings.HasPrefix(f.posts[0], "joyousaversion/gardenio#42 ") ||
		!strings.Contains(f.posts[0], "moved to draft") || !strings.Contains(f.posts[0], "`gb pr ready`") ||
		!strings.Contains(f.posts[0], draftCommentMarker) {
		t.Errorf("posts = %q, want one marked draft notice on #42", f.posts)
	}
	if len(d.upserts) != 1 {
		t.Errorf("upserts = %d, want the PR still mapped onto its bead", len(d.upserts))
	}
}

func TestPR_DraftAgentPRsLeavesOtherPRsAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		drafter bool
		edit    func(pr map[string]any)
	}{
		// Flag off: run() wires no drafter (TestNewAgentPRDrafter).
		"flag off":         {drafter: false},
		"already a draft":  {drafter: true, edit: func(pr map[string]any) { pr["draft"] = true }},
		"non-agent branch": {drafter: true, edit: func(pr map[string]any) { pr["head"].(map[string]any)["ref"] = "fix/typo" }},
		"base not main":    {drafter: true, edit: func(pr map[string]any) { pr["base"].(map[string]any)["ref"] = "release/2026.10" }},
	} {
		t.Run(name, func(t *testing.T) {
			d := daemonWithTask(map[string]string{})
			f := &fakeDrafter{}
			h := newPRHandler(d, quiet())
			if tc.drafter {
				h.drafter = f
			}
			if err := deliverOpened(t, h, tc.edit); err != nil {
				t.Fatal(err)
			}
			if len(f.converts)+len(f.posts) != 0 {
				t.Errorf("converts=%v posts=%v, want no call", f.converts, f.posts)
			}
		})
	}
}

func TestPR_DraftAgentPRsOnlyOnOpened(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	f := &fakeDrafter{}
	if err := deliver(t, draftHandler(d, f), "pull_request_synchronize", "d-sync"); err != nil {
		t.Fatal(err)
	}
	if len(f.converts) != 0 {
		t.Errorf("converts = %v on synchronize, want none", f.converts)
	}
}

func TestPR_DraftMutationErrorStillHandlesEvent(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	f := &fakeDrafter{convertErr: errors.New("Resource not accessible by integration")}
	if err := deliverOpened(t, draftHandler(d, f), nil); err != nil {
		t.Fatalf("Handle = %v, want a failed conversion swallowed", err)
	}
	if len(f.converts) != 1 || len(f.posts) != 0 {
		t.Errorf("converts=%d posts=%d, want one attempt and no notice for a PR left ready", len(f.converts), len(f.posts))
	}
	if len(d.upserts) != 1 || len(d.createdOfType("message")) != 1 {
		t.Errorf("upserts=%d notices=%d, want the event mapped as usual", len(d.upserts), len(d.createdOfType("message")))
	}
}

func TestPR_DraftNoticeNotRepeatedOnRedelivery(t *testing.T) {
	d := daemonWithTask(map[string]string{})
	f := &fakeDrafter{marked: true}
	if err := deliverOpened(t, draftHandler(d, f), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.converts) != 1 || len(f.posts) != 0 {
		t.Errorf("converts=%d posts=%d, want a convert and no second notice", len(f.converts), len(f.posts))
	}
}

func TestGitHubDrafter_ConvertToDraft(t *testing.T) {
	for name, tc := range map[string]struct {
		resp    string
		wantErr string
	}{
		"converted":     {resp: `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`},
		"errors array":  {resp: `{"errors":[{"message":"Resource not accessible by integration"}]}`, wantErr: "not accessible"},
		"still ready":   {resp: `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":false}}}}`, wantErr: "not a draft"},
		"null mutation": {resp: `{"data":{"convertPullRequestToDraft":null}}`, wantErr: "not a draft"},
	} {
		t.Run(name, func(t *testing.T) {
			var path, auth, query string
			var id any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, auth = r.URL.Path, r.Header.Get("Authorization")
				var req struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				query, id = req.Query, req.Variables["id"]
				_, _ = w.Write([]byte(tc.resp))
			}))
			defer srv.Close()
			c := &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}
			dr := &githubDrafter{githubCommenter: c, gql: &graphQLThreads{c}}
			err := dr.ConvertToDraft(context.Background(), "o/r", "PR_node")
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if path != "/graphql" || auth != "Bearer tok-o/r" || !strings.Contains(query, "convertPullRequestToDraft") || id != "PR_node" {
				t.Errorf("request path=%q auth=%q id=%v query=%q", path, auth, id, query)
			}
		})
	}
}

// A merge stamps the merge commit and time on the MR bead, so a release's
// contents join a first-parent commit to its PR on sha (releases-design.md
// §3.4).
func TestPR_MergedStampsMergeSHAOnMRBead(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	h := newPRHandler(d, quiet())
	if err := deliver(t, h, "pull_request_closed_merged", "d-ms1"); err != nil {
		t.Fatal(err)
	}
	u := d.updates[mrBead]
	if u["merge_sha"] != "0288f47d232f0859097aa85e1920c3f706f81949" || u["merged_at"] != "2026-10-27T18:04:14Z" {
		t.Errorf("MR bead fields = %v, want merge_sha and merged_at from the payload", u)
	}
}

// An unmerged close carries GitHub's test-merge sha, which is not on any
// branch: nothing is stamped.
func TestPR_UnmergedCloseStampsNoMergeSHA(t *testing.T) {
	d := daemonWithTask(map[string]string{"mr_url": ghPRURL})
	h := newPRHandler(d, quiet())
	if err := deliver(t, h, "pull_request_closed_unmerged", "d-ms2"); err != nil {
		t.Fatal(err)
	}
	if u, ok := d.updates[mrBead]; ok {
		t.Errorf("MR bead fields = %v after an unmerged close, want no write", u)
	}
}
