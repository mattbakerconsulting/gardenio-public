package main

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// botOperatorDisplayName is the fleet bot identity the daemon stamps as
// operated_by_display on beads a human actually drove (e.g. through a Slack
// thread). It is a bot-name sentinel, NOT a real person, so a human-operated
// bead carrying it must be re-attributed to the human (ka-0000003240). Compared
// case-insensitively against the trimmed display.
const botOperatorDisplayName = "gardenio"

// operatorChip is the template view-model for one rendering of operated_by.
// Five intentional states are represented:
//
//   - Kind="human"     a resolved person — render display + raw chip + link to filter.
//   - Kind="machine"   an infra / bot sentinel (operated_by has a `fleet:` or
//     `system:` prefix — the exact machine-operator family the kbeads daemon
//     stamps and pre-seeds; kbeads internal/server/audit.go). Render a muted,
//     non-linked "(automated: <source>)" label, never a bolded human name, and
//     never IsViewerSelf (ka-0000003465).
//   - Kind="scheduler" the scheduler sentinel (operated_by IS NULL and CreatedBy
//     looks like a `sched-…` agent) — render the literal "(scheduler)".
//   - Kind="agent"     no human operator known but a CreatedBy agent identity
//     is on the row. Render the agent name with an "(agent)" suffix so the
//     column carries real attribution signal instead of an em-dash. Per
//     ka-0000003067: "FAR better to actually make sure the operator column
//     is ACTUALLY POPULATED" than to hide it when sparse.
//   - Kind="empty"     no operator known and no CreatedBy either. Render the
//     em-dash sentinel.
//
// The implementing reference is docs/research/human-attribution-ux.md §2.3 / §8.
type operatorChip struct {
	Kind         string // "human" | "machine" | "scheduler" | "agent" | "empty"
	Display      string // human-readable label (resolved or fallback to raw ID)
	RawID        string // opaque underlying id (slack:U…, oidc:…, email)
	Email        string // canonical email resolved server-side (empty when unresolved)
	FilterHref   string // URL to filter list views by this operator (empty when not applicable)
	IsViewerSelf bool   // true when this chip resolves to the current OIDC viewer
	// Slug is the operator's per-user-homepage slug (ka-0000000022 /
	// epic ka-0000006114). When non-empty, the chip's display label is
	// wrapped in an `/u/{slug}` link. Empty for scheduler/agent/empty
	// chips and for human chips whose slug hasn't been resolved yet
	// (legacy templates pre-M1).
	Slug string
}

