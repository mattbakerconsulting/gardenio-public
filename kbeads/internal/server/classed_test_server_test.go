package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
)

const (
	testStaticToken     = "static-tok"
	testAgentToken      = "agent-tok-a"
	testAgentToken2     = "agent-tok-b"
	testBridgeToken     = "bridge-tok"
	testOtherFleetToken = "other-fleet-tok"
)

// newClassedTestServer wires one server with every caller class the P7 tasks
// distinguish: the primary static token, two per-agent fleet tokens, a bridge
// fleet token, an unlisted fleet token, and bearerSubjectVerifier so any other
// bearer authenticates as an OIDC admin whose email is the bearer value.
func newClassedTestServer(t *testing.T) (*BeadsServer, *mockStore, http.Handler) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull(testAgentToken, "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	fc.RegisterTokenFull(testAgentToken2, "agent-gd-a2", "gardenio", nil, nil, time.Time{}, "crew-b", false)
	fc.RegisterTokenFull(testBridgeToken, "slack-bridge", "", nil, nil, time.Time{}, "", false)
	fc.RegisterTokenFull(testOtherFleetToken, "qa-fleet", "", nil, nil, time.Time{}, "", false)
	s.SetFleetAuth(fc)
	return s, ms, s.NewHTTPHandler(&bearerSubjectVerifier{}, withTestToken(fc, testStaticToken))
}

// doBearerJSON sends body as JSON with Authorization: Bearer <bearer> and any
// extra headers.
func doBearerJSON(t *testing.T, h http.Handler, method, path, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
