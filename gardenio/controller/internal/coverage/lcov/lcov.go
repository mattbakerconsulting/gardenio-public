// Package lcov is a minimal LCOV-tracefile parser, scoped to the records
// the patch-cov library needs: per-file source path (SF) and per-line hit
// counts (DA). Other LCOV records (FN/FNDA, BRDA, LH/LF) are tolerated
// and skipped.
//
// The format is documented at:
// https://manpages.debian.org/unstable/lcov/geninfo.1.en.html#FILES
package lcov

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// File is a parsed LCOV tracefile: a list of per-source records keyed by
// path. Multiple records for the same SF in the input are merged
// (DA hit counts summed) so callers see one Record per file.
type File struct {
	Records map[string]*Record
}

// Record is the per-source-file slice of an LCOV tracefile.
type Record struct {
	// Path is the SF: value, repo-root-relative after the W3 normalizer
	// has run.
	Path string
	// Lines maps executable line number to total hit count. A line is
	// "executable" iff it appears as a DA: entry. Hit count > 0 means
	// the line was covered by at least one test run.
	Lines map[int]int
}

// New returns an empty File ready to be populated.
func New() *File {
	return &File{Records: map[string]*Record{}}
}

// Get returns the Record for path, creating it on first access.
func (f *File) Get(path string) *Record {
	r, ok := f.Records[path]
	if !ok {
		r = &Record{Path: path, Lines: map[int]int{}}
		f.Records[path] = r
	}
	return r
}

// Parse reads an LCOV tracefile from r and returns the parsed File.
// Unknown records are silently ignored; only SF, DA, and end_of_record are
// load-bearing.
func Parse(r io.Reader) (*File, error) {
	out := New()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var current *Record
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "SF:"):
			current = out.Get(line[3:])
		case strings.HasPrefix(line, "DA:"):
			if current == nil {
				continue
			}
			lineStr, rest, ok := strings.Cut(line[3:], ",")
			if !ok {
				continue
			}
			lineNo, err := strconv.Atoi(lineStr)
			if err != nil {
				continue
			}
			// Trim optional ",<checksum>" tail.
			if hitsStr, _, ok := strings.Cut(rest, ","); ok {
				rest = hitsStr
			}
			hits, err := strconv.Atoi(rest)
			if err != nil {
				continue
			}
			current.Lines[lineNo] += hits
		case line == "end_of_record":
			current = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseFile reads and parses an LCOV tracefile at path.
func ParseFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}