// resolveOperatorChip computes the rendering state for a bead-level / event /
// dep operated_by value.
//
//	operatedBy        — the raw value from the bead (may be empty)
//	operatedByDisplay — server-resolved display label (may be empty; fallback to raw)
//	operatedByEmail   — server-resolved canonical email (may be empty)
//	operatedBySlug    — server-resolved per-user-homepage slug (may be
//	                    empty; when set, the renderer emits an
//	                    `<a href="/u/{slug}">` chip per M2 §6 /
//	                    ka-0000007787)
//	createdBy         — the agent identity that created the row (used to detect scheduler)
//	basePath          — viewer base path for filter links
//	viewerEmail       — the authenticated viewer's email (for IsViewerSelf)
func resolveOperatorChip(operatedBy, operatedByDisplay, operatedByEmail, operatedBySlug, createdBy, basePath, viewerEmail string) operatorChip {
	op := strings.TrimSpace(operatedBy)
	if op == "" {
		// operated_by is empty: distinguish scheduler-driven from agent-
		// driven from truly-unknown. The agent fallback (ka-0000003067)
		// populates the Operator column with the CreatedBy identity so
		// rows always carry attribution signal — em-dash is reserved for
		// rows where we know nothing.
		if isSchedulerAgent(createdBy) {
			return operatorChip{Kind: "scheduler"}
		}
		if cb := strings.TrimSpace(createdBy); cb != "" {
			chip := operatorChip{
				Kind:    "agent",
				Display: cb,
				RawID:   cb,
			}
			if basePath != "" {
				chip.FilterHref = basePath + "/search?operated_by=" + url.QueryEscape(cb)
			}
			return chip
		}
		return operatorChip{Kind: "empty"}
	}
	// Machine-sentinel guard (ka-0000003465): a non-empty operated_by that is a
	// `fleet:`/`system:` sentinel (e.g. fleet:slack-bridge, system:machine) is
	// an infra/bot identity, NOT a person. Without this branch the human tail
	// below unconditionally builds a Kind:human chip for it — bolded display,
	// filter link, self-detection — so a machine is rendered as a fake human
	// (the reason operated_by "looks incorrect" to Matt). Classify it as
	// Kind:machine and render a muted, non-linked "(automated: <source>)"
	// label instead. This mirrors the daemon's own machine-operator family
	// (kbeads internal/server/audit.go: sentinels are `system:*` / `fleet:*`).
	if src, ok := machineOperatorSource(op); ok {
		return operatorChip{
			Kind:    "machine",
			Display: src,
			RawID:   op,
		}
	}
	email := strings.TrimSpace(operatedByEmail)
	display := strings.TrimSpace(operatedByDisplay)
	// Bot-name-sentinel re-attribution (ka-0000003240): a genuine human
	// operated_by (slack:U… / oidc:…@…) can carry a bot-name display like
	// "gardenio" — the daemon stamps the fleet identity as operated_by_display
	// on beads a human actually drove through a Slack thread. Rendering that
	// verbatim mislabels every such bead as the bot (e.g. every /plans card
	// reads "gardenio"/GA). When the display is a bot-name sentinel (or is
	// missing), prefer the human's own signal — the resolved email's name,
	// else the slug — so the chip attributes the real author. Non-human ids
	// (fleet:/system: sentinels, jira:/gitlab: ids, bare agent names) keep
	// their display verbatim; re-labelling those is ka-0000003465's concern.
	if bridge.IsHumanOperator(op) && (display == "" || isBotOperatorDisplay(display)) {
		if human := humanLabelFromEmail(email); human != "" {
			display = human
		} else if slug := strings.TrimSpace(operatedBySlug); slug != "" {
			display = humanLabelFromSlug(slug)
		} else {
			// No better human signal: drop the bot-name sentinel so we fall
			// through to the raw-id fallback below (which the compact chip
			// renders as "??"), matching the existing unresolved-human
			// convention rather than mislabeling the bead as the bot.
			display = ""
		}
	}
	if display == "" {
		display = op
	}
	chip := operatorChip{
		Kind:    "human",
		Display: display,
		RawID:   op,
		Email:   email,
		Slug:    strings.TrimSpace(operatedBySlug),
	}
	if basePath != "" {
		chip.FilterHref = basePath + "/search?operated_by=" + url.QueryEscape(op)
	}
	if viewerEmail != "" && operatorIDsMatchViewer(op, viewerEmail) {
		chip.IsViewerSelf = true
	}
	return chip
}

// isBotOperatorDisplay reports whether a resolved operated_by_display value is
// a bot-name sentinel rather than a real person's name. Today the only such
// sentinel the daemon stamps on human-driven beads is the fleet identity
// "gardenio" (ka-0000003240); the fleet:/system: RAW-id sentinels are handled
// separately by ka-0000003465. Compared case-insensitively on the trimmed
// value.
func isBotOperatorDisplay(display string) bool {
	return strings.EqualFold(strings.TrimSpace(display), botOperatorDisplayName)
}

// humanLabelFromEmail derives a display name from a canonical email address
// by title-casing the local part's dotted/underscored/hyphenated segments:
//
//	"Matthew.Baker@acme.example" → "Matthew Baker"
//	"alice_smith@example.com"   → "Alice Smith"
//	"mike@example.com"          → "Mike"
//
// Returns "" when email is empty or has no usable local part, so the caller
// can fall through to the next signal (slug / raw id). This is a best-effort
// humanization for chips whose server-resolved display was a bot-name
// sentinel — not an authoritative identity resolution.
func humanLabelFromEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	local := email
	if at := strings.IndexByte(local, '@'); at >= 0 {
		local = local[:at]
	}
	// A "+tag" suffix (plus-addressing) is not part of the name.
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		local = local[:plus]
	}
	return titleCaseSegments(local)
}

// humanLabelFromSlug derives a display name from a per-user-homepage slug
// (e.g. "matthew-baker" → "Matthew Baker"). Same title-casing as
// humanLabelFromEmail. Returns "" when the slug yields no segments.
func humanLabelFromSlug(slug string) string {
	return titleCaseSegments(strings.TrimSpace(slug))
}

