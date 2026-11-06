package bridge

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type fakeUsersClient struct {
	users     []slack.User
	listCalls int
	listErr   error
}

func (f *fakeUsersClient) GetUsers(...slack.GetUsersOption) ([]slack.User, error) {
	f.listCalls++
	return f.users, f.listErr
}

func (f *fakeUsersClient) GetUserInfo(id string) (*slack.User, error) {
	for i := range f.users {
		if f.users[i].ID == id {
			return &f.users[i], nil
		}
	}
	return nil, errors.New("user_not_found")
}

func (f *fakeUsersClient) GetUserByEmail(email string) (*slack.User, error) {
	for i := range f.users {
		if f.users[i].Profile.Email == email {
			return &f.users[i], nil
		}
	}
	return nil, errors.New("users_not_found")
}

func mkSlackUser(id, handle, display, real, email string) slack.User {
	u := slack.User{ID: id, Name: handle, RealName: real}
	u.Profile.DisplayName = display
	u.Profile.Email = email
	return u
}

func lookupVia(t *testing.T, a *SlackUserAPI, q string) (int, []SlackUserInfo) {
	t.Helper()
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/slack/users/lookup?q="+q, nil))
	var out []SlackUserInfo
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, out
}

func TestSlackUserLookup(t *testing.T) {
	gone := mkSlackUser("U0000GONE1", "alice", "alice", "Alice Old", "old@x.io")
	gone.Deleted = true
	f := &fakeUsersClient{users: []slack.User{
		gone,
		mkSlackUser("U0000ALICE", "alice", "Alice", "Alice Smith", "alice@x.io"),
		mkSlackUser("U0000ALBRT", "albert", "Bert", "Albert Jones", "albert@x.io"),
	}}
	a := NewSlackUserAPI(f, slog.Default())

	cases := []struct {
		name, q string
		wantIDs []string
	}{
		{"handle skips deactivated", "%40alice", []string{"U0000ALICE"}},
		{"bare handle", "alice", []string{"U0000ALICE"}},
		{"real name", "Alice%20Smith", []string{"U0000ALICE"}},
		{"user id", "U0000ALBRT", []string{"U0000ALBRT"}},
		{"mention", "%3C%40U0000ALBRT%7Cal%3E", []string{"U0000ALBRT"}},
		{"email", "albert%40x.io", []string{"U0000ALBRT"}},
		{"ambiguous prefix", "al", []string{"U0000ALBRT", "U0000ALICE"}},
		{"unknown", "nobody", nil},
		{"unknown id", "U9999NOONE", nil},
		{"unknown email", "nobody%40x.io", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, got := lookupVia(t, a, tc.q)
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %+v, want ids %v", got, tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("match %d = %s, want %s", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestSlackUserLookup_CachesAndServesStale(t *testing.T) {
	f := &fakeUsersClient{users: []slack.User{mkSlackUser("U0000ALICE", "alice", "Alice", "Alice Smith", "a@x.io")}}
	a := NewSlackUserAPI(f, slog.Default())
	now := time.Now()
	a.now = func() time.Time { return now }

	lookupVia(t, a, "alice")
	lookupVia(t, a, "alice")
	if f.listCalls != 1 {
		t.Fatalf("users.list called %d times within TTL, want 1", f.listCalls)
	}

	now = now.Add(a.cacheTTL + time.Second)
	f.listErr = errors.New("boom")
	code, got := lookupVia(t, a, "alice")
	if code != http.StatusOK || len(got) != 1 {
		t.Fatalf("stale fallback: status=%d got=%+v", code, got)
	}
}

func TestSlackUserLookup_RequiresQuery(t *testing.T) {
	a := NewSlackUserAPI(&fakeUsersClient{}, slog.Default())
	if code, _ := lookupVia(t, a, ""); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}
