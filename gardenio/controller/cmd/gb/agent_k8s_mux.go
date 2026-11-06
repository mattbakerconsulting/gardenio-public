package main

// agent_k8s_mux.go — coopmux registration and deregistration.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type muxClient struct {
	url       string
	authToken string
}

func newMuxClient() *muxClient {
	return &muxClient{
		url:       os.Getenv("COOP_MUX_URL"),
		authToken: envOr("COOP_MUX_AUTH_TOKEN", envOr("COOP_AUTH_TOKEN", os.Getenv("COOP_BROKER_TOKEN"))),
	}
}

// Register registers this agent session with coopmux. Logs a warning and
// returns nil on failure (registration is best-effort).
func (m *muxClient) Register(ctx context.Context, sessionID, coopURL, role, agent, project, pod, podIP string) error {
	if m.url == "" {
		return nil
	}

	// Wait for local coop to be healthy before registering.
	if err := m.waitForCoop(ctx, coopURL); err != nil {
		fmt.Printf("[gb agent start] WARNING: coop health check before mux registration: %v\n", err)
	}

	type metadata struct {
		Role    string         `json:"role"`
		Agent   string         `json:"agent"`
		Project string         `json:"project,omitempty"`
		K8s     map[string]any `json:"k8s"`
	}
	type payload struct {
		URL         string   `json:"url"`
		ID          string   `json:"id"`
		AuthToken   string   `json:"auth_token,omitempty"`
		AccountName string   `json:"account_name,omitempty"`
		Metadata    metadata `json:"metadata"`
	}

	p := payload{
		URL:         coopURL,
		ID:          sessionID,
		AccountName: os.Getenv("BOAT_ACCOUNT"),
		Metadata: metadata{
			Role:    role,
			Agent:   agent,
			Project: project,
			K8s:     map[string]any{"pod": pod, "ip": podIP},
		},
	}
	if m.authToken != "" {
		p.AuthToken = m.authToken
	}

	body, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url+"/api/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.authToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[gb agent start] WARNING: mux registration failed: %v\n", err)
		return nil
	}
	resp.Body.Close()

	fmt.Printf("[gb agent start] registered with mux as '%s'\n", sessionID)
	return nil
}

// Deregister removes this agent session from coopmux on exit.
func (m *muxClient) Deregister(sessionID string) {
	if m.url == "" || sessionID == "" {
		return
	}

	req, err := http.NewRequest(http.MethodDelete, m.url+"/api/v1/sessions/"+sessionID, nil)
	if err != nil {
		return
	}
	if m.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.authToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[gb agent start] WARNING: mux deregister failed: %v\n", err)
		return
	}
	resp.Body.Close()
	fmt.Printf("[gb agent start] deregistered from mux (%s)\n", sessionID)
}

// waitForCoop polls the coop health endpoint until it returns 200 or ctx expires.
func (m *muxClient) waitForCoop(ctx context.Context, coopURL string) error {
	healthURL := coopURL + "/api/v1/health"
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
		}
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for coop health at %s", healthURL)
}
