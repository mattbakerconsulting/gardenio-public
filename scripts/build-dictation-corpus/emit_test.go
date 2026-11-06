package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteGlossary_EmitsJSONArray(t *testing.T) {
	dir := t.TempDir()
	terms := []string{"Gardenio", "kbeads", "coopmux"}
	if err := writeGlossary(dir, terms); err != nil {
		t.Fatalf("writeGlossary: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, artifactGlossary))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	if len(got) != len(terms) {
		t.Fatalf("len(got)=%d want %d", len(got), len(terms))
	}
	for i, term := range terms {
		if got[i] != term {
			t.Errorf("[%d] %q != %q", i, got[i], term)
		}
	}
}

func TestWriteGlossary_EmptyProducesEmptyArray(t *testing.T) {
	dir := t.TempDir()
	if err := writeGlossary(dir, nil); err != nil {
		t.Fatalf("writeGlossary: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, artifactGlossary))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("want [], got %s", b)
	}
}

func TestWritePhrases_ShapeIsValidYAMLSubset(t *testing.T) {
	dir := t.TempDir()
	doc := phrasesDoc{
		Identifier:  "ai.acme.gardenio.dictation",
		CustomWords: []string{"Gardenio", "kbeads"},
		Phrases:     []string{"bead k d dash A B C", "Gardenio controller crashed"},
	}
	if err := writePhrases(dir, doc); err != nil {
		t.Fatalf("writePhrases: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, artifactPhrases))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"locale: \"en-US\"",
		"identifier: \"ai.acme.gardenio.dictation\"",
		"version: 1",
		"custom_words:",
		"  - \"Gardenio\"",
		"  - \"kbeads\"",
		"phrases:",
		"  - \"bead k d dash A B C\"",
		"  - \"Gardenio controller crashed\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s\n---", want, s)
		}
	}
}

func TestWritePhrases_EscapesHazards(t *testing.T) {
	dir := t.TempDir()
	doc := phrasesDoc{
		Phrases: []string{`he said "hi"`, `leading: colon`, `- dash start`, "tab\there"},
	}
	if err := writePhrases(dir, doc); err != nil {
		t.Fatalf("writePhrases: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, artifactPhrases))
	s := string(b)
	// Every phrase must appear inside a double-quoted scalar with escaped
	// internal quotes — json.Marshal produces this shape.
	for _, want := range []string{
		`  - "he said \"hi\""`,
		`  - "leading: colon"`,
		`  - "- dash start"`,
		`  - "tab\there"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing escaped scalar %q\n---\n%s\n---", want, s)
		}
	}
}

func TestWriteCorpus_OneChunkPerLineDropsEmpty(t *testing.T) {
	dir := t.TempDir()
	chunks := []string{"first chunk", "", "second  chunk\nwith newline", "   "}
	if err := writeCorpus(dir, chunks); err != nil {
		t.Fatalf("writeCorpus: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, artifactCorpus))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	if lines[0] != "first chunk" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if strings.Contains(lines[1], "\n") {
		t.Errorf("line 1 still has newline: %q", lines[1])
	}
}
