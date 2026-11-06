// bot_reactions.go handles Slack reaction_added events.
//
// When a user adds an emoji reaction to a Slack message, this handler checks
// whether a matching "reaction-rule" bead exists. If so, it spawns an agent
// with the configured prompt, role, and project.
//
// Reaction rules are defined as beads of type "reaction-rule" with fields:
//   - emoji    (required): Slack emoji name to match (without colons)
//   - match    (optional): text the reacted-to message must contain
//   - role     (optional): agent role to spawn (default: "crew")
//   - prompt   (required): agent prompt template with {placeholders}
//   - project  (required): project to spawn the agent in
//
// Supported prompt placeholders are extracted from the reacted-to message
// context and include {channel}, {thread_ts}, {message_ts}, {user}, and
// {message_text}. Additional context-specific placeholders may be extracted
// from the message text (e.g., MR IID from "MR !123").
package bridge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"golang.org/x/sync/singleflight"

	"gardenio/controller/internal/beadsapi"
)

// reactionRuleCache caches reaction-rule beads to avoid querying the daemon
// on every reaction event.
//
// mu guards only the in-memory fields (rules/fetchedAt) — it is NEVER held
// across the daemon fetch. A burst of reaction events that all find the cache
// stale must not serialize behind one in-flight round-trip (see
// ka-0000008004), so the refresh runs outside the lock and concurrent
// refreshers are collapsed into a single daemon call by `sf`.
type reactionRuleCache struct {
	mu        sync.Mutex
	rules     []*beadsapi.BeadDetail
	fetchedAt time.Time
	ttl       time.Duration
	sf        singleflight.Group
}

const reactionRuleCacheTTL = 2 * time.Minute

// reactionSpawnThrottle prevents duplicate spawns from the same reaction.
type reactionSpawnThrottle struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	cooldown time.Duration
}

const reactionSpawnCooldown = 10 * time.Minute

func newReactionSpawnThrottle() *reactionSpawnThrottle {
	return &reactionSpawnThrottle{
		seen:     make(map[string]time.Time),
		cooldown: reactionSpawnCooldown,
	}
}

func (t *reactionSpawnThrottle) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.seen[key]; ok && time.Since(last) < t.cooldown {
		return false
	}
	t.seen[key] = time.Now()
	return true
}

// Cleanup removes expired entries from the throttle map.
func (t *reactionSpawnThrottle) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.seen {
		if time.Since(v) > t.cooldown {
			delete(t.seen, k)
		}
	}
}

// handleReactionAdded processes a Slack reaction_added event.
func (b *Bot) handleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	// Ignore bot's own reactions.
	if ev.User == b.botUserID {
		return
	}
	// Stamp operator on every daemon call made downstream. See
	// operatorFromSlackUser for the slack→email resolution policy.
	if ev.User != "" {
		ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, ev.User))
	}

	b.logger.Debug("reaction_added",
		"user", ev.User,
		"reaction", ev.Reaction,
		"item_type", ev.Item.Type,
		"channel", ev.Item.Channel,
		"ts", ev.Item.Timestamp)

	// Only handle reactions on messages.
	if ev.Item.Type != "message" {
		return
	}

	// :bar_chart: on a bot-authored message manually triggers the human
	// exit survey. Routed before reaction-rule matching so rule-based
	// overrides can't collide with this built-in.
	if ev.Reaction == humanSurveyEmoji {
		b.handleBarChartReaction(ctx, ev)
		return
	}

	// A :black_square_for_stop: (⏹️) on the thread card is the Slack-native
	// wish-close gesture (1b-11, ka-0000001476): close the resolved wish HEAD
	// with close_reason=slack-thread-user-close, attributed to the reactor (the
	// operator stamped on ctx above). The glyph is DELIBERATELY distinct from
	// the bot's outbound ✅ done-lifecycle reaction (ka-0000002200) so the two
	// never overload one symbol. Routed here — right after :bar_chart:
	// and BEFORE loadReactionRules — so a rule bead carrying the same emoji
	// can't collide with the built-in close ON A THREAD CARD. The bot's OWN
	// 'done' lifecycle reaction (bot_reactions_lifecycle.go) can never reach
	// this branch on two counts: the ev.User == b.botUserID guard at the top of
	// this function returns for bot self-reactions, AND the bot's glyph (✅) is
	// not the wish-close glyph (⏹️).
	//
	// We only short-circuit when the reaction actually resolved to a wish head
	// (handleWishCloseReaction returns true). A ⏹️ react on an ordinary
	// (non-thread-card) message resolves to no head and falls through to the
	// legacy reaction-rule + unified trigger paths below — so a rule/trigger
	// bead keyed on black_square_for_stop still fires off-card and is not
	// silently swallowed.
	//
	// Slice 4b: the gesture is dispatched as Inbound{React, done}
	// (inbound_react.go), the same entry the Commons Done button takes.
	if isWishCloseEmoji(ev.Reaction) {
		_, err := b.inbound().Dispatch(ctx, slackReactInbound(ev, beadsapi.OperatorFromContext(ctx)))
		if err == nil {
			return
		}
		if !errors.Is(err, errReactionNotTaken) {
			b.logger.Warn("inbound: slack wish-close reaction failed", "channel", ev.Item.Channel, "ts", ev.Item.Timestamp, "error", err)
			return
		}
	}

	// Load reaction rules (cached).
	rules, err := b.loadReactionRules(ctx)
	if err != nil {
		b.logger.Error("reaction: failed to load rules", "error", err)
		// Fall through: a rule-load failure must not suppress the unified
		// trigger:slack-reaction path, which evaluates independently.
	}

	// Find legacy reaction rules matching this emoji.
	matching := matchReactionRulesByEmoji(ev.Reaction, rules)

	// Short-circuit only when neither path can act: no matching legacy rule
	// AND the unified trigger path is not wired. Avoids an unnecessary
	// fetchMessageText round-trip on the (common) bare reaction.
	if len(matching) == 0 && b.reactionTrigger == nil {
		return
	}

	// Fetch the reacted-to message text once, shared by both paths for pattern
	// matching and prompt/template interpolation.
	msgText := b.fetchMessageText(ctx, ev.Item.Channel, ev.Item.Timestamp)
	if msgText == "" {
		b.logger.Debug("reaction: empty or unfetchable message",
			"channel", ev.Item.Channel, "ts", ev.Item.Timestamp)
		return
	}

	// Unified path: buffer the event for the trigger engine (no-op when the
	// unified path is not wired). This runs in addition to — never instead of
	// — the legacy reaction-rule path below; the buffered source is configured
	// with SkipLegacy so legacy reaction-rule beads aren't double-handled.
	b.bufferReactionTrigger(ev.User, ev.Reaction, ev.Item.Channel, ev.Item.Timestamp, msgText)

	// Legacy path: check each matching rule for message pattern match.
	for _, rule := range matching {
		matchPattern := rule.Fields["match"]
		if matchPattern != "" && !strings.Contains(msgText, matchPattern) {
			continue
		}

		b.fireReactionRule(ctx, rule, ev, msgText)
	}
}

