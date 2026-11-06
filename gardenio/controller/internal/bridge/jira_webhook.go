// Jira Webhook Handler
//
// Receives Jira Cloud (or Server/Data Center) webhook events and processes
// comment_created events to bridge Jira comments into beads and nudge agents.
// comment_deleted is handled as an explicit, logged no-op (the bead
// conversation is append-only — see the switch in JiraWebhookHandler).
//
// Endpoint: POST /webhook
// Port:     8091 (configurable via JIRA_LISTEN_ADDR)
//
// Authentication:
//
//	HMAC-SHA256 via X-Hub-Signature header (format: "sha256=<hex>").
//	REQUIRED in production — requests are rejected when JIRA_WEBHOOK_SECRET is
//	not set (fail-closed). Set JIRA_WEBHOOK_DEV_MODE=true to skip verification
//	during local development/testing only.
//
// Jira Cloud webhook registration:
//  1. Jira Settings → System → Webhooks → Create
//  2. Set URL to https://<jiraBridge.webhook.host>/webhook
//  3. Set Secret to the same value as JIRA_WEBHOOK_SECRET / K8s secret
//  4. Enable events: comment_created (optionally comment_updated),
//     jira:issue_created (so a @gardenio mention in a NEW ticket's description
//     spawns an agent — H2, epic ka-0000007401 — not only mentions in comments),
//     and jira:issue_updated (so a @gardenio mention ADDED to an existing
//     ticket's description by an edit spawns an agent — H5, epic ka-0000007401 —
//     firing only when the edit newly introduced the mention)
//  5. Optional JQL filter: project in (PE, DEVOPS)
//
// Jira Server/Data Center:
//
//	Same steps via Administration → System → Webhooks. HMAC verification
//	uses the same X-Hub-Signature header. Older Jira Server versions that
//	do not support HMAC signing should use JIRA_WEBHOOK_DEV_MODE=true.
//
// Helm config (values.yaml):
//
//	jiraBridge.webhook.enabled: true
//	jiraBridge.webhook.host: "jira-webhook.example.com"
//	jiraBridge.webhook.secret: "<shared-secret>"          (dev/testing)
//	jiraBridge.webhook.secretName: "<k8s-secret-name>"    (production, key: webhook-secret)
//	jiraBridge.webhook.devMode: false                     (skip HMAC, dev only)
//
// See also: gardenio/helm/gardenio/templates/jira-bridge/ingressroute.yaml
package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// defaultNudgeCooldown is the default throttle window for Jira comment nudges.
const defaultNudgeCooldown = 5 * time.Minute

// jiraMaxBodyBytes bounds Jira webhook request bodies. Jira comment_created
// payloads are typically tens of KB but can be larger when issue descriptions
// or ADF comment bodies are big; 2 MiB is a generous cap that still prevents
// memory exhaustion from an attacker-controlled body. See AUDIT-2026-04-18 §H2.
const jiraMaxBodyBytes = 2 << 20 // 2 MiB

// JiraWebhookConfig holds configuration for the Jira webhook handler.
type JiraWebhookConfig struct {
	Daemon         JiraWebhookBeadClient
	WebhookSecret  string // HMAC-SHA256 secret for verifying Jira webhook signatures
	DevMode        bool   // when true, skip HMAC verification (dev/testing only)
	Nudge          NudgeFunc
	NudgeCooldown  time.Duration       // throttle window for comment nudges; 0 uses defaultNudgeCooldown
	MentionHandler *JiraMentionHandler // optional: handles @gardenio mentions in comments
	// Resolver, when non-nil, upgrades follow-up delivery from nudge-only to
	// nudge-or-respawn: a live assigned agent is nudged as before, and a
	// STOPPED one is re-spawned under the same name so the entrypoint resumes
	// the prior session and the ticket conversation continues (F3, plan
	// ka-0000007401 — parity with the Slack thread re-spawn). Nil preserves
	// the legacy nudge-only behavior.
	Resolver *JiraAgentResolver
	// WritePolicy gates the F3 re-spawn per project (F5 spine: PE/DEVOPS
	// allow, FSM block, mode-independent). Nil falls back to the legacy
	// deny-by-default project gate — never to allow-all.
	WritePolicy *WritePolicyStore
	// BotAccountID is the bridge's own Jira account id (JIRA_BOT_ACCOUNT_ID).
	// Comments it authored (agent replies / squawk-forwards echoed back by
	// this webhook) must never re-spawn an agent, or a stopped agent's own
	// reply would wake it in a loop.
	BotAccountID string
	// BotAccountIDs is the SET of gardenio bot Jira account ids
	// (JIRA_BOT_ACCOUNT_IDS, comma-separated). It suppresses the WAKE for a
	// comment authored by any of these accounts while still mirroring the
	// comment to the bead — the identity-based defence for the 4th self-wake
	// variant (ka-0000003581), where an agent's own free-text `gb jira reply`
	// wakes the assignee. Content classification cannot catch arbitrary agent
	// prose, but the AUTHOR is always one of gardenio's bot accounts, so an
	// author-set check is the only stable guard. Distinct from BotAccountID
	// above (singular, unset in prod, and an early full-return that also drops
	// bridging): this is a set that suppresses ONLY the wake. Empty ⇒ no
	// author-based suppression (the content guards still apply).
	BotAccountIDs []string
	// History fetches the ticket's recent comments so follow-up nudges (and
	// the F3 respawn prompt, which reuses the nudge message) include the prior
	// conversation (F4, plan ka-0000007401). Nil ⇒ nudges carry only the
	// triggering comment.
	History JiraCommentHistoryClient
	// InboundCommentDLQ, when non-nil, upgrades the inbound comment-mirror write
	// from bounded-retry-then-drop to bounded-retry-then-QUEUE: a transient-class
	// terminal add_comment failure is enqueued to a durable, state-backed DLQ and
	// replayed by a background loop when the daemon recovers, so a daemon outage
	// longer than the inline retry budget no longer silently drops a human
	// follow-up from the bead conversation (epic ka-0000007401 — Slack parity with
	// thread_reply_dlq.go). Nil preserves the pre-DLQ behavior exactly (count +
	// drop). See jira_inbound_comment_dlq.go.
	InboundCommentDLQ *jiraInboundCommentDLQ
	// NonActionableStatuses is the set of Jira status NAMES (case-insensitive)
	// that, when a ticket transitions INTO one, should make gardenio reconsider
	// any in-flight task bead for that ticket (ka-0000005172). Empty ⇒ the
	// conservative built-in default (defaultJiraNonActionableStatuses). Populated
	// in prod from JIRA_NONACTIONABLE_STATUSES. See jira_status_transition.go.
	NonActionableStatuses []string
	Logger                *slog.Logger
}

