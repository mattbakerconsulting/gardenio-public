package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// resetBridgeAgent forgets the process's cached bridge agent identity, now
// and after the test.
func resetBridgeAgent(t *testing.T) {
	t.Helper()
	reset := func() {
		bridgeAgentOnce = sync.Once{}
		bridgeAgentName, bridgeAgentTok = "", ""
	}
	reset()
	t.Cleanup(reset)
}

// useMeDaemon points gb's daemon client, on token tok, at a daemon whose GET
// /v1/me answers me.
func useMeDaemon(t *testing.T, tok string, me beadsapi.MeResponse) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" || r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(me)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	resetBridgeAgent(t)
}

// agentTokenBridge records each call's agent token header, and the agent its
// body or query names.
type agentTokenBridge struct {
	mu            sync.Mutex
	tokens, names []string
}

func (b *agentTokenBridge) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("agent")
		if r.Method == http.MethodPost {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			name = body["agent"]
		}
		b.mu.Lock()
		b.tokens = append(b.tokens, r.Header.Get(bridge.AgentTokenHeader))
		b.names = append(b.names, name)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversation/say":
			_, _ = io.WriteString(w, `{"ok":true,"id":"gd-SAY1"}`)
		case "/api/conversation/thread":
			_, _ = io.WriteString(w, `{"thread":"gd-HEAD1","messages":[]}`)
		case "/api/slack/threads":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// gd-P5wjHDweih: on its own per-agent token gb sends the token to the bridge's
// conversation routes and names the token's agent (the bridge refuses any
// other), whatever its actor says.
func TestBridgeAgentToken_SentForAnAgentToken(t *testing.T) {
	b := &agentTokenBridge{}
	bridgeURL := b.serve(t)
	setSayEnv(t, bridgeURL)
	actor = "git-user-name"
	useMeDaemon(t, "agent-tok", beadsapi.MeResponse{Agent: "steward", AgentBead: "gd-steward1", TokenProject: "acre"})
	resetSayFlags(t)

	sayThread = "gd-HEAD1"
	captureStdout(t, func() error { return runSay(sayCmd, []string{"hello"}) })
	captureStdout(t, func() error { return runThread(threadCmd, []string{"gd-HEAD1"}) })
	if msgs := fetchThreadMessages(bridgeURL, "C999", "111.222", 0); msgs == nil {
		t.Fatal("legacy thread read failed")
	}

	if len(b.tokens) != 3 {
		t.Fatalf("bridge calls = %d (%v), want 3", len(b.tokens), b.names)
	}
	for i := range b.tokens {
		if b.tokens[i] != "agent-tok" || b.names[i] != "steward" {
			t.Errorf("call %d: token %q agent %q, want agent-tok as steward", i, b.tokens[i], b.names[i])
		}
	}
}

// Any other credential is never handed to the bridge: gb names its actor and
// sends no token.
func TestBridgeAgentToken_NotSentForAnotherToken(t *testing.T) {
	b := &agentTokenBridge{}
	setSayEnv(t, b.serve(t))
	useMeDaemon(t, "static-tok", beadsapi.MeResponse{OperatorID: "crew-say", Source: "token"})
	resetSayFlags(t)

	sayThread = "gd-HEAD1"
	captureStdout(t, func() error { return runSay(sayCmd, []string{"hello"}) })
	captureStdout(t, func() error { return runThread(threadCmd, []string{"gd-HEAD1"}) })

	if len(b.tokens) != 2 {
		t.Fatalf("bridge calls = %d, want 2", len(b.tokens))
	}
	for i := range b.tokens {
		if b.tokens[i] != "" || b.names[i] != "crew-say" {
			t.Errorf("call %d: token %q agent %q, want no token, as crew-say", i, b.tokens[i], b.names[i])
		}
	}
}
