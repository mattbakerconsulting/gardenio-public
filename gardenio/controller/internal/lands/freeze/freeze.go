// Package freeze is the per-land kill switch of Lands Track B (spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §5.5,
// step 10.5): freezing a tenant land sets land_frozen on its project bead, and
// thawing clears it. gb lands freeze|thaw and the operator's Freeze button in
// /garden both write through here, so the two can never disagree on what a
// freeze is.
//
// The switch is the one field; everything a freeze does is read from it
// elsewhere, live:
//
//   - the spawn wall (internal/lands/wall) refuses new spawns with
//     land_frozen, at the controller's pod doors and in gardenerPreFireCheck,
//     so the steward's gardener is paused too;
//   - the purse watcher (internal/lands/watcher) parks every running agent
//     of the land, park_reason=land_frozen, and after the grace blocks the
//     land's LiteLLM key, unblocking it on the thaw;
//   - kbeads revokes the land's own-machine agents (the Lands API's
//     createAgent) as the switch is set, and refuses minting or refreshing
//     one until the thaw;
//   - the Commons refuses the land's members' messages with a banner, and
//     kbeads refuses a scoped principal's creates in the land.
//
// Who froze it, when and why ride along in land_frozen_by, land_frozen_at and
// land_frozen_reason (operator-only: the tenant's banner does not show the
// reason), and each freeze and thaw leaves a comment on the project bead.
// Project fields are admin-written in kbeads, so only an operator can flip it.
package freeze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// Project bead fields. FieldFrozen is the switch itself (a boolean); the rest
// say who, when (RFC3339) and why, and are cleared by a thaw.
const (
	FieldFrozen = "land_frozen"
	FieldBy     = "land_frozen_by"
	FieldAt     = "land_frozen_at"
	FieldReason = "land_frozen_reason"

	// CommentAuthor signs the project bead comment when no operator is named.
	CommentAuthor = "gardenio-lands"
)

// Daemon is the slice of the daemon client a freeze reads and writes
// through. *beadsapi.Client satisfies it.
type Daemon interface {
	purse.Source
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// ErrOperatorLand refuses freezing one of the operator's own lands: the kill
// switch is read only on tenant lands, so it would do nothing.
var ErrOperatorLand = errors.New("operator lands have no kill switch: only a tenant land can be frozen")

// State is a land's kill switch as its project bead holds it.
type State struct {
	Land      string `json:"land"`
	ProjectID string `json:"project_id"`
	Frozen    bool   `json:"frozen"`
	By        string `json:"frozen_by,omitempty"`
	At        string `json:"frozen_at,omitempty"`
	Reason    string `json:"frozen_reason,omitempty"`
	// Changed is false when the land was already in the state asked for, and
	// nothing was written.
	Changed bool `json:"changed"`
	// Warning is a write after the switch that failed (the who/when/why
	// fields, the comment): the switch itself is set.
	Warning string `json:"warning,omitempty"`
}

// StateOf reads the kill switch off a project bead's fields.
func StateOf(land string, b *beadsapi.BeadDetail) State {
	s := State{Land: land}
	if b == nil {
		return s
	}
	s.ProjectID = b.ID
	s.Frozen = strings.TrimSpace(b.Fields[FieldFrozen]) == "true"
	if s.Frozen {
		s.By = strings.TrimSpace(b.Fields[FieldBy])
		s.At = strings.TrimSpace(b.Fields[FieldAt])
		s.Reason = strings.TrimSpace(b.Fields[FieldReason])
	}
	return s
}

// Read is a tenant land's kill switch. purse.ErrNotLand when no live project
// has the name, ErrOperatorLand for an operator land.
func Read(ctx context.Context, d purse.Source, land string) (State, error) {
	b, tenant, err := purse.Project(ctx, d, land)
	if err != nil {
		return State{Land: land}, err
	}
	if !tenant {
		return State{Land: land}, fmt.Errorf("land %q: %w", land, ErrOperatorLand)
	}
	return StateOf(land, b), nil
}

// Freeze freezes a tenant land. reason is required; by is the operator. A
// land already frozen is left as it is (its first reason stands).
func Freeze(ctx context.Context, d Daemon, land, reason, by string, now time.Time) (State, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return State{Land: land}, errors.New("a freeze needs a reason (--reason): it is the operator's record of why")
	}
	s, err := Read(ctx, d, land)
	if err != nil || s.Frozen {
		return s, err
	}
	// The switch goes first and alone, so nothing after it can keep a land
	// from freezing.
	if err := d.UpdateBeadFieldsTyped(ctx, s.ProjectID, map[string]any{FieldFrozen: true}); err != nil {
		return s, fmt.Errorf("freezing land %q: %w", land, err)
	}
	s.Frozen, s.Changed = true, true
	s.By, s.At, s.Reason = by, now.UTC().Format(time.RFC3339), reason
	var warn []string
	if err := d.UpdateBeadFieldsTyped(ctx, s.ProjectID, map[string]any{FieldBy: s.By, FieldAt: s.At, FieldReason: s.Reason}); err != nil {
		warn = append(warn, "recording who froze it: "+err.Error())
	}
	if err := d.AddComment(ctx, s.ProjectID, author(by), fmt.Sprintf("Land %s frozen by %s: %s. New spawns are refused, running agents are parked (park_reason=land_frozen) and the land's LiteLLM key is blocked after the grace, its own-machine agents are revoked, and members cannot post in the Commons until it is thawed.", land, orSomeone(by), reason)); err != nil {
		warn = append(warn, "commenting on the project bead: "+err.Error())
	}
	s.Warning = strings.Join(warn, "; ")
	return s, nil
}

// Thaw thaws a frozen tenant land. Agents the freeze suspended stay parked:
// nothing resumes on its own. A land not frozen is left as it is.
func Thaw(ctx context.Context, d Daemon, land, by string) (State, error) {
	s, err := Read(ctx, d, land)
	if err != nil || !s.Frozen {
		return s, err
	}
	prev := s
	if err := d.UpdateBeadFieldsTyped(ctx, s.ProjectID, map[string]any{FieldFrozen: false}); err != nil {
		return s, fmt.Errorf("thawing land %q: %w", land, err)
	}
	s = State{Land: land, ProjectID: s.ProjectID, Changed: true}
	var warn []string
	if err := d.UpdateBeadFieldsTyped(ctx, s.ProjectID, map[string]any{FieldBy: "", FieldAt: "", FieldReason: ""}); err != nil {
		warn = append(warn, "clearing who froze it: "+err.Error())
	}
	text := fmt.Sprintf("Land %s thawed by %s. Spawns, the land's LiteLLM key and Commons posts are open again; agents the freeze suspended stay parked until someone resumes them, and members start new own-machine agents.", land, orSomeone(by))
	if prev.Reason != "" {
		text += fmt.Sprintf(" (It was frozen by %s at %s: %s.)", orSomeone(prev.By), prev.At, prev.Reason)
	}
	if err := d.AddComment(ctx, s.ProjectID, author(by), text); err != nil {
		warn = append(warn, "commenting on the project bead: "+err.Error())
	}
	s.Warning = strings.Join(warn, "; ")
	return s, nil
}

func author(by string) string {
	if by == "" {
		return CommentAuthor
	}
	return by
}

func orSomeone(by string) string {
	if by == "" {
		return "an operator"
	}
	return by
}
