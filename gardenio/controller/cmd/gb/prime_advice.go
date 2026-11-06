package main

// prime_advice.go contains outputAdvice which renders matched advice for an agent.
// Subscription matching logic lives in internal/advice/.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// primeAdviceByteBudget is the default ceiling on the rendered size (in bytes)
// of the non-inline abbreviated portion of the advice section in gb prime
// output. Inline beads (label "inline:true") bypass this cap — they're the
// irreducible must-inline policy. Overridable via GB_PRIME_ADVICE_BUDGET_BYTES
// for tests and for agents that want to raise/lower it.
//
// Sized together with primeAdviceTitleByteBudget so the advice section caps
// near 20k tokens (~80 KB at 4 bytes/token) per the operator policy choice
// to give advice 20k tokens, everywhere (ka-0000006677). Inline beads still
// bypass.
const primeAdviceByteBudget = 60000

// primeAdviceTitleByteBudget is the additional budget reserved for title-only
// overflow lines. Once the abbreviated (title + summary + pointer) budget is
// exhausted, remaining items render as compact title-only lines
// (`**[Header]** Title — gb advice show ka-XXX`) up to this second budget.
// Surfaces every advice bead's title to the agent — at minimum — so nothing
// is silently dropped just because it sorts late. Overridable via
// GB_PRIME_ADVICE_TITLE_BUDGET_BYTES.
//
// Combined with primeAdviceByteBudget the advice section caps near 20k tokens
// (~80 KB at 4 bytes/token).
const primeAdviceTitleByteBudget = 20000

// primeAdviceDiffByteBudget and primeAdviceDiffTitleByteBudget are the much
// tighter defaults applied to the role-transition advice diff
// (outputAdviceDiffItems). A role diff is a supplementary delta shown on top of
// the agent's already-rendered advice section, so it must stay small even when
// the target role's catalog is large — it cannot inherit the 80 KB main-path
// cap. These honor the same GB_PRIME_ADVICE_BUDGET_BYTES /
// GB_PRIME_ADVICE_TITLE_BUDGET_BYTES overrides as the main path.
const primeAdviceDiffByteBudget = 4096
const primeAdviceDiffTitleByteBudget = 3072

// primeAdviceCheapByteBudget is the tighter abbreviated advice budget applied
// to cheap, high-frequency mechanical roles (see primeAdviceCheapRoles). These
// roles run on cheaper/smaller models and do a narrow mechanical job, so they
// do not need the full ~20k-token advice section a crew/gardener gets. A tighter
// abbreviated cap makes lower-priority advice spill to compact title-only lines
// — which still surface every advice title + `gb advice show` pointer, so
// NOTHING is dropped; the agent simply gets fewer full-summary items inline.
//
// Sized off the measured live catalog: the gardenio advice section renders
// ~28 KB / ~43 items at the default 60 KB cap (nowhere near binding), so a
// 16 KB abbreviated cap keeps the top ~half in full-summary form and title-only
// renders the rest — a measured ~10 KB (~2.5k-token) drop per session for these
// roles while every title still surfaces. Overridable, like the default, via
// GB_PRIME_ADVICE_BUDGET_BYTES (an explicit env override always wins, for
// every role).
const primeAdviceCheapByteBudget = 16000

// primeAdviceCheapRoles is the set of roles that receive
// primeAdviceCheapByteBudget instead of the default primeAdviceByteBudget.
// These are lightweight/mechanical roles (haiku-tier crews, single-purpose
// conversational and triage loops) identified from the live run-frequency
// census as high-volume yet narrow-scope — exactly the sessions where a full
// 20k-token advice prefix is the least amortized. The title-only overflow tier
// is unchanged, so every advice bead still surfaces at least as a title line.
//
// Matched on the PRIMARY role segment (see isCheapAdviceRole) so a
// comma-composed role that includes a non-cheap role keeps the full budget.
var primeAdviceCheapRoles = map[string]bool{
	"crew-ultralite": true,
	"phase0-haiku":   true,
	"thread":         true,
	"jira-mention":   true,
	"fsm-triage":     true,
	"auto-inspector": true,
}

