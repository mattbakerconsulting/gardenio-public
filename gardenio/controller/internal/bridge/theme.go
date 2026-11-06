package bridge

// theme.go defines the selectable Garden/Business Slack card visual system
// (gd-XFizxz9y3Y). Design spec: gardenio/plans/slack-greenhouse-restyle.md —
// do not add/rename glyphs, bead types, or colors here without updating that
// spec first; this file mirrors it field-for-field.

import (
	"context"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Theme bundles everything a card-rendering call site needs to stay
// consistent within one visual system: lifecycle-state glyphs, bead-type
// glyphs, a semantic color palette, and a bot avatar description. Garden and
// Business are the only two instances; callers resolve one via themeFor
// rather than referencing Garden/Business literals directly, so a future
// third theme only touches this file.
type Theme struct {
	// Name is the value stored in a project bead's card_theme field.
	Name string

	// Lifecycle maps an agent lifecycle state (the lifecycleXxx constants
	// below) to its glyph. Per the spec's accessibility rule, every call
	// site must keep the glyph glued to its verbal label in the same text
	// run (e.g. "🍃 Working"), never a bare glyph — inline Slack unicode
	// emoji have no settable alt text.
	Lifecycle map[string]string

	// BeadType maps a bead type ("task", "bug", "feature", "epic",
	// "decision", "plan", "escalation", "agent", "bundle", "doc",
	// "message", "advice", "dashboard") to its badge glyph (Garden) or
	// bracket tag (Business). Types outside the map take BeadTypeFallback.
	BeadType map[string]string

	// BeadTypeFallback is the glyph for a bead type outside BeadType. Empty
	// means a bracket tag of the type word ("[RATING]"), as in Business. It
	// lives here rather than in bridge code so that the heraldry library
	// excludes it with the other theme glyphs (spec R4, gd-vQvHXp4Rl3).
	BeadTypeFallback string

	// Color maps a semantic role (the colorXxx constants below) to a hex
	// color, for Slack attachment color bars and accents.
	Color map[string]string

	// SemanticGlyphs maps a cross-cutting message meaning (the SemXxx constants
	// below: ok, fail, warn, link, schedule, …) to its glyph, replacing the
	// hard-coded :shortcode:/emoji in command replies and alert posts. Same
	// accessibility rule as Lifecycle: the glyph stays glued to a verbal
	// label at the call site. Read it through Semantic(), which falls back.
	SemanticGlyphs map[string]string

	// ToolGlyphs maps an agent tool name (Read, Grep, Bash, …) to its glyph for
	// the live-stream stop summary and activity lines; ToolDefault covers
	// every tool without its own entry. Read it through Tool().
	ToolGlyphs map[string]string

	// ReactionNames maps a reaction state (the ReactionXxx constants below) to a
	// stock Slack emoji NAME — no colons, and stock only, because custom
	// emoji are not available in every workspace. Read it through
	// Reaction().
	ReactionNames map[string]string

	// Avatar documents the theme's bot avatar. Slack app icons are
	// configured once in the Slack app admin, not per message, so this is
	// not rendered inline — it records the locked design for whoever next
	// updates the app icon.
	Avatar string
}

// Lifecycle state keys, shared by both themes' Lifecycle maps.
const (
	LifecycleSpawning      = "spawning"
	LifecycleWorking       = "working"
	LifecycleContinued     = "continued"
	LifecycleNeedsDecision = "needs_decision"
	LifecycleHandedOff     = "handed_off"
	LifecycleDone          = "done"
	LifecycleBlocked       = "blocked"
	LifecycleAborted       = "aborted"
	LifecycleReaped        = "reaped"
	LifecycleRestarted     = "restarted"
)

// Color role keys, shared by both themes' Color maps.
const (
	ColorNeutral = "neutral" // spawning / neutral chrome
	ColorActive  = "active"  // working / continued / restarted
	ColorSuccess = "success" // done
	ColorWarning = "warning" // blocked / needs a decision
	ColorDanger  = "danger"  // aborted
	ColorMuted   = "muted"   // reaped
)

// Semantic keys (Phase 4, gd-Z3Sgg8W5Ih), shared by both themes' Semantic
// maps. Each replaces a family of hard-coded shortcodes: the comment names the
// legacy emoji the key stands in for.
const (
	SemOK        = "ok"         // ✅ :white_check_mark:
	SemFail      = "fail"       // ❌ :x:
	SemWarn      = "warn"       // ⚠️ :warning:
	SemInfo      = "info"       // ℹ️ :information_source:
	SemDenied    = "denied"     // 🚫 :no_entry_sign: / :lock: on permission refusals
	SemBlockedBy = "blocked_by" // ⛔ :no_entry:
	SemHandoff   = "handoff"    // 🤝 :handshake:
	SemSchedule  = "schedule"   // ⏰ :clock1: / :alarm_clock:
	SemTrigger   = "trigger"    // ⚡ :zap: on trigger lines
	SemSpawn     = "spawn"      // 🚀 :rocket: on "spawning agent"
	SemRelease   = "release"    // 🚀 :rocket: on release/deploy headers
	SemIdle      = "idle"       // 💤 :zzz:
	SemLink      = "link"       // 🔗 :link:
	SemAssignee  = "assignee"   // 👤 :bust_in_silhouette:
	SemPaused    = "paused"     // ⏸ :pause_button:
	SemResumed   = "resumed"    // ▶️ :arrow_forward:
	SemStop      = "stop"       // ⏹ :black_square_for_stop:
	SemNote      = "note"       // 📝 :memo:
	SemQuestion  = "question"   // ❓ :question:
	SemReview    = "review"     // 🔍 :mag: on review requests
	SemCost      = "cost"       // 💰 :moneybag:
	SemContext   = "context"    // 🧠 :brain:
	SemTurns     = "turns"      // 🔁 :repeat:
	SemLatency   = "latency"    // ⚡ :zap: on TTFT
	SemReport    = "report"     // 📊 :bar_chart:
	SemLocal     = "local"      // 💻 :computer: on a laptop agent's card, roster line and runner card
)

// ToolDefault is the Tool map key used for any tool without its own entry
// (legacy :wrench:).
const ToolDefault = "default"

// Reaction state keys, shared by both themes' Reaction maps. The legacy
// names are eyes / hourglass_flowing_sand / white_check_mark / x /
// no_entry_sign (bot_reactions_lifecycle.go, bot_mentions.go).
const (
	ReactionSeen    = "seen"
	ReactionWorking = "working"
	ReactionDone    = "done"
	ReactionFailed  = "failed"
	ReactionRefused = "refused"
)

// Garden is the default theme: the plant-growth vocabulary.
var Garden = Theme{
	Name: "garden",
	Lifecycle: map[string]string{
		LifecycleSpawning:      "🫘",
		LifecycleWorking:       "🍃",
		LifecycleContinued:     "🌿",
		LifecycleNeedsDecision: "🧑‍🌾",
		LifecycleHandedOff:     "🪴",
		LifecycleDone:          "🌳",
		LifecycleBlocked:       "🥀",
		LifecycleAborted:       "🍄",
		LifecycleReaped:        "🍂",
		LifecycleRestarted:     "🌾",
	},
	BeadType: map[string]string{
		"task":       "🌱",
		"bug":        "🐛",
		"feature":    "🌸",
		"epic":       "🌻",
		"decision":   "🪧",
		"plan":       "🗺️",
		"escalation": "🚨",
		"agent":      "🤖",
		"bundle":     "🪺",
		"doc":        "🪵",
		"message":    "🦜",
		"advice":     "🦉",
		"dashboard":  "🥕",
	},
	BeadTypeFallback: "🪹",
	Color: map[string]string{
		ColorNeutral: "#A9C77C", // Seedling
		ColorActive:  "#6E8F42", // Moss
		ColorSuccess: "#3F6B3A", // Forest
		ColorWarning: "#C9A13B", // Harvest
		ColorDanger:  "#B5652E", // Clay
		ColorMuted:   "#8B4C39", // Wilt
	},
	SemanticGlyphs: map[string]string{
		SemOK:        "🌼",
		SemFail:      "🪨",
		SemWarn:      "🌵",
		SemInfo:      "💧",
		SemDenied:    "🚧",
		SemBlockedBy: "🧱",
		SemHandoff:   "🧤",
		SemSchedule:  "☀️",
		SemTrigger:   "🌩️",
		SemSpawn:     "🌰",
		SemRelease:   "🚜",
		SemIdle:      "🐌",
		SemLink:      "🪢",
		SemAssignee:  "🐝",
		SemPaused:    "🌙",
		SemResumed:   "🌅",
		SemStop:      "🪓",
		SemNote:      "🪶",
		SemQuestion:  "🪻",
		SemReview:    "🐞",
		SemCost:      "🍯",
		SemContext:   "🧺",
		SemTurns:     "♻️",
		SemLatency:   "🐇",
		SemReport:    "🍅",
		SemLocal:     "🧑‍💻",
	},
	ToolGlyphs: map[string]string{
		"Read":         "📖",
		"Grep":         "🔍",
		"Glob":         "🗂️",
		"Edit":         "✂️",
		"Write":        "🖋️",
		"Bash":         "⚙️",
		"WebFetch":     "🐕",
		"WebSearch":    "🔭",
		"Agent":        "👥",
		"NotebookEdit": "📓",
		ToolDefault:    "🔧",
	},
	// Stock plant emoji, echoing the lifecycle glyphs (seedling → leaves →
	// tree; wilted flower for a failure).
	ReactionNames: map[string]string{
		ReactionSeen:    "seedling",
		ReactionWorking: "leaves",
		ReactionDone:    "deciduous_tree",
		ReactionFailed:  "wilted_flower",
		ReactionRefused: "fallen_leaf",
	},
	Avatar: "leaf mark — white leaf silhouette on Forest #3F6B3A",
}

// Business is the plant-free alternative: plain geometric glyphs and bracket
// tags, for channels/orgs that want the same structural upgrade (color,
// accessible pairing, a unique glyph per state) without the plant theme.
var Business = Theme{
	Name: "business",
	Lifecycle: map[string]string{
		LifecycleSpawning:      "○",
		LifecycleWorking:       "◐",
		LifecycleContinued:     "↻",
		LifecycleNeedsDecision: "◆",
		LifecycleHandedOff:     "→",
		LifecycleDone:          "●",
		LifecycleBlocked:       "■",
		LifecycleAborted:       "✕",
		LifecycleReaped:        "◻",
		LifecycleRestarted:     "↺",
	},
	BeadType: map[string]string{
		"task":       "[TASK]",
		"bug":        "[BUG]",
		"feature":    "[FEAT]",
		"epic":       "[EPIC]",
		"decision":   "[DECISION]",
		"plan":       "[PLAN]",
		"escalation": "[ESCALATION]",
		"agent":      "[AGENT]",
		"bundle":     "[BUNDLE]",
		"doc":        "[DOC]",
		"message":    "[MESSAGE]",
		"advice":     "[ADVICE]",
		"dashboard":  "[DASHBOARD]",
	},
	Color: map[string]string{
		ColorNeutral: "#6B7280", // Slate
		ColorActive:  "#2563EB", // Info
		ColorSuccess: "#16A34A", // Success
		ColorWarning: "#D97706", // Warning
		ColorDanger:  "#DC2626", // Danger
		ColorMuted:   "#9CA3AF", // Muted
	},
	// Bracket tags, like BeadType: plain text is its own accessible label.
	SemanticGlyphs: map[string]string{
		SemOK:        "[OK]",
		SemFail:      "[FAIL]",
		SemWarn:      "[WARN]",
		SemInfo:      "[INFO]",
		SemDenied:    "[DENIED]",
		SemBlockedBy: "[BLOCKED BY]",
		SemHandoff:   "[HANDOFF]",
		SemSchedule:  "[SCHEDULE]",
		SemTrigger:   "[TRIGGER]",
		SemSpawn:     "[SPAWN]",
		SemRelease:   "[RELEASE]",
		SemIdle:      "[IDLE]",
		SemLink:      "[LINK]",
		SemAssignee:  "[ASSIGNEE]",
		SemPaused:    "[PAUSED]",
		SemResumed:   "[RESUMED]",
		SemStop:      "[STOP]",
		SemNote:      "[NOTE]",
		SemQuestion:  "[QUESTION]",
		SemReview:    "[REVIEW]",
		SemCost:      "[COST]",
		SemContext:   "[CTX]",
		SemTurns:     "[TURNS]",
		SemLatency:   "[TTFT]",
		SemReport:    "[REPORT]",
		SemLocal:     "[LOCAL]",
	},
	// Plain geometric / ASCII marks (no color emoji). Tool names always
	// appear beside the mark in the stop summary, so it need not be spelled.
	ToolGlyphs: map[string]string{
		"Read":         "≡",
		"Grep":         "/",
		"Glob":         "*",
		"Edit":         "✎",
		"Write":        "▪",
		"Bash":         "$",
		"WebFetch":     "↓",
		"WebSearch":    "?",
		"Agent":        "»",
		"NotebookEdit": "▦",
		ToolDefault:    "·",
	},
	// Stock marks only; the working/done/failed/refused names are the
	// pre-theme ones, so Business reactions look unchanged.
	ReactionNames: map[string]string{
		ReactionSeen:    "white_circle",
		ReactionWorking: "hourglass_flowing_sand",
		ReactionDone:    "white_check_mark",
		ReactionFailed:  "x",
		ReactionRefused: "no_entry_sign",
	},
	Avatar: "grey rounded square with a single white chevron ›",
}

// ColorForLifecycle returns this theme's color for the semantic role that
// corresponds to a lifecycle state, per the spec's Business palette "Used
// for" column (Garden's palette carries the same roles, just named after
// plants rather than described by function). The spec's table leaves
// handed_off unassigned; it is grouped with active here as the same
// transition family as continued/restarted (gardener-authoring-guide Rule 6
// — resolving an implementation-level ambiguity in-lane).
func (t Theme) ColorForLifecycle(state string) string {
	switch state {
	case LifecycleSpawning:
		return t.Color[ColorNeutral]
	case LifecycleWorking, LifecycleContinued, LifecycleRestarted, LifecycleHandedOff:
		return t.Color[ColorActive]
	case LifecycleDone:
		return t.Color[ColorSuccess]
	case LifecycleBlocked, LifecycleNeedsDecision:
		return t.Color[ColorWarning]
	case LifecycleAborted:
		return t.Color[ColorDanger]
	case LifecycleReaped:
		return t.Color[ColorMuted]
	default:
		return t.Color[ColorNeutral]
	}
}

// themeByName resolves a project bead's card_theme field value to its Theme.
// Unset, empty, or unrecognized values default to Garden.
func themeByName(name string) Theme {
	if name == Business.Name {
		return Business
	}
	return Garden
}

// Glyph returns this theme's glyph for a lifecycle state, or "" when the
// state is unknown.
func (t Theme) Glyph(state string) string { return t.Lifecycle[state] }

// Semantic returns this theme's glyph for a semantic key (SemXxx). A key the
// theme lacks falls back to Garden's glyph, then "" — never a panic or a
// blank for a key a theme simply hasn't caught up on.
func (t Theme) Semantic(key string) string {
	if g, ok := t.SemanticGlyphs[key]; ok {
		return g
	}
	return Garden.SemanticGlyphs[key]
}

// Tool returns this theme's glyph for a tool name, falling back to the
// theme's ToolDefault entry (and Garden's if the theme has none) for tools
// without their own.
func (t Theme) Tool(tool string) string {
	if g, ok := t.ToolGlyphs[tool]; ok {
		return g
	}
	if g, ok := t.ToolGlyphs[ToolDefault]; ok {
		return g
	}
	return Garden.ToolGlyphs[ToolDefault]
}

// Reaction returns the stock Slack emoji name (no colons) for a reaction
// state (ReactionXxx), falling back to Garden's name for an unknown key.
func (t Theme) Reaction(state string) string {
	if n, ok := t.ReactionNames[state]; ok {
		return n
	}
	return Garden.ReactionNames[state]
}

// BeadTypeGlyph returns this theme's glyph for a bead type: its BeadType
// entry, else BeadTypeFallback, else (Business) a bracket tag of the type
// word.
func (t Theme) BeadTypeGlyph(beadType string) string {
	if g, ok := t.BeadType[beadType]; ok {
		return g
	}
	if t.BeadTypeFallback != "" {
		return t.BeadTypeFallback
	}
	return "[" + strings.ToUpper(beadDisplayType(beadType)) + "]"
}

// BeadTypeBadge renders the type badge for a bead in the unfurl metadata line,
// keeping the glyph glued to its verbal label per the accessibility rule.
// Garden's emoji badge is paired with the type word ("🌻 `epic`"); Business's
// bracket tag ("[EPIC]") is itself plain text, so it stands alone.
func (t Theme) BeadTypeBadge(beadType string) string {
	g := t.BeadTypeGlyph(beadType)
	if strings.HasPrefix(g, "[") {
		return g
	}
	return g + " `" + beadDisplayType(beadType) + "`"
}

// themeCacheTTL bounds how stale a channel's resolved theme can be. themeFor
// runs on hot paths (every agent-card flush, every stream status tick), and a
// card_theme edit in beads-viewer is a cosmetic change that can wait a few
// seconds, so one daemon project listing is shared across a short window.
const themeCacheTTL = 30 * time.Second

type themeCacheEntry struct {
	theme   Theme
	expires time.Time
}

// themeCache is a small per-channel TTL cache for themeFor. The zero value is
// ready to use.
type themeCache struct {
	mu      sync.Mutex
	entries map[string]themeCacheEntry
}

func (c *themeCache) get(channel string, now time.Time) (Theme, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[channel]
	if !ok || !now.Before(e.expires) {
		return Theme{}, false
	}
	return e.theme, true
}

func (c *themeCache) put(channel string, t Theme, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]themeCacheEntry)
	}
	c.entries[channel] = themeCacheEntry{theme: t, expires: now.Add(themeCacheTTL)}
}