// titleCaseSegments splits s on the common name separators (dot, underscore,
// hyphen, space) and joins the non-empty segments with single spaces, upper-
// casing the first rune of each. Returns "" when no segment survives. Purely
// mechanical — it does not know real capitalization (e.g. "McBride" becomes
// "Mcbride"), which is an acceptable best-effort for a fallback chip label.
func titleCaseSegments(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == ' '
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(p)
		if r == utf8.RuneError {
			continue
		}
		out = append(out, string(unicode.ToUpper(r))+p[size:])
	}
	return strings.Join(out, " ")
}

// resolveCommentOperatorChip is the comment-row variant: per R8 §6.1 the
// per-comment chip is suppressed when operated_by equals the comment's
// author (the OIDC direct-CLI case) AND when operated_by is empty for an
// agent author — the bead-level row already discloses both. The literal
// "(scheduler)" sentinel still appears mid-thread when a gardener bead is
// human-operated but a specific comment came from the scheduler (R8 §6.1
// last bullet).
func resolveCommentOperatorChip(operatedBy, operatedByDisplay, operatedByEmail, operatedBySlug, author, createdBy, basePath, viewerEmail string) operatorChip {
	op := strings.TrimSpace(operatedBy)
	authorTrim := strings.TrimSpace(author)
	if op != "" && op == authorTrim {
		// Author commented as themselves — no extra chip.
		return operatorChip{Kind: "self"}
	}
	if op == "" {
		// Empty for agent author: suppress the per-comment segment to
		// reduce noise. The bead-level metadata still carries the state.
		// Scheduler mid-thread mismatch (op empty, createdBy is sched-…)
		// is rendered explicitly so the operator can spot the cron tag.
		if isSchedulerAgent(createdBy) {
			return operatorChip{Kind: "scheduler"}
		}
		return operatorChip{Kind: "suppressed"}
	}
	chip := resolveOperatorChip(op, operatedByDisplay, operatedByEmail, operatedBySlug, createdBy, basePath, viewerEmail)
	return chip
}

// beadOperatorChip is a template-friendly wrapper that resolves and renders
// the chip for a bead's operated_by in one call. Used by bead_show.html so
// the template stays declarative.
//
// Templates call `{{beadOperatorChip .Bead .BasePath .ViewerEmail}}`.
func beadOperatorChip(bead *beadsapi.BeadDetail, basePath, viewerEmail string) template.HTML {
	if bead == nil {
		return ""
	}
	chip := resolveOperatorChip(bead.OperatedBy, bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBySlug, bead.CreatedBy, basePath, viewerEmail)
	return renderOperatorChip(chip)
}

// resolveOperatedByParam interprets the ?operated_by=<value> query param
// for list views. Returns the resolved operator-id (or empty for no filter)
// and a flag set to true when the param was the literal alias `me`.
//
// Aliases:
//
//	""      → no filter
//	"me"    → resolves to the viewer's canonical operator id
//	          (canonicalOIDC(viewerEmail) → "oidc:<lowercased-email>"); empty
//	          when the viewer is not authenticated
//	"null"  → caller should set OperatedByIsNull=true (signaled by an
//	          empty return + the literal "null" being preserved upstream)
//	other   → passed through verbatim as a literal-match operator id
//
// The `me` branch runs the viewer email through canonicalOIDC so the daemon
// alias filter receives the SAME "oidc:<email>" shape (lowercased) the fleet
// stamps on writes. Returning the bare mixed-case email instead never matched:
// the operated_by_aliases subquery keys on the "kind:subject" form and the
// match was case-sensitive, so "?operated_by=me" silently showed nothing
// (ka-0000002505).
func resolveOperatedByParam(raw, viewerEmail string) (resolved string, isMe bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", false
	}
	if strings.EqualFold(v, "me") {
		return canonicalOIDC(viewerEmail), true
	}
	if strings.EqualFold(v, "null") || strings.EqualFold(v, "(none)") {
		return "", false
	}
	return v, false
}