// JiraWebhookBeadClient is the subset of the beads API needed by the Jira
// webhook handler. It mirrors GitLabBeadClient with the methods required
// to look up beads by jira_key, add comments, and update fields. Task-bead
// listing includes CLOSED beads: a follow-up comment usually arrives after
// the agent finished and closed its task, and that is exactly the F3
// re-spawn case (same reasoning as the GitLab webhook handlers).
type JiraWebhookBeadClient interface {
	ListTaskBeadsAll(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	// ListTaskBeadsForJiraKey narrows the lookup to one ticket server-side, so a
	// single comment event does not drag the whole task corpus into memory
	// (ka-0000007917).
	ListTaskBeadsForJiraKey(ctx context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBead patches mutable columns (status) and fields in a single atomic
	// daemon write — used by the status-transition-to-non-actionable path to
	// pause an unattended task bead (status→blocked) and stamp the
	// jira_nonactionable flag in one event. See jira_status_transition.go.
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// jiraWebhookEvent is the top-level Jira webhook payload.
type jiraWebhookEvent struct {
	WebhookEvent string        `json:"webhookEvent"`
	Comment      *jiraComment  `json:"comment"`
	Issue        *jiraIssueRef `json:"issue"`
	// Changelog carries the field-level diff on a jira:issue_updated event.
	// The webhook handler reads it to fire only when an edit NEWLY introduced a
	// @gardenio mention in the description (H5, epic ka-0000007401) — a plain
	// mention already present before the edit, or an edit to any other field,
	// must never re-spawn (the re-fire guard).
	Changelog *jiraChangelog `json:"changelog"`
	// User is the actor who performed the update on a jira:issue_updated event
	// (the human who edited the description and added the mention). It attributes
	// the H5 spawn to the editor — the person who wrote @gardenio — and gates the
	// bot-editor loop guard. Absent on comment/create events, which carry the
	// author on the comment / reporter on the issue instead.
	User *jiraWebhookUser `json:"user"`
}

// jiraChangelog is the Jira webhook changelog: an ordered list of per-field
// before/after items describing what an edit changed.
type jiraChangelog struct {
	Items []jiraChangelogItem `json:"items"`
}

// jiraChangelogItem is a single field change in a jira:issue_updated changelog.
// FromString/ToString are the human-readable (plain-text) values Jira renders
// for the field before and after the edit; for the description field they are
// the flattened text of the ADF body, which is exactly what @gardenio detection
// runs on.
type jiraChangelogItem struct {
	Field      string `json:"field"`
	FieldID    string `json:"fieldId"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// descriptionChange returns the description field's before/after text and true
// when this changelog contains a description edit. Jira identifies the field by
// fieldId "description" (Cloud) or field name "description" (Server).
func (c *jiraChangelog) descriptionChange() (from, to string, ok bool) {
	if c == nil {
		return "", "", false
	}
	for _, item := range c.Items {
		if item.FieldID == "description" || item.Field == "description" {
			return item.FromString, item.ToString, true
		}
	}
	return "", "", false
}

// jiraComment represents a Jira issue comment from the webhook payload.
// Body is json.RawMessage to handle both plain text strings (Jira Server)
// and ADF objects (Jira Cloud).
type jiraComment struct {
	ID     string          `json:"id"`
	Body   json.RawMessage `json:"body"`
	Author struct {
		DisplayName string `json:"displayName"`
		AccountID   string `json:"accountId"`
		// EmailAddress is present on Jira webhook author payloads when the
		// instance exposes it (visibility depends on the user's profile
		// privacy setting). Used as the primary key to resolve the commenter
		// to a fleet user bead, since user beads store email but rarely the
		// opaque accountId (ka-0000004473).
		EmailAddress string `json:"emailAddress"`
	} `json:"author"`
}

// BodyText extracts the plain text from the comment body, handling both
// plain text strings and ADF (Atlassian Document Format) JSON objects.
func (c *jiraComment) BodyText() string {
	return jiraCommentBodyToText(c.Body)
}

// jiraIssueRef is the issue reference in the webhook payload.
type jiraIssueRef struct {
	Key    string                 `json:"key"`
	Fields jiraWebhookIssueFields `json:"fields"`
}

// jiraWebhookIssueFields are the issue fields the webhook handler reads.
// Description and Reporter are carried for jira:issue_created events, where an
// @gardenio mention in the DESCRIPTION (authored by the reporter) spawns an
// agent (H2, epic ka-0000007401). Description is json.RawMessage to handle both
// plain-text (Jira Server) and ADF (Jira Cloud) bodies, mirroring jiraComment.
type jiraWebhookIssueFields struct {
	Summary     string           `json:"summary"`
	Description json.RawMessage  `json:"description"`
	Reporter    *jiraWebhookUser `json:"reporter"`
}

// jiraWebhookUser is a Jira user in the webhook payload (issue reporter). It
// mirrors the comment author fields so the reporter resolves to a fleet human
// via the same UserDirectory keys (accountId, then email — see ka-0000004473).
type jiraWebhookUser struct {
	DisplayName  string `json:"displayName"`
	AccountID    string `json:"accountId"`
	EmailAddress string `json:"emailAddress"`
}

// nudgeThrottle provides per-key rate limiting for nudge delivery.
// First call for a key is allowed immediately; subsequent calls within the
// cooldown window are suppressed.
type nudgeThrottle struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	cooldown time.Duration
}

func newNudgeThrottle(cooldown time.Duration) *nudgeThrottle {
	return &nudgeThrottle{
		seen:     make(map[string]time.Time),
		cooldown: cooldown,
	}
}

// Allow returns true if the key is not throttled (first call or cooldown expired).
// When allowed, records the current time for the key.
func (t *nudgeThrottle) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.seen[key]; ok && time.Since(last) < t.cooldown {
		return false
	}
	t.seen[key] = time.Now()
	return true
}

// JiraWebhookHandler returns an http.Handler that processes Jira webhook events.
// Currently handles comment_created events: matches the issue key to a bead,
// adds the comment, sets jira_has_comments, and nudges the assigned agent.
// Nudges are throttled per bead to prevent spam from rapid successive comments.
func JiraWebhookHandler(cfg JiraWebhookConfig) http.Handler {
	logger := cfg.Logger
	cooldown := cfg.NudgeCooldown
	if cooldown == 0 {
		cooldown = defaultNudgeCooldown
	}
	throttle := newNudgeThrottle(cooldown)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// started times this delivery for the handling-latency histogram
		// (webhook_latency.go). Taken as the very first statement so the
		// measurement covers the whole handler — including HMAC verification
		// and the body read, which is where a reject-path regression would show.
		started := time.Now()
		// Every terminal path records exactly one outcome on
		// gardenio_jira_webhook_deliveries_total (jira_webhook_deadman.go) so a
		// still-arriving-but-rejected stream (hmac_failed/bad_request) is
		// distinguishable from silence, and the deadman monitor can tell a
		// working hook from a dark one. See bug ka-0000000426.
		if r.Method != http.MethodPost {
			recordWebhookOutcome(webhookOutcomeBadRequest, started)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read body for HMAC verification. Bound the read with MaxBytesReader so
		// an unbounded body cannot exhaust memory before signature verification.
		// See AUDIT-2026-04-18 §H2 (ka-0000002162).
		r.Body = http.MaxBytesReader(w, r.Body, jiraMaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			recordWebhookOutcome(webhookOutcomeBadRequest, started)
			logger.Error("jira-webhook: failed to read body", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Verify HMAC signature — fail-closed unless dev mode is explicitly enabled.
		// A missing secret or a bad signature is recorded as hmac_failed (its own
		// signal): a wrong Secret in the Jira console looks like a working hook
		// from Jira's side, so it must NOT read as a healthy, receiving path.
		if cfg.DevMode {
			logger.Debug("jira-webhook: dev mode enabled, skipping HMAC verification")
		} else if cfg.WebhookSecret == "" {
			recordWebhookOutcome(webhookOutcomeHMACFailed, started)
			logger.Error("jira-webhook: JIRA_WEBHOOK_SECRET not configured and dev mode not enabled, rejecting request")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		} else if !verifyJiraHMAC(body, r.Header.Get("X-Hub-Signature"), cfg.WebhookSecret) {
			recordWebhookOutcome(webhookOutcomeHMACFailed, started)
			logger.Warn("jira-webhook: HMAC verification failed")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		var event jiraWebhookEvent
		if err := json.Unmarshal(body, &event); err != nil {
			recordWebhookOutcome(webhookOutcomeBadRequest, started)
			logger.Error("jira-webhook: failed to decode body", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		switch event.WebhookEvent {
		case "comment_created", "comment_updated":
			handleJiraCommentWebhook(r.Context(), event, cfg, throttle, logger)
			recordWebhookOutcome(webhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","event":"%s"}`, event.WebhookEvent)
		case "jira:issue_created":
			handleJiraIssueCreatedWebhook(r.Context(), event, cfg, logger)
			recordWebhookOutcome(webhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","event":"%s"}`, event.WebhookEvent)
		case "jira:issue_updated":
			handleJiraIssueUpdatedWebhook(r.Context(), event, cfg, logger)
			// A jira:issue_updated also carries a STATUS transition in its
			// changelog. Independently of the mention path above, reconsider any
			// in-flight task bead when the ticket moved INTO a non-actionable
			// status (Deprioritized/Cancelled/Won't Do/Done/Closed) — ka-0000005172.
			handleJiraStatusTransition(r.Context(), event, cfg, throttle, logger)
			recordWebhookOutcome(webhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","event":"%s"}`, event.WebhookEvent)
		case "comment_deleted":
			// A deleted comment is a deliberate no-op (GAP 5, epic
			// ka-0000007401). The bead conversation is an append-only mirror —
			// there is no DeleteComment on JiraWebhookBeadClient and Slack
			// parity does not retract a bridged bead comment when its source
			// message is deleted, so nothing is retracted here. The reply anchor
			// (jira_comment_id) is likewise left untouched: clearing it would
			// expand the guarded write surface (F5 spine) for a rare, cosmetic,
			// self-healing degradation (the next comment re-anchors it), and a
			// deleted-but-still-referenced comment id degrades a later gb jira
			// reply only to a flat post — the same graceful fallback an agent
			// without an anchor already gets. This branch exists so the outcome
			// is an EXPLICIT, logged ignore rather than an anonymous fall-through
			// to default, making a deleted mention comment traceable. It still
			// records `ignored` (an authenticated, decoded delivery that proves
			// the receive path is alive for the deadman monitor), not a distinct
			// outcome.
			handleJiraCommentDeletedWebhook(event, logger)
			recordWebhookOutcome(webhookOutcomeIgnored, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"ignored","event":"%s"}`, event.WebhookEvent)
		default:
			recordWebhookOutcome(webhookOutcomeIgnored, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"ignored","event":"%s"}`, event.WebhookEvent)
		}
	})
}

