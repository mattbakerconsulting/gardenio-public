package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

// ProfilePref is the persisted per-user customization layer for /u/{slug}
// (R5 §5.1, M2 §2 / ka-0000004773). It is stored as a JSON-typed field named
// `profile_prefs` on the operator's `type=user kind=config` bead. Reader is
// forward-compatible: unknown JSON keys are ignored, and a missing or
// malformed blob yields the zero value so the homepage falls back to the
// default 8-section render.
//
// Fields:
//   - PinnedBeadIDs: bead ids surfaced at the top of /u/{slug}#pinned.
//   - HiddenSections: section slugs the viewer has chosen to hide.
//   - SectionOrder: section slugs in the order the viewer wants them
//     rendered. Slugs not present in the list keep the default relative
//     order after the explicitly-ordered ones.
type ProfilePref struct {
	PinnedBeadIDs  []string `json:"pinned_bead_ids,omitempty"`
	HiddenSections []string `json:"hidden_sections,omitempty"`
	SectionOrder   []string `json:"section_order,omitempty"`
}

// allProfileSections is the canonical list of homepage section slugs in
// their default render order. The settings UI iterates this list when
// rendering the visibility + ordering controls, and the apply step uses it
// as the fallback when SectionOrder is empty or partial.
var allProfileSections = []string{
	sectionPinned,
	sectionActiveAgents,
	sectionOpenDecisions,
	sectionRecentComments,
	sectionOpenMRs,
	sectionRecentCommits,
	sectionRecentMerged,
	sectionTheirAutomations,
	sectionForwardedMentions,
}

// profileSectionTitles maps each section slug to the human-readable label
// surfaced in the settings UI. Keeping the labels here (rather than in the
// template) avoids drift if a section is renamed.
var profileSectionTitles = map[string]string{
	sectionPinned:            "Pinned",
	sectionActiveAgents:      "Active agents",
	sectionOpenDecisions:     "Open decisions",
	sectionRecentComments:    "Recent comments",
	sectionOpenMRs:           "Open MRs",
	sectionRecentCommits:     "Merged MRs",
	sectionRecentMerged:      "Closed work",
	sectionTheirAutomations:  "Their automations",
	sectionForwardedMentions: "Forwarded mentions",
}

