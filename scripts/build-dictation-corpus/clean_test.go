package main

import (
	"strings"
	"testing"
)

func TestCleanText_StripsCodeFences(t *testing.T) {
	in := "before\n```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\nafter"
	got := cleanText(in)
	if strings.Contains(got, "func main") || strings.Contains(got, "```") {
		t.Fatalf("code fence leaked through: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("surrounding prose lost: %q", got)
	}
}

func TestCleanText_StripsMultipleFences(t *testing.T) {
	in := "```\nA\n```\nmid\n```sh\nB\n```\ntail"
	got := cleanText(in)
	if strings.Contains(got, "```") || strings.Contains(got, "A") || strings.Contains(got, "B") {
		t.Fatalf("fences not fully stripped: %q", got)
	}
	if !strings.Contains(got, "mid") || !strings.Contains(got, "tail") {
		t.Fatalf("prose between fences lost: %q", got)
	}
}

func TestCleanText_StripsURLs(t *testing.T) {
	in := "see https://example.com/foo?x=1 and http://a.b for details"
	got := cleanText(in)
	if strings.Contains(got, "http") || strings.Contains(got, "example.com") {
		t.Fatalf("url leaked: %q", got)
	}
}

func TestCleanText_StripsInlineCode(t *testing.T) {
	in := "use `go test ./...` then ship"
	got := cleanText(in)
	if strings.Contains(got, "`") || strings.Contains(got, "go test") {
		t.Fatalf("inline code leaked: %q", got)
	}
}

func TestCleanText_KeepsBeadIDsAndGlossaryTerms(t *testing.T) {
	in := "Gardenio bug in ka-0000000316 ships to prod"
	got := cleanText(in)
	if !strings.Contains(got, "ka-0000000316") {
		t.Fatalf("bead id dropped: %q", got)
	}
	if !strings.Contains(got, "Gardenio") {
		t.Fatalf("glossary term case lost: %q", got)
	}
}

func TestCleanText_MarkdownLink(t *testing.T) {
	in := "See [the epic](https://gitlab.example.com/x/y) for more"
	got := cleanText(in)
	if !strings.Contains(got, "the epic") {
		t.Fatalf("link text lost: %q", got)
	}
	if strings.Contains(got, "gitlab.example.com") {
		t.Fatalf("link target leaked: %q", got)
	}
}

func TestCleanText_EmptyAndWhitespace(t *testing.T) {
	cases := []string{"", "   ", "\n\n\n", "\t \t\n \n"}
	for _, c := range cases {
		if got := cleanText(c); got != "" {
			t.Errorf("cleanText(%q) = %q, want empty", c, got)
		}
	}
}

func TestCleanText_CollapsesWhitespace(t *testing.T) {
	in := "a    b\t\tc\n\n\n\nd"
	got := cleanText(in)
	// runs of spaces/tabs -> single space within a line
	if strings.Contains(got, "  ") || strings.Contains(got, "\t") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	// 4 newlines -> 2
	if strings.Count(got, "\n\n\n") != 0 {
		t.Fatalf("blank lines not collapsed: %q", got)
	}
}

func TestCleanText_HTMLTags(t *testing.T) {
	in := "Hello <b>world</b> and <a href=\"x\">link</a>!"
	got := cleanText(in)
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("tags not stripped: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Fatalf("inner text lost: %q", got)
	}
}
