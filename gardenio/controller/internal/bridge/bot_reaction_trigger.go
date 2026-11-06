// bot_reaction_trigger.go wires the unified SlackReactionTriggerSource into the
// slack-bridge Bot.
//
// Background: SlackReactionTriggerSource (trigger_source_slack_reaction.go) is
// the unified-trigger flavor of reaction handling — new-style
// trigger:slack-reaction beads evaluated by the shared TriggerEngine. Unlike
// the other trigger sources it cannot live in the standalone trigger-engine
// process, because matching needs the reaction_added event stream and the
// reacted-to message text, both of which only the Slack-connected bot has.
//
// This file gives the Bot a TriggerEngine with the reaction source registered,
// buffers every reaction_added event into the source (BufferReaction), and runs
// a background evaluation loop. The legacy reaction-rule path
// (bot_reactions.go) is preserved untouched and runs in parallel; the source is
// wired with SkipLegacy so legacy reaction-rule beads are NOT auto-converted
// here (otherwise they would spawn twice — once via bot_reactions.go and once
// via the engine).
package bridge

import (
	"context"
	"time"

	"gardenio/controller/internal/spawnguard"
)

// reactionTriggerPollInterval is how often the engine reloads
// trigger:slack-reaction definitions and re-evaluates buffered events when no
// event signal arrives. Buffered events evaluate immediately via the source's
// Notify channel; the ticker is the slow floor for picking up newly-created or
// edited trigger beads.
const reactionTriggerPollInterval = 30 * time.Second

// EnableReactionTriggers wires the unified trigger:slack-reaction path onto the
// bot. daemon must satisfy TriggerBeadClient (the production *beadsapi.Client
// does). notifier publishes notify-action output (typically a NoticeNotifier
// that writes a trigger-notice bead picked up over SSE); nil disables the
// notify leg. Call once before Run; the engine's CatchUp + eval loop start in
// Run via runReactionTriggerLoop.
//
// Returns the bot for fluent wiring. Safe to call with a nil daemon (no-op),
// which keeps tests that build a Bot literal without trigger wiring working.
func (b *Bot) EnableReactionTriggers(daemon TriggerBeadClient, notifier TriggerNotifier, guard *spawnguard.SpawnGuard) *Bot {
	if daemon == nil {
		return b
	}
	src := NewSlackReactionTriggerSource(b.logger).SkipLegacy()
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   daemon,
		Notifier: notifier,
		Logger:   b.logger,
		Guard:    guard,
	})
	engine.RegisterSource(src)
	b.reactionTrigger = src
	b.triggerEngine = engine
	return b
}

// bufferReactionTrigger pushes a reaction_added event into the unified trigger
// source (if wired). Called from handleReactionAdded after the message text is
// fetched, in addition to — never instead of — the legacy reaction-rule path.
func (b *Bot) bufferReactionTrigger(user, emoji, channel, messageTS, messageText string) {
	if b.reactionTrigger == nil {
		return
	}
	b.reactionTrigger.BufferReaction(user, emoji, channel, messageTS, messageText)
}

// runReactionTriggerLoop runs the trigger:slack-reaction evaluation loop until
// ctx is canceled. It mirrors the standalone trigger-engine's runTriggerEngine:
// CatchUp once (seed dedup keys from already-spawned beads so a restart does not
// re-fire), evaluate immediately, then on every tick and every buffered-event
// signal. The source's pending buffer is drained after each cycle so reaction
// events that matched no trigger this pass do not accumulate forever.
//
// No-op (returns immediately) when the unified path was never enabled.
func (b *Bot) runReactionTriggerLoop(ctx context.Context) {
	engine := b.triggerEngine
	src := b.reactionTrigger
	if engine == nil || src == nil {
		return
	}

	b.logger.Info("reaction trigger loop started", "poll_interval", reactionTriggerPollInterval)

	// Seed dedup keys from already-filed beads so a bridge restart does not
	// re-spawn for reactions it already handled.
	engine.CatchUpSource(ctx, src.Name())

	// First evaluation handles any events buffered during startup, then drain.
	evaluateAndDrain(ctx, engine, src)

	ticker := time.NewTicker(reactionTriggerPollInterval)
	defer ticker.Stop()
	notify := src.Notify()
	for {
		select {
		case <-ctx.Done():
			b.logger.Info("reaction trigger loop stopping")
			return
		case <-ticker.C:
			evaluateAndDrain(ctx, engine, src)
		case <-notify:
			evaluateAndDrain(ctx, engine, src)
		}
	}
}

// evaluateAndDrain evaluates the reaction source once and then drains exactly
// the events it evaluated. It snapshots the pending count BEFORE evaluation and
// drains that many AFTER, so a reaction_added event that a concurrent
// BufferEvent appends mid-cycle (after Evaluate has snapshotted the buffer but
// before the drain) is NOT swept away — it stays buffered and its coalesced
// Notify signal re-wakes the loop to evaluate it next pass. Blind-clearing the
// whole buffer here (the old DrainPending) dropped such events unrecoverably.
func evaluateAndDrain(ctx context.Context, engine *TriggerEngine, src *SlackReactionTriggerSource) {
	n := src.PendingCount()
	engine.EvaluateSource(ctx, src.Name())
	src.DrainFirst(n)
}
