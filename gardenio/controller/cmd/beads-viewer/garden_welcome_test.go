package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/gardenhub"
)

// welcomeDaemon keeps one person's welcome tour flag, and the bearer each call
// went out on.
type welcomeDaemon struct {
	beadAPI
	seenAt  string
	err     error
	bearers []string
}

func (d *welcomeDaemon) MyWelcomeTour(ctx context.Context) (*beadsapi.WelcomeTour, error) {
	d.bearers = append(d.bearers, beadsapi.UserBearerFromContext(ctx))
	if d.err != nil {
		return nil, d.err
	}
	return &beadsapi.WelcomeTour{UserBeadID: "u-ann", SeenAt: d.seenAt}, nil
}

func (d *welcomeDaemon) SeenMyWelcomeTour(ctx context.Context) (*beadsapi.WelcomeTour, error) {
	d.bearers = append(d.bearers, beadsapi.UserBearerFromContext(ctx))
	if d.err != nil {
		return nil, d.err
	}
	if d.seenAt == "" {
		d.seenAt = "2026-11-01T12:00:00Z"
	}
	return &beadsapi.WelcomeTour{UserBeadID: "u-ann", SeenAt: d.seenAt}, nil
}

func TestWelcomeLand(t *testing.T) {
	for _, c := range []struct {
		name string
		sc   viewerScope
		want string
	}{
		{"unrestricted", viewerScope{Admin: true}, ""},
		{"no land", viewerScope{}, ""},
		{"gatehouse guest", viewerScope{Projects: []string{"gatehouse"}}, "gatehouse"},
		{"own land over the gatehouse", viewerScope{Projects: []string{"gatehouse", "zinnia"}}, "zinnia"},
		{"first own land by name", viewerScope{Projects: []string{"zinnia", "aster", "gatehouse"}}, "aster"},
	} {
		if got := welcomeLand(c.sc, "gatehouse"); got != c.want {
			t.Errorf("%s: welcomeLand = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGardenWelcome_DueOnceForALandsPerson(t *testing.T) {
	d := &welcomeDaemon{}
	s := &Server{daemon: d, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), landsGatehouse: "gatehouse"}
	guest := viewerScope{Projects: []string{"gatehouse"}, Roles: map[string]conversation.LandRole{"gatehouse": conversation.LandRoleGuest}}
	r := httptest.NewRequest("GET", "/garden", nil)
	r = r.WithContext(withViewerScope(beadsapi.WithUserBearer(r.Context(), "ann-token"), guest))

	got := s.gardenWelcome(r)
	if want := (gardenhub.WelcomeTour{Land: "gatehouse", Role: string(conversation.LandRoleGuest), Gate: true}); got != want {
		t.Fatalf("before the tour: %+v, want %+v", got, want)
	}
	if err := s.gardenWelcomeSeen(httptest.NewRequest("POST", "/garden/welcome", nil).WithContext(r.Context())); err != nil {
		t.Fatalf("gardenWelcomeSeen: %v", err)
	}
	if got := s.gardenWelcome(r); got != (gardenhub.WelcomeTour{}) {
		t.Fatalf("after the tour: %+v, want none due", got)
	}
	for i, b := range d.bearers {
		if b != "ann-token" {
			t.Errorf("call %d went out on %q, want the person's own bearer", i, b)
		}
	}

	// An unrestricted viewer is never toured, and the daemon is not asked.
	d.bearers = nil
	admin := httptest.NewRequest("GET", "/garden", nil)
	if got := s.gardenWelcome(admin); got != (gardenhub.WelcomeTour{}) || len(d.bearers) != 0 {
		t.Fatalf("admin: %+v after %d calls, want none due and no call", got, len(d.bearers))
	}

	// A flag that cannot be read runs no tour; a stamp that fails says so.
	d.seenAt, d.err = "", errors.New("daemon down")
	if got := s.gardenWelcome(r); got != (gardenhub.WelcomeTour{}) {
		t.Fatalf("unreadable flag: %+v, want none due", got)
	}
	if err := s.gardenWelcomeSeen(r); err == nil {
		t.Fatalf("a failed stamp answered nil")
	}
}
