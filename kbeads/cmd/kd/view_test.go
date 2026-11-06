package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestViewConfigDeserialization(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantDeps bool
		types    []string
		fields   []string
	}{
		{
			name:     "no deps",
			input:    `{"filter":{"status":["open"]},"sort":"priority","columns":["id","title"]}`,
			wantDeps: false,
		},
		{
			name:     "with deps",
			input:    `{"filter":{},"deps":{"types":["blocks","parent-child"],"fields":["id","title","status"]}}`,
			wantDeps: true,
			types:    []string{"blocks", "parent-child"},
			fields:   []string{"id", "title", "status"},
		},
		{
			name:     "deps empty config",
			input:    `{"filter":{},"deps":{}}`,
			wantDeps: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var vc viewConfig
			if err := json.Unmarshal([]byte(tt.input), &vc); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if tt.wantDeps && vc.Deps == nil {
				t.Fatal("expected Deps to be non-nil")
			}
			if !tt.wantDeps && vc.Deps != nil {
				t.Fatal("expected Deps to be nil")
			}
			if tt.wantDeps && vc.Deps != nil {
				if len(tt.types) > 0 {
					if len(vc.Deps.Types) != len(tt.types) {
						t.Fatalf("got %d types, want %d", len(vc.Deps.Types), len(tt.types))
					}
					for i, typ := range tt.types {
						if vc.Deps.Types[i] != typ {
							t.Errorf("types[%d] = %q, want %q", i, vc.Deps.Types[i], typ)
						}
					}
				}
				if len(tt.fields) > 0 {
					if len(vc.Deps.Fields) != len(tt.fields) {
						t.Fatalf("got %d fields, want %d", len(vc.Deps.Fields), len(tt.fields))
					}
					for i, f := range tt.fields {
						if vc.Deps.Fields[i] != f {
							t.Errorf("fields[%d] = %q, want %q", i, vc.Deps.Fields[i], f)
						}
					}
				}
			}
		})
	}
}

func TestBeadField(t *testing.T) {
	b := &model.Bead{
		ID:        "bd-123",
		Title:     "Test bead",
		Status:    model.StatusOpen,
		Type:      model.TypeTask,
		Kind:      model.KindIssue,
		Priority:  2,
		Assignee:  "alice",
		Owner:     "bob",
		CreatedBy: "charlie",
		Labels:    []string{"urgent", "backend"},
	}

	tests := []struct {
		col  string
		want string
	}{
		{"id", "bd-123"},
		{"title", "Test bead"},
		{"status", "open"},
		{"type", "task"},
		{"kind", "issue"},
		{"priority", "2"},
		{"assignee", "alice"},
		{"owner", "bob"},
		{"created_by", "charlie"},
		{"labels", "urgent,backend"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.col, func(t *testing.T) {
			got := beadField(b, tt.col)
			if got != tt.want {
				t.Errorf("beadField(%q) = %q, want %q", tt.col, got, tt.want)
			}
		})
	}
}

func TestBeadFieldTitleTruncation(t *testing.T) {
	longTitle := strings.Repeat("a", 60)
	b := &model.Bead{Title: longTitle}
	got := beadField(b, "title")
	if len(got) != 50 {
		t.Errorf("expected truncated title of length 50, got %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("expected truncated title to end with ...")
	}
}

// captureStdout captures stdout output from a function call.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(out)
}

func TestPrintBeadTableFiltered(t *testing.T) {
	b := &model.Bead{
		ID:       "bd-abc",
		Title:    "Filtered bead",
		Status:   model.StatusOpen,
		Type:     model.TypeTask,
		Kind:     model.KindIssue,
		Priority: 1,
		Assignee: "alice",
		Owner:    "bob",
	}

	t.Run("nil fields prints all", func(t *testing.T) {
		out := captureStdout(t, func() { printBeadTableFiltered(b, nil) })
		if !strings.Contains(out, "bd-abc") {
			t.Error("expected ID in output")
		}
		if !strings.Contains(out, "Filtered bead") {
			t.Error("expected title in output")
		}
		if !strings.Contains(out, "alice") {
			t.Error("expected assignee in output")
		}
	})

	t.Run("whitelist restricts output", func(t *testing.T) {
		out := captureStdout(t, func() { printBeadTableFiltered(b, []string{"id", "status"}) })
		if !strings.Contains(out, "bd-abc") {
			t.Error("expected ID in output")
		}
		if !strings.Contains(out, "open") {
			t.Error("expected status in output")
		}
		if strings.Contains(out, "Assignee") {
			t.Error("did not expect Assignee in filtered output")
		}
		if strings.Contains(out, "Title") {
			t.Error("did not expect Title label in filtered output")
		}
	})
}

