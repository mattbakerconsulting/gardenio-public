package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// liveAliasFeed mirrors the real rows served by
// GET /v1/admin/operators/slack-aliases on 2026-08-25, including the three
// shapes that break a naive resolver:
//
//   - operator 229 (Matt) carries TWO non-id subjects (`matt.baker`,
//     `Matt Baker`) alongside the real `U0OPERATOR1`, and a SECOND distinct
//     real id `U0OPERATOR2` under the same email → the email is ambiguous.
//   - the feed's emails are mixed-case (`Matthew.Baker@…`, `terry.vance@…`)
//     while operated_by carries the lowercase form.
//   - Alex's slack-aliased operator is 364, but the decision addressed to
//     him (ka-0000005256) carries operated_by_id=370 — his oidc operator row.
//     An operator_id-keyed lookup misses him; the email-keyed one finds him.
func liveAliasFeed() []beadsapi.SlackOperatorAlias {
	return []beadsapi.SlackOperatorAlias{
		{OperatorID: 226, Subject: "U02Q159FPA8", Display: "Jordan Lake", Email: "jordan.lake@acme.example"},
		{OperatorID: 227, Subject: "U0PERSON004", Display: "Pat Stone", Email: "pat.stone@acme.example"},
		{OperatorID: 229, Subject: "matt.baker", Display: "gardenio", Email: "Matthew.Baker@acme.example"},
		{OperatorID: 229, Subject: "Matt Baker", Display: "gardenio", Email: "Matthew.Baker@acme.example"},
		{OperatorID: 229, Subject: "U0OPERATOR1", Display: "gardenio", Email: "Matthew.Baker@acme.example"},
		{OperatorID: 371, Subject: "U0OPERATOR2", Display: "Matt Baker", Email: "matthew.baker@acme.example"},
		{OperatorID: 232, Subject: "U0PERSON007", Display: "Tim", Email: "terry.vance@acme.example"},
		{OperatorID: 363, Subject: "U0PERSON005", Display: "Sky Bennett", Email: "sky.bennett@acme.example"},
		{OperatorID: 364, Subject: "U0PERSON003", Display: "Alex Novak", Email: "alex.novak@contractor-b.example"},
		{OperatorID: 999, Subject: "U0PERSON010", Display: "", Email: "slack-u0basnwr9fc@noreply.kbeads.local"},
	}
}

// cveGateOptions is the verbatim options payload from ka-0000005256, the
// decision the operator answered on Alex's behalf.
const cveGateOptions = `[
 {"artifact_type":"report","id":"fail-closed","label":"Option A — gate blocks on any non-allowlisted high/critical; new no-fix CVE wedges main until a human lands a per-CVE allowlist entry","short":"Option A — gate blocks"},
 {"artifact_type":"report","id":"fail-on-fixable","label":"Option B — gate blocks only on fixable findings; new no-fix high/critical reports+alerts but does not block, allowlisted on a scheduled sweep","short":"Option B — fail on fixable"}
]`

// recordedDM is one captured Slack DM.
type recordedDM struct {
	UserID string
	Text   string
}

// fakeDMSender captures DMs instead of calling Slack.
type fakeDMSender struct {
	mu   sync.Mutex
	dms  []recordedDM
	fail error
}

func (f *fakeDMSender) SendDM(_ context.Context, userID, text string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", "", f.fail
	}
	f.dms = append(f.dms, recordedDM{UserID: userID, Text: text})
	return "D0FAKE", "1756000000.000100", nil
}

func (f *fakeDMSender) all() []recordedDM {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedDM, len(f.dms))
	copy(out, f.dms)
	return out
}

func crossOpTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordedThreadPost is one captured in-thread notice.
type recordedThreadPost struct {
	BeadID string
	Text   string
}

// fakeThreadPoster captures in-thread posts instead of calling Slack. `known`
// is the set of beads for which the bridge has a Slack message ref; a bead
// outside it reports posted=false, which is what drives the DM fallback.
type fakeThreadPoster struct {
	mu    sync.Mutex
	known map[string]bool
	posts []recordedThreadPost
	fail  error
}

func (f *fakeThreadPoster) ReplyInBeadThread(_ context.Context, beadID, text string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	if !f.known[beadID] {
		return false, nil
	}
	f.posts = append(f.posts, recordedThreadPost{BeadID: beadID, Text: text})
	return true, nil
}

func (f *fakeThreadPoster) all() []recordedThreadPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedThreadPost, len(f.posts))
	copy(out, f.posts)
	return out
}

// crossOperatorHarness wires a CrossOperatorNotify over a mock daemon and a
// capturing DM sender.
//
// It deliberately leaves Thread nil, so every pre-existing test continues to
// assert on the DM route. That is not laziness: the DM is still the real
// fallback path for a bead the bridge has no message for, and those tests cover
// resolution/dedup/suppression logic that is route-independent. The thread
// route has its own harness below.
func crossOperatorHarness(t *testing.T, beads map[string]*beadsapi.BeadDetail) (*CrossOperatorNotify, *fakeDMSender, *mockDaemon) {
	t.Helper()
	daemon := &mockDaemon{beads: beads, slackAliases: liveAliasFeed()}
	sender := &fakeDMSender{}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon: daemon,
		Sender: sender,
		Logger: crossOpTestLogger(),
		LinkBead: func(id, title string) string {
			return "<https://beads.example/" + id + "|" + id + " — " + title + ">"
		},
	})
	if c == nil {
		t.Fatal("NewCrossOperatorNotify returned nil")
	}
	return c, sender, daemon
}

// crossOperatorThreadHarness is crossOperatorHarness plus the thread poster, so
// the production routing (thread first, DM fallback) is under test.
func crossOperatorThreadHarness(t *testing.T, beads map[string]*beadsapi.BeadDetail, threaded ...string) (*CrossOperatorNotify, *fakeThreadPoster, *fakeDMSender) {
	t.Helper()
	known := make(map[string]bool, len(threaded))
	for _, id := range threaded {
		known[id] = true
	}
	daemon := &mockDaemon{beads: beads, slackAliases: liveAliasFeed()}
	sender := &fakeDMSender{}
	thread := &fakeThreadPoster{known: known}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon: daemon,
		Sender: sender,
		Thread: thread,
		Logger: crossOpTestLogger(),
		LinkBead: func(id, title string) string {
			return "<https://beads.example/" + id + "|" + id + " — " + title + ">"
		},
	})
	if c == nil {
		t.Fatal("NewCrossOperatorNotify returned nil")
	}
	return c, thread, sender
}

