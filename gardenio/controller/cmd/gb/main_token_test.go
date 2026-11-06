package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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

// A long-lived gb's client reads the file on each request.
func TestTokenFileSourceRereads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	src := tokenFile(path)
	if _, err := src.Token(t.Context()); err == nil {
		t.Error("missing file: want an error")
	}
	for _, tok := range []string{"one", "two"} {
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := src.Token(t.Context()); err != nil || got != tok {
			t.Errorf("Token() = %q, %v; want %q", got, err, tok)
		}
	}
}

// A token file that cannot be read stops gb: it must not send the credential
// store's token, which may be the person's who started a land agent.
func TestRootRefusesAnUnreadableTokenFile(t *testing.T) {
	saved, savedURL := daemon, httpURL
	t.Cleanup(func() { daemon, httpURL = saved, savedURL })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GARDENIO_NO_LOGIN_SESSION", "1")
	t.Setenv(tokenFileEnv, filepath.Join(t.TempDir(), "gone"))
	httpURL = "http://127.0.0.1:1"
	err := rootCmd.PersistentPreRunE(&cobra.Command{Use: "probe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("PersistentPreRunE = %v, want a refusal for the missing token file", err)
	}
}

// A long-lived gb's client asks the token file on each request.
func TestRootReadsTheTokenFileOnEachRequest(t *testing.T) {
	saved, savedURL := daemon, httpURL
	t.Cleanup(func() { daemon, httpURL = saved, savedURL })
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GARDENIO_NO_LOGIN_SESSION", "1")
	path := filepath.Join(t.TempDir(), "token")
	t.Setenv(tokenFileEnv, path)
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	httpURL = srv.URL
	if err := rootCmd.PersistentPreRunE(&cobra.Command{Use: "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = daemon.Me(t.Context())
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = daemon.Me(t.Context())
	if want := []string{"Bearer one", "Bearer two"}; !slices.Equal(got, want) {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
