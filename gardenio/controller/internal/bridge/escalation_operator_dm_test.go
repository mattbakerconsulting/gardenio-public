package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// escalationDMHarness wires an EscalationOperatorDM over the mock daemon and the
// capturing DM sender, reusing the live alias feed fixture from
// cross_operator_notify_test.go (same resolver, same ambiguity traps).
func escalationDMHarness(t *testing.T, beads map[string]*beadsapi.BeadDetail) (*EscalationOperatorDM, *fakeDMSender, *mockDaemon) {
	t.Helper()
	daemon := &mockDaemon{beads: beads, slackAliases: liveAliasFeed()}
	sender := &fakeDMSender{}
	e := NewEscalationOperatorDM(EscalationOperatorDMConfig{
		Daemon: daemon,
		Sender: sender,
		Logger: crossOpTestLogger(),
		LinkBead: func(id, title string) string {
			return "<https://beads.example/" + id + "|" + id + " — " + title + ">"
		},
		ReviewURL: func(id string) string {
			// Mirrors the real Bot.escalationReviewURL shape: the bead deep link
			// "$BEADS_WEB_URL/<id>" that resolves to beads-viewer "GET /{id}" and
			// lands the operator on the approve/reject control (ka-0000005895).
			return "https://beads.example/" + id
		},
	})
	if e == nil {
		t.Fatal("NewEscalationOperatorDM returned nil")
	}
	return e, sender, daemon
}

// mergeEscalation is a realistic gitlab_mr_merge escalation addressed to a human,
// modelled on the shape measured on 2026-08-26 (agent-filed, operated_by set to
// the operator who must approve).
func mergeEscalation(operatedBy string, operatorID *int64) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:           "gd-B07MJ9scWG",
		Type:         "escalation",
		Status:       "open",
		Title:        "Merge !2590: shard study-web:test 4-way",
		OperatedBy:   operatedBy,
		OperatedByID: operatorID,
		CreatedBy:    "merge-queue-sweeper",
		Fields: map[string]string{
			"operation_type":   "gitlab_mr_merge",
			"audience":         "owner", // the filer chose the owner; a desk merge is silent (TestEscalationOperatorDM_AudienceGate)
			"execution_target": "local",
			"requesting_agent": "gd-bglSvGlTNR",
			"context":          "96 gardenio MRs are waiting on one approval each.\nHighest leverage: !2590.",
		},
	}
}

func escalationEvent(d *beadsapi.BeadDetail) BeadEvent {
	fields := make(map[string]string, len(d.Fields))
	for k, v := range d.Fields {
		fields[k] = v
	}
	return BeadEvent{
		ID:        d.ID,
		Type:      "escalation",
		Title:     d.Title,
		Status:    "open",
		CreatedBy: d.CreatedBy,
		Fields:    fields,
	}
}

