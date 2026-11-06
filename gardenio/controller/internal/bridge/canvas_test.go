package bridge

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestShouldUseCanvas(t *testing.T) {
	short := strings.Repeat("x", canvasContentThreshold-1)
	if shouldUseCanvas(short) {
		t.Error("content below threshold should not use canvas")
	}

	exact := strings.Repeat("x", canvasContentThreshold)
	if shouldUseCanvas(exact) {
		t.Error("content at exact threshold should not use canvas")
	}

	long := strings.Repeat("x", canvasContentThreshold+1)
	if !shouldUseCanvas(long) {
		t.Error("content above threshold should use canvas")
	}
}

func TestFirstLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{"short", "hello", 100, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncate at newline", "line1\nline2\nline3", 12, "line1\nline2"},
		{"truncate no newline", strings.Repeat("x", 600), 500, strings.Repeat("x", 500)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := firstLines(tc.input, tc.n)
			if got != tc.want {
				t.Errorf("firstLines(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.want)
			}
		})
	}
}

func TestCanvasReportBlocks(t *testing.T) {
	blocks := canvasReportBlocks(":page_facing_up:", "plan", "My Decision", "canvas-123", "preview text")

	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(blocks))
	}

	// First block is a divider.
	if _, ok := blocks[0].(*slack.DividerBlock); !ok {
		t.Errorf("expected first block to be DividerBlock, got %T", blocks[0])
	}
}

func TestCanvasReportBlocks_NoPreview(t *testing.T) {
	blocks := canvasReportBlocks(":page_facing_up:", "plan", "My Decision", "canvas-123", "")

	// Without preview, should be 3 blocks (divider, section, context).
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks without preview, got %d", len(blocks))
	}
}
