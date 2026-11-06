package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var t0 = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)

type comment struct{ bead, text string }

type fakeDaemon struct {
	beads    []*beadsapi.BeadDetail
	comments []comment
	keys     map[string]string
	listErr  error
	// failTypes fails a list of any of these bead types.
	failTypes map[string]bool
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	for _, typ := range q.Types {
		if f.failTypes[typ] {
			return nil, errors.New(typ + " list failed")
		}
	}
	var match []*beadsapi.BeadDetail
	for _, b := range f.beads {
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
		if ok {
			match = append(match, b)
		}
	}
	if q.Offset >= len(match) {
		match = nil
	} else {
		match = match[q.Offset:]
	}
	return &beadsapi.ListBeadsResult{Beads: match, Total: len(match)}, nil
}

func (f *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	for _, b := range f.beads {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.keys == nil {
		f.keys = map[string]string{}
	}
	if id, ok := f.keys[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		return id, nil
	}
	fields := map[string]string{}
	_ = json.Unmarshal(req.Fields, &fields)
	b := f.add(req.Type, req.Title, req.Status, req.Labels, fields)
	b.CreatedBy = beadsapi.ActorFromContext(ctx) // the static token's X-Actor
	if req.IdempotencyKey != "" {
		f.keys[req.IdempotencyKey] = b.ID
	}
	return b.ID, nil
}

func (f *fakeDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	b, err := f.GetBead(context.Background(), id)
	if err != nil {
		return err
	}
	for k, v := range fields {
		b.Fields[k] = v
	}
	return nil
}

func (f *fakeDaemon) AddComment(_ context.Context, id, _, text string) error {
	f.comments = append(f.comments, comment{id, text})
	return nil
}

func (f *fakeDaemon) add(typ, title, status string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: fmt.Sprintf("kd-%03d", len(f.beads)+1), Type: typ, Title: title, Status: status, Labels: labels, Fields: fields,
	}
	f.beads = append(f.beads, b)
	return b
}

// notices are the message beads posted in a land.
// origins is the origin field of each message bead, in post order.
func (f *fakeDaemon) origins() []string {
	var out []string
	for _, b := range f.beads {
		if b.Type == "message" {
			out = append(out, b.Fields["origin"])
		}
	}
	return out
}

func (f *fakeDaemon) notices() []string {
	var out []string
	for _, b := range f.beads {
		if b.Type == "message" {
			out = append(out, b.Fields["text"])
		}
	}
	return out
}

// fakePods is the agent pods by namespace.
type fakePods struct {
	pods map[string][]corev1.Pod
	err  error
}

func (p *fakePods) ListAgentPods(_ context.Context, namespace string, _ map[string]string) ([]corev1.Pod, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.pods[namespace], nil
}

// run gives agent bead a a running pod in land's namespace, as the controller
// creates it (named and annotated by the controller, not the bead).
func (p *fakePods) run(land string, a *beadsapi.BeadDetail) *corev1.Pod {
	if p.pods == nil {
		p.pods = map[string][]corev1.Pod{}
	}
	ns := "land-" + land
	p.pods[ns] = append(p.pods[ns], corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crew-" + land + "-" + a.ID, Namespace: ns,
			Annotations: map[string]string{podmanager.AnnotationBeadID: a.ID},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})
	return &p.pods[ns][len(p.pods[ns])-1]
}

type nudge struct{ agent, text string }

type fakeNudger struct{ sent []nudge }

func (n *fakeNudger) Nudge(_ context.Context, agent, text string) error {
	n.sent = append(n.sent, nudge{agent, text})
	return nil
}

type fakeSteward struct {
	woken, parked []string
	// parkErr fails PurseEmpty.
	parkErr error
}

func (s *fakeSteward) PurseRefilled(_ context.Context, land string, _ purse.Purse) error {
	s.woken = append(s.woken, land)
	return nil
}

func (s *fakeSteward) PurseEmpty(_ context.Context, land string, _ purse.Purse) error {
	if s.parkErr != nil {
		return s.parkErr
	}
	s.parked = append(s.parked, land)
	return nil
}

// farm is tenant land "tomato-patch" granted 100 credits at x1.5, synced a
// minute before the clock, with #general seeded and one working agent; the
// operator land "gardenio" has a working agent too. Each agent has a running
// pod. spend sets the account's total_cost_usd.
type farm struct {
	d       *fakeDaemon
	pods    *fakePods
	land    *beadsapi.BeadDetail
	acct    *beadsapi.BeadDetail
	agent   *beadsapi.BeadDetail
	opAgent *beadsapi.BeadDetail
	nudger  *fakeNudger
	steward *fakeSteward
	mail    *mailer.Recorder
	clock   time.Time
	w       *Watcher
}

