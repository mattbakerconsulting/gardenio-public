package badge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gardenio/controller/internal/heraldry"
)

// Generator authors a Tier-2 (model-authored) SVG badge for a bead by asking a
// model to draw its arms (or, with none, its subject) as a small picture. It talks to an
// Anthropic-messages-compatible endpoint (the in-cluster LiteLLM, which mounts
// /v1/messages) exactly the way the namegen package does — same request shape,
// same fallback discipline. A nil *Generator is the "model disabled" state and
// makes Generate fall straight through to Tier-0, so callers never branch on
// availability.
type Generator struct {
	apiKey  string
	baseURL string
	model   string
	style   string
	client  *http.Client
}

// Style names the art direction the model prompt asks for. The concrete choice
// (bold color vs. quiet monochrome vs. painterly) is an open operator decision
// for the Phase 1 bake-off (ka-0000004332), so it is a parameter rather than
// baked in — Phase 1 renders candidates by varying this, and Phase 2+ pins
// whichever the operator picks. StyleLineArtBold is a sane default that works
// without any external image model (SVG line art is authored as text).
const (
	// StyleLineArtBold: recognizable subject as line art on a bold color fill.
	StyleLineArtBold = "line-art-bold"
	// StyleLineArtSubtle: monochrome/quiet line art that recedes into
	// wayfinding rather than decoration.
	StyleLineArtSubtle = "line-art-subtle"
	// StylePoster: a rich, layered composition with depth and one strong
	// silhouette, judged at 128px and above (gd-RcmmFKWytW). It is the large,
	// memorable picture, so it is never drawn for a 24px slot: it gets its own
	// prompts (no 24px rule), a larger byte budget and a longer model call.
	StylePoster = "poster"
)

// Styles lists every style, in the order the CLI help names them.
var Styles = []string{StyleLineArtBold, StyleLineArtSubtle, StylePoster}

// PosterMinSize is the smallest square edge, in pixels, a poster badge is
// judged and rendered at. Below it the depth and detail turn to mush, so a
// slot smaller than this takes one of the line-art styles instead.
const PosterMinSize = 128

// DefaultModel is the model id a Generator uses when WithModel is not given.
// It is a current-generation model, served both by the public API and as a
// LiteLLM alias in-cluster. It is the owner's pick from the step 8 sample sheet
// of plan gd-pQZO9ie11D (decision gd-zqkw2TUnp7, with the poster style; see
// gardenio/docs/heraldry/badges-round-1.md). gb badge reads BADGE_MODEL before
// falling back to it.
const DefaultModel = "claude-opus-5-5"

// PromptVersion is the version of the prompts buildPrompt writes. The badge
// cache key hashes it (CacheKey), so bumping it redraws every cached picture
// on its next request. Bump it whenever a change to the prompts would draw a
// different picture; TestPromptVersionPinsPrompts fails until you do.
const PromptVersion = 1

// maxTokens bounds one model reply. Thinking is left at the model's default
// (on), and its tokens count against this cap alongside the SVG, so it is
// sized for a rich drawing plus the thinking before it rather than for the
// ~1 KB glyph alone: a truncated reply has no closing </svg> and falls back to
// Tier-0.
const maxTokens = 16000

// posterMaxTokens is maxTokens for StylePoster: a layered poster is several
// times the SVG of a glyph, and still follows the thinking.
const posterMaxTokens = 32000

// maxSVGBytes caps a line-art badge (see sanitizeSVG); maxPosterSVGBytes caps a
// poster, whose gradients and layers need more room. A poster is never inlined
// into a /plans card, so the card byte budget does not bind it.
const (
	maxSVGBytes       = 8 * 1024
	maxPosterSVGBytes = 24 * 1024
)

// RequestTimeout bounds one model call for a style. A poster reply is longer,
// so it gets longer; callers that wrap Generate in their own deadline size it
// from this.
func RequestTimeout(style string) time.Duration {
	if style == StylePoster {
		return 300 * time.Second
	}
	return 90 * time.Second
}

// Option configures a Generator.
type Option func(*Generator)

// WithBaseURL overrides the messages API base URL (default: the public
// Anthropic API; in-cluster callers pass the LiteLLM service URL).
func WithBaseURL(url string) Option { return func(g *Generator) { g.baseURL = url } }

