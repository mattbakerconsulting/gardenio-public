package main

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// threadFake is a purpose-built, minimal store fake satisfying the three
// narrow thread-store interfaces (threadLister / threadMemberStore /
// threadTimelineStore). It is intentionally separate from the package-wide
// fakeDaemon: these aggregators take narrow interfaces precisely so they can be
// driven by a tiny, legible fixture instead of the big router.
type threadFake struct {
	beads    map[string]*beadsapi.BeadDetail
	revDeps  map[string][]beadsapi.Dependency // reverse edges keyed by bead id
	fwdDeps  map[string][]beadsapi.Dependency // forward edges keyed by bead id (e.g. produces)
	comments map[string][]beadsapi.Comment

	// listErr / depErr / batchErr / commentErr force the corresponding fetch
	// to fail so error propagation can be exercised.
	listErr    error
	depErr     error
	batchErr   error
	commentErr error

	// omitFromBatch simulates the BatchGetBeads-omits-a-member race: an id in
	// this set is withheld from BatchGetBeads results even though it exists in
	// beads (and is still returned by the thread_id field query in
	// ListBeadsFiltered). It exercises the field-snapshot fallback (gap 3) and the
	// synthesized head-created path (gap 2).
	omitFromBatch map[string]bool

	// listQueries records every ListBeadsFiltered query so tests can assert the
	// membership queries carry no status filter (lifecycle-agnostic) and a
	// non-zero limit (no silent 1000-row truncation).
	listQueries []beadsapi.ListBeadsQuery
}

func newThreadFake() *threadFake {
	return &threadFake{
		beads:    map[string]*beadsapi.BeadDetail{},
		revDeps:  map[string][]beadsapi.Dependency{},
		fwdDeps:  map[string][]beadsapi.Dependency{},
		comments: map[string][]beadsapi.Comment{},
	}
}

// ListBeadsFiltered answers exactly the two query shapes the aggregators issue:
//   - a label-scoped head enumeration (Labels contains "thread"), returning
//     every bead carrying ALL requested labels, sorted by -updated_at;
//   - a thread_id field-equality query, returning every bead whose
//     fields.thread_id matches.
func (f *threadFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.listQueries = append(f.listQueries, q)
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*beadsapi.BeadDetail

	if wantID, ok := q.Fields[threadIDField]; ok {
		for _, b := range f.beads {
			if b != nil && b.Fields[threadIDField] == wantID {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}

	// Label-scoped enumeration: bead must carry every requested label.
	for _, b := range f.beads {
		if b == nil {
			continue
		}
		if !hasAllLabels(b.Labels, q.Labels) {
			continue
		}
		out = append(out, b)
	}
	if q.Sort == "-updated_at" {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		})
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *threadFake) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	if f.depErr != nil {
		return nil, f.depErr
	}
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		if direction == "reverse" {
			out[id] = f.revDeps[id]
		} else {
			out[id] = f.fwdDeps[id]
		}
	}
	return out, nil
}

func (f *threadFake) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if f.omitFromBatch[id] {
			continue // simulate a daemon split / deletion race dropping this body
		}
		if b, ok := f.beads[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func (f *threadFake) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	if f.commentErr != nil {
		return nil, f.commentErr
	}
	return f.comments[id], nil
}

func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// --- fixture helpers -------------------------------------------------------

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// head builds a thread head bead: carries the bare `thread` label and NO
// thread_id field (it IS the wish).
func (f *threadFake) addHead(id, title, project string, updated time.Time, fields map[string]string) *beadsapi.BeadDetail {
	labels := []string{threadHeadLabel}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	if fields == nil {
		fields = map[string]string{}
	}
	b := &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Status:    "open",
		Labels:    labels,
		Fields:    fields,
		CreatedAt: updated,
		UpdatedAt: updated,
	}
	f.beads[id] = b
	return b
}

// addMemberByField adds a derivative bound to headID purely by the thread_id
// field (no parent-child dep) — a cross-epic wish-pointer member.
func (f *threadFake) addMemberByField(id, title, headID string, created, updated time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Status:    "open",
		Fields:    map[string]string{threadIDField: headID},
		CreatedAt: created,
		UpdatedAt: updated,
	}
	f.beads[id] = b
	return b
}