func newFarm(t *testing.T) *farm {
	t.Helper()
	d := &fakeDaemon{}
	app := d.add("plot_application", "tomato-patch", "open", []string{"project:lands-intake"}, map[string]string{
		"applicant_email": "ann@example.com", "github_login": "ann",
	})
	land := d.add("project", "tomato-patch", "open", nil, map[string]string{
		"land_kind": "tenant", "account": "lands-tomato-patch", "land_application_id": app.ID,
	})
	d.add("project", "gardenio", "open", nil, map[string]string{"account": "fleet"})
	d.add("credit_rate", "v1", "closed", nil, map[string]string{"version": "1", "llm_multiplier": "1.5"})
	d.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "tomato-patch", "amount_credits": "100"})
	acct := d.add("account", "lands-tomato-patch", "open", nil, map[string]string{"total_cost_usd": "0"})
	d.add("channel", "general", "open", []string{"project:tomato-patch"}, map[string]string{"channel_kind": "general", "name": "general"})
	agent := d.add("agent", "crew-1", "open", []string{"project:tomato-patch"}, map[string]string{
		"agent": "crew-1", "project": "tomato-patch", "agent_state": "working",
	})
	opAgent := d.add("agent", "op-1", "open", []string{"project:gardenio"}, map[string]string{
		"agent": "op-1", "project": "gardenio", "agent_state": "working",
	})
	f := &farm{d: d, pods: &fakePods{}, land: land, acct: acct, agent: agent, opAgent: opAgent,
		nudger: &fakeNudger{}, steward: &fakeSteward{}, mail: mailer.NewRecorder(), clock: t0}
	f.pods.run("tomato-patch", agent)
	f.pods.run("gardenio", opAgent)
	f.w = &Watcher{
		Daemon: d, Pods: f.pods, Nudger: f.nudger, Steward: f.steward, Mailer: f.mail,
		Templates: mailer.NewTemplates(mailer.Config{BaseURL: "https://gardenio.example"}),
		Now:       func() time.Time { return f.clock },
	}
	f.spend(0)
	return f
}

// spend sets the land's synced spend in US dollars (credits ÷ 1.5).
func (f *farm) spend(usd float64) {
	f.acct.Fields["total_cost_usd"] = fmt.Sprint(usd)
	f.acct.Fields[purse.FieldSpendSyncedAt] = f.clock.Add(-time.Minute).Format(time.RFC3339)
}

func (f *farm) grant(credits float64) {
	f.d.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "tomato-patch", "amount_credits": fmt.Sprint(credits)})
}

func (f *farm) pass(t *testing.T) {
	t.Helper()
	f.spend(purse.FieldFloat(f.acct.Fields, "total_cost_usd"))
	if err := f.w.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
}

func (f *farm) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func TestPass_FirstSightRecordsTheLedgerSilently(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	if got := f.acct.Fields[FieldGrantedSeen]; got != "100" {
		t.Fatalf("purse_granted_seen = %q, want 100", got)
	}
	if n := f.d.notices(); len(n) != 0 {
		t.Fatalf("a fresh land with credit must post nothing, posted %q", n)
	}
	if len(f.steward.woken) != 0 || len(f.nudger.sent) != 0 || len(f.mail.Sent()) != 0 {
		t.Fatalf("first sight must not wake, nudge or mail: steward %v nudges %v mail %d", f.steward.woken, f.nudger.sent, len(f.mail.Sent()))
	}
}

func TestPass_ThresholdNoticesOncePerLevel(t *testing.T) {
	f := newFarm(t)
	f.pass(t)

	f.spend(40) // 60 credits = 60%
	f.pass(t)
	f.pass(t)
	if n := f.d.notices(); len(n) != 1 || !strings.Contains(n[0], "half spent") || !strings.Contains(n[0], "40 of 100") {
		t.Fatalf("at 60%% want one half-spent line, got %q", n)
	}
	if len(f.mail.Sent()) != 0 {
		t.Fatal("no mail at 50%")
	}
	// The daemon takes a message's author only from its writer, or a bridge
	// or machine (kbeads message_author.go): on the static token the notice's
	// X-Actor is the agent its author names (gd-PjNjRhAHPn).
	for _, b := range f.d.beads {
		if b.Type == "message" && (b.Fields["author"] != Author || "agent:"+b.CreatedBy != Author) {
			t.Fatalf("notice author %q written as %q, want %s written as its agent", b.Fields["author"], b.CreatedBy, Author)
		}
	}

	f.spend(56) // 84 credits
	f.pass(t)
	f.pass(t)
	n := f.d.notices()
	if len(n) != 2 || !strings.Contains(n[1], "running low") || !strings.Contains(n[1], "16 of 100") {
		t.Fatalf("at 84%% want a running-low line, got %q", n)
	}
	low := f.mail.SentKind(mailer.KindPurseLow)
	if len(low) != 1 || low[0].To[0] != "ann@example.com" || !strings.Contains(low[0].Text, "$16 left of $100") {
		t.Fatalf("want one purse-low mail to the owner, got %+v", low)
	}
	if got := f.acct.Fields[FieldNoticeLevel]; got != "80" {
		t.Fatalf("purse_notice_level = %q, want 80", got)
	}
}

