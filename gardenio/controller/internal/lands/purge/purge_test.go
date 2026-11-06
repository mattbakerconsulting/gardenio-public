package purge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeDaemon is the daemon as the sweep sees it: a bead list it filters the
// way the daemon does, and the purge endpoint's rules.
type fakeDaemon struct {
	beads    map[string]*beadsapi.BeadDetail
	purges   []beadsapi.LandPurgeRequest
	updates  map[string]map[string]any
	purgeErr error
}

func newFakeDaemon(beads ...*beadsapi.BeadDetail) *fakeDaemon {
	d := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}, updates: map[string]map[string]any{}}
	for _, b := range beads {
		d.beads[b.ID] = b
	}
	return d
}

func (d *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if ok && q.Offset == 0 {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b *beadsapi.BeadDetail) int { return strings.Compare(a.ID, b.ID) })
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (d *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	b := d.beads[id]
	if b == nil {
		return errors.New("no bead " + id)
	}
	for k, v := range fields {
		b.Fields[k] = v.(string)
	}
	if d.updates[id] == nil {
		d.updates[id] = map[string]any{}
	}
	for k, v := range fields {
		d.updates[id][k] = v
	}
	return nil
}

func (d *fakeDaemon) PurgeLand(_ context.Context, req beadsapi.LandPurgeRequest) (*beadsapi.LandPurgeResult, error) {
	if d.purgeErr != nil {
		return nil, d.purgeErr
	}
	d.purges = append(d.purges, req)
	res := &beadsapi.LandPurgeResult{Types: map[string]string{}}
	var sel []*beadsapi.BeadDetail
	if req.ApplicationID != "" {
		if b := d.beads[req.ApplicationID]; b != nil {
			sel = append(sel, b)
		}
	} else {
		for _, b := range d.beads {
			if !slices.Contains(b.Labels, "project:"+req.Land) || b.Type == "project" ||
				(b.Type == "credit_grant" && !req.IncludeRecords) {
				continue
			}
			// A bead that is also another project's stays (KeepShared).
			if slices.ContainsFunc(b.Labels, func(l string) bool {
				return strings.HasPrefix(l, "project:") && l != "project:"+req.Land
			}) {
				continue
			}
			sel = append(sel, b)
		}
	}
	for _, b := range sel {
		res.Purged = append(res.Purged, b.ID)
		res.Types[b.ID] = b.Type
		if !req.DryRun {
			delete(d.beads, b.ID)
		}
	}
	slices.Sort(res.Purged)
	return res, nil
}

type fakeSessions struct {
	prefixes []string
	counted  []string
	err      error
}

func (f *fakeSessions) DeletePrefix(_ context.Context, prefix string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.prefixes = append(f.prefixes, prefix)
	return 2, nil
}

func (f *fakeSessions) CountPrefix(_ context.Context, prefix string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.counted = append(f.counted, prefix)
	return 2, nil
}

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func bead(id, typ, status string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	if fields == nil {
		fields = map[string]string{}
	}
	return &beadsapi.BeadDetail{ID: id, Title: id, Type: typ, Status: status, Labels: labels, Fields: fields}
}

// land is a tenant land "acme" closed closedDaysAgo days before now, with
// two agents (one repointed to an earlier agent of the land, as a respawn
// chain is), a task, a credit grant, an agent it shares with the operator's
// project, and an agent of another land.
func land(closedDaysAgo int, extra map[string]string) []*beadsapi.BeadDetail {
	f := map[string]string{"land_kind": "tenant", "land_closed_at": now.AddDate(0, 0, -closedDaysAgo).Format(time.RFC3339)}
	for k, v := range extra {
		f[k] = v
	}
	p := bead("p-acme", "project", "closed", []string{"project:acme"}, f)
	p.Title = "acme"
	in := []string{"project:acme"}
	return []*beadsapi.BeadDetail{
		p,
		bead("a-1", "agent", "closed", in, nil),
		bead("a-2", "agent", "closed", in, map[string]string{"s3_session_key": "a-1"}),
		bead("t-1", "task", "closed", in, nil),
		bead("g-1", "credit_grant", "closed", in, nil),
		bead("s-1", "agent", "closed", []string{"project:acme", "project:gardenio"}, nil),
		bead("o-1", "agent", "open", []string{"project:other"}, nil),
	}
}

func sweeper(d *fakeDaemon, s Sessions) *Sweeper {
	return &Sweeper{Daemon: d, Sessions: s, SessionPrefix: "coop/sessions/", Now: func() time.Time { return now }}
}