func i64(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// THE TWO TESTS THAT MATTER
// ---------------------------------------------------------------------------

// TestCrossOperatorNotify_ForeignResolverFiresExactlyOneMention is the primary
// acceptance test: a decision addressed to one operator, resolved by somebody
// else, produces EXACTLY ONE DM to the addressed operator that names the
// resolver and the chosen option.
//
// The fixture is the real event: ka-0000005256, a CVE-gate policy call
// addressed to alex.novak@contractor-b.example (operated_by_id 370, whose
// slack-aliased row is the DIFFERENT operator 364), answered by
// matthew.baker@acme.example as `fail-on-fixable`.
func TestCrossOperatorNotify_ForeignResolverFiresExactlyOneMention(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:           "ka-0000005256",
		Type:         "decision",
		Status:       "closed",
		Title:        "PE-13036 piece 2: fail-closed or fail-on-fixable for a new no-fix CVE?",
		OperatedBy:   "oidc:alex.novak@contractor-b.example",
		OperatedByID: i64(370),
		Fields:       map[string]string{"chosen": "fail-on-fixable", "options": cveGateOptions},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})

	ev := BeadEvent{
		ID:     detail.ID,
		Type:   "decision",
		Title:  detail.Title,
		Status: "closed",
		Fields: map[string]string{"chosen": "fail-on-fixable", "options": cveGateOptions},
		Actor:  "matthew.baker@acme.example",
		DecisionAnswer: &DecisionAnswer{
			Chosen:      "fail-on-fixable",
			RespondedBy: "matthew.baker@acme.example",
		},
	}
	c.DecisionResolved(context.Background(), ev)

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want exactly 1 DM, got %d: %+v", len(dms), dms)
	}
	got := dms[0]

	// Reaches the ADDRESSED operator — Alex's Slack id, resolved via the
	// alias feed by email, not via his operator_id (370, which has no alias).
	if got.UserID != "U0PERSON003" {
		t.Errorf("DM went to %q, want Alex U0PERSON003", got.UserID)
	}
	// @-mentions him, so the message reads as addressed to him.
	if !strings.Contains(got.Text, "<@U0PERSON003>") {
		t.Errorf("message lacks the operator @-mention:\n%s", got.Text)
	}
	// Names WHICH BEAD.
	if !strings.Contains(got.Text, "ka-0000005256") {
		t.Errorf("message lacks the bead id:\n%s", got.Text)
	}
	// Names WHO resolved it — not "someone".
	if !strings.Contains(got.Text, "matthew.baker@acme.example") {
		t.Errorf("message lacks the resolver identity:\n%s", got.Text)
	}
	if strings.Contains(strings.ToLower(got.Text), "by someone\n") {
		t.Errorf("message is vague about who resolved it:\n%s", got.Text)
	}
	// Names WHICH OPTION — the id AND its label. This is the whole point.
	if !strings.Contains(got.Text, "`fail-on-fixable`") {
		t.Errorf("message lacks the chosen option id:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "Option B") {
		t.Errorf("message lacks the chosen option label:\n%s", got.Text)
	}

	// A redelivered close event (SSE reconnect) must not DM twice.
	c.DecisionResolved(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("redelivered event produced %d DMs, want 1", n)
	}
}

// TestCrossOperatorNotify_SelfResolutionIsSilent is the second acceptance test:
// the NORMAL case — the addressed operator answering their own decision — must
// fire nothing at all. Anything else spams real humans on every resolution.
func TestCrossOperatorNotify_SelfResolutionIsSilent(t *testing.T) {
	// Every shape the resolving identity actually arrives in, all of them the
	// same human as the addressed operator.
	cases := []struct {
		name       string
		operatedBy string
		operatorID *int64
		resolver   string
	}{
		{"oidc addressed, bare-email actor", "oidc:matthew.baker@acme.example", i64(229), "matthew.baker@acme.example"},
		{"oidc addressed, oidc actor", "oidc:matthew.baker@acme.example", i64(229), "oidc:matthew.baker@acme.example"},
		{"oidc addressed, mixed-case actor", "oidc:matthew.baker@acme.example", i64(229), "Matthew.Baker@acme.example"},
		{"slack addressed, slack actor", "slack:U0OPERATOR1", i64(229), "slack:U0OPERATOR1"},
		{"slack addressed, bare-id actor", "slack:U0OPERATOR1", i64(229), "U0OPERATOR1"},
		{"slack addressed, that human's email", "slack:U0OPERATOR1", i64(229), "matthew.baker@acme.example"},
		{"oidc addressed, that human's slack id", "oidc:pat.stone@acme.example", i64(227), "slack:U0PERSON004"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := &beadsapi.BeadDetail{
				ID:           "gd-SELF00001",
				Type:         "decision",
				Status:       "closed",
				Title:        "a decision the addressed operator answered themselves",
				OperatedBy:   tc.operatedBy,
				OperatedByID: tc.operatorID,
				Fields:       map[string]string{"chosen": "fail-on-fixable", "options": cveGateOptions},
			}
			c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			c.DecisionResolved(context.Background(), BeadEvent{
				ID:             detail.ID,
				Type:           "decision",
				Status:         "closed",
				Fields:         detail.Fields,
				Actor:          tc.resolver,
				DecisionAnswer: &DecisionAnswer{Chosen: "fail-on-fixable", RespondedBy: tc.resolver},
			})
			if dms := sender.all(); len(dms) != 0 {
				t.Fatalf("self-resolution must be silent, got %d DM(s): %+v", len(dms), dms)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Escalations
// ---------------------------------------------------------------------------

// TestCrossOperatorNotify_EscalationApprovedByOther covers the higher-value
// half: escalations carry the privileged actions (merges, applies, credential
// mints), and approval is the moment the privileged call is authorized.
func TestCrossOperatorNotify_EscalationApprovedByOther(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:           "gd-V6DZ8FFKD2",
		Type:         "escalation",
		Status:       "in_progress",
		Title:        "merge !2038 on the monorepo merge queue",
		OperatedBy:   "oidc:terry.vance@acme.example",
		OperatedByID: i64(232),
		Fields: map[string]string{
			"approved_by":      "gardenio-any-agent-fleet-ein",
			"audience":         "owner",
			"operation_type":   "gitlab_merge",
			"execution_target": "local",
		},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})

	ev := BeadEvent{
		ID:     detail.ID,
		Type:   "escalation",
		Title:  detail.Title,
		Status: "in_progress",
		Fields: detail.Fields,
	}
	c.EscalationApproved(context.Background(), ev)

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM, got %d: %+v", len(dms), dms)
	}
	if dms[0].UserID != "U0PERSON007" {
		t.Errorf("DM went to %q, want Terry Vance U0PERSON007", dms[0].UserID)
	}
	for _, want := range []string{
		"<@U0PERSON007>",              // the mention
		"gd-V6DZ8FFKD2",               // which bead
		"gardenio-any-agent-fleet-ein", // WHO approved it — an agent fleet token
		"approved",                    // the outcome
		"`gitlab_merge`",              // WHICH privileged operation
	} {
		if !strings.Contains(dms[0].Text, want) {
			t.Errorf("message missing %q:\n%s", want, dms[0].Text)
		}
	}

	// The later close event for the same escalation must NOT produce a second
	// DM — one bead, one message.
	c.EscalationResolved(context.Background(), BeadEvent{
		ID:     detail.ID,
		Type:   "escalation",
		Status: "closed",
		Fields: map[string]string{
			"approved_by":   "gardenio-any-agent-fleet-ein",
			"result_status": "success",
			"executor":      "gardenio-any-agent-fleet-ein",
		},
	})
	if n := len(sender.all()); n != 1 {
		t.Errorf("approve+close produced %d DMs, want 1", n)
	}
}

// TestCrossOperatorNotify_EscalationSelfApprovalIsSilent — the operator
// approving their own escalation is the normal case for all 155 escalations
// addressed to them. Firing there would be pure noise.
func TestCrossOperatorNotify_EscalationSelfApprovalIsSilent(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:           "gd-BNO8YWCC38",
		Type:         "escalation",
		Status:       "in_progress",
		Title:        "apply terraform in acme-dev",
		OperatedBy:   "slack:U0OPERATOR1",
		OperatedByID: i64(229),
		Fields: map[string]string{
			"approved_by":       "gd-mjBjMWy6O4",
			"approved_by_email": "matthew.baker@acme.example",
			"operation_type":    "terraform_apply",
		},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	c.EscalationApproved(context.Background(), BeadEvent{
		ID: detail.ID, Type: "escalation", Status: "in_progress", Fields: detail.Fields,
	})
	if dms := sender.all(); len(dms) != 0 {
		t.Fatalf("self-approval must be silent, got %d DM(s): %+v", len(dms), dms)
	}
}

// TestCrossOperatorNotify_EscalationRejectedByOther — rejection is inferred
// from a close with no approved_by (escalations.go), and the rejecter is the
// resolving identity.
func TestCrossOperatorNotify_EscalationRejectedByOther(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:           "gd-YU6VZ9FRRW",
		Type:         "escalation",
		Status:       "closed",
		Title:        "mint a credential for the perf realm",
		OperatedBy:   "oidc:sky.bennett@acme.example",
		OperatedByID: i64(363),
		Fields:       map[string]string{"result_status": "failure", "audience": "owner"},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	c.EscalationResolved(context.Background(), BeadEvent{
		ID:       detail.ID,
		Type:     "escalation",
		Status:   "closed",
		Fields:   map[string]string{"result_status": "failure", "rejected_by": "pat.stone@acme.example"},
		ClosedBy: "pat.stone@acme.example",
	})
	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM, got %d", len(dms))
	}
	if dms[0].UserID != "U0PERSON005" {
		t.Errorf("DM went to %q, want Sky Bennett U0PERSON005", dms[0].UserID)
	}
	if !strings.Contains(dms[0].Text, "pat.stone@acme.example") {
		t.Errorf("message lacks the rejecter:\n%s", dms[0].Text)
	}
	if !strings.Contains(dms[0].Text, "rejected") {
		t.Errorf("message lacks the rejected outcome:\n%s", dms[0].Text)
	}
}

