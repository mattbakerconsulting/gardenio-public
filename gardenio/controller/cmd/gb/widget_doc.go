// Shared parsing for the free-htmx widget authoring artifact.
//
// Agents author free-htmx widgets as a single Markdown file with four
// well-known sections — `## Summary`, `## Schema` (json fenced),
// `## Sample payload` (json fenced) and `## Template` (html fenced) — and
// the `gb widget` CLI consumes that document via scaffold-htmx, lint,
// preview and validate. The parser is intentionally permissive: missing
// sections surface as zero-value fields and the caller decides what to
// gate on.
//
// See gardenio/docs/research/free-htmx-widget-authoring-runbook.md §"End-to-end
// CLI tape" for the canonical body.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// widgetDoc is the parsed 4-section authoring document.
type widgetDoc struct {
	Slug     string
	Version  string
	Summary  string
	Schema   string // raw JSON
	Sample   string // raw JSON
	Template string // raw HTML/template source

	// Path is the source file path if loaded from disk; empty for stdin.
	Path string
}

// parseWidgetDoc reads a widget authoring artifact from raw bytes.
//
// The format is forgiving: any text outside a `## <heading>` section is
// dropped, and code-fenced blocks (```json / ```html / ```) are unwrapped
// when present. Headings match case-insensitively against a small known
// set so authors can write "Sample Payload" or "sample payload".
func parseWidgetDoc(body []byte) (*widgetDoc, error) {
	doc := &widgetDoc{}

	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var current string
	var buf strings.Builder
	flush := func() {
		val := strings.TrimSpace(buf.String())
		val = unwrapFence(val)
		switch current {
		case "summary":
			doc.Summary = val
		case "schema":
			doc.Schema = val
		case "sample":
			doc.Sample = val
		case "template":
			doc.Template = val
		}
		buf.Reset()
	}

	inFence := false
	for scanner.Scan() {
		line := scanner.Text()
		// Track fenced blocks so a `## Foo` inside a fence is treated as text.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			buf.WriteString(line)
			buf.WriteByte('\n')
			continue
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			flush()
			heading := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			current = headingToSection(heading)
			continue
		}
		if current == "" {
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading widget doc: %w", err)
	}
	return doc, nil
}

// loadWidgetDoc reads and parses a widget doc from a file path. Use "-"
// for stdin.
func loadWidgetDoc(path string) (*widgetDoc, error) {
	var body []byte
	var err error
	if path == "-" {
		body, err = readAllStdin()
	} else {
		body, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading widget doc %q: %w", path, err)
	}
	doc, err := parseWidgetDoc(body)
	if err != nil {
		return nil, err
	}
	if path != "-" {
		doc.Path = path
	}
	return doc, nil
}

func headingToSection(heading string) string {
	heading = strings.TrimSuffix(heading, ":")
	switch heading {
	case "summary":
		return "summary"
	case "schema":
		return "schema"
	case "sample", "sample payload", "sample-payload", "example", "example payload":
		return "sample"
	case "template", "body", "html":
		return "template"
	}
	return ""
}

// unwrapFence strips a single leading/trailing ```language fence pair if
// present. Multi-fence blocks (rare in our format) are returned as-is.
func unwrapFence(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s
	}
	first := strings.TrimSpace(lines[0])
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(first, "```") || last != "```" {
		return s
	}
	// Count fences; if >2 we don't unwrap.
	fences := 0
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fences++
		}
	}
	if fences != 2 {
		return s
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

// readAllStdin reads all of stdin and returns the bytes. Pulled out so
// tests can stub it.
var readAllStdin = func() ([]byte, error) {
	return io.ReadAll(os.Stdin)
}
