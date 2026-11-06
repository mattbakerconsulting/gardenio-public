// Command `gb badge` renders a bead's icon/badge as SVG: it reads the bead
// straight from the daemon (the same daemon-direct read path as `gb plan
// show` — no viewer URL), resolves the arms the badge is drawn from (the
// bead's own, else the nearest armed parent-child ancestor), then produces a
// badge via the badge package. The output is a self-contained ~1KB SVG on
// stdout. `gb plan badge` is kept as a deprecated alias.
//
// The generation is model-authored with a deterministic Tier-0 fallback
// (plan ka-0000005980, Phase 2 task ka-0000002437): when a model is reachable
// (BADGE_API_KEY / the pod's LiteLLM messages endpoint) and returns a usable
// self-contained SVG, that picture is used — drawn from the resolved arms'
// charges by name when the line bears any (gd-jo06g2WlQ8); on ANY failure — no key, API error,
// junk output — it falls back to the deterministic hash-of-id badge, so the
// command ALWAYS emits a valid badge and never has an empty state. Pass
// --tier0 to force the deterministic tier (zero cost, byte-stable), which is
// what the cache/backfill jobs use when they want reproducibility over
// recognizability.
package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
)

var (
	planBadgeTier0   bool
	planBadgeStyle   string
	planBadgeModel   string
	planBadgeBaseURL string
	planBadgePNG     bool
	planBadgeSize    int
	planBadgeOut     string
	planBadgeUpload  bool
	planBadgeNoCache bool
	planBadgeRefresh bool
)

var badgeCmd = &cobra.Command{
	Use:   "badge <bead-id>",
	Short: "Render a bead's icon/badge as a self-contained SVG, drawn from its arms (model-authored, deterministic fallback)",
	Long: `Render an SVG icon/badge for the bead <bead-id>, of any type.

The bead (id, title, description) is read STRAIGHT from the beads daemon —
the same daemon-direct read path as 'gb plan show', needing only
BEADS_AUTH_TOKEN, no viewer URL.

Arms: the badge is drawn from the bead's arms (heraldry). A bead with none
inherits them from its line: the parent-child parents are walked upward (task
to phase to plan to project) and the first arms found are used, with the bead
that supplied them reported on stderr and as arms_from in --json. A bead with
no arms anywhere in its line is drawn from its title and description only, and
the command says so.

The badge is then produced two ways:

  - Model-authored (Tier-2): when a model is reachable, it authors a small SVG
    picture, legible at 24px. With arms, the prompt carries the run and each
    charge's plain name, and the charges are the subject of the picture: the
    model composes them into one picture, with no emoji glyphs and no
    letters; the title and description are only context. With no arms it
    draws the bead's subject from its title and description. The
    endpoint/key come from BADGE_API_KEY (or ANTHROPIC_API_KEY) + --base-url
    (default: the pod's ANTHROPIC_BASE_URL, i.e. the in-cluster LiteLLM); the
    model from --model (default: BADGE_MODEL, else ` + badge.DefaultModel + `).
  - Deterministic (Tier-0): a pure function of the bead id — hash picks a hue,
    a background shape and a ring, plus a 2-letter monogram from the title.
    Zero cost, zero network, byte-stable forever.

Generation ALWAYS succeeds: on any model failure it falls back to Tier-0, so
there is no empty state. Pass --tier0 to force the deterministic tier.

Styles (--style), the art direction the model is asked for:

  poster             (default) the large, memorable picture: a layered
                     composition with depth and one strong silhouette, judged
                     at 128px and above. Never for a 24px slot: with --png,
                     --size must be at least ` + strconv.Itoa(badge.PosterMinSize) + `. A poster is cached apart
                     from the line-art badge (the style is part of the cache
                     key), and its model call may take a few minutes. Sample:
                       gb badge <bead-id> --png --size 512 --out x.png
  line-art-bold      a bold glyph on one saturated color; reads at 24px. Pass it
                     for a small slot.
  line-art-subtle    quiet monochrome line art for wayfinding; reads at 24px.

The defaults (poster, ` + badge.DefaultModel + `) are the owner's pick from the
sample sheet gardenio/docs/heraldry/badges-round-1.md (decision gd-zqkw2TUnp7).

Caching (content-addressed, reproducible): when a bucket is configured
(COOP_S3_BUCKET), the FIRST model-authored badge for a bead is pinned in S3 at
<COOP_S3_PREFIX>/images/badges/<id>/<cache_key>.svg and served byte-identically
on every later call — so a bead's icon is stable across restarts and free on
the hot path. cache_key hashes what the picture is drawn from: the id, the
title, the canonical run of the resolved arms, the style and the prompt
version. Change any of them (a retitle, new arms, another --style, reworded
prompts) and it lands on a new address and redraws naturally. Tier-0
fallbacks are never cached (they are already byte-stable and free to
recompute). Pass --refresh to redraw when you want a new picture: the cache is
not read, and the new model picture replaces the pinned one. Pass --no-cache
to always regenerate and never read/write the cache; --tier0 bypasses it too.

The SVG is authored into a 100x100 viewBox with NO width/height, so the
consumer scales it to 128 / 40 / 24px with CSS. With --json it emits
{id, title, tier, svg, cache_hit, cache_key, arms, arms_from} for
scripting/caching; arms is the emoji run ("" for none) and arms_from the bead
that supplied it ("" for none).

Raster (PNG) output — for consumers that cannot render SVG inline (Slack
unfurls, GitLab MR / Jira <img>, OpenGraph cards, favicons):

  --png              rasterize to PNG on stdout (pure-Go oksvg+rasterx; no
                     external binary). --json then emits content-address
                     metadata instead of the SVG.
  --size <px>        square edge for --png (default 128, at most ` + strconv.Itoa(badge.MaxRasterSize) + `).
  --out <file>, -o   write the PNG to a file (implies --png).
  --upload           upload the SVG and the rasterized PNG beside the cached
                     pictures, write their viewer-proxied URLs onto the bead
                     as icon_svg_url and icon_png_url (any bead type), and
                     print the PNG's URL (implies --png).

SVG stays the source of truth; the PNG is a derived, content-addressed
artifact. See docs/badge-art-spec.md (Raster) for the toolchain choice and the
raster-vs-SVG cost/benefit.`,
	Args: cobra.ExactArgs(1),
	RunE: runBadge,
}