// themeFor resolves the card theme for a Slack channel: channel -> owning
// project (the same channel->project lookup bot_notifications.go /
// bot_commands.go use for other per-project behavior) -> that project's
// card_theme field. Defaults to Garden when the channel has no resolvable
// project (none claim it, or several conflict) or the project hasn't set
// card_theme. Successful resolutions are cached for themeCacheTTL; a failed
// project listing is not cached, so the next call retries.
func (b *Bot) themeFor(ctx context.Context, channel string) Theme {
	if b == nil || b.daemon == nil {
		return Garden
	}
	now := time.Now()
	if t, ok := b.themes.get(channel, now); ok {
		return t
	}
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("themeFor: failed to list projects", "channel", channel, "error", err)
		return Garden
	}
	theme := Garden
	if project, conflicts := beadsapi.ProjectForChannel(projects, channel); len(conflicts) == 0 && project != "" {
		theme = themeByName(projects[project].CardTheme)
	}
	b.themes.put(channel, theme, now)
	return theme
}

// themeResolver is the slice of *Bot the stream manager needs to theme the
// assistant thread status text. The manager reaches the Bot through its
// ThreadStatusSetter, so a setter that also resolves themes opts in.
type themeResolver interface {
	themeFor(ctx context.Context, channel string) Theme
}

// themeFor resolves the theme for channel through the thread-status setter
// (the Bot in production), defaulting to Garden when the setter cannot.
func (m *AgentStreamManager) themeFor(ctx context.Context, channel string) Theme {
	if r, ok := m.threadStatus.(themeResolver); ok {
		return r.themeFor(ctx, channel)
	}
	return Garden
}
