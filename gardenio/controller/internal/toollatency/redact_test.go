package toollatency

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactText(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantRedacted  []string // substrings that must NOT survive
		wantPreserved []string // substrings that must survive
	}{
		{
			name:          "github token prefix",
			in:            "curl -H 'Authorization: token ghp_abcdefghijklmnopqrstuvwxyz0123456789' https://api.github.com", // gitleaks:allow: fake token for the redaction test
			wantRedacted:  []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789"},                                             // gitleaks:allow: fake token for the redaction test
			wantPreserved: []string{"curl", "api.github.com"},
		},
		{
			name:          "anthropic key prefix",
			in:            "export ANTHROPIC_API_KEY=sk-ant-abc123DEF456",
			wantRedacted:  []string{"sk-ant-abc123DEF456"},
			wantPreserved: []string{"export", "ANTHROPIC_API_KEY"},
		},
		{
			name:          "bearer token",
			in:            "curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.secretpayload' https://example.com", // gitleaks:allow: fake token for the redaction test
			wantRedacted:  []string{"eyJhbGciOiJIUzI1NiJ9.secretpayload"},
			wantPreserved: []string{"Bearer", "curl", "example.com"},
		},
		{
			name:          "env assignment secret",
			in:            "DB_PASSWORD=sup3rsecret psql -h localhost",
			wantRedacted:  []string{"sup3rsecret"},
			wantPreserved: []string{"DB_PASSWORD", "psql", "localhost"},
		},
		{
			name:          "cli flag secret",
			in:            "mycli login --password hunter2 --user alice",
			wantRedacted:  []string{"hunter2"},
			wantPreserved: []string{"mycli", "login", "alice"},
		},
		{
			name:          "cli flag secret equals form",
			in:            "mycli login --token=abc123xyz --user alice",
			wantRedacted:  []string{"abc123xyz"},
			wantPreserved: []string{"mycli", "login", "alice"},
		},
		{
			name:          "no secret present",
			in:            "git push origin main",
			wantPreserved: []string{"git push origin main"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactText(tc.in)
			for _, bad := range tc.wantRedacted {
				if strings.Contains(got, bad) {
					t.Errorf("RedactText(%q) = %q, still contains secret %q", tc.in, got, bad)
				}
			}
			for _, keep := range tc.wantPreserved {
				if !strings.Contains(got, keep) {
					t.Errorf("RedactText(%q) = %q, lost expected content %q", tc.in, got, keep)
				}
			}
		})
	}
}

// TestRedactJSON_SeededTokenComesBackRedacted is acceptance criterion #3 of
// gd-sWwPnwkZtw: "a seeded row containing a token comes back redacted."
func TestRedactJSON_SeededTokenComesBackRedacted(t *testing.T) {
	raw := []byte(`{"command":"curl -H 'Authorization: token ghp_0123456789abcdefghijklmnopqrstuvwxyz' https://api.github.com","env":{"GITHUB_TOKEN":"ghp_0123456789abcdefghijklmnopqrstuvwxyz"}}`) // gitleaks:allow: fake token for the redaction test
	got := RedactJSON(raw)
	if strings.Contains(string(got), "ghp_0123456789abcdefghijklmnopqrstuvwxyz") { // gitleaks:allow: fake token for the redaction test
		t.Fatalf("RedactJSON leaked the seeded token: %s", got)
	}

	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("RedactJSON produced invalid JSON: %v; got %s", err, got)
	}
	if decoded["env"].(map[string]any)["GITHUB_TOKEN"] != redactedPlaceholder {
		t.Errorf("expected GITHUB_TOKEN value fully blanked, got %v", decoded["env"])
	}
	if !strings.Contains(decoded["command"].(string), "curl") {
		t.Errorf("expected non-secret content preserved in command, got %v", decoded["command"])
	}
}

func TestRedactJSON_MalformedFallsBackToTextRedaction(t *testing.T) {
	raw := []byte(`not valid json but has ghp_abcdefghijklmnopqrstuvwxyz0123456789 in it`) // gitleaks:allow: fake token for the redaction test
	got := RedactJSON(raw)
	if strings.Contains(string(got), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") { // gitleaks:allow: fake token for the redaction test
		t.Fatalf("RedactJSON leaked token on malformed input: %s", got)
	}
}

func TestRedactJSON_EmptyInput(t *testing.T) {
	if got := RedactJSON(nil); len(got) != 0 {
		t.Errorf("RedactJSON(nil) = %q, want empty", got)
	}
}