// TestEscalationOperatorDM_AddressedHumanGetsExactlyOneDM is the primary
// acceptance test for the operator ask: an escalation created with a human in
// operated_by produces EXACTLY ONE DM to that human, naming the bead, the
// operation, and where to approve.
func TestEscalationOperatorDM_AddressedHumanGetsExactlyOneDM(t *testing.T) {
	// Alex: the case a naive resolver misses — his slack alias hangs off
	// operator 364 while the bead carries operated_by_id 370.
	detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
	e, sender, daemon := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})

	ev := escalationEvent(detail)
	e.EscalationCreated(context.Background(), ev)

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want exactly 1 DM, got %d: %+v", len(dms), dms)
	}
	got := dms[0]

	if got.UserID != "U0PERSON003" {
		t.Errorf("DM went to %q, want Alex U0PERSON003", got.UserID)
	}
	if !strings.Contains(got.Text, "<@U0PERSON003>") {
		t.Errorf("message lacks the operator @-mention:\n%s", got.Text)
	}
	// Names WHICH escalation.
	if !strings.Contains(got.Text, "gd-B07MJ9scWG") {
		t.Errorf("message lacks the bead id:\n%s", got.Text)
	}
	// Names WHAT is being asked — an operator with 130 open escalations needs
	// to triage from the DM itself.
	if !strings.Contains(got.Text, "`gitlab_mr_merge`") {
		t.Errorf("message lacks the operation type:\n%s", got.Text)
	}
	// Carries the context body so the DM is actionable without a click.
	if !strings.Contains(got.Text, "96 gardenio MRs") {
		t.Errorf("message lacks the escalation context:\n%s", got.Text)
	}
	// Points at where approval actually happens: the bead deep link, not the
	// dead /ui/escalations.html#<id> shape (ka-0000005895).
	if !strings.Contains(got.Text, "https://beads.example/gd-B07MJ9scWG") {
		t.Errorf("message lacks the web-UI review link:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "ui/escalations.html") {
		t.Errorf("message still carries the dead /ui/escalations.html link shape:\n%s", got.Text)
	}

	// Delivery is stamped on the bead so an agent can read "the operator was
	// pinged personally", not just "a card was posted".
	if v := daemon.beads[detail.ID].Fields["operator_dm_at"]; v == "" {
		t.Error("operator_dm_at was not stamped on the escalation")
	}
	if v := daemon.beads[detail.ID].Fields["operator_dm_channel"]; v != "D0FAKE" {
		t.Errorf("operator_dm_channel = %q, want D0FAKE", v)
	}

	// An SSE reconnect that redelivers beads.bead.created must not re-ping.
	e.EscalationCreated(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("redelivered create event produced %d DMs, want 1", n)
	}
}

// TestEscalationOperatorDM_SilentCases covers every population that must NOT
// generate a DM. This is the noise guard: 171 escalations were created in the 7
// days before this landed, so a wrong "fire" here is 24 spurious DMs a day.
func TestEscalationOperatorDM_SilentCases(t *testing.T) {
	cases := []struct {
		name  string
		mutdt func(*beadsapi.BeadDetail)
		mutev func(*BeadEvent)
		why   string
	}{
		{
			name:  "no operated_by",
			mutdt: func(d *beadsapi.BeadDetail) { d.OperatedBy = ""; d.OperatedByID = nil },
			why:   "nobody is addressed; the channel card is the whole delivery",
		},
		{
			name:  "fleet sentinel operator",
			mutdt: func(d *beadsapi.BeadDetail) { d.OperatedBy = "fleet:bridge:jira"; d.OperatedByID = nil },
			why:   "not a human — IsHumanOperator rejects it",
		},
		{
			name:  "agent-shaped operator",
			mutdt: func(d *beadsapi.BeadDetail) { d.OperatedBy = "agent:gardener-merge-queue"; d.OperatedByID = nil },
			why:   "not a human",
		},
		{
			name:  "already approved at creation",
			mutev: func(ev *BeadEvent) { ev.Fields["approved_by"] = "matthew.baker@acme.example" },
			why:   "a pre-approved escalation is a record, not an ask",
		},
		{
			name:  "operator filed it themselves (oidc actor)",
			mutev: func(ev *BeadEvent) { ev.Actor = "alex.novak@contractor-b.example" },
			why:   "they already know; DMing them about their own filing is noise",
		},
		{
			name: "ambiguous email resolves to no single slack id",
			mutdt: func(d *beadsapi.BeadDetail) {
				// Matt's email carries TWO distinct real ids in the live feed;
				// soleSlackID refuses rather than guess a human.
				d.OperatedBy = "oidc:matthew.baker@acme.example"
				d.OperatedByID = nil
			},
			why: "mentioning the wrong human is worse than not mentioning",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
			if tc.mutdt != nil {
				tc.mutdt(detail)
			}
			e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			ev := escalationEvent(detail)
			if tc.mutev != nil {
				tc.mutev(&ev)
			}
			e.EscalationCreated(context.Background(), ev)
			if dms := sender.all(); len(dms) != 0 {
				t.Errorf("want silence (%s), got %d DMs: %+v", tc.why, len(dms), dms)
			}
		})
	}
}