func TestPass_WallPostsEmptyMailsAndNudges(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(64) // 96 credits: 4 left, margin 5 → wall
	f.pass(t)

	n := f.d.notices()
	if len(n) != 1 || !strings.Contains(n[0], "is empty") {
		t.Fatalf("a jump to the wall says only the empty line, got %q", n)
	}
	if len(f.mail.SentKind(mailer.KindPurseEmpty)) != 1 || len(f.mail.SentKind(mailer.KindPurseLow)) != 0 {
		t.Fatalf("a jump to the wall mails only purse-empty, sent %+v", f.mail.Sent())
	}
	if len(f.nudger.sent) != 1 || f.nudger.sent[0].agent != "crew-1" || !strings.Contains(f.nudger.sent[0].text, "Commit and push") {
		t.Fatalf("want the tenant agent nudged once, got %+v", f.nudger.sent)
	}
	if f.agent.Fields[FieldNudgedAt] != t0.Format(time.RFC3339) {
		t.Fatalf("purse_nudged_at = %q", f.agent.Fields[FieldNudgedAt])
	}
	if f.opAgent.Fields[FieldNudgedAt] != "" || f.opAgent.Fields["agent_state"] != "working" {
		t.Fatal("an operator land's agent must never be touched")
	}
}

func TestPass_GraceThenSuspend(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(70)
	f.pass(t) // nudge

	f.advance(4 * time.Minute)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "working" || len(f.nudger.sent) != 1 {
		t.Fatalf("inside the grace: no suspend and no second nudge (state %q, nudges %d)", f.agent.Fields["agent_state"], len(f.nudger.sent))
	}

	f.advance(time.Minute)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "stopping" || f.agent.Fields[FieldParkReason] != ParkReason {
		t.Fatalf("after the grace: want stopping with park_reason=purse_empty, got %v", f.agent.Fields)
	}
	if f.agent.Fields[FieldNudgedAt] != "" {
		t.Fatal("a suspended agent's grace is over: purse_nudged_at is cleared")
	}
	var suspended bool
	for _, c := range f.d.comments {
		suspended = suspended || (c.bead == f.agent.ID && strings.Contains(c.text, "Suspended"))
	}
	if !suspended {
		t.Fatalf("want a suspend comment, got %+v", f.d.comments)
	}

	f.advance(time.Minute)
	f.pass(t)
	if len(f.nudger.sent) != 1 {
		t.Fatal("a stopping agent is not nudged again")
	}
}

func TestPass_RefillPostsWakesAndCallsOffTheSuspend(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(70) // 105 credits: over the wall
	f.pass(t)   // nudge
	f.advance(2 * time.Minute)

	f.grant(100) // 200 granted, 95 left
	f.pass(t)

	n := f.d.notices()
	if len(n) != 2 || !strings.Contains(n[1], "refilled: 100 credits added, 95 of 200 credits left") {
		t.Fatalf("want a refill line, got %q", n)
	}
	if !slices.Equal(f.steward.woken, []string{"tomato-patch"}) {
		t.Fatalf("the steward hook is woken once, got %v", f.steward.woken)
	}
	// The steward trigger tells a threshold from a refill by origin.
	if o := f.d.origins(); !slices.Equal(o, []string{OriginNotice, OriginRefill}) {
		t.Fatalf("post origins = %q, want the wall notice then the refill", o)
	}
	if f.acct.Fields[FieldGrantedSeen] != "200" || f.acct.Fields[FieldNoticeLevel] != "50" {
		t.Fatalf("refill re-arms the levels: seen %q level %q", f.acct.Fields[FieldGrantedSeen], f.acct.Fields[FieldNoticeLevel])
	}
	if f.agent.Fields[FieldNudgedAt] != "" || f.agent.Fields["agent_state"] != "working" {
		t.Fatalf("the nudged agent carries on: %v", f.agent.Fields)
	}
	if last := f.nudger.sent[len(f.nudger.sent)-1]; !strings.Contains(last.text, "Carry on") {
		t.Fatalf("want a carry-on nudge, got %+v", f.nudger.sent)
	}

	f.advance(10 * time.Minute)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "working" || len(f.steward.woken) != 1 || len(f.d.notices()) != 2 {
		t.Fatal("after the refill nothing more happens")
	}

	f.spend(140) // 210 of 200 credits: the wall again, said again
	f.pass(t)
	if n := f.d.notices(); len(n) != 3 || !strings.Contains(n[2], "is empty") {
		t.Fatalf("a re-armed wall is said again, got %q", n)
	}
}

