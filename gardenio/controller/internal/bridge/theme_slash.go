package bridge

// theme_slash.go holds the small helpers shared by the themed slash-command
// and interactive-reply surfaces (gd-anVGNChYkZ, Phase 4 of
// gardenio/plans/slack-greenhouse-restyle.md). The per-site glyph choice
// lives in the callers; only logic used by more than one file lives here.

import (
	"context"
	"strings"
)

// agentStateBadge renders an agent's liveness state as a themed glyph glued
// to its verbal label ("🍃 working", "◐ working"), per the spec's
// accessibility rule — the roster and dashboard rows used to lead with a bare
// color dot. States without a lifecycle twin fall back to the idle glyph with
// the state word, so an unfamiliar state is still named.
func agentStateBadge(th Theme, state string) string {
	switch state {
	case "working":
		return th.Glyph(LifecycleWorking) + " working"
	case "spawning":
		return th.Glyph(LifecycleSpawning) + " spawning"
	case "done":
		return th.Glyph(LifecycleDone) + " done"
	case "failed":
		return th.Glyph(LifecycleAborted) + " failed"
	case "rate_limited":
		return th.Semantic(SemWarn) + " rate limited"
	case "paused":
		return th.Glyph(LifecycleBlocked) + " paused (laptop offline)"
	}
	label := strings.ReplaceAll(state, "_", " ")
	if label == "" {
		label = "idle"
	}
	return th.Semantic(SemIdle) + " " + label
}

// escalationTheme resolves the card theme for an escalation notification: the
// channel the requesting agent's messages land in (the same lookup the
// notification itself uses to pick a target), so the message is themed like
// the rest of that agent's output.
func (b *Bot) escalationTheme(ctx context.Context, bead BeadEvent) Theme {
	return b.agentTheme(ctx, bead.Fields["requesting_agent"])
}

// agentTheme resolves the card theme of the channel an agent's messages land
// in. For surfaces with no channel of their own yet (a DM to a user), it is the
// agent's project channel that decides the theme. Garden when b is nil.
func (b *Bot) agentTheme(ctx context.Context, agent string) Theme {
	if b == nil {
		return Garden
	}
	return b.themeFor(ctx, b.resolveChannel(extractAgentName(agent)))
}
