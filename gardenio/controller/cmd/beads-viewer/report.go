package main

// Per-type view for type=report beads (ka-0000000186).
//
// Report beads carry long-form content in a `content` custom field, with a
// sibling `format` field (e.g. "markdown") indicating the intended renderer.
// The generic field-list path emits content as a raw <pre>, which leaves
// markdown headers, tables, and code spans unrendered for the reader. This
// view promotes `content` into its own rendered block above the remaining
// fields, mirroring the description-render pipeline (goldmark + LinkifyHTML).
//
// Formats other than `markdown` (or absent) fall through to an escaped <pre>
// so the body still surfaces but no rich-text rendering is implied.

import (
	"bytes"
	"html/template"
	"sort"

	"github.com/yuin/goldmark"

	"gardenio/controller/internal/beadsapi"
)

// reportView is the per-type view model for `report` beads.
type reportView struct {
	// Format is the lowercased `format` field value (e.g. "markdown"). Empty
	// when the bead carries no format hint.
	Format string
	// ReportType mirrors the `report_type` field (e.g. "design", "post-mortem").
	// Surfaced as a small header chip; empty when not set.
	ReportType string
	// Content is the raw text of the `content` field. The template falls back
	// to this in an escaped <pre> when ContentHTML is empty (non-markdown
	// formats and the empty case).
	Content string
	// ContentHTML is the rendered HTML for markdown content. Non-empty only
	// when Format == "markdown" and the renderer succeeded.
	ContentHTML template.HTML
	// OtherFields surfaces every Field other than `content` and `format` so
	// the generic field list still appears underneath the rendered body.
	// Sorted by key for stable output. `report_type` is included here too —
	// the surfaced chip is for emphasis, not suppression.
	OtherFields []reportField
}

// reportField is one key/value pair from the bead's Fields map, minus the
// ones promoted into the report header / content area.
type reportField struct {
	Key   string
	Value string
}

// buildReportView extracts content + format from the bead's fields and runs
// the content through goldmark when format=markdown. Errors degrade the
// rendered HTML to empty so the template falls back to the escaped <pre>
// path; the page still surfaces the raw body either way.
//
// The basePath is used to linkify bead IDs inside the rendered HTML, mirroring
// the description-render pathway. md may be nil in test contexts — the view
// then surfaces empty ContentHTML and the template emits the escaped <pre>.
func buildReportView(bead *beadsapi.BeadDetail, md goldmark.Markdown, basePath string) *reportView {
	if bead == nil {
		return nil
	}
	format := bead.Fields["format"]
	content := bead.Fields["content"]
	v := &reportView{
		Format:     format,
		ReportType: bead.Fields["report_type"],
		Content:    content,
	}
	if format == "markdown" && content != "" && md != nil {
		var buf bytes.Buffer
		if err := md.Convert([]byte(content), &buf); err == nil {
			v.ContentHTML = LinkifyHTML(buf.String(), basePath)
		}
	}
	for k, val := range bead.Fields {
		if k == "content" || k == "format" {
			continue
		}
		v.OtherFields = append(v.OtherFields, reportField{Key: k, Value: val})
	}
	sort.Slice(v.OtherFields, func(i, j int) bool {
		return v.OtherFields[i].Key < v.OtherFields[j].Key
	})
	return v
}