// handleJiraCommentWebhook processes a Jira comment_created or comment_updated
// webhook event. It first checks for @gardenio mentions (spawning agents if
// detected), then matches the issue key to beads via jira_key field, adds the
// comment, sets jira_has_comments + jira_comment_id, and delivers the follow-up
// to the assigned agent (throttled per bead): a live agent is nudged; a stopped
// one is re-spawned via the resolver so the conversation continues (F3).
func handleJiraCommentWebhook(ctx context.Context, event jiraWebhookEvent, cfg JiraWebhookConfig, throttle *nudgeThrottle, logger *slog.Logger) {
	daemon, mention := cfg.Daemon, cfg.MentionHandler
	if event.Comment == nil || event.Issue == nil {
		logger.Debug("jira-webhook: comment event missing comment or issue")
		return
	}

	jiraKey := event.Issue.Key
	if jiraKey == "" {
		logger.Debug("jira-webhook: comment event missing issue key")
		return
	}

	logger.Info("jira-webhook: comment event",
		"event", event.WebhookEvent,
		"jira_key", jiraKey,
		"author", event.Comment.Author.DisplayName,
		"comment_id", event.Comment.ID)

	// Never react to the bridge's own comments: agent replies and squawk
	// forwards are posted under the bot account and echoed back by this
	// webhook. Nudging is merely noisy, but the F3 re-spawn would resurrect a
	// stopped agent off its own goodbye message in a loop.
	if cfg.BotAccountID != "" && event.Comment.Author.AccountID == cfg.BotAccountID {
		logger.Debug("jira-webhook: ignoring bridge's own comment",
			"jira_key", jiraKey, "comment_id", event.Comment.ID)
		return
	}

	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:jira-bridge. JIRA comment author is logged above and forwarded
	// into the comment body downstream.

	// Check for @gardenio mention and spawn agent if detected.
	// This runs before comment bridging so the spawned agent exists
	// when the comment is forwarded to existing beads.
	mentionHandled := false
	if mention != nil {
		mentionHandled = mention.HandleMention(ctx, event)
	}

	// Task beads for THIS ticket only, all statuses — a follow-up usually lands
	// after the task closed. Server-side jira_key predicate: listing the whole
	// task corpus per comment event is a memory spike AND silently lossy past
	// the list cap (ka-0000007917, advice ka-0000003555).
	beads, err := daemon.ListTaskBeadsForJiraKey(ctx, jiraKey)
	if err != nil {
		logger.Error("jira-webhook: failed to list beads", "jira_key", jiraKey, "error", err)
		return
	}

	// Format the comment for the bead.
	bodyText := event.Comment.BodyText()
	commentText := fmt.Sprintf("**Jira comment** by %s:\n\n%s",
		event.Comment.Author.DisplayName, bodyText)

	matched := false
	for _, bead := range beads {
		if bead.Fields["jira_key"] != jiraKey {
			continue
		}
		matched = true

		// Add comment to the bead. Retry transient daemon errors so a momentary
		// daemon blip (rollout / restart / 5xx) does not silently drop a human
		// follow-up from the bead conversation — the webhook returns 200 and
		// there is no comment re-sync, so a dropped mirror is permanent. Slack's
		// inbound reply path closed the same gap (ka-0000000612).
		if err := retryJiraInboundWrite(ctx, "add_comment", logger, func() error {
			return daemon.AddComment(ctx, bead.ID, "jira-bridge", commentText)
		}); err != nil {
			jiraInboundCommentWriteFailures.WithLabelValues("add_comment").Inc()
			logger.Error("jira-webhook: failed to add comment to bead",
				"bead", bead.ID, "jira_key", jiraKey, "error", err)
			// Terminal failure after the bounded retry. Only a TRANSIENT-class
			// failure is worth queuing: a daemon down/overloaded long enough to
			// exhaust the inline retry will likely recover, so the DLQ replays the
			// mirror later. A deterministic failure (a 4xx validation rejection)
			// would fail identically on every replay, so it is dropped here
			// (logged + counted) instead of churning the queue. The DLQ replay
			// de-dups against existing comments, so a transient failure that was
			// really a lost response replays as a skip rather than a duplicate.
			// A nil InboundCommentDLQ preserves the pre-DLQ drop behavior.
			if isTransientDaemonError(err) {
				cfg.InboundCommentDLQ.enqueue(JiraInboundCommentDLQEntry{
					BeadID:         bead.ID,
					JiraKey:        jiraKey,
					JiraComment:    event.Comment.ID,
					CommentText:    commentText,
					SetHasComments: true,
				})
			} else {
				jiraInboundCommentDLQDropped.WithLabelValues("nontransient").Inc()
			}
			continue
		}

		logger.Info("jira-webhook: added comment to bead",
			"bead", bead.ID, "jira_key", jiraKey,
			"author", event.Comment.Author.DisplayName)

		// Set jira_has_comments and record the originating comment id so a
		// later `gb jira reply` can thread to the comment that triggered the
		// wake (F3 — previously the id was only logged).
		fields := map[string]string{"jira_has_comments": "true"}
		if event.Comment.ID != "" {
			fields["jira_comment_id"] = event.Comment.ID
		}
		// Retry transient errors like the comment mirror above: a dropped
		// jira_comment_id leaves a stale reply anchor so a later gb jira reply
		// threads to the wrong (older) comment. This is NOT enqueued to the DLQ
		// on terminal failure: the comment itself already landed above (so no
		// conversation content is lost), and a stale anchor is a cosmetic,
		// self-healing degradation — the next comment on the ticket re-anchors it,
		// and a gb jira reply with a stale/absent anchor gracefully falls back to
		// a flat post. The DLQ is reserved for the data-loss case (a dropped
		// comment mirror). Kept observable via the failure counter.
		if err := retryJiraInboundWrite(ctx, "set_fields", logger, func() error {
			return daemon.UpdateBeadFields(ctx, bead.ID, fields)
		}); err != nil {
			jiraInboundCommentWriteFailures.WithLabelValues("set_fields").Inc()
			logger.Error("jira-webhook: failed to set jira_has_comments",
				"bead", bead.ID, "error", err)
		}

		// Deliver the follow-up to the assigned agent (throttled per bead to
		// prevent spam). Skip if a mention was already handled — the mention
		// handler took care of spawning/nudging the agent for this comment.
		if bead.Assignee == "" || mentionHandled {
			continue
		}
		// Never wake an agent for GitLab's own "mentioned this issue in a
		// merge request" crosslink (ka-0000005445). These are posted by the
		// GitLab⇄Jira integration, not by a human asking for anything, and
		// they arrive unprompted whenever ANY branch anywhere cites the key —
		// including a stale or mistaken citation in an unrelated MR. Combined
		// with the deliberately all-statuses lookup above (a follow-up usually
		// lands after the task closed), an F3 respawn off one resurrects a
		// finished agent that has nothing to do, and the loop is
		// self-sustaining: the woken agent files a bug, the fix branch cites
		// the ticket, that crosslink wakes it again. Observed on DEVOPS-738,
		// which respawned the same agent 3x in 64 minutes off its own
		// predecessors' MRs. The comment is still bridged to the bead above,
		// so the crosslink stays visible — only the wake is suppressed. A real
		// human follow-up (and an @gardenio re-mention, handled earlier) is
		// unaffected.
		if isGitLabCrosslinkComment(bodyText) {
			logger.Info("jira-webhook: skipping wake for GitLab crosslink comment",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey,
				"comment_id", event.Comment.ID)
			continue
		}
		// Never wake an agent for a comment this bridge posted ITSELF
		// (ka-0000004998) — the claim / "Automated MR created" / "MR merged"
		// notices from jira_sync. Same self-sustaining shape as the crosslink
		// case above: a bead re-open re-fires the notice, the notice wakes the
		// assignee, the woken agent cannot clear the condition that caused the
		// re-open, and it repeats. The BotAccountID guard upstream would be the
		// natural place to catch this but is unset in prod and singular where
		// gardenio has two bot accounts — see isBridgeGeneratedComment.
		if isBridgeGeneratedComment(bodyText) {
			logger.Info("jira-webhook: skipping wake for bridge-generated comment",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey,
				"comment_id", event.Comment.ID)
			continue
		}
		// Never wake an agent for a comment authored by one of gardenio's own
		// bot Jira accounts (ka-0000003581) — the 4th member of this self-wake
		// family and the first that content classification CANNOT fix. A
		// `gb jira reply` posts ARBITRARY free-text prose under the bot
		// account, so no marker list (isBridgeGeneratedComment above) can ever
		// enumerate it; only the AUTHOR identity is stable and bounded. The
		// loop is self-sustaining because the woken agent's natural response —
		// replying on the ticket — is itself an unguarded free-text comment
		// under the same bot account. Observed live on PE-7289: comment 115851
		// (a fixVersion write-up, not a template) F3-respawned the assignee of
		// a task closed 5 months earlier, 4 seconds after posting.
		//
		// Suppress ONLY the wake, at the same point as the two content guards,
		// so the comment still mirrors to the bead above and stays visible. A
		// HUMAN follow-up and an @gardenio re-mention are authored by human
		// accounts and so pass through untouched. Distinct from the
		// early-return BotAccountID guard upstream (singular, unset in prod);
		// this is a SET seeded with both gardenio bot accounts.
		if isBotAuthoredComment(event.Comment.Author.AccountID, cfg.BotAccountIDs) {
			logger.Info("jira-webhook: skipping wake for bot-authored comment",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey,
				"comment_id", event.Comment.ID,
				"author_account_id", event.Comment.Author.AccountID)
			continue
		}
		throttleKey := "jira-comment-nudge:" + bead.ID
		if throttle != nil && !throttle.Allow(throttleKey) {
			logger.Info("jira-webhook: nudge throttled",
				"bead", bead.ID, "agent", bead.Assignee)
			continue
		}
		// Honor a --fresh flag on the follow-up comment (ka-0000008188, Slack
		// parity): "@gardenio --fresh <text>" asks to continue this ticket
		// conversation with a CLEAN context. Strip the token from the body used to
		// build the nudge so it never leaks into the prompt the (respawned) agent
		// reads, and thread the flag into deliverJiraFollowUp so a RESPAWN starts
		// clean instead of resuming the prior transcript. On the live-agent path
		// the flag is inert (a running pod cannot forget in-memory context); the
		// resolver logs it and delivers a normal nudge.
		fresh, cleanedBody := parseFreshFlag(event.Comment.BodyText())
		// Include the ticket's recent conversation and the operator stamped on
		// the task bead so the (possibly respawned) agent replies in context
		// and addresses the right human (F4, plan ka-0000007401).
		history := fetchJiraCommentHistory(ctx, cfg.History, logger, jiraKey, event.Comment.ID)
		msg := buildJiraCommentNudgeMessage(event, bead, history, cleanedBody)
		deliverJiraFollowUp(ctx, cfg, bead, jiraKey, event.Comment.ID, msg, fresh, logger)
	}

	if !matched {
		logger.Debug("jira-webhook: no bead found for Jira key", "jira_key", jiraKey)
	}
}

