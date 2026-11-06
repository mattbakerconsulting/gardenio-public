package bridge

import "context"

// ThemeResolver resolves the card theme for a Slack channel. The standalone
// alert watchers (coverage, SCIF gate/release, model-fallback, refusal) are not
// Bots, so main wires them to (*Bot).ThemeResolver. A nil resolver means
// Garden, which keeps every watcher usable without a daemon in tests.
type ThemeResolver func(ctx context.Context, channel string) Theme

// ThemeResolver exposes themeFor to the standalone alert watchers.
func (b *Bot) ThemeResolver() ThemeResolver {
	if b == nil {
		return nil
	}
	return b.themeFor
}

// themeOf resolves channel through r, defaulting to Garden.
func themeOf(ctx context.Context, r ThemeResolver, channel string) Theme {
	if r == nil {
		return Garden
	}
	return r(ctx, channel)
}

// semLabel glues a theme's semantic glyph to its verbal label (the
// accessibility rule): "🌵 Model fallback" in Garden, "[WARN] Model fallback"
// in Business.
func semLabel(th Theme, key, label string) string {
	return th.Semantic(key) + " " + label
}