// ---------------------------------------------------------------------------
// Non-firing conditions
// ---------------------------------------------------------------------------

func TestCrossOperatorNotify_SkipsNonHumanAndMechanical(t *testing.T) {
	cases := []struct {
		name       string
		operatedBy string
		operatorID *int64
		event      BeadEvent
	}{{
		// 153 of the open escalations are stamped fleet:bridge:jira. There is
		// no human addressed, so there is nobody to tell.
		name:       "non-human operated_by",
		operatedBy: "fleet:bridge:jira",
		event:      BeadEvent{ID: "gd-GFIMRMLX0M", Type: "escalation", Fields: map[string]string{"approved_by": "some-agent"}},
	}, {
		name:       "no operated_by at all",
		operatedBy: "",
		event:      BeadEvent{ID: "gd-GFIMRMLX0M", Type: "escalation", Fields: map[string]string{"approved_by": "some-agent"}},
	}, {
		// A TTL expiry is not a party answering for someone else.
		name:       "system timeout resolver",
		operatedBy: "oidc:terry.vance@acme.example",
		operatorID: i64(232),
		event: BeadEvent{ID: "gd-GFIMRMLX0M", Type: "decision", Fields: map[string]string{"chosen": "fail-closed"},
			Actor: "system:timeout"},
	}, {
		// The approve→replay auto-close is the machinery finishing a call a
		// human already made; approved_by carries the human, and when it is
		// absent there is no accountable party.
		name:       "auto-replay close with no approver",
		operatedBy: "oidc:terry.vance@acme.example",
		operatorID: i64(232),
		event: BeadEvent{ID: "gd-GFIMRMLX0M", Type: "escalation", Fields: map[string]string{"result_status": "success"},
			ClosedBy: "auto-replay"},
	}, {
		name:       "empty resolver",
		operatedBy: "oidc:terry.vance@acme.example",
		operatorID: i64(232),
		event:      BeadEvent{ID: "gd-GFIMRMLX0M", Type: "decision", Fields: map[string]string{"chosen": "fail-closed"}},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := &beadsapi.BeadDetail{
				ID: "gd-GFIMRMLX0M", Type: tc.event.Type, Title: "skipped",
				OperatedBy: tc.operatedBy, OperatedByID: tc.operatorID,
				Fields: tc.event.Fields,
			}
			c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			if tc.event.Type == "escalation" {
				c.EscalationResolved(context.Background(), tc.event)
			} else {
				c.DecisionResolved(context.Background(), tc.event)
			}
			if dms := sender.all(); len(dms) != 0 {
				t.Fatalf("expected silence, got %d DM(s): %+v", len(dms), dms)
			}
		})
	}
}

// TestCrossOperatorNotify_AmbiguousAliasRefusesToMention — the live feed maps
// matthew.baker@acme.example to TWO distinct Slack ids. Guessing would DM a
// stale account, so the resolver refuses and the control stays quiet rather
// than mentioning the wrong human.
func TestCrossOperatorNotify_AmbiguousAliasRefusesToMention(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID: "gd-S7RYRSEKHG", Type: "decision", Status: "closed",
		Title:      "a decision addressed to the ambiguously-aliased operator",
		OperatedBy: "oidc:matthew.baker@acme.example",
		Fields:     map[string]string{"chosen": "fail-closed", "options": cveGateOptions},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	c.DecisionResolved(context.Background(), BeadEvent{
		ID: detail.ID, Type: "decision", Fields: detail.Fields,
		Actor: "gardenio-some-agent-abc",
	})
	if dms := sender.all(); len(dms) != 0 {
		t.Fatalf("ambiguous alias must not be mentioned, got %+v", dms)
	}
}