// handleJiraIssueCreatedWebhook processes a jira:issue_created event. It runs
// @gardenio mention detection against the new issue's DESCRIPTION (authored by
// the reporter) and, when a mention is found, spawns an agent via the same
// mention path used for comment mentions (H2, epic ka-0000007401). Without
// this, a mention written in a ticket description — rather than a comment —
// was silently dropped: the JQL poller mirrors the issue to a bead but does no
// mention detection.
//
// The description is adapted into a synthetic comment so the entire existing
// spawn path is reused unchanged: the deny-by-default project gate (PE/DEVOPS
// allow, FSM hard-block), the open-mention dedup, human-operator resolution,
// and attribution all behave exactly as they do for a comment mention. There
// is no originating comment id (the mention is the description), so replies
// post flat — the same behavior as any first reply on a freshly-spawned ticket.
func handleJiraIssueCreatedWebhook(ctx context.Context, event jiraWebhookEvent, cfg JiraWebhookConfig, logger *slog.Logger) {
	mention := cfg.MentionHandler
	if mention == nil {
		return
	}
	if event.Issue == nil || event.Issue.Key == "" {
		logger.Debug("jira-webhook: issue_created event missing issue or key")
		return
	}
	if len(event.Issue.Fields.Description) == 0 {
		logger.Debug("jira-webhook: issue_created has no description",
			"jira_key", event.Issue.Key)
		return
	}

	// Never react to an issue the bot itself created (e.g. a mirror or an
	// agent-filed ticket) — its description could legitimately contain
	// @gardenio and would otherwise spawn an agent off the bridge's own write.
	reporter := event.Issue.Fields.Reporter
	if cfg.BotAccountID != "" && reporter != nil && reporter.AccountID == cfg.BotAccountID {
		logger.Debug("jira-webhook: ignoring bot-reported issue",
			"jira_key", event.Issue.Key)
		return
	}

	logger.Info("jira-webhook: issue_created event",
		"jira_key", event.Issue.Key,
		"reporter", jiraReporterDisplayName(reporter))

	// Adapt the description into a synthetic comment authored by the reporter so
	// HandleMention runs the full mention pipeline (gate → dedup → resolve →
	// spawn) unchanged. No comment ID: the mention originates in the
	// description, not a comment, so replies post flat.
	synthetic := jiraWebhookEvent{
		WebhookEvent: event.WebhookEvent,
		Issue:        event.Issue,
		Comment: &jiraComment{
			Body: event.Issue.Fields.Description,
		},
	}
	if reporter != nil {
		synthetic.Comment.Author.DisplayName = reporter.DisplayName
		synthetic.Comment.Author.AccountID = reporter.AccountID
		synthetic.Comment.Author.EmailAddress = reporter.EmailAddress
	}

	if handled := mention.HandleMention(ctx, synthetic); !handled {
		logger.Debug("jira-webhook: issue_created description had no actionable mention",
			"jira_key", event.Issue.Key)
	}
}

