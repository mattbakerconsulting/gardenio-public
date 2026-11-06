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
)

// linkOperatorStub builds an httptest server that mocks /v1/whoami,
// /v1/beads, and /v1/operators/me/aliases for identity link-operator tests.
type linkOperatorStub struct {
	whoamiSource     string
	whoamiOperator   string
	userBeads        []map[string]any
	linkResp         beadsapi.LinkOperatorAliasResponse
	linkStatus       int
	gotLinkRequest   beadsapi.LinkOperatorAliasRequest
	linkRequestCount int
}

func (s *linkOperatorStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"operator_id": s.whoamiOperator,
			"source":      s.whoamiSource,
		})
	})
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		beads := make([]map[string]any, 0, len(s.userBeads))
		for _, b := range s.userBeads {
			beads = append(beads, map[string]any{
				"id":     b["id"],
				"title":  b["title"],
				"type":   "user",
				"kind":   "config",
				"fields": b["fields"],
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": beads,
			"total": len(beads),
		})
	})
	mux.HandleFunc("/v1/operators/me/aliases", func(w http.ResponseWriter, r *http.Request) {
		s.linkRequestCount++
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &s.gotLinkRequest)
		if s.linkStatus != 0 && s.linkStatus != http.StatusOK {
			w.WriteHeader(s.linkStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(s.linkResp)
	})
	return mux
}

func setupLinkOperatorDaemon(t *testing.T, stub *linkOperatorStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

func resetLinkOperatorFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = identityLinkOperatorCmd.Flags().Set("slack-id", "")
		_ = identityLinkOperatorCmd.Flags().Set("email", "")
	})
}

func TestLookupSlackIDForEmail_FindsMatch(t *testing.T) {
	stub := &linkOperatorStub{
		userBeads: []map[string]any{
			{"id": "gd-user1", "fields": map[string]string{"email": "alice@example.com", "slack_id": "U_ALICE"}},
			{"id": "gd-user2", "fields": map[string]string{"email": "bob@example.com", "slack_id": "U_BOB"}},
		},
	}
	setupLinkOperatorDaemon(t, stub)

	slackID, err := lookupSlackIDForEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slackID != "U_ALICE" {
		t.Errorf("slackID = %q, want U_ALICE", slackID)
	}
}

func TestLookupSlackIDForEmail_CaseInsensitive(t *testing.T) {
	stub := &linkOperatorStub{
		userBeads: []map[string]any{
			{"id": "gd-user1", "fields": map[string]string{"email": "Alice@Example.com", "slack_id": "U_ALICE"}},
		},
	}
	setupLinkOperatorDaemon(t, stub)

	slackID, err := lookupSlackIDForEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slackID != "U_ALICE" {
		t.Errorf("slackID = %q, want U_ALICE", slackID)
	}
}

func TestLookupSlackIDForEmail_NoMatch(t *testing.T) {
	stub := &linkOperatorStub{
		userBeads: []map[string]any{
			{"id": "gd-user1", "fields": map[string]string{"email": "bob@example.com", "slack_id": "U_BOB"}},
		},
	}
	setupLinkOperatorDaemon(t, stub)

	_, err := lookupSlackIDForEmail(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("expected error for unknown email")
	}
	if !strings.Contains(err.Error(), "no user identity bead found") {
		t.Errorf("error = %q, want 'no user identity bead found'", err.Error())
	}
}

func TestLookupSlackIDForEmail_MatchHasNoSlackID(t *testing.T) {
	stub := &linkOperatorStub{
		userBeads: []map[string]any{
			{"id": "gd-user1", "fields": map[string]string{"email": "alice@example.com", "slack_id": ""}},
		},
	}
	setupLinkOperatorDaemon(t, stub)

	_, err := lookupSlackIDForEmail(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("expected error when matching bead has no slack_id")
	}
	if !strings.Contains(err.Error(), "no slack_id") {
		t.Errorf("error = %q, want 'no slack_id'", err.Error())
	}
}

func TestLinkOperatorCmd_HappyPath(t *testing.T) {
	stub := &linkOperatorStub{
		whoamiSource:   "auth",
		whoamiOperator: "alice@example.com",
		userBeads: []map[string]any{
			{"id": "gd-user1", "fields": map[string]string{"email": "alice@example.com", "slack_id": "U_ALICE"}},
		},
		linkResp: beadsapi.LinkOperatorAliasResponse{
			OperatorID:    99,
			Kind:          "slack",
			Subject:       "U_ALICE",
			Merged:        true,
			MergedFromID:  17,
			RowsRepointed: 5,
		},
	}
	setupLinkOperatorDaemon(t, stub)
	resetLinkOperatorFlags(t)

	cmd := identityLinkOperatorCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if stub.linkRequestCount != 1 {
		t.Errorf("expected 1 link request, got %d", stub.linkRequestCount)
	}
	if stub.gotLinkRequest.Kind != "slack" {
		t.Errorf("Kind = %q, want slack", stub.gotLinkRequest.Kind)
	}
	if stub.gotLinkRequest.Subject != "U_ALICE" {
		t.Errorf("Subject = %q, want U_ALICE", stub.gotLinkRequest.Subject)
	}
	if stub.gotLinkRequest.Source != "identity-link" {
		t.Errorf("Source = %q, want identity-link", stub.gotLinkRequest.Source)
	}
}

func TestLinkOperatorCmd_RequiresAuthSource(t *testing.T) {
	stub := &linkOperatorStub{
		whoamiSource:   "anonymous",
		whoamiOperator: "",
	}
	setupLinkOperatorDaemon(t, stub)
	resetLinkOperatorFlags(t)

	cmd := identityLinkOperatorCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected error when whoami source != auth")
	}
	if !strings.Contains(err.Error(), "OIDC authentication") {
		t.Errorf("error = %q, want 'OIDC authentication'", err.Error())
	}
	if stub.linkRequestCount != 0 {
		t.Errorf("expected 0 link requests, got %d", stub.linkRequestCount)
	}
}

func TestLinkOperatorCmd_SlackIDFlagSkipsBeadLookup(t *testing.T) {
	stub := &linkOperatorStub{
		whoamiSource:   "auth",
		whoamiOperator: "alice@example.com",
		// No user beads — flag must allow bypassing the lookup.
		linkResp: beadsapi.LinkOperatorAliasResponse{OperatorID: 99, Kind: "slack", Subject: "U_OVERRIDE"},
	}
	setupLinkOperatorDaemon(t, stub)
	resetLinkOperatorFlags(t)

	if err := identityLinkOperatorCmd.Flags().Set("slack-id", "U_OVERRIDE"); err != nil {
		t.Fatal(err)
	}
	cmd := identityLinkOperatorCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if stub.gotLinkRequest.Subject != "U_OVERRIDE" {
		t.Errorf("Subject = %q, want U_OVERRIDE", stub.gotLinkRequest.Subject)
	}
}