// TestEscalationOperatorDM_SensitiveWithholdsBody asserts the sensitivity
// carve-out: a sensitive/secret escalation is still DELIVERED (a DM is more
// private than the channel post that already happens), but its body is withheld
// from the text and the DM channel id is withheld from the bead fields, mirroring
// the slack_channel carve-out in NotifyNewEscalation.
func TestEscalationOperatorDM_SensitiveWithholdsBody(t *testing.T) {
	for _, sens := range []string{"sensitive", "secret"} {
		t.Run(sens, func(t *testing.T) {
			detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
			detail.Fields["sensitivity"] = sens
			detail.Fields["context"] = "rotate the prod signing key: current value AKIAsecretvalue"
			e, sender, daemon := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})

			e.EscalationCreated(context.Background(), escalationEvent(detail))

			dms := sender.all()
			if len(dms) != 1 {
				t.Fatalf("want 1 DM (sensitive escalations must still be delivered), got %d", len(dms))
			}
			if strings.Contains(dms[0].Text, "AKIAsecretvalue") {
				t.Errorf("sensitive context body leaked into the DM:\n%s", dms[0].Text)
			}
			// It still tells them there IS something to look at.
			if !strings.Contains(dms[0].Text, "gd-B07MJ9scWG") ||
				!strings.Contains(dms[0].Text, "`"+sens+"`") {
				t.Errorf("DM must still name the bead and its sensitivity:\n%s", dms[0].Text)
			}
			if v := daemon.beads[detail.ID].Fields["operator_dm_channel"]; v != "" {
				t.Errorf("operator_dm_channel = %q, want withheld for %s", v, sens)
			}
			if v := daemon.beads[detail.ID].Fields["operator_dm_at"]; v == "" {
				t.Error("operator_dm_at must still be stamped for a sensitive escalation")
			}
		})
	}
}

// TestEscalationOperatorDM_SendFailureReleasesClaim asserts a transient Slack
// error does not permanently swallow the ping: the dedup claim is handed back so
// a redelivered create event retries.
func TestEscalationOperatorDM_SendFailureReleasesClaim(t *testing.T) {
	detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
	e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	ev := escalationEvent(detail)

	sender.fail = errors.New("slack: rate_limited")
	e.EscalationCreated(context.Background(), ev)
	if n := len(sender.all()); n != 0 {
		t.Fatalf("failing sender recorded %d DMs, want 0", n)
	}

	sender.fail = nil
	e.EscalationCreated(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("retry after transient failure produced %d DMs, want 1", n)
	}
}

// TestEscalationOperatorDM_NilReceiverIsSafe asserts the disabled path: a
// slack-bridge with no Slack app wires a nil notifier, and the watcher calls it
// unconditionally.
func TestEscalationOperatorDM_NilReceiverIsSafe(t *testing.T) {
	var e *EscalationOperatorDM
	e.EscalationCreated(context.Background(), BeadEvent{ID: "gd-X", Type: "escalation"})

	if got := NewEscalationOperatorDM(EscalationOperatorDMConfig{Sender: &fakeDMSender{}}); got != nil {
		t.Error("nil Daemon must disable the notifier")
	}
	if got := NewEscalationOperatorDM(EscalationOperatorDMConfig{Daemon: &mockDaemon{}}); got != nil {
		t.Error("nil Sender must disable the notifier")
	}
}

// TestBuildEscalationOperatorDM_NoReviewURL asserts the degraded render: with no
// absolute web-UI base configured, the DM still says where approval happens
// rather than emitting a relative link Slack would reject.
func TestBuildEscalationOperatorDM_NoReviewURL(t *testing.T) {
	text := buildEscalationOperatorDM(escalationOperatorDMMessage{
		BeadLink:      "gd-X — merge something",
		Mention:       "<@U0PERSON003>",
		OperationType: "gitlab_mr_merge",
	})
	if strings.Contains(text, "<|") || strings.Contains(text, "<>") {
		t.Errorf("empty review URL rendered as a broken Slack link:\n%s", text)
	}
	if !strings.Contains(text, "web UI") {
		t.Errorf("degraded render must still point at the web UI:\n%s", text)
	}
}