// hasStatusFilter reports whether any recorded ListBeadsFiltered query carried
// a non-empty Statuses filter — the model-fidelity invariant being that thread
// membership/enumeration queries are lifecycle-agnostic (empty Statuses, which
// the daemon treats as "all statuses").
func (f *threadFake) anyQueryHadStatusFilter() bool {
	for _, q := range f.listQueries {
		if len(q.Statuses) > 0 {
			return true
		}
	}
	return false
}

// allQueriesHaveLimit reports whether every recorded query set a non-zero Limit
// (so the daemon's silent 1000-row default never truncates membership).
func (f *threadFake) allQueriesHaveLimit() bool {
	for _, q := range f.listQueries {
		if q.Limit <= 0 {
			return false
		}
	}
	return len(f.listQueries) > 0
}

// addChild adds a bead as a parent-child child of parentID by registering the
// reverse-dep edge (child depends-on parent). withField controls whether the
// child also carries the thread_id field (a fully-linked member) or not (a
// "half-link": durable dep present, field not yet copied — the 1b-3 window).
func (f *threadFake) addChild(id, title, parentID, headID string, created time.Time, withField bool) *beadsapi.BeadDetail {
	fields := map[string]string{}
	if withField {
		fields[threadIDField] = headID
	}
	b := &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Status:    "open",
		Fields:    fields,
		CreatedAt: created,
		UpdatedAt: created,
	}
	f.beads[id] = b
	f.revDeps[parentID] = append(f.revDeps[parentID], beadsapi.Dependency{
		BeadID:      id,
		DependsOnID: parentID,
		Type:        "parent-child",
	})
	return b
}

// addMRBead registers a type=merge-request bead carrying the MR fields the
// gitlab-bridge projects (mr_url / mr_iid / mr_title / mr_state /
// mr_pipeline_status). It is the target of a `produces` edge — see addProduces.
func (f *threadFake) addMRBead(id string, fields map[string]string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  fields["mr_title"],
		Type:   "merge-request",
		Status: "open",
		Fields: fields,
	}
	f.beads[id] = b
	return b
}

// addProduces registers a forward `produces` dep edge from producer to produced
// (producer --produces--> produced), the edge beadsapi.MRBeadForTask walks and
// the shape threadProducedMRs consumes.
func (f *threadFake) addProduces(producer, produced string) {
	f.fwdDeps[producer] = append(f.fwdDeps[producer], beadsapi.Dependency{
		BeadID:      producer,
		DependsOnID: produced,
		Type:        "produces",
	})
}

// --- threadList tests ------------------------------------------------------

func TestThreadList_Empty(t *testing.T) {
	f := newThreadFake()
	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no threads, got %d: %+v", len(got), got)
	}
}

func TestThreadList_SingleHeadNoMembers(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish one", "gardenio", ts("2026-06-20T10:00:00Z"),
		map[string]string{threadRootTextField: "please make the thing"})

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(got))
	}
	s := got[0]
	if s.ThreadID != "gd-wish1" {
		t.Errorf("ThreadID = %q, want gd-wish1", s.ThreadID)
	}
	if s.WishText != "please make the thing" {
		t.Errorf("WishText = %q, want the slack_root_text value", s.WishText)
	}
	if s.Project != "gardenio" {
		t.Errorf("Project = %q, want gardenio", s.Project)
	}
	if s.MemberCount != 1 {
		t.Errorf("MemberCount = %d, want 1 (head only)", s.MemberCount)
	}
	if !s.LastActivity.Equal(ts("2026-06-20T10:00:00Z")) {
		t.Errorf("LastActivity = %v, want head updated_at", s.LastActivity)
	}
}

func TestThreadList_WishTextFallsBackToTitle(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Title is the wish", "", ts("2026-06-20T10:00:00Z"), nil)
	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if got[0].WishText != "Title is the wish" {
		t.Errorf("WishText = %q, want title fallback", got[0].WishText)
	}
}

func TestThreadList_MemberCountAndLastActivity(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	f.addMemberByField("gd-m1", "member 1", "gd-wish1", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	// Newest activity is on a member, so it must drive LastActivity.
	f.addMemberByField("gd-m2", "member 2", "gd-wish1", ts("2026-06-20T12:00:00Z"), ts("2026-06-21T09:30:00Z"))

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(got))
	}
	if got[0].MemberCount != 3 { // head + 2 field members
		t.Errorf("MemberCount = %d, want 3", got[0].MemberCount)
	}
	if !got[0].LastActivity.Equal(ts("2026-06-21T09:30:00Z")) {
		t.Errorf("LastActivity = %v, want the newest member updated_at", got[0].LastActivity)
	}
}

