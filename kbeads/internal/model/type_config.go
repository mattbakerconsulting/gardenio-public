package model

// FieldType identifies the JSON schema type of a typed field.
type FieldType string

const (
	FieldTypeString    FieldType = "string"
	FieldTypeInteger   FieldType = "integer"
	FieldTypeFloat     FieldType = "float"
	FieldTypeBoolean   FieldType = "boolean"
	FieldTypeTimestamp FieldType = "timestamp"
	FieldTypeEnum      FieldType = "enum"
	FieldTypeStrings   FieldType = "string[]"
	FieldTypeEnums     FieldType = "enum[]"
	FieldTypeJSON      FieldType = "json"
)

// FieldDef describes a single typed field on a bead type.
type FieldDef struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
	Values   []string  `json:"values,omitempty"` // allowed values for enum / enum[]
	Min      *int      `json:"min,omitempty"`    // inclusive lower bound for integer fields
	Max      *int      `json:"max,omitempty"`    // inclusive upper bound for integer fields

	// ServiceOwnedBy declares that this field is SERVICE-OWNED STATE: an
	// automation identity legitimately stamps it on beads it did not create,
	// as a record of what the service DID, never as user-authored content or
	// configuration. Each entry is a server-derived authenticated subject
	// (AuthIdentity.Subject, e.g. "bridge:acme-scif"), or "*" for "any
	// authenticated caller".
	//
	// WHY IT LIVES HERE (ka-0000005214). The authoritative fact "which fields
	// does a service stamp on this bead type?" used to live in two unbound
	// places: the stamp helper that writes them, and
	// serviceManagedFieldsByType (beads.go) that permits them. They drifted
	// silently — commit 6a8511bce folded last_outcome/last_outcome_at into the
	// trigger last_checked patch and updated THIS schema (the unknown-field
	// 400 forced it) but not the allowlist, so the ownership bypass stopped
	// applying and three prod Jira intake lanes ran with dead liveness signals
	// for 11 days. Hanging the permission off the declaration the writer is
	// ALREADY forced to update collapses those two places into one.
	//
	// SECURITY: this declaration is only ever read from the BUILTIN (in-code)
	// type configs, never from a store override — see
	// serviceOwnedFieldsFromBuiltins (service_field_declaration_test.go). A
	// store-resolved TypeConfig is attacker-influenced: PUT /v1/configs/{key}
	// is requireRole(RoleAdmin), and requireRole short-circuits every non-OIDC
	// caller, so ANY fleet token can rewrite a type config. Reading permission
	// from the store copy would let a bridge declare itself the owner of any
	// field — including fields.requested_by, the input to the acme-scif
	// four-eyes check.
	ServiceOwnedBy []string `json:"service_owned_by,omitempty"`
}

// WritePolicy defines fine-grained authorization rules for write operations
// on beads of a given type. When nil or zero-valued, the default RBAC route-level
// check (operator for mutations, admin for deletes) applies.
type WritePolicy struct {
	// MinRole is the minimum RBAC role required for write operations on this
	// bead type. Empty means the route-level default applies.
	MinRole string `json:"min_role,omitempty"`

	// ProjectScoped requires that the acting user's authorized_projects include
	// the bead's project label. Only enforced for OIDC-authenticated requests.
	ProjectScoped bool `json:"project_scoped,omitempty"`

	// Escalatable indicates that authorization denials for this type can be
	// retried with the --escalate flag to create an escalation bead.
	Escalatable bool `json:"escalatable,omitempty"`
}

// TypeConfig defines the kind and typed fields for a bead type.
type TypeConfig struct {
	// TypeName is the bead type this config describes (e.g. "task", "trigger").
	// It is NOT part of the persisted schema JSON (the config is keyed by
	// "type:<name>" in the store, so the name is redundant there) — it is
	// stamped in-memory by resolveTypeConfig so field-validation errors can
	// point the author at `kd fields <type>` rather than dumping a truncated
	// field list. Empty when unknown (e.g. a hand-built TypeConfig in a test).
	TypeName string `json:"-"`

	Kind   Kind       `json:"kind"`
	Fields []FieldDef `json:"fields,omitempty"`
	// AllowAdditionalFields, when true, makes ValidateTypeFields skip the
	// unknown-field check and accept arbitrary keys not listed in Fields.
	// Used by open-schema types like type:config tunables, where keys
	// mirror env-var names chosen by the consuming component.
	AllowAdditionalFields bool         `json:"allow_additional_fields,omitempty"`
	WritePolicy           *WritePolicy `json:"write_policy,omitempty"`
}