// fireReactionRule spawns an agent for a matched reaction rule.
func (b *Bot) fireReactionRule(ctx context.Context, rule *beadsapi.BeadDetail, ev *slackevents.ReactionAddedEvent, msgText string) {
	project := rule.Fields["project"]
	role := rule.Fields["role"]
	if role == "" {
		role = "crew"
	}
	promptTemplate := rule.Fields["prompt"]
	if promptTemplate == "" {
		b.logger.Warn("reaction-rule: no prompt configured", "rule", rule.ID)
		return
	}
	if project == "" {
		b.logger.Warn("reaction-rule: no project configured", "rule", rule.ID)
		return
	}
	if err := b.reactionRuleProjectError(ctx, rule, project); err != nil {
		b.logger.Warn("reaction-rule: refused", "rule", rule.ID, "project", project, "labels", rule.Labels, "error", err)
		return
	}

	// Throttle: one spawn per (rule + message) per cooldown period.
	throttleKey := fmt.Sprintf("reaction:%s:%s:%s", rule.ID, ev.Item.Channel, ev.Item.Timestamp)
	if b.reactionThrottle == nil {
		b.mu.Lock()
		if b.reactionThrottle == nil {
			b.reactionThrottle = newReactionSpawnThrottle()
		}
		b.mu.Unlock()
	}
	if !b.reactionThrottle.Allow(throttleKey) {
		b.logger.Info("reaction: spawn throttled",
			"rule", rule.ID, "emoji", ev.Reaction,
			"channel", ev.Item.Channel, "ts", ev.Item.Timestamp)
		return
	}

	// Build context variables for prompt interpolation.
	vars := map[string]string{
		"channel":      ev.Item.Channel,
		"thread_ts":    ev.Item.Timestamp,
		"message_ts":   ev.Item.Timestamp,
		"user":         ev.User,
		"emoji":        ev.Reaction,
		"message_text": msgText,
	}
	// Extract MR IID from message text (e.g., "MR !123" or "!123").
	if m := mrIIDPattern.FindStringSubmatch(msgText); len(m) > 1 {
		vars["mr_iid"] = m[1]
	}
	// Extract branch name from message text.
	if m := branchPattern.FindStringSubmatch(msgText); len(m) > 1 {
		vars["source_branch"] = m[1]
	}

	prompt := interpolatePrompt(promptTemplate, vars)

	// Generate agent name.
	agentName := generateSpawnName(project)

	b.logger.Info("reaction: spawning agent",
		"rule", rule.ID, "emoji", ev.Reaction,
		"agent", agentName, "project", project, "role", role,
		"channel", ev.Item.Channel, "ts", ev.Item.Timestamp)

	beadID, err := b.daemon.SpawnAgent(ctx, agentName, project, "", role, prompt)
	if err != nil {
		b.logger.Error("reaction: failed to spawn agent",
			"rule", rule.ID, "agent", agentName, "error", err)
		_, _ = b.api.PostEphemeral(ev.Item.Channel, ev.User,
			slack.MsgOptionText(fmt.Sprintf(":x: Failed to spawn agent for :%s: reaction: %s", ev.Reaction, err.Error()), false))
		return
	}

	// Store Slack context on the agent bead.
	_ = b.daemon.UpdateBeadFields(ctx, beadID, map[string]string{
		"slack_user_id":       ev.User,
		"slack_spawn_channel": ev.Item.Channel,
	})

	// Post confirmation as a thread reply to the reacted message.
	text := fmt.Sprintf(":rocket: Spawning agent *%s* (:%s: reaction rule: _%s_)\nBead: `%s`",
		agentName, ev.Reaction, rule.Title, beadID)
	_, _, _ = b.api.PostMessage(ev.Item.Channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(ev.Item.Timestamp))

	b.logger.Info("reaction: agent spawned",
		"rule", rule.ID, "agent", agentName, "bead", beadID)
}