// planBadgeCmd is the old spelling, `gb plan badge`, kept as an alias that
// says it is deprecated (on stderr, so a piped SVG/PNG is untouched) and then
// runs `gb badge`.
var planBadgeCmd = &cobra.Command{
	Use:    "badge <bead-id>",
	Short:  "Deprecated alias of 'gb badge <bead-id>'",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(os.Stderr, "gb plan badge is deprecated; use gb badge <bead-id>")
		return runBadge(cmd, args)
	},
}

func init() {
	rootCmd.AddCommand(badgeCmd)
	planCmd.AddCommand(planBadgeCmd)
	addBadgeFlags(badgeCmd)
	addBadgeFlags(planBadgeCmd)
}

// addBadgeFlags binds the badge flags on c. gb badge and its gb plan badge
// alias share the same flag variables, so either spelling behaves the same.
func addBadgeFlags(c *cobra.Command) {
	c.Flags().BoolVar(&planBadgeTier0, "tier0", false,
		"force the deterministic (hash-of-id) badge; skip the model entirely")
	c.Flags().StringVar(&planBadgeStyle, "style", badge.StylePoster,
		"art direction for the model tier: "+strings.Join(badge.Styles, " | "))
	c.Flags().StringVar(&planBadgeModel, "model", badgeDefaultModel(),
		"model id for the badge model tier (env BADGE_MODEL)")
	c.Flags().StringVar(&planBadgeBaseURL, "base-url", "",
		"messages API base URL for the model tier (default: ANTHROPIC_BASE_URL)")
	c.Flags().BoolVar(&planBadgePNG, "png", false,
		"emit a rasterized PNG instead of SVG (pure-Go oksvg+rasterx, no external binary)")
	c.Flags().IntVar(&planBadgeSize, "size", badge.DefaultRasterSize,
		"square edge in pixels for --png (default 128, max "+strconv.Itoa(badge.MaxRasterSize)+")")
	c.Flags().StringVarP(&planBadgeOut, "out", "o", "",
		"write the PNG to this file instead of stdout (implies --png)")
	c.Flags().BoolVar(&planBadgeUpload, "upload", false,
		"upload the SVG and PNG, write icon_svg_url and icon_png_url onto the bead, and print the PNG's viewer-proxied URL (implies --png)")
	c.Flags().BoolVar(&planBadgeNoCache, "no-cache", false,
		"skip the content-address cache: always (re)generate and never write through")
	c.Flags().BoolVar(&planBadgeRefresh, "refresh", false,
		"redraw: do not read the cache, and replace the pinned picture with the new model picture")
	c.SilenceUsage = true
}