// WithModel overrides the model id.
func WithModel(model string) Option { return func(g *Generator) { g.model = model } }

// WithStyle sets the art direction the prompt requests (see Style* constants).
func WithStyle(style string) Option {
	return func(g *Generator) {
		if style != "" {
			g.style = style
		}
	}
}

// NewGenerator builds a model-backed badge generator. Returns nil when apiKey
// is empty (disabled) — a nil generator is a valid argument to Generate, which
// then returns the Tier-0 badge.
func NewGenerator(apiKey string, opts ...Option) *Generator {
	if apiKey == "" {
		return nil
	}
	g := &Generator{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
		model:   DefaultModel,
		style:   StyleLineArtBold,
	}
	for _, o := range opts {
		o(g)
	}
	// The default client is built after the options so its timeout follows
	// the chosen style; WithHTTPClient still wins.
	if g.client == nil {
		g.client = &http.Client{Timeout: RequestTimeout(g.style)}
	}
	return g
}

// Anthropic /v1/messages request/response subset (mirrors namegen). There is
// no thinking field, so thinking runs at the model's default, and no assistant
// prefill, which current models refuse.
type messagesReq struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResp struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// styleDirective maps a Style constant to the art-direction sentence spliced
// into the prompt. Unknown styles fall back to the bold default.
func styleDirective(style string) string {
	switch style {
	case StyleLineArtSubtle:
		return "Quiet and monochrome: a single muted foreground color on a very light or transparent background, thin strokes. The badge should recede and act as wayfinding, not decoration."
	case StylePoster:
		return "Poster art: a rich, layered composition with real depth (a background, a middle ground and a foreground), light and shadow built from overlapping shapes, gradients and opacity, a deliberate palette of a few harmonious colors, all held together by ONE strong, instantly readable silhouette. It should be a picture someone remembers after one look."
	default:
		return "Bold and confident: one saturated background color with a light foreground glyph, clean thick strokes. The badge should read as a distinct sticker."
	}
}

// promptOutputRules are the output constraints every prompt ends with: one
// self-contained SVG in the 100x100 viewBox Tier-0 uses, so the two tiers are
// drop-in swappable and sanitizeSVG and the raster need no change.
const promptOutputRules = `Output requirements (STRICT):
- Output ONLY a single complete <svg>...</svg> element. No markdown fences, no prose, no explanation.
- The root <svg> MUST use viewBox="0 0 100 100" and MUST declare xmlns="http://www.w3.org/2000/svg".
- Self-contained: no external references, no <image>, no <script>, no external fonts.`

// promptTemplate asks the model to author a complete SVG glyph for a bead that
// bears no arms, drawn from its title and description. The hard constraints are
// the acceptance criteria from the plan narrative (ka-0000005497): it MUST be
// legible at 24px and it MUST be authored into the same 100x100 viewBox Tier-0
// uses. Its text is pinned by testdata/prompt-no-arms.golden.txt.
const promptTemplate = `You are designing a tiny icon/badge that represents a software project plan. It will be shown as small as 24x24 pixels in a list and as large as 128px on a header, so it must be BOLD and LEGIBLE at 24px — a single recognizable subject, not fine detail or text.

Art direction: %s

Draw the plan's SUBJECT as simple geometric line/shape art. Do NOT put letters, words, or a monogram in the icon. Avoid thin lines, small gaps, and more than ~3 distinct elements — they collapse into mush at 24px.

` + promptOutputRules + `

Plan title: %s
Plan description: %s`

// armsPromptTemplate asks for a picture of a bead's arms: the charges are the
// subject, named in plain words from the heraldry library, and the title and
// description are only context for reading them. Its %s verbs take, in order,
// the style directive, the run, the numbered charge names, the title and the
// description. Its text is pinned by testdata/prompt-arms*.golden.txt.
const armsPromptTemplate = `You are drawing a badge: a picture of the arms of a piece of software work. It will be shown as small as 24x24 pixels in a list and as large as 128px on a header, so it must be BOLD and LEGIBLE at 24px.

Art direction: %s

The arms are a row of emoji chosen to stand for this work. Each emoji is a charge, and the charges are the SUBJECT of the picture. Compose the things the charges name into ONE picture, drawn as simple geometric line/shape art, not as a row of separate icons. The title and description below are only context for how to read the charges; do not draw them. Do NOT draw emoji glyphs, and do NOT put letters, words, or a monogram in the picture.

Arms: %s
Charges, in order:
%s

` + promptOutputRules + `

Title: %s
Description: %s`

