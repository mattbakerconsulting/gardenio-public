package edgeauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bearerPAT extracts the token from the Authorization: Bearer header the
// validator now sends (it previously used PRIVATE-TOKEN).
func bearerPAT(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// fakeGitLab is a stub GitLab server with configurable responses for /user
// and /groups/<group>/members/all/<id>. Call counters expose how many
// upstream hits each test triggered, so cache-hit assertions are not
// implicit.
type fakeGitLab struct {
	server *httptest.Server

	// expectedToken, if non-empty, makes /user return 401 for any other PAT.
	expectedToken string

	user             userResponse
	userStatusOnAuth int // status used when PAT matches; default 200

	memberStatus int    // status for /members lookup; default 200
	groupPath    string // expected group path on the URL; e.g. "gardenio"

	userCalls    atomic.Int64
	membersCalls atomic.Int64
}

type userResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{
		expectedToken:    "good-pat",
		userStatusOnAuth: http.StatusOK,
		memberStatus:     http.StatusOK,
		groupPath:        "gardenio",
		user: userResponse{
			ID:       42,
			Username: "alice",
			Email:    "alice@example.com",
			Name:     "Alice",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		f.userCalls.Add(1)
		if bearerPAT(r) != f.expectedToken {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.userStatusOnAuth)
		if f.userStatusOnAuth != http.StatusOK {
			return
		}
		fmt.Fprintf(w, `{"id":%d,"username":%q,"email":%q,"name":%q}`,
			f.user.ID, f.user.Username, f.user.Email, f.user.Name)
	})
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		f.membersCalls.Add(1)
		want := fmt.Sprintf("/api/v4/groups/%s/members/all/%d", f.groupPath, f.user.ID)
		if r.URL.Path != want {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if f.memberStatus != http.StatusOK {
			w.WriteHeader(f.memberStatus)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"username":%q}`, f.user.ID, f.user.Username)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func newTestValidator(t *testing.T, f *fakeGitLab) *GitLabValidator {
	t.Helper()
	v, err := NewGitLabValidator(GitLabConfig{
		BaseURL:     f.server.URL,
		Group:       "gardenio",
		PositiveTTL: 5 * time.Minute,
		NegativeTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewGitLabValidator: %v", err)
	}
	return v
}

func TestValidate_Success(t *testing.T) {
	f := newFakeGitLab(t)
	v := newTestValidator(t, f)

	id, err := v.Validate(context.Background(), "good-pat")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.UserID != 42 || id.Email != "alice@example.com" || id.Username != "alice" {
		t.Errorf("unexpected identity: %+v", id)
	}
	if got := f.userCalls.Load(); got != 1 {
		t.Errorf("user calls=%d, want 1", got)
	}
	if got := f.membersCalls.Load(); got != 1 {
		t.Errorf("members calls=%d, want 1", got)
	}
}

func TestValidate_InvalidToken(t *testing.T) {
	f := newFakeGitLab(t)
	v := newTestValidator(t, f)

	_, err := v.Validate(context.Background(), "bogus-pat")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err=%v, want ErrInvalidToken", err)
	}
	if got := f.membersCalls.Load(); got != 0 {
		t.Errorf("members called %d times for invalid PAT; should short-circuit", got)
	}
}

func TestValidate_NotInGroup(t *testing.T) {
	f := newFakeGitLab(t)
	f.memberStatus = http.StatusNotFound
	v := newTestValidator(t, f)

	id, err := v.Validate(context.Background(), "good-pat")
	if !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("err=%v, want ErrNotInGroup", err)
	}
	// Partial identity from /user must ride along with the deny so audit
	// records can attribute the request to a specific actor.
	if id == nil {
		t.Fatal("identity=nil on ErrNotInGroup; want partial identity from /user")
	}
	if id.UserID != 42 || id.Email != "alice@example.com" || id.Name != "Alice" {
		t.Errorf("partial identity=%+v, want UserID=42, Email=alice@example.com, Name=Alice", id)
	}
}

func TestValidate_GroupCheck_Forbidden_IsDeny(t *testing.T) {
	f := newFakeGitLab(t)
	f.memberStatus = http.StatusForbidden
	v := newTestValidator(t, f)

	id, err := v.Validate(context.Background(), "good-pat")
	if !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("err=%v, want ErrNotInGroup (forbidden treated as deny)", err)
	}
	if id == nil || id.Email != "alice@example.com" {
		t.Errorf("partial identity=%+v, want non-nil with Email=alice@example.com", id)
	}
}

func TestValidate_NotInGroup_CachedHitReturnsPartialIdentity(t *testing.T) {
	f := newFakeGitLab(t)
	f.memberStatus = http.StatusNotFound
	v := newTestValidator(t, f)

	// First call populates the negative cache with a partial identity.
	if _, err := v.Validate(context.Background(), "good-pat"); !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("first Validate: err=%v, want ErrNotInGroup", err)
	}
	// Second call should hit the cache and still return the partial identity
	// alongside ErrNotInGroup so the audit-on-cached-deny carries the actor.
	id, err := v.Validate(context.Background(), "good-pat")
	if !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("cached Validate: err=%v, want ErrNotInGroup", err)
	}
	if id == nil || id.Email != "alice@example.com" {
		t.Errorf("cached partial identity=%+v, want non-nil with Email=alice@example.com", id)
	}
	if got := f.userCalls.Load(); got != 1 {
		t.Errorf("user calls=%d, want 1 (negative cache should absorb the second call)", got)
	}
}

func TestValidate_EmptyToken(t *testing.T) {
	f := newFakeGitLab(t)
	v := newTestValidator(t, f)

	_, err := v.Validate(context.Background(), "")
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("err=%v, want ErrEmptyToken", err)
	}
	if got := f.userCalls.Load(); got != 0 {
		t.Errorf("user calls=%d for empty token; should not hit upstream", got)
	}
}

func TestValidate_PositiveCacheHit(t *testing.T) {
	f := newFakeGitLab(t)
	v := newTestValidator(t, f)

	for i := range 3 {
		if _, err := v.Validate(context.Background(), "good-pat"); err != nil {
			t.Fatalf("Validate #%d: %v", i, err)
		}
	}
	if got := f.userCalls.Load(); got != 1 {
		t.Errorf("user calls=%d, want 1 (cache should absorb the rest)", got)
	}
	if got := f.membersCalls.Load(); got != 1 {
		t.Errorf("members calls=%d, want 1 (cache should absorb the rest)", got)
	}
}

func TestValidate_NegativeCacheHit(t *testing.T) {
	f := newFakeGitLab(t)
	v := newTestValidator(t, f)

	for i := range 3 {
		_, err := v.Validate(context.Background(), "bogus-pat")
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Validate #%d: err=%v, want ErrInvalidToken", i, err)
		}
	}
	if got := f.userCalls.Load(); got != 1 {
		t.Errorf("user calls=%d, want 1 (negative cache should absorb the rest)", got)
	}
}

func TestValidate_TransientFailureNotCached(t *testing.T) {
	f := newFakeGitLab(t)
	f.userStatusOnAuth = http.StatusInternalServerError
	v := newTestValidator(t, f)

	for i := range 2 {
		_, err := v.Validate(context.Background(), "good-pat")
		if !errors.Is(err, ErrGitLabUnavailable) {
			t.Fatalf("Validate #%d: err=%v, want ErrGitLabUnavailable", i, err)
		}
	}
	if got := f.userCalls.Load(); got != 2 {
		t.Errorf("user calls=%d, want 2 (transient 5xx must not be cached)", got)
	}
}

func TestValidate_PositiveCacheExpires(t *testing.T) {
	f := newFakeGitLab(t)

	now := time.Unix(1_700_000_000, 0)
	v, err := NewGitLabValidator(GitLabConfig{
		BaseURL:     f.server.URL,
		Group:       "gardenio",
		PositiveTTL: 5 * time.Minute,
		NegativeTTL: 30 * time.Second,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewGitLabValidator: %v", err)
	}

	if _, err := v.Validate(context.Background(), "good-pat"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	now = now.Add(6 * time.Minute) // past PositiveTTL
	if _, err := v.Validate(context.Background(), "good-pat"); err != nil {
		t.Fatalf("Validate after expiry: %v", err)
	}
	if got := f.userCalls.Load(); got != 2 {
		t.Errorf("user calls=%d, want 2 (cache should expire)", got)
	}
}

func TestValidate_NegativeCacheExpires(t *testing.T) {
	f := newFakeGitLab(t)

	now := time.Unix(1_700_000_000, 0)
	v, err := NewGitLabValidator(GitLabConfig{
		BaseURL:     f.server.URL,
		Group:       "gardenio",
		PositiveTTL: 5 * time.Minute,
		NegativeTTL: 30 * time.Second,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewGitLabValidator: %v", err)
	}

	_, _ = v.Validate(context.Background(), "bogus-pat")
	now = now.Add(31 * time.Second) // past NegativeTTL
	_, _ = v.Validate(context.Background(), "bogus-pat")
	if got := f.userCalls.Load(); got != 2 {
		t.Errorf("user calls=%d, want 2 (negative cache should expire)", got)
	}
}

func TestNewGitLabValidator_RejectsMissingConfig(t *testing.T) {
	if _, err := NewGitLabValidator(GitLabConfig{Group: "g"}); err == nil {
		t.Error("expected error for missing BaseURL")
	}
	if _, err := NewGitLabValidator(GitLabConfig{BaseURL: "https://gitlab.com"}); err == nil {
		t.Error("expected error for missing Group (fails open is unsafe)")
	}
}

func TestCacheKeyHashesPAT(t *testing.T) {
	// The cache must not retain raw PATs. Hash twice and check stability;
	// then check that distinct PATs produce distinct keys.
	a := hashPAT("token-one")
	b := hashPAT("token-one")
	c := hashPAT("token-two")
	if a != b {
		t.Errorf("hashPAT not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("hashPAT collision: %q for different inputs", a)
	}
	if len(a) != 64 {
		t.Errorf("hashPAT length=%d, want 64 (sha256 hex)", len(a))
	}
}