func TestPass_SuspendedAgentsStayParkedAfterARefill(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(70)
	f.pass(t)
	f.advance(Grace)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "stopping" {
		t.Fatal("setup: the agent should be suspended")
	}
	f.grant(100)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "stopping" || f.agent.Fields[FieldParkReason] != ParkReason {
		t.Fatal("nothing resumes on its own: the steward decides")
	}
}

func TestPass_UnknownPurseParksNothing(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.acct.Fields["total_cost_usd"] = "70"
	f.acct.Fields[purse.FieldSpendSyncedAt] = t0.Add(-time.Hour).Format(time.RFC3339)
	if err := f.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.nudger.sent) != 0 || len(f.d.notices()) != 0 || f.acct.Fields[FieldNoticeLevel] != "" {
		t.Fatalf("an unknown purse: running agents continue and nothing is said (nudges %v notices %q)", f.nudger.sent, f.d.notices())
	}
}

func TestPass_UnknownHoldsAPendingSuspend(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(70)
	f.pass(t) // nudge
	f.advance(Grace + time.Minute)
	f.acct.Fields[purse.FieldSpendSyncedAt] = f.clock.Add(-time.Hour).Format(time.RFC3339)
	if err := f.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.agent.Fields["agent_state"] != "working" || f.agent.Fields[FieldNudgedAt] == "" {
		t.Fatalf("unknown neither suspends nor calls off: %v", f.agent.Fields)
	}
}

func TestPass_NoGeneralChannelStillParksAndRecords(t *testing.T) {
	f := newFarm(t)
	for _, b := range f.d.beads {
		if b.Type == "channel" {
			b.Status = "closed"
		}
	}
	f.pass(t)
	f.spend(70)
	f.pass(t)
	if f.acct.Fields[FieldNoticeLevel] != "100" || len(f.nudger.sent) != 1 || len(f.mail.SentKind(mailer.KindPurseEmpty)) != 1 {
		t.Fatalf("a land with no #general still records, nudges and mails: level %q", f.acct.Fields[FieldNoticeLevel])
	}
	if len(f.d.notices()) != 0 {
		t.Fatal("nothing to post in")
	}
}

func TestPass_MailOffSendsNothing(t *testing.T) {
	f := newFarm(t)
	f.w.Mailer = mailer.Disabled{}
	f.pass(t)
	f.spend(70)
	f.pass(t)
	if len(f.d.notices()) != 1 {
		t.Fatal("notices still post with mail off")
	}
}

func TestPass_NoTenantLandsReadsLittle(t *testing.T) {
	d := &fakeDaemon{}
	d.add("project", "gardenio", "open", nil, map[string]string{})
	w := &Watcher{Daemon: d, Now: func() time.Time { return t0 }}
	if err := w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPass_ReadErrorIsReturned(t *testing.T) {
	d := &fakeDaemon{listErr: errors.New("daemon down")}
	w := &Watcher{Daemon: d}
	if err := w.Pass(context.Background()); err == nil {
		t.Fatal("want the read error")
	}
}

func TestPass_IdempotentPostsSurviveALostStateWrite(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(40)
	f.pass(t)
	delete(f.acct.Fields, FieldNoticeLevel) // the bookkeeping write was lost
	f.pass(t)
	if n := f.d.notices(); len(n) != 1 {
		t.Fatalf("the idempotency key keeps one line, got %q", n)
	}
}

func TestLevel(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		p    purse.Purse
		want int
	}{
		{purse.Purse{State: purse.StateOK}, LevelNone},
		{purse.Purse{State: purse.StateOK, UsedPct: pct(49.99)}, LevelNone},
		{purse.Purse{State: purse.StateOK, UsedPct: pct(50)}, LevelHalf},
		{purse.Purse{State: purse.StateOK, UsedPct: pct(80)}, LevelLow},
		{purse.Purse{State: purse.StateOK, UsedPct: pct(100)}, LevelEmpty},
		{purse.Purse{State: purse.StateWall, UsedPct: pct(95)}, LevelEmpty},
	} {
		if got := Level(c.p); got != c.want {
			t.Errorf("Level(%+v) = %d, want %d", c.p, got, c.want)
		}
	}
}