// parseProfilePref extracts the ProfilePref blob from a user bead's Fields
// map. Empty / malformed JSON collapses to the zero value — the homepage
// then renders with the default order and all sections visible.
func parseProfilePref(fields map[string]string) ProfilePref {
	var p ProfilePref
	raw := strings.TrimSpace(fields["profile_prefs"])
	if raw == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// normalizeProfilePref filters HiddenSections and SectionOrder down to known
// section slugs, dropping duplicates and unknown values. This is the gate
// the write path runs before persisting — it keeps the stored blob clean
// so a stray pref doesn't accumulate into something the reader can't
// reason about.
func normalizeProfilePref(p ProfilePref) ProfilePref {
	known := make(map[string]struct{}, len(allProfileSections))
	for _, s := range allProfileSections {
		known[s] = struct{}{}
	}

	cleanList := func(in []string) []string {
		if len(in) == 0 {
			return nil
		}
		seen := make(map[string]struct{}, len(in))
		out := make([]string, 0, len(in))
		for _, s := range in {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, ok := known[s]; !ok {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	cleanPins := func(in []string) []string {
		if len(in) == 0 {
			return nil
		}
		seen := make(map[string]struct{}, len(in))
		out := make([]string, 0, len(in))
		for _, s := range in {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	return ProfilePref{
		PinnedBeadIDs:  cleanPins(p.PinnedBeadIDs),
		HiddenSections: cleanList(p.HiddenSections),
		SectionOrder:   cleanList(p.SectionOrder),
	}
}

// profileSectionView is the per-section row the settings UI iterates over.
// It carries both the storage slug (used for form field names + JSON
// persistence) and a human-readable title (rendered in the row label).
// Visible mirrors the inverse of HiddenSections, computed up front so the
// template doesn't need to do membership lookups.
type profileSectionView struct {
	Slug    string
	Title   string
	Visible bool
}

// buildProfileSectionView projects a stored ProfilePref into the per-row
// view-model the settings templates iterate over. The order honors
// SectionOrder (explicit ordering first, defaults appended after) so the
// editor and the homepage agree on what "current order" means.
func buildProfileSectionView(p ProfilePref) []profileSectionView {
	hidden := make(map[string]struct{}, len(p.HiddenSections))
	for _, s := range p.HiddenSections {
		hidden[s] = struct{}{}
	}
	known := make(map[string]struct{}, len(allProfileSections))
	for _, s := range allProfileSections {
		known[s] = struct{}{}
	}

	used := make(map[string]struct{}, len(allProfileSections))
	out := make([]profileSectionView, 0, len(allProfileSections))

	add := func(slug string) {
		if _, ok := used[slug]; ok {
			return
		}
		if _, ok := known[slug]; !ok {
			return
		}
		used[slug] = struct{}{}
		_, hide := hidden[slug]
		out = append(out, profileSectionView{
			Slug:    slug,
			Title:   profileSectionTitles[slug],
			Visible: !hide,
		})
	}

	for _, slug := range p.SectionOrder {
		add(slug)
	}
	for _, slug := range allProfileSections {
		add(slug)
	}
	return out
}

// parseProfileFromForm reads the settings editor's form values back into a
// ProfilePref. The editor submits:
//   - `section_order`: comma-separated list of section slugs in display
//     order (rendered from the row order at submit time).
//   - `section_visible_<slug>`: presence indicates the section is visible;
//     absence means hidden (standard unchecked-checkbox semantics).
//
// PinnedBeadIDs is not editable here yet, so we preserve whatever the
// stored blob already carries via the `existing` argument. Normalization
// happens at the call site so the editor partial can re-render an
// in-flight draft without losing user intent.
func parseProfileFromForm(form url.Values, existing ProfilePref) ProfilePref {
	rawOrder := form.Get("section_order")
	order := make([]string, 0, len(allProfileSections))
	if rawOrder != "" {
		for tok := range strings.SplitSeq(rawOrder, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			order = append(order, tok)
		}
	}
	hidden := make([]string, 0, len(allProfileSections))
	for _, slug := range allProfileSections {
		if form.Get("section_visible_"+slug) == "" {
			hidden = append(hidden, slug)
		}
	}
	return ProfilePref{
		PinnedBeadIDs:  existing.PinnedBeadIDs,
		HiddenSections: hidden,
		SectionOrder:   order,
	}
}

// encodeProfilePref marshals the prefs blob to its canonical JSON form for
// persistence on the user bead's profile_prefs field. An empty blob
// serializes to `{}` rather than `null` so consumers (and the daemon's
// JSON-typed-field validator) always see an object.
func encodeProfilePref(p ProfilePref) (string, error) {
	if len(p.PinnedBeadIDs) == 0 && len(p.HiddenSections) == 0 && len(p.SectionOrder) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// applyProfilePref reorders and filters a slice of homepage sections per
// the viewer's stored preferences. The input order is the controller's
// default ordering; the output order honors SectionOrder (sections listed
// there appear first, in the order given) followed by any remaining
// sections in their default relative order. Sections whose slugs appear in
// HiddenSections are dropped entirely.
//
// Empty / nil prefs is a no-op: applyProfilePref returns the input slice
// untouched so the default render keeps shipping when the user hasn't
// customized anything.
func applyProfilePref(sections []userHomeSection, prefs ProfilePref) []userHomeSection {
	if len(sections) == 0 {
		return sections
	}
	if len(prefs.HiddenSections) == 0 && len(prefs.SectionOrder) == 0 {
		return sections
	}

	hidden := make(map[string]struct{}, len(prefs.HiddenSections))
	for _, s := range prefs.HiddenSections {
		hidden[s] = struct{}{}
	}

	bySlug := make(map[string]userHomeSection, len(sections))
	defaultOrder := make([]string, 0, len(sections))
	for _, sec := range sections {
		bySlug[sec.Slug] = sec
		defaultOrder = append(defaultOrder, sec.Slug)
	}

	used := make(map[string]struct{}, len(sections))
	out := make([]userHomeSection, 0, len(sections))

	for _, slug := range prefs.SectionOrder {
		if _, ok := used[slug]; ok {
			continue
		}
		sec, ok := bySlug[slug]
		if !ok {
			continue
		}
		used[slug] = struct{}{}
		if _, hide := hidden[slug]; hide {
			continue
		}
		out = append(out, sec)
	}
	for _, slug := range defaultOrder {
		if _, ok := used[slug]; ok {
			continue
		}
		used[slug] = struct{}{}
		if _, hide := hidden[slug]; hide {
			continue
		}
		out = append(out, bySlug[slug])
	}
	return out
}
