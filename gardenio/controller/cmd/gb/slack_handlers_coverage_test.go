package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── deleteSlackMessage ────────────────────────────────────────────────────

func TestDeleteSlackMessage_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/messages/delete" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(slackDeleteResponse{
			OK: true, Channel: "C1", Timestamp: "123.456",
		})
	}))
	defer srv.Close()

	result, err := deleteSlackMessage(srv.URL, "C1", "123.456")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !result.OK {
		t.Errorf("expected OK")
	}
}

func TestDeleteSlackMessage_NotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(slackDeleteResponse{OK: false, Error: "no_perm"})
	}))
	defer srv.Close()

	result, err := deleteSlackMessage(srv.URL, "C1", "1")
	if err == nil {
		t.Fatal("expected err")
	}
	if result == nil || result.Error != "no_perm" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestDeleteSlackMessage_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, err := deleteSlackMessage(srv.URL, "C1", "1"); err == nil {
		t.Error("expected decode error")
	}
}

func TestDeleteSlackMessage_ConnRefused(t *testing.T) {
	if _, err := deleteSlackMessage("http://127.0.0.1:1", "C1", "1"); err == nil {
		t.Error("expected connection error")
	}
}

// ── runSlackChannel ───────────────────────────────────────────────────────

func TestRunSlackChannel_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackChannel(cmd, []string{"general"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackChannel_ByID_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/slack/channels/history") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]slackChannelMessage{
			{Author: "alice", Text: "hi"},
		})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackChannel(cmd, []string{"C12345"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackChannel_ByName_ResolveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackChannel(cmd, []string{"general"}); err == nil {
		t.Error("expected resolve error")
	}
}

func TestRunSlackChannel_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackChannel(cmd, []string{"C1"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackChannel_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelMessage{{Author: "a", Text: "x"}})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackChannel(cmd, []string{"C1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── runSlackSearch ────────────────────────────────────────────────────────

func TestRunSlackSearch_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 20, "")
	cmd.Flags().IntP("context", "C", 2, "")
	cmd.Flags().Int("scan", 200, "")
	cmd.Flags().Int("max-chars", 12000, "")

	if err := runSlackSearch(cmd, []string{"general", "hi"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackSearch_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackSearchResult{
			{Message: slackChannelMessage{Author: "alice", Text: "hit"}},
		})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 20, "")
	cmd.Flags().IntP("context", "C", 2, "")
	cmd.Flags().Int("scan", 200, "")
	cmd.Flags().Int("max-chars", 12000, "")

	if err := runSlackSearch(cmd, []string{"C1", "hit"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackSearch_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackSearchResult{})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 20, "")
	cmd.Flags().IntP("context", "C", 2, "")
	cmd.Flags().Int("scan", 200, "")
	cmd.Flags().Int("max-chars", 12000, "")

	if err := runSlackSearch(cmd, []string{"C1", "nomatch"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackSearch_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 20, "")
	cmd.Flags().IntP("context", "C", 2, "")
	cmd.Flags().Int("scan", 200, "")
	cmd.Flags().Int("max-chars", 12000, "")

	if err := runSlackSearch(cmd, []string{"C1", "x"}); err == nil {
		t.Error("expected error")
	}
}

// ── runSlackReadThread ────────────────────────────────────────────────────

func TestRunSlackReadThread_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 100, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackReadThread(cmd, []string{"general", "1.2"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackReadThread_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelMessage{
			{Author: "alice", Text: "reply"},
		})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 100, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackReadThread(cmd, []string{"C1", "1.2"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackReadThread_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 100, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	if err := runSlackReadThread(cmd, []string{"C1", "1.2"}); err == nil {
		t.Error("expected error")
	}
}

// ── runSlackDelete ────────────────────────────────────────────────────────

func TestRunSlackDelete_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	if err := runSlackDelete(cmd, []string{"general", "1.2"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackDelete_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/messages/delete" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(slackDeleteResponse{OK: true})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	if err := runSlackDelete(cmd, []string{"C1", "1.2"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackDelete_NotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(slackDeleteResponse{OK: false, Error: "nope"})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	// Underlying deleteSlackMessage returns an error when OK=false, so this bubbles up.
	if err := runSlackDelete(cmd, []string{"C1", "1.2"}); err == nil {
		t.Error("expected error from NotOK response")
	}
}

// ── runSlackDeleteBotMsgs ────────────────────────────────────────────────

func TestRunSlackDeleteBotMsgs_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Int("scan", 200, "")

	if err := runSlackDeleteBotMsgs(cmd, []string{"general"}); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackDeleteBotMsgs_DryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/slack/channels/history") {
			_ = json.NewEncoder(w).Encode([]slackChannelMessage{
				{Author: "bot-a", Text: "msg", IsBot: true, Timestamp: "1.1"},
				{Author: "human", Text: "ignored"},
			})
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("dry-run", true, "")
	cmd.Flags().Int("scan", 200, "")

	if err := runSlackDeleteBotMsgs(cmd, []string{"C1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackDeleteBotMsgs_NoBotMsgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/slack/channels/history") {
			_ = json.NewEncoder(w).Encode([]slackChannelMessage{
				{Author: "human", Text: "hi"},
			})
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Int("scan", 200, "")

	if err := runSlackDeleteBotMsgs(cmd, []string{"C1"}); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunSlackDeleteBotMsgs_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("dry-run", true, "")
	cmd.Flags().Int("scan", 200, "")

	if err := runSlackDeleteBotMsgs(cmd, []string{"C1"}); err == nil {
		t.Error("expected error")
	}
}