// A frozen land (step 10.5) parks every running agent on the same path as
// the wall, with park_reason=land_frozen, whatever the purse says; operator
// lands are untouched.
func TestPass_FrozenLandParksEveryAgent(t *testing.T) {
	f := newFarm(t)
	second := f.d.add("agent", "crew-2", "open", []string{"project:tomato-patch"}, map[string]string{
		"agent": "crew-2", "project": "tomato-patch", "agent_state": "idle",
	})
	f.pods.run("tomato-patch", second)
	queued := f.d.add("agent", "crew-3", "open", []string{"project:tomato-patch"}, map[string]string{
		"agent": "crew-3", "project": "tomato-patch", "agent_state": wallState,
	})
	f.pass(t)
	f.land.Fields["land_frozen"] = "true"
	f.pass(t)
	if len(f.nudger.sent) != 2 {
		t.Fatalf("want both running agents nudged, got %+v", f.nudger.sent)
	}
	for _, n := range f.nudger.sent {
		if !strings.Contains(n.text, "frozen") || !strings.Contains(n.text, "Commit and push") {
			t.Errorf("nudge to %s = %q, want the frozen nudge", n.agent, n.text)
		}
	}
	if len(f.d.notices()) != 0 {
		t.Errorf("a freeze with a full purse posts no purse notice, got %q", f.d.notices())
	}

	f.advance(Grace)
	f.pass(t)
	for _, a := range []*beadsapi.BeadDetail{f.agent, second} {
		if a.Fields["agent_state"] != "stopping" || a.Fields[FieldParkReason] != ParkFrozen {
			t.Errorf("%s after the grace: want stopping with park_reason=%s, got %v", a.Fields["agent"], ParkFrozen, a.Fields)
		}
	}
	if queued.Fields["agent_state"] != wallState || queued.Fields[FieldNudgedAt] != "" {
		t.Errorf("an agent not yet started is the spawn wall's, not the watcher's: %v", queued.Fields)
	}
	if f.opAgent.Fields[FieldNudgedAt] != "" || f.opAgent.Fields["agent_state"] != "working" {
		t.Fatal("an operator land's agent must never be touched")
	}

	// A thaw resumes nothing on its own.
	f.land.Fields["land_frozen"] = "false"
	f.pass(t)
	if f.agent.Fields["agent_state"] != "stopping" {
		t.Fatal("a thaw must not resume a suspended agent")
	}
}

// A purse_nudged_at in the future held the grace open for ever, so a frozen
// land's agent that wrote one was never suspended (gd-qpFpWcWucD; the daemon
// now refuses the write, this covers a stamp already stored). The watcher
// reads it as no nudge: it nudges again, and suspends after one grace.
func TestPass_FutureNudgeStampStillSuspends(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.agent.Fields[FieldNudgedAt] = f.clock.Add(100 * 365 * 24 * time.Hour).Format(time.RFC3339)
	f.land.Fields["land_frozen"] = "true"
	f.pass(t)
	if len(f.nudger.sent) != 1 || f.agent.Fields[FieldNudgedAt] != f.clock.Format(time.RFC3339) {
		t.Fatalf("a future stamp: want a fresh nudge stamped now, got nudges %+v, stamp %q", f.nudger.sent, f.agent.Fields[FieldNudgedAt])
	}
	f.advance(Grace)
	f.pass(t)
	if f.agent.Fields["agent_state"] != "stopping" || f.agent.Fields[FieldParkReason] != ParkFrozen {
		t.Fatalf("after the grace: want stopping with park_reason=%s, got %v", ParkFrozen, f.agent.Fields)
	}
}

// wallState is the agent_state the spawn wall leaves a refused bead in.
const wallState = "budget_exceeded"

func TestPass_FrozenParksEvenWhenThePurseIsUnknown(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.land.Fields["land_frozen"] = "true"
	f.acct.Fields[purse.FieldSpendSyncedAt] = t0.Add(-time.Hour).Format(time.RFC3339)
	if err := f.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.nudger.sent) != 1 || f.agent.Fields[FieldNudgedAt] == "" {
		t.Fatalf("an unknown purse must not shield a frozen land: nudges %+v", f.nudger.sent)
	}
}

// The purse and account reads both list the account beads. When they fail,
// a frozen land is parked anyway (the freeze is on the project bead), a land
// that is not frozen is left alone, and the pass still reports the failure.
func TestPass_FrozenParksWhenThePurseAndAccountReadsFail(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.d.failTypes = map[string]bool{"account": true}
	if err := f.w.Pass(context.Background()); err == nil {
		t.Fatal("want the failed reads reported")
	}
	if len(f.nudger.sent) != 0 || f.agent.Fields[FieldNudgedAt] != "" {
		t.Fatalf("a failed read parks nothing on a land that is not frozen: nudges %+v", f.nudger.sent)
	}
	f.land.Fields["land_frozen"] = "true"
	if err := f.w.Pass(context.Background()); err == nil {
		t.Fatal("want the failed reads reported")
	}
	if len(f.nudger.sent) != 1 || f.agent.Fields[FieldNudgedAt] == "" {
		t.Fatalf("a failed purse read must not shield a frozen land: nudges %+v", f.nudger.sent)
	}
	f.advance(Grace)
	_ = f.w.Pass(context.Background())
	if f.agent.Fields["agent_state"] != "stopping" || f.agent.Fields[FieldParkReason] != ParkFrozen {
		t.Errorf("after the grace: want stopping with park_reason=%s, got %v", ParkFrozen, f.agent.Fields)
	}
}

