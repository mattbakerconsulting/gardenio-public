package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFields(t *testing.T) {
	tests := []struct {
		name    string
		pairs   []string
		want    string
		wantErr bool
	}{
		{
			name:  "nil input",
			pairs: nil,
			want:  "",
		},
		{
			name:  "plain strings",
			pairs: []string{"question=What next?", "requestor=agent-1"},
			want:  `{"question":"What next?","requestor":"agent-1"}`,
		},
		{
			name:  "json array value",
			pairs: []string{`options=[{"id":"done","label":"Done"}]`},
			want:  `{"options":[{"id":"done","label":"Done"}]}`,
		},
		{
			name:  "json object value",
			pairs: []string{`meta={"key":"val"}`},
			want:  `{"meta":{"key":"val"}}`,
		},
		{
			name:  "boolean and number",
			pairs: []string{"active=true", "count=42", "ratio=3.14"},
			want:  `{"active":true,"count":42,"ratio":3.14}`,
		},
		{
			name:  "null value",
			pairs: []string{"cleared=null"},
			want:  `{"cleared":null}`,
		},
		{
			name:  "quoted json string",
			pairs: []string{`name="hello"`},
			want:  `{"name":"hello"}`,
		},
		{
			name:  "plain number-like string that is not valid json",
			pairs: []string{"version=1.2.3"},
			want:  `{"version":"1.2.3"}`,
		},
		{
			name:    "missing equals",
			pairs:   []string{"noequals"},
			wantErr: true,
		},
		{
			name:    "empty key",
			pairs:   []string{"=value"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFields(tt.pairs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("expected nil, got %s", got)
				}
				return
			}
			// Compare as unmarshaled maps to ignore key ordering.
			var wantMap, gotMap map[string]any
			if err := json.Unmarshal([]byte(tt.want), &wantMap); err != nil {
				t.Fatalf("bad test want: %v", err)
			}
			if err := json.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("result is not valid JSON: %s", got)
			}
			// Re-marshal both to canonical form for comparison.
			wantJSON, _ := json.Marshal(wantMap)
			gotJSON, _ := json.Marshal(gotMap)
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("got  %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestParseFieldsWithUnset covers the --unset-field path (ka-0000007246):
// each unset key must be emitted as an explicit JSON null so the daemon
// deletes it (RFC 7386), and a key given to both -f and --unset-field is an
// error.
func TestParseFieldsWithUnset(t *testing.T) {
	// Pure unset → JSON null.
	got, err := parseFieldsWithUnset(nil, []string{"model"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"model":null}` {
		t.Errorf("unset-only wire = %s, want {\"model\":null}", got)
	}

	// Mixed set + unset → both keys, null for the unset one.
	got, err = parseFieldsWithUnset([]string{"account=acct-1"}, []string{"model"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("result not valid JSON: %s", got)
	}
	if v, ok := m["model"]; !ok || v != nil {
		t.Errorf("model = %v (present=%v), want explicit null", v, ok)
	}
	if m["account"] != "acct-1" {
		t.Errorf("account = %v, want acct-1", m["account"])
	}

	// Empty inputs → nil, matching parseFields.
	if got, err := parseFieldsWithUnset(nil, nil); err != nil || got != nil {
		t.Errorf("empty inputs = (%s, %v), want (nil, nil)", got, err)
	}

	// Conflict: same key in -f and --unset-field is rejected.
	if _, err := parseFieldsWithUnset([]string{"model=claude"}, []string{"model"}); err == nil {
		t.Error("expected error for key set with both -f and --unset-field, got nil")
	}

	// Empty unset key is rejected.
	if _, err := parseFieldsWithUnset(nil, []string{""}); err == nil {
		t.Error("expected error for empty --unset-field key, got nil")
	}
}

// TestParseFieldsFileValue covers the key=@file syntax (ka-0000002135): the
// file contents must land in the field as a literal string, even when they
// are valid JSON-object text that rawOrString would otherwise embed raw.
func TestParseFieldsFileValue(t *testing.T) {
	dir := t.TempDir()

	dashboard := `{"title":"My Dashboard","panels":[{"id":1,"type":"graph"}]}`
	dashPath := filepath.Join(dir, "dashboard.json")
	if err := os.WriteFile(dashPath, []byte(dashboard), 0o600); err != nil {
		t.Fatalf("write dashboard: %v", err)
	}

	multiline := "line one\nline two\nline three"
	mlPath := filepath.Join(dir, "multiline.txt")
	if err := os.WriteFile(mlPath, []byte(multiline), 0o600); err != nil {
		t.Fatalf("write multiline: %v", err)
	}

	t.Run("json object from file stays a string", func(t *testing.T) {
		got, err := parseFields([]string{"content=@" + dashPath})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("result not valid JSON: %s", got)
		}
		s, ok := m["content"].(string)
		if !ok {
			t.Fatalf("content is %T, want string (value: %v)", m["content"], m["content"])
		}
		if s != dashboard {
			t.Errorf("content = %q, want %q", s, dashboard)
		}
	})

	t.Run("multiline value from file survives intact", func(t *testing.T) {
		got, err := parseFields([]string{"notes=@" + mlPath})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("result not valid JSON: %s", got)
		}
		if m["notes"] != multiline {
			t.Errorf("notes = %q, want %q", m["notes"], multiline)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := parseFields([]string{"content=@" + filepath.Join(dir, "nope.json")}); err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})
}

func TestResolveFieldValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// @@ escape: literal value beginning with a single '@'.
	v, err := resolveFieldValue("@@literal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "@literal" {
		t.Errorf("@@ escape = %v, want @literal", v)
	}

	// @file -> contents as a literal string, never raw JSON.
	v, err = resolveFieldValue("@" + path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s, ok := v.(string); !ok || s != `{"a":1}` {
		t.Errorf("@file = %#v, want string `{\"a\":1}`", v)
	}

	// plain value still flows through rawOrString (object embedded raw).
	v, err = resolveFieldValue(`{"a":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := v.(json.RawMessage); !ok {
		t.Errorf("plain object value = %T, want json.RawMessage", v)
	}
}

// TestResolveTextValue is the regression guard for ka-0000003715: `-d @file`
// (and --notes/--title) must read the FILE, and a missing @path must fail loud
// rather than silently persist the literal pointer string as the body.
func TestResolveTextValue(t *testing.T) {
	dir := t.TempDir()
	body := "# Plan of record\n\nThis is an 18KB design doc, not a pointer.\n"
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	// @file → the file's contents become the value (what the caller meant).
	got, err := resolveTextValue("@" + path)
	if err != nil {
		t.Fatalf("@file unexpected error: %v", err)
	}
	if got != body {
		t.Errorf("@file = %q, want the file body %q", got, body)
	}

	// @@ escape → a literal value beginning with a single '@' (e.g. an
	// intentional description that starts with "@mention").
	got, err = resolveTextValue("@@mention the operator")
	if err != nil {
		t.Fatalf("@@ escape unexpected error: %v", err)
	}
	if got != "@mention the operator" {
		t.Errorf("@@ escape = %q, want %q", got, "@mention the operator")
	}

	// Plain text is returned verbatim (the overwhelmingly common case).
	got, err = resolveTextValue("just an inline description")
	if err != nil {
		t.Fatalf("plain unexpected error: %v", err)
	}
	if got != "just an inline description" {
		t.Errorf("plain = %q, want verbatim", got)
	}

	// THE BUG: a bare "@/path" that does NOT exist must ERROR, not silently
	// store the literal string. This is the exact footgun that clobbered a
	// plan-of-record bead twice — a dangling scratchpad pointer.
	if _, err := resolveTextValue("@" + filepath.Join(dir, "gone.md")); err == nil {
		t.Fatal("expected error for missing @file, got nil — the data-loss footgun is back")
	}
}
