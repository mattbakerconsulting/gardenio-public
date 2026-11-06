package themeglyphs

import (
	"reflect"
	"strings"
	"testing"
)

const src = `package bridge

var Garden = Theme{
	Name: "garden",
	BeadType: map[string]string{"task": "🌱", "plan": "🗺️"},
	SemanticGlyphs: map[string]string{"ok": "🌼", "again": "🌼"},
	ReactionNames: map[string]string{"seen": "seedling", "done": "white_check_mark"},
	Avatar: "leaf mark — white leaf",
}

var Business = Theme{
	Lifecycle: map[string]string{"spawning": "○", "reaped": "◻"},
	BeadType: map[string]string{"task": "[TASK]"},
}

var Other = Theme{BeadType: map[string]string{"task": "🦊"}}
`

func TestExtract(t *testing.T) {
	got, err := Extract([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by string; ASCII-only values and the Other var are left out,
	// reactions become their emoji, 🌱 is found twice and kept once.
	want := []string{"—", "○", "◻", "✅", "🌱", "🌼", "🗺️"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract = %q, want %q", got, want)
	}
}

func TestExtractUnknownReaction(t *testing.T) {
	bad := strings.Replace(src, `"seedling"`, `"cactus"`, 1)
	if _, err := Extract([]byte(bad)); err == nil || !strings.Contains(err.Error(), `"cactus"`) {
		t.Fatalf("want an error naming the unknown reaction, got %v", err)
	}
}

func TestExtractMissingTheme(t *testing.T) {
	bad := strings.Replace(src, "var Business", "var Corporate", 1)
	if _, err := Extract([]byte(bad)); err == nil || !strings.Contains(err.Error(), "Business") {
		t.Fatalf("want an error naming the missing theme, got %v", err)
	}
}

func TestKey(t *testing.T) {
	if Key("⚙️") != Key("⚙") || Key("☂︎") != "☂" {
		t.Fatal("Key must drop presentation selectors")
	}
}
