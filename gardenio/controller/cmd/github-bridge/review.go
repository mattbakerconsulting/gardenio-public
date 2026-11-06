package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

// squawkMarkerPrefix opens the hidden HTML comment the squawk poster appends to
// every PR comment it writes (squawk.go). The review handler drops any comment
// carrying it, so an agent's own squawk is never echoed back as feedback.
const squawkMarkerPrefix = "<!-- gardenio-squawk:"

// reviewFlagField is the task-bead field gitlab-bridge sets when a reviewer
// leaves feedback; beads-viewer and the resolver key on it.
const reviewFlagField = "mr_has_review_comments"

const (
	// quoteMaxRunes caps the review text quoted into a Slack thread line.
	quoteMaxRunes = 500
	// mailBodyMaxRunes caps the review text carried in the agent mail.
	mailBodyMaxRunes = 4000
)

// ghUser is the author block shared by reviews and comments.
type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // User | Bot | Organization | Mannequin
}

// isBot reports whether the account is an automation account: an App's own
// bot user (including this App, whose squawks are posted as PR comments), or
// any other integration. Their comments are never review feedback for an agent.
func (u ghUser) isBot() bool {
	return strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(u.Login, "[bot]")
}

// trustedAssociation reports whether a comment's author_association (REST) or
// authorAssociation (GraphQL) makes its author someone whose words may steer
// the agent: the repository owner, a member of the owning organization, or an
// invited collaborator. Everyone else (CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR,
// FIRST_TIMER, MANNEQUIN, NONE, or a missing value) is anyone with a GitHub
// account, and on a public repo their PR comment is untrusted input
// (gd-b5drtT0ymT). GitHub sets the value; the commenter cannot.
func trustedAssociation(a string) bool {
	switch strings.ToUpper(strings.TrimSpace(a)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// reviewEvent is the union of the three webhook payloads the handler reads:
// pull_request_review, pull_request_review_comment and issue_comment.
type reviewEvent struct {
	Action string `json:"action"`
	Review *struct {
		User              ghUser `json:"user"`
		AuthorAssociation string `json:"author_association"`
		Body              string `json:"body"`
		State             string `json:"state"` // approved | changes_requested | commented | dismissed
		HTMLURL           string `json:"html_url"`
	} `json:"review"`
	Comment *struct {
		ID                int64  `json:"id"`
		User              ghUser `json:"user"`
		AuthorAssociation string `json:"author_association"`
		Body              string `json:"body"`
		HTMLURL           string `json:"html_url"`
		Path              string `json:"path"`
		Line              int    `json:"line"`
		// OriginalLine is set when the commented line has since moved or the
		// comment is outdated; Line is then null.
		OriginalLine int `json:"original_line"`
	} `json:"comment"`
	// PullRequest is present on the two pull_request_review* events.
	PullRequest *struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
	} `json:"pull_request"`
	// Issue is present on issue_comment; it is a PR when PullRequest is set.
	Issue *struct {
		Number      int    `json:"number"`
		HTMLURL     string `json:"html_url"`
		Title       string `json:"title"`
		PullRequest *struct {
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"`
	} `json:"issue"`
}

// reviewItem is one piece of review feedback normalized from any of the three
// event shapes.
type reviewItem struct {
	Kind   string // "review" | "comment" | "inline"
	State  string // review state, empty for comments
	Author ghUser
	// Association is GitHub's author_association for Author on this repo;
	// see trustedAssociation.
	Association string
	Body        string
	URL         string // permalink to the review/comment
	PRURL       string
	PRNumber    int
	PRTitle     string
	Path        string
	Line        int
	CommentID   int64
}

// reviewItemFor normalizes ev, or returns nil for an event that is not
// reviewer feedback worth forwarding.
func reviewItemFor(ev Event, re *reviewEvent) *reviewItem {
	switch ev.Type {
	case "pull_request_review":
		// edited and dismissed change an old review; only the submission is news.
		if re.Action != "submitted" || re.Review == nil || re.PullRequest == nil {
			return nil
		}
		state := strings.ToLower(re.Review.State)
		// A "commented" review with no text is the envelope GitHub wraps around
		// inline comments, each of which arrives as its own event.
		if state == "commented" && strings.TrimSpace(re.Review.Body) == "" {
			return nil
		}
		if state != "approved" && state != "changes_requested" && state != "commented" {
			return nil
		}
		return &reviewItem{
			Kind: "review", State: state, Author: re.Review.User,
			Association: re.Review.AuthorAssociation, Body: re.Review.Body,
			URL: re.Review.HTMLURL, PRURL: re.PullRequest.HTMLURL,
			PRNumber: re.PullRequest.Number, PRTitle: re.PullRequest.Title,
		}
	case "pull_request_review_comment":
		if re.Action != "created" || re.Comment == nil || re.PullRequest == nil {
			return nil
		}
		line := re.Comment.Line
		if line == 0 {
			line = re.Comment.OriginalLine
		}
		return &reviewItem{
			Kind: "inline", Author: re.Comment.User,
			Association: re.Comment.AuthorAssociation, Body: re.Comment.Body,
			URL: re.Comment.HTMLURL, PRURL: re.PullRequest.HTMLURL,
			PRNumber: re.PullRequest.Number, PRTitle: re.PullRequest.Title,
			Path: re.Comment.Path, Line: line, CommentID: re.Comment.ID,
		}
	case "issue_comment":
		// issue_comment also fires for plain issues; only PR conversations count.
		if re.Action != "created" || re.Comment == nil || re.Issue == nil || re.Issue.PullRequest == nil {
			return nil
		}
		prURL := firstNonEmpty(re.Issue.PullRequest.HTMLURL, re.Issue.HTMLURL)
		return &reviewItem{
			Kind: "comment", Author: re.Comment.User,
			Association: re.Comment.AuthorAssociation, Body: re.Comment.Body,
			URL: re.Comment.HTMLURL, PRURL: prURL,
			PRNumber: re.Issue.Number, PRTitle: re.Issue.Title,
			CommentID: re.Comment.ID,
		}
	}
	return nil
}

// reviewHandler forwards reviewer feedback to the agent that owns the PR: a
// quote with a link in its Slack thread, a mail the agent polls, and the
// mr_has_review_comments flag on the task bead. Only feedback from a trusted
// author (trustedAssociation) is forwarded; anyone else's comment is announced
// to the humans in the thread by link only, so its text never reaches the
// agent, its Slack thread context or the resolver flag.
type reviewHandler struct {
	daemon   beadClient
	notifier *beadNotifier
	logger   *slog.Logger
}

func newReviewHandler(daemon beadClient, logger *slog.Logger) *reviewHandler {
	return &reviewHandler{daemon: daemon, notifier: &beadNotifier{daemon: daemon, logger: logger}, logger: logger}
}

// Handle consumes the three review event types and ignores the rest (it sits
// in a fanoutSink). Every write is idempotent (set-fields, idempotency-keyed
// beads), so returning an error for a 5xx and redelivery is safe.
func (h *reviewHandler) Handle(ctx context.Context, ev Event) error {
	switch ev.Type {
	case "pull_request_review", "pull_request_review_comment", "issue_comment":
	default:
		return nil
	}
	var re reviewEvent
	if err := json.Unmarshal(ev.Payload, &re); err != nil {
		// Signed by GitHub but undecodable: retrying cannot help.
		h.logger.Warn("review: undecodable payload", "delivery", ev.DeliveryID, "event", ev.Type, "error", err)
		return nil
	}
	item := reviewItemFor(ev, &re)
	if item == nil || item.PRURL == "" {
		return nil
	}
	if item.Author.isBot() || strings.Contains(item.Body, squawkMarkerPrefix) {
		h.logger.Debug("review: bot or self comment, ignored",
			"delivery", ev.DeliveryID, "author", item.Author.Login, "pr", item.PRURL)
		return nil
	}

	tasks, err := h.daemon.ListTaskBeadsForMR(ctx, item.PRURL)
	if err != nil {
		return fmt.Errorf("list task beads for %s: %w", item.PRURL, err)
	}
	var owned []*beadsapi.BeadDetail
	for _, t := range tasks {
		// Backstop for a widened server-side query, as in gitlab-bridge.
		if t.Fields["mr_url"] == item.PRURL && workBeadTypes[t.Type] {
			owned = append(owned, t)
		}
	}
	if len(owned) == 0 {
		h.logger.Info("review: no task bead linked to pull request, ignoring",
			"delivery", ev.DeliveryID, "pr", item.PRURL, "event", ev.Type)
		return nil
	}

	trusted := trustedAssociation(item.Association)
	for _, task := range owned {
		if !trusted {
			h.announceUntrusted(ctx, ev, item, task)
			continue
		}
		if err := h.forward(ctx, ev, item, task); err != nil {
			return err
		}
	}
	return nil
}

// announceUntrusted tells the humans in the agent's thread that someone outside
// the repo's collaborators commented, with a link and no quote: the text is
// attacker-controlled, and a thread line is read back into agent context. The
// task is not flagged and the agent is not mailed.
func (h *reviewHandler) announceUntrusted(ctx context.Context, ev Event, item *reviewItem, task *beadsapi.BeadDetail) {
	h.logger.Info("review: comment from untrusted author, not forwarded to agent",
		"delivery", ev.DeliveryID, "kind", item.Kind, "author", item.Author.Login,
		"association", item.Association, "pr", item.PRURL, "task", task.ID)
	ag, err := findAgentForTask(ctx, h.daemon, task)
	if err != nil {
		h.logger.Warn("review: agent lookup failed", "task", task.ID, "error", err)
		return
	}
	key := "github-review:" + ev.DeliveryID + ":" + task.ID
	h.notifier.postThread(ctx, ag, key, untrustedThreadLine(item))
}

func (h *reviewHandler) forward(ctx context.Context, ev Event, item *reviewItem, task *beadsapi.BeadDetail) error {
	finished := task.Status == "closed" || task.Status == "done"

	// Flag first: it is the durable record, and a failure here should redeliver
	// before anything is announced. An approval carries nothing to address, and
	// a finished task has no one to address it.
	if !finished && item.State != "approved" && task.Fields[reviewFlagField] != "true" {
		if err := h.daemon.UpdateBeadFields(ctx, task.ID, map[string]string{reviewFlagField: "true"}); err != nil {
			return fmt.Errorf("set %s on %s: %w", reviewFlagField, task.ID, err)
		}
	}

	ag, err := findAgentForTask(ctx, h.daemon, task)
	if err != nil {
		h.logger.Warn("review: agent lookup failed", "task", task.ID, "error", err)
		return nil
	}
	key := "github-review:" + ev.DeliveryID + ":" + task.ID
	h.notifier.postThread(ctx, ag, key, reviewThreadLine(item))
	if !finished {
		h.notifier.mailAgent(ctx, ag, key+":mail", reviewMailSubject(item), reviewMailBody(item, task.ID))
	}
	h.logger.Info("review: forwarded",
		"delivery", ev.DeliveryID, "kind", item.Kind, "author", item.Author.Login,
		"pr", item.PRURL, "task", task.ID, "flagged", !finished && item.State != "approved")
	return nil
}

// truncateRunes cuts s to at most n runes, marking the cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " \n") + "…"
}

// quoteBlock renders body as a Slack blockquote, one "> " per line, escaped.
func quoteBlock(body string) string {
	body = strings.TrimSpace(truncateRunes(strings.TrimSpace(body), quoteMaxRunes))
	if body == "" {
		return ""
	}
	lines := strings.Split(slackText(strings.ReplaceAll(body, "\r\n", "\n")), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// reviewThreadLine is what the agent's Slack thread shows: who said what about
// which PR, with a link to the exact comment, then the quoted text.
func reviewThreadLine(it *reviewItem) string {
	pr := fmt.Sprintf("<%s|#%d %s>", it.PRURL, it.PRNumber, slackText(it.PRTitle))
	who := "*" + slackText(it.Author.Login) + "*"
	link := ""
	if it.URL != "" {
		link = fmt.Sprintf(" · <%s|view on GitHub>", it.URL)
	}
	var head string
	switch {
	case it.Kind == "review" && it.State == "approved":
		head = fmt.Sprintf(":white_check_mark: %s approved %s%s", who, pr, link)
	case it.Kind == "review" && it.State == "changes_requested":
		head = fmt.Sprintf(":x: %s requested changes on %s%s", who, pr, link)
	case it.Kind == "review":
		head = fmt.Sprintf(":speech_balloon: %s reviewed %s%s", who, pr, link)
	case it.Kind == "inline":
		head = fmt.Sprintf(":speech_balloon: %s commented on `%s` in %s%s", who, slackText(pathLine(it)), pr, link)
	default:
		head = fmt.Sprintf(":speech_balloon: %s commented on %s%s", who, pr, link)
	}
	if q := quoteBlock(it.Body); q != "" {
		return head + "\n" + q
	}
	return head
}

// untrustedThreadLine names who commented where, but carries none of the
// comment's text: a human can open the link and relay it if it is worth acting on.
func untrustedThreadLine(it *reviewItem) string {
	pr := fmt.Sprintf("<%s|#%d %s>", it.PRURL, it.PRNumber, slackText(it.PRTitle))
	assoc := strings.ToLower(strings.TrimSpace(it.Association))
	if assoc == "" {
		assoc = "unknown"
	}
	line := fmt.Sprintf(":warning: *%s* (%s, not a repo collaborator) commented on %s",
		slackText(it.Author.Login), slackText(assoc), pr)
	if it.URL != "" {
		line += fmt.Sprintf(" · <%s|view on GitHub>", it.URL)
	}
	return line + "\n_Not forwarded to the agent. Relay it yourself if it should be acted on._"
}

func pathLine(it *reviewItem) string {
	if it.Line > 0 {
		return fmt.Sprintf("%s:%d", it.Path, it.Line)
	}
	return it.Path
}

func reviewMailSubject(it *reviewItem) string {
	what := "commented on"
	switch {
	case it.State == "approved":
		what = "approved"
	case it.State == "changes_requested":
		what = "requested changes on"
	case it.Kind == "review":
		what = "reviewed"
	}
	return fmt.Sprintf("%s %s PR #%d: %s", it.Author.Login, what, it.PRNumber, it.PRTitle)
}

// reviewMailBody is plain text for the agent: everything needed to act on the
// feedback without opening GitHub first.
func reviewMailBody(it *reviewItem, taskID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub review feedback from @%s on %s", it.Author.Login, it.PRURL)
	if it.Path != "" {
		fmt.Fprintf(&b, "\nFile: %s", pathLine(it))
	}
	if body := strings.TrimSpace(it.Body); body != "" {
		fmt.Fprintf(&b, "\nComment: %s", truncateRunes(body, mailBodyMaxRunes))
	}
	if it.URL != "" {
		fmt.Fprintf(&b, "\nLink: %s", it.URL)
	}
	if it.CommentID != 0 {
		fmt.Fprintf(&b, "\nComment ID: %d", it.CommentID)
	}
	fmt.Fprintf(&b, "\nBead: %s", taskID)
	return b.String()
}