func TestPass_PurgesALandClosedPastRetention(t *testing.T) {
	d := newFakeDaemon(land(31, nil)...)
	sess := &fakeSessions{}
	if err := sweeper(d, sess).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	want := []string{"coop/sessions/a-1/", "coop/sessions/a-2/"}
	if !slices.Equal(sess.prefixes, want) {
		t.Fatalf("session prefixes = %v, want %v", sess.prefixes, want)
	}
	for _, id := range []string{"a-1", "a-2", "t-1"} {
		if d.beads[id] != nil {
			t.Errorf("%s survived", id)
		}
	}
	for _, id := range []string{"p-acme", "g-1", "s-1", "o-1"} {
		if d.beads[id] == nil {
			t.Errorf("%s was purged", id)
		}
	}
	if got := d.beads["p-acme"].Fields["land_purged_at"]; got != now.Format(time.RFC3339) {
		t.Fatalf("land_purged_at = %q", got)
	}
	// Sessions go before the beads they are found through.
	if len(d.purges) != 2 || !d.purges[0].DryRun || d.purges[1].DryRun {
		t.Fatalf("purges = %+v, want a dry run then the purge", d.purges)
	}

	// The next pass has nothing to do until the records period is over.
	d.purges = nil
	if err := sweeper(d, sess).Pass(context.Background()); err != nil || len(d.purges) != 0 {
		t.Fatalf("second pass: purges %+v, err %v", d.purges, err)
	}
}

func TestPass_LeavesALandInsideRetention(t *testing.T) {
	d := newFakeDaemon(land(29, nil)...)
	sess := &fakeSessions{}
	if err := sweeper(d, sess).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.purges) != 0 || len(sess.prefixes) != 0 || d.beads["t-1"] == nil {
		t.Fatalf("a land closed 29 days ago was touched: purges %+v, sessions %v", d.purges, sess.prefixes)
	}

	// The retention is configuration.
	s := sweeper(d, sess)
	s.Retention = Days(7)
	if err := s.Pass(context.Background()); err != nil || d.beads["t-1"] != nil {
		t.Fatalf("with 7-day retention the land should be purged: err %v", err)
	}
}

func TestPass_SkipsWhatIsNotAClosedTenantLand(t *testing.T) {
	for name, mutate := range map[string]func(p *beadsapi.BeadDetail){
		"operator land":     func(p *beadsapi.BeadDetail) { p.Fields["land_kind"] = "operator" },
		"open land":         func(p *beadsapi.BeadDetail) { p.Status = "open" },
		"no land_closed_at": func(p *beadsapi.BeadDetail) { delete(p.Fields, "land_closed_at") },
	} {
		t.Run(name, func(t *testing.T) {
			beads := land(400, nil)
			mutate(beads[0])
			d := newFakeDaemon(beads...)
			if err := sweeper(d, &fakeSessions{}).Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(d.purges) != 0 {
				t.Fatalf("purged: %+v", d.purges)
			}
		})
	}
}

func TestPass_RecordsGoAfterTheRecordsPeriod(t *testing.T) {
	purged := now.AddDate(0, 0, -700).Format(time.RFC3339)
	d := newFakeDaemon(land(729, map[string]string{"land_purged_at": purged})...)
	if err := sweeper(d, &fakeSessions{}).Pass(context.Background()); err != nil || len(d.purges) != 0 {
		t.Fatalf("records purged early: %+v, %v", d.purges, err)
	}

	d = newFakeDaemon(land(731, map[string]string{"land_purged_at": purged})...)
	sess := &fakeSessions{}
	if err := sweeper(d, sess).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.purges) != 1 || !d.purges[0].IncludeRecords || d.beads["g-1"] != nil {
		t.Fatalf("records purge: %+v, g-1 left = %v", d.purges, d.beads["g-1"] != nil)
	}
	if d.beads["p-acme"].Fields["land_records_purged_at"] == "" {
		t.Fatal("land_records_purged_at not stamped")
	}
	if len(sess.prefixes) != 0 {
		t.Fatalf("records purge touched sessions: %v", sess.prefixes)
	}
}

func TestPass_AFailureIsRetriedAndNeverStampsThePurge(t *testing.T) {
	d := newFakeDaemon(land(31, nil)...)
	sess := &fakeSessions{err: errors.New("s3 down")}
	if err := sweeper(d, sess).Pass(context.Background()); err == nil {
		t.Fatal("want the session failure back")
	}
	if d.beads["t-1"] == nil || d.beads["p-acme"].Fields["land_purged_at"] != "" {
		t.Fatal("beads were purged, or the land stamped, after the sessions failed")
	}

	sess.err = nil
	if err := sweeper(d, sess).Pass(context.Background()); err != nil || d.beads["t-1"] != nil {
		t.Fatalf("the retry did not purge: %v", err)
	}

	d = newFakeDaemon(land(31, nil)...)
	d.purgeErr = errors.New("403")
	if err := sweeper(d, &fakeSessions{}).Pass(context.Background()); err == nil || d.beads["p-acme"].Fields["land_purged_at"] != "" {
		t.Fatalf("a refused purge stamped the land: %v", err)
	}
}

// An agent's pod writes its own coop_session_id and s3_session_key, so a
// tenant can point them anywhere. The sweep deletes only by the ids of the
// agent beads the daemon is purging.
func TestPass_DeletesOnlyTheLandsOwnAgentSessions(t *testing.T) {
	for _, key := range []string{"o-1", "s-1", "gd-operator", "../x", "a/b", "*", ""} {
		beads := land(31, nil)
		beads[2].Fields["coop_session_id"] = key
		beads[2].Fields["s3_session_key"] = key
		d := newFakeDaemon(beads...)
		sess := &fakeSessions{}
		if err := sweeper(d, sess).Pass(context.Background()); err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if want := []string{"coop/sessions/a-1/", "coop/sessions/a-2/"}; !slices.Equal(sess.prefixes, want) {
			t.Fatalf("key %q: session prefixes = %v, want %v", key, sess.prefixes, want)
		}
	}
}

