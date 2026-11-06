package main

import (
	"strings"
	"testing"
)

func TestOperatorInitials(t *testing.T) {
	cases := []struct {
		name string
		chip operatorChip
		want string
	}{
		{
			name: "empty display falls back to ??",
			chip: operatorChip{Kind: "human", Display: "", RawID: ""},
			want: "??",
		},
		{
			name: "whitespace display falls back to ??",
			chip: operatorChip{Kind: "human", Display: "   ", RawID: "slack:U123"},
			want: "??",
		},
		{
			name: "raw slack id echoed as display is unresolved",
			chip: operatorChip{Kind: "human", Display: "slack:U0OPERATOR1", RawID: "slack:U0OPERATOR1"},
			want: "??",
		},
		{
			name: "raw oidc id echoed as display is unresolved",
			chip: operatorChip{Kind: "human", Display: "oidc:alice@example.com", RawID: "oidc:alice@example.com"},
			want: "??",
		},
		{
			name: "two-word human name → MB",
			chip: operatorChip{Kind: "human", Display: "Matt Baker", RawID: "slack:U123"},
			want: "MB",
		},
		{
			name: "three-word human name → MJB (capped)",
			chip: operatorChip{Kind: "human", Display: "Matt J Baker", RawID: "slack:U123"},
			want: "MJB",
		},
		{
			name: "four-word name capped at three",
			chip: operatorChip{Kind: "human", Display: "Matt J. Baker Jr", RawID: "slack:U123"},
			want: "MJB",
		},
		{
			name: "single-segment name → first two runes",
			chip: operatorChip{Kind: "human", Display: "alice", RawID: "alice@example.com"},
			want: "AL",
		},
		{
			name: "agent display with hyphens → GGD (capped 3)",
			chip: operatorChip{Kind: "agent", Display: "gardenio-gardener-delivery", RawID: "gardenio-gardener-delivery"},
			want: "GGD",
		},
		{
			name: "agent name with four segments → first three",
			chip: operatorChip{Kind: "agent", Display: "gardenio-inbox-pushdense-condense", RawID: "gardenio-inbox-pushdense-condense"},
			want: "GIP",
		},
		{
			name: "dotted name → MT",
			chip: operatorChip{Kind: "human", Display: "mike.tilkov", RawID: "oidc:mike@example.com"},
			want: "MT",
		},
		{
			name: "single letter",
			chip: operatorChip{Kind: "human", Display: "a", RawID: "alice"},
			want: "A",
		},
		{
			name: "display equals raw id but no scheme prefix → use display",
			chip: operatorChip{Kind: "agent", Display: "agent-foo", RawID: "agent-foo"},
			want: "AF",
		},
		{
			name: "display differs from raw slack id → use display",
			chip: operatorChip{Kind: "human", Display: "Alice Example", RawID: "slack:U0OPERATOR1"},
			want: "AE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := operatorInitials(tc.chip)
			if got != tc.want {
				t.Errorf("operatorInitials(%+v) = %q; want %q", tc.chip, got, tc.want)
			}
			// Pin: raw slack:U… never leaks into the monogram.
			if strings.Contains(got, "slack:") || strings.Contains(got, "oidc:") {
				t.Errorf("operatorInitials(%+v) = %q; leaked raw scheme prefix", tc.chip, got)
			}
		})
	}
}

func TestRenderOperatorChipCompact_Human(t *testing.T) {
	chip := operatorChip{
		Kind:       "human",
		Display:    "Matt Baker",
		RawID:      "slack:U0OPERATOR1",
		FilterHref: "/b/search?operated_by=slack%3AU0OPERATOR1",
	}
	out := string(renderOperatorChipCompact(chip))
	if !strings.Contains(out, `class="operator-chip-compact"`) {
		t.Errorf("missing base compact class: %s", out)
	}
	if strings.Contains(out, "is-viewer-self") {
		t.Errorf("is-viewer-self should not fire when IsViewerSelf=false: %s", out)
	}
	if strings.Contains(out, "is-agent") {
		t.Errorf("is-agent should not fire on human chip: %s", out)
	}
	if !strings.Contains(out, ">MB<") {
		t.Errorf("missing MB monogram: %s", out)
	}
	if !strings.Contains(out, `title="Matt Baker"`) {
		t.Errorf("missing title attribute: %s", out)
	}
	if !strings.Contains(out, `data-operated-by="slack:U0OPERATOR1"`) {
		t.Errorf("missing data-operated-by: %s", out)
	}
	if !strings.Contains(out, `href="/b/search?operated_by=slack%3AU0OPERATOR1"`) {
		t.Errorf("missing filter href: %s", out)
	}
	if !strings.HasPrefix(out, "<a ") {
		t.Errorf("expected anchor when FilterHref set: %s", out)
	}
	// Pin: never emit raw slack: in the monogram body.
	if strings.Contains(out, ">slack:") {
		t.Errorf("raw slack: leaked into body: %s", out)
	}
}

