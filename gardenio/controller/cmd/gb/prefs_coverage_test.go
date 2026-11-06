package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── resolveUserBead ──────────────────────────────────────────────────────

func TestResolveUserBead_NoUser(t *testing.T) {
	t.Setenv("SLACK_USER_ID", "")
	cmd := &cobra.Command{}
	cmd.Flags().String("user", "", "")
	cmd.SetContext(context.Background())
	if _, err := resolveUserBead(cmd, ""); err == nil {
		t.Error("expected error")
	}
}

func TestResolveUserBead_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if _, err := resolveUserBead(cmd, "U1"); err == nil {
		t.Error("expected not found error")
	}
}

func TestResolveUserBead_FoundBySlackID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1","email":"a@x.com"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	b, err := resolveUserBead(cmd, "U1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if b.ID != "gd-U1" {
		t.Errorf("got %q", b.ID)
	}
}

func TestResolveUserBead_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if _, err := resolveUserBead(cmd, "U1"); err == nil {
		t.Error("expected error")
	}
}

// ── getPreferencesMap ────────────────────────────────────────────────────

func beadWithPrefs(prefs string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{Fields: map[string]string{"preferences": prefs}}
}

func TestGetPreferencesMap(t *testing.T) {
	m, err := getPreferencesMap(beadWithPrefs(""))
	if err != nil || len(m) != 0 {
		t.Errorf("empty: %v / %v", m, err)
	}

	m, err = getPreferencesMap(beadWithPrefs("null"))
	if err != nil || len(m) != 0 {
		t.Errorf("null: %v / %v", m, err)
	}

	m, err = getPreferencesMap(beadWithPrefs(`{"timezone":"PST"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m["timezone"] != "PST" {
		t.Errorf("got %+v", m)
	}

	if _, err := getPreferencesMap(beadWithPrefs("{bad json")); err == nil {
		t.Error("expected parse error")
	}
}

// ── prefs get/set/list/delete ────────────────────────────────────────────

func newPrefsCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("user", "U1", "")
	return cmd
}

func TestPrefsGet_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1","preferences":"{\"tz\":\"UTC\"}"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsGetCmd.RunE(cmd, []string{"tz"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrefsGet_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsGetCmd.RunE(cmd, []string{"missing"}); err == nil {
		t.Error("expected not found")
	}
}

func TestPrefsList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsListCmd.RunE(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrefsList_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1","preferences":"{\"tz\":\"UTC\",\"lang\":\"en\"}"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsListCmd.RunE(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrefsSet_OK(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			patched = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
						"status": "open",
						"fields": json.RawMessage(`{"slack_id":"U1"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsSetCmd.RunE(cmd, []string{"tz", "UTC"}); err != nil {
		t.Errorf("err: %v", err)
	}
	if !patched {
		t.Error("expected patch")
	}
}

// TestPrefsSet_VibeModeStoresStringForm pins the cross-consumer contract for
// the gb-ux vibe_mode preference (epic ka-0000005059): `gb prefs set <user>
// vibe_mode true` must persist the STRING form {"vibe_mode":"true"}. That is
// the shape beads-viewer's flexBool reads on the settings page, and keeping
// the whole blob an all-strings object is what lets gb prefs / gb prime /
// quick_spawn continue decoding it into map[string]string.
func TestPrefsSet_VibeModeStoresStringForm(t *testing.T) {
	var patchFields []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			raw, _ := io.ReadAll(r.Body)
			var wrapper struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.Unmarshal(raw, &wrapper)
			patchFields = wrapper.Fields
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// UpdateBeadFieldsTyped reads the bead before patching.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
				"status": "open",
				"fields": json.RawMessage(`{"slack_id":"U1"}`),
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
						"status": "open",
						"fields": json.RawMessage(`{"slack_id":"U1"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsSetCmd.RunE(cmd, []string{"vibe_mode", "true"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if patchFields == nil {
		t.Fatal("expected a PATCH with a fields payload")
	}
	// The patched preferences field must carry the string form. The map[string]
	// string round-trip in getPreferencesMap guarantees the value is quoted.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patchFields, &fields); err != nil {
		t.Fatalf("decode fields: %v (fields=%s)", err, patchFields)
	}
	prefsRaw := string(fields["preferences"])
	if !strings.Contains(prefsRaw, `"vibe_mode":"true"`) {
		t.Errorf("gb prefs set vibe_mode true must store the string form, got %s", prefsRaw)
	}
	if strings.Contains(prefsRaw, `"vibe_mode":true`) {
		t.Errorf("gb prefs must not store a native JSON bool: %s", prefsRaw)
	}
}

// TestValidatePrefValue covers the typed validator that guards boolean keys.
// vibe_mode accepts any spelling strconv.ParseBool understands and normalizes
// to the canonical "true"/"false" string form; unparseable values are rejected;
// unknown keys pass through untouched.
func TestValidatePrefValue(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		want    string
		wantErr bool
	}{
		{"vibe true", "vibe_mode", "true", "true", false},
		{"vibe false", "vibe_mode", "false", "false", false},
		{"vibe 1 normalizes", "vibe_mode", "1", "true", false},
		{"vibe 0 normalizes", "vibe_mode", "0", "false", false},
		{"vibe uppercase normalizes", "vibe_mode", "TRUE", "true", false},
		{"vibe t normalizes", "vibe_mode", "t", "true", false},
		{"vibe yes rejected", "vibe_mode", "yes", "", true},
		{"vibe empty rejected", "vibe_mode", "", "", true},
		{"vibe garbage rejected", "vibe_mode", "maybe", "", true},
		// Unknown keys are never validated or normalized.
		{"other key passthrough", "timezone", "yes", "yes", false},
		{"other key arbitrary", "default_project", "anything", "anything", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validatePrefValue(tc.key, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validatePrefValue(%q, %q) = %q, want error", tc.key, tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatePrefValue(%q, %q) unexpected error: %v", tc.key, tc.value, err)
			}
			if got != tc.want {
				t.Errorf("validatePrefValue(%q, %q) = %q, want %q", tc.key, tc.value, got, tc.want)
			}
		})
	}
}

// TestPrefsSet_VibeModeRejectsNonBool asserts `gb prefs set u vibe_mode yes`
// errors and never PATCHes the bead — the invalid value must not be persisted
// (the viewer would silently coerce it to false, so rejecting up front avoids
// a confusing "I set it but it reads off" footgun).
func TestPrefsSet_VibeModeRejectsNonBool(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patched = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	err := prefsSetCmd.RunE(cmd, []string{"vibe_mode", "yes"})
	if err == nil {
		t.Fatal("expected error setting vibe_mode to a non-boolean")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention boolean, got %q", err.Error())
	}
	if patched {
		t.Error("invalid vibe_mode value must not be persisted")
	}
}

// TestPrefsSet_VibeModeFalseStoresStringForm completes the round-trip: setting
// vibe_mode false persists the string form "false" (not a native JSON bool,
// not omitted), and a normalizing spelling ("1") stores the canonical "true".
func TestPrefsSet_VibeModeFalseStoresStringForm(t *testing.T) {
	set := func(t *testing.T, value string) string {
		t.Helper()
		var patchFields []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
				raw, _ := io.ReadAll(r.Body)
				var wrapper struct {
					Fields json.RawMessage `json:"fields"`
				}
				_ = json.Unmarshal(raw, &wrapper)
				patchFields = wrapper.Fields
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1"}`),
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"beads": []map[string]any{
						{
							"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
							"status": "open",
							"fields": json.RawMessage(`{"slack_id":"U1"}`),
						},
					},
				})
			}
		}))
		defer srv.Close()
		swapDaemon(t, srv.URL)

		cmd := newPrefsCmd()
		cmd.SetContext(context.Background())
		if err := prefsSetCmd.RunE(cmd, []string{"vibe_mode", value}); err != nil {
			t.Fatalf("set vibe_mode %q: %v", value, err)
		}
		if patchFields == nil {
			t.Fatal("expected a PATCH with a fields payload")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(patchFields, &fields); err != nil {
			t.Fatalf("decode fields: %v (fields=%s)", err, patchFields)
		}
		return string(fields["preferences"])
	}

	falseRaw := set(t, "false")
	if !strings.Contains(falseRaw, `"vibe_mode":"false"`) {
		t.Errorf("set vibe_mode false must store the string form, got %s", falseRaw)
	}
	if strings.Contains(falseRaw, `"vibe_mode":false`) {
		t.Errorf("gb prefs must not store a native JSON bool: %s", falseRaw)
	}

	// A normalizing spelling still lands as the canonical "true".
	normRaw := set(t, "1")
	if !strings.Contains(normRaw, `"vibe_mode":"true"`) {
		t.Errorf("set vibe_mode 1 must normalize to \"true\", got %s", normRaw)
	}
}

