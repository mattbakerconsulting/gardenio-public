package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/lands/freeze"
	"gardenio/controller/internal/lands/purse"
)

// landSwitchEvery is how often the Commons re-reads which lands are frozen.
// A freeze from the Freeze button refreshes at once; one from gb lands freeze
// shows within this, and kbeads refuses the members' writes from the moment
// it lands either way.
const landSwitchEvery = 15 * time.Second

// landSwitches is the Lands kill switch as the Commons reads it (Track B
// §5.5, step 10.5): the tenant lands and which are frozen, re-read in the
// background on the viewer's own token (a system read: which lands are
// frozen is the same for every viewer, and a member's bearer need not reach
// the project beads). A failed read keeps the last good one; before any good
// read nothing is frozen, which fails open here because kbeads refuses the
// write itself.
type landSwitches struct {
	src    purse.Source
	logger *slog.Logger

	mu     sync.RWMutex
	frozen map[string]bool // by tenant land
}

func newLandSwitches(src purse.Source, logger *slog.Logger) *landSwitches {
	return &landSwitches{src: src, logger: logger}
}

// refresh re-reads the tenant lands.
func (l *landSwitches) refresh() {
	ctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(context.Background()), 10*time.Second)
	defer cancel()
	projects, err := purse.TenantProjects(ctx, l.src)
	if err != nil {
		l.logger.Warn("commons: reading which lands are frozen failed; keeping the last read", "error", err)
		return
	}
	next := make(map[string]bool, len(projects))
	for name, b := range projects {
		next[name] = freeze.StateOf(name, b).Frozen
	}
	l.mu.Lock()
	l.frozen = next
	l.mu.Unlock()
}

// start runs the background refresh; the func it returns stops it.
func (l *landSwitches) start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go l.run(ctx)
	return cancel
}

// run refreshes every landSwitchEvery until ctx ends, with one read first.
func (l *landSwitches) run(ctx context.Context) {
	t := time.NewTicker(landSwitchEvery)
	defer t.Stop()
	for {
		l.refresh()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// state is commons.LandSwitch.State.
func (l *landSwitches) state(_ context.Context, land string) (tenant, frozen bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	frozen, tenant = l.frozen[land]
	return tenant, frozen
}

// switchFunc is commons.LandSwitch.Set: the freeze or thaw goes out on the
// operator's own bearer (project fields are admin-written in kbeads), signed
// by them, then the cache is re-read so the answer shows it.
func (l *landSwitches) switchFunc(d freeze.Daemon) func(r *http.Request, land string, frozen bool, reason string) error {
	return func(r *http.Request, land string, frozen bool, reason string) error {
		var s freeze.State
		var err error
		if frozen {
			s, err = freeze.Freeze(r.Context(), d, land, reason, commonsSelf(r), time.Now())
		} else {
			s, err = freeze.Thaw(r.Context(), d, land, commonsSelf(r))
		}
		if err != nil {
			return err
		}
		if s.Warning != "" {
			l.logger.Warn("commons: land switch set with a warning", "land", land, "frozen", frozen, "warning", s.Warning)
		}
		l.logger.Info("commons: land switch set", "land", land, "frozen", frozen, "changed", s.Changed, "by", commonsSelf(r))
		l.refresh()
		return nil
	}
}

// landSwitch is the commons.LandSwitch over l, writing through d.
func (l *landSwitches) landSwitch(d freeze.Daemon) commons.LandSwitch {
	return commons.LandSwitch{State: l.state, Set: l.switchFunc(d)}
}
