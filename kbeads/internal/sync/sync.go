package sync

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gardenio/kbeads/internal/store"
)

// Destination is the interface for a sync target (S3, git, etc.).
type Destination interface {
	// Write sends the JSONL payload to the destination.
	Write(ctx context.Context, data []byte) error
}

// Scheduler runs periodic syncs to one or more destinations.
type Scheduler struct {
	store        store.Store
	destinations []Destination
	interval     time.Duration
	logger       *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewScheduler creates a scheduler that exports from the store to the given
// destinations at the specified interval.
func NewScheduler(s store.Store, destinations []Destination, interval time.Duration, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:        s,
		destinations: destinations,
		interval:     interval,
		logger:       logger,
	}
}

// Start begins periodic sync. It runs an initial sync immediately, then
// on each tick.
func (s *Scheduler) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(ctx)
	}()
}

// Stop cancels the scheduler and waits for the current sync (if any) to finish.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context) {
	// Run once immediately at startup.
	s.syncOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

func (s *Scheduler) syncOnce(ctx context.Context) {
	var buf bytes.Buffer
	if err := ExportJSONL(ctx, s.store, &buf); err != nil {
		s.logger.Error("sync export failed", "err", err)
		return
	}
	data := buf.Bytes()

	for i, dest := range s.destinations {
		if err := dest.Write(ctx, data); err != nil {
			s.logger.Error("sync destination write failed", "destination", fmt.Sprintf("%d", i), "err", err)
		}
	}

	s.logger.Info("sync completed", "destinations", len(s.destinations), "bytes", len(data))
}
