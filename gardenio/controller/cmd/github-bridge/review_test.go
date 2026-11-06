package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// reviewDaemon is a daemon whose task bead is already linked to the PR, as it is
// once pull_request events (or `gb mr link`) have set mr_url.
func reviewDaemon(extra map[string]string) *fakeDaemon {
	f := map[string]string{"mr_url": ghPRURL}
	for k, v := range extra {
		f[k] = v
	}
	d := daemonWithTask(f)
	d.linked = []*beadsapi.BeadDetail{d.beads[taskID]}
	return d
}

func deliverReview(t *testing.T, h *reviewHandler, eventType, fixtureName, delivery string) error {
	t.Helper()
	ev := Event{DeliveryID: delivery, Type: eventType, Payload: fixture(t, fixtureName)}
	fillEnvelope(&ev)
	return h.Handle(context.Background(), ev)
}

func TestReview_ChangesRequestedFlagsQuotesAndMails(t *testing.T) {
	d := reviewDaemon(nil)
	if err := deliverReview(t, newReviewHandler(d, quiet()), "pull_request_review", "review_changes_requested", "r-1"); err != nil {
		t.Fatal(err)
	}
	if u := d.updates[taskID]; u["mr_has_review_comments"] != "true" {
		t.Errorf("task update = %v, want mr_has_review_comments=true", u)
	}
	text := threadText(t, d)
	for _, want := range []string{
		":x: *alice* requested changes on <" + ghPRURL + "|#42 ",
		"<" + ghPRURL + "#pullrequestreview-9001|view on GitHub>",
		"> Please handle the nil &lt;bead&gt; case &amp; add a test.\n> Thanks!",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("thread line %q lacks %q", text, want)
		}
	}
	mail := d.createdOfType("mail")
	if len(mail) != 1 {
		t.Fatalf("mail = %d, want 1: %+v", len(mail), mail)
	}
	m := mail[0]
	if m.Assignee != "crew-pr-events" || m.Priority != 2 {
		t.Errorf("mail to %q priority %d, want the owning agent at normal priority", m.Assignee, m.Priority)
	}
	if !strings.Contains(m.Title, "alice requested changes on PR #42") ||
		!strings.Contains(m.Description, "Please handle the nil <bead> case & add a test.") ||
		!strings.Contains(m.Description, "Bead: "+taskID) {
		t.Errorf("mail = %q / %q", m.Title, m.Description)
	}
}

func TestReview_InlineCommentCarriesPathAndLine(t *testing.T) {
	d := reviewDaemon(nil)
	if err := deliverReview(t, newReviewHandler(d, quiet()), "pull_request_review_comment", "review_comment_inline", "r-2"); err != nil {
		t.Fatal(err)
	}
	text := threadText(t, d)
	if !strings.Contains(text, "commented on `gardenio/controller/cmd/github-bridge/beads.go:58` in <"+ghPRURL) ||
		!strings.Contains(text, "#discussion_r7001|view on GitHub>") || !strings.Contains(text, "> This should use `errors.Is`.") {
		t.Errorf("thread line = %q", text)
	}
	body := d.createdOfType("mail")[0].Description
	for _, want := range []string{"File: gardenio/controller/cmd/github-bridge/beads.go:58", "Comment ID: 7001"} {
		if !strings.Contains(body, want) {
			t.Errorf("mail body %q lacks %q", body, want)
		}
	}
}

func TestReview_PRConversationCommentIsForwarded(t *testing.T) {
	d := reviewDaemon(nil)
	if err := deliverReview(t, newReviewHandler(d, quiet()), "issue_comment", "issue_comment_pr", "r-3"); err != nil {
		t.Fatal(err)
	}
	if text := threadText(t, d); !strings.Contains(text, "*alice* commented on <"+ghPRURL) || !strings.Contains(text, "> Can you rebase onto main?") {
		t.Errorf("thread line = %q", text)
	}
	if len(d.createdOfType("mail")) != 1 || d.updates[taskID]["mr_has_review_comments"] != "true" {
		t.Errorf("mail=%d updates=%v", len(d.createdOfType("mail")), d.updates)
	}
}