func runBadge(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	if err := checkBadgeStyle(); err != nil {
		return err
	}

	bead, err := daemon.GetBead(cmd.Context(), beadID)
	if err != nil {
		return fmt.Errorf("reading bead %s: %w", beadID, err)
	}

	arms := resolveBadgeArms(cmd.Context(), bead)
	reportBadgeArms(bead.ID, arms)

	subject := badge.CacheSubject{ID: bead.ID, Title: bead.Title, Arms: arms.Heraldry, Style: planBadgeStyle}
	bdg, cacheHit, tierErr := badgeForPlan(cmd, subject, bead.Description)
	// tierErr is advisory: badge is always valid. Surface it on stderr so a
	// human sees WHY the model tier was skipped, without corrupting stdout.
	if tierErr != nil && !planBadgeTier0 {
		fmt.Fprintf(os.Stderr, "gb badge: using deterministic fallback: %v\n", tierErr)
	}
	if cacheHit {
		fmt.Fprintln(os.Stderr, "gb badge: served from content-address cache")
	}

	// Raster path: --png / --out / --upload all produce a PNG. --out and
	// --upload imply --png so a caller doesn't have to pass both.
	if planBadgePNG || planBadgeOut != "" || planBadgeUpload {
		return emitPlanBadgePNG(cmd, bead.ID, bead.Title, bdg, arms)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":        bead.ID,
			"title":     bead.Title,
			"tier":      bdg.Tier,
			"svg":       bdg.SVG,
			"cache_hit": cacheHit,
			"cache_key": badge.CacheKey(subject),
			"arms":      beadsapi.HeraldryRun(arms.Heraldry),
			"arms_from": arms.From,
		})
		return nil
	}
	fmt.Println(bdg.SVG)
	return nil
}

// checkBadgeStyle refuses an unknown --style, and a poster asked for at a
// raster size below badge.PosterMinSize: a poster is never drawn for a small
// slot (gd-RcmmFKWytW).
func checkBadgeStyle() error {
	if !slices.Contains(badge.Styles, planBadgeStyle) {
		return fmt.Errorf("unknown --style %q (want one of: %s)", planBadgeStyle, strings.Join(badge.Styles, ", "))
	}
	raster := planBadgePNG || planBadgeOut != "" || planBadgeUpload
	if planBadgeStyle == badge.StylePoster && raster && planBadgeSize > 0 && planBadgeSize < badge.PosterMinSize {
		return fmt.Errorf("--style poster is judged at %dpx and above; --size %d is a small-slot size, use line-art-bold or line-art-subtle there",
			badge.PosterMinSize, planBadgeSize)
	}
	return nil
}

// resolveBadgeArms resolves the arms bead's badge is drawn from: its own, else
// the nearest armed parent-child ancestor's, over every parent-child edge (a
// thread-filed task's thread bead bears no arms; its phase does). The bead is already read, so the
// walk serves it without a second GetBead; each ancestor costs a GetBead and a
// GetDependencies. A failed read part way up is reported on stderr and treated
// as no arms, so the badge is still drawn.
func resolveBadgeArms(ctx context.Context, bead *beadsapi.BeadDetail) badge.Arms {
	lookup := func(ctx context.Context, id string) (badge.LineNode, error) {
		b := bead
		if id != bead.ID {
			var err error
			if b, err = daemon.GetBead(ctx, id); err != nil {
				return badge.LineNode{}, err
			}
		}
		node := badge.LineNode{ID: b.ID, Heraldry: b.Heraldry, Type: b.Type}
		if node.Heraldry != "" {
			return node, nil
		}
		deps, err := daemon.GetDependencies(ctx, id)
		if err != nil {
			return badge.LineNode{}, err
		}
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				node.Parents = append(node.Parents, d.DependsOnID)
			}
		}
		return node, nil
	}
	arms, err := badge.ResolveArms(ctx, bead.ID, lookup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gb badge: could not walk %s's line for arms: %v\n", bead.ID, err)
	}
	return arms
}

