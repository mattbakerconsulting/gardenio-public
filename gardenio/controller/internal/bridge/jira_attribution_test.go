package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorAttributionLine(t *testing.T) {
	tests := []struct {
		name       string
		operatedBy string
		display    string
		want       string
	}{
		{
			name:       "human with resolved display",
			operatedBy: "slack:U0OPERATOR1",
			display:    "Matt Baker",
			want:       "Acted by Matt Baker (slack:U0OPERATOR1)",
		},
		{
			name:       "human oidc with resolved display",
			operatedBy: "oidc:matt@acme.example",
			display:    "Matt Baker",
			want:       "Acted by Matt Baker (oidc:matt@acme.example)",
		},
		{
			name:       "human no display mapping falls back to raw",
			operatedBy: "slack:U0OPERATOR1",
			display:    "",
			want:       "Acted by slack:U0OPERATOR1",
		},
		{
			name:       "display equal to raw collapses to single mention",
			operatedBy: "oidc:matt@acme.example",
			display:    "oidc:matt@acme.example",
			want:       "Acted by oidc:matt@acme.example",
		},
		{
			name:       "empty operated_by omits the line",
			operatedBy: "",
			display:    "Matt Baker",
			want:       "",
		},
		{
			name:       "whitespace-only operated_by omits the line",
			operatedBy: "   ",
			display:    "",
			want:       "",
		},
		{
			name:       "fleet sentinel omits the line",
			operatedBy: "fleet:slack-bridge",
			display:    "",
			want:       "",
		},
		{
			name:       "system sentinel omits the line",
			operatedBy: "system:legacy-pre-attribution",
			display:    "",
			want:       "",
		},
		{
			name:       "bare agent name omits the line",
			operatedBy: "gardenio-some-agent-1ab",
			display:    "",
			want:       "",
		},
		{
			name:       "jira provider id omits the line",
			operatedBy: "jira:5b10ac8d82e05b22cc7d4ef5", // gitleaks:allow: Atlassian's documented example account id
			display:    "",
			want:       "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := operatorAttributionLine(tt.operatedBy, tt.display)
			if got != tt.want {
				t.Fatalf("operatorAttributionLine(%q, %q) = %q, want %q",
					tt.operatedBy, tt.display, got, tt.want)
			}
			// Invariant: a non-empty line MUST contain the raw operated_by
			// verbatim — the E2E harness verdict_check4 greps for it literally.
			if got != "" && !strings.Contains(got, strings.TrimSpace(tt.operatedBy)) {
				t.Errorf("attribution line %q is missing the raw operated_by %q (harness grep would not match)",
					got, tt.operatedBy)
			}
		})
	}
}

func TestWithAttribution(t *testing.T) {
	if got := withAttribution("body text", ""); got != "body text" {
		t.Errorf("empty attribution should leave body unchanged, got %q", got)
	}
	got := withAttribution("body text", "Acted by slack:U0OPERATOR1")
	want := "body text\n\nActed by slack:U0OPERATOR1"
	if got != want {
		t.Errorf("withAttribution = %q, want %q", got, want)
	}
}

// adfText concatenates every text node in an adfParagraph document, in order,
// so a test can assert the rendered content survives the newline→hardBreak
// promotion regardless of node boundaries.
func adfText(doc map[string]any) string {
	raw, _ := json.Marshal(doc)
	var parsed struct {
		Content []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &parsed)
	var sb strings.Builder
	for _, para := range parsed.Content {
		for _, node := range para.Content {
			sb.WriteString(node.Text)
		}
	}
	return sb.String()
}

func adfCountType(doc map[string]any, typ string) int {
	raw, _ := json.Marshal(doc)
	var parsed struct {
		Content []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &parsed)
	n := 0
	for _, para := range parsed.Content {
		for _, node := range para.Content {
			if node.Type == typ {
				n++
			}
		}
	}
	return n
}

func TestADFParagraph_SingleLine(t *testing.T) {
	doc := adfParagraph("just one line")
	if got := adfText(doc); got != "just one line" {
		t.Errorf("adfText = %q, want %q", got, "just one line")
	}
	if n := adfCountType(doc, "hardBreak"); n != 0 {
		t.Errorf("single-line body should have 0 hardBreaks, got %d", n)
	}
	if n := adfCountType(doc, "text"); n != 1 {
		t.Errorf("single-line body should have 1 text node, got %d", n)
	}
}

func TestADFParagraph_AttributionOnOwnLine(t *testing.T) {
	body := withAttribution("Gardenio agent is working on this issue.", "Acted by slack:U0OPERATOR1")
	doc := adfParagraph(body)

	// Both runs must survive as text, and the raw operated_by must be present
	// verbatim for the harness grep.
	full := adfText(doc)
	if !strings.Contains(full, "Gardenio agent is working on this issue.") {
		t.Errorf("ADF text missing the body run: %q", full)
	}
	if !strings.Contains(full, "Acted by slack:U0OPERATOR1") {
		t.Errorf("ADF text missing the attribution run: %q", full)
	}
	// The "\n\n" join must render as hardBreaks (a literal newline inside one
	// text node would render as a space in Jira), not be swallowed.
	if n := adfCountType(doc, "hardBreak"); n != 2 {
		t.Errorf("expected 2 hardBreaks for a double-newline join, got %d", n)
	}
	// The blank middle line must NOT produce an empty text node (invalid ADF).
	for _, txt := range adfTextNodes(doc) {
		if txt == "" {
			t.Errorf("found an empty text node — invalid ADF")
		}
	}
}

func adfTextNodes(doc map[string]any) []string {
	raw, _ := json.Marshal(doc)
	var parsed struct {
		Content []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &parsed)
	var out []string
	for _, para := range parsed.Content {
		for _, node := range para.Content {
			if node.Type == "text" {
				out = append(out, node.Text)
			}
		}
	}
	return out
}