// isCheapAdviceRole reports whether role should receive the tighter cheap-role
// advice budget. It matches on the primary (first, comma-delimited) role
// segment: a composed role like "crew-ultralite,crew" resolves to its first
// segment, and any role not in primeAdviceCheapRoles keeps the default budget.
func isCheapAdviceRole(role string) bool {
	primary := strings.TrimSpace(role)
	if i := strings.IndexByte(primary, ','); i >= 0 {
		primary = strings.TrimSpace(primary[:i])
	}
	return primeAdviceCheapRoles[primary]
}

// primeAdviceBudgetEnv is the env var name for overriding the byte budget.
const primeAdviceBudgetEnv = "GB_PRIME_ADVICE_BUDGET_BYTES"

// primeAdviceTitleBudgetEnv is the env var name for overriding the title-only
// overflow budget.
const primeAdviceTitleBudgetEnv = "GB_PRIME_ADVICE_TITLE_BUDGET_BYTES"

// primeAdviceSummaryMaxChars caps the per-item abbreviated summary rendered in
// gb prime output. The legacy behaviour was to show only the first line of the
// description, which on multi-line advice (the common case) clipped the body
// before the actionable payload — exact commands, URLs, "do X not Y" — which
// frequently live on line 2+. A ~500-char boundary-aware excerpt captures the
// full opening rule for every item (the longest observed first line is ~420
// chars) plus the immediate concrete payload, while leaving rationale and
// worked examples for `gb advice show <id>`. This stays well inside the 60 KB
// abbreviated budget (the firstLine era used only ~12 KB of it). Overridable
// via GB_PRIME_ADVICE_SUMMARY_CHARS.
const primeAdviceSummaryMaxChars = 500

// primeAdviceSummaryEnv is the env var name for overriding the per-item summary
// character cap.
const primeAdviceSummaryEnv = "GB_PRIME_ADVICE_SUMMARY_CHARS"

