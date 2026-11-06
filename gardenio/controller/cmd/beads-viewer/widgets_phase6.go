package main

// Phase 6 — diff widget (#12 Fix Available in Pending Branch).
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 6.
// Bead: ka-0000005934.
//
// One widget lands here:
//   #12 fix-available-pending-branch — branch metadata, CI status pill, file/
//       hunk diff with chroma-highlighted per-line content, fast-track-merge
//       action gated by D3 policy (CI green + bypass-record audit).
//
// Inline view only for v1; split-view (diff2html) is feature-deferred per
// synthesis-doc resolution.

import (
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
)

func init() {
	registerWidget("fix-available-pending-branch", renderFixAvailablePendingBranch)
}

// fixAvailableBranch carries the rendered widget shape. CI status normalises
// to "passed" | "failed" | "running" | "pending"; unknown states surface as
// "pending" so the pill always picks a known palette entry.
type fixAvailableBranch struct {
	Title        string
	Branch       string
	BaseBranch   string
	CommitSHA    string
	CommitShort  string // first 7 chars
	Author       string
	MRURL        string
	MRID         string
	BeadID       string
	CIStatus     string // bands above
	CIDetailURL  string
	Summary      template.HTML
	FilesCount   int
	Adds         int
	Dels         int
	Files        []renderedDiffFile
	MergeButton  template.HTML
	OpenMR       template.HTML
	SwapTargetID string // matches swapTargetIDFor() — placeholder anchor
}

// renderedDiffFile is the per-file shape the template iterates. Code is
// highlighted hunk-by-hunk so each side picks up language-aware colours.
type renderedDiffFile struct {
	Path     string
	OldPath  string
	NewPath  string
	Adds     int
	Dels     int
	Language string
	Hunks    []renderedHunk
}

// renderedHunk groups three things the template needs together: the header
// line for context, and parallel slices of pre-highlighted lines.
type renderedHunk struct {
	Header  string
	Context string
	Lines   []renderedDiffLine
}

// renderedDiffLine is a single line in the rendered diff. Kind drives CSS and
// gutter rendering; HTML is the chroma-highlighted span sequence.
type renderedDiffLine struct {
	Kind    string // "ctx" | "add" | "del"
	OldNum  int
	NewNum  int
	HasOld  bool
	HasNew  bool
	HTML    template.HTML
	RawText string
}

func renderFixAvailablePendingBranch(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := fixAvailableBranch{
		Title:       widgetString(p, "title"),
		Branch:      widgetString(p, "branch"),
		BaseBranch:  widgetString(p, "base_branch"),
		CommitSHA:   widgetString(p, "commit_sha"),
		Author:      widgetString(p, "author"),
		MRURL:       widgetString(p, "mr_url"),
		MRID:        widgetString(p, "mr_id"),
		BeadID:      widgetString(p, "bead_id"),
		CIStatus:    normaliseCIStatus(widgetString(p, "ci_status")),
		CIDetailURL: widgetString(p, "ci_detail_url"),
	}
	if v.BaseBranch == "" {
		v.BaseBranch = "main"
	}
	v.CommitShort = v.CommitSHA
	if len(v.CommitShort) > 7 {
		v.CommitShort = v.CommitShort[:7]
	}
	if md := widgetString(p, "summary_markdown"); md != "" {
		var mdRenderer goldmark.Markdown
		if ctx != nil {
			mdRenderer = ctx.md
		}
		v.Summary = renderMarkdown(mdRenderer, md)
	}

	parsed := parseUnifiedDiff(widgetString(p, "diff"))
	for _, f := range parsed {
		path := f.NewPath
		if path == "" {
			path = f.OldPath
		}
		rf := renderedDiffFile{
			Path:     path,
			OldPath:  f.OldPath,
			NewPath:  f.NewPath,
			Adds:     f.Adds,
			Dels:     f.Dels,
			Language: f.Language,
		}
		for _, h := range f.Hunks {
			rh := renderedHunk{Header: h.Header, Context: h.Context}
			for _, ln := range h.Lines {
				rh.Lines = append(rh.Lines, renderedDiffLine{
					Kind:    ln.Kind,
					OldNum:  ln.OldNum,
					NewNum:  ln.NewNum,
					HasOld:  ln.OldNum > 0,
					HasNew:  ln.NewNum > 0,
					HTML:    highlightDiffLine(ctx, f.Language, ln.Text),
					RawText: ln.Text,
				})
			}
			rf.Hunks = append(rf.Hunks, rh)
		}
		v.Files = append(v.Files, rf)
		v.Adds += f.Adds
		v.Dels += f.Dels
	}
	v.FilesCount = len(v.Files)

	v.SwapTargetID = swapTargetIDFor(v.MRID, v.BeadID)
	v.MergeButton = template.HTML(actionButtonJSON("Fast-track merge", FastTrackMergeAction(v.MRID, v.MRURL, v.BeadID, v.CIStatus, "#"+v.SwapTargetID)))
	if v.MRURL != "" {
		label := "Open MR"
		if v.MRID != "" {
			label = "Open " + v.MRID
		}
		v.OpenMR = template.HTML(actionButtonJSON(label, OpenMRAction(v.MRID, v.MRURL)))
	}

	return renderWidgetTemplate(ctx, "widget_fix_available_pending_branch", v)
}

