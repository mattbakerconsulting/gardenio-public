package main

// Lightweight unified-diff parser for Phase 6 (#12 Fix Available in Pending
// Branch). Plan-of-record: synthesis.md §Phase 6. Bead: ka-0000005934.
//
// We deliberately avoid pulling in go-diff for v1 — the widget needs hunk
// boundaries, file headers, and per-line classification (context/add/del),
// which is ~80 lines of straightforward parsing. The output feeds Chroma per
// hunk so each side gets language-aware highlighting.
//
// v1 is inline view only. Split-view (diff2html) is deferred per synthesis-doc
// resolution; pulling that dependency in is the explicit signal that
// stakeholder demand has materialised.

import (
	"strings"
)

// diffFile is one file in a unified-diff payload — the smallest unit a
// renderer needs.
type diffFile struct {
	OldPath  string
	NewPath  string
	Language string // hint pulled from new path extension
	Hunks    []diffHunk
	// Counts surface in the widget header strip without forcing the renderer
	// to walk the lines twice.
	Adds int
	Dels int
}

// diffHunk is a contiguous patch region.
type diffHunk struct {
	Header   string // raw "@@ -a,b +c,d @@ context"
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Context  string // text after the second @@
	Lines    []diffLine
}

// diffLine is one line inside a hunk. Kind is one of "ctx", "add", "del".
// OldNum / NewNum are 1-based line numbers in the original / new file. A line
// number of 0 means "not applicable" (e.g. a "+" line has no OldNum).
type diffLine struct {
	Kind   string
	Text   string // without the leading +/-/space
	OldNum int
	NewNum int
}

// parseUnifiedDiff parses a unified-diff string into a slice of files. Lines
// outside any "diff --git" / "--- " block are ignored, so it's tolerant of
// `git format-patch` headers and trailing junk.
func parseUnifiedDiff(src string) []diffFile {
	var files []diffFile
	var current *diffFile
	var hunk *diffHunk

	flush := func() {
		if hunk != nil && current != nil {
			current.Hunks = append(current.Hunks, *hunk)
			hunk = nil
		}
		if current != nil {
			files = append(files, *current)
			current = nil
		}
	}

	for _, raw := range strings.Split(src, "\n") {
		line := raw
		switch {
		case strings.HasPrefix(line, "diff --git"):
			flush()
			current = &diffFile{}
		case strings.HasPrefix(line, "--- "):
			if current == nil {
				current = &diffFile{}
			}
			current.OldPath = stripDiffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			if current == nil {
				current = &diffFile{}
			}
			current.NewPath = stripDiffPath(strings.TrimPrefix(line, "+++ "))
			current.Language = languageFromPath(current.NewPath)
		case strings.HasPrefix(line, "@@"):
			if current == nil {
				continue
			}
			if hunk != nil {
				current.Hunks = append(current.Hunks, *hunk)
			}
			hunk = parseHunkHeader(line)
		default:
			if hunk == nil || current == nil {
				continue
			}
			if line == "" {
				// Trailing empty line in the diff — preserve as a context line
				// so the rendered diff doesn't drop blank lines from the source.
				hunk.Lines = append(hunk.Lines, diffLine{Kind: "ctx"})
				continue
			}
			ch := line[0]
			body := line[1:]
			switch ch {
			case '+':
				if strings.HasPrefix(line, "+++ ") {
					continue
				}
				hunk.Lines = append(hunk.Lines, diffLine{Kind: "add", Text: body})
				current.Adds++
			case '-':
				if strings.HasPrefix(line, "--- ") {
					continue
				}
				hunk.Lines = append(hunk.Lines, diffLine{Kind: "del", Text: body})
				current.Dels++
			case ' ':
				hunk.Lines = append(hunk.Lines, diffLine{Kind: "ctx", Text: body})
			case '\\':
				// "\ No newline at end of file" — skip; the widget doesn't surface this.
				continue
			default:
				// Unknown line; skip rather than panic.
				continue
			}
		}
	}
	flush()

	for i := range files {
		assignLineNumbers(&files[i])
	}
	return files
}

// stripDiffPath removes the leading "a/" or "b/" prefix git adds to paths in
// unified diffs. Tab-or-space-separated trailing metadata (timestamps) is
// dropped.
func stripDiffPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexAny(p, " \t"); i >= 0 {
		p = p[:i]
	}
	switch {
	case strings.HasPrefix(p, "a/"):
		return strings.TrimPrefix(p, "a/")
	case strings.HasPrefix(p, "b/"):
		return strings.TrimPrefix(p, "b/")
	case p == "/dev/null":
		return ""
	}
	return p
}

// parseHunkHeader extracts the four numbers from "@@ -a,b +c,d @@". Lines
// that don't match the expected shape return a zero-counts hunk; the renderer
// degrades to displaying lines without numbers, which is fine for malformed
// input.
func parseHunkHeader(line string) *diffHunk {
	h := &diffHunk{Header: line}
	// "@@ -A,B +C,D @@ context"
	end := strings.Index(line[2:], "@@")
	if end < 0 {
		return h
	}
	core := strings.TrimSpace(line[2 : 2+end])
	h.Context = strings.TrimSpace(line[2+end+2:])
	parts := strings.Fields(core)
	if len(parts) < 2 {
		return h
	}
	h.OldStart, h.OldCount = parseHunkRange(parts[0])
	h.NewStart, h.NewCount = parseHunkRange(parts[1])
	return h
}

// parseHunkRange parses "-A,B" or "+C,D" (count defaults to 1 when omitted).
func parseHunkRange(s string) (start, count int) {
	if len(s) == 0 {
		return 0, 0
	}
	s = s[1:] // drop sign
	count = 1
	if i := strings.Index(s, ","); i >= 0 {
		start = atoiSafe(s[:i])
		count = atoiSafe(s[i+1:])
	} else {
		start = atoiSafe(s)
	}
	return
}

// atoiSafe parses a positive integer, returning 0 on any failure. Mirror of
// the strict-or-zero idiom widgetInt uses.
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// assignLineNumbers walks each hunk and sets OldNum / NewNum on every line
// post-parse so the renderer can drop them straight into the gutter.
func assignLineNumbers(f *diffFile) {
	for hi := range f.Hunks {
		h := &f.Hunks[hi]
		oldN := h.OldStart
		newN := h.NewStart
		for li := range h.Lines {
			l := &h.Lines[li]
			switch l.Kind {
			case "ctx":
				l.OldNum = oldN
				l.NewNum = newN
				oldN++
				newN++
			case "add":
				l.NewNum = newN
				newN++
			case "del":
				l.OldNum = oldN
				oldN++
			}
		}
	}
}

// languageFromPath maps a file extension to a chroma lexer name. Empty result
// means "let chroma analyse" — used by renderHighlight via the codeHighlighter
// fallback.
func languageFromPath(p string) string {
	p = strings.ToLower(p)
	if i := strings.LastIndex(p, "."); i >= 0 {
		ext := p[i+1:]
		switch ext {
		case "go":
			return "go"
		case "ts", "tsx":
			return "typescript"
		case "js", "jsx":
			return "javascript"
		case "rs":
			return "rust"
		case "py":
			return "python"
		case "sh", "bash":
			return "bash"
		case "yml", "yaml":
			return "yaml"
		case "json":
			return "json"
		case "html":
			return "html"
		case "css":
			return "css"
		case "md":
			return "markdown"
		case "sql":
			return "sql"
		case "java":
			return "java"
		}
	}
	return ""
}