// posterRenderRules are the extra output constraints for StylePoster. A poster
// is also rasterized by the pure-Go oksvg renderer (raster.go), which draws
// shapes and gradients but silently drops filters, masks, clip paths, patterns
// and text, so the prompt keeps the picture inside what it draws.
const posterRenderRules = `- Use ONLY <g>, <path>, <circle>, <ellipse>, <rect>, <polygon>, <polyline>, <line>, and <linearGradient>/<radialGradient> inside <defs>, with fill-opacity, stroke-opacity or opacity for depth.
- Do NOT use <filter>, <mask>, <clipPath>, <pattern>, <text>, <use>, <style> or CSS; the picture is also rasterized by a renderer that ignores them.
- Fill the whole 100x100 canvas edge to edge; keep the complete SVG under 20 KB.`

// posterPromptTemplate is promptTemplate for StylePoster: the same subject
// from the title and description, but judged at 128px and above, so it asks
// for a full composition instead of a 24px glyph. Its text is pinned by
// testdata/prompt-poster-no-arms.golden.txt.
const posterPromptTemplate = `You are painting a poster-style badge that represents a software project plan. It is shown LARGE, at 128px and above (up to 512px), never in a small list slot, so it can carry a full composition. It must still read as one strong silhouette at 128px, and it should be memorable.

Art direction: %s

Paint the plan's SUBJECT as one scene built from vector shapes. Do NOT put letters, words, or a monogram in the picture.

` + promptOutputRules + `
` + posterRenderRules + `

Plan title: %s
Plan description: %s`

// posterArmsPromptTemplate is armsPromptTemplate for StylePoster. Its %s verbs
// are in the same order. Its text is pinned by
// testdata/prompt-poster-arms.golden.txt.
const posterArmsPromptTemplate = `You are painting a poster-style badge: a picture of the arms of a piece of software work. It is shown LARGE, at 128px and above (up to 512px), never in a small list slot, so it can carry a full composition. It must still read as one strong silhouette at 128px, and it should be memorable.

Art direction: %s

The arms are a row of emoji chosen to stand for this work. Each emoji is a charge, and the charges are the SUBJECT of the picture. Compose the things the charges name into ONE scene with depth, built from vector shapes, not as a row of separate icons. The title and description below are only context for how to read the charges; do not draw them. Do NOT draw emoji glyphs, and do NOT put letters, words, or a monogram in the picture.

Arms: %s
Charges, in order:
%s

` + promptOutputRules + `
` + posterRenderRules + `

Title: %s
Description: %s`

// buildPrompt returns the prompt for one badge. stored is the arms in a stored
// form (the run, or the legacy six-token shield form; "" for none), read
// through heraldry.Parse. With no arms, or arms that do not parse, it is the
// title-and-description prompt, unchanged from before arms were fed in, so a
// badge is always drawn.
func buildPrompt(style, title, prose, stored string) string {
	plain, withArms := promptTemplate, armsPromptTemplate
	if style == StylePoster {
		plain, withArms = posterPromptTemplate, posterArmsPromptTemplate
	}
	arms, err := heraldry.Parse(stored)
	if err != nil {
		return fmt.Sprintf(plain, styleDirective(style), title, prose)
	}
	var charges strings.Builder
	for i, c := range arms.Charges {
		if i > 0 {
			charges.WriteByte('\n')
		}
		fmt.Fprintf(&charges, "%d. %s", i+1, c.Name)
	}
	return fmt.Sprintf(withArms, styleDirective(style), arms.Run(), charges.String(), title, prose)
}

