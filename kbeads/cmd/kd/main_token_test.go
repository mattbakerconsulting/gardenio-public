package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDefaultTokenResolution(t *testing.T) {
	envs := []string{"GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"}
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"none-set", nil, ""},
		{"only-gardenio", map[string]string{"GARDENIO_TOKEN": "gb-1"}, "gb-1"},
		{"only-kbeads", map[string]string{"KBEADS_TOKEN": "kb-1"}, "kb-1"},
		{"only-beads-auth", map[string]string{"BEADS_AUTH_TOKEN": "ba-1"}, "ba-1"},
		{"only-beads-daemon", map[string]string{"BEADS_DAEMON_TOKEN": "bd-1"}, "bd-1"},
		{
			name: "gardenio-wins-over-kbeads",
			set:  map[string]string{"GARDENIO_TOKEN": "gb-1", "KBEADS_TOKEN": "kb-1"},
			want: "gb-1",
		},
		{
			name: "kbeads-wins-over-beads-auth",
			set:  map[string]string{"KBEADS_TOKEN": "kb-1", "BEADS_AUTH_TOKEN": "ba-1"},
			want: "kb-1",
		},
		{
			name: "beads-auth-wins-over-beads-daemon",
			set:  map[string]string{"BEADS_AUTH_TOKEN": "ba-1", "BEADS_DAEMON_TOKEN": "bd-1"},
			want: "ba-1",
		},
		{
			name: "gardenio-wins-over-all",
			set: map[string]string{
				"GARDENIO_TOKEN":     "gb-1",
				"KBEADS_TOKEN":       "kb-1",
				"BEADS_AUTH_TOKEN":   "ba-1",
				"BEADS_DAEMON_TOKEN": "bd-1",
			},
			want: "gb-1",
		},
		{
			name: "empty-string-is-skipped",
			set:  map[string]string{"GARDENIO_TOKEN": "", "KBEADS_TOKEN": "kb-1"},
			want: "kb-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range envs {
				t.Setenv(k, "")
			}
			for k, v := range tc.set {
				t.Setenv(k, v)
			}
			if got := defaultToken(); got != tc.want {
				t.Errorf("defaultToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A JSON-wrapped bearer ({"token":"<hex>"}) is what a misconfigured
// ExternalSecret mirror injects; sending it verbatim 401s every call
// (bug ka-0000003595 / ka-0000001051).
func TestUnwrapTokenJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw-hex-passthrough", "abc123def456", "abc123def456"},
		{"json-wrapped", `{"token":"abc123def456"}`, "abc123def456"},              // gitleaks:allow: fake token
		{"json-wrapped-whitespace", ` {"token":"abc123def456"} `, "abc123def456"}, // gitleaks:allow: fake token
		{"json-extra-fields", `{"token":"abc123","other":"x"}`, "abc123"},
		{"json-empty-token", `{"token":""}`, `{"token":""}`},
		{"json-no-token-field", `{"other":"x"}`, `{"other":"x"}`},
		{"malformed-json", `{"token":`, `{"token":`},
		{"brace-but-not-json", "{not-json}", "{not-json}"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapTokenJSON(tc.in); got != tc.want {
				t.Errorf("unwrapTokenJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultTokenUnwrapsJSONWrappedEnv(t *testing.T) {
	for _, k := range []string{"GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("BEADS_AUTH_TOKEN", `{"token":"deadbeef"}`)
	if got := defaultToken(); got != "deadbeef" {
		t.Errorf("defaultToken() = %q, want %q", got, "deadbeef")
	}
}

// GARDENIO_TOKEN_FILE wins over every token variable and is read on each
// call, so a token renewed into it is the next one sent (gd-Vbvd3EgqTI). A
// file that cannot be read resolves no token: never another variable's.
func TestDefaultTokenFile(t *testing.T) {
	t.Setenv("GARDENIO_TOKEN", "static")
	path := filepath.Join(t.TempDir(), "token")
	t.Setenv(tokenFileEnv, path)
	if got := defaultToken(); got != "" {
		t.Errorf("missing file: defaultToken() = %q, want none", got)
	}
	for _, tc := range []struct{ body, want string }{
		{"first\n", "first"},
		{"second", "second"},
		{`{"token":"third"}`, "third"},
		{"  \n", ""},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := defaultToken(); got != tc.want {
			t.Errorf("file %q: defaultToken() = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A token file that cannot be read stops kd: it must not send the active
// remote's token, which may be the person's who started a land agent.
func TestRootRefusesAnUnreadableTokenFile(t *testing.T) {
	saved, savedURL := beadsClient, httpURL
	t.Cleanup(func() { beadsClient, httpURL = saved, savedURL })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GARDENIO_NO_LOGIN_SESSION", "1")
	t.Setenv(tokenFileEnv, filepath.Join(t.TempDir(), "gone"))
	httpURL = "http://127.0.0.1:1"
	err := rootCmd.PersistentPreRunE(&cobra.Command{Use: "probe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("PersistentPreRunE = %v, want a refusal for the missing token file", err)
	}
}