// TestEscalationOperatorDM_SlackRootedOperatorIsTheDominantShape pins the shape
// that actually dominates production, which the oidc-addressed fixtures above do
// not exercise. Measured 2026-08-26 over 174 escalations created in 7 days:
// 167 carry a `slack:U…` operated_by (162 of them one operator) and only 7 carry
// an `oidc:` address. `slack:`-rooted values short-circuit Resolve BEFORE the
// email lookup, so the alias ambiguity under matthew.baker@acme.example — which
// silences the oidc form of the same human — does NOT apply here and the DM is
// delivered. That asymmetry is surprising enough to be worth a test rather than
// a comment.
func TestEscalationOperatorDM_SlackRootedOperatorIsTheDominantShape(t *testing.T) {
	t.Run("agent-filed: delivered despite the ambiguous email", func(t *testing.T) {
		detail := mergeEscalation("slack:U0OPERATOR1", nil)
		e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
		e.EscalationCreated(context.Background(), escalationEvent(detail))

		dms := sender.all()
		if len(dms) != 1 {
			t.Fatalf("want 1 DM for a slack:-rooted operator, got %d", len(dms))
		}
		if dms[0].UserID != "U0OPERATOR1" {
			t.Errorf("DM went to %q, want U0OPERATOR1", dms[0].UserID)
		}
	})

	// The 6 human-filed escalations in that window were all filed by the same
	// human they are addressed to, arriving as a bare OIDC email against a
	// `slack:`-rooted operated_by — the two do not string-compare, so this only
	// stays silent because Resolve recovers the operator's email from the alias
	// feed. Verify it, because a regression here DMs a human about their own
	// filing ~1x/day.
	t.Run("self-filed by email: silent against a slack:-rooted operator", func(t *testing.T) {
		detail := mergeEscalation("slack:U0OPERATOR1", nil)
		detail.CreatedBy = "matthew.baker@acme.example"
		e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
		ev := escalationEvent(detail)
		ev.CreatedBy = "matthew.baker@acme.example"
		e.EscalationCreated(context.Background(), ev)

		if dms := sender.all(); len(dms) != 0 {
			t.Errorf("want silence for a self-filed escalation, got %d DMs: %+v", len(dms), dms)
		}
	})
}

// TestEscalationOperatorDM_TruncationIsRuneSafe guards the bug family that has
// twice silently DROPPED escalation notifications: a byte-sliced truncation
// splits a multi-byte rune, Slack rejects the invalid UTF-8, and the whole
// message is lost (ka-0000000920, ka-0000006181 — the reason
// NotifyNewEscalation truncates its context body by runes).
//
// This is not hypothetical for this code path. Escalation context bodies in this
// repo routinely carry em-dashes and arrows: with the byte-based truncateText a
// sweep over one realistic body found 160 of 1351 candidate cut lengths emitting
// invalid UTF-8. Every field this file truncates is swept here, at every length
// around its cap, so a future edit that reaches for truncateText reds.
func TestEscalationOperatorDM_TruncationIsRuneSafe(t *testing.T) {
	// Multi-byte throughout: em-dash, arrow, non-Latin, and an emoji (4 bytes).
	multibyte := []rune(strings.Repeat("blocked — merge !2590 → main 🚨 ключ; ", 60))

	// Inputs are sliced by RUNE — a bead field arrives JSON-decoded and therefore
	// already valid, so feeding deliberately-invalid input would test nothing.
	// What is under test is whether the TRUNCATION inside the builder splits a
	// rune, which it does for any input long enough to hit a cap.
	for n := 1; n <= len(multibyte); n++ {
		in := string(multibyte[:n])
		text := buildEscalationOperatorDM(escalationOperatorDMMessage{
			BeadLink:        "gd-X",
			Mention:         "<@U0OPERATOR1>",
			OperationType:   in,
			ExecutionTarget: in,
			Sensitivity:     in,
			RequestingAgent: in,
			Context:         in,
		})
		if !utf8.ValidString(text) {
			t.Fatalf("byte-split rune at input rune-length %d produced invalid UTF-8 "+
				"— Slack would reject this and the escalation DM would be lost", n)
		}
	}

	// And the context body specifically at its own cap, which is the field most
	// likely to actually exceed a limit in production.
	long := buildEscalationOperatorDM(escalationOperatorDMMessage{
		BeadLink: "gd-X", Mention: "<@U0OPERATOR1>", Context: string(multibyte),
	})
	if !utf8.ValidString(long) {
		t.Error("context truncated at its cap produced invalid UTF-8")
	}
	if !strings.Contains(long, "...") {
		t.Error("an over-cap context body should be visibly truncated")
	}
}