// TestCrossOperatorNotify_OperatorIDFallback — a slack-rooted operator whose
// email is blank still resolves, via operator_id.
func TestCrossOperatorNotify_OperatorIDFallback(t *testing.T) {
	daemon := &mockDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-5VINIR1PKZ": {
			ID: "gd-5VINIR1PKZ", Type: "decision", Title: "id-only operator",
			OperatedBy: "oidc:someone@acme.example", OperatedByID: i64(363),
			Fields: map[string]string{"chosen": "hold"},
		}},
		// Feed carries the operator row but NO email for it.
		slackAliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 363, Subject: "U0PERSON005", Display: "Sky Bennett", Email: ""},
		},
	}
	sender := &fakeDMSender{}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{Daemon: daemon, Sender: sender, Logger: crossOpTestLogger()})
	c.DecisionResolved(context.Background(), BeadEvent{
		ID: "gd-5VINIR1PKZ", Type: "decision",
		Fields: map[string]string{"chosen": "hold"}, Actor: "some-other-agent",
	})
	dms := sender.all()
	if len(dms) != 1 || dms[0].UserID != "U0PERSON005" {
		t.Fatalf("operator_id fallback failed: %+v", dms)
	}
}

// TestCrossOperatorNotify_FreeTextAnswerIsNamed — a decision answered as free
// text carries no `chosen`; the message must still say what was said rather
// than "your decision was resolved".
func TestCrossOperatorNotify_FreeTextAnswerIsNamed(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID: "gd-SKWOHQDIDN", Type: "decision", Status: "closed", Title: "open question",
		OperatedBy: "oidc:pat.stone@acme.example", OperatedByID: i64(227),
		Fields: map[string]string{"response_text": "hold the MR until the SSM params land"},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	c.DecisionResolved(context.Background(), BeadEvent{
		ID: detail.ID, Type: "decision", Fields: detail.Fields,
		DecisionAnswer: &DecisionAnswer{ResponseText: "hold the MR until the SSM params land", RespondedBy: "sky.bennett@acme.example"},
	})
	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM, got %d", len(dms))
	}
	if !strings.Contains(dms[0].Text, "hold the MR until the SSM params land") {
		t.Errorf("message lacks the free-text answer:\n%s", dms[0].Text)
	}
}

// TestCrossOperatorNotify_DegradesWhenAliasFeedFails — the accountability
// message must not be lost just because the admin endpoint is unavailable; a
// slack-rooted operator is still mentionable with no lookup at all.
func TestCrossOperatorNotify_DegradesWhenAliasFeedFails(t *testing.T) {
	daemon := &mockDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-HQXWTQ7L8K": {
			ID: "gd-HQXWTQ7L8K", Type: "decision", Title: "feed down",
			OperatedBy: "slack:U0PERSON004",
			Fields:     map[string]string{"chosen": "merge_now"},
		}},
		slackAliasErr: errors.New("403 forbidden"),
	}
	sender := &fakeDMSender{}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{Daemon: daemon, Sender: sender, Logger: crossOpTestLogger()})
	c.DecisionResolved(context.Background(), BeadEvent{
		ID: "gd-HQXWTQ7L8K", Type: "decision",
		Fields: map[string]string{"chosen": "merge_now"}, Actor: "gardenio-some-agent",
	})
	dms := sender.all()
	if len(dms) != 1 || dms[0].UserID != "U0PERSON004" {
		t.Fatalf("slack-rooted operator must still be reachable with the feed down: %+v", dms)
	}
}

// TestCrossOperatorNotify_AliasFeedIsCached — one fetch, not one per event.
func TestCrossOperatorNotify_AliasFeedIsCached(t *testing.T) {
	beads := map[string]*beadsapi.BeadDetail{}
	for _, id := range []string{"gd-TR1DRL6HEK", "gd-4XJBBVLKYG", "gd-1NXO1HBWTW"} {
		beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "decision", Title: "cached",
			OperatedBy: "oidc:terry.vance@acme.example", OperatedByID: i64(232),
			Fields: map[string]string{"chosen": "hold"},
		}
	}
	c, sender, daemon := crossOperatorHarness(t, beads)
	for id := range beads {
		c.DecisionResolved(context.Background(), BeadEvent{
			ID: id, Type: "decision", Fields: map[string]string{"chosen": "hold"},
			Actor: "gardenio-some-agent",
		})
	}
	if n := len(sender.all()); n != 3 {
		t.Fatalf("want 3 DMs (one per bead), got %d", n)
	}
	daemon.mu.Lock()
	calls := daemon.slackAliasCalls
	daemon.mu.Unlock()
	if calls != 1 {
		t.Errorf("alias feed fetched %d times, want 1 (TTL cache)", calls)
	}
}

