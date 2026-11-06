package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// mockEscalationClient implements EscalationAPIClient for testing.
type mockEscalationClient struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (m *mockEscalationClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *mockEscalationClient) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if m.err != nil {
		return nil, m.err
	}
	for _, b := range m.beads {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

// mockUserResolver implements UserResolver for testing.
type mockUserResolver struct {
	user *UserIdentity
}

func (m *mockUserResolver) LookupByEmail(_ context.Context, email string) *UserIdentity {
	if m.user != nil && m.user.Email == email {
		return m.user
	}
	return nil
}

func TestEscalationAPI_List(t *testing.T) {
	client := &mockEscalationClient{
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "gd-esc1",
				Title:    "Create IAM role",
				Type:     "escalation",
				Status:   "open",
				Priority: 1,
				Fields: map[string]string{
					"operation_type": "aws_iam",
					"operation_spec": `{"action":"create_role"}`,
				},
			},
		},
	}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Escalations []*beadsapi.BeadDetail `json:"escalations"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Escalations) != 1 {
		t.Fatalf("got %d escalations, want 1", len(resp.Escalations))
	}
	if resp.Escalations[0].ID != "gd-esc1" {
		t.Errorf("id=%q, want gd-esc1", resp.Escalations[0].ID)
	}
}

func TestEscalationAPI_Show(t *testing.T) {
	client := &mockEscalationClient{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-esc2", Type: "escalation", Status: "open"},
		},
	}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations/gd-esc2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestEscalationAPI_Show_WrongType(t *testing.T) {
	client := &mockEscalationClient{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-task1", Type: "task", Status: "open"},
		},
	}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations/gd-task1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for non-escalation bead", w.Code)
	}
}

func TestEscalationAPI_Me_Authenticated(t *testing.T) {
	user := &UserIdentity{
		BeadID:       "gd-user1",
		Email:        "alice@example.com",
		DisplayName:  "Alice",
		Capabilities: []string{"approve:escalation"},
	}
	api := NewEscalationAPI(&mockEscalationClient{}, &mockUserResolver{user: user}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations/me", nil)
	req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), "alice@example.com"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["bead_id"] != "gd-user1" {
		t.Errorf("bead_id=%v, want gd-user1", resp["bead_id"])
	}
}

func TestEscalationAPI_Me_NoUser(t *testing.T) {
	api := NewEscalationAPI(&mockEscalationClient{}, &mockUserResolver{}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations/me", nil)
	req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), "unknown@example.com"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// A signed-in caller cannot read someone else's user bead by setting
// X-Auth-Request-Email: /me answers for the verified identity only
// (gd-Fhizmbk9Ee).
func TestEscalationAPI_Me_IgnoresForgedEmailHeader(t *testing.T) {
	alice := &UserIdentity{BeadID: "gd-alice", Email: "alice@example.com", Capabilities: []string{"approve:escalation"}}
	mux := http.NewServeMux()
	NewEscalationAPI(&mockEscalationClient{}, &mockUserResolver{user: alice}, slog.Default()).RegisterRoutes(mux)

	t.Run("header alone", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/escalations/me", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want 401; body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("header over a verified caller", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/escalations/me", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), "mallory@example.com"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "gd-alice") {
			t.Fatalf("status=%d body=%s, want 403 for mallory and no alice bead", w.Code, w.Body.String())
		}
	})
}

// P7 Task 4: the bridge no longer approves or rejects escalations by PATCHing
// approved_by; Slack buttons and the beads viewer are the approval surfaces.
func TestEscalationAPI_ApproveRejectGone(t *testing.T) {
	a := NewEscalationAPI(nil, nil, slog.Default())
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	for _, action := range []string{"approve", "reject"} {
		req := httptest.NewRequest(http.MethodPost, "/api/escalations/gd-x/"+action, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("%s: %d, want 410", action, rec.Code)
		}
	}
}

func TestEscalationAPI_SlackRef(t *testing.T) {
	client := &mockEscalationClient{}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	api.SetSlackRefLookup(func(id string) (MessageRef, bool) {
		if id == "gd-esc-known" {
			return MessageRef{ChannelID: "C123", Timestamp: "1234.5678", ThreadTS: "1234.0000"}, true
		}
		return MessageRef{}, false
	})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Known escalation — returns the stored ref.
	req := httptest.NewRequest(http.MethodGet, "/api/escalations/gd-esc-known/slack-ref", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("known: status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["channel"] != "C123" || got["ts"] != "1234.5678" || got["thread_ts"] != "1234.0000" {
		t.Errorf("ref=%+v, want channel=C123 ts=1234.5678 thread_ts=1234.0000", got)
	}

	// Unknown escalation — 404.
	req = httptest.NewRequest(http.MethodGet, "/api/escalations/gd-esc-unknown/slack-ref", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown: status=%d, want 404", w.Code)
	}
}

func TestEscalationAPI_SlackRef_NotConfigured(t *testing.T) {
	client := &mockEscalationClient{}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	// No SetSlackRefLookup call — lookup is nil.
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/escalations/gd-esc/slack-ref", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", w.Code)
	}
}

func TestEscalationAPI_MethodNotAllowed(t *testing.T) {
	client := &mockEscalationClient{}
	api := NewEscalationAPI(client, &mockUserResolver{}, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/escalations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", w.Code)
	}
}
