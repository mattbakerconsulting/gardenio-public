package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── runInbox ──────────────────────────────────────────────────────────────

func newInboxCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 20, "")
	return cmd
}

func TestRunInbox_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := newInboxCmd()
	cmd.SetContext(context.Background())
	if err := runInbox(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunInbox_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-M1", "title": "hello", "type": "mail",
					"status": "open", "assignee": "tester",
					"labels": []string{"from:alice"},
				},
			},
			"total": 1,
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := newInboxCmd()
	cmd.SetContext(context.Background())
	if err := runInbox(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunInbox_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{"id": "gd-M1", "title": "x", "type": "mail", "labels": []string{"from:a"}},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	t.Setenv("KD_ACTOR", "tester")
	cmd := newInboxCmd()
	cmd.SetContext(context.Background())
	if err := runInbox(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunInbox_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := newInboxCmd()
	cmd.SetContext(context.Background())
	if err := runInbox(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── mail list ────────────────────────────────────────────────────────────

func TestMailList_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{"id": "gd-M1", "title": "m", "type": "mail", "labels": []string{"from:b"}},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := &cobra.Command{}
	cmd.Flags().StringSlice("status", []string{}, "")
	cmd.Flags().Int("limit", 20, "")
	cmd.SetContext(context.Background())
	if err := mailListCmd.RunE(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestMailList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := &cobra.Command{}
	cmd.Flags().StringSlice("status", []string{"closed"}, "")
	cmd.Flags().Int("limit", 20, "")
	cmd.SetContext(context.Background())
	if err := mailListCmd.RunE(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestMailList_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")
	cmd := &cobra.Command{}
	cmd.Flags().StringSlice("status", []string{}, "")
	cmd.Flags().Int("limit", 20, "")
	cmd.SetContext(context.Background())
	if err := mailListCmd.RunE(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── mail read ────────────────────────────────────────────────────────────

func TestMailRead_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/beads/gd-M1") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-M1", "title": "hi", "type": "mail",
				"description": "body", "assignee": "tester",
				"labels": []string{"from:alice"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := mailReadCmd.RunE(cmd, []string{"gd-M1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestMailRead_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-M1", "title": "hi", "type": "mail",
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := mailReadCmd.RunE(cmd, []string{"gd-M1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestMailRead_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := mailReadCmd.RunE(cmd, []string{"gd-M1"}); err == nil {
		t.Error("expected error")
	}
}

// ── mail send ────────────────────────────────────────────────────────────

func newMailSendCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("subject", "s", "", "")
	cmd.Flags().String("body", "", "")
	cmd.Flags().Int("priority", 2, "")
	cmd.Flags().Bool("interrupt", false, "")
	cmd.Flags().StringArray("label", nil, "")
	return cmd
}

func TestMailSend_MissingSubject(t *testing.T) {
	cmd := newMailSendCmd()
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err == nil {
		t.Error("expected error for missing subject")
	}
}

func TestMailSend_BadPriority(t *testing.T) {
	cmd := newMailSendCmd()
	_ = cmd.Flags().Set("subject", "hi")
	_ = cmd.Flags().Set("priority", "99")
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err == nil {
		t.Error("expected error for bad priority")
	}
}

func TestMailSend_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/beads") {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-M9"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")

	cmd := newMailSendCmd()
	_ = cmd.Flags().Set("subject", "hi")
	_ = cmd.Flags().Set("body", "body")
	_ = cmd.Flags().Set("interrupt", "true")
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestMailSend_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_ACTOR", "tester")

	cmd := newMailSendCmd()
	_ = cmd.Flags().Set("subject", "hi")
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err == nil {
		t.Error("expected error")
	}
}

func TestMailSendLabelFlag(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/beads") {
			var req struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			got = req.Labels
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-M10"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	t.Setenv("KD_ACTOR", "x")

	cmd := newMailSendCmd()
	_ = cmd.Flags().Set("subject", "Stop")
	_ = cmd.Flags().Set("label", "control:kill")
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := []string{"from:x", "control:kill"}; !slices.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

func TestMailSendLabelFlag_UnknownControlVerb(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-M11"})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	t.Setenv("KD_ACTOR", "x")

	cmd := newMailSendCmd()
	_ = cmd.Flags().Set("subject", "Reboot")
	_ = cmd.Flags().Set("label", "control:reboot")
	cmd.SetContext(context.Background())
	if err := mailSendCmd.RunE(cmd, []string{"alice"}); err == nil {
		t.Fatal("expected error for unknown control verb")
	}
	if posted {
		t.Fatal("mail must not be created when --label is refused")
	}
}