// resolveDefaultOperatedBy returns the effective ?operated_by= value for a
// default-on surface (today: /active in Phase 1; /search later under P3).
//
// Precedence — URL is the source of truth:
//
//  1. If the URL key is present (any value, including the literal empty
//     "?operated_by=" that R2 specifies as "show all"), echo back the
//     caller-trimmed currentParam unchanged. The flip is skipped.
//  2. Else if the request carries an authenticated viewer email
//     (oidcauth.EmailFromContext), return the canonical "oidc:<email>"
//     operator id.
//  3. Else (anonymous / static-token / dev without auth-proxy), return "".
//
// The Has() check — not currentParam == "" — is the load-bearing trigger:
// `?operated_by=` carries the key with an empty value, and re-defaulting on
// that path would silently undo the user's "show all" override on every
// subsequent request. See viewer-default-operated-by-r3-server.md §1.
//
// Helper only. Callers (handleActive in P2, handleSearch in P3) decide
// whether their surface is default-on; this helper assumes default-on and
// is not called from default-off paths.
func resolveDefaultOperatedBy(r *http.Request, currentParam string) string {
	if r != nil && r.URL != nil && r.URL.Query().Has("operated_by") {
		return currentParam
	}
	email := strings.TrimSpace(oidcauth.EmailFromContext(r.Context()))
	if email == "" {
		return ""
	}
	// canonicalOIDC lowercases the oidc form so the cold-load default filter
	// matches the canonical "oidc:<lowercased-email>" the fleet stamps on
	// writes. Building "oidc:" + the raw X-Auth-Request-Email header (which
	// Keycloak sets mixed-case) fed the daemon a subject the case-sensitive
	// alias filter never matched — the "my plans shows nothing" bug
	// (ka-0000002505).
	return canonicalOIDC(email)
}

// operatorHeaderChip is the view-model for the header-strip chip that
// appears on default-on list surfaces (P2 wires /active; P3 wires /search).
// The chip reflects the active ?operated_by= filter state and offers links
// to switch to the most useful alternatives.
//
// Five intentional states mirror viewer-default-operated-by-r2-ux.md §4.2:
//
//   - "mine"      viewer's own work (cold-load default OR explicit ?operated_by=me)
//   - "all"       explicit "?operated_by=" (no filter)
//   - "unowned"   ?operated_by=null / (none) → operated_by IS NULL
//   - "other"     literal operator id (renders display + raw id chip)
//   - "anonymous" no viewer identity; surfaces a sign-in CTA
//
// IsDefault is meaningful only for "mine" — true when the server applied
// the default (URL had no operated_by key), false when the URL was
// explicitly ?operated_by=me. Renders a "(default)" pill so the user knows
// the scope came from the cold-load default and not from a peer's link.
//
// BasePath is the surface base path used when building switch-to links
// (e.g. "/active", "/search"). ExtraQuery preserves sibling filters
// (?project=, ?include_scheduled=) — caller passes an already-encoded
// fragment with no leading separator; the renderer prepends "&".
type operatorHeaderChip struct {
	State      string
	Display    string
	RawID      string
	IsDefault  bool
	BasePath   string
	ExtraQuery string
	SignInHref string
}

// resolveOperatorHeaderChip computes the chip view-model from already-
// resolved request state. Handlers in P2 (handleActive) and P3 (handleSearch)
// call this after they have run resolveOperatedByParam +
// resolveDefaultOperatedBy, so the chip layer doesn't re-derive the
// precedence rules.
//
//	urlHasKey      — true when r.URL.Query().Has("operated_by") (R3 §1
//	                 trigger; lets us tell "default applied" from "explicit
//	                 all").
//	resolved       — post-resolution operator id used by the daemon query
//	                 (after `me` expansion + default application). Empty
//	                 string means "no operator id was applied" (either
//	                 explicit-all or anonymous).
//	isUnowned      — true when the resolved filter is operated_by IS NULL
//	                 (raw was "null" / "(none)").
//	display        — human-readable label for `resolved` when it is a
//	                 third party. May be empty (renderer falls back to raw).
//	viewerEmail    — current OIDC viewer email; empty for anonymous.
//	basePath       — surface base path for emitting links.
//	extraQuery     — encoded sibling-filter fragment to preserve on every
//	                 emitted link (no leading separator).
func resolveOperatorHeaderChip(urlHasKey bool, resolved string, isUnowned bool, display, viewerEmail, basePath, extraQuery string) operatorHeaderChip {
	chip := operatorHeaderChip{
		BasePath:   basePath,
		ExtraQuery: extraQuery,
	}
	if isUnowned {
		chip.State = "unowned"
		return chip
	}
	if resolved != "" {
		// Filter is set to a concrete operator id. If it matches the
		// viewer, render "mine"; else render the third-party form.
		if viewerEmail != "" && operatorIDsMatchViewer(resolved, viewerEmail) {
			chip.State = "mine"
			// "mine" came from the default iff the URL had no
			// operated_by key at all. ?operated_by=me with the key
			// present is explicit-mine, no (default) pill.
			chip.IsDefault = !urlHasKey
			return chip
		}
		chip.State = "other"
		chip.RawID = resolved
		chip.Display = strings.TrimSpace(display)
		if chip.Display == "" {
			chip.Display = resolved
		}
		return chip
	}
	// resolved == "" and not unowned: either explicit-all or anonymous.
	if viewerEmail == "" {
		chip.State = "anonymous"
		return chip
	}
	chip.State = "all"
	return chip
}

