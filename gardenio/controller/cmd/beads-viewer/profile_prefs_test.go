package main

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// TestParseProfilePref covers the same shapes as the legacy reader plus the
// new HiddenSections / SectionOrder fields.
func TestParseProfilePref(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want ProfilePref
	}{
		{"nil-fields", nil, ProfilePref{}},
		{"empty-string", map[string]string{"profile_prefs": ""}, ProfilePref{}},
		{"garbage", map[string]string{"profile_prefs": "not json"}, ProfilePref{}},
		{"empty-object", map[string]string{"profile_prefs": "{}"}, ProfilePref{}},
		{
			"pins-only",
			map[string]string{"profile_prefs": `{"pinned_bead_ids":["gd-a"]}`},
			ProfilePref{PinnedBeadIDs: []string{"gd-a"}},
		},
		{
			"sections",
			map[string]string{"profile_prefs": `{"hidden_sections":["pinned"],"section_order":["open-mrs","pinned"]}`},
			ProfilePref{
				HiddenSections: []string{"pinned"},
				SectionOrder:   []string{"open-mrs", "pinned"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProfilePref(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseProfilePref(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeProfilePref drops unknown / duplicate section slugs, leaves
// PinnedBeadIDs alone except for trimming empties + dedup.
func TestNormalizeProfilePref(t *testing.T) {
	in := ProfilePref{
		PinnedBeadIDs:  []string{"gd-a", "", "gd-a", "gd-b"},
		HiddenSections: []string{"pinned", "bogus", "pinned", "open-mrs"},
		SectionOrder:   []string{"open-mrs", "bogus", "open-mrs", "pinned"},
	}
	got := normalizeProfilePref(in)
	want := ProfilePref{
		PinnedBeadIDs:  []string{"gd-a", "gd-b"},
		HiddenSections: []string{"pinned", "open-mrs"},
		SectionOrder:   []string{"open-mrs", "pinned"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalize() = %+v, want %+v", got, want)
	}
}

// TestApplyProfilePref_NoPrefs leaves the input untouched.
func TestApplyProfilePref_NoPrefs(t *testing.T) {
	in := []userHomeSection{
		{Slug: sectionPinned, Title: "Pinned"},
		{Slug: sectionOpenMRs, Title: "Open MRs"},
	}
	got := applyProfilePref(in, ProfilePref{})
	if !reflect.DeepEqual(got, in) {
		t.Errorf("expected pass-through, got %+v", got)
	}
}

// TestApplyProfilePref_HiddenAndReorder filters out hidden sections and
// honors the explicit order, with un-listed sections appended in default
// order at the tail.
func TestApplyProfilePref_HiddenAndReorder(t *testing.T) {
	in := []userHomeSection{
		{Slug: sectionPinned},
		{Slug: sectionActiveAgents},
		{Slug: sectionOpenDecisions},
		{Slug: sectionOpenMRs},
	}
	prefs := ProfilePref{
		HiddenSections: []string{sectionOpenDecisions},
		SectionOrder:   []string{sectionOpenMRs, sectionPinned},
	}
	got := applyProfilePref(in, prefs)
	gotSlugs := make([]string, len(got))
	for i, s := range got {
		gotSlugs[i] = s.Slug
	}
	want := []string{sectionOpenMRs, sectionPinned, sectionActiveAgents}
	if !reflect.DeepEqual(gotSlugs, want) {
		t.Errorf("order = %v, want %v", gotSlugs, want)
	}
}

// TestApplyProfilePref_UnknownSlugInOrderIgnored guards against the editor
// shipping a slug the controller doesn't recognize: the slug is silently
// dropped so the remaining order still composes a valid render plan.
func TestApplyProfilePref_UnknownSlugInOrderIgnored(t *testing.T) {
	in := []userHomeSection{
		{Slug: sectionPinned},
		{Slug: sectionOpenMRs},
	}
	prefs := ProfilePref{SectionOrder: []string{"made-up", sectionOpenMRs}}
	got := applyProfilePref(in, prefs)
	if len(got) != 2 || got[0].Slug != sectionOpenMRs || got[1].Slug != sectionPinned {
		t.Errorf("unexpected order %+v", got)
	}
}

// TestBuildProfileSectionView returns all 8 sections, honoring custom
// order + visibility.
func TestBuildProfileSectionView(t *testing.T) {
	p := ProfilePref{
		HiddenSections: []string{sectionRecentMerged, sectionForwardedMentions},
		SectionOrder:   []string{sectionOpenMRs, sectionPinned},
	}
	got := buildProfileSectionView(p)
	if len(got) != len(allProfileSections) {
		t.Fatalf("expected %d rows, got %d", len(allProfileSections), len(got))
	}
	// First two rows reflect explicit order.
	if got[0].Slug != sectionOpenMRs || got[1].Slug != sectionPinned {
		t.Errorf("explicit order not honored: %+v", got[:2])
	}
	// Hidden rows present but with Visible=false.
	for _, row := range got {
		switch row.Slug {
		case sectionRecentMerged, sectionForwardedMentions:
			if row.Visible {
				t.Errorf("section %s should be hidden", row.Slug)
			}
		default:
			if !row.Visible {
				t.Errorf("section %s should be visible", row.Slug)
			}
		}
	}
}

// TestParseProfileFromForm assembles a ProfilePref from a settings POST
// body. Sections without a section_visible_<slug> field are hidden; the
// section_order csv drives ordering; PinnedBeadIDs are preserved from the
// `existing` argument.
func TestParseProfileFromForm(t *testing.T) {
	form := url.Values{
		"section_order":                              []string{"open-mrs,pinned,active-agents"},
		"section_visible_open-mrs":                   []string{"1"},
		"section_visible_pinned":                     []string{"1"},
		"section_visible_open-decisions":             []string{"1"},
		"section_visible_recent-comments":            []string{"1"},
		"section_visible_recent-commits":             []string{"1"},
		"section_visible_recent-merged":              []string{"1"},
		"section_visible_forwarded-mentions":         []string{"1"},
		// active-agents intentionally omitted → hidden
	}
	existing := ProfilePref{PinnedBeadIDs: []string{"gd-keepme"}}
	got := parseProfileFromForm(form, existing)
	if !reflect.DeepEqual(got.PinnedBeadIDs, []string{"gd-keepme"}) {
		t.Errorf("PinnedBeadIDs not preserved: %+v", got.PinnedBeadIDs)
	}
	wantOrder := []string{"open-mrs", "pinned", "active-agents"}
	if !reflect.DeepEqual(got.SectionOrder, wantOrder) {
		t.Errorf("SectionOrder = %v, want %v", got.SectionOrder, wantOrder)
	}
	hiddenSet := map[string]bool{}
	for _, h := range got.HiddenSections {
		hiddenSet[h] = true
	}
	if !hiddenSet["active-agents"] {
		t.Errorf("active-agents should be hidden, got %v", got.HiddenSections)
	}
	if hiddenSet["open-mrs"] {
		t.Errorf("open-mrs should be visible, got %v", got.HiddenSections)
	}
}

// TestEncodeProfilePref emits "{}" for the zero value (so the daemon's
// JSON-typed-field validator always sees an object) and otherwise emits a
// canonical JSON blob.
func TestEncodeProfilePref(t *testing.T) {
	empty, err := encodeProfilePref(ProfilePref{})
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	if empty != "{}" {
		t.Errorf("empty profile = %q, want {}", empty)
	}
	full, err := encodeProfilePref(ProfilePref{
		HiddenSections: []string{"pinned"},
		SectionOrder:   []string{"open-mrs", "pinned"},
	})
	if err != nil {
		t.Fatalf("encode full: %v", err)
	}
	if !strings.Contains(full, `"hidden_sections":["pinned"]`) {
		t.Errorf("hidden_sections missing in %q", full)
	}
	if !strings.Contains(full, `"section_order":["open-mrs","pinned"]`) {
		t.Errorf("section_order missing in %q", full)
	}
}