func TestThreadList_SortedByLastActivityDesc(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-a", "older", "", ts("2026-06-20T10:00:00Z"), nil)
	f.addHead("gd-b", "newer", "", ts("2026-06-22T10:00:00Z"), nil)
	f.addHead("gd-c", "middle", "", ts("2026-06-21T10:00:00Z"), nil)

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	gotOrder := []string{got[0].ThreadID, got[1].ThreadID, got[2].ThreadID}
	want := []string{"gd-b", "gd-c", "gd-a"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotOrder, want)
		}
	}
}

func TestThreadList_ProjectScope(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-g", "gardenio wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	f.addHead("gd-o", "other wish", "other", ts("2026-06-21T10:00:00Z"), nil)

	got, err := threadList(context.Background(), f, "gardenio")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 1 || got[0].ThreadID != "gd-g" {
		t.Fatalf("project scope failed: got %+v", got)
	}
}

func TestThreadList_PropagatesListError(t *testing.T) {
	f := newThreadFake()
	f.listErr = context.DeadlineExceeded
	if _, err := threadList(context.Background(), f, ""); err == nil {
		t.Fatal("expected error to propagate")
	}
}

// Regression: a thread head and field-only members in non-open/closed
// lifecycle statuses (blocked, deferred) must NOT be silently dropped. The
// membership queries are lifecycle-agnostic, so a deferred head still appears
// in the index and a blocked member still counts.
func TestThreadList_IncludesBlockedAndDeferredLifecycleStates(t *testing.T) {
	f := newThreadFake()
	h := f.addHead("gd-wish1", "deferred wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	h.Status = "deferred"
	m := f.addMemberByField("gd-m1", "blocked member", "gd-wish1", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	m.Status = "blocked"

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("deferred head dropped: got %d threads", len(got))
	}
	if got[0].MemberCount != 2 { // head + blocked member
		t.Errorf("MemberCount = %d, want 2 (blocked member must count)", got[0].MemberCount)
	}
	if f.anyQueryHadStatusFilter() {
		t.Errorf("threadList membership queries must be lifecycle-agnostic (no Statuses filter)")
	}
	if !f.allQueriesHaveLimit() {
		t.Errorf("threadList queries must set a non-zero Limit to avoid silent 1000-row truncation")
	}
}

// --- index-card enrichment tests (ka-0000000064) ---------------------------

// human stamps a well-formed slack: human operator id on a fixture bead, plus
// its resolved display/email, so participant/starter avatars resolve as people.
func human(b *beadsapi.BeadDetail, slackID, display, email string) *beadsapi.BeadDetail {
	b.OperatedBy = "slack:" + slackID
	b.OperatedByDisplay = display
	b.OperatedByEmail = email
	return b
}

func TestThreadList_StarterAndParticipants(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"),
		map[string]string{threadRootTextField: "please make the thing"})
	human(head, "UALICE", "Alice Ant", "alice@acme.example")

	// A message member from Bob (a second human participant), newest activity.
	m := f.addMemberByField("gd-m1", "reply", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T12:00:00Z"))
	m.Type = "message"
	m.Fields["text"] = "on it"
	human(m, "UBOB", "Bob Bee", "bob@acme.example")

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 thread, got %d", len(got))
	}
	s := got[0]
	if !s.HasStarter() || s.Starter.Display != "Alice Ant" {
		t.Errorf("Starter = %+v, want Alice Ant", s.Starter)
	}
	if s.Starter.Initials == "" {
		t.Errorf("starter initials empty")
	}
	if len(s.Participants) != 2 {
		t.Fatalf("want 2 participants, got %d: %+v", len(s.Participants), s.Participants)
	}
	// Bob is most-recently-active so he sorts first.
	if s.Participants[0].Display != "Bob Bee" {
		t.Errorf("participant[0] = %q, want Bob Bee (most recent)", s.Participants[0].Display)
	}
	if s.ParticipantOverflow != 0 {
		t.Errorf("ParticipantOverflow = %d, want 0", s.ParticipantOverflow)
	}
}