// ---------------------------------------------------------------------------
// gd-So39r4Fo4c: alias feed has no row for the addressed operator
// ---------------------------------------------------------------------------

// The owner's gardenio sign-in email (oidc:matthewbaker1110@gmail.com) has no
// row in the operators slack-alias feed, but their user bead carries
// email + slack_id and their Slack account is linked. The DM must still go out.
func TestEscalationOperatorDM_AliasMissFallsBackToLinkAndUserBead(t *testing.T) {
	const operatedBy = "oidc:matthewbaker1110@gmail.com"
	userBead := BeadEvent{ID: "gd-uNTW7ZgXwq", Title: "Matthew Baker", Fields: map[string]string{
		"slack_id": "U0C9806910D", "email": "matthewbaker1110@gmail.com",
	}}

	cases := []struct {
		name    string
		users   *UserDirectory
		linked  func(string) []string
		wantDM  string // "" = no DM expected
		wantErr bool
	}{
		{name: "user bead", users: seedUserDirectory(t, []BeadEvent{userBead}), wantDM: "U0C9806910D"},
		{name: "account link", linked: func(e string) []string {
			if e == "matthewbaker1110@gmail.com" {
				return []string{"U0LINKED001"}
			}
			return nil
		}, wantDM: "U0LINKED001"},
		{name: "ambiguous user beads", users: seedUserDirectory(t, []BeadEvent{userBead,
			{ID: "gd-dup", Fields: map[string]string{"slack_id": "U0OTHER0001", "email": "MatthewBaker1110@gmail.com"}}}), wantErr: true},
		{name: "ambiguous links", linked: func(string) []string { return []string{"U0AAAAAA01", "U0BBBBBB02"} }, wantErr: true},
		{name: "no source knows them", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := mergeEscalation(operatedBy, nil)
			e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			e.users, e.linked = tc.users, tc.linked

			before := testutil.ToFloat64(escalationOperatorDMUnresolvedTotal)
			e.EscalationCreated(context.Background(), escalationEvent(detail))
			missed := testutil.ToFloat64(escalationOperatorDMUnresolvedTotal) - before

			dms := sender.all()
			if tc.wantErr {
				if len(dms) != 0 {
					t.Fatalf("unresolved operator must not be DMed, got %+v", dms)
				}
				if missed != 1 {
					t.Errorf("unresolved counter moved by %v, want 1", missed)
				}
				return
			}
			if len(dms) != 1 || dms[0].UserID != tc.wantDM {
				t.Fatalf("want one DM to %s, got %+v", tc.wantDM, dms)
			}
			if missed != 0 {
				t.Errorf("unresolved counter moved by %v on a delivered DM", missed)
			}
		})
	}
}