// reportBadgeArms says on stderr where the badge's arms came from, or that
// there are none and the badge is drawn from the title and description only.
// Own arms are the expected case and stay quiet.
func reportBadgeArms(id string, arms badge.Arms) {
	switch {
	case arms.None():
		fmt.Fprintf(os.Stderr, "gb badge: %s has no arms in its line; drawing from title and description only\n", id)
	case arms.Inherited(id):
		fmt.Fprintf(os.Stderr, "gb badge: %s bears no arms; using %s inherited from %s\n",
			id, beadsapi.HeraldryRun(arms.Heraldry), arms.From)
	}
}

// badgeForPlan returns the badge for subject s, reading through the
// content-address cache when it applies. The cache is skipped for the
// deterministic tier (--tier0 is already byte-stable and free to recompute)
// and when --no-cache is passed; otherwise the FIRST model-authored badge for a
// given subject (id, title, arms, style, prompt version) is pinned in S3 and
// served on every later call, so a bead's icon is reproducible across restarts
// and cheap on the hot path. Changing any part of the subject changes the
// content address and redraws naturally (badge.CacheKey); --refresh redraws
// without a change. It returns the badge (always valid), whether it was a
// cache hit, and an advisory tier error.
func badgeForPlan(cmd *cobra.Command, s badge.CacheSubject, prose string) (badge.Badge, bool, error) {
	generate := func(context.Context) (badge.Badge, error) {
		return generatePlanBadge(cmd, s.ID, s.Title, prose, s.Arms)
	}
	// The deterministic tier and an explicit --no-cache both bypass the cache
	// entirely: just generate.
	if planBadgeTier0 || planBadgeNoCache {
		bdg, err := generate(cmd.Context())
		return bdg, false, err
	}

	// A missing/unavailable store is non-fatal: LoadOrGenerate treats a nil
	// store as "no cache" and always generates.
	store, err := newBadgeStore(cmd.Context())
	if err != nil {
		fmt.Fprintf(os.Stderr, "gb badge: cache unavailable (%v); generating without cache\n", err)
		store = nil
	}

	// LoadOrGenerate needs a *badge.BadgeStore interface value; a typed-nil
	// *s3BadgeStore would be a non-nil interface, so pass an explicit nil when
	// there is no store.
	var bs badge.BadgeStore
	if store != nil {
		bs = store
	}

	return badge.LoadOrGenerate(cmd.Context(), bs, s3Prefix(), s, planBadgeRefresh, generate)
}

// badgeDefaultModel is the --model default: BADGE_MODEL, else the badge
// package's default. The pod's ANTHROPIC_MODEL is the agent's own model and is
// deliberately not consulted.
func badgeDefaultModel() string {
	if m := os.Getenv("BADGE_MODEL"); m != "" {
		return m
	}
	return badge.DefaultModel
}

// generatePlanBadge produces the badge, honoring --tier0 (deterministic only)
// and otherwise wiring a model generator from flags + the pod environment. arms
// is the resolved arms in stored form ("" for none). It returns the badge
// (always valid) and an advisory error explaining any fallback to Tier-0.
func generatePlanBadge(cmd *cobra.Command, id, title, prose, arms string) (badge.Badge, error) {
	if planBadgeTier0 {
		return badge.Tier0(id, title), nil
	}

	apiKey := firstNonEmptyEnv("BADGE_API_KEY", "ANTHROPIC_API_KEY")
	baseURL := planBadgeBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	model := planBadgeModel

	var opts []badge.Option
	if baseURL != "" {
		opts = append(opts, badge.WithBaseURL(baseURL))
	}
	if model != "" {
		opts = append(opts, badge.WithModel(model))
	}
	opts = append(opts, badge.WithStyle(planBadgeStyle))

	// NewGenerator returns nil when apiKey is empty; Generate treats a nil
	// generator as "disabled" and returns the Tier-0 badge, so no extra
	// branch is needed here.
	gen := badge.NewGenerator(apiKey, opts...)

	// Bound the whole badge generation (including the model round-trip) so
	// `gb badge` cannot hang a caller; derive from the command context
	// so an outer cancellation still propagates.
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	// Thinking runs at the model's default, so a reply takes longer than a
	// bare glyph did, and a poster longer still; leave the request its whole
	// timeout plus a margin.
	ctx, cancel := context.WithTimeout(parent, badge.RequestTimeout(planBadgeStyle)+10*time.Second)
	defer cancel()
	return gen.Generate(ctx, id, title, prose, arms)
}
