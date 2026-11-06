package bridge

// The config schemas configs (init.go) registers in the beads daemon: types,
// saved views and saved contexts.

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// ConfigSetter can create/update config beads in the beads daemon.
type ConfigSetter interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadDescription(ctx context.Context, beadID, description string) error
}

// TypeConfig mirrors model.TypeConfig for JSON serialization.
type TypeConfig struct {
	Kind   string     `json:"kind" yaml:"kind"`
	Fields []FieldDef `json:"fields,omitempty" yaml:"fields"`
}

// FieldDef mirrors model.FieldDef.
type FieldDef struct {
	Name     string   `json:"name" yaml:"name"`
	Type     string   `json:"type" yaml:"type"`
	Required bool     `json:"required,omitempty" yaml:"required"`
	Values   []string `json:"values,omitempty" yaml:"values"`
}

// ViewConfig is the saved-view schema consumed by `kd view`.
type ViewConfig struct {
	Filter  ViewFilter `json:"filter" yaml:"filter"`
	Sort    string     `json:"sort,omitempty" yaml:"sort"`
	Columns []string   `json:"columns,omitempty" yaml:"columns"`
	Limit   int32      `json:"limit,omitempty" yaml:"limit"`
}

// ViewFilter matches the filter fields accepted by ListBeads.
type ViewFilter struct {
	Status   []string `json:"status,omitempty" yaml:"status"`
	Type     []string `json:"type,omitempty" yaml:"type"`
	Kind     []string `json:"kind,omitempty" yaml:"kind"`
	Labels   []string `json:"labels,omitempty" yaml:"labels"`
	Assignee string   `json:"assignee,omitempty" yaml:"assignee"`
	Search   string   `json:"search,omitempty" yaml:"search"`
}

// ContextConfig is the saved-context schema consumed by `kd context`.
type ContextConfig struct {
	Sections []ContextSection `json:"sections" yaml:"sections"`
}

// ContextSection describes one block of a rendered context.
type ContextSection struct {
	Header string   `json:"header" yaml:"header"`
	View   string   `json:"view" yaml:"view"`
	Format string   `json:"format,omitempty" yaml:"format"` // "table" (default), "list", "count"
	Fields []string `json:"fields,omitempty" yaml:"fields"` // for "list" format
}
