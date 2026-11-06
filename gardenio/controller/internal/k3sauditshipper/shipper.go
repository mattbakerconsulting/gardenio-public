package k3sauditshipper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/vlsink"
)

// Config is the shipper's runtime configuration (populated from env by the cmd).
type Config struct {
	// Path is the audit log, e.g. /var/log/k3s-audit/audit.log.
	Path string
	// StatePath is the checkpoint file. It must live on storage that survives a
	// pod restart (a hostPath directory), or every restart re-reads the log.
	StatePath string
	// FromStart reads Path from its beginning when there is no checkpoint
	// (back-fill of up to one rotation, ~100 MB); false starts at its end.
	FromStart bool
	// PollInterval is how long to wait at EOF before looking again.
	PollInterval time.Duration
	// FlushInterval bounds how long a partial batch waits before it is POSTed.
	FlushInterval time.Duration
	// MaxBatchLines flushes once this many records are queued.
	MaxBatchLines int
	// MaxLineBytes discards a longer audit line instead of buffering it.
	MaxLineBytes int
	// RetryBackoff is the wait between attempts to deliver a failed batch.
	RetryBackoff time.Duration
	// Transform options.
	Options Options
}

// Shipper tails the audit log and delivers transformed records at-least-once:
// the checkpoint only advances after the sink accepts the batch, and a failed
// batch is retried in place rather than dropped.
type Shipper struct {
	cfg  Config
	sink vlsink.Sink
	log  *slog.Logger
}

// New builds a Shipper, filling defaults.
func New(cfg Config, sink vlsink.Sink, log *slog.Logger) *Shipper {
	if log == nil {
		log = slog.Default()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.MaxBatchLines <= 0 {
		cfg.MaxBatchLines = 500
	}
	if cfg.MaxLineBytes <= 0 {
		cfg.MaxLineBytes = 4 * 1024 * 1024
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 5 * time.Second
	}
	return &Shipper{cfg: cfg, sink: sink, log: log}
}

// Run ships until ctx is cancelled, then returns ctx.Err().
func (s *Shipper) Run(ctx context.Context) error {
	cp, have := loadCheckpoint(s.cfg.StatePath)
	t, err := newTailer(s.cfg.Path, s.cfg.MaxLineBytes, cp, have, s.cfg.FromStart)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer t.close()
	s.log.Info("tailing audit log",
		"path", s.cfg.Path, "resumed", have, "checkpoint_inode", cp.Inode, "checkpoint_offset", cp.Offset)

	var (
		batch    bytes.Buffer
		lines    int
		oversize int
		dropped  int
		unship   int
		lastSave = cp
		lastTime = time.Now()
	)
	// flush delivers the batch (retrying until it lands), then commits the
	// position of the last line read, which includes lines the filter dropped.
	flush := func() error {
		if lines > 0 {
			for {
				err := s.sink.Insert(ctx, batch.Bytes())
				if err == nil {
					break
				}
				s.log.Error("insert to VictoriaLogs failed; retrying", "err", err, "records", lines,
					"backoff", s.cfg.RetryBackoff.String())
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(s.cfg.RetryBackoff):
				}
			}
			s.log.Debug("shipped batch", "records", lines)
			batch.Reset()
			lines = 0
		}
		lastTime = time.Now()
		if t.inode != lastSave.Inode || t.offset != lastSave.Offset {
			pos := t.position()
			if pos.Inode == 0 {
				return nil
			}
			if err := saveCheckpoint(s.cfg.StatePath, pos); err != nil {
				// Not fatal: the cost is duplicates after a restart.
				s.log.Error("save checkpoint", "err", err)
			} else {
				lastSave = pos
			}
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, skipped, err := t.next()
		switch {
		case err == nil:
			if skipped {
				oversize++
				s.log.Warn("skipped over-long audit line", "max_bytes", s.cfg.MaxLineBytes, "total", oversize)
			} else {
				switch out, st := TransformStatus(line, s.cfg.Options); st {
				case Kept:
					batch.Write(out)
					batch.WriteByte('\n')
					lines++
				case Unshippable:
					// An audit gap, so it is loud and counted; the event is not
					// logged (its content failed redaction).
					unship++
					s.log.Error("dropped audit event that failed the output gate", "total", unship)
				default:
					dropped++
				}
			}
			if lines >= s.cfg.MaxBatchLines {
				if err := flush(); err != nil {
					return err
				}
			}
		case errors.Is(err, errNoData):
			if time.Since(lastTime) >= s.cfg.FlushInterval {
				if err := flush(); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.cfg.PollInterval):
			}
		default:
			return fmt.Errorf("tail audit log: %w", err)
		}
	}
}
