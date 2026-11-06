package main

import "gardenio/controller/internal/claudeconfig"

// The config-bead category registry and layer-merge machinery live in
// internal/claudeconfig, shared with the beads-viewer /claude-config matrix
// (task ka-0000006351). cmd/gb keeps these aliases/wrappers so its own call
// sites and test suite are unchanged.

// MergeStrategy defines how config layers are combined when multiple beads match
// an agent's context. Alias of claudeconfig.MergeStrategy.
type MergeStrategy = claudeconfig.MergeStrategy

const (
	MergeOverride   = claudeconfig.MergeOverride
	MergeConcat     = claudeconfig.MergeConcat
	MergeConcatList = claudeconfig.MergeConcatList
)

// ConfigCategory describes a known config bead category. Alias of
// claudeconfig.ConfigCategory.
type ConfigCategory = claudeconfig.ConfigCategory

// configCategories is the authoritative list of config bead categories, sourced
// from the shared package so the two consumers can never drift.
var configCategories = claudeconfig.Categories()

// ConfigCategoryNames returns the names of all known config categories.
func ConfigCategoryNames() []string { return claudeconfig.ConfigCategoryNames() }