// reactionRuleProjectError refuses a rule the bridge must not act on: one
// whose project: labels are not exactly fields.project, or whose project is a
// tenant land. The spawn runs as the bridge, so the rule's own project is the
// only fence on where it lands; a tenant land never gets fleet spawns from a
// rule (gd-bRLiEzvMsy). A land lookup failure refuses.
func (b *Bot) reactionRuleProjectError(ctx context.Context, rule *beadsapi.BeadDetail, project string) error {
	labelled := false
	for _, l := range rule.Labels {
		p, ok := strings.CutPrefix(l, "project:")
		if !ok {
			continue
		}
		if p != project {
			return fmt.Errorf("label project:%s does not match fields.project %q", p, project)
		}
		labelled = true
	}
	if !labelled {
		return fmt.Errorf("no project:%s label", project)
	}
	kind, err := b.tenantKind(ctx, project)
	if err != nil {
		return err
	}
	if kind != "" {
		return fmt.Errorf("project %q is a %s land", project, kind)
	}
	return nil
}

// loadReactionRules returns cached reaction-rule beads, refreshing if stale.
func (b *Bot) loadReactionRules(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	// Lazily init the cache struct under b.mu. The nil-check + assignment is
	// done ENTIRELY under the lock — an unguarded lock-free read here races the
	// guarded write when concurrent reaction events arrive before the cache is
	// first populated (data race caught by -race; part of ka-0000008004).
	b.mu.Lock()
	if b.reactionRuleCache == nil {
		b.reactionRuleCache = &reactionRuleCache{ttl: reactionRuleCacheTTL}
	}
	cache := b.reactionRuleCache
	b.mu.Unlock()

	// Fast path: return a fresh cache under a short-lived lock. The lock is
	// NEVER held across the daemon fetch below — a burst of stale-cache
	// reaction events must not serialize behind one in-flight round-trip
	// (ka-0000008004).
	cache.mu.Lock()
	if time.Since(cache.fetchedAt) < cache.ttl && cache.rules != nil {
		rules := cache.rules
		cache.mu.Unlock()
		return rules, nil
	}
	cache.mu.Unlock()

	// Stale (or unpopulated): refresh outside the lock. singleflight collapses
	// a concurrent burst into ONE daemon call; the losers block on the shared
	// result instead of each firing their own query or piling up on cache.mu.
	res, err, _ := cache.sf.Do("reaction-rules", func() (any, error) {
		result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"reaction-rule"},
			Statuses: []string{"open"},
			Limit:    100,
		})
		if err != nil {
			return nil, fmt.Errorf("list reaction-rule beads: %w", err)
		}

		cache.mu.Lock()
		cache.rules = result.Beads
		cache.fetchedAt = time.Now()
		cache.mu.Unlock()

		b.logger.Debug("reaction: loaded rules", "count", len(result.Beads))
		return result.Beads, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]*beadsapi.BeadDetail), nil
}

// matchReactionRulesByEmoji returns rules whose emoji field matches the given reaction.
func matchReactionRulesByEmoji(emoji string, rules []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	var matched []*beadsapi.BeadDetail
	for _, r := range rules {
		if r.Fields["emoji"] == emoji {
			matched = append(matched, r)
		}
	}
	return matched
}

// interpolatePrompt replaces {key} placeholders in a template with values from vars.
func interpolatePrompt(tmpl string, vars map[string]string) string {
	result := tmpl
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{"+k+"}", v)
	}
	return result
}

// Patterns for extracting context from message text.
var (
	mrIIDPattern  = regexp.MustCompile(`[!#](\d{2,})`)
	branchPattern = regexp.MustCompile(`(?i)(?:branch:\s*|source_branch:\s*)(\S+)`)
)
