package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
)

// TestHandleThawStatus covers the re-nudge mid-THAW status endpoint
// (ka-0000005830, design ka-0000005125):
//   - nil lifecycle (flag off) → 200 {"thawing":false} (routing-unchanged no-op)
//   - missing ?agent= → 400
//   - lifecycle present, no thaw in flight → 200 {"thawing":false}
func TestHandleThawStatus(t *testing.T) {
	t.Run("nil lifecycle reports thawing=false", func(t *testing.T) {
		h := handleThawStatus(nil, discardLogger())
		rr := doThawStatus(t, h, "agentsim")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if decodeThawing(t, rr) {
			t.Errorf("thawing = true, want false on nil lifecycle (flag off must be a no-op)")
		}
	})

	t.Run("missing agent param returns 400", func(t *testing.T) {
		h := handleThawStatus(nil, discardLogger())
		rr := doThawStatus(t, h, "")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 when ?agent= missing", rr.Code)
		}
	})

	t.Run("lifecycle present, no thaw in flight reports thawing=false", func(t *testing.T) {
		cfg := &config.Config{FreezeThawEnabled: true}
		ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
		h := handleThawStatus(ft, discardLogger())
		rr := doThawStatus(t, h, "agentsim")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if decodeThawing(t, rr) {
			t.Errorf("thawing = true, want false (no thaw registered for agent)")
		}
	})

	t.Run("response echoes the agent name", func(t *testing.T) {
		h := handleThawStatus(nil, discardLogger())
		rr := doThawStatus(t, h, "my-agent-42")
		var resp struct {
			Agent   string `json:"agent"`
			Thawing bool   `json:"thawing"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (body=%q)", err, rr.Body.String())
		}
		if resp.Agent != "my-agent-42" {
			t.Errorf("agent = %q, want my-agent-42", resp.Agent)
		}
	})
}

func doThawStatus(t *testing.T, h http.HandlerFunc, agent string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/thaw/status"
	if agent != "" {
		target += "?agent=" + agent
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func decodeThawing(t *testing.T, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	var resp struct {
		Thawing bool `json:"thawing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rr.Body.String())
	}
	return resp.Thawing
}