// TestCrossOperatorNotify_NilIsInert — a bridge with no Slack bot must not
// panic on either entry point.
func TestCrossOperatorNotify_NilIsInert(t *testing.T) {
	var c *CrossOperatorNotify
	c.DecisionResolved(context.Background(), BeadEvent{ID: "gd-GERWSQRQBT", Type: "decision"})
	c.EscalationApproved(context.Background(), BeadEvent{ID: "gd-GERWSQRQBT", Type: "escalation"})
	c.EscalationResolved(context.Background(), BeadEvent{ID: "gd-GERWSQRQBT", Type: "escalation"})
	if got := NewCrossOperatorNotify(CrossOperatorNotifyConfig{}); got != nil {
		t.Errorf("constructor with no daemon/sender should return nil, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestOperatorSlackIndexDropsNonIDSubjectsAndSentinelEmails(t *testing.T) {
	daemon := &mockDaemon{slackAliases: liveAliasFeed()}
	x := newOperatorSlackIndex(daemon, crossOpTestLogger())

	// Operator 229 has three kind=slack aliases, only one of which is a real
	// Slack id — the other two ("matt.baker", "Matt Baker") must be dropped so
	// the operator_id lookup is unambiguous.
	got := x.Resolve(context.Background(), "slack:U0OPERATOR1", i64(229), "")
	if got.SlackID != "U0OPERATOR1" {
		t.Errorf("slack-rooted resolve = %q, want U0OPERATOR1", got.SlackID)
	}

	// The sentinel-email row must not be reachable by email.
	if got := x.Resolve(context.Background(), "oidc:slack-u0basnwr9fc@noreply.kbeads.local", nil, ""); got.SlackID != "" {
		t.Errorf("sentinel email resolved to %q, want no mention", got.SlackID)
	}

	// Mixed-case feed email vs lowercase operated_by.
	if got := x.Resolve(context.Background(), "oidc:terry.vance@acme.example", nil, ""); got.SlackID != "U0PERSON007" {
		t.Errorf("case-insensitive email resolve = %q, want U0PERSON007", got.SlackID)
	}
}

func TestEmailFromOperator(t *testing.T) {
	cases := map[string]string{
		"oidc:matthew.baker@acme.example": "matthew.baker@acme.example",
		"matthew.baker@acme.example":      "matthew.baker@acme.example",
		"slack:U0OPERATOR1":              "",
		"fleet:bridge:jira":              "",
		"system:timeout":                 "",
		"gardenio-some-agent-abc":         "",
		"":                               "",
	}
	for in, want := range cases {
		if got := emailFromOperator(in); got != want {
			t.Errorf("emailFromOperator(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscalationResolverPrefersTheDecidingHuman(t *testing.T) {
	// The executor mechanically ran an approval a human made; approved_by is
	// the accountable party, so keying on executor would fire on essentially
	// every escalation and say nothing.
	got := escalationResolver(BeadEvent{Fields: map[string]string{
		"approved_by_email": "sky.bennett@acme.example",
		"approved_by":       "gd-Cf0RgEf3T3",
		"executor":          "gardenio-worker-xyz",
	}, ClosedBy: "auto-replay"})
	if got != "sky.bennett@acme.example" {
		t.Errorf("escalationResolver = %q, want the approving human", got)
	}

	// closed_by == "auto-replay" is mechanical and must not be treated as a
	// resolving party.
	if got := escalationResolver(BeadEvent{ClosedBy: "auto-replay"}); got != "" {
		t.Errorf("auto-replay leaked through as resolver: %q", got)
	}
}

func TestStatusChangedToClosed(t *testing.T) {
	if statusChangedToClosed(nil) {
		t.Error("nil event")
	}
	if statusChangedToClosed(&BeadEvent{}) {
		t.Error("no changes")
	}
	if statusChangedToClosed(&BeadEvent{Changes: map[string]any{"priority": 1}}) {
		t.Error("unrelated change")
	}
	if statusChangedToClosed(&BeadEvent{Changes: map[string]any{"status": "in_progress"}}) {
		t.Error("non-terminal status")
	}
	if !statusChangedToClosed(&BeadEvent{Changes: map[string]any{"status": "closed"}}) {
		t.Error("PATCH-to-closed must be detected — it is the only event that path emits")
	}
}

func TestBuildCrossOperatorMessageShape(t *testing.T) {
	got := buildCrossOperatorMessage(crossOperatorMessage{
		Kind:     "decision",
		BeadLink: "<https://beads/gd-X|gd-X — a title>",
		Mention:  "<@U0PERSON003>",
		Resolver: "matthew.baker@acme.example",
		Outcome:  "`fail-on-fixable` — Option B",
	})
	want := ":eyes: <@U0PERSON003> — your decision was resolved by someone else.\n" +
		"*<https://beads/gd-X|gd-X — a title>*\n" +
		"Resolved by: `matthew.baker@acme.example`\n" +
		"Resolved as: `fail-on-fixable` — Option B\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("message shape drifted.\n got: %q\nwant prefix: %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Wired through the real SSE handlers
// ---------------------------------------------------------------------------

// sseClosePayload builds the wire-shape payload the daemon publishes on
// beads.bead.closed / beads.bead.updated, including the wrapper-level fields
// (closed_by / actor / decision_answer / changes) that marshalSSEBeadPayload
// drops.
func sseClosePayload(bead BeadEvent) []byte {
	w := map[string]any{"bead": bead}
	if bead.ClosedBy != "" {
		w["closed_by"] = bead.ClosedBy
	}
	if bead.Actor != "" {
		w["actor"] = bead.Actor
	}
	if bead.DecisionAnswer != nil {
		w["decision_answer"] = bead.DecisionAnswer
	}
	if len(bead.Changes) > 0 {
		w["changes"] = bead.Changes
	}
	data, _ := json.Marshal(w)
	return data
}

// TestDecisions_HandleClosed_WiresCrossOperatorMention drives the REAL
// Decisions.handleClosed SSE handler end to end and asserts the DM fires — the
// hook being wired is the thing that ships, not the notifier in isolation.
func TestDecisions_HandleClosed_WiresCrossOperatorMention(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = liveAliasFeed()
	daemon.beads["gd-NARKMVGCHN"] = &beadsapi.BeadDetail{
		ID: "gd-NARKMVGCHN", Type: "decision", Status: "closed",
		Title:      "unity.smoketest study-build slowness: which follow-up?",
		OperatedBy: "oidc:terry.vance@acme.example", OperatedByID: i64(232),
		Fields: map[string]string{"chosen": "otel-only", "options": `[{"id":"otel-only","label":"Open MR for the otel_trace_level=info change ONLY"}]`},
	}
	sender := &fakeDMSender{}
	cross := NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(),
	})
	d := NewDecisions(DecisionsConfig{
		Daemon: daemon, Notifier: &mockNotifier{}, CrossOperator: cross, Logger: crossOpTestLogger(),
	})

	d.handleClosed(context.Background(), sseClosePayload(BeadEvent{
		ID: "gd-NARKMVGCHN", Type: "decision", Status: "closed",
		Fields:         map[string]string{"chosen": "otel-only"},
		DecisionAnswer: &DecisionAnswer{Chosen: "otel-only", RespondedBy: "matthew.baker@acme.example"},
	}))

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM through the real handler, got %d: %+v", len(dms), dms)
	}
	if dms[0].UserID != "U0PERSON007" {
		t.Errorf("DM went to %q, want Terry Vance U0PERSON007", dms[0].UserID)
	}
	for _, want := range []string{"<@U0PERSON007>", "gd-NARKMVGCHN", "matthew.baker@acme.example", "`otel-only`"} {
		if !strings.Contains(dms[0].Text, want) {
			t.Errorf("message missing %q:\n%s", want, dms[0].Text)
		}
	}
}

// TestDecisions_HandleClosed_SelfResolutionSendsNothing is the negative half of
// the wired test: the addressed operator answering their own decision goes
// through the same handler and produces no Slack side effect at all.
func TestDecisions_HandleClosed_SelfResolutionSendsNothing(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = liveAliasFeed()
	daemon.beads["gd-SRNYEATI6B"] = &beadsapi.BeadDetail{
		ID: "gd-SRNYEATI6B", Type: "decision", Status: "closed", Title: "my own decision",
		OperatedBy: "slack:U0OPERATOR1", OperatedByID: i64(229),
		Fields: map[string]string{"chosen": "merge_now"},
	}
	sender := &fakeDMSender{}
	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		CrossOperator: NewCrossOperatorNotify(CrossOperatorNotifyConfig{
			Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(),
		}),
		Logger: crossOpTestLogger(),
	})

	d.handleClosed(context.Background(), sseClosePayload(BeadEvent{
		ID: "gd-SRNYEATI6B", Type: "decision", Status: "closed",
		Fields:         map[string]string{"chosen": "merge_now"},
		DecisionAnswer: &DecisionAnswer{Chosen: "merge_now", RespondedBy: "matthew.baker@acme.example"},
	}))

	if dms := sender.all(); len(dms) != 0 {
		t.Fatalf("addressed operator resolving their own decision must send nothing, got: %+v", dms)
	}
}

// TestEscalations_HandleUpdated_WiresApprovalMention drives the REAL
// Escalations.handleUpdated handler on the approval transition.
func TestEscalations_HandleUpdated_WiresApprovalMention(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = liveAliasFeed()
	daemon.beads["gd-WBV4FVZSLC"] = &beadsapi.BeadDetail{
		ID: "gd-WBV4FVZSLC", Type: "escalation", Status: "in_progress",
		Title:      "site-deployment !1264: merge",
		OperatedBy: "oidc:pat.stone@acme.example", OperatedByID: i64(227),
		Fields: map[string]string{
			"approved_by":    "gardenio-any-agent-fleet-ein",
			"audience":       "owner",
			"operation_type": "gitlab_merge",
		},
	}
	sender := &fakeDMSender{}
	e := NewEscalations(EscalationsConfig{
		Daemon: daemon,
		Logger: crossOpTestLogger(),
		CrossOperator: NewCrossOperatorNotify(CrossOperatorNotifyConfig{
			Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(),
		}),
	})

	e.handleUpdated(context.Background(), sseClosePayload(BeadEvent{
		ID: "gd-WBV4FVZSLC", Type: "escalation", Status: "in_progress",
		Fields: map[string]string{
			"approved_by":    "gardenio-any-agent-fleet-ein",
			"operation_type": "gitlab_merge",
		},
		Changes: map[string]any{"approved_by": "gardenio-any-agent-fleet-ein", "status": "in_progress"},
	}))

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want 1 DM through the real handler, got %d: %+v", len(dms), dms)
	}
	if dms[0].UserID != "U0PERSON004" {
		t.Errorf("DM went to %q, want Pat Stone U0PERSON004", dms[0].UserID)
	}
	if !strings.Contains(dms[0].Text, "gardenio-any-agent-fleet-ein") {
		t.Errorf("message lacks the approving agent:\n%s", dms[0].Text)
	}
	if !strings.Contains(dms[0].Text, "`gitlab_merge`") {
		t.Errorf("message lacks the privileged operation:\n%s", dms[0].Text)
	}
}

