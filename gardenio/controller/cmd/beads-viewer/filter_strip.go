package main

// filterChip is the ONE canonical view model for a chip in a page-level
// filter strip, rendered by templates/partials/filter_strip.html (audit
// finding F6, ka-0000005357). Before this type each page carried its own
// near-identical chip struct (typeFilterChip, triggerFilterChip, agentChip,
// inboxChipView) and its own inline strip markup, which is what kept
// regenerating the F1-F4 menu-bar drift. Every page-level filter strip now
// builds []filterChip and hands it to the shared partial, so the wrapper /
// active-state / aria / count decisions can no longer diverge by copy-paste.
//
// Not every field applies to every strip; the partial's CountMode / ActiveSpan
// / ChipClass knobs (passed per call site) select which are honored, so the
// rendered DOM stays byte-identical to each page's pre-consolidation markup.
type filterChip struct {
	// Label is the visible chip text.
	Label string
	// Value is the chip's filter identity (e.g. the type slice key, the trigger
	// source/action, the project name). Not rendered directly by the partial —
	// it is what the Go builder uses to compute Href/Active and, where a strip
	// carries a data-attribute, the source of DataVal. Kept on the view model
	// so handler tests can assert per-chip identity.
	Value string
	// Href is the destination for the (anchor) chip. Ignored for the active
	// chip when the call site renders active chips as a non-link <span>.
	Href string
	// Active marks the currently-selected chip (bold pill + aria-current, or a
	// non-link span when the strip uses ActiveSpan).
	Active bool
	// Count is the badge number after the label. Whether/how it renders is
	// governed by the strip's CountMode (none / gated / always).
	Count int
	// ShowCount gates the badge under CountMode "gated": the badge renders only
	// when ShowCount is true AND Count>0 (project/hub label-only fallback on a
	// summary daemon error sets this false).
	ShowCount bool
	// Disabled renders the chip as a non-interactive <span aria-disabled> with
	// a Tooltip (inbox retired-scope chips, design §4.3.1).
	Disabled bool
	// Tooltip is the title= text on a disabled chip.
	Tooltip string
	// DataVal is the value written to the strip's data-<key> attribute when the
	// strip is invoked with a DataKey ("source"/"action" on the trigger
	// strips). The attribute NAME is a strip-level knob (html/template forbids
	// a dynamic attribute name), so the chip only carries the value here.
	DataVal string
}
