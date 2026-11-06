package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Commons III step 5: read_cursors on the caller's own user bead, at most one
// write per interval per person.

func readCursorsRequest(method, email string, body any) *http.Request {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/v1/users/me/read-cursors", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	// A land member: scoped to one land by its group, no global role.
	ctx = WithRoles(ctx, []string{RoleProjectMember})
	ctx = WithGroups(ctx, []string{"/project-alpha"})
	return req.WithContext(ctx)
}

func setCursors(cursors map[string]string) map[string]any {
	return map[string]any{"read_cursors": cursors}
}

func TestReadCursors_WritesOwnBeadThenDebounces(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com", "status_message": "hi"})
	seedLinkUserBead(ms, "u-bob", "auto-provision", map[string]any{"email": "bob@example.com"})
	bobBefore := string(ms.beads["u-bob"].Fields)

	// The first position goes to the bead now.
	rec := httptest.NewRecorder()
	s.handleSetReadCursors(rec, readCursorsRequest(http.MethodPost, "alice@example.com", setCursors(map[string]string{"gd-ch1": "gd-m1"})))
	requireStatus(t, rec, http.StatusOK)
	var resp ReadCursorsResponse
	decodeJSON(t, rec, &resp)
	if !resp.Written || resp.UserBeadID != "u-alice" || resp.ReadCursors["gd-ch1"] != "gd-m1" {
		t.Fatalf("first set = %+v, want written on u-alice", resp)
	}
	if got := storedReadCursors(ms.beads["u-alice"]); got["gd-ch1"] != "gd-m1" {
		t.Fatalf("stored = %v", got)
	}
	if f := userBeadFields(ms.beads["u-alice"]); f["status_message"] != "hi" || f["email"] != "alice@example.com" {
		t.Fatalf("other fields changed: %v", f)
	}

	// A second inside the interval waits, and GET already shows it.
	rec = httptest.NewRecorder()
	s.handleSetReadCursors(rec, readCursorsRequest(http.MethodPost, "alice@example.com", setCursors(map[string]string{"gd-ch1": "gd-m2", "gd-ch2": "gd-m9"})))
	requireStatus(t, rec, http.StatusOK)
	resp = ReadCursorsResponse{}
	decodeJSON(t, rec, &resp)
	if resp.Written {
		t.Fatalf("second set inside the interval was written now")
	}
	if got := storedReadCursors(ms.beads["u-alice"]); got["gd-ch1"] != "gd-m1" || got["gd-ch2"] != "" {
		t.Fatalf("stored moved inside the interval: %v", got)
	}
	rec = httptest.NewRecorder()
	s.handleGetReadCursors(rec, readCursorsRequest(http.MethodGet, "alice@example.com", nil))
	requireStatus(t, rec, http.StatusOK)
	resp = ReadCursorsResponse{}
	decodeJSON(t, rec, &resp)
	if resp.ReadCursors["gd-ch1"] != "gd-m2" || resp.ReadCursors["gd-ch2"] != "gd-m9" {
		t.Fatalf("GET = %v, want the waiting positions laid over the stored ones", resp.ReadCursors)
	}
	if after := string(ms.beads["u-bob"].Fields); after != bobBefore {
		t.Fatalf("another person's bead changed:\n before %s\n after  %s", bobBefore, after)
	}
}