// TestEscalations_HandleUpdated_PatchToClosedIsCaught covers the coverage hole:
// a PATCH that sets status=closed emits only beads.bead.updated, so a
// closed-only watcher would never see that termination.
func TestEscalations_HandleUpdated_PatchToClosedIsCaught(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = liveAliasFeed()
	daemon.beads["gd-LTSNKDYERB"] = &beadsapi.BeadDetail{
		ID: "gd-LTSNKDYERB", Type: "escalation", Status: "closed",
		Title:      "an escalation terminated with the wrong verb",
		OperatedBy: "oidc:sky.bennett@acme.example", OperatedByID: i64(363),
		Fields: map[string]string{"result_status": "failure", "audience": "owner"},
	}
	sender := &fakeDMSender{}
	e := NewEscalations(EscalationsConfig{
		Daemon: daemon,
		Logger: crossOpTestLogger(),
		CrossOperator: NewCrossOperatorNotify(CrossOperatorNotifyConfig{
			Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(),
		}),
	})

	e.handleUpdated(context.Background(), sseClosePayload(BeadEvent{
		ID: "gd-LTSNKDYERB", Type: "escalation", Status: "closed",
		Fields:  map[string]string{"result_status": "failure"},
		Changes: map[string]any{"status": "closed"},
		Actor:   "gardenio-some-other-agent",
	}))

	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("PATCH-to-closed must still notify, got %d DM(s)", len(dms))
	}
	if dms[0].UserID != "U0PERSON005" {
		t.Errorf("DM went to %q, want Sky Bennett U0PERSON005", dms[0].UserID)
	}
}

// TestCrossOperatorNotify_ServiceActorFallbackIsSilent guards the failure mode
// most likely to spam a real human. The daemon's decisionAttribution keeps the
// literal actor "static" when a service-token resolve has no resolvable human
// operator (kbeads http_decisions.go:330). Treating that as a resolving party
// would DM the addressed operator "resolved by `static`" on every unattributed
// service resolve.
func TestCrossOperatorNotify_ServiceActorFallbackIsSilent(t *testing.T) {
	for _, resolver := range []string{"static", "system", "system:timeout", "fleet:bridge:jira", "anonymous", "unknown", "auto-replay"} {
		t.Run(resolver, func(t *testing.T) {
			detail := &beadsapi.BeadDetail{
				ID: "gd-KCGAL1G1ES", Type: "decision", Status: "closed", Title: "unattributed service resolve",
				OperatedBy: "oidc:terry.vance@acme.example", OperatedByID: i64(232),
				Fields: map[string]string{"chosen": "hold"},
			}
			c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			c.DecisionResolved(context.Background(), BeadEvent{
				ID: detail.ID, Type: "decision", Fields: detail.Fields,
				DecisionAnswer: &DecisionAnswer{Chosen: "hold", RespondedBy: resolver},
			})
			if dms := sender.all(); len(dms) != 0 {
				t.Fatalf("resolver %q must be treated as mechanical, got %+v", resolver, dms)
			}
		})
	}
}

// TestCrossOperatorNotify_SlackModalRelayIsSilent documents WHY the Slack
// modal path does not spam: the bridge relays an operator's answer under its
// own ordinary fleet token, but the daemon's decisionAttribution yields the
// "static" service actor to the human threaded via X-Operated-By, so
// responded_by carries the clicking human — and that human is the addressed
// operator in the overwhelmingly common case.
func TestCrossOperatorNotify_SlackModalRelayIsSilent(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID: "gd-9ROZCNAMRB", Type: "decision", Status: "closed", Title: "answered from the Slack modal",
		OperatedBy: "slack:U0OPERATOR1", OperatedByID: i64(229),
		Fields: map[string]string{"chosen": "merge_now"},
	}
	c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	c.DecisionResolved(context.Background(), BeadEvent{
		ID: detail.ID, Type: "decision", Fields: detail.Fields,
		// What the daemon actually stamps for a bridge-relayed operator click.
		DecisionAnswer: &DecisionAnswer{Chosen: "merge_now", RespondedBy: "slack:U0OPERATOR1"},
	})
	if dms := sender.all(); len(dms) != 0 {
		t.Fatalf("a relayed operator click must be silent, got %+v", dms)
	}
}

// ---------------------------------------------------------------------------
// Measured spam sources from the live queue
// ---------------------------------------------------------------------------

// seedUserDirectory builds a UserDirectory pre-populated from user beads so
// resolver-side provider-handle resolution can be exercised.
func seedUserDirectory(t *testing.T, beads []BeadEvent) *UserDirectory {
	t.Helper()
	dir := NewUserDirectory(&mockDaemon{}, crossOpTestLogger())
	for i := range beads {
		dir.upsertFromEvent(&beads[i])
	}
	// Mark fresh so ensureFresh does not clobber the seed with an empty Refresh.
	dir.mu.Lock()
	dir.refreshedAt = time.Now()
	dir.mu.Unlock()
	return dir
}