// normaliseCIStatus maps free-form status strings to the four states the
// pill palette knows. Anything we don't recognise becomes "pending" rather
// than empty so the badge always renders.
func normaliseCIStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "passed", "success", "succeeded", "ok":
		return "passed"
	case "failed", "failure", "fail":
		return "failed"
	case "running", "pending-run":
		return "running"
	case "pending", "queued", "manual":
		return "pending"
	case "":
		return ""
	}
	return "pending"
}

// highlightDiffLine runs a single diff line through the chroma highlighter.
// Empty body returns "" (template falls through to a blank cell). Falling
// through plainTextPre would wrap a <pre> per line — too noisy — so the
// highlighter is bypassed here and we hand-escape.
func highlightDiffLine(ctx *widgetRenderCtx, language, text string) template.HTML {
	if text == "" {
		return ""
	}
	if ctx == nil || ctx.hl == nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	// chroma wraps output in <pre>; for inline-per-line we strip it back to
	// the inner spans so the table layout drives the line wrapping.
	wrapped := string(ctx.hl.highlight(language, text))
	return template.HTML(stripChromaWrap(wrapped))
}

// stripChromaWrap turns chroma's <pre><code>…</code></pre> wrapper into the
// inner spans only. Chroma's html formatter emits a stable shape, so the
// substring approach is safe; if a future version changes the wrapper, the
// fallback returns the original (still escaped) string.
func stripChromaWrap(s string) string {
	// Chroma formatter output looks like:
	//   <pre tabindex="0" style="...">…<code>…</code></pre>
	if i := strings.Index(s, "<code>"); i >= 0 {
		s = s[i+len("<code>"):]
	} else if i := strings.Index(s, "<pre"); i >= 0 {
		// no code tag — strip the outer pre
		if j := strings.Index(s[i:], ">"); j >= 0 {
			s = s[i+j+1:]
		}
	}
	if i := strings.LastIndex(s, "</code>"); i >= 0 {
		s = s[:i]
	} else if i := strings.LastIndex(s, "</pre>"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "\n")
}

// swapTargetIDFor builds a stable element id for the fast-track-merge swap
// target. Used both in the descriptor and in the template so the button knows
// where to drop the server response.
func swapTargetIDFor(mrID, beadID string) string {
	parts := []string{"ii-merge-result"}
	if mrID != "" {
		parts = append(parts, domSlug(mrID))
	}
	if beadID != "" {
		parts = append(parts, domSlug(beadID))
	}
	return strings.Join(parts, "-")
}

// domSlug produces a DOM-id-safe slug — letters, digits, dashes only.
func domSlug(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == '-' || c == '_':
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "x"
	}
	return out
}