func TestSlackLinker_SlackIDsForEmail(t *testing.T) {
	f := newLinkFixture(t, nil)
	for _, l := range []*SlackLink{
		{SlackUserID: "U0AAAAAA01", Email: "Owner@Example.com"},
		{SlackUserID: "U0BBBBBB02", Email: "other@example.com"},
	} {
		if err := f.store.Put(l); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.linker.SlackIDsForEmail("owner@example.com"); len(got) != 1 || got[0] != "U0AAAAAA01" {
		t.Errorf("SlackIDsForEmail = %v, want [U0AAAAAA01]", got)
	}
	if got := f.linker.SlackIDsForEmail(""); got != nil {
		t.Errorf("blank email must match nothing, got %v", got)
	}
	if got := f.linker.SlackIDsForEmail("nobody@example.com"); len(got) != 0 {
		t.Errorf("unknown email matched %v", got)
	}
}

// TestEscalationOperatorDM_FollowsHeldBy: #756's audience gate decides
// whether; held_by decides who (D9). Fixtures carry the audience Task 2
// stamps for each held_by (D11). The operator-held and unstamped-owner cases
// are the positive controls.
func TestEscalationOperatorDM_FollowsHeldBy(t *testing.T) {
	for _, c := range []struct {
		held, audience string
		dms            int
	}{
		{"", "owner", 1},
		{"", "", 0}, // unstamped merge: #756's default audience is desk
		{"operator", "owner", 1},
		{"land-owner:acme", "owner", 0}, // its owners are reached in the Commons (Task 12)
		{"coordinator:laptop", "operator", 0},
		{"coordinator:merge-desk", "desk", 0},
		{"steward:acme", "desk", 0},
		{"requester", "operator", 0},
	} {
		detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
		if c.held != "" {
			detail.Fields["held_by"] = c.held
		}
		if c.audience != "" {
			detail.Fields["audience"] = c.audience
		} else {
			delete(detail.Fields, "audience")
		}
		e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
		e.EscalationCreated(context.Background(), escalationEvent(detail))
		if n := len(sender.all()); n != c.dms {
			t.Errorf("held_by=%q audience=%q: %d DMs, want %d", c.held, c.audience, n, c.dms)
		}
	}
}

// TestEscalationOperatorDM_HandUpDMsOnce: a coordinator hands an escalation up;
// the operator gets one DM saying who handed it up and why, and a redelivered
// event does not re-send it.
func TestEscalationOperatorDM_HandUpDMsOnce(t *testing.T) {
	detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
	detail.Fields["held_by"] = "coordinator:laptop"
	detail.Fields["audience"] = "operator"
	e, sender, daemon := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	e.EscalationCreated(context.Background(), escalationEvent(detail))
	if n := len(sender.all()); n != 0 {
		t.Fatalf("coordinator-held create sent %d DMs", n)
	}

	// The daemon's hand-up (Task 5) moves held_by, its audience (D11) and
	// stamps the hand-up fields.
	daemon.beads[detail.ID].Fields["held_by"] = "operator"
	daemon.beads[detail.ID].Fields["audience"] = "owner"
	daemon.beads[detail.ID].Fields["handed_up_by"] = "machine:gardenio-laptop-operator"
	daemon.beads[detail.ID].Fields["handed_up_at"] = "2026-10-20T05:00:00Z"
	daemon.beads[detail.ID].Fields["hand_up_reason"] = "needs the owner's GitHub session"
	ev := escalationEvent(daemon.beads[detail.ID])

	e.EscalationHandedUp(context.Background(), ev)
	dms := sender.all()
	if len(dms) != 1 || dms[0].UserID != "U0PERSON003" {
		t.Fatalf("hand-up DMs = %+v, want one to U0PERSON003", dms)
	}
	for _, want := range []string{"handed this escalation up to you", "needs the owner's GitHub session", "gd-B07MJ9scWG"} {
		if !strings.Contains(dms[0].Text, want) {
			t.Errorf("hand-up DM missing %q:\n%s", want, dms[0].Text)
		}
	}
	e.EscalationHandedUp(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("redelivered hand-up sent %d DMs, want 1", n)
	}
}

// TestEscalationRunsWhere: the approver sees the named runner's hostname and
// owner for a local escalation, and the cluster role otherwise (spec §9).
func TestEscalationRunsWhere(t *testing.T) {
	daemon := &mockDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-r1": {ID: "gd-r1", Type: "runner", Title: "runner gd-r1", Fields: map[string]string{"hostname": "matts-mbp", "owner": "oidc:matt@example.com"}},
	}}
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"named runner", map[string]string{"execution_target": "local", "target_runner": "gd-r1"}, "matts-mbp (matt@example.com)"},
		{"unknown runner keeps its id", map[string]string{"execution_target": "local", "target_runner": "gd-gone", "target_owner": "oidc:a@example.com"}, "gd-gone (a@example.com)"},
		{"owner only", map[string]string{"execution_target": "local", "target_owner": "oidc:a@example.com"}, "a runner of a@example.com"},
		{"local, nothing named", map[string]string{"execution_target": "local"}, "a local runner (none named)"},
		{"service account", map[string]string{"execution_target": "service_account", "spawn_role": "infra"}, "cluster, role infra"},
		{"cluster, no role", map[string]string{"execution_target": "k8s"}, "cluster"},
		{"requester executes", map[string]string{"execution_target": "local", "execute_by": beadsapi.LaneRequester}, "the requesting agent"},
		{"no target", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escalationRunsWhere(context.Background(), daemon, tc.fields); got != tc.want {
				t.Errorf("escalationRunsWhere(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

// The operator DM for a local escalation names the runner it will run on.
func TestEscalationOperatorDM_ShowsWhereItWillRun(t *testing.T) {
	detail := mergeEscalation("slack:U0OPERATOR1", nil)
	detail.Fields["target_runner"] = "gd-r1"
	runner := &beadsapi.BeadDetail{ID: "gd-r1", Type: "runner", Fields: map[string]string{"hostname": "matts-mbp", "owner": "oidc:matt@example.com"}}
	e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail, runner.ID: runner})
	e.EscalationCreated(context.Background(), escalationEvent(detail))

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM, got %d", len(dms))
	}
	if !strings.Contains(dms[0].Text, "*Where it will run:* matts-mbp (matt@example.com)") {
		t.Errorf("DM does not say where the escalation will run:\n%s", dms[0].Text)
	}
}