// handleJiraIssueUpdatedWebhook processes a jira:issue_updated event. It fires
// the mention pipeline only when the edit NEWLY introduced a @gardenio mention in
// the DESCRIPTION (H5, epic ka-0000007401) — people commonly forget to @gardenio
// at creation and add it in a later edit, which jira:issue_created never sees.
//
// The re-fire guard is the reason this reads the changelog rather than the
// current description: an issue_updated fires on EVERY field edit (status,
// labels, assignee, a description tweak that already contained @gardenio). Firing
// on the current description would re-spawn on every subsequent edit of an
// already-mentioned ticket. So the handler requires (1) a description field in
// the changelog and (2) the mention present in the new text AND absent from the
// old — i.e. this specific edit is what added it. The open-mention dedup in
// HandleMention is a second line of defense, but the changelog diff is what
// keeps unrelated edits (and re-edits of a mentioned description) from ever
// reaching the spawn path.
//
// The new description is adapted into a synthetic comment authored by the
// editor so the entire existing spawn path is reused unchanged: the
// deny-by-default project gate (PE/DEVOPS allow, FSM hard-block), open-mention
// dedup, human-operator resolution, and attribution all behave exactly as they
// do for a comment or issue_created mention. There is no originating comment id
// (the mention is the description), so replies post flat.
func handleJiraIssueUpdatedWebhook(ctx context.Context, event jiraWebhookEvent, cfg JiraWebhookConfig, logger *slog.Logger) {
	mention := cfg.MentionHandler
	if mention == nil {
		return
	}
	if event.Issue == nil || event.Issue.Key == "" {
		logger.Debug("jira-webhook: issue_updated event missing issue or key")
		return
	}

	oldText, newText, ok := event.Changelog.descriptionChange()
	if !ok {
		logger.Debug("jira-webhook: issue_updated has no description change — ignoring",
			"jira_key", event.Issue.Key)
		return
	}

	// The re-fire guard: only act when THIS edit introduced the mention.
	// A mention already present before the edit (or absent from the new text)
	// must never re-spawn. Detection uses the same trigger the mention handler
	// applies, so an @gardenio that survived the edit unchanged is a no-op.
	if !mention.MentionNewlyIntroduced(oldText, newText) {
		logger.Debug("jira-webhook: issue_updated description edit did not newly add a mention — ignoring",
			"jira_key", event.Issue.Key)
		return
	}

	// Never react to an edit the bot itself made (loop/self-trigger guard): a
	// bridge write that touches the description could carry @gardenio and would
	// otherwise spawn an agent off the bridge's own edit.
	editor := event.User
	if cfg.BotAccountID != "" && editor != nil && editor.AccountID == cfg.BotAccountID {
		logger.Debug("jira-webhook: ignoring bot-authored issue edit",
			"jira_key", event.Issue.Key)
		return
	}

	logger.Info("jira-webhook: issue_updated event newly added a mention",
		"jira_key", event.Issue.Key,
		"editor", jiraReporterDisplayName(editor))

	// Adapt the edited description into a synthetic comment authored by the
	// editor so HandleMention runs the full mention pipeline (gate → dedup →
	// resolve → spawn) unchanged. The new description text carries the mention;
	// no comment ID, so replies post flat (same as issue_created).
	synthetic := jiraWebhookEvent{
		WebhookEvent: event.WebhookEvent,
		Issue:        event.Issue,
		Comment: &jiraComment{
			Body: descriptionBodyForMention(event.Issue.Fields.Description, newText),
		},
	}
	if editor != nil {
		synthetic.Comment.Author.DisplayName = editor.DisplayName
		synthetic.Comment.Author.AccountID = editor.AccountID
		synthetic.Comment.Author.EmailAddress = editor.EmailAddress
	}

	if handled := mention.HandleMention(ctx, synthetic); !handled {
		logger.Debug("jira-webhook: issue_updated description had no actionable mention after diff",
			"jira_key", event.Issue.Key)
	}
}

