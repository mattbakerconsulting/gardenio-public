package main

import (
	"strings"
	"testing"
)

func TestRenderUserLink(t *testing.T) {
	tests := []struct {
		name     string
		display  string
		slug     string
		basePath string
		want     string
	}{
		{
			name:    "empty display returns empty",
			display: "",
			slug:    "matt-baker",
			want:    "",
		},
		{
			name:    "whitespace display returns empty",
			display: "   ",
			slug:    "matt-baker",
			want:    "",
		},
		{
			name:    "no slug renders plain escaped text",
			display: "Matt Baker",
			slug:    "",
			want:    "Matt Baker",
		},
		{
			name:    "plain text escapes HTML special chars",
			display: "<script>",
			slug:    "",
			want:    "&lt;script&gt;",
		},
		{
			name:    "slug renders /u/{slug} link",
			display: "Matt Baker",
			slug:    "matt-baker",
			want:    `<a class="user-link" href="/u/matt-baker" title="Open Matt Baker&#39;s homepage">Matt Baker</a>`,
		},
		{
			name:     "slug honors base path",
			display:  "Matt Baker",
			slug:     "matt-baker",
			basePath: "/viewer",
			want:     `<a class="user-link" href="/viewer/u/matt-baker" title="Open Matt Baker&#39;s homepage">Matt Baker</a>`,
		},
		{
			name:    "display with quotes is escaped in title and body",
			display: `Matt "M.B." Baker`,
			slug:    "matt-baker",
			// Body and title both reflect HTML-escaped quotes; title attribute uses
			// the same escape so apostrophe stays consistent.
			want: `<a class="user-link" href="/u/matt-baker" title="Open Matt &#34;M.B.&#34; Baker&#39;s homepage">Matt &#34;M.B.&#34; Baker</a>`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(renderUserLink(tc.display, tc.slug, tc.basePath))
			if got != tc.want {
				t.Errorf("renderUserLink(%q, %q, %q):\n  got  %s\n  want %s", tc.display, tc.slug, tc.basePath, got, tc.want)
			}
		})
	}
}

// Smoke-test that the helper survives malicious slug inputs without producing
// a broken-out attribute. We don't claim defense-in-depth here — slugs come
// from the daemon, not user input — but the escape should still hold.
func TestRenderUserLinkEscapesSlug(t *testing.T) {
	got := string(renderUserLink("alice", `evil"`, ""))
	if strings.Contains(got, `href="/u/evil"`) {
		t.Errorf("slug not escaped; got %s", got)
	}
}
