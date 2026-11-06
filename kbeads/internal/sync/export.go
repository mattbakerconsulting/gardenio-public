package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// header is the first JSONL record written by ExportJSONL.
type header struct {
	Version     string    `json:"version"`
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	BeadCount   int       `json:"bead_count"`
	ConfigCount int       `json:"config_count"`
}

// record wraps a single JSONL line with a type discriminator.
type record struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// ExportJSONL writes all beads and configs from the store as JSONL to w.
// Beads are sorted by ID and include embedded labels, dependencies, and comments.
func ExportJSONL(ctx context.Context, s store.Store, w io.Writer) error {
	// Fetch all beads (no filter, no limit).
	beads, _, err := s.ListBeads(ctx, model.BeadFilter{Sort: "created_at"})
	if err != nil {
		return fmt.Errorf("list beads: %w", err)
	}

	// Populate relational data for each bead.
	for _, b := range beads {
		labels, err := s.GetLabels(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("get labels for %s: %w", b.ID, err)
		}
		b.Labels = labels

		deps, err := s.GetDependencies(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("get dependencies for %s: %w", b.ID, err)
		}
		b.Dependencies = deps

		comments, err := s.GetComments(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("get comments for %s: %w", b.ID, err)
		}
		b.Comments = comments
	}

	// Sort beads by ID.
	sort.Slice(beads, func(i, j int) bool {
		return beads[i].ID < beads[j].ID
	})

	// Fetch all configs.
	configs, err := s.ListAllConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	// Write header.
	if err := enc.Encode(header{
		Version:     "1",
		Type:        "header",
		Timestamp:   time.Now().UTC(),
		BeadCount:   len(beads),
		ConfigCount: len(configs),
	}); err != nil {
		return fmt.Errorf("encode header: %w", err)
	}

	// Write beads.
	for _, b := range beads {
		if err := enc.Encode(record{Type: "bead", Data: b}); err != nil {
			return fmt.Errorf("encode bead %s: %w", b.ID, err)
		}
	}

	// Write configs.
	for _, c := range configs {
		if err := enc.Encode(record{Type: "config", Data: c}); err != nil {
			return fmt.Errorf("encode config %s: %w", c.Key, err)
		}
	}

	return nil
}