// descriptionBodyForMention selects the comment body used to re-run mention
// detection for an issue_updated edit. It prefers the full ADF description from
// the issue payload (so the spawned agent gets code blocks, links and lists via
// the H3 markdown render), falling back to the changelog's flattened plain-text
// (toString) when the webhook omitted the issue's description field. Returned as
// a json.RawMessage matching jiraComment.Body: a raw ADF object passes through;
// plain text is JSON-encoded as a string (the Jira Server body shape).
func descriptionBodyForMention(adf json.RawMessage, plain string) json.RawMessage {
	if len(adf) > 0 {
		return adf
	}
	b, err := json.Marshal(plain)
	if err != nil {
		return nil
	}
	return b
}

// handleJiraCommentDeletedWebhook processes a comment_deleted event (GAP 5,
// epic ka-0000007401). It intentionally performs NO bead mutation — see the
// switch arm in JiraWebhookHandler for the full rationale (append-only mirror,
// no retraction, anchor left intact). Its sole job is to emit one
// operator-visible log line so a deleted comment — in particular a deleted
// @gardenio mention comment — is traceable, rather than vanishing into the
// anonymous `default` ignore. The event still counts as an authenticated,
// decoded delivery for the deadman monitor (the caller records `ignored`).
func handleJiraCommentDeletedWebhook(event jiraWebhookEvent, logger *slog.Logger) {
	// A well-formed Jira comment_deleted payload carries the issue and the
	// deleted comment's id/author; tolerate either being absent so a sparse
	// or malformed payload is still a clean, logged no-op.
	var jiraKey, commentID, author string
	if event.Issue != nil {
		jiraKey = event.Issue.Key
	}
	if event.Comment != nil {
		commentID = event.Comment.ID
		author = event.Comment.Author.DisplayName
	}
	logger.Info("jira-webhook: comment_deleted — no-op (bead conversation is append-only; not retracting, reply anchor left intact)",
		"jira_key", jiraKey, "comment_id", commentID, "author", author)
}

// jiraReporterDisplayName returns the reporter's display name for logging, or a
// stable placeholder when the reporter is absent from the payload.
func jiraReporterDisplayName(reporter *jiraWebhookUser) string {
	if reporter == nil || reporter.DisplayName == "" {
		return "unknown"
	}
	return reporter.DisplayName
}

