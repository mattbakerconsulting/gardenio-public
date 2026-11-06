// Package diff parses unified-diff output (`git diff --unified=N`) into
// a structure tailored for patch-coverage computation: per-file lists of
// added or modified line numbers in the *new* (post-change) file.
//
// The patch-cov library only cares about which lines were added or
// modified — deletions don't contribute executable lines. Comments and
// whitespace are not filtered here; that is done at coverage-projection
// time by the LCOV intersection (a non-executable line has no DA entry).
package diff

import (
	"bufio"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Diff is the parse result for a unified diff: a list of FileDiff,
// indexed by post-change path. Files whose path appears as `/dev/null`
// (pure deletion) are dropped entirely.
type Diff struct {
	Files map[string]*FileDiff
}

// FileDiff records added/modified lines in a single post-change file.
type FileDiff struct {
	Path string
	// AddedLines is the sorted, deduplicated set of line numbers added or
	// modified in the post-change file. Whitespace and comment filtering
	// is the caller's responsibility (the LCOV DA: intersection handles
	// it for free).
	AddedLines []int
}

// Parse reads a unified diff from r and returns the parsed Diff.
func Parse(r io.Reader) (*Diff, error) {
	out := &Diff{Files: map[string]*FileDiff{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var current *FileDiff
	var newLineNo int
	var inHunk bool

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git"):
			current = nil
			inHunk = false
		case strings.HasPrefix(line, "+++ "):
			path := parseHeaderPath(line[4:])
			if path == "" {
				current = nil
			} else {
				current = out.get(path)
			}
			inHunk = false
		case strings.HasPrefix(line, "--- "):
			// Tracked via +++ only.
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			ln, ok := parseHunkNewLine(line)
			if !ok {
				inHunk = false
				continue
			}
			newLineNo = ln
		case inHunk && current != nil:
			if len(line) == 0 {
				// Empty body line — counts as an unchanged line in
				// unified=N>=1 contexts; with --unified=0 you should not
				// see these between hunks.
				newLineNo++
				continue
			}
			switch line[0] {
			case '+':
				current.AddedLines = append(current.AddedLines, newLineNo)
				newLineNo++
			case '-':
				// Removed line — does not advance the new-file counter.
			case ' ':
				newLineNo++
			case '\\':
				// "\ No newline at end of file" — ignore.
			default:
				// Unknown — be lenient and treat like context.
				newLineNo++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, fd := range out.Files {
		fd.AddedLines = dedupSorted(fd.AddedLines)
	}
	return out, nil
}

// ParseFile reads and parses a unified-diff file at path.
func ParseFile(path string) (*Diff, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

func (d *Diff) get(path string) *FileDiff {
	fd, ok := d.Files[path]
	if !ok {
		fd = &FileDiff{Path: path}
		d.Files[path] = fd
	}
	return fd
}

// parseHeaderPath extracts the file path from a "+++ b/path" or
// "+++ /dev/null" header line. Returns "" for /dev/null.
func parseHeaderPath(s string) string {
	// Trim trailing tab + timestamp if present.
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	}
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	// Strip "a/" or "b/" git prefix if present.
	if len(s) >= 2 && (s[:2] == "a/" || s[:2] == "b/") {
		s = s[2:]
	}
	return s
}

// parseHunkNewLine extracts the new-file starting line number from a hunk
// header, e.g. "@@ -10,0 +11,3 @@ ctx" → 11.
func parseHunkNewLine(line string) (int, bool) {
	_, rest, ok := strings.Cut(line, "+")
	if !ok {
		return 0, false
	}
	end := strings.IndexAny(rest, " ,@")
	if end < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

func dedupSorted(xs []int) []int {
	if len(xs) <= 1 {
		return xs
	}
	sort.Ints(xs)
	out := xs[:1]
	for i := 1; i < len(xs); i++ {
		if xs[i] != out[len(out)-1] {
			out = append(out, xs[i])
		}
	}
	return out
}