// TestEscalationOperatorDM_SettledBeforeReadIsSilent: a spawn-policy
// escalation is auto-approved and closed in the request that created it, so
// the create event still reads as an open ask while the bead the bridge reads
// a few seconds later is settled. No DM (gd-Q86qORzVrx, from gd-cSd7Cgo3CA).
func TestEscalationOperatorDM_SettledBeforeReadIsSilent(t *testing.T) {
	cases := []struct {
		name   string
		status string
		fields map[string]string
	}{
		{"approved and closed by policy", "closed", map[string]string{"approved_by": "system:spawn-policy", "result_status": "success"}},
		{"approved, replay running", "in_progress", map[string]string{"approved_by": "system:spawn-policy"}},
		{"closed unapproved", "closed", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
			detail.Fields["held_by"] = "operator"
			ev := escalationEvent(detail) // the create event: open, unapproved
			detail.Status = c.status
			for k, v := range c.fields {
				detail.Fields[k] = v
			}
			e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			e.EscalationCreated(context.Background(), ev)
			if n := len(sender.all()); n != 0 {
				t.Errorf("settled escalation sent %d DMs, want 0", n)
			}

			detail.Fields["handed_up_at"] = "2026-10-20T05:00:00Z"
			ev.Fields["handed_up_at"] = "2026-10-20T05:00:00Z"
			e.EscalationHandedUp(context.Background(), ev)
			if n := len(sender.all()); n != 0 {
				t.Errorf("settled escalation hand-up sent %d DMs, want 0", n)
			}
		})
	}
}