// renderOperatorHeaderChip renders the header chip as inline HTML. Returns
// the empty string when State is empty so callers can embed the result
// unconditionally.
func renderOperatorHeaderChip(chip operatorHeaderChip) template.HTML {
	if chip.State == "" || chip.BasePath == "" {
		return ""
	}
	link := func(value, label, title string) string {
		href := buildOperatorHeaderHref(chip.BasePath, chip.ExtraQuery, value)
		var b strings.Builder
		b.WriteString(`<a class="operator-header-chip-link" href="`)
		b.WriteString(template.HTMLEscapeString(href))
		b.WriteString(`" title="`)
		b.WriteString(template.HTMLEscapeString(title))
		b.WriteString(`">`)
		b.WriteString(template.HTMLEscapeString(label))
		b.WriteString(`</a>`)
		return b.String()
	}
	var headline, actions string
	switch chip.State {
	case "mine":
		headline = "Showing your work"
		if chip.IsDefault {
			headline += ` <span class="operator-header-chip-default" title="Applied by the cold-load default">(default)</span>`
		}
		actions = "switch to " +
			link("", "all", "Show all work — clears the operated_by filter") +
			" / " +
			link("null", "unowned", "Show unowned work — operated_by IS NULL")
	case "all":
		headline = "Showing all work"
		actions = "switch to " +
			link("me", "just mine", "Scope to your own work") +
			" / " +
			link("null", "unowned", "Show unowned work — operated_by IS NULL")
	case "unowned":
		headline = "Showing unowned work"
		actions = "switch to " +
			link("me", "just mine", "Scope to your own work") +
			" / " +
			link("", "all", "Show all work — clears the operated_by filter")
	case "other":
		var b strings.Builder
		b.WriteString(`Showing work operated by <strong>`)
		b.WriteString(template.HTMLEscapeString(chip.Display))
		b.WriteString(`</strong>`)
		if chip.RawID != "" && chip.RawID != chip.Display {
			b.WriteString(` <code class="operator-chip-raw">`)
			b.WriteString(template.HTMLEscapeString(chip.RawID))
			b.WriteString(`</code>`)
		}
		headline = b.String()
		actions = "switch to " +
			link("me", "just mine", "Scope to your own work") +
			" / " +
			link("", "all", "Show all work — clears the operated_by filter") +
			" / " +
			link("null", "unowned", "Show unowned work — operated_by IS NULL")
	case "anonymous":
		headline = "Showing all work"
		signIn := strings.TrimSpace(chip.SignInHref)
		if signIn == "" {
			signIn = "/login"
		}
		actions = `<a class="operator-header-chip-link" href="` +
			template.HTMLEscapeString(signIn) +
			`" title="Sign in to scope this view to your own work">sign in</a> to scope to yourself / ` +
			link("null", "unowned", "Show unowned work — operated_by IS NULL")
	default:
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="operator-header-chip" data-state="`)
	b.WriteString(template.HTMLEscapeString(chip.State))
	b.WriteString(`">`)
	b.WriteString(`<span class="operator-header-chip-headline">`)
	b.WriteString(headline)
	b.WriteString(`</span> &mdash; <span class="operator-header-chip-actions">`)
	b.WriteString(actions)
	b.WriteString(`</span></span>`)
	return template.HTML(b.String())
}

// operatorHeaderChipHelper is the template-friendly wrapper registered as the
// `operatorHeaderChip` template func. Resolves and renders the header chip in
// one call so list-surface templates can drop in a single invocation:
//
//	{{operatorHeaderChip .OperatedByURLHasKey .EffectiveOperatedBy .OperatedByIsUnowned .OperatedByDisplay .ViewerEmail .ChipBasePath .ChipExtraQuery}}
func operatorHeaderChipHelper(urlHasKey bool, resolved string, isUnowned bool, display, viewerEmail, basePath, extraQuery string) template.HTML {
	chip := resolveOperatorHeaderChip(urlHasKey, resolved, isUnowned, display, viewerEmail, basePath, extraQuery)
	return renderOperatorHeaderChip(chip)
}

// renderPlanHeaderChip is the /plans-specific header-chip renderer (Step 2,
// ka-0000005414 — "sharpen the header-chip filtration UX"). It reuses the SAME
// five-state view-model the shared header chip resolves (resolveOperatorHeaderChip,
// so the mine/all/unowned/other/anonymous precedence can never diverge from
// /active and /search), but rewrites the copy for the plans surface so the scope
// states are unmistakable:
//
//   - the scope's plan COUNT rides in the headline ("Showing your 3 plans"), so
//     the operator sees at a glance whether narrowing hid anything;
//   - the "all" affordance reads as DISCOVERY ("everyone's plans") not a RESET
//     ("all"), matching the epic's cross-user-discoverability goal
//     (ka-0000004110) — switching to everyone's plans is how you find a peer's
//     work, not how you clear a filter;
//   - the "(default)" pill copy is tightened to a plain-language tooltip so a
//     cold-load scope reads as "we picked this for you", not a mysterious tag.
//
// The narrow created-by-me pivot the decision's spirit wants kept one click away
// lives on each card's planCreatorChip (unchanged) — clicking a creator scopes
// the list to exactly their plans — so this header chip owns only the mine/all
// axis. count is the number of plan cards currently rendered under the active
// scope; it is shown only for the "mine" and "other" (a specific person's)
// scopes, where "N plans" is meaningful — "all"/"unowned"/"anonymous" span the
// whole fleet and a count there would read as a firehose size, not a scope.
func renderPlanHeaderChip(urlHasKey bool, resolved string, isUnowned bool, display, viewerEmail, basePath, extraQuery string, count int) template.HTML {
	chip := resolveOperatorHeaderChip(urlHasKey, resolved, isUnowned, display, viewerEmail, basePath, extraQuery)
	if chip.State == "" || chip.BasePath == "" {
		return ""
	}
	link := func(value, label, title string) string {
		href := buildOperatorHeaderHref(chip.BasePath, chip.ExtraQuery, value)
		var b strings.Builder
		b.WriteString(`<a class="operator-header-chip-link" href="`)
		b.WriteString(template.HTMLEscapeString(href))
		b.WriteString(`" title="`)
		b.WriteString(template.HTMLEscapeString(title))
		b.WriteString(`">`)
		b.WriteString(template.HTMLEscapeString(label))
		b.WriteString(`</a>`)
		return b.String()
	}
	// plansNoun renders "1 plan" / "N plans" so the count reads naturally.
	plansNoun := func(n int) string {
		if n == 1 {
			return "1 plan"
		}
		return strconv.Itoa(n) + " plans"
	}
	// "everyone's plans" is the DISCOVERY affordance (not "all"): the whole point
	// of the plans epic is cross-user discoverability, so the widen-out link is
	// framed as "go find what other people are planning", identically worded in
	// every state that offers it.
	const discoverLabel = "everyone's plans"
	const discoverTitle = "Discover everyone's plans — see what other people are planning"
	var headline, actions string
	switch chip.State {
	case "mine":
		headline = "Showing your " + plansNoun(count)
		if chip.IsDefault {
			headline += ` <span class="operator-header-chip-default" title="We scoped this to you automatically — switch to everyone's plans to see the rest">(auto)</span>`
		}
		actions = "switch to " +
			link("", discoverLabel, discoverTitle)
	case "all":
		headline = "Showing everyone's plans"
		actions = "switch to " +
			link("me", "just your plans", "Scope to the plans you're involved in")
	case "unowned":
		headline = "Showing unowned plans"
		actions = "switch to " +
			link("me", "just your plans", "Scope to the plans you're involved in") +
			" / " +
			link("", discoverLabel, discoverTitle)
	case "other":
		var b strings.Builder
		b.WriteString(`Showing <strong>`)
		b.WriteString(template.HTMLEscapeString(chip.Display))
		b.WriteString(`</strong>'s `)
		b.WriteString(template.HTMLEscapeString(plansNoun(count)))
		if chip.RawID != "" && chip.RawID != chip.Display {
			b.WriteString(` <code class="operator-chip-raw">`)
			b.WriteString(template.HTMLEscapeString(chip.RawID))
			b.WriteString(`</code>`)
		}
		headline = b.String()
		actions = "switch to " +
			link("me", "just your plans", "Scope to the plans you're involved in") +
			" / " +
			link("", discoverLabel, discoverTitle)
	case "anonymous":
		headline = "Showing everyone's plans"
		signIn := strings.TrimSpace(chip.SignInHref)
		if signIn == "" {
			signIn = "/login"
		}
		actions = `<a class="operator-header-chip-link" href="` +
			template.HTMLEscapeString(signIn) +
			`" title="Sign in to scope this view to the plans you're involved in">sign in</a> to see just your plans`
	default:
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="operator-header-chip" data-state="`)
	b.WriteString(template.HTMLEscapeString(chip.State))
	b.WriteString(`">`)
	b.WriteString(`<span class="operator-header-chip-headline">`)
	b.WriteString(headline)
	b.WriteString(`</span> &mdash; <span class="operator-header-chip-actions">`)
	b.WriteString(actions)
	b.WriteString(`</span></span>`)
	return template.HTML(b.String())
}

// buildOperatorHeaderHref composes a switch-to link for the header chip.
// value is the raw ?operated_by= value (empty string is meaningful — it
// renders "operated_by=" with no value, the explicit-all opt-out per R2
// §2). extraQuery preserves sibling filters and is expected to be already
// URL-encoded with no leading separator.
func buildOperatorHeaderHref(basePath, extraQuery, value string) string {
	var b strings.Builder
	b.WriteString(basePath)
	b.WriteByte('?')
	if extraQuery != "" {
		b.WriteString(extraQuery)
		b.WriteByte('&')
	}
	b.WriteString("operated_by=")
	b.WriteString(url.QueryEscape(value))
	return b.String()
}

// isSchedulerAgent reports whether the given created_by identity looks like
// the cron scheduler (`sched-…` agents). The R4 schema does not introduce a
// sentinel value; the discriminator is the agent name.
func isSchedulerAgent(createdBy string) bool {
	return strings.HasPrefix(strings.TrimSpace(createdBy), "sched-")
}

// machineOperatorSource reports whether operatedBy is a machine/infra sentinel
// — a `fleet:<name>` or `system:<name>` id — and if so returns the human-
// readable source suffix (the part after the prefix) for the muted
// "(automated: <source>)" chip label. This is the viewer-side mirror of the
// kbeads daemon's machine-operator family (kbeads internal/server/audit.go:
// isHumanOperator REJECTS `system:*` / `fleet:*`, and migration 015 pre-seeds
// exactly those two sentinel prefixes). Keep the prefixes in lockstep with the
// daemon — a value the daemon treats as a sentinel but the viewer renders as a
// human re-introduces ka-0000003465 (a machine shown as a fake person).
//
// The returned source falls back to the full trimmed id when the suffix is
// empty (e.g. a bare "fleet:" / "system:") so the label never renders a blank.
func machineOperatorSource(operatedBy string) (source string, ok bool) {
	op := strings.TrimSpace(operatedBy)
	for _, prefix := range []string{"fleet:", "system:"} {
		if rest, found := strings.CutPrefix(op, prefix); found {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return op, true
			}
			return rest, true
		}
	}
	return "", false
}

// operatorIDsMatchViewer compares an operated_by id with a viewer email.
// The OIDC collapse path stores `email` directly; the slack path stores
// `slack:U…` which won't match an email, so this just covers the OIDC case.
func operatorIDsMatchViewer(opID, viewerEmail string) bool {
	op := strings.TrimSpace(opID)
	v := strings.TrimSpace(viewerEmail)
	if op == "" || v == "" {
		return false
	}
	if strings.EqualFold(op, v) {
		return true
	}
	if strings.EqualFold(op, "oidc:"+v) {
		return true
	}
	return false
}

// renderOperatorChip is the html/template helper used by every operator
// chip. Returns safe HTML for inline rendering. Kind "self" and "suppressed"
// render an empty fragment so the comment partial can simply call this and
// drop the whole adjacent separator when the chip is empty (length-0 HTML).
func renderOperatorChip(chip operatorChip) template.HTML {
	switch chip.Kind {
	case "human":
		display := template.HTMLEscapeString(chip.Display)
		raw := template.HTMLEscapeString(chip.RawID)
		var b strings.Builder
		b.WriteString(`<span class="operator-chip" data-operated-by="`)
		b.WriteString(raw)
		if chip.Slug != "" {
			b.WriteString(`" data-slug="`)
			b.WriteString(template.HTMLEscapeString(chip.Slug))
		}
		b.WriteString(`">`)
		// Per-user-homepage M1 (ka-0000000022): when slug is populated,
		// the display label links to /u/{slug} so the chip becomes the
		// entry-point to the operator's homepage. The existing filter
		// link is still available via the FilterHref-only fallback path
		// for callers that haven't migrated yet.
		switch {
		case chip.Slug != "":
			slugHref := template.HTMLEscapeString("/u/" + chip.Slug)
			b.WriteString(`<a class="operator-chip-link operator-chip-link-slug" href="`)
			b.WriteString(slugHref)
			b.WriteString(`" title="Open `)
			b.WriteString(display)
			b.WriteString(`'s homepage"><strong>`)
			b.WriteString(display)
			b.WriteString(`</strong></a>`)
		case chip.FilterHref != "":
			href := template.HTMLEscapeString(chip.FilterHref)
			b.WriteString(`<a class="operator-chip-link" href="`)
			b.WriteString(href)
			b.WriteString(`" title="Filter by `)
			b.WriteString(raw)
			b.WriteString(`"><strong>`)
			b.WriteString(display)
			b.WriteString(`</strong></a>`)
		default:
			b.WriteString(`<strong>`)
			b.WriteString(display)
			b.WriteString(`</strong>`)
		}
		if chip.RawID != "" && chip.RawID != chip.Display {
			b.WriteString(` <code class="operator-chip-raw">`)
			b.WriteString(raw)
			b.WriteString(`</code>`)
		}
		if chip.Email != "" && chip.Email != chip.Display {
			email := template.HTMLEscapeString(chip.Email)
			b.WriteString(` <a class="operator-chip-email" href="mailto:`)
			b.WriteString(email)
			b.WriteString(`" title="Email `)
			b.WriteString(email)
			b.WriteString(`">&lt;`)
			b.WriteString(email)
			b.WriteString(`&gt;</a>`)
		}
		if chip.IsViewerSelf {
			b.WriteString(` <span class="operator-chip-self" title="This is you">(self)</span>`)
		}
		b.WriteString(`</span>`)
		return template.HTML(b.String())
	case "machine":
		// Muted, non-linked infra/bot sentinel (ka-0000003465). Never a bolded
		// human name, no filter/slug link, no (self) — Display holds the source
		// suffix (e.g. "slack-bridge") and RawID the full sentinel id.
		source := template.HTMLEscapeString(chip.Display)
		raw := template.HTMLEscapeString(chip.RawID)
		var b strings.Builder
		b.WriteString(`<span class="operator-chip operator-chip-machine" data-operated-by="`)
		b.WriteString(raw)
		b.WriteString(`" title="Automated write — `)
		b.WriteString(raw)
		b.WriteString(` is a machine/infra sentinel, not a person">(automated: `)
		b.WriteString(source)
		b.WriteString(`)</span>`)
		return template.HTML(b.String())
	case "scheduler":
		return template.HTML(`<span class="operator-chip operator-chip-scheduler" title="Operated by the cron scheduler">(scheduler)</span>`)
	case "agent":
		display := template.HTMLEscapeString(chip.Display)
		raw := template.HTMLEscapeString(chip.RawID)
		var b strings.Builder
		b.WriteString(`<span class="operator-chip operator-chip-agent" data-operated-by="`)
		b.WriteString(raw)
		b.WriteString(`" title="No human operator — bead created by agent `)
		b.WriteString(raw)
		b.WriteString(`">`)
		if chip.FilterHref != "" {
			href := template.HTMLEscapeString(chip.FilterHref)
			b.WriteString(`<a class="operator-chip-link" href="`)
			b.WriteString(href)
			b.WriteString(`">`)
			b.WriteString(display)
			b.WriteString(`</a>`)
		} else {
			b.WriteString(display)
		}
		b.WriteString(` <span class="operator-chip-agent-tag">(agent)</span></span>`)
		return template.HTML(b.String())
	case "empty":
		return template.HTML(`<span class="empty">&mdash;</span>`)
	case "self", "suppressed":
		return ""
	default:
		return ""
	}
}