// An agent id that is not one path segment is never deleted by, and does
// not hold back the rest of the land.
func TestPass_SkipsAnAgentIDThatIsNotOneSegment(t *testing.T) {
	for _, id := range []string{"../x", "a/b", "*", ".hidden"} {
		beads := append(land(31, nil), bead(id, "agent", "closed", []string{"project:acme"}, nil))
		d := newFakeDaemon(beads...)
		sess := &fakeSessions{}
		if err := sweeper(d, sess).Pass(context.Background()); err != nil {
			t.Fatalf("id %q: %v", id, err)
		}
		if want := []string{"coop/sessions/a-1/", "coop/sessions/a-2/"}; !slices.Equal(sess.prefixes, want) {
			t.Fatalf("id %q: session prefixes = %v, want %v", id, sess.prefixes, want)
		}
		if d.beads["t-1"] != nil || d.beads["p-acme"].Fields["land_purged_at"] == "" {
			t.Fatalf("id %q: the rest of the land was held back", id)
		}
	}
}

func TestPass_DryRunDeletesAndStampsNothing(t *testing.T) {
	intake := []string{"project:lands-intake"}
	old := now.AddDate(0, 0, -91).Format(time.RFC3339)
	purged := now.AddDate(0, 0, -700).Format(time.RFC3339)
	records := land(731, map[string]string{"land_purged_at": purged})
	records[0].ID, records[0].Title = "p-old", "old"
	for _, b := range records[1:] {
		b.ID, b.Labels = "old-"+b.ID, []string{"project:old"}
	}
	d := newFakeDaemon(append(append(land(31, nil), records...),
		bead("pa-old", "plot_application", "closed", intake, map[string]string{"state": "denied", "decided_at": old}))...)
	sess := &fakeSessions{}
	s := sweeper(d, sess)
	s.DryRun = true
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sess.prefixes) != 0 {
		t.Fatalf("a dry run deleted sessions: %v", sess.prefixes)
	}
	if want := []string{"coop/sessions/a-1/", "coop/sessions/a-2/"}; !slices.Equal(sess.counted, want) {
		t.Fatalf("counted = %v, want %v", sess.counted, want)
	}
	if len(d.purges) != 3 {
		t.Fatalf("purges = %+v, want the land, the records and the application", d.purges)
	}
	for _, p := range d.purges {
		if !p.DryRun {
			t.Fatalf("a dry run sent a real purge: %+v", p)
		}
	}
	if len(d.updates) != 0 {
		t.Fatalf("a dry run stamped: %v", d.updates)
	}
	for _, id := range []string{"t-1", "a-1", "old-g-1", "pa-old"} {
		if d.beads[id] == nil {
			t.Errorf("a dry run deleted %s", id)
		}
	}
}

func TestPass_NoSessionStore(t *testing.T) {
	d := newFakeDaemon(land(31, nil)...)
	if err := sweeper(d, nil).Pass(context.Background()); err != nil || d.beads["t-1"] != nil {
		t.Fatalf("without a session store the beads still go: %v", err)
	}
}

func TestPass_Applications(t *testing.T) {
	intake := []string{"project:lands-intake"}
	old := now.AddDate(0, 0, -91).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -89).Format(time.RFC3339)
	withdrawn := bead("pa-w", "plot_application", "closed", intake, map[string]string{"state": "withdrawn"})
	withdrawn.ClosedAt = old
	d := newFakeDaemon(
		bead("pa-old", "plot_application", "closed", intake, map[string]string{"state": "denied", "decided_at": old}),
		bead("pa-new", "plot_application", "closed", intake, map[string]string{"state": "denied", "decided_at": recent}),
		bead("pa-land", "plot_application", "closed", intake, map[string]string{"state": "closed", "decided_at": old}),
		bead("pa-x", "plot_application", "closed", intake, map[string]string{"state": "approved", "decided_at": old}),
		withdrawn,
	)
	if err := sweeper(d, &fakeSessions{}).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range d.purges {
		got = append(got, p.ApplicationID)
	}
	slices.Sort(got)
	if want := []string{"pa-old", "pa-w"}; !slices.Equal(got, want) {
		t.Fatalf("purged applications = %v, want %v", got, want)
	}
}

func TestPass_RecordsShorterThanRetentionIsAnError(t *testing.T) {
	d := newFakeDaemon(land(31, nil)...)
	s := sweeper(d, &fakeSessions{})
	s.Retention, s.RecordsRetention = Days(30), Days(10)
	if err := s.Pass(context.Background()); err == nil || len(d.purges) != 0 {
		t.Fatalf("want a config error and no purge: %v, %+v", err, d.purges)
	}
}