func TestPass_ThawInsideTheGraceCallsOffTheSuspend(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.land.Fields["land_frozen"] = "true"
	f.pass(t) // nudge
	f.advance(2 * time.Minute)
	delete(f.land.Fields, "land_frozen")
	f.pass(t)
	if f.agent.Fields[FieldNudgedAt] != "" || f.agent.Fields["agent_state"] != "working" {
		t.Fatalf("a thaw inside the grace calls the suspend off: %v", f.agent.Fields)
	}
	if last := f.nudger.sent[len(f.nudger.sent)-1]; !strings.Contains(last.text, "Carry on") {
		t.Fatalf("want a carry-on nudge, got %+v", f.nudger.sent)
	}
	// Frozen at the wall: the freeze is the reason given.
	f.land.Fields["land_frozen"] = "true"
	f.spend(70)
	f.pass(t)
	f.advance(Grace)
	f.pass(t)
	if f.agent.Fields[FieldParkReason] != ParkFrozen {
		t.Errorf("frozen and at the wall: park_reason = %q, want %s", f.agent.Fields[FieldParkReason], ParkFrozen)
	}
}

type fakeKeys struct {
	blocked map[string]bool
	calls   []string
	err     error
}

func (k *fakeKeys) Block(_ context.Context, key string) error {
	k.calls = append(k.calls, "block "+key)
	if k.err != nil {
		return k.err
	}
	k.blocked[key] = true
	return nil
}

func (k *fakeKeys) Unblock(_ context.Context, key string) error {
	k.calls = append(k.calls, "unblock "+key)
	if k.err != nil {
		return k.err
	}
	delete(k.blocked, key)
	return nil
}

// A frozen land's LiteLLM key is blocked once the grace is over, and only
// once; a thaw unblocks it (gd-nHh7M0k3ps). Without it the key spends on
// until the purse wall or its max_budget.
func TestPass_FrozenLandKeyBlockedAfterTheGraceAndUnblockedOnThaw(t *testing.T) {
	f := newFarm(t)
	keys := &fakeKeys{blocked: map[string]bool{}}
	f.w.Keys = keys
	f.acct.Fields["litellm_key"] = "sk-land"
	f.pass(t)
	f.land.Fields["land_frozen"] = "true"
	f.land.Fields["land_frozen_at"] = f.clock.Format(time.RFC3339)
	f.pass(t)
	if len(keys.calls) != 0 {
		t.Fatalf("inside the grace the agents still push: want the key open, got %v", keys.calls)
	}
	f.advance(Grace)
	f.pass(t)
	f.pass(t)
	if !keys.blocked["sk-land"] || len(keys.calls) != 1 || f.acct.Fields[FieldKeyBlocked] != "true" {
		t.Fatalf("after the grace: blocked %v calls %v field %q", keys.blocked, keys.calls, f.acct.Fields[FieldKeyBlocked])
	}

	f.land.Fields["land_frozen"] = "false"
	f.pass(t)
	f.pass(t)
	if keys.blocked["sk-land"] || len(keys.calls) != 2 || f.acct.Fields[FieldKeyBlocked] != "" {
		t.Fatalf("after the thaw: blocked %v calls %v field %q", keys.blocked, keys.calls, f.acct.Fields[FieldKeyBlocked])
	}
}

// A freeze with no land_frozen_at blocks at once; a failed block is retried
// on the next pass; a land never frozen is never blocked.
func TestPass_FrozenLandKeyBlockRetriedAndFailsClosed(t *testing.T) {
	f := newFarm(t)
	keys := &fakeKeys{blocked: map[string]bool{}}
	f.w.Keys = keys
	f.acct.Fields["litellm_key"] = "sk-land"
	f.pass(t)
	if len(keys.calls) != 0 {
		t.Fatalf("a land not frozen: %v", keys.calls)
	}
	f.land.Fields["land_frozen"] = "true"
	keys.err = errors.New("litellm down")
	_ = f.w.Pass(context.Background())
	if f.acct.Fields[FieldKeyBlocked] != "" {
		t.Fatal("a failed block was recorded as done")
	}
	keys.err = nil
	f.pass(t)
	if !keys.blocked["sk-land"] || f.acct.Fields[FieldKeyBlocked] != "true" {
		t.Fatalf("retry: blocked %v field %q", keys.blocked, f.acct.Fields[FieldKeyBlocked])
	}
}