// adviceSummaryMaxChars returns the per-item summary cap, honoring
// GB_PRIME_ADVICE_SUMMARY_CHARS when set to a positive integer.
func adviceSummaryMaxChars() int {
	if v := os.Getenv(primeAdviceSummaryEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return primeAdviceSummaryMaxChars
}

// adviceBudgetBytes returns the abbreviated advice budget for the given role.
// An explicit GB_PRIME_ADVICE_BUDGET_BYTES override always wins (for every
// role); otherwise cheap mechanical roles (isCheapAdviceRole) get the tighter
// primeAdviceCheapByteBudget and all other roles get primeAdviceByteBudget.
func adviceBudgetBytes(role string) int {
	if v := os.Getenv(primeAdviceBudgetEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if isCheapAdviceRole(role) {
		return primeAdviceCheapByteBudget
	}
	return primeAdviceByteBudget
}

// adviceTitleBudgetBytes returns the title-only overflow budget, honoring
// GB_PRIME_ADVICE_TITLE_BUDGET_BYTES when set to a positive integer.
func adviceTitleBudgetBytes() int {
	if v := os.Getenv(primeAdviceTitleBudgetEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return primeAdviceTitleByteBudget
}

// adviceDiffBudgetBytes returns the abbreviated budget for the role-diff path,
// honoring GB_PRIME_ADVICE_BUDGET_BYTES when set to a positive integer and
// otherwise falling back to the tight diff-specific default.
func adviceDiffBudgetBytes() int {
	if v := os.Getenv(primeAdviceBudgetEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return primeAdviceDiffByteBudget
}

// adviceDiffTitleBudgetBytes returns the title-only overflow budget for the
// role-diff path, honoring GB_PRIME_ADVICE_TITLE_BUDGET_BYTES when set and
// otherwise falling back to the tight diff-specific default.
func adviceDiffTitleBudgetBytes() int {
	if v := os.Getenv(primeAdviceTitleBudgetEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return primeAdviceDiffTitleByteBudget
}

// outputAdvice fetches open advice beads, filters by agent subscriptions,
// groups by scope, and writes markdown to w.
func outputAdvice(w io.Writer, agentID, role string) {
	matched, _, err := advice.ListAdviceForAgent(context.Background(), daemon, agentID)
	if err != nil || len(matched) == 0 {
		return
	}

	// Guardrail: drop legacy hippo→advice mirror beads still present in the
	// DB. The hippo subsystem has been fully removed, but historical mirror
	// beads may remain. Both source:hippo-global mirrors (malformed
	// title/description that gets emitted 3×) and source:hippo-memory Q/A
	// gardener-cycle replays (ka-0000001799) are filtered out.
	matched = filterOutHippoMirrors(matched)
	if len(matched) == 0 {
		return
	}

	if jsonOutput {
		type jsonItem struct {
			ID            string   `json:"id"`
			Title         string   `json:"title"`
			Description   string   `json:"description,omitempty"`
			Labels        []string `json:"labels"`
			MatchedLabels []string `json:"matched_labels"`
		}
		items := make([]jsonItem, len(matched))
		for i, m := range matched {
			items[i] = jsonItem{
				ID:            m.Bead.ID,
				Title:         m.Bead.Title,
				Description:   m.Bead.Description,
				Labels:        m.Bead.Labels,
				MatchedLabels: m.MatchedLabels,
			}
		}
		data, _ := json.MarshalIndent(items, "", "  ")
		fmt.Fprintln(w, string(data))
		return
	}

	type scopeGroup struct {
		Scope  string
		Target string
		Header string
		Items  []advice.MatchedAdvice
	}

	groupMap := make(map[string]*scopeGroup)
	for _, m := range matched {
		key := m.Scope + ":" + m.ScopeTarget
		g, ok := groupMap[key]
		if !ok {
			g = &scopeGroup{
				Scope:  m.Scope,
				Target: m.ScopeTarget,
				Header: m.ScopeHeader,
			}
			groupMap[key] = g
		}
		g.Items = append(g.Items, m)
	}

	var groups []*scopeGroup
	for _, g := range groupMap {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return advice.GroupSortKey(groups[i].Scope, groups[i].Target) < advice.GroupSortKey(groups[j].Scope, groups[j].Target)
	})

	fmt.Fprintf(w, "\n## Advice (%d items)\n\n", len(matched))

	var items []adviceRenderItem
	for _, g := range groups {
		// Deterministic intra-group priority+recency sort BEFORE the byte-budget
		// cascade evicts anything, so the highest-priority / most-recent advice
		// is the last to spill to title-only or the footer (F1, ka-0000008175).
		// Group order (GroupSortKey) stays the primary priority axis; this
		// orders items underneath it.
		advice.SortByPriorityRecency(g.Items)
		for _, item := range g.Items {
			items = append(items, adviceRenderItem{Header: g.Header, Bead: item.Bead})
		}
	}
	renderAdviceItemsWithBudget(w, agentID, role, items)
}

// adviceRenderItem is a flat (header, bead) entry consumed by
// renderAdviceItemsWithBudget. Callers group/sort upstream.
type adviceRenderItem struct {
	Header string
	Bead   *beadsapi.BeadDetail
}

// renderAdviceItemsWithBudget writes a pre-ordered list of advice items using
// the role-tiered abbreviated budget (adviceBudgetBytes(role)) and the shared
// title-only overflow budget (adviceTitleBudgetBytes). It is the entry point
// for the primary outputAdvice path; callers that need a tighter cap (e.g.
// outputAdviceDiffItems) use renderAdviceItemsWithLimits. The title budget is
// deliberately NOT tiered by role, so cheap roles still surface every advice
// title even when their abbreviated summaries are capped tighter.
func renderAdviceItemsWithBudget(w io.Writer, agentID, role string, items []adviceRenderItem) {
	renderAdviceItemsWithLimits(w, agentID, items, adviceBudgetBytes(role), adviceTitleBudgetBytes())
}

// renderAdviceItemsWithLimits writes a pre-ordered list of advice items,
// applying the two-tier byte budget cascade shared by outputAdvice and
// outputAdviceRoleDiff. Inline beads always render in full and bypass the
// budget; abbreviated items accumulate up to budget; the rest fall through to
// title-only render up to titleBudget; anything past both budgets collapses
// into a single "... N more" footer keyed on agentID so the operator can fetch
// the full list via gb advice list --for.
func renderAdviceItemsWithLimits(w io.Writer, agentID string, items []adviceRenderItem, budget, titleBudget int) {
	var regularBuf, titleBuf bytes.Buffer
	type overflowItem struct {
		header string
		bead   *beadsapi.BeadDetail
	}
	var overflow []overflowItem
	for _, it := range items {
		if isInlineAdvice(it.Bead) {
			renderInlineAdviceItem(w, it.Header, it.Bead)
			continue
		}
		var one bytes.Buffer
		renderAbbrevAdviceItem(&one, it.Header, it.Bead)
		if regularBuf.Len()+one.Len() > budget {
			overflow = append(overflow, overflowItem{header: it.Header, bead: it.Bead})
			continue
		}
		one.WriteTo(&regularBuf)
	}
	regularBuf.WriteTo(w)

	truncated := 0
	for _, ov := range overflow {
		var one bytes.Buffer
		renderTitleOnlyAdviceItem(&one, ov.header, ov.bead)
		if titleBuf.Len()+one.Len() > titleBudget {
			truncated++
			continue
		}
		one.WriteTo(&titleBuf)
	}
	titleBuf.WriteTo(w)

	if truncated > 0 {
		fmt.Fprintf(w, "... %d more advice items not shown (budget exceeded). Run `gb advice list --for %s` to see all.\n\n",
			truncated, agentID)
	}
}

// renderTitleOnlyAdviceItem writes the most compact rendering of an advice
// bead: title + `gb advice show` pointer, no summary line. Used as the
// overflow form once the abbreviated budget is exhausted, so every advice
// still surfaces at least as a title. Delegates to advice.RenderTitleOnlyItem —
// the canonical rendering shared with the controller render-size metric.
func renderTitleOnlyAdviceItem(w io.Writer, header string, b *beadsapi.BeadDetail) {
	advice.RenderTitleOnlyItem(w, header, b)
}

// renderInlineAdviceItem writes the full-text (inline) rendering of an advice
// bead. Delegates to advice.RenderInlineItem (the canonical rendering).
func renderInlineAdviceItem(w io.Writer, header string, b *beadsapi.BeadDetail) {
	advice.RenderInlineItem(w, header, b)
}

// renderAbbrevAdviceItem writes the abbreviated pointer rendering of an advice
// bead. Delegates to advice.RenderAbbrevItem so the bytes gb prime emits and the
// bytes the R4 tripwire metric measures are produced by one code path
// (ka-0000002748). Honors the GB_PRIME_ADVICE_SUMMARY_CHARS override.
func renderAbbrevAdviceItem(w io.Writer, header string, b *beadsapi.BeadDetail) {
	advice.RenderAbbrevItem(w, header, b, adviceSummaryMaxChars())
}

// isInlineAdvice returns true if the bead carries the "inline:true" label.
// Delegates to advice.IsInline.
func isInlineAdvice(b *beadsapi.BeadDetail) bool {
	return advice.IsInline(b)
}

// isHippoMirror returns true if the bead is a legacy hippo→advice mirror
// artifact from the removed hippo subsystem — see advice.IsHippoMirror, the
// shared predicate this delegates to so gb prime, gb advice list, gb advice
// audit, and the controller catalog metrics can never drift apart
// (ka-0000002735 parity contract).
func isHippoMirror(b *beadsapi.BeadDetail) bool {
	return advice.IsHippoMirror(b)
}

// filterOutHippoMirrors returns matches whose beads are not legacy hippo
// mirror artifacts. Preserves input order.
func filterOutHippoMirrors(matches []advice.MatchedAdvice) []advice.MatchedAdvice {
	out := matches[:0]
	for _, m := range matches {
		if isHippoMirror(m.Bead) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}
