package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── parseSecretEntries ─────────────────────────────────────────────────────

func TestParseSecretEntries(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"null", "null", 0, false},
		{"whitespace", "  ", 0, false},
		{"empty array", "[]", 0, false},
		{
			name: "native array",
			raw:  `[{"env":"GIT_TOKEN","secret":"s","key":"token"}]`,
			want: 1,
		},
		{
			name: "multi entry",
			raw:  `[{"env":"A","secret":"s","key":"k"},{"env":"B","secret":"s2","key":"k2"}]`,
			want: 2,
		},
		{
			// Some legacy project beads store secrets as a JSON STRING whose
			// content is the array text. fieldsMap()/ParseFieldsJSON normalizes
			// the native-array and string-wrapped forms to the same array text
			// before parseSecretEntries sees it, so a plain string field never
			// reaches here — but pin the array-shape parse regardless so a
			// future change that surfaces the raw string is caught.
			name: "array text (post-normalization)",
			raw:  `[{"env":"GITLAB_TOKEN","secret":"gl","key":"token"}]`,
			want: 1,
		},
		{
			name:    "malformed object not array",
			raw:     `{"env":"X"}`,
			wantErr: true,
		},
		{
			name:    "garbage",
			raw:     `not json`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSecretEntries(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("got %d entries, want %d", len(got), tt.want)
			}
		})
	}
}

// ── upsertSecretEntry (idempotency) ────────────────────────────────────────

