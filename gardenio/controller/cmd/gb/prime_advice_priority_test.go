package main

// prime_advice_priority_test.go covers F1 (ka-0000008175): the deterministic
// priority+recency sort applied to each scope group BEFORE the byte-budget
// cascade evicts anything, so a high-priority recent bead is retained (rendered
// abbreviated) over a low-priority stale one when budget forces a drop.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// TestAdvicePriorityRecency_HighPriorityRetainedUnderBudget seeds two advice
// beads in the SAME scope group — a high-priority recent one and a low-priority
// stale one — with a byte budget that fits exactly one abbreviated item. After
// the priority+recency sort, the high-priority recent bead must render
// abbreviated (with its summary) while the low-priority stale bead spills to
// the title-only tier. This is the exact autonomy regression F1 defends
// against: the important instruction surviving budget pressure.
func TestAdvicePriorityRecency_HighPriorityRetainedUnderBudget(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)

	// Intentionally supply the low-value bead FIRST so the pre-sort (server)
	// order would evict the high-value one — the sort must reverse this.
	group := []advice.MatchedAdvice{
		{Bead: &beadsapi.BeadDetail{
			ID:          "gd-lowStale",
			Title:       "Low priority stale rule",
			Description: "A stale low-value note that used to win eviction by luck of server order.",
			Priority:    4,
			UpdatedAt:   old,
			CreatedAt:   old,
			Labels:      []string{"global"},
		}},
		{Bead: &beadsapi.BeadDetail{
			ID:          "gd-highFresh",
			Title:       "High priority fresh rule",
			Description: "A critical, freshly-written rule the agent must not lose under budget pressure.",
			Priority:    0,
			UpdatedAt:   fresh,
			CreatedAt:   fresh,
			Labels:      []string{"global"},
		}},
	}

	advice.SortByPriorityRecency(group)

	var items []adviceRenderItem
	for _, m := range group {
		items = append(items, adviceRenderItem{Header: "Global", Bead: m.Bead})
	}

	// A budget that fits exactly one abbreviated item forces the other to the
	// title-only tier. Abbreviated render is ~150 bytes here; 200 fits one.
	var buf bytes.Buffer
	renderAdviceItemsWithLimits(&buf, "gardenio/crews/test", items, 200, 16384)
	got := buf.String()

	// The high-priority fresh bead must render ABBREVIATED — its summary text
	// present.
	if !strings.Contains(got, "critical, freshly-written rule") {
		t.Errorf("high-priority fresh bead was evicted from abbreviated tier:\n%s", got)
	}
	// The low-priority stale bead must be the one degraded to title-only — its
	// summary text must be ABSENT (title-only render drops the summary).
	if strings.Contains(got, "stale low-value note") {
		t.Errorf("low-priority stale bead should have spilled to title-only, but rendered abbreviated:\n%s", got)
	}
	// Both beads still surface (truncate, don't drop): the stale one at least as
	// a title-only pointer.
	if !strings.Contains(got, "`gb advice show gd-lowStale`") {
		t.Errorf("low-priority stale bead dropped entirely instead of title-only:\n%s", got)
	}

	// The high-priority abbreviated line must appear BEFORE the low-priority
	// title-only line in the rendered output (eviction order is stable).
	hi := strings.Index(got, "gd-highFresh")
	lo := strings.Index(got, "gd-lowStale")
	if hi < 0 || lo < 0 || hi > lo {
		t.Errorf("expected high-priority bead rendered before low-priority: hi=%d lo=%d\n%s", hi, lo, got)
	}
}

// TestAdvicePriorityRecency_DeterministicRender asserts the render is
// reproducible: the same corpus fed in two different input orders produces
// byte-identical output after the sort.
func TestAdvicePriorityRecency_DeterministicRender(t *testing.T) {
	t1 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	mk := func(id, title string, prio int, upd time.Time) advice.MatchedAdvice {
		return advice.MatchedAdvice{Bead: &beadsapi.BeadDetail{
			ID: id, Title: title, Description: title + " body.",
			Priority: prio, UpdatedAt: upd, CreatedAt: upd, Labels: []string{"global"},
		}}
	}
	base := []advice.MatchedAdvice{
		mk("gd-1", "Rule one", 2, t1),
		mk("gd-2", "Rule two", 0, t2),
		mk("gd-3", "Rule three", 2, t3),
		mk("gd-4", "Rule four", 1, t2),
	}

	render := func(in []advice.MatchedAdvice) string {
		cp := append([]advice.MatchedAdvice(nil), in...)
		advice.SortByPriorityRecency(cp)
		var items []adviceRenderItem
		for _, m := range cp {
			items = append(items, adviceRenderItem{Header: "Global", Bead: m.Bead})
		}
		var buf bytes.Buffer
		// Tight budget so the cascade actually evicts, exercising order-sensitivity.
		renderAdviceItemsWithLimits(&buf, "gardenio/crews/test", items, 180, 180)
		return buf.String()
	}

	fwd := render(base)
	rev := make([]advice.MatchedAdvice, len(base))
	for i := range base {
		rev[len(base)-1-i] = base[i]
	}
	if got := render(rev); got != fwd {
		t.Errorf("render not deterministic across input permutations:\nforward:\n%s\nreversed:\n%s", fwd, got)
	}
}
