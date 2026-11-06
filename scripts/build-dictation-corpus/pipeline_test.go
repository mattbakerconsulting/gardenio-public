package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// End-to-end smoke test for the core pipeline steps that glue the
// clean → tokenize → ID-spell → emit stages together, without any
// network calls. Guards the chunk-building + phrase-assembly contract
// that the CI regen relies on.
func TestBuildChunks_PullsDescriptionCommentsAndStringFields(t *testing.T) {
	fields, _ := json.Marshal(map[string]any{
		"prompt":        "decide whether to upgrade kbeads to v3",
		"rationale":     "we need the new incremental API",
		"response_text": "approved — ship the upgrade",
		"not_a_string":  42,
	})
	beads := []*bead{
		{
			ID:          "gd-fnhtkrbdiq",
			Title:       "Gardenio controller loops",
			Description: "see gd-hkbgwaothb for root cause",
			Fields:      fields,
		},
		{
			ID:    "gd-hkbgwaothb",
			Title: "Root cause of the loop",
			// Description with a code fence to prove we clean.
			Description: "intro\n```go\nfunc main(){}\n```\noutro",
		},
	}
	comments := map[string][]*comment{
		"gd-fnhtkrbdiq": {{Text: "user said https://x.y/z broke gd-7yfi0rsi0u"}},
	}

	chunks, ids := buildChunks(beads, comments)

	// Chunks contain cleaned text only — no code fences, no URLs.
	joined := strings.Join(chunks, "\n")
	for _, forbidden := range []string{"```", "func main", "https://"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("chunks contain forbidden %q:\n%s", forbidden, joined)
		}
	}
	// Title, description, decision fields, and comments all flowed in.
	for _, want := range []string{
		"Gardenio controller loops",
		"decide whether to upgrade kbeads",
		"approved",
		"user said",
		"outro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in chunks:\n%s", want, joined)
		}
	}

	// All three bead ids referenced anywhere should surface.
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}
	for _, want := range []string{"gd-fnhtkrbdiq", "gd-hkbgwaothb", "gd-7yfi0rsi0u"} {
		if !idSet[want] {
			t.Errorf("missing id %q in %v", want, ids)
		}
	}
}

func TestBuildPhrases_IDTemplatesFirstThenSentences(t *testing.T) {
	ids := []string{"gd-fnhtkrbdiq"}
	chunks := []string{
		"The gardenio controller crashed in prod. Investigation pending.",
		"kbeads rolled forward.",
	}
	glossary := []string{"gardenio", "kbeads"}

	got := buildPhrases(chunks, ids, glossary, 100)
	if len(got) < 4 {
		t.Fatalf("want at least 4 phrases (3 spellings + >=1 sentence), got %d: %v", len(got), got)
	}
	// First three must be the spelling templates for the ID.
	spell := spellID(ids[0])
	if got[0] != "bead "+spell {
		t.Errorf("phrase[0] = %q, want %q", got[0], "bead "+spell)
	}
	if got[1] != spell {
		t.Errorf("phrase[1] = %q, want %q", got[1], spell)
	}
	if got[2] != "see "+spell {
		t.Errorf("phrase[2] = %q, want %q", got[2], "see "+spell)
	}
	// At least one of the sentences landed.
	joined := strings.Join(got[3:], " | ")
	if !strings.Contains(joined, "gardenio controller crashed") && !strings.Contains(joined, "kbeads rolled forward") {
		t.Errorf("expected sentence phrase with glossary term, got: %v", got[3:])
	}
}

func TestBuildPhrases_RespectsCap(t *testing.T) {
	ids := []string{"gd-fnhtkrbdiq", "gd-hkbgwaothb", "gd-7yfi0rsi0u"}
	chunks := []string{"gardenio one. gardenio two. gardenio three."}
	glossary := []string{"gardenio"}
	got := buildPhrases(chunks, ids, glossary, 5)
	if len(got) != 5 {
		t.Fatalf("want exactly 5, got %d: %v", len(got), got)
	}
}

func TestBuildPhrases_DedupesExactMatches(t *testing.T) {
	ids := []string{"gd-fnhtkrbdiq"}
	// Same sentence twice -> only kept once.
	chunks := []string{
		"gardenio controller crashed in prod.",
		"gardenio controller crashed in prod.",
	}
	glossary := []string{"gardenio"}
	got := buildPhrases(chunks, ids, glossary, 100)
	seen := map[string]int{}
	for _, p := range got {
		seen[p]++
	}
	for p, n := range seen {
		if n > 1 {
			t.Errorf("phrase %q appears %d times", p, n)
		}
	}
}