func TestUpsertSecretEntry_Append(t *testing.T) {
	entries := []beadsapi.SecretEntry{
		{Env: "GIT_TOKEN", Secret: "s", Key: "token"},
	}
	want := beadsapi.SecretEntry{Env: "GITLAB_TOKEN", Secret: "gl", Key: "token"}
	got, updated := upsertSecretEntry(entries, want)
	if updated {
		t.Error("expected append (updated=false) for a new env")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[1] != want {
		t.Errorf("appended entry = %+v, want %+v", got[1], want)
	}
}

func TestUpsertSecretEntry_UpdateInPlace(t *testing.T) {
	entries := []beadsapi.SecretEntry{
		{Env: "GIT_TOKEN", Secret: "s", Key: "token"},
		{Env: "GITLAB_TOKEN", Secret: "old-secret", Key: "old-key"},
	}
	want := beadsapi.SecretEntry{Env: "GITLAB_TOKEN", Secret: "new-secret", Key: "new-key"}
	got, updated := upsertSecretEntry(entries, want)
	if !updated {
		t.Error("expected update (updated=true) for an existing env")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (no dup), got %d", len(got))
	}
	if got[1] != want {
		t.Errorf("updated entry = %+v, want %+v", got[1], want)
	}
}

func TestUpsertSecretEntry_IdempotentRepeat(t *testing.T) {
	// Adding the same env twice must never produce a duplicate.
	want := beadsapi.SecretEntry{Env: "GITLAB_TOKEN", Secret: "gl", Key: "token"}
	var entries []beadsapi.SecretEntry
	entries, _ = upsertSecretEntry(entries, want)
	entries, updated := upsertSecretEntry(entries, want)
	if !updated {
		t.Error("second add of same env should report updated=true")
	}
	if len(entries) != 1 {
		t.Fatalf("idempotency violated: got %d entries, want 1", len(entries))
	}
}

// ── awsCreateSecretHint / awsPutSecretValueHint (printed AWS path) ──────────

func TestAwsCreateSecretHint(t *testing.T) {
	got := awsCreateSecretHint("gardenio-gitlab-token", "token")
	want := `aws secretsmanager create-secret --name gardenio/gardenio-gitlab-token --secret-string '{"token":"<value>"}'`
	if got != want {
		t.Errorf("hint =\n  %q\nwant\n  %q", got, want)
	}
}

func TestAwsCreateSecretHint_CustomKey(t *testing.T) {
	got := awsCreateSecretHint("daily-bundle-git-credentials", "username")
	if !strings.Contains(got, "--name gardenio/daily-bundle-git-credentials") {
		t.Errorf("missing gardenio/ path prefix: %q", got)
	}
	if !strings.Contains(got, `{"username":"<value>"}`) {
		t.Errorf("missing key in JSON: %q", got)
	}
}

func TestAwsPutSecretValueHint(t *testing.T) {
	got := awsPutSecretValueHint("gardenio-gitlab-token", "token")
	want := `aws secretsmanager put-secret-value --secret-id gardenio/gardenio-gitlab-token --secret-string '{"token":"<value>"}'`
	if got != want {
		t.Errorf("hint =\n  %q\nwant\n  %q", got, want)
	}
}

// ── runSecretAdd: flag validation ──────────────────────────────────────────

func TestRunSecretAdd_MissingFlags(t *testing.T) {
	cases := []struct{ env, secret, key string }{
		{"", "s", "token"},
		{"E", "", "token"},
		{"E", "s", ""},
	}
	for _, c := range cases {
		secretAddEnv, secretAddSecret, secretAddKey = c.env, c.secret, c.key
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runSecretAdd(cmd, []string{"gardenio"}); err == nil {
			t.Errorf("expected error for env=%q secret=%q key=%q", c.env, c.secret, c.key)
		}
	}
	secretAddEnv, secretAddSecret, secretAddKey = "", "", ""
}

// ── runSecretAdd: end-to-end append against a fake daemon ──────────────────

func TestRunSecretAdd_AppendsAndPrintsHint(t *testing.T) {
	var patched json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-PROJ"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
				"labels": []string{},
				"fields": json.RawMessage(`{"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-git","key":"token"}]}`),
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-PROJ"):
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			patched = body["fields"]
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			// listBeads (type=project) for GetProjectBead
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
						"labels": []string{},
						"fields": json.RawMessage(`{"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-git","key":"token"}]}`),
					},
				},
				"total": 1,
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	secretAddEnv, secretAddSecret, secretAddKey = "GITLAB_TOKEN", "gardenio-gitlab-token", "token"
	t.Cleanup(func() { secretAddEnv, secretAddSecret, secretAddKey = "", "", "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	out := captureSecretStdout(t, func() error { return runSecretAdd(cmd, []string{"gardenio"}) })

	// The printed AWS path must be the exact copy-pasteable command.
	wantHint := `aws secretsmanager create-secret --name gardenio/gardenio-gitlab-token --secret-string '{"token":"<value>"}'`
	if !strings.Contains(out, wantHint) {
		t.Errorf("output missing AWS create-secret hint.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "gb did NOT create the AWS secret") {
		t.Errorf("output should make clear gb does not create the secret.\nGot:\n%s", out)
	}

	// The PATCH must have appended the new entry, preserving the existing one.
	var fields map[string][]beadsapi.SecretEntry
	if err := json.Unmarshal(patched, &fields); err != nil {
		t.Fatalf("patched fields not the expected shape: %v (%s)", err, patched)
	}
	entries := fields["secrets"]
	if len(entries) != 2 {
		t.Fatalf("expected 2 secret entries after append, got %d (%s)", len(entries), patched)
	}
	if entries[1].Env != "GITLAB_TOKEN" || entries[1].Secret != "gardenio-gitlab-token" {
		t.Errorf("appended entry wrong: %+v", entries[1])
	}
}

func TestRunSecretAdd_UpdateIsIdempotent(t *testing.T) {
	var patched json.RawMessage
	existing := `{"secrets":[{"env":"GITLAB_TOKEN","secret":"old","key":"token"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-PROJ"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
				"labels": []string{}, "fields": json.RawMessage(existing),
			})
		case r.Method == http.MethodPatch:
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			patched = body["fields"]
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
						"labels": []string{}, "fields": json.RawMessage(existing)},
				}, "total": 1,
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	secretAddEnv, secretAddSecret, secretAddKey = "GITLAB_TOKEN", "new-secret", "token"
	t.Cleanup(func() { secretAddEnv, secretAddSecret, secretAddKey = "", "", "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	out := captureSecretStdout(t, func() error { return runSecretAdd(cmd, []string{"gardenio"}) })

	if !strings.Contains(out, "Updated secret mapping") {
		t.Errorf("expected 'Updated' wording for existing env.\nGot:\n%s", out)
	}
	var fields map[string][]beadsapi.SecretEntry
	if err := json.Unmarshal(patched, &fields); err != nil {
		t.Fatalf("bad patch: %v", err)
	}
	if len(fields["secrets"]) != 1 {
		t.Fatalf("idempotency violated: expected 1 entry, got %d", len(fields["secrets"]))
	}
	if fields["secrets"][0].Secret != "new-secret" {
		t.Errorf("entry not updated in place: %+v", fields["secrets"][0])
	}
}

func TestRunSecretAdd_MalformedSecretsRejected(t *testing.T) {
	// A project bead whose secrets field is a malformed object (not an array)
	// must be refused, not silently clobbered.
	bad := `{"secrets":{"env":"X"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-PROJ"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
				"labels": []string{}, "fields": json.RawMessage(bad),
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{"id": "gd-PROJ", "title": "gardenio", "type": "project", "status": "open",
						"labels": []string{}, "fields": json.RawMessage(bad)},
				}, "total": 1,
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	secretAddEnv, secretAddSecret, secretAddKey = "GITLAB_TOKEN", "s", "token"
	t.Cleanup(func() { secretAddEnv, secretAddSecret, secretAddKey = "", "", "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSecretAdd(cmd, []string{"gardenio"}); err == nil {
		t.Error("expected error for malformed secrets[] field")
	}
}

func TestRunSecretAdd_UnknownProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	secretAddEnv, secretAddSecret, secretAddKey = "GITLAB_TOKEN", "s", "token"
	t.Cleanup(func() { secretAddEnv, secretAddSecret, secretAddKey = "", "", "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSecretAdd(cmd, []string{"nonexistent"}); err == nil {
		t.Error("expected error for unknown project")
	}
}

// captureSecretStdout runs fn with os.Stdout redirected and returns what it
// wrote. Mirrors captureStdout in news_test.go but returns a string and treats
// a non-nil fn error as a fatal (the success-path tests expect no error).
func captureSecretStdout(t *testing.T, fn func() error) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	runErr := fn()
	_ = w.Close()
	out := <-done
	if runErr != nil {
		t.Fatalf("runSecretAdd: %v", runErr)
	}
	return out
}