func TestPrintDepSubSection(t *testing.T) {
	t.Run("empty deps", func(t *testing.T) {
		out := captureStdout(t, func() { printDepSubSection(nil, nil) })
		if out != "" {
			t.Errorf("expected empty output, got %q", out)
		}
	})

	t.Run("resolved deps", func(t *testing.T) {
		deps := []resolvedDep{
			{
				Dep:  &model.Dependency{DependsOnID: "bd-xyz", Type: model.DepBlocks},
				Bead: &model.Bead{ID: "bd-xyz", Title: "Setup DB", Status: model.StatusOpen},
			},
		}
		out := captureStdout(t, func() { printDepSubSection(deps, nil) })
		if !strings.Contains(out, "blocks:") {
			t.Error("expected dep type label")
		}
		if !strings.Contains(out, "bd-xyz") {
			t.Error("expected dep bead ID")
		}
		if !strings.Contains(out, "Setup DB") {
			t.Error("expected dep bead title")
		}
	})

	t.Run("unresolved bead fallback", func(t *testing.T) {
		deps := []resolvedDep{
			{
				Dep:  &model.Dependency{DependsOnID: "bd-missing", Type: model.DepBlocks},
				Bead: nil,
			},
		}
		out := captureStdout(t, func() { printDepSubSection(deps, nil) })
		if !strings.Contains(out, "bd-missing") {
			t.Error("expected fallback ID")
		}
		if !strings.Contains(out, "unresolved") {
			t.Error("expected unresolved label")
		}
	})

	t.Run("custom fields", func(t *testing.T) {
		deps := []resolvedDep{
			{
				Dep:  &model.Dependency{DependsOnID: "bd-xyz", Type: model.DepParentChild},
				Bead: &model.Bead{ID: "bd-xyz", Title: "Epic", Status: model.StatusInProgress, Assignee: "bob"},
			},
		}
		out := captureStdout(t, func() { printDepSubSection(deps, []string{"id", "assignee"}) })
		if !strings.Contains(out, "bd-xyz") {
			t.Error("expected dep bead ID")
		}
		if !strings.Contains(out, "bob") {
			t.Error("expected assignee")
		}
		if strings.Contains(out, "Epic") {
			t.Error("did not expect title with custom fields")
		}
	})
}

func TestPrintComments(t *testing.T) {
	t.Run("empty comments", func(t *testing.T) {
		out := captureStdout(t, func() { printComments(nil) })
		if out != "" {
			t.Errorf("expected empty output for nil comments, got %q", out)
		}
	})

	t.Run("with comments", func(t *testing.T) {
		comments := []*model.Comment{
			{
				Author:    "alice",
				Text:      "Looks good",
				CreatedAt: time.Now(),
			},
			{
				Author: "bob",
				Text:   "Needs changes",
			},
		}
		out := captureStdout(t, func() { printComments(comments) })
		if !strings.Contains(out, "Comments:") {
			t.Error("expected Comments header")
		}
		if !strings.Contains(out, "alice") {
			t.Error("expected author alice")
		}
		if !strings.Contains(out, "Looks good") {
			t.Error("expected comment text")
		}
		if !strings.Contains(out, "bob") {
			t.Error("expected author bob")
		}
	})
}

func TestExpandVar(t *testing.T) {
	origActor := actor
	defer func() { actor = origActor }()

	actor = "test-user"

	tests := []struct {
		input string
		want  string
	}{
		{"$BEADS_ACTOR", "test-user"},
		{"assigned to $BEADS_ACTOR today", "assigned to test-user today"},
		{"no vars here", "no vars here"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := expandVar(tt.input)
			if got != tt.want {
				t.Errorf("expandVar(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
