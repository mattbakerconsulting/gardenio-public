package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/sideshow/apns2"
)

// fakePushDaemon is a PushDeviceSource over an in-memory registry. Like
// kbeads, an empty owner list returns every device, so a test that expects
// no push catches a regression to the old fleet-wide fan-out.
type fakePushDaemon struct {
	mu      sync.Mutex
	devices []*beadsapi.Device
	beads   map[string]*beadsapi.BeadDetail
	agents  map[string]*beadsapi.BeadDetail
	listed  [][]string
	deleted []string
	listAll bool // ignore the owner filter, as a daemon that predates it would
}

func (f *fakePushDaemon) ListDevices(_ context.Context, userEmails []string, platform string) ([]*beadsapi.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, append([]string(nil), userEmails...))
	want := map[string]bool{}
	for _, e := range userEmails {
		want[e] = true
	}
	var out []*beadsapi.Device
	for _, d := range f.devices {
		if !f.listAll && len(want) > 0 && !want[d.UserEmail] {
			continue
		}
		if platform != "" && d.Platform != platform {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (f *fakePushDaemon) DeleteDevice(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, token)
	return nil
}

func (f *fakePushDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (f *fakePushDaemon) FindAgentBead(_ context.Context, name string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.agents[name]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

// fakeAPNs records every token pushed to and answers with status.
type fakeAPNs struct {
	mu       sync.Mutex
	tokens   []string
	payloads []string // each push's JSON
	status   int
	reason   string
}

func (f *fakeAPNs) PushWithContext(_ apns2.Context, n *apns2.Notification) (*apns2.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, n.DeviceToken)
	if b, err := json.Marshal(n.Payload); err == nil {
		f.payloads = append(f.payloads, string(b))
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &apns2.Response{StatusCode: status, Reason: f.reason}, nil
}

func (f *fakeAPNs) pushed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.tokens...)
	sort.Strings(out)
	return out
}

func newTestPushNotifier(d *fakePushDaemon, a *fakeAPNs) *PushNotifier {
	return &PushNotifier{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon:   d,
		apns:     a,
		bundleID: "com.gardenio.app",
	}
}

// twoPeople is a registry with the owner's phone and iPad and a teammate's
// phone.
func twoPeople() *fakePushDaemon {
	return &fakePushDaemon{
		devices: []*beadsapi.Device{
			{Token: "owner-phone", Platform: "ios", UserEmail: "owner@example.com"},
			{Token: "owner-ipad", Platform: "ios", UserEmail: "owner@example.com"},
			{Token: "mate-phone", Platform: "ios", UserEmail: "teammate@example.com"},
		},
		beads:  map[string]*beadsapi.BeadDetail{},
		agents: map[string]*beadsapi.BeadDetail{},
	}
}

func TestPushNotifier_DecisionGoesOnlyToItsOperatorsDevices(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	p := newTestPushNotifier(d, a)

	err := p.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-dec1", Type: "decision", Title: "Pick one",
		Fields: map[string]string{"operated_by": "oidc:Teammate@Example.com"},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	if got := a.pushed(); strings.Join(got, ",") != "mate-phone" {
		t.Fatalf("pushed to %v, want only the teammate's phone", got)
	}
	if len(d.listed) != 1 || strings.Join(d.listed[0], ",") != "teammate@example.com" {
		t.Fatalf("ListDevices owners = %v", d.listed)
	}
}

func TestPushNotifier_FallsBackToRequestingAgentsOperator(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	d.beads["gd-dec2"] = &beadsapi.BeadDetail{ID: "gd-dec2"}
	d.agents["gardenio-crew-x"] = &beadsapi.BeadDetail{
		ID: "gd-agent", Fields: map[string]string{"operated_by": "oidc:owner@example.com"},
	}
	p := newTestPushNotifier(d, a)

	if err := p.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-dec2", Type: "decision", Assignee: "gardenio/crew/gardenio-crew-x",
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	if got := a.pushed(); strings.Join(got, ",") != "owner-ipad,owner-phone" {
		t.Fatalf("pushed to %v, want both of the owner's devices", got)
	}
}

func TestPushNotifier_EscalationUsesRowOperatedBy(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	// The SSE payload drops operated_by; the row has it, resolved to an email
	// even for a Slack-keyed operator.
	d.beads["gd-esc1"] = &beadsapi.BeadDetail{ID: "gd-esc1", OperatedBy: "slack:U0TEAMMATE", OperatedByEmail: "teammate@example.com"}
	p := newTestPushNotifier(d, a)

	if err := p.NotifyEscalation(context.Background(), BeadEvent{ID: "gd-esc1", Type: "escalation"}); err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	if got := a.pushed(); strings.Join(got, ",") != "mate-phone" {
		t.Fatalf("pushed to %v", got)
	}
}

func TestPushNotifier_EscalationFallsBackToRequestingAgentBeadID(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	d.beads["gd-esc2"] = &beadsapi.BeadDetail{ID: "gd-esc2"}
	d.beads["gd-agent2"] = &beadsapi.BeadDetail{ID: "gd-agent2", OperatedBy: "oidc:teammate@example.com"}
	p := newTestPushNotifier(d, a)

	if err := p.NotifyEscalation(context.Background(), BeadEvent{
		ID: "gd-esc2", Type: "escalation", Fields: map[string]string{"requesting_agent": "gd-agent2"},
	}); err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	if got := a.pushed(); strings.Join(got, ",") != "mate-phone" {
		t.Fatalf("pushed to %v", got)
	}
}

// TestPushNotifier_NoAddresseeSendsNothing pins the fix: before 4.7b an alert
// with nobody addressed went to every registered device.
func TestPushNotifier_NoAddresseeSendsNothing(t *testing.T) {
	cases := map[string]struct {
		bead BeadEvent
		row  *beadsapi.BeadDetail
	}{
		"nobody at all": {bead: BeadEvent{ID: "gd-x", Type: "decision"}},
		"non-human operated_by": {
			bead: BeadEvent{ID: "gd-x", Type: "escalation"},
			row:  &beadsapi.BeadDetail{ID: "gd-x", OperatedBy: "fleet:bridge:jira"},
		},
		"unresolvable slack operator": {
			bead: BeadEvent{ID: "gd-x", Type: "decision", Fields: map[string]string{"operated_by": "slack:U0NOBODY"}},
		},
		"agent without an operator": {
			bead: BeadEvent{ID: "gd-x", Type: "decision", Assignee: "lonely-agent"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, a := twoPeople(), &fakeAPNs{}
			if tc.row != nil {
				d.beads[tc.row.ID] = tc.row
			}
			d.agents["lonely-agent"] = &beadsapi.BeadDetail{ID: "gd-lonely"}
			p := newTestPushNotifier(d, a)
			if err := p.NotifyDecision(context.Background(), tc.bead); err != nil {
				t.Fatalf("NotifyDecision: %v", err)
			}
			if got := a.pushed(); len(got) != 0 {
				t.Fatalf("pushed to %v, want nobody", got)
			}
			if len(d.listed) != 0 {
				t.Fatalf("ListDevices called with %v; an empty owner list means every device", d.listed)
			}
		})
	}
}

// TestPushNotifier_GiftedDecisionIgnoresRowEmail: a decision gifted to a
// Slack-keyed human must not borrow the row's email for a different operator.
func TestPushNotifier_GiftedDecisionIgnoresRowEmail(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	d.beads["gd-gift"] = &beadsapi.BeadDetail{ID: "gd-gift", OperatedBy: "oidc:owner@example.com", OperatedByEmail: "owner@example.com"}
	p := newTestPushNotifier(d, a)

	if err := p.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-gift", Type: "decision", Fields: map[string]string{"operated_by": "slack:U0SOMEONE"},
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	if got := a.pushed(); len(got) != 0 {
		t.Fatalf("pushed to %v, want nobody (the addressee has no registered email)", got)
	}
}

// TestPushNotifier_IgnoresUnfilteredDaemonRows: even a daemon that returns
// every device must not turn an addressed alert into a broadcast.
func TestPushNotifier_IgnoresUnfilteredDaemonRows(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	d.listAll = true
	p := newTestPushNotifier(d, a)

	if err := p.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-dec3", Type: "decision", Fields: map[string]string{"operated_by": "oidc:teammate@example.com"},
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	if got := a.pushed(); strings.Join(got, ",") != "mate-phone" {
		t.Fatalf("pushed to %v", got)
	}
}

func TestPushNotifier_PrunesGoneToken(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{status: http.StatusGone, reason: "Unregistered"}
	p := newTestPushNotifier(d, a)

	if err := p.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-dec4", Type: "decision", Fields: map[string]string{"operated_by": "oidc:teammate@example.com"},
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	if strings.Join(d.deleted, ",") != "mate-phone" {
		t.Fatalf("pruned %v, want the teammate's dead token only", d.deleted)
	}
}

func TestPushRecipientEmail(t *testing.T) {
	cases := []struct{ op, email, want string }{
		{"oidc:Owner@Example.com", "", "owner@example.com"},
		{"slack:U0ABC", "Mate@Example.com", "mate@example.com"},
		{"slack:U0ABC", "", ""},
		{"fleet:bridge:jira", "", ""},
		{"oidc:not-an-email", "", ""},
		{"gardenio-crew-x", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := pushRecipientEmail(tc.op, tc.email); got != tc.want {
			t.Errorf("pushRecipientEmail(%q, %q) = %q, want %q", tc.op, tc.email, got, tc.want)
		}
	}
}

// An alert deep-links to the bead's place in the Commons, which the viewer
// resolves to the thread holding its card when the alert is tapped (Commons on
// a phone M6), and its body is the title's first line.
func TestPushNotifier_DeepLinksToTheCommons(t *testing.T) {
	d, a := twoPeople(), &fakeAPNs{}
	p := newTestPushNotifier(d, a)
	p.gardenURL = "https://garden.example/beads"
	ctx := context.Background()
	who := map[string]string{"operated_by": "oidc:owner@example.com"}
	if err := p.NotifyDecision(ctx, BeadEvent{ID: "gd-dec1", Type: "decision", Priority: 2, Title: "Ship the chart fix?\nThe values schema is fixed.", Fields: who}); err != nil {
		t.Fatal(err)
	}
	if err := p.NotifyEscalation(ctx, BeadEvent{ID: "gd-esc1", Type: "escalation", Title: "Merge PR #9\nCI is green", Fields: who}); err != nil {
		t.Fatal(err)
	}
	type alert struct {
		APS struct {
			Alert struct{ Title, Body string } `json:"alert"`
		} `json:"aps"`
		URL    string `json:"url"`
		BeadID string `json:"bead_id"`
	}
	want := map[string]alert{}
	for id, body := range map[string]string{"gd-dec1": "Ship the chart fix?", "gd-esc1": "Merge PR #9"} {
		var w alert
		w.APS.Alert.Body, w.URL, w.BeadID = body, "https://garden.example/beads/garden/c/bead/"+id, id
		want[id] = w
	}
	if len(a.payloads) != 4 { // the owner's phone and iPad, twice
		t.Fatalf("payloads = %v", a.payloads)
	}
	for _, raw := range a.payloads {
		var got alert
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		w := want[got.BeadID]
		if got.URL != w.URL || got.APS.Alert.Body != w.APS.Alert.Body {
			t.Errorf("payload %s: url %q body %q, want %q %q", raw, got.URL, got.APS.Alert.Body, w.URL, w.APS.Alert.Body)
		}
	}

	// Without the viewer's URL there is no link, and nothing else changes.
	a2 := &fakeAPNs{}
	if err := newTestPushNotifier(twoPeople(), a2).NotifyDecision(ctx, BeadEvent{ID: "gd-dec1", Title: "Ship it?", Fields: who}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range a2.payloads {
		if strings.Contains(raw, `"url"`) {
			t.Errorf("payload without a garden URL has a link: %s", raw)
		}
	}
}
