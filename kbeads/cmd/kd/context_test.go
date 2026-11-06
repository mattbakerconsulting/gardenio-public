package main

import (
	"encoding/json"
	"testing"
)

func TestContextConfigDeserialization(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		format     string
		depth      int
		fieldCount int
	}{
		{
			name:   "detail format",
			input:  `{"sections":[{"header":"Details","view":"my-view","format":"detail","fields":["id","title","status"]}]}`,
			format: "detail",
			depth:  0,
			fieldCount: 3,
		},
		{
			name:   "tree format with depth",
			input:  `{"sections":[{"header":"Tree","view":"deps-view","format":"tree","depth":5}]}`,
			format: "tree",
			depth:  5,
		},
		{
			name:   "tree format default depth",
			input:  `{"sections":[{"header":"Tree","view":"deps-view","format":"tree"}]}`,
			format: "tree",
			depth:  0,
		},
		{
			name:   "table format",
			input:  `{"sections":[{"header":"Table","view":"my-view","format":"table"}]}`,
			format: "table",
		},
		{
			name:   "count format",
			input:  `{"sections":[{"header":"Count","view":"my-view","format":"count"}]}`,
			format: "count",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cc contextConfig
			if err := json.Unmarshal([]byte(tt.input), &cc); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if len(cc.Sections) != 1 {
				t.Fatalf("got %d sections, want 1", len(cc.Sections))
			}
			s := cc.Sections[0]
			if s.Format != tt.format {
				t.Errorf("format = %q, want %q", s.Format, tt.format)
			}
			if s.Depth != tt.depth {
				t.Errorf("depth = %d, want %d", s.Depth, tt.depth)
			}
			if tt.fieldCount > 0 && len(s.Fields) != tt.fieldCount {
				t.Errorf("got %d fields, want %d", len(s.Fields), tt.fieldCount)
			}
		})
	}
}