// Generate returns a badge for the bead id, drawn from arms (its stored form,
// "" for none) when it bears any, else from the title and description (prose).
// It ALWAYS succeeds: on a nil
// generator, an empty title, an API error, or a model response that is not a
// usable self-contained SVG, it returns the deterministic Tier-0 badge for
// (id, title). The error it returns is advisory — the Badge is always valid —
// so a caller can log why the model tier was skipped without special-casing the
// output. This is the single always-succeeds entry point the plan bets on.
func (g *Generator) Generate(ctx context.Context, id, title, prose, arms string) (Badge, error) {
	fallback := Tier0(id, title)
	if g == nil {
		return fallback, fmt.Errorf("badge: generator disabled (no api key)")
	}

	svg, err := g.authorSVG(ctx, title, prose, arms)
	if err != nil {
		return fallback, err
	}
	clean, err := sanitizeSVG(svg, g.maxSVGBytes())
	if err != nil {
		return fallback, err
	}
	return Badge{SVG: clean, Tier: TierModel}, nil
}

// authorSVG performs the model round-trip and returns the raw text the model
// produced (still to be validated/sanitized by the caller).
func (g *Generator) authorSVG(ctx context.Context, title, prose, arms string) (string, error) {
	// Keep the description bounded so the call stays cheap and within any
	// context budget — the subject is usually clear from the first paragraph.
	if len(prose) > 2000 {
		prose = prose[:2000]
	}

	body, err := json.Marshal(messagesReq{
		Model:     g.model,
		MaxTokens: g.maxTokens(),
		Messages: []message{{
			Role:    "user",
			Content: buildPrompt(g.style, title, prose, arms),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("badge: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("badge: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", g.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("badge: API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("badge: API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var apiResp messagesResp
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("badge: decode response: %w", err)
	}
	for _, block := range apiResp.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("badge: no text block in response")
}

// maxTokens is the reply cap for the generator's style.
func (g *Generator) maxTokens() int {
	if g.style == StylePoster {
		return posterMaxTokens
	}
	return maxTokens
}

// maxSVGBytes is the sanitizer's size cap for the generator's style.
func (g *Generator) maxSVGBytes() int {
	if g.style == StylePoster {
		return maxPosterSVGBytes
	}
	return maxSVGBytes
}

// svgElementRe extracts the first <svg …>…</svg> element from arbitrary model
// output (which may wrap it in ```svg fences or a stray sentence despite the
// prompt). (?is): case-insensitive, dot-matches-newline.
var svgElementRe = regexp.MustCompile(`(?is)<svg\b.*?</svg\s*>`)

// forbiddenRe rejects the constructs that would make a badge unsafe to inline
// or dependent on the network: script, external <image>, foreignObject, event
// handlers (on…=), and external url()/href references.
var forbiddenRe = regexp.MustCompile(`(?is)<script\b|<foreignobject\b|<image\b|\son\w+\s*=|xlink:href\s*=\s*["']?https?:|href\s*=\s*["']?https?:`)

// sanitizeSVG validates that model output is a single self-contained SVG we can
// safely inline, at most maxBytes long, and returns just that element. It is the gate that turns
// "model returned SOMETHING" into "model returned a badge we trust" — anything
// that fails here drops the caller back to Tier-0.
func sanitizeSVG(raw string, maxBytes int) (string, error) {
	svg := svgElementRe.FindString(raw)
	if svg == "" {
		return "", fmt.Errorf("badge: model output contained no <svg> element")
	}
	svg = strings.TrimSpace(svg)

	if forbiddenRe.MatchString(svg) {
		return "", fmt.Errorf("badge: model SVG contained a disallowed construct (script/image/handler/external ref)")
	}
	// A badge must be authored into the shared 100x100 coordinate system so
	// it swaps in for Tier-0 without rescaling. Enforce the viewBox rather
	// than trusting the prompt.
	if !strings.Contains(strings.ToLower(svg), `viewbox="0 0 100 100"`) {
		return "", fmt.Errorf("badge: model SVG missing required viewBox=\"0 0 100 100\"")
	}
	// Guard the byte budget: the plan explicitly polices card byte density
	// (deckCardByteBudget = 10 KB). A badge is meant to be ~1KB; anything
	// past a few KB is a runaway and not worth inlining. A poster's larger cap
	// is the caller's to pass.
	if len(svg) > maxBytes {
		return "", fmt.Errorf("badge: model SVG too large (%d bytes)", len(svg))
	}
	return svg, nil
}
