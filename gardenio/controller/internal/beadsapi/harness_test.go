package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseHarness(t *testing.T) {
	for in, want := range map[string]string{"": "", "  ": "", "claude": "claude", "codex": "codex", " codex\n": "codex", "gemini": "gemini", " gemini ": "gemini"} {
		if got, err := ParseHarness(in); err != nil || got != want {
			t.Errorf("ParseHarness(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Codex", "CLAUDE", "Gemini", "gpt", "claude,codex"} {
		if _, err := ParseHarness(in); err == nil {
			t.Errorf("ParseHarness(%q) should fail", in)
		}
	}
}

func TestStampResolvedHarness(t *testing.T) {
	var patch map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"bd-1","type":"agent","fields":{"harness":"claude"}}`))
		case http.MethodPatch:
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			patch = body.Fields
			_, _ = w.Write([]byte(`{"id":"bd-1"}`))
		}
	}))
	defer srv.Close()
	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.StampResolvedHarness(context.Background(), "bd-1", HarnessCodex); err != nil {
		t.Fatalf("StampResolvedHarness: %v", err)
	}
	// Only resolved_harness is written: the agent-level override ("harness")
	// must not be touched, or the result would become a sticky override.
	if len(patch) != 1 || patch[FieldResolvedHarness] != "codex" {
		t.Errorf("patched fields = %v, want only {resolved_harness: codex}", patch)
	}

	for _, tc := range []struct{ id, harness string }{{"", "codex"}, {"bd-1", ""}, {"bd-1", "gpt"}, {"bd-1", "Codex"}} {
		patch = nil
		if err := c.StampResolvedHarness(context.Background(), tc.id, tc.harness); err == nil {
			t.Errorf("StampResolvedHarness(%q, %q) should fail", tc.id, tc.harness)
		}
		if patch != nil {
			t.Errorf("StampResolvedHarness(%q, %q) wrote %v despite the error", tc.id, tc.harness, patch)
		}
	}
}