func TestThreadList_ParticipantsDedupAndCapOverflow(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")

	// Six MORE distinct humans → 7 total, over the cap of 5, so overflow = 2.
	for i := range 6 {
		id := "gd-m" + string(rune('a'+i))
		m := f.addMemberByField(id, "member", "gd-wish",
			ts("2026-06-20T10:00:00Z"), ts("2026-06-20T10:0"+string(rune('1'+i))+":00Z"))
		human(m, "UUSER"+string(rune('A'+i)), "User "+string(rune('A'+i)), "u"+string(rune('a'+i))+"@acme.example")
	}
	// Alice replies AGAIN via a second member — must NOT create a second avatar.
	dup := f.addMemberByField("gd-dup", "alice again", "gd-wish",
		ts("2026-06-20T10:00:00Z"), ts("2026-06-20T13:00:00Z"))
	human(dup, "UALICE", "Alice Ant", "alice@acme.example")

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	s := got[0]
	// 7 distinct humans total (Alice + 6). Cap 5 → 5 shown, 2 overflow.
	if len(s.Participants) != threadParticipantCap {
		t.Errorf("participants shown = %d, want %d (capped)", len(s.Participants), threadParticipantCap)
	}
	if s.ParticipantOverflow != 2 {
		t.Errorf("ParticipantOverflow = %d, want 2 (7 distinct - cap 5)", s.ParticipantOverflow)
	}
	// Alice appears exactly once despite two beads.
	aliceCount := 0
	for _, p := range s.Participants {
		if p.Display == "Alice Ant" {
			aliceCount++
		}
	}
	if aliceCount > 1 {
		t.Errorf("Alice deduped incorrectly, appears %d times", aliceCount)
	}
}