func TestPass_SnapshotFirstStamp(t *testing.T) {
	f := newFarm(t)
	f.spend(20) // 30 credits
	f.pass(t)
	want := map[string]string{
		FieldGrantedCredits: "100", FieldUsedCredits: "30", FieldState: purse.StateOK,
		FieldReadAt: t0.Format(time.RFC3339),
	}
	for k, v := range want {
		if got := f.acct.Fields[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestPass_SnapshotNotRewrittenInsideTheHeartbeat(t *testing.T) {
	f := newFarm(t)
	f.spend(20)
	f.pass(t)
	f.advance(SnapshotHeartbeat - time.Minute)
	f.spend(20.001) // the same at two decimals
	f.pass(t)
	if got := f.acct.Fields[FieldReadAt]; got != t0.Format(time.RFC3339) {
		t.Fatalf("an unchanged purse is not rewritten inside the heartbeat: purse_read_at = %q", got)
	}
}

func TestPass_SnapshotRewrittenOnAChange(t *testing.T) {
	f := newFarm(t)
	f.spend(20)
	f.pass(t)
	f.advance(time.Minute)
	f.spend(30) // 45 credits
	f.pass(t)
	if got, at := f.acct.Fields[FieldUsedCredits], f.acct.Fields[FieldReadAt]; got != "45" || at != f.clock.Format(time.RFC3339) {
		t.Fatalf("a change rewrites the snapshot: used %q read_at %q", got, at)
	}
	f.advance(time.Minute)
	f.spend(64) // the wall
	f.pass(t)
	if got := f.acct.Fields[FieldState]; got != purse.StateWall {
		t.Fatalf("purse_state = %q, want wall", got)
	}
}

func TestPass_SnapshotHeartbeat(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.advance(SnapshotHeartbeat)
	f.pass(t)
	if got := f.acct.Fields[FieldReadAt]; got != f.clock.Format(time.RFC3339) {
		t.Fatalf("after %s an unchanged snapshot is rewritten: purse_read_at = %q", SnapshotHeartbeat, got)
	}
}

func TestPass_SnapshotUnknownPurse(t *testing.T) {
	f := newFarm(t)
	f.spend(20)
	f.pass(t)
	for _, b := range f.d.beads {
		if b.Type == "credit_rate" {
			b.Fields["llm_multiplier"] = "0" // no rate in force: spend cannot be priced
		}
	}
	f.advance(time.Minute)
	f.pass(t)
	if s, used := f.acct.Fields[FieldState], f.acct.Fields[FieldUsedCredits]; s != purse.StateUnknown || used != "" {
		t.Fatalf("an unknown purse stamps state=unknown, used empty: state %q used %q", s, used)
	}
	if got := f.acct.Fields[FieldGrantedCredits]; got != "100" {
		t.Fatalf("purse_granted_credits = %q, want 100", got)
	}
}

func TestPass_StewardParksOncePerWall(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(40) // 60%: not the wall
	f.pass(t)
	if len(f.steward.parked) != 0 || f.acct.Fields[FieldStewardPark] != "" {
		t.Fatalf("the steward runs until the wall: parked %v", f.steward.parked)
	}

	f.spend(70) // the wall
	f.pass(t)
	f.advance(Grace)
	f.pass(t)
	f.pass(t)
	if !slices.Equal(f.steward.parked, []string{"tomato-patch"}) || f.acct.Fields[FieldStewardPark] != ParkReason {
		t.Fatalf("at the wall the steward is parked once: parked %v, %s=%q", f.steward.parked, FieldStewardPark, f.acct.Fields[FieldStewardPark])
	}

	f.grant(100) // 200 granted, 95 left
	f.pass(t)
	if f.acct.Fields[FieldStewardPark] != "" {
		t.Fatalf("a refill unparks the steward: %s=%q", FieldStewardPark, f.acct.Fields[FieldStewardPark])
	}

	f.spend(140) // the wall again
	f.pass(t)
	if len(f.steward.parked) != 2 || f.acct.Fields[FieldStewardPark] != ParkReason {
		t.Fatalf("the next wall parks it again: parked %v", f.steward.parked)
	}
}

func TestPass_StewardParkRetriedUntilSaid(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.steward.parkErr = errors.New("daemon busy")
	f.spend(70)
	f.pass(t)
	if f.acct.Fields[FieldStewardPark] != "" {
		t.Fatal("a failed park line is not recorded as said")
	}
	if f.acct.Fields[FieldNoticeLevel] != "100" || len(f.nudger.sent) != 1 {
		t.Fatal("a failed steward hook does not hold up the notice or the parking")
	}
	f.steward.parkErr = nil
	f.pass(t)
	if !slices.Equal(f.steward.parked, []string{"tomato-patch"}) || f.acct.Fields[FieldStewardPark] != ParkReason {
		t.Fatalf("the next pass says it: parked %v", f.steward.parked)
	}
}

func TestPass_StewardNotParkedOnAnUnknownPurse(t *testing.T) {
	f := newFarm(t)
	f.pass(t)
	f.spend(70)
	f.pass(t)
	f.acct.Fields[purse.FieldSpendSyncedAt] = f.clock.Add(-time.Hour).Format(time.RFC3339)
	f.acct.Fields["total_cost_usd"] = "0"
	if err := f.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.acct.Fields[FieldStewardPark] != ParkReason {
		t.Fatal("an unknown purse neither parks nor unparks the steward")
	}
}

// An agent writes its own bead, so the park is chosen by the pods the
// controller runs, not by what the bead says (gd-Nc7yMpW7U6): an agent with a
// running pod that calls itself paused, spawning or prewarmed, blanks its
// agent field, or drops its project label and field is parked all the same.
func TestPass_ParkFollowsThePodNotTheBead(t *testing.T) {
	for name, dodge := range map[string]func(a *beadsapi.BeadDetail){
		"paused":     func(a *beadsapi.BeadDetail) { a.Fields["agent_state"] = "paused" },
		"spawning":   func(a *beadsapi.BeadDetail) { a.Fields["agent_state"] = "spawning" },
		"prewarmed":  func(a *beadsapi.BeadDetail) { a.Fields["agent_state"] = "prewarmed" },
		"stopping":   func(a *beadsapi.BeadDetail) { a.Fields["agent_state"] = "stopping" },
		"no agent":   func(a *beadsapi.BeadDetail) { a.Fields["agent"] = "" },
		"no project": func(a *beadsapi.BeadDetail) { a.Labels = nil; a.Fields["project"] = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFarm(t)
			f.pass(t)
			dodge(f.agent)
			f.spend(70) // the wall
			f.pass(t)
			if f.agent.Fields[FieldNudgedAt] == "" {
				t.Fatalf("an agent with a running pod is nudged at the wall whatever its bead says: %v", f.agent.Fields)
			}
			f.advance(Grace)
			f.pass(t)
			if f.agent.Fields["agent_state"] != "stopping" || f.agent.Fields[FieldParkReason] != ParkReason {
				t.Fatalf("after the grace: want stopping with park_reason=%s, got %v", ParkReason, f.agent.Fields)
			}
			nudges := len(f.nudger.sent)
			f.advance(time.Minute)
			f.pass(t)
			if len(f.nudger.sent) != nudges || f.agent.Fields[FieldNudgedAt] != "" {
				t.Fatalf("a suspended agent whose pod has not gone yet is not nudged again: %v", f.agent.Fields)
			}
		})
	}
}

// What the land runs is its pods: a bead that says working with no pod, or a
// pod being deleted, is nothing to park, and a land whose pods cannot be
// listed parks nothing and reports it.
func TestPass_ParkReadsTheLandsPods(t *testing.T) {
	f := newFarm(t)
	podless := f.d.add("agent", "crew-2", "open", []string{"project:tomato-patch"}, map[string]string{
		"agent": "crew-2", "project": "tomato-patch", "agent_state": "working",
	})
	leaving := f.d.add("agent", "crew-3", "open", []string{"project:tomato-patch"}, map[string]string{
		"agent": "crew-3", "project": "tomato-patch", "agent_state": "working",
	})
	f.pods.run("tomato-patch", leaving).DeletionTimestamp = &metav1.Time{Time: t0}
	f.pass(t)
	f.spend(70)
	f.pass(t)
	if len(f.nudger.sent) != 1 || f.nudger.sent[0].agent != "crew-1" {
		t.Fatalf("only the agent with a running pod is nudged, got %+v", f.nudger.sent)
	}
	if podless.Fields[FieldNudgedAt] != "" || leaving.Fields[FieldNudgedAt] != "" {
		t.Errorf("no pod, or a pod going away, is nothing to park: %v %v", podless.Fields, leaving.Fields)
	}

	g := newFarm(t)
	g.pass(t)
	g.pods.err = errors.New("apiserver down")
	g.land.Fields["land_frozen"] = "true"
	if err := g.w.land(context.Background(), land{name: "tomato-patch", project: g.land}, g.clock); err == nil {
		t.Fatal("a land whose pods cannot be listed must report it")
	}
	if len(g.nudger.sent) != 0 {
		t.Errorf("nothing is parked on a pod list that failed, got %+v", g.nudger.sent)
	}
	// A land namespace not made yet, or not bound to the controller, holds
	// no pod the controller made.
	g.pods.err = apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("no rolebinding"))
	if err := g.w.land(context.Background(), land{name: "tomato-patch", project: g.land}, g.clock); err != nil {
		t.Fatalf("an unbound land namespace runs nothing: %v", err)
	}
	g.pods.err = nil
	g.w.Pods = nil
	if err := g.w.land(context.Background(), land{name: "tomato-patch", project: g.land}, g.clock); err == nil {
		t.Fatal("a watcher with no pod lister must report it, not park nothing quietly")
	}
}