// TestCrossOperatorNotify_GitLabBotTokenIsMechanical guards the largest
// measured spam source. Of 300 recently-closed escalations, 33 carry a GitLab
// group access token identity in approved_by — the escalation reaper recording
// that an automation satisfied the request out of band. There is no party who
// answered for the addressed operator, so there is nothing to report.
func TestCrossOperatorNotify_GitLabBotTokenIsMechanical(t *testing.T) {
	for _, resolver := range []string{
		"group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa",
		"project_99_bot_deadbeefcafe1234",
	} {
		t.Run(resolver, func(t *testing.T) {
			detail := &beadsapi.BeadDetail{
				ID: "gd-LSGOUYBHXH", Type: "escalation", Status: "closed",
				Title:      "merge the MR this escalation was asking about",
				OperatedBy: "slack:U0OPERATOR1", OperatedByID: i64(229),
				Fields: map[string]string{"approved_by": resolver, "result_status": "success"},
			}
			c, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			c.EscalationResolved(context.Background(), BeadEvent{
				ID: detail.ID, Type: "escalation", Status: "closed", Fields: detail.Fields,
			})
			if dms := sender.all(); len(dms) != 0 {
				t.Fatalf("bot-token resolver %q must be mechanical, got %+v", resolver, dms)
			}
		})
	}
	// Sanity: a real agent name of similar shape must still fire.
	if isMechanicalResolver("gardenio-any-agent-fleet-ein") {
		t.Error("an agent name must NOT be treated as mechanical — surfacing that is the point")
	}
}

// TestCrossOperatorNotify_GitLabHandleOfSameHumanIsSilent guards the false
// positive that would have DM'd real operators about their own work. Measured:
// approved_by carries the bare GitLab usernames `matthewbaker`, `fshot` and
// `gardeniobot` on closed escalations. None of those string-compare against
// `slack:U0OPERATOR1` or `oidc:...`, so without directory resolution the
// operator gets told a stranger actioned their own escalation.
//
// Note the handle is `matthewbaker`, NOT `matthew.baker` — the dotted form is a
// different GitLab account, so this has to be a real lookup.
func TestCrossOperatorNotify_GitLabHandleOfSameHumanIsSilent(t *testing.T) {
	dir := seedUserDirectory(t, []BeadEvent{{
		ID: "gd-LMGAHQWbU0", Title: "Matt Baker", Fields: map[string]string{
			"slack_id": "U0OPERATOR1", "email": "matthew.baker@acme.example", "gitlab_username": "matthewbaker",
		},
	}})
	if u := dir.LookupByGitLabUsername(context.Background(), "matthewbaker"); u == nil || u.SlackID != "U0OPERATOR1" {
		t.Fatalf("setup: directory did not index the gitlab handle: %+v", u)
	}

	daemon := &mockDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-B93CRUUX3G": {
			ID: "gd-B93CRUUX3G", Type: "escalation", Status: "closed",
			Title:      "an escalation the operator actioned under their GitLab identity",
			OperatedBy: "slack:U0OPERATOR1", OperatedByID: i64(229),
			Fields: map[string]string{"approved_by": "matthewbaker", "result_status": "success"},
		}},
		slackAliases: liveAliasFeed(),
	}
	sender := &fakeDMSender{}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(), Users: dir,
	})
	c.EscalationResolved(context.Background(), BeadEvent{
		ID: "gd-B93CRUUX3G", Type: "escalation", Status: "closed",
		Fields: map[string]string{"approved_by": "matthewbaker", "result_status": "success"},
	})
	if dms := sender.all(); len(dms) != 0 {
		t.Fatalf("the operator's own GitLab handle must be recognised as them, got %+v", dms)
	}

	// A DIFFERENT human's GitLab handle must still fire.
	daemon.beads["gd-1FK959QJIH"] = &beadsapi.BeadDetail{
		ID: "gd-1FK959QJIH", Type: "escalation", Status: "closed",
		Title:      "an escalation actioned by a different human",
		OperatedBy: "oidc:sky.bennett@acme.example", OperatedByID: i64(363),
		Fields: map[string]string{"approved_by": "matthewbaker", "result_status": "success", "audience": "owner"},
	}
	c.EscalationResolved(context.Background(), BeadEvent{
		ID: "gd-1FK959QJIH", Type: "escalation", Status: "closed",
		Fields: map[string]string{"approved_by": "matthewbaker", "result_status": "success"},
	})
	dms := sender.all()
	if len(dms) != 1 || dms[0].UserID != "U0PERSON005" {
		t.Fatalf("a different human's handle must still notify Shannon: %+v", dms)
	}
}

// TestCrossOperatorNotify_SendFailureIsRetried — the control's whole premise is
// that a cross-operator resolution is never silent, so a transient Slack error
// must not become a permanent drop. The bead's dedup claim is released on send
// failure so a redelivered SSE event retries; once a send succeeds the claim
// sticks and a further redelivery is a no-op.
func TestCrossOperatorNotify_SendFailureIsRetried(t *testing.T) {
	daemon := &mockDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-R30D9VIFFT": {
			ID: "gd-R30D9VIFFT", Type: "decision", Status: "closed", Title: "transient slack failure",
			OperatedBy: "oidc:pat.stone@acme.example", OperatedByID: i64(227),
			Fields: map[string]string{"chosen": "hold"},
		}},
		slackAliases: liveAliasFeed(),
	}
	sender := &fakeDMSender{fail: errors.New("slack 503")}
	c := NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon: daemon, Sender: sender, Logger: crossOpTestLogger(),
	})
	ev := BeadEvent{
		ID: "gd-R30D9VIFFT", Type: "decision", Fields: map[string]string{"chosen": "hold"},
		DecisionAnswer: &DecisionAnswer{Chosen: "hold", RespondedBy: "sky.bennett@acme.example"},
	}

	c.DecisionResolved(context.Background(), ev)
	if n := len(sender.all()); n != 0 {
		t.Fatalf("setup: send was supposed to fail, recorded %d", n)
	}

	// Slack recovers; the redelivered event must now get through.
	sender.mu.Lock()
	sender.fail = nil
	sender.mu.Unlock()
	c.DecisionResolved(context.Background(), ev)
	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("retry after a transient failure must deliver exactly once, got %d", len(dms))
	}
	if dms[0].UserID != "U0PERSON004" {
		t.Errorf("DM went to %q, want Pat Stone U0PERSON004", dms[0].UserID)
	}

	// And the claim now sticks.
	c.DecisionResolved(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("after a successful send the claim must stick, got %d DMs", n)
	}
}

// ---------------------------------------------------------------------------
// Routing: in-thread first, DM only as fallback (ka-0000000294)
// ---------------------------------------------------------------------------