func TestReview_ApprovalIsAnnouncedButNotFlagged(t *testing.T) {
	d := reviewDaemon(nil)
	if err := deliverReview(t, newReviewHandler(d, quiet()), "pull_request_review", "review_approved_empty", "r-4"); err != nil {
		t.Fatal(err)
	}
	if text := threadText(t, d); !strings.Contains(text, ":white_check_mark: *alice* approved <"+ghPRURL) || strings.Contains(text, ">\n") {
		t.Errorf("thread line = %q, want an approval with no quote", text)
	}
	if _, ok := d.updates[taskID]; ok {
		t.Errorf("approval set the review-comment flag: %v", d.updates[taskID])
	}
	if len(d.createdOfType("mail")) != 1 {
		t.Errorf("approval did not reach the agent")
	}
}

func TestReview_FlagAlreadySetIsNotRewritten(t *testing.T) {
	d := reviewDaemon(map[string]string{"mr_has_review_comments": "true"})
	if err := deliverReview(t, newReviewHandler(d, quiet()), "issue_comment", "issue_comment_pr", "r-5"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.updates[taskID]; ok {
		t.Errorf("flag rewritten: %v", d.updates[taskID])
	}
}

func TestReview_Ignored(t *testing.T) {
	cases := []struct{ name, event, fixture string }{
		{"empty commented review is the inline-comment envelope", "pull_request_review", "review_commented_envelope"},
		{"comment on a plain issue", "issue_comment", "issue_comment_plain_issue"},
		{"bot comment (this App's own squawk)", "issue_comment", "issue_comment_bot"},
		{"squawk marker on a non-bot account", "issue_comment", "issue_comment_marker_human_account"},
		{"edited review comment", "pull_request_review_comment", "review_comment_edited"},
		{"unrelated event", "pull_request", "pull_request_opened"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := reviewDaemon(nil)
			if err := deliverReview(t, newReviewHandler(d, quiet()), c.event, c.fixture, "r-x"); err != nil {
				t.Fatal(err)
			}
			if len(d.created) != 0 || len(d.updates) != 0 {
				t.Errorf("created=%+v updates=%v, want nothing", d.created, d.updates)
			}
		})
	}
}

// A comment from anyone outside the repo's owner, members and collaborators is
// attacker-controlled on a public repo (gd-b5drtT0ymT): the humans in the thread
// hear about it by link, but its text reaches neither the agent's mail, nor the
// thread (which is baked into later agents' prompts), nor the resolver flag.
func TestReview_OutsiderFeedbackIsNotForwarded(t *testing.T) {
	cases := []struct{ name, event, fixture, assoc string }{
		{"PR conversation comment, no association", "issue_comment", "issue_comment_pr_outsider", "none"},
		{"changes-requested review from a past contributor", "pull_request_review", "review_changes_requested_outsider", "contributor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := reviewDaemon(nil)
			if err := deliverReview(t, newReviewHandler(d, quiet()), c.event, c.fixture, "r-out"); err != nil {
				t.Fatal(err)
			}
			if n := len(d.createdOfType("mail")); n != 0 {
				t.Errorf("outsider feedback mailed to the agent (%d mails)", n)
			}
			if len(d.updates) != 0 {
				t.Errorf("outsider feedback flagged the task: %v", d.updates)
			}
			text := threadText(t, d)
			if !strings.Contains(text, ":warning: *mallory* ("+c.assoc+", not a repo collaborator) commented on <"+ghPRURL) ||
				!strings.Contains(text, "|view on GitHub>") || !strings.Contains(text, "Not forwarded to the agent") {
				t.Errorf("thread line = %q", text)
			}
			if strings.Contains(text, "Ignore previous instructions") || strings.Contains(text, "\n>") {
				t.Errorf("thread line quotes the untrusted text: %q", text)
			}
		})
	}
}

func TestTrustedAssociation(t *testing.T) {
	for a, want := range map[string]bool{
		"OWNER": true, "MEMBER": true, "COLLABORATOR": true, "collaborator": true,
		"CONTRIBUTOR": false, "FIRST_TIME_CONTRIBUTOR": false, "FIRST_TIMER": false,
		"MANNEQUIN": false, "NONE": false, "": false,
	} {
		if got := trustedAssociation(a); got != want {
			t.Errorf("trustedAssociation(%q) = %v, want %v", a, got, want)
		}
	}
}