func TestThreadList_SkipsNonHumanParticipants(t *testing.T) {
	f := newThreadFake()
	// Head minted by an agent/system with no human operator → no starter, no avatar.
	f.addHead("gd-wish", "agent-minted wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	m := f.addMemberByField("gd-m1", "agent work", "gd-wish", ts("2026-06-20T10:00:00Z"), ts("2026-06-20T11:00:00Z"))
	m.OperatedBy = "" // agent-created member, no human

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	s := got[0]
	if s.HasStarter() {
		t.Errorf("agent-minted head should have no starter, got %+v", s.Starter)
	}
	if len(s.Participants) != 0 {
		t.Errorf("no human participants expected, got %d", len(s.Participants))
	}
}

func TestThreadList_LastMessage(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")

	// An older and a newer message member; the newer one wins.
	older := f.addMemberByField("gd-msg1", "older", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	older.Type = "message"
	older.Fields["text"] = "first reply"
	human(older, "UALICE", "Alice Ant", "alice@acme.example")

	newer := f.addMemberByField("gd-msg2", "newer", "gd-wish", ts("2026-06-20T12:00:00Z"), ts("2026-06-20T12:00:00Z"))
	newer.Type = "message"
	newer.Fields["text"] = "latest reply"
	human(newer, "UBOB", "Bob Bee", "bob@acme.example")

	// A non-message work member must NOT be treated as a message.
	f.addMemberByField("gd-task", "some task", "gd-wish", ts("2026-06-20T13:00:00Z"), ts("2026-06-20T13:00:00Z"))

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	s := got[0]
	if !s.HasLastMessage() {
		t.Fatalf("expected a last message")
	}
	if s.LastMessage.Text != "latest reply" {
		t.Errorf("LastMessage.Text = %q, want 'latest reply'", s.LastMessage.Text)
	}
	if s.LastMessage.Author != "Bob Bee" {
		t.Errorf("LastMessage.Author = %q, want Bob Bee", s.LastMessage.Author)
	}
}

func TestThreadList_NoLastMessageWhenNoMessages(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")
	f.addMemberByField("gd-task", "some task", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if got[0].HasLastMessage() {
		t.Errorf("no message members → HasLastMessage should be false, got %+v", got[0].LastMessage)
	}
}

func TestThreadList_ProducedMRs(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")
	// A member task that produced an MR.
	f.addMemberByField("gd-task", "task", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	f.addMRBead("gd-mr1", map[string]string{
		"mr_url": "https://gitlab/mr/1", "mr_iid": "1", "mr_title": "the fix", "mr_state": "merged",
	})
	f.addProduces("gd-task", "gd-mr1")

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	s := got[0]
	if !s.HasProducedMRs() || s.ProducedMRCount() != 1 {
		t.Fatalf("want 1 produced MR, got %d", s.ProducedMRCount())
	}
	if s.ProducedMRs[0].URL != "https://gitlab/mr/1" || s.ProducedMRs[0].State != "merged" {
		t.Errorf("MR pill = %+v, want url+merged", s.ProducedMRs[0])
	}
}

func TestThreadList_NoProducedMRsIsClean(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")
	f.addMemberByField("gd-task", "task", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList: %v", err)
	}
	if got[0].HasProducedMRs() {
		t.Errorf("no produces edges → HasProducedMRs should be false")
	}
}

// A produced-MR walk failure must degrade to no pills, never fail the whole
// index — losing the pills must never blank the list.
func TestThreadList_ProducedMRWalkFailureDegrades(t *testing.T) {
	f := newThreadFake()
	head := f.addHead("gd-wish", "wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	human(head, "UALICE", "Alice Ant", "alice@acme.example")
	f.addMemberByField("gd-task", "task", "gd-wish", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	f.depErr = context.DeadlineExceeded // forward-dep walk fails

	got, err := threadList(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadList must not fail on MR walk error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 thread despite MR walk failure, got %d", len(got))
	}
	if got[0].HasProducedMRs() {
		t.Errorf("MR walk failed → expected no pills, got %d", got[0].ProducedMRCount())
	}
}

// --- threadMembers tests (membership UNION) --------------------------------

func TestThreadMembers_EmptyHeadID(t *testing.T) {
	f := newThreadFake()
	got, err := threadMembers(context.Background(), f, "")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty head, got %v", got)
	}
}

func TestThreadMembers_HeadOnly(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T10:00:00Z"), nil)
	got, err := threadMembers(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-wish1" {
		t.Fatalf("expected [gd-wish1], got %v", got)
	}
}

// The core acceptance case: membership = UNION(field, parent-child closure),
// including a "half-link" child (dep present, thread_id field absent) that
// ONLY the closure half can reach, and a field-only member that ONLY the field
// half can reach.
func TestThreadMembers_FieldClosureUnion(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	// Closure half: epic is a parent-child child of the head; task is a child
	// of the epic (2-level closure). The task is a HALF-LINK — durable dep, no
	// thread_id field yet (the 1b-3 write-ordering window).
	f.addChild("gd-epic", "epic under wish", "gd-wish1", "gd-wish1", ts("2026-06-20T10:05:00Z"), true)
	f.addChild("gd-task", "task under epic (half-link)", "gd-epic", "gd-wish1", ts("2026-06-20T10:10:00Z"), false)
	// Field half: a message bound only by the wish pointer, NOT a parent-child
	// descendant of the head.
	f.addMemberByField("gd-msg", "cross-epic message", "gd-wish1", ts("2026-06-20T10:07:00Z"), ts("2026-06-20T10:07:00Z"))

	got, err := threadMembers(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}

	// All four must be present (union), deduplicated.
	wantSet := map[string]bool{"gd-wish1": true, "gd-epic": true, "gd-task": true, "gd-msg": true}
	if len(got) != len(wantSet) {
		t.Fatalf("member count = %d (%v), want %d", len(got), got, len(wantSet))
	}
	for _, id := range got {
		if !wantSet[id] {
			t.Errorf("unexpected member %q", id)
		}
		delete(wantSet, id)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing members: %v", wantSet)
	}

	// dep-first ordering: head first, then closure (BFS, head's children
	// before grandchildren), then field-only members appended last.
	if got[0] != "gd-wish1" {
		t.Errorf("element 0 = %q, want head gd-wish1", got[0])
	}
	posOf := func(id string) int {
		for i, g := range got {
			if g == id {
				return i
			}
		}
		return -1
	}
	if posOf("gd-epic") >= posOf("gd-task") {
		t.Errorf("closure BFS broken: epic (%d) should precede its child task (%d)", posOf("gd-epic"), posOf("gd-task"))
	}
	// The field-only member is appended after the entire closure.
	if posOf("gd-msg") < posOf("gd-task") {
		t.Errorf("field-only member gd-msg (%d) should come after closure member gd-task (%d)", posOf("gd-msg"), posOf("gd-task"))
	}
}

func TestThreadMembers_DedupsFullyLinkedChild(t *testing.T) {
	// A fully-linked child appears in BOTH halves (closure dep AND thread_id
	// field). It must appear exactly once, from the authoritative closure half.
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T10:00:00Z"), nil)
	f.addChild("gd-c1", "fully linked child", "gd-wish1", "gd-wish1", ts("2026-06-20T10:05:00Z"), true)

	got, err := threadMembers(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	count := 0
	for _, id := range got {
		if id == "gd-c1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("gd-c1 appeared %d times, want exactly 1 (dedup): %v", count, got)
	}
}

func TestThreadMembers_PropagatesDepError(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T10:00:00Z"), nil)
	f.depErr = context.DeadlineExceeded
	if _, err := threadMembers(context.Background(), f, "gd-wish1"); err == nil {
		t.Fatal("expected dep error to propagate")
	}
}

// Regression: a blocked/deferred field-only member must be part of the exact
// union, and the field query must be lifecycle-agnostic with a non-zero limit.
func TestThreadMembers_FieldQueryLifecycleAgnosticAndLimited(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T10:00:00Z"), nil)
	m := f.addMemberByField("gd-m1", "deferred field member", "gd-wish1", ts("2026-06-20T11:00:00Z"), ts("2026-06-20T11:00:00Z"))
	m.Status = "deferred"

	got, err := threadMembers(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	if !slices.Contains(got, "gd-m1") {
		t.Fatalf("deferred field-only member dropped from union: %v", got)
	}
	if f.anyQueryHadStatusFilter() {
		t.Errorf("threadMembers field query must be lifecycle-agnostic (no Statuses filter)")
	}
	if !f.allQueriesHaveLimit() {
		t.Errorf("threadMembers field query must set a non-zero Limit")
	}
}

// --- fuseThreadTimeline tests ----------------------------------------------

func TestFuseTimeline_EmptyThread(t *testing.T) {
	f := newThreadFake()
	tl, err := fuseThreadTimeline(context.Background(), f, "")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	if len(tl.Entries) != 0 || len(tl.Members) != 0 {
		t.Fatalf("expected empty timeline, got %+v", tl)
	}
}

func TestFuseTimeline_SingleBeadThread(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T10:00:00Z"), nil)
	f.comments["gd-wish1"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "first comment", CreatedAt: ts("2026-06-20T10:30:00Z")},
	}

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	if len(tl.Members) != 1 || tl.Members[0] != "gd-wish1" {
		t.Fatalf("members = %v, want [gd-wish1]", tl.Members)
	}
	if len(tl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (created + 1 comment)", len(tl.Entries))
	}
	if tl.Entries[0].Kind != threadEntryBeadCreated {
		t.Errorf("entry[0].Kind = %q, want bead_created", tl.Entries[0].Kind)
	}
	if tl.Entries[1].Kind != threadEntryComment || tl.Entries[1].Text != "first comment" {
		t.Errorf("entry[1] = %+v, want the comment", tl.Entries[1])
	}
}

// Multi-bead fused ordering: entries from THREE different member beads must
// interleave strictly by timestamp into one chronological stream.
func TestFuseTimeline_MultiBeadFusedOrdering(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
	// epic child of head; task child of epic.
	f.addChild("gd-epic", "epic", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	f.addChild("gd-task", "task", "gd-epic", "gd-wish1", ts("2026-06-20T10:00:00Z"), true)

	// Comments scattered across all three beads at interleaved times.
	f.comments["gd-wish1"] = []beadsapi.Comment{
		{ID: 10, Author: "alice", Text: "on wish", CreatedAt: ts("2026-06-20T11:00:00Z")},
	}
	f.comments["gd-epic"] = []beadsapi.Comment{
		{ID: 20, Author: "bob", Text: "on epic", CreatedAt: ts("2026-06-20T09:45:00Z")},
	}
	f.comments["gd-task"] = []beadsapi.Comment{
		{ID: 30, Author: "carol", Text: "on task", CreatedAt: ts("2026-06-20T10:30:00Z")},
	}
	// Close the task to emit a bead_closed entry at the latest time.
	f.beads["gd-task"].Status = "closed"
	f.beads["gd-task"].ClosedAt = "2026-06-20T12:00:00Z"
	f.beads["gd-task"].ClosedBy = "carol"

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}

	// Expected fused chronological order by timestamp:
	//   09:00 wish created
	//   09:30 epic created
	//   09:45 comment on epic
	//   10:00 task created
	//   10:30 comment on task
	//   11:00 comment on wish
	//   12:00 task closed
	type want struct {
		at   string
		kind string
		bead string
	}
	expected := []want{
		{"2026-06-20T09:00:00Z", threadEntryBeadCreated, "gd-wish1"},
		{"2026-06-20T09:30:00Z", threadEntryBeadCreated, "gd-epic"},
		{"2026-06-20T09:45:00Z", threadEntryComment, "gd-epic"},
		{"2026-06-20T10:00:00Z", threadEntryBeadCreated, "gd-task"},
		{"2026-06-20T10:30:00Z", threadEntryComment, "gd-task"},
		{"2026-06-20T11:00:00Z", threadEntryComment, "gd-wish1"},
		{"2026-06-20T12:00:00Z", threadEntryBeadClosed, "gd-task"},
	}
	if len(tl.Entries) != len(expected) {
		t.Fatalf("entries = %d, want %d\n%s", len(tl.Entries), len(expected), dumpEntries(tl.Entries))
	}
	for i, e := range expected {
		got := tl.Entries[i]
		if !got.At.Equal(ts(e.at)) || got.Kind != e.kind || got.BeadID != e.bead {
			t.Errorf("entry[%d] = {%s %s %s}, want {%s %s %s}\nfull:\n%s",
				i, got.At.Format(time.RFC3339), got.Kind, got.BeadID,
				e.at, e.kind, e.bead, dumpEntries(tl.Entries))
		}
	}

	// Verify the timeline is globally non-decreasing in time.
	for i := 1; i < len(tl.Entries); i++ {
		if tl.Entries[i].At.Before(tl.Entries[i-1].At) {
			t.Fatalf("timeline not chronological at %d", i)
		}
	}
}

// Same-timestamp entries on different members must tie-break by dep-first
// member position (the 1b-3 ordering principle consumed by the fuser).
func TestFuseTimeline_SameTimestampTieBreaksByDepOrder(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.addChild("gd-epic", "epic", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	f.addChild("gd-task", "task", "gd-epic", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)

	// epic and task were created at the SAME instant. dep-first order is
	// head < epic < task, so epic's created entry must precede task's.
	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	posByBead := map[string]int{}
	for i, e := range tl.Entries {
		if e.Kind == threadEntryBeadCreated {
			posByBead[e.BeadID] = i
		}
	}
	if posByBead["gd-epic"] >= posByBead["gd-task"] {
		t.Fatalf("same-timestamp tie-break broken: epic@%d should precede task@%d\n%s",
			posByBead["gd-epic"], posByBead["gd-task"], dumpEntries(tl.Entries))
	}
}

// Phase 3 bug fix (ka-0000005790, rubric C5/K3): an empty or unparseable
// ClosedAt must NO LONGER silently drop the bead_closed entry. The close event
// is always emitted; its timestamp is imputed to the bead's created_at and the
// entry is flagged ClosedAtImputed so the audit layer can surface it. This test
// replaces the obsolete TestFuseTimeline_MalformedClosedAtDropsClosedEntry,
// which asserted the now-fixed silent-drop behavior.
func TestFuseTimeline_MalformedClosedAtImputesNotDrops(t *testing.T) {
	for _, badClosedAt := range []string{"not-a-timestamp", ""} {
		f := newThreadFake()
		f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
		f.beads["gd-wish1"].Status = "closed"
		f.beads["gd-wish1"].ClosedAt = badClosedAt
		f.beads["gd-wish1"].ClosedBy = "alice"

		tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
		if err != nil {
			t.Fatalf("fuseThreadTimeline: %v", err)
		}
		var closed *threadTimelineEntry
		for i := range tl.Entries {
			if tl.Entries[i].Kind == threadEntryBeadClosed {
				closed = &tl.Entries[i]
			}
		}
		if closed == nil {
			t.Fatalf("closed_at=%q: bead_closed entry was dropped; it must be emitted (imputed), not dropped", badClosedAt)
		}
		if !closed.ClosedAtImputed {
			t.Errorf("closed_at=%q: bead_closed entry should be flagged ClosedAtImputed", badClosedAt)
		}
		if !closed.At.Equal(ts("2026-06-20T09:00:00Z")) {
			t.Errorf("closed_at=%q: imputed timestamp = %v, want the bead's created_at", badClosedAt, closed.At)
		}
		if closed.Author != "alice" {
			t.Errorf("closed_at=%q: closed entry author = %q, want alice", badClosedAt, closed.Author)
		}
	}
}

// A parseable ClosedAt must still produce a NON-imputed close entry at the
// parsed instant — the imputation path must not bleed into the happy case.
func TestFuseTimeline_ParseableClosedAtNotImputed(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.beads["gd-wish1"].Status = "closed"
	f.beads["gd-wish1"].ClosedAt = "2026-06-20T12:34:56.789Z"
	f.beads["gd-wish1"].ClosedBy = "bob"

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	var closed *threadTimelineEntry
	for i := range tl.Entries {
		if tl.Entries[i].Kind == threadEntryBeadClosed {
			closed = &tl.Entries[i]
		}
	}
	if closed == nil {
		t.Fatal("expected a bead_closed entry for a parseable closed_at")
	}
	if closed.ClosedAtImputed {
		t.Errorf("parseable closed_at must NOT be flagged imputed")
	}
	if !closed.At.Equal(ts("2026-06-20T12:34:56.789Z")) {
		t.Errorf("closed entry At = %v, want the parsed closed_at to the nanosecond", closed.At)
	}
}

// Bug fix (ka-0000005790, rubric C6): a member resolved by membership but whose
// body BatchGetBeads did NOT return must keep its comments — the old code
// `continue`d past the comment fetch and dropped them. The comments appear with
// an empty title (no body to read it from) and stay attributed to the missing
// member, never bleeding a sibling's or the head's title.
func TestFuseTimeline_MissingMemberBodyKeepsComments(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	// gd-ghost is a parent-child member of the head (so membership resolves it)
	// but we delete its body so BatchGetBeads omits it.
	f.addChild("gd-ghost", "ghost title", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	delete(f.beads, "gd-ghost")
	f.comments["gd-ghost"] = []beadsapi.Comment{
		{ID: 7, Author: "carol", Text: "comment on a bodyless member", CreatedAt: ts("2026-06-20T10:00:00Z")},
	}
	// The head carries its own comment, to prove no title bleed across members.
	f.comments["gd-wish1"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "comment on the head", CreatedAt: ts("2026-06-20T09:15:00Z")},
	}

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	// gd-ghost stays a member even with no body.
	if !slices.Contains(tl.Members, "gd-ghost") {
		t.Fatalf("missing-body member dropped from membership: %v", tl.Members)
	}
	var ghostComment *threadTimelineEntry
	for i := range tl.Entries {
		e := &tl.Entries[i]
		if e.Kind == threadEntryComment && e.CommentID == 7 {
			ghostComment = e
		}
		// No bead_created/closed entry should exist for the bodyless member.
		if e.BeadID == "gd-ghost" && e.Kind != threadEntryComment {
			t.Errorf("bodyless member produced a %s entry; only its comments should survive", e.Kind)
		}
	}
	if ghostComment == nil {
		t.Fatal("comment on a bodyless member was dropped; it must be preserved (rubric C6)")
	}
	if ghostComment.BeadID != "gd-ghost" {
		t.Errorf("ghost comment BeadID = %q, want gd-ghost (no re-attribution)", ghostComment.BeadID)
	}
	if ghostComment.BeadTitle != "" {
		t.Errorf("ghost comment title = %q, want empty (no sibling/head title bleed)", ghostComment.BeadTitle)
	}
}

func TestFuseTimeline_PropagatesCommentError(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.commentErr = context.DeadlineExceeded
	if _, err := fuseThreadTimeline(context.Background(), f, "gd-wish1"); err == nil {
		t.Fatal("expected comment error to propagate")
	}
}

func dumpEntries(entries []threadTimelineEntry) string {
	var b strings.Builder
	for i, e := range entries {
		b.WriteString(e.At.Format(time.RFC3339))
		b.WriteString("  ")
		b.WriteString(e.Kind)
		b.WriteString("  ")
		b.WriteString(e.BeadID)
		if e.Text != "" {
			b.WriteString("  \"")
			b.WriteString(e.Text)
			b.WriteString("\"")
		}
		if i < len(entries)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// parseThreadClosedAt unit coverage for the space-separated layout the daemon
// can emit in some code paths.
func TestParseThreadClosedAt(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"2026-06-20T12:00:00Z", true},
		{"2026-06-20T12:00:00.123456Z", true},
		{"2026-06-20 12:00:00", true},
		{"", false},
		{"   ", false},
		{"garbage", false},
	}
	for _, c := range cases {
		_, ok := parseThreadClosedAt(c.in)
		if ok != c.wantOK {
			t.Errorf("parseThreadClosedAt(%q) ok = %v, want %v", c.in, ok, c.wantOK)
		}
	}
}
