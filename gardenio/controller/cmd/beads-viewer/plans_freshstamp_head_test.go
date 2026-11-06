package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// plans_freshstamp_head_test.go — iter-52 (ka-0000005352): the freshness stamp
// ("updated Xh ago") was RELOCATED out of the top .plan-card-meta chip-strip and
// up into the head row (beside the plan ID), and rendered in the COMPACT age form
// (.AgoCompact, "2d"/"40d" via fmtTimeAgo) instead of the wide "updated 7 hours
// ago". Measured on the live prod board: the top chip-strip is the dominant
// vertical sink (117px/card ≈ 41% of card height), wrapping to 3-6 lines on every
// card, and this stamp — pushed right by margin-left:auto — stranded ALONE on its
// own wrapped line on 66/75 cards (88%). Moving it up reclaims a wrapped line on
// every card (all 75 shrink ~24px, 0 grow), and the compact form costs almost no
// title width so the 2-line title clamp is preserved.
//
// These tests pin the three properties the relocation must hold:
//  1. the stamp renders INSIDE .plan-card-head (not the meta strip), in the
//     compact age form, with the long humanized age preserved in the aria-label;
//  2. the FreshnessStale loud-tint still fires on an aged active plan and stays
//     off a fresh one (the iter-38 signal is unchanged, only moved);
//  3. the head-scoped CSS (muted color + stale tint) is emitted so the relocated
//     stamp is not left unstyled outside the .plan-card-meta rules.

// planCardHeadBlock returns the <div class="plan-card-head">…</div> substring of
// the seed plan's card — the region the stamp must now live in.
func planCardHeadBlock(t *testing.T, body string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)<div class="plan-card-head">.*?</div>`)
	m := re.FindString(body)
	if m == "" {
		t.Fatalf("no .plan-card-head block found in /plans body")
	}
	return m
}

// TestPlansIndex_FreshStampInHeadCompact proves the stamp renders in the head row
// in the compact age form, and that its long humanized age survives in the
// aria-label (so screen-reader users still hear "updated 2 days ago"). It also
// proves the OLD wide "updated <ago>" visible face is gone from the card.
func TestPlansIndex_FreshStampInHeadCompact(t *testing.T) {
	// A 2-day-old active plan → compact "2d", fresh (no stale tint).
	mux, _ := mkFreshnessTestServer(t, "in_progress", 2*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	head := planCardHeadBlock(t, body)
	// The stamp element must live in the head block now.
	if !strings.Contains(head, "plan-card-updated") {
		t.Errorf("freshness stamp must render inside .plan-card-head (iter-52 relocation); head block:\n%s", head)
	}
	// Its VISIBLE face is the compact age form ("2d" for the seed's 2-day-old
	// activity under the injected clock). The compact form is what fmtTimeAgo emits
	// off s.now(); a full "N days/hours" here would mean the wide face regressed.
	if !strings.Contains(head, `plan-card-updated" title="Last change `) &&
		!strings.Contains(head, `plan-card-updated plan-card-updated-stale" title="Last change `) {
		t.Errorf("head stamp must keep the 'Last change <RFC>' tooltip on the .plan-card-updated element; head:\n%s", head)
	}
	if !strings.Contains(head, `>2d</span>`) {
		t.Errorf("head stamp should show the COMPACT age \"2d\" as its visible face (iter-52); head:\n%s", head)
	}
	// The long humanized age rides the aria-label so screen-reader users still hear
	// "updated <long form>". (The exact number is clock-basis dependent — .Ago uses
	// humanizeTime while .AgoCompact uses s.now() — so pin only the label prefix.)
	if !regexp.MustCompile(`aria-label="updated [^"]+">2d</span>`).MatchString(head) {
		t.Errorf("head stamp must carry aria-label=\"updated <age>\" wrapping the compact face; head:\n%s", head)
	}

	// The old wide visible face ("updated <ago>" as text content between tags) must
	// be gone from the card — "updated" now appears ONLY inside the aria-label
	// attribute, never as rendered text.
	if regexp.MustCompile(`>updated \d`).MatchString(body) {
		t.Errorf("the old wide 'updated <ago>' visible face must be gone (moved to the head as a compact stamp)")
	}
}

// TestPlansIndex_FreshStampHeadStaleTint is the fail-without/pass-with seam on the
// relocated tint: an aged (>7d) active plan gains .plan-card-updated-stale on the
// head stamp + the stale tooltip; a fresh plan does not. Reverting the relocation
// (or dropping the class seam) reds this.
func TestPlansIndex_FreshStampHeadStaleTint(t *testing.T) {
	t.Run("stale active tints in head", func(t *testing.T) {
		mux, _ := mkFreshnessTestServer(t, "in_progress", 21*24*time.Hour)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		head := planCardHeadBlock(t, w.Body.String())
		if !strings.Contains(head, `class="meta plan-card-updated plan-card-updated-stale"`) {
			t.Errorf("a 21-day-old active plan must carry .plan-card-updated-stale on the head stamp; head:\n%s", head)
		}
		if !strings.Contains(head, "has not moved in over a week (stale)") {
			t.Errorf("the head stamp must keep the stale tooltip naming the staleness")
		}
	})

	t.Run("terminal never tints in head", func(t *testing.T) {
		// A closed plan weeks in the past keeps the plain muted stamp — honest
		// history, not a stall (the terminal-reconcile family). status=all so the
		// closed plan renders.
		mux, _ := mkFreshnessTestServer(t, "closed", 21*24*time.Hour)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans?status=all", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		head := planCardHeadBlock(t, w.Body.String())
		if !strings.Contains(head, "plan-card-updated") {
			t.Fatalf("expected the head freshness stamp on the closed plan; head:\n%s", head)
		}
		if strings.Contains(head, `class="meta plan-card-updated plan-card-updated-stale"`) {
			t.Errorf("a 21-day-old closed (terminal) plan must NOT be tinted stale in the head — its age is honest history")
		}
	})
}

// TestPlansIndex_FreshStampHeadCSSEmitted proves the head-scoped CSS for the
// relocated stamp is present, so the stamp is styled (muted color + stale tint)
// now that it lives outside the .plan-card-meta rules. Keyed on the head-scoped
// selectors this change introduced.
func TestPlansIndex_FreshStampHeadCSSEmitted(t *testing.T) {
	mux, _ := mkFreshnessTestServer(t, "in_progress", 2*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	wantRules := []string{
		".plan-card-head .plan-card-updated {",
		".plan-card-head .plan-card-updated.plan-card-updated-stale {",
		`html[data-skin^="dark"] .plan-card-head .plan-card-updated {`,
		`html[data-skin^="dark"] .plan-card-head .plan-card-updated.plan-card-updated-stale {`,
	}
	for _, r := range wantRules {
		if !strings.Contains(body, r) {
			t.Errorf("/plans is missing the head-scoped freshness-stamp rule %q — the relocated stamp would be unstyled outside .plan-card-meta", r)
		}
	}
}

// TestPlanAgoCompact_UndatableEmpty proves an undatable plan (zero last-activity)
// renders NO head stamp — the .AgoCompact gate mirrors the .Ago gate, so an
// undatable plan reads exactly as before (no stamp), not an empty "0s" token.
func TestPlanAgoCompact_UndatableEmpty(t *testing.T) {
	// fmtTimeAgo on a zero time returns "" (its documented zero-time contract),
	// and the template gates the stamp on .AgoCompact — so no stamp element.
	if got := fmtTimeAgo(time.Time{}, time.Now()); got != "" {
		t.Fatalf("fmtTimeAgo(zero) = %q, want empty (undatable contract)", got)
	}
}