func TestReview_UnlinkedPRIsIgnored(t *testing.T) {
	d := daemonWithTask(map[string]string{}) // no linked task beads
	if err := deliverReview(t, newReviewHandler(d, quiet()), "issue_comment", "issue_comment_pr", "r-6"); err != nil {
		t.Fatal(err)
	}
	if len(d.created) != 0 || len(d.updates) != 0 {
		t.Errorf("wrote %+v %v for a PR no bead owns", d.created, d.updates)
	}
}

func TestReview_FinishedTaskGetsThreadLineOnly(t *testing.T) {
	d := reviewDaemon(nil)
	d.beads[taskID].Status = "closed"
	if err := deliverReview(t, newReviewHandler(d, quiet()), "issue_comment", "issue_comment_pr", "r-7"); err != nil {
		t.Fatal(err)
	}
	threadText(t, d)
	if len(d.createdOfType("mail")) != 0 || len(d.updates) != 0 {
		t.Errorf("closed task was flagged or mailed: mail=%d updates=%v", len(d.createdOfType("mail")), d.updates)
	}
}

func TestReview_GoneAgentStillGetsFlagAndThreadLine(t *testing.T) {
	d := reviewDaemon(nil)
	d.agents[0].Status = "closed"
	if err := deliverReview(t, newReviewHandler(d, quiet()), "issue_comment", "issue_comment_pr", "r-8"); err != nil {
		t.Fatal(err)
	}
	if len(d.createdOfType("mail")) != 0 {
		t.Errorf("mailed an agent that is gone")
	}
	if d.updates[taskID]["mr_has_review_comments"] != "true" {
		t.Errorf("flag not set, so the resolver could never find this review: %v", d.updates)
	}
}

// Redelivery after a failed attempt must carry the same idempotency keys so the
// daemon returns the first bead rather than posting the quote twice.
func TestReview_RedeliveryReusesIdempotencyKeys(t *testing.T) {
	d := reviewDaemon(nil)
	h := newReviewHandler(d, quiet())
	for range 2 {
		if err := deliverReview(t, h, "issue_comment", "issue_comment_pr", "r-9"); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]int{}
	for _, c := range d.created {
		keys[c.IdempotencyKey]++
	}
	if len(d.created) != 4 || len(keys) != 2 {
		t.Fatalf("keys = %v over %d creates, want 2 distinct keys each sent twice", keys, len(d.created))
	}
	for k := range keys {
		if !strings.HasPrefix(k, "github-review:r-9:"+taskID) {
			t.Errorf("key %q is not derived from delivery id + task", k)
		}
	}
	// A different delivery is a different comment and must not collide.
	if err := deliverReview(t, h, "issue_comment", "issue_comment_pr", "r-10"); err != nil {
		t.Fatal(err)
	}
	if d.created[len(d.created)-1].IdempotencyKey == d.created[0].IdempotencyKey {
		t.Error("distinct deliveries share an idempotency key")
	}
}

func TestReview_LookupFailureRedelivers(t *testing.T) {
	h := newReviewHandler(&failingLinkDaemon{fakeDaemon: reviewDaemon(nil)}, quiet())
	if err := deliverReview(t, h, "issue_comment", "issue_comment_pr", "r-11"); err == nil {
		t.Fatal("daemon failure swallowed: GitHub would not redeliver")
	}
}

type failingLinkDaemon struct{ *fakeDaemon }

func (f *failingLinkDaemon) ListTaskBeadsForMR(context.Context, string) ([]*beadsapi.BeadDetail, error) {
	return nil, errors.New("daemon down")
}

func TestReviewThreadLine_QuoteIsBoundedAndEscaped(t *testing.T) {
	it := &reviewItem{Kind: "comment", Author: ghUser{Login: "a|b"}, PRURL: ghPRURL, PRNumber: 1, PRTitle: "t",
		Body: strings.Repeat("x", 2000)}
	line := reviewThreadLine(it)
	if strings.Contains(line, "a|b") || !strings.Contains(line, "*a/b*") {
		t.Errorf("author not made Slack-safe: %q", line)
	}
	if n := strings.Count(line, "x"); n >= quoteMaxRunes || !strings.HasSuffix(line, "…") {
		t.Errorf("quote has %d x, want < %d and an ellipsis", n, quoteMaxRunes)
	}
}