func TestRenderOperatorChipCompact_ViewerSelf(t *testing.T) {
	chip := operatorChip{
		Kind:         "human",
		Display:      "Matt Baker",
		RawID:        "oidc:matt@example.com",
		FilterHref:   "/b/search?operated_by=oidc%3Amatt%40example.com",
		IsViewerSelf: true,
	}
	out := string(renderOperatorChipCompact(chip))
	if !strings.Contains(out, "operator-chip-compact is-viewer-self") {
		t.Errorf("missing is-viewer-self class: %s", out)
	}
	if !strings.Contains(out, ">MB<") {
		t.Errorf("missing MB monogram: %s", out)
	}
}

func TestRenderOperatorChipCompact_ViewerSelfRequiresMatch(t *testing.T) {
	// Resolve via the real pipeline so IsViewerSelf only fires when the
	// viewer's email matches the chip operator. Pins that resolveOperatorChip
	// is the single gate for the is-viewer-self class.
	mine := resolveOperatorChip("oidc:matt@example.com", "Matt Baker", "matt@example.com", "", "agent", "/b", "matt@example.com")
	if !mine.IsViewerSelf {
		t.Fatal("expected IsViewerSelf=true for matching viewer")
	}
	mineOut := string(renderOperatorChipCompact(mine))
	if !strings.Contains(mineOut, "is-viewer-self") {
		t.Errorf("matching viewer email should set is-viewer-self: %s", mineOut)
	}

	other := resolveOperatorChip("oidc:alice@example.com", "Alice Example", "alice@example.com", "", "agent", "/b", "matt@example.com")
	if other.IsViewerSelf {
		t.Fatal("expected IsViewerSelf=false for non-matching viewer")
	}
	otherOut := string(renderOperatorChipCompact(other))
	if strings.Contains(otherOut, "is-viewer-self") {
		t.Errorf("non-matching viewer should NOT set is-viewer-self: %s", otherOut)
	}

	anon := resolveOperatorChip("oidc:matt@example.com", "Matt Baker", "matt@example.com", "", "agent", "/b", "")
	if anon.IsViewerSelf {
		t.Fatal("expected IsViewerSelf=false when viewer is anonymous")
	}
	anonOut := string(renderOperatorChipCompact(anon))
	if strings.Contains(anonOut, "is-viewer-self") {
		t.Errorf("anonymous viewer should NOT set is-viewer-self: %s", anonOut)
	}
}

func TestRenderOperatorChipCompact_Agent(t *testing.T) {
	chip := operatorChip{
		Kind:       "agent",
		Display:    "gardenio-gardener-delivery",
		RawID:      "gardenio-gardener-delivery",
		FilterHref: "/b/search?operated_by=gardenio-gardener-delivery",
	}
	out := string(renderOperatorChipCompact(chip))
	if !strings.Contains(out, "operator-chip-compact is-agent") {
		t.Errorf("missing is-agent class: %s", out)
	}
	if strings.Contains(out, "is-viewer-self") {
		t.Errorf("is-viewer-self should not fire on agent chip: %s", out)
	}
	if !strings.Contains(out, ">GGD<") {
		t.Errorf("missing GGD monogram: %s", out)
	}
	if !strings.Contains(out, `title="gardenio-gardener-delivery"`) {
		t.Errorf("missing title attribute: %s", out)
	}
}

func TestRenderOperatorChipCompact_Unresolved(t *testing.T) {
	// Server-resolved display was empty so the resolver echoed the raw
	// `slack:U…` into Display. Compact chip must render "??" instead of
	// the raw id.
	chip := resolveOperatorChip("slack:U0OPERATOR1", "", "", "", "agent", "/b", "")
	out := string(renderOperatorChipCompact(chip))
	if !strings.Contains(out, ">??<") {
		t.Errorf("expected ?? fallback for unresolved chip: %s", out)
	}
	if strings.Contains(out, ">slack:") {
		t.Errorf("raw slack: leaked into compact chip body: %s", out)
	}
	// Even unresolved, the underlying raw id and title are preserved for
	// hover so the operator can still investigate.
	if !strings.Contains(out, `data-operated-by="slack:U0OPERATOR1"`) {
		t.Errorf("missing data-operated-by on unresolved chip: %s", out)
	}
}

func TestRenderOperatorChipCompact_NoLink(t *testing.T) {
	// When no FilterHref is set (e.g. basePath empty) the compact chip
	// should fall back to a <span> wrapper instead of an <a>.
	chip := operatorChip{
		Kind:    "human",
		Display: "Matt Baker",
		RawID:   "slack:U123",
	}
	out := string(renderOperatorChipCompact(chip))
	if !strings.HasPrefix(out, "<span ") {
		t.Errorf("expected span wrapper when FilterHref empty: %s", out)
	}
	if strings.Contains(out, "<a ") {
		t.Errorf("should not emit anchor when FilterHref empty: %s", out)
	}
}

func TestRenderOperatorChipCompact_NonChipKindsAreEmpty(t *testing.T) {
	// scheduler / empty / self / suppressed render nothing — they have
	// their own treatments and the compact chip doesn't add value.
	for _, kind := range []string{"scheduler", "empty", "self", "suppressed", ""} {
		chip := operatorChip{Kind: kind, Display: "x", RawID: "x"}
		out := string(renderOperatorChipCompact(chip))
		if out != "" {
			t.Errorf("kind %q: expected empty, got %q", kind, out)
		}
	}
}