func TestReadCursors_RefusesNonOIDCAndBadIDs(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})

	// Static, machine and land principals have no "me".
	for _, c := range []struct {
		method authMethod
		id     AuthIdentity
	}{
		{authMethodStatic, AuthIdentity{Type: "static", Subject: "static"}},
		{authMethodMachine, AuthIdentity{Type: "machine", Subject: "machine:beads-viewer"}},
		{authMethodLand, AuthIdentity{Type: "oidc", Subject: "alice@example.com"}},
	} {
		for _, post := range []bool{false, true} {
			req := readCursorsRequest(http.MethodGet, "alice@example.com", nil)
			if post {
				req = readCursorsRequest(http.MethodPost, "alice@example.com", setCursors(map[string]string{"gd-ch1": "gd-m1"}))
			}
			req = req.WithContext(WithAuthIdentity(withAuthMethod(req.Context(), c.method), c.id))
			rec := httptest.NewRecorder()
			if post {
				s.handleSetReadCursors(rec, req)
			} else {
				s.handleGetReadCursors(rec, req)
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("%v (post=%v): status %d, want 403", c.method, post, rec.Code)
			}
		}
	}

	// An applicant (realm sign-in, no gardenio role, no group) has no Commons,
	// so no positions, even on their own provisioned bead.
	for _, post := range []bool{false, true} {
		req := readCursorsRequest(http.MethodGet, "alice@example.com", nil)
		if post {
			req = readCursorsRequest(http.MethodPost, "alice@example.com", setCursors(map[string]string{"gd-ch1": "gd-m1"}))
		}
		req = req.WithContext(WithGroups(WithRoles(req.Context(), nil), nil))
		rec := httptest.NewRecorder()
		if post {
			s.handleSetReadCursors(rec, req)
		} else {
			s.handleGetReadCursors(rec, req)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("applicant (post=%v): status %d, want 403", post, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	s.handleGetReadCursors(rec, readCursorsRequest(http.MethodGet, "nobody@example.com", nil))
	requireStatus(t, rec, http.StatusNotFound)

	for name, body := range map[string]any{
		"empty":     setCursors(map[string]string{}),
		"bad key":   setCursors(map[string]string{"gd ch": "gd-m1"}),
		"bad value": setCursors(map[string]string{"gd-ch1": "../x"}),
	} {
		rec = httptest.NewRecorder()
		s.handleSetReadCursors(rec, readCursorsRequest(http.MethodPost, "alice@example.com", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if _, ok := userBeadFields(ms.beads["u-alice"])[readCursorsField]; ok {
		t.Fatalf("a refused request wrote read_cursors")
	}
}

// The buffer writes at most once per interval per user, flushes what waited
// when the interval ends with the newest position per channel, and keeps a
// failed write's positions for the next one.
func TestReadCursorBuffer_OneWritePerInterval(t *testing.T) {
	var mu sync.Mutex
	var writes []map[string]string
	fail := false
	flushed := make(chan struct{}, 4)
	b := newReadCursorBuffer(50*time.Millisecond, func(_ context.Context, id, actor string, set map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		defer func() { flushed <- struct{}{} }()
		if fail {
			fail = false
			return errors.New("daemon down")
		}
		writes = append(writes, set)
		return nil
	})
	ctx := context.Background()
	if w, err := b.put(ctx, "u1", "a@x", map[string]string{"c1": "m1"}); !w || err != nil {
		t.Fatalf("first put written=%v err=%v", w, err)
	}
	<-flushed
	for _, m := range []string{"m2", "m3"} {
		if w, _ := b.put(ctx, "u1", "a@x", map[string]string{"c1": m}); w {
			t.Fatalf("put %s inside the interval was written now", m)
		}
	}
	b.put(ctx, "u1", "a@x", map[string]string{"c2": "m7"})
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting positions were never written")
	}
	mu.Lock()
	if len(writes) != 2 || writes[1]["c1"] != "m3" || writes[1]["c2"] != "m7" {
		t.Fatalf("writes = %v, want one deferred write of the newest positions", writes)
	}
	fail = true
	mu.Unlock()

	time.Sleep(60 * time.Millisecond)
	if w, err := b.put(ctx, "u1", "a@x", map[string]string{"c3": "m8"}); !w || err == nil {
		t.Fatalf("put after the interval: written=%v err=%v, want a failed write now", w, err)
	}
	<-flushed
	if got := b.pending("u1"); got["c3"] != "m8" {
		t.Fatalf("pending after a failed write = %v, want it kept", got)
	}
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("the failed write was never retried")
	}
	mu.Lock()
	defer mu.Unlock()
	if last := writes[len(writes)-1]; last["c3"] != "m8" {
		t.Fatalf("retry wrote %v", last)
	}
}

// read_cursors is written only through /v1/users/me/read-cursors. The static
// token every agent pod holds and fleet tokens are refused on the generic
// update paths (HTTP PATCH and gRPC UpdateBead share updateBead), on any
// person's user bead, whatever the map's size or ids; an echo of the stored
// map from a read-modify-write client is not a write.
func TestReadCursors_GenericWriteGate(t *testing.T) {
	const stored = `{"email":"alice@example.com","read_cursors":{"gd-ch1":"gd-m1"}}`
	big := map[string]string{}
	for i := 0; i <= readCursorsMax; i++ {
		big[fmt.Sprintf("gd-ch%d", i)] = "gd-m9"
	}
	patches := map[string]map[string]any{
		"mark read":    setCursors(map[string]string{"gd-ch1": "gd-m9"}),
		"over the cap": setCursors(big),
		"bad ids":      setCursors(map[string]string{"../x": "not an id"}),
	}
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for pname, patch := range patches {
			raw, _ := json.Marshal(patch)
			t.Run(name+"/"+pname+"/updateBead", func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedIdentityUserBead(t, ms, "gd-user1", stored)
				_, err := s.updateBead(ctx, "gd-user1", updateBeadInput{Fields: raw})
				requireDenied(t, err)
				if got := string(ms.beads["gd-user1"].Fields); got != stored {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
			t.Run(name+"/"+pname+"/grpc", func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedIdentityUserBead(t, ms, "gd-user1", stored)
				_, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-user1", Fields: raw})
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("gRPC UpdateBead: %v, want PermissionDenied", err)
				}
				if got := string(ms.beads["gd-user1"].Fields); got != stored {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
		}
		t.Run(name+"/echo", func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedIdentityUserBead(t, ms, "gd-user1", stored)
			patch := json.RawMessage(`{"read_cursors":{"gd-ch1":"gd-m1"},"status_message":"x"}`)
			if _, err := s.updateBead(ctx, "gd-user1", updateBeadInput{Fields: patch}); err != nil {
				t.Fatalf("echoed unchanged read_cursors denied: %v", err)
			}
		})
	}
}