// deliverJiraFollowUp routes a follow-up comment to the task bead's assigned
// agent. With a resolver wired it re-spawns a stopped agent (same-name spawn →
// session resume) instead of letting the conversation die; without one it
// preserves the legacy nudge-only behavior. The F3 re-spawn is project-gated
// (F5 spine): a non-writable project (FSM in every mode) downgrades to
// nudge-only, so no agent is ever resurrected for a blocked project.
//
// fresh carries the follow-up's --fresh flag (ka-0000008188): when a RESPAWN
// results, the resolver starts a clean conversation instead of resuming the
// prior transcript. It has no effect on the legacy nudge-only path (a live pod
// cannot forget its in-memory context).
func deliverJiraFollowUp(ctx context.Context, cfg JiraWebhookConfig, bead *beadsapi.BeadDetail, jiraKey, commentID, msg string, fresh bool, logger *slog.Logger) {
	if cfg.Resolver != nil && jiraFollowUpProjectAllowed(cfg, jiraKey) {
		// The task bead's project label was stamped at mention-spawn time via
		// resolveProjectFromJiraKey, so a respawn lands in the same (possibly
		// prefix-mapped, G3 epic ka-0000007401) boat project as the original.
		project := projectFromLabels(bead.Labels)
		err := cfg.Resolver.ResolveAndNudgeFollowUp(ctx, bead.Assignee, jiraKey, project, bead.ID, commentID, msg, fresh)
		if err != nil {
			logger.Error("jira-webhook: failed to deliver follow-up via resolver",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey, "error", err)
		} else {
			logger.Info("jira-webhook: follow-up delivered via resolver",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey)
		}
		return
	}

	if cfg.Nudge == nil {
		return
	}
	if err := cfg.Nudge(ctx, bead.Assignee, msg); err != nil {
		logger.Error("jira-webhook: failed to nudge agent",
			"bead", bead.ID, "agent", bead.Assignee, "error", err)
	} else {
		logger.Info("jira-webhook: nudged agent",
			"bead", bead.ID, "agent", bead.Assignee)
	}
}

// gitLabCrosslinkMarkers are the object phrasings GitLab's Jira integration
// uses in its "<user> mentioned this issue in <object> of <project>" notice.
// Matching the object as well as the verb keeps a human who happens to write
// "I mentioned this issue in standup" from being classified as automation.
var gitLabCrosslinkMarkers = []string{
	"mentioned this issue in a merge request",
	"mentioned this issue in a commit",
	"mentioned this issue in a branch",
	"mentioned this issue in an issue",
}

// bridgeGeneratedMarkers are the leading phrasings of the comments this bridge
// posts itself, one per withAttribution call site in jira_sync.go: the claim
// notice, the MR-created notice, the MR-merged notice, and the bead-closure
// notice. Each is matched as a PREFIX of the flattened body (the attribution
// line is appended after a blank line, so the generated sentence always leads)
// to keep a human who quotes one back — "why did "Automated MR created" fire
// twice?" — from being classified as automation.
//
// Keep this list in sync with jira_sync.go: a missed call site is not a cosmetic
// gap but a live wake loop. The bead-closure entry was exactly that miss
// (ka-0000004442) — see the loop it closes on isBridgeGeneratedComment.
//
// The claim notice needs a SUFFIX too, not just the "Gardenio agent " prefix:
// the agent name sits in the middle ("Gardenio agent <name> is working on this
// issue."), and a bare prefix match would also swallow a human who simply opens
// by addressing the agent — "Gardenio agent please take another look". That is a
// real follow-up, and silently dropping it is the worst failure this guard can
// have, so the claim entry is matched as prefix AND suffix.
//
// The closure notice is prefix-ONLY because its tail varies: the bead ID is
// interpolated and an optional " MR: <url>" is appended when the bead carries
// one, so there is no stable suffix to pin. The prefix carries the specificity
// instead — a human does not plausibly open a comment with "task bead closed in
// beads system (bead ".
var bridgeGeneratedMarkers = []struct{ prefix, suffix string }{
	{"gardenio agent ", " is working on this issue."}, // jira_sync.go:504
	{"automated mr created:", ""},                     // jira_sync.go:539
	{"mr merged — transitioning to review.", ""},      // jira_sync.go:556
	{"task bead closed in beads system (bead ", ""},   // jira_sync.go:838
}

// isBridgeGeneratedComment reports whether a Jira comment is one this bridge
// posted itself (ka-0000004998) — the sibling of the GitLab crosslink loop
// below, with the same shape and a different author.
//
// Why content and not author identity: the BotAccountID guard upstream is the
// obvious defence and it does not work. In prod it is UNSET (helm
// values.yaml botAccountID: "" and the deployment only emits
// JIRA_BOT_ACCOUNT_ID when truthy), so the check short-circuits to false and is
// effectively dead code. It is also singular, while gardenio posts under TWO
// accounts — 712020:66bb0993-… ("Gardenio Agent") and 712020:a2f3e121-…
// ("Automated Gardenio Agent"), both already enumerated as gardenio bots in the
// outbox Automation rules. Content-classifying needs no operator config, so it
// holds even while the account config stays empty.
//
// The closure notice closes the TIGHTEST loop in this family (ka-0000004442),
// and it needs no external input at all: an agent finishes and closes its task
// bead, the SSE close event drives jira_sync.syncBeadClosure to post "Task bead
// closed in beads system (bead <ID>).", and this webhook wakes the very agent
// that just closed it. The woken agent has nothing to do, investigates, closes
// its own bead — which posts another closure notice. Observed live on
// DEVOPS-738, a ticket Finished since 2026-06-16.
//
// The other loop this breaks, observed end-to-end on DEVOPS-897 in three seconds: the
// gitlab-bridge reconcile sweep re-opens a task bead whose MR is still unmerged
// → jira_sync sees the re-opened bead's mr_url and posts "Automated MR created"
// → this webhook F3-respawns the assignee off the bridge's own comment. The
// woken agent cannot land the MR (protected main needs an operator), so the
// sweep re-opens the bead again and it repeats. jira_sync's own isDuplicate
// cannot damp this: syncTTL is 10 minutes and the sweep cadence is far slower.
//
// As with crosslinks, the comment is still bridged to the bead by the caller —
// only the wake is suppressed.
func isBridgeGeneratedComment(text string) bool {
	// Drop the attribution line ("Acted by <operator>") that withAttribution
	// appends, so a suffix match sees the generated sentence's real ending.
	body := text
	if idx := strings.Index(body, "\n\n"); idx >= 0 {
		body = body[:idx]
	}
	lower := strings.ToLower(strings.TrimSpace(body))
	for _, marker := range bridgeGeneratedMarkers {
		if !strings.HasPrefix(lower, marker.prefix) {
			continue
		}
		if marker.suffix == "" || strings.HasSuffix(lower, marker.suffix) {
			return true
		}
	}
	return false
}

// isBotAuthoredComment reports whether accountID belongs to the set of gardenio
// bot Jira accounts (JIRA_BOT_ACCOUNT_IDS). Comparison is case-insensitive and
// whitespace-trimmed, matching normalizeAccountIDs on the mention path so the
// same value (e.g. "712020:66bb0993-…") is recognised regardless of how the
// operator cased or padded it in the chart. An empty accountID or empty set
// yields false so the caller's other guards still apply.
func isBotAuthoredComment(accountID string, botAccountIDs []string) bool {
	if accountID == "" || len(botAccountIDs) == 0 {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(accountID))
	for _, id := range botAccountIDs {
		if strings.ToLower(strings.TrimSpace(id)) == target {
			return true
		}
	}
	return false
}

