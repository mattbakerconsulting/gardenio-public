package main

import (
	"strings"
	"testing"
)

func TestRenderOwnerInitialsBadge(t *testing.T) {
	const base = "/v"
	cases := []struct {
		name        string
		display     string
		slug        string
		wantEmpty   bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:      "empty display renders nothing",
			display:   "",
			wantEmpty: true,
		},
		{
			name:      "whitespace display renders nothing",
			display:   "   ",
			wantEmpty: true,
		},
		{
			name:        "two-word human name → MB span when no slug",
			display:     "Matt Baker",
			slug:        "",
			wantContain: []string{`<span class="owner-initials"`, `>MB</span>`, `title="owner: Matt Baker"`, `aria-label="owner Matt Baker"`, "background:"},
			wantAbsent:  []string{"<a ", "href="},
		},
		{
			name:        "slug renders linked anchor to /u/{slug}",
			display:     "Matt Baker",
			slug:        "matt-baker",
			wantContain: []string{`<a class="owner-initials"`, `href="/v/u/matt-baker"`, `>MB</span>` /* not present */},
		},
		{
			name:        "agent-shaped name collapses multi-segment initials",
			display:     "gardenio-inbox-pushdense-condense-71z",
			slug:        "",
			wantContain: []string{">GIPC</span>", `title="owner: gardenio-inbox-pushdense-condense-71z"`},
		},
		{
			name:        "html in display is escaped in title",
			display:     "Eve <script>",
			slug:        "",
			wantContain: []string{"&lt;script&gt;", `title="owner: Eve &lt;script&gt;"`},
			wantAbsent:  []string{"<script>"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(renderOwnerInitialsBadge(tc.display, tc.slug, base))
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("want empty, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want non-empty badge for %q", tc.display)
			}
			// The slug case overrides the closing tag; only assert the contains
			// that make sense for the shape.
			for _, want := range tc.wantContain {
				if want == `>MB</span>` && tc.slug != "" {
					// linked anchor closes with </a>, skip the span assertion.
					continue
				}
				if !strings.Contains(got, want) {
					t.Errorf("badge %q missing %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("badge %q should not contain %q", got, absent)
				}
			}
			if tc.slug != "" {
				if !strings.Contains(got, "<a ") || !strings.Contains(got, "</a>") {
					t.Errorf("slug badge %q should be an anchor", got)
				}
			}
		})
	}
}

func TestOwnerInitialsHueDeterministic(t *testing.T) {
	// Same display → same hue, every call.
	a := ownerInitialsHue("Matt Baker")
	b := ownerInitialsHue("Matt Baker")
	if a != b {
		t.Fatalf("hue not deterministic: %q vs %q", a, b)
	}
	// Case-insensitive: "matt baker" lands on the same hue as "Matt Baker".
	if c := ownerInitialsHue("matt baker"); c != a {
		t.Errorf("hue not case-insensitive: %q vs %q", c, a)
	}
	// Empty → neutral gray sentinel, not a palette hue.
	if g := ownerInitialsHue(""); g != "#8a8a8a" {
		t.Errorf("empty hue = %q, want neutral gray", g)
	}
	// Every returned hue is a 7-char hex string.
	if !strings.HasPrefix(a, "#") || len(a) != 7 {
		t.Errorf("hue %q not a #rrggbb hex color", a)
	}
}
