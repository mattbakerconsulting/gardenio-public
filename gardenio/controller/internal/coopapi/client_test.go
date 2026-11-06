package coopapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetAgentState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(AgentState{
			State:         "idle",
			ErrorCategory: "",
			LastMessage:   "hello",
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	state, err := c.GetAgentState(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.State != "idle" {
		t.Errorf("expected state=idle, got %q", state.State)
	}
	if state.LastMessage != "hello" {
		t.Errorf("expected last_message=hello, got %q", state.LastMessage)
	}
}

// TestGetAgentState_RequestCount pins the request_count wire field the
// rate-limit livelock bound keys on (ka-0000001209). It advances only on a
// usage-bearing successful turn, so the controller reads it to tell a genuine
// turn apart from a doomed 429 retry that merely flips coop to "working".
func TestGetAgentState_RequestCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"working","request_count":7}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	state, err := c.GetAgentState(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.RequestCount != 7 {
		t.Errorf("expected request_count=7, got %d", state.RequestCount)
	}
}

func TestGetAgentState_WithPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"state": "starting",
			"prompt": map[string]string{
				"type":    "setup",
				"subtype": "project",
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	state, err := c.GetAgentState(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Prompt == nil {
		t.Fatal("expected prompt to be non-nil")
	}
	if state.Prompt.Type != "setup" {
		t.Errorf("expected prompt type=setup, got %q", state.Prompt.Type)
	}
}

func TestGetAgentState_PlanPromptWithOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"state": "prompt",
			"prompt": map[string]any{
				"type":    "plan",
				"options": []string{"Approve bypass", "Approve auto", "Approve manual", "Feedback"},
				"ready":   true,
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	state, err := c.GetAgentState(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Prompt == nil {
		t.Fatal("expected prompt to be non-nil")
	}
	if !state.Prompt.IsPlan() {
		t.Errorf("expected IsPlan()=true, got false")
	}
	if len(state.Prompt.Options) != 4 {
		t.Errorf("expected 4 options, got %d", len(state.Prompt.Options))
	}
	if !state.Prompt.Ready {
		t.Error("expected ready=true")
	}
}

func TestSendKeys(t *testing.T) {
	var received struct {
		Keys []string `json:"keys"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/input/keys" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.SendKeys(context.Background(), []string{"Escape", "Return"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(received.Keys) != 2 || received.Keys[0] != "Escape" || received.Keys[1] != "Return" {
		t.Errorf("unexpected keys sent: %v", received.Keys)
	}
}

func TestRespond(t *testing.T) {
	var received struct {
		Option int `json:"option"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/respond" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Respond(context.Background(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received.Option != 2 {
		t.Errorf("expected option=2, got %d", received.Option)
	}
}

func TestRespondFull_PlanApprove(t *testing.T) {
	var received RespondRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/respond" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(RespondResponse{Delivered: true, PromptType: "plan"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	accept := true
	resp, err := c.RespondFull(context.Background(), RespondRequest{Accept: &accept})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Delivered {
		t.Error("expected delivered=true")
	}
	if resp.PromptType != "plan" {
		t.Errorf("expected prompt_type=plan, got %q", resp.PromptType)
	}
	if received.Accept == nil || !*received.Accept {
		t.Error("expected accept=true in request body")
	}
}

func TestRespondFull_PlanFeedback(t *testing.T) {
	var received RespondRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(RespondResponse{Delivered: true, PromptType: "plan"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	option := 4
	resp, err := c.RespondFull(context.Background(), RespondRequest{
		Option: &option,
		Text:   "Don't modify the schema",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Delivered {
		t.Error("expected delivered=true")
	}
	if received.Option == nil || *received.Option != 4 {
		t.Error("expected option=4 in request body")
	}
	if received.Text != "Don't modify the schema" {
		t.Errorf("expected text='Don't modify the schema', got %q", received.Text)
	}
}

func TestRespondPlan_ApproveOption(t *testing.T) {
	var received RespondRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(RespondResponse{Delivered: true, PromptType: "plan"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.RespondPlan(context.Background(), 2, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Delivered {
		t.Error("expected delivered=true")
	}
	if received.Option == nil || *received.Option != 2 {
		t.Errorf("expected option=2, got %v", received.Option)
	}
	if received.Text != "" {
		t.Errorf("expected empty text, got %q", received.Text)
	}
}

func TestRespondPlan_WithFeedback(t *testing.T) {
	var received RespondRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(RespondResponse{Delivered: true, PromptType: "plan"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.RespondPlan(context.Background(), 4, "Rename Foo to Bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Delivered {
		t.Error("expected delivered=true")
	}
	if received.Option == nil || *received.Option != 4 {
		t.Errorf("expected option=4, got %v", received.Option)
	}
	if received.Text != "Rename Foo to Bar" {
		t.Errorf("expected text='Rename Foo to Bar', got %q", received.Text)
	}
}

func TestAgentPrompt_TypeHelpers(t *testing.T) {
	plan := &AgentPrompt{Type: "plan"}
	if !plan.IsPlan() {
		t.Error("expected IsPlan()=true")
	}
	if plan.IsPermission() {
		t.Error("expected IsPermission()=false")
	}

	perm := &AgentPrompt{Type: "permission"}
	if !perm.IsPermission() {
		t.Error("expected IsPermission()=true")
	}

	question := &AgentPrompt{Type: "question"}
	if !question.IsQuestion() {
		t.Error("expected IsQuestion()=true")
	}
}

func TestNudge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/nudge" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["message"] != "do work" {
			t.Errorf("expected message='do work', got %q", req["message"])
		}
		json.NewEncoder(w).Encode(NudgeResponse{Delivered: true})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.Nudge(context.Background(), "do work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Delivered {
		t.Error("expected delivered=true")
	}
}

func TestSendInput(t *testing.T) {
	var received struct {
		Text  string `json:"text"`
		Enter bool   `json:"enter"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/input" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]int{"bytes_written": len(received.Text)})
	}))
	defer srv.Close()

	c := New(srv.URL)
	// enter=true must be forwarded so the text is SUBMITTED, not left sitting
	// in the input buffer (ka-0000006423).
	if err := c.SendInput(context.Background(), "continue where you left off", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received.Text != "continue where you left off" {
		t.Errorf("expected text='continue where you left off', got %q", received.Text)
	}
	if !received.Enter {
		t.Error("expected enter=true to be forwarded so the prompt is submitted")
	}
}

func TestShutdown(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shutdown" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected shutdown endpoint to be called")
	}
}

func TestGetScreenText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/screen/text" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte("Resume Session\n> Latest"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	text, err := c.GetScreenText(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Resume Session\n> Latest" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestGetAgentState_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetAgentState(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetAgentState_ConnectionRefused(t *testing.T) {
	c := New("http://localhost:1") // nothing listening
	_, err := c.GetAgentState(context.Background())
	if err == nil {
		t.Fatal("expected connection error")
	}
}