// jiraWikiLinkRE matches Jira wiki-markup inline links — [display|target] (and
// the alias-less [target]) — so they can be flattened to their display text
// before crosslink matching. The alias segment [^\]|]* stops at the first '|'
// or ']', so a link never spans into surrounding prose.
var jiraWikiLinkRE = regexp.MustCompile(`\[([^\]|]*)\|[^\]]*\]`)

// flattenJiraWikiLinks rewrites Jira wiki-markup links [display|target] to just
// their display text (dropping the |target] tail). It is a no-op on text that
// carries no wiki links — notably the ADF-flattened Jira Cloud body, which has
// none — so it is safe to run unconditionally before marker matching.
//
// This is what makes isGitLabCrosslinkComment work against the REAL prod input.
// GitLab's Jira Server crosslink notices wrap the object phrase in a link, e.g.
// "…mentioned this issue in [a commit|https://…] of [Pi Health / … |https://…]"
// so a raw substring scan for "mentioned this issue in a commit" never matches
// — the [a commit|url] brackets split the phrase (ka-0000006177). The Jira
// Cloud form arrives already flattened (adfToPlainText drops link marks), which
// is why the bug hid: the classifier's own tests, and the flattened body an
// agent reconstructed by hand, both match while the on-the-wire Server body
// does not.
func flattenJiraWikiLinks(text string) string {
	if !strings.Contains(text, "[") {
		return text
	}
	return jiraWikiLinkRE.ReplaceAllString(text, "$1")
}

// isGitLabCrosslinkComment reports whether a Jira comment is GitLab's
// automated cross-reference notice rather than something a human wrote
// (ka-0000005445). Detection is case-insensitive on the comment text after
// flattening Jira wiki-markup links (flattenJiraWikiLinks); the marker phrases
// are generated by GitLab's Jira integration, so they are stable and not
// user-authored.
//
// The wiki-link flattening is load-bearing on prod. GitLab's Jira Server
// integration posts the crosslink as wiki markup with the object phrase as a
// link — "…mentioned this issue in [a merge request|url] of …" — so matching
// the raw body missed every real crosslink and the wake fired anyway even with
// this guard deployed (ka-0000006177). The Jira Cloud body arrives pre-
// flattened (ADF drops link marks), so the flatten is a no-op there.
//
// This is deliberately narrower than "was the author a bot": the crosslinks
// arrive under whichever account owns the GitLab⇄Jira integration (on prod,
// "Pi Health Root" — a shared human-looking account that ALSO posts legitimate
// traffic), so author identity cannot separate them. The BotAccountID guard
// upstream only covers the bridge's OWN account, which is a different identity
// and therefore never matches these.
func isGitLabCrosslinkComment(text string) bool {
	lower := strings.ToLower(flattenJiraWikiLinks(text))
	for _, marker := range gitLabCrosslinkMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// jiraFollowUpProjectAllowed applies the F5 project gate to the F3 re-spawn
// path. Re-spawning a stopped agent is an agent-lifecycle action, not an
// outbound Jira write, so it uses the SPAWN-eligibility gate (AllowSpawn) — the
// same gate the initial mention spawn uses (ka-0000001940). This keeps a GBFSM
// follow-up from being dropped merely because the outbox write toggle is off,
// while still denying re-spawns on FSM and unknown projects (deny-by-default).
// When the write policy is unwired the legacy deny-by-default gate
// (allow={PE,DEVOPS}, block={FSM}) decides. Never allow-all.
func jiraFollowUpProjectAllowed(cfg JiraWebhookConfig, jiraKey string) bool {
	if cfg.WritePolicy != nil {
		return cfg.WritePolicy.AllowSpawn(jiraKey)
	}
	return syncAllowedProject(jiraKey)
}

// buildJiraCommentNudgeMessage constructs a nudge message for an agent from a
// Jira comment, including issue key, summary, author, and comment body. The
// task bead supplies the resolved operator identity and history is the
// pre-formatted recent-comment transcript ("" ⇒ omitted) — both F4 additions
// (plan ka-0000007401) so a nudged (or F3-respawned) agent replies in context
// and addresses the right human.
//
// commentBody is the comment text to render (the caller strips any --fresh flag
// first, ka-0000008188, so the control token never leaks into the prompt). An
// empty commentBody falls back to the raw event body.
func buildJiraCommentNudgeMessage(event jiraWebhookEvent, bead *beadsapi.BeadDetail, history, commentBody string) string {
	if commentBody == "" {
		commentBody = event.Comment.BodyText()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Jira comment on %s", event.Issue.Key)
	if event.Issue.Fields.Summary != "" {
		fmt.Fprintf(&b, " %q", event.Issue.Fields.Summary)
	}
	fmt.Fprintf(&b, " by %s", event.Comment.Author.DisplayName)
	if history != "" {
		fmt.Fprintf(&b, "\nRecent comments on %s (oldest first):\n%s", event.Issue.Key, history)
	}
	fmt.Fprintf(&b, "\nComment: %s", commentBody)
	operatedBy := bead.OperatedBy
	if operatedBy == "" {
		operatedBy = bead.Fields["operated_by"]
	}
	if line := jiraOperatorPromptLine(operatedBy, bead.OperatedByDisplay); line != "" {
		fmt.Fprintf(&b, "\n%s", line)
	}
	fmt.Fprintf(&b, "\nJira key: %s", event.Issue.Key)
	fmt.Fprintf(&b, "\nBead: %s", bead.ID)
	b.WriteString("\nReply on the ticket with `gb jira reply '<text>'` (posts an internal note by default; --public is role-gated).")
	b.WriteString("\nRe-read the full ticket conversation with `gb jira thread` — the excerpt above is just the latest comment.")
	fmt.Fprintf(&b, "\nRe-read the current ticket body/status/fields with `gb jira show %s` — the body may have been edited since you were spawned.", event.Issue.Key)
	return b.String()
}

// verifyJiraHMAC verifies the HMAC-SHA256 signature of a Jira webhook payload.
// Jira sends the signature in the X-Hub-Signature header as "sha256=<hex>".
func verifyJiraHMAC(body []byte, signature, secret string) bool {
	if signature == "" {
		return false
	}

	sig := strings.TrimPrefix(signature, "sha256=")

	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}