// TestCrossOperatorNotify_PostsInBeadThreadNotDM is the acceptance test for the
// operator ask that reversed the original routing (Pat Stone, 2026-08-26:
// "I really don't like getting DM's to say that one of your threads has updated
// a decision. That should just be captured in the actual slack conversation.").
//
// The fixture is the real event that prompted it: gd-36bK0krDWA, addressed to
// Patrick (slack:U0PERSON004), closed by the thread agent
// thread-1787597902-823769 — an AGENT resolving a human's decision, which is
// exactly the case this control exists to surface.
//
// A DM here is a REGRESSION, not a second copy, so zero DMs is asserted.
func TestCrossOperatorNotify_PostsInBeadThreadNotDM(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:         "gd-36bK0krDWA",
		Type:       "decision",
		Status:     "closed",
		Title:      "e2e-qa nightly: register it as a Forge promote target",
		OperatedBy: "slack:U0PERSON004",
		Fields:     map[string]string{"chosen": "register"},
	}
	c, thread, sender := crossOperatorThreadHarness(t,
		map[string]*beadsapi.BeadDetail{detail.ID: detail}, detail.ID)

	ev := BeadEvent{
		ID: detail.ID, Type: "decision", Title: detail.Title, Status: "closed",
		Fields:   map[string]string{"chosen": "register"},
		ClosedBy: "thread-1787597902-823769",
		DecisionAnswer: &DecisionAnswer{
			Chosen:      "register",
			RespondedBy: "thread-1787597902-823769",
		},
	}
	c.DecisionResolved(context.Background(), ev)

	posts := thread.all()
	if len(posts) != 1 {
		t.Fatalf("want exactly 1 in-thread post, got %d: %+v", len(posts), posts)
	}
	if n := len(sender.all()); n != 0 {
		t.Fatalf("the notice must NOT also be DMed, got %d DMs: %+v", n, sender.all())
	}
	if posts[0].BeadID != detail.ID {
		t.Errorf("posted under bead %q, want %q", posts[0].BeadID, detail.ID)
	}
	// The in-thread notice keeps every element the DM carried: the @-mention
	// (so the addressed operator is still notified), the bead, and the resolver.
	if !strings.Contains(posts[0].Text, "<@U0PERSON004>") {
		t.Errorf("in-thread notice lacks the operator @-mention:\n%s", posts[0].Text)
	}
	if !strings.Contains(posts[0].Text, "thread-1787597902-823769") {
		t.Errorf("in-thread notice lacks the resolver identity:\n%s", posts[0].Text)
	}

	// Redelivery (SSE reconnect) must not double-post in the thread either.
	c.DecisionResolved(context.Background(), ev)
	if n := len(thread.all()); n != 1 {
		t.Errorf("redelivered event produced %d in-thread posts, want 1", n)
	}
}

// TestCrossOperatorNotify_FallsBackToDMWithoutSlackMessage guards the reason the
// DM path survives at all: a bead the bridge has no message ref for. Dropping
// the notice there would reintroduce the silence this control exists to
// prevent — the motivating case (ka-0000005256) was addressed to an external
// collaborator with no channel presence.
func TestCrossOperatorNotify_FallsBackToDMWithoutSlackMessage(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:           "ka-0000005256",
		Type:         "decision",
		Status:       "closed",
		Title:        "PE-13036 piece 2: fail-closed or fail-on-fixable for a new no-fix CVE?",
		OperatedBy:   "oidc:alex.novak@contractor-b.example",
		OperatedByID: i64(370),
		Fields:       map[string]string{"chosen": "fail-on-fixable", "options": cveGateOptions},
	}
	// No bead id passed as threaded → the bridge has no message for it.
	c, thread, sender := crossOperatorThreadHarness(t,
		map[string]*beadsapi.BeadDetail{detail.ID: detail})

	c.DecisionResolved(context.Background(), BeadEvent{
		ID: detail.ID, Type: "decision", Title: detail.Title, Status: "closed",
		Fields: map[string]string{"chosen": "fail-on-fixable", "options": cveGateOptions},
		Actor:  "matthew.baker@acme.example",
		DecisionAnswer: &DecisionAnswer{
			Chosen:      "fail-on-fixable",
			RespondedBy: "matthew.baker@acme.example",
		},
	})

	if n := len(thread.all()); n != 0 {
		t.Fatalf("nothing should have posted in-thread, got %d", n)
	}
	dms := sender.all()
	if len(dms) != 1 {
		t.Fatalf("want exactly 1 fallback DM, got %d: %+v", len(dms), dms)
	}
	if dms[0].UserID != "U0PERSON003" {
		t.Errorf("fallback DM went to %q, want Alex U0PERSON003", dms[0].UserID)
	}
}

// TestCrossOperatorNotify_ThreadPostFailureReleasesClaim mirrors the DM-side
// retry guarantee on the new primary route. A transient Slack error must not
// silently consume the one notice this bead will ever get, and it must NOT
// silently degrade to a DM either — a failed post is a failure to retry, not
// evidence the thread is missing.
func TestCrossOperatorNotify_ThreadPostFailureReleasesClaim(t *testing.T) {
	detail := &beadsapi.BeadDetail{
		ID:         "gd-MCSAQEQ7ZK",
		Type:       "decision",
		Status:     "closed",
		Title:      "transient Slack failure on the thread route",
		OperatedBy: "slack:U0PERSON004",
		Fields:     map[string]string{"chosen": "hold"},
	}
	c, thread, sender := crossOperatorThreadHarness(t,
		map[string]*beadsapi.BeadDetail{detail.ID: detail}, detail.ID)
	thread.mu.Lock()
	thread.fail = errors.New("slack: rate_limited")
	thread.mu.Unlock()

	ev := BeadEvent{
		ID: detail.ID, Type: "decision", Status: "closed",
		Fields:         map[string]string{"chosen": "hold"},
		DecisionAnswer: &DecisionAnswer{Chosen: "hold", RespondedBy: "sky.bennett@acme.example"},
	}
	c.DecisionResolved(context.Background(), ev)
	if n := len(thread.all()); n != 0 {
		t.Fatalf("setup: the post was supposed to fail, recorded %d", n)
	}
	if n := len(sender.all()); n != 0 {
		t.Fatalf("a failed thread post must not degrade to a DM, got %d", n)
	}

	// Slack recovers; the redelivered event must now get through in-thread.
	thread.mu.Lock()
	thread.fail = nil
	thread.mu.Unlock()
	c.DecisionResolved(context.Background(), ev)
	if n := len(thread.all()); n != 1 {
		t.Fatalf("retry after a transient failure must post exactly once, got %d", n)
	}

	// And the claim now sticks.
	c.DecisionResolved(context.Background(), ev)
	if n := len(thread.all()); n != 1 {
		t.Errorf("after a successful post the claim must stick, got %d posts", n)
	}
}
