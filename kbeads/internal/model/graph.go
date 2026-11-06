package model

// GraphNode is a projection of a Bead for graph visualization.
// Lighter than a full Bead — only the fields the frontend needs.
type GraphNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	IssueType string   `json:"issue_type"`
	ParentID  string   `json:"parent_id,omitempty"`
	Assignee  string   `json:"assignee,omitempty"`
	Priority  int      `json:"priority"`
	Labels    []string `json:"labels,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`

	// Populated only when IncludeBody is set.
	Description string `json:"description,omitempty"`
	Notes       string `json:"notes,omitempty"`

	DepCount   int  `json:"dep_count"`
	DepByCount int  `json:"dep_by_count"`
	Ephemeral  bool `json:"ephemeral,omitempty"`

	// Agent memory stats — populated from agent bead fields when available (ka-0000000124).
	MemoryTotal       int     `json:"memory_total,omitempty"`
	MemoryAvgStrength float64 `json:"memory_avg_strength,omitempty"`
}

// GraphArgs are the request parameters for the graph endpoint.
type GraphArgs struct {
	Status       []string `json:"status,omitempty"`
	ExcludeTypes []string `json:"exclude_types,omitempty"`
	Types        []string `json:"types,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	LabelsAny    []string `json:"labels_any,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	PriorityMin  *int     `json:"priority_min,omitempty"`
	PriorityMax  *int     `json:"priority_max,omitempty"`
	ParentID     string   `json:"parent_id,omitempty"`
	Fleet        string   `json:"fleet,omitempty"`
	Assignee     string   `json:"assignee,omitempty"`
	Query        string   `json:"query,omitempty"`
	MaxAgeDays   int      `json:"max_age_days,omitempty"`
	Limit        int      `json:"limit,omitempty"`

	IncludeDeps            bool  `json:"include_deps,omitempty"`
	IncludeBody            bool  `json:"include_body,omitempty"`
	IncludeAgents          bool  `json:"include_agents,omitempty"`
	IncludeConnectedClosed *bool `json:"include_connected_closed,omitempty"`
	// IncludeTextReferences controls whether ka-id references parsed out of
	// bead text (description, notes, title, comments — see
	// docs/design-bead-references.md) are emitted as `mentions` edges.
	// Defaults to true when nil — callers must set it explicitly to false
	// to opt out.
	IncludeTextReferences *bool `json:"include_text_references,omitempty"`
}

// DependencyCounts holds forward and reverse dependency counts for a bead.
type DependencyCounts struct {
	DependencyCount int `json:"dependency_count"`
	DependentCount  int `json:"dependent_count"`
}

// GraphEdge represents a dependency relationship as a graph edge.
//
// DepType mirrors Type and is populated for all edges so consumers that
// rename `type` → `dep_type` on the wire (the convention in beads3d /
// beads-viewer) can read a stable field name. For typed text mentions
// (epic ka-0000003840) DepType carries the parsed relationship type
// (e.g. "blocks", "parent-child"); untyped mentions keep DepType="mentions".
//
// Metadata["source"] distinguishes edge provenance per the typed-mention
// research § 8 / Decision 4: "text" for bead_reference-derived edges
// (typed and untyped), "dep" for deps-table rows. Renderer parity with
// deps is on the consumer side; this layer only carries the data.
type GraphEdge struct {
	Source   string            `json:"source"`
	Target   string            `json:"target"`
	Type     string            `json:"type"`
	DepType  string            `json:"dep_type,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// BeadReference is a row from bead_references — a ka-id reference parsed
// out of bead text (description, notes, title) or a comment. See
// kbeads/docs/design-bead-references.md.
//
// RelationshipType is populated for typed mentions (epic ka-0000003840):
// `parent(ka-X)` / `parent: ka-X` syntaxes set this column to the parsed
// verb. Untyped `ka-X` mentions leave it nil and render as flat
// `mentions` edges. See research-typed-mention-syntax.md § 4.2.
type BeadReference struct {
	SourceBeadID     string  `json:"source_bead_id"`
	TargetBeadID     string  `json:"target_bead_id"`
	SourceField      string  `json:"source_field"`
	RelationshipType *string `json:"relationship_type,omitempty"`
}

// GraphStats holds aggregate bead counts by status.
type GraphStats struct {
	TotalOpen       int `json:"total_open"`
	TotalInProgress int `json:"total_in_progress"`
	TotalBlocked    int `json:"total_blocked"`
	TotalClosed     int `json:"total_closed"`
	TotalDeferred   int `json:"total_deferred"`
}

// GraphResponse is the response for the graph visualization endpoint.
type GraphResponse struct {
	Nodes []GraphNode  `json:"nodes"`
	Edges []*GraphEdge `json:"edges"`
	Stats *GraphStats  `json:"stats"`
}
