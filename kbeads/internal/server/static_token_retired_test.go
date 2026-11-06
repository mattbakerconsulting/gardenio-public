package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/events"
)

// gd-ke6PC7bRto: the daemon accepts no shared static bearer. A value that is
// not a registered fleet token (or a verifiable JWT) is 401 on HTTP and
// Unauthenticated on gRPC, whatever BEADS_AUTH_TOKEN says, and X-Actor /
// X-Fleet change nothing for the fleet tokens that remain.
func TestRetiredStaticToken_RefusedOnHTTPAndGRPC(t *testing.T) {
	const (
		oldStatic = "old-shared-static-token"
		fleetTok  = "registered-fleet-token"
	)
	// The daemon no longer reads these; set them to prove it.
	t.Setenv("BEADS_AUTH_TOKEN", oldStatic)
	t.Setenv("BEADS_PREVIOUS_TOKEN", oldStatic+"-prev")

	s := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull(fleetTok, "qa-fleet", "", nil, nil, time.Time{}, "qa", false)
	s.SetFleetAuth(fc)
	h := s.NewHTTPHandler(nil, fc)

	get := func(bearer string, headers map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, bearer := range []string{oldStatic, oldStatic + "-prev", `{"token":"` + oldStatic + `"}`} {
		if code := get(bearer, map[string]string{"X-Actor": "gardenio-crew-x", "X-Fleet": "qa-fleet"}); code != http.StatusUnauthorized {
			t.Errorf("HTTP with %q: %d, want 401", bearer, code)
		}
	}
	if code := get(fleetTok, nil); code != http.StatusOK {
		t.Errorf("HTTP with a registered fleet token: %d, want 200", code)
	}
	// A JSON-wrapped form of a registered token still matches its raw form.
	if code := get(`{"token":"`+fleetTok+`"}`, nil); code != http.StatusOK {
		t.Errorf("HTTP with a JSON-wrapped fleet token: %d, want 200", code)
	}

	interceptor := AuthInterceptor(fc, nil)
	call := func(bearer string, extra ...string) (AuthIdentity, error) {
		md := metadata.Pairs(append([]string{"authorization", "Bearer " + bearer}, extra...)...)
		var got AuthIdentity
		_, err := interceptor(metadata.NewIncomingContext(context.Background(), md), nil,
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/GetBead"},
			func(ctx context.Context, _ any) (any, error) {
				got = AuthIdentityFromContext(ctx)
				return nil, nil
			})
		return got, err
	}
	if _, err := call(oldStatic); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC with the old static token: %v, want Unauthenticated", err)
	}
	id, err := call(fleetTok, "x-actor", "gardenio-crew-x", "x-fleet", "other-fleet")
	if err != nil {
		t.Fatalf("gRPC with a registered fleet token: %v", err)
	}
	if id.Type != "fleet" || id.Subject != "qa" {
		t.Errorf("gRPC identity = %+v, want fleet/qa (X-Actor ignored)", id)
	}
}