// TestPrefsSet_UnknownKeyBypassesValidator confirms non-typed keys stay on the
// generic path: a value that would be rejected for vibe_mode is accepted and
// persisted verbatim for an arbitrary key.
func TestPrefsSet_UnknownKeyBypassesValidator(t *testing.T) {
	var patchFields []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			raw, _ := io.ReadAll(r.Body)
			var wrapper struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.Unmarshal(raw, &wrapper)
			patchFields = wrapper.Fields
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
				"status": "open",
				"fields": json.RawMessage(`{"slack_id":"U1"}`),
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
						"status": "open",
						"fields": json.RawMessage(`{"slack_id":"U1"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsSetCmd.RunE(cmd, []string{"notification_style", "yes"}); err != nil {
		t.Fatalf("unknown key should pass through: %v", err)
	}
	if !strings.Contains(string(patchFields), `"notification_style":"yes"`) {
		t.Errorf("unknown key value must persist verbatim, got %s", patchFields)
	}
}

func TestPrefsDelete_NotSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsDeleteCmd.RunE(cmd, []string{"tz"}); err == nil {
		t.Error("expected not set error")
	}
}

func TestPrefsDelete_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
						"status": "open",
						"fields": json.RawMessage(`{"slack_id":"U1","preferences":"{\"tz\":\"UTC\"}"}`),
					},
				},
			})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPrefsCmd()
	cmd.SetContext(context.Background())
	if err := prefsDeleteCmd.RunE(cmd, []string{"tz"}); err != nil {
		t.Errorf("err: %v", err)
	}
}
