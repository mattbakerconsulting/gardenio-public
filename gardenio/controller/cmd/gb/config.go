package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"os"
	"sort"

	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:     "config",
	Short:   "Config bead management",
	GroupID: "session",
}

// --- Serialization types ---

// configEntry represents a legacy KV config entry (deprecated).
// Retained for backward-compatible parsing of old dump files.
type configEntry struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type adviceEntry struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Labels      []string `json:"labels,omitempty"`
	Priority    int      `json:"priority"`
}

// configBeadEntry represents a label-based config bead in the dump format.
type configBeadEntry struct {
	Title  string          `json:"title"`
	Labels []string        `json:"labels"`
	Value  json.RawMessage `json:"value"`
}

// roleEntry represents a role bead in the dump format. Fields are stored
// as a string-keyed map matching the bead's field storage.
type roleEntry struct {
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
}

// configDump is the top-level dump/load format.
type configDump struct {
	Configs     []configEntry     `json:"configs,omitempty"`
	ConfigBeads []configBeadEntry `json:"config_beads,omitempty"`
	Roles       []roleEntry       `json:"roles,omitempty"`
	Triggers    []roleEntry       `json:"triggers,omitempty"`
	Schedules   []roleEntry       `json:"schedules,omitempty"`
	Projects    []roleEntry       `json:"projects,omitempty"`
	Accounts    []roleEntry       `json:"accounts,omitempty"`
	Formulas    []roleEntry       `json:"formulas,omitempty"`
	Advice      []adviceEntry     `json:"advice,omitempty"`
}

// --- Interfaces for testability ---

type beadLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// --- Core logic ---

func dumpAdvice(ctx context.Context, client beadLister) ([]adviceEntry, error) {
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing advice: %w", err)
	}

	entries := make([]adviceEntry, 0, len(result.Beads))
	for _, b := range result.Beads {
		entries = append(entries, adviceEntry{
			Title:       b.Title,
			Description: b.Description,
			Labels:      b.Labels,
			Priority:    b.Priority,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Title < entries[j].Title
	})

	return entries, nil
}

// configBeadCreator is the interface for creating config beads during load.
type configBeadCreator interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

func dumpConfigBeads(ctx context.Context, client beadLister) ([]configBeadEntry, error) {
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads: %w", err)
	}

	entries := make([]configBeadEntry, 0, len(result.Beads))
	for _, b := range result.Beads {
		// Empty descriptions produce null; invalid JSON descriptions are skipped
		// to prevent marshal failures from polluted config beads.
		val := json.RawMessage(b.Description)
		if len(val) == 0 {
			val = json.RawMessage("null")
		} else if !json.Valid(val) {
			continue
		}
		entries = append(entries, configBeadEntry{
			Title:  b.Title,
			Labels: b.Labels,
			Value:  val,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Title != entries[j].Title {
			return entries[i].Title < entries[j].Title
		}
		return strings.Join(entries[i].Labels, ",") < strings.Join(entries[j].Labels, ",")
	})

	return entries, nil
}

// roleFields are the fields captured/restored for role beads in dump/load.
var roleFields = []string{
	"image", "storage_class", "service_account",
	"docker", "postgres",
	"cpu_request", "cpu_limit", "memory_request", "memory_limit",
	"secrets", "env", "env_json", "claude_config",
}

func dumpRoles(ctx context.Context, client beadLister) ([]roleEntry, error) {
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"role"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing role beads: %w", err)
	}

	entries := make([]roleEntry, 0, len(result.Beads))
	for _, b := range result.Beads {
		if b.Title == "" {
			continue
		}
		fields := make(map[string]string)
		for _, f := range roleFields {
			if v, ok := b.Fields[f]; ok && v != "" {
				fields[f] = v
			}
		}
		entries = append(entries, roleEntry{
			Name:   b.Title,
			Fields: fields,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return entries, nil
}

// roleLoader is the interface for creating/updating role beads during load.
type roleLoader interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

func loadRoles(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	// Fetch existing role beads for dedup.
	existing, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"role"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] warning: failed to list existing role beads: %v\n", err)
	}

	existingMap := make(map[string]string) // name -> bead ID
	if existing != nil {
		for _, b := range existing.Beads {
			if b.Title != "" {
				existingMap[b.Title] = b.ID
			}
		}
	}

	for _, e := range entries {
		if id, ok := existingMap[e.Name]; ok {
			// Update existing role bead fields.
			if err := client.UpdateBeadFields(ctx, id, e.Fields); err != nil {
				fmt.Fprintf(os.Stderr, "[config] warning: failed to update role bead %q: %v\n", e.Name, err)
				errors++
				continue
			}
			fmt.Fprintf(os.Stderr, "[config] updated role bead: %s\n", e.Name)
			updated++
		} else {
			// Create new role bead.
			fieldsJSON, _ := json.Marshal(e.Fields)
			_, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Title:  e.Name,
				Type:   "role",
				Kind:   "config",
				Fields: fieldsJSON,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[config] warning: failed to create role bead %q: %v\n", e.Name, err)
				errors++
				continue
			}
			fmt.Fprintf(os.Stderr, "[config] created role bead: %s\n", e.Name)
			created++
		}
	}
	return created, updated, errors
}

// --- Field-keyed bead sections (trigger/schedule/project/account/formula) ---
//
// These mirror the role dump/load pattern exactly: each section is a list of
// roleEntry{Name, Fields} where Name is the bead title and Fields is an
// allowlisted subset of the bead's field storage. The allowlists below
// intentionally exclude runtime/telemetry fields so a dump is a clean,
// portable config snapshot.

// triggerFields are the fields captured/restored for trigger beads (covers
// gardeners, which are role=gardener triggers).
var triggerFields = []string{
	"cron", "role", "action", "prompt", "enabled", "project", "timezone",
	"max_concurrent", "cooldown_minutes", "source", "event", "account",
	"assign_bead", "max_runs", "terminal_goal", "stop", "progress",
	"triggers", "slack_user_id", "operated_by",
}

// scheduleFields are the fields captured/restored for schedule beads.
var scheduleFields = []string{
	"cron", "enabled", "prompt", "role", "project", "timezone", "triggers",
	"stop", "progress", "terminal_goal", "max_runs", "assign_bead",
	"operated_by", "slack_user_id",
}

// projectFields are the fields captured/restored for project beads. The
// secrets[] field holds k8s secret-name references (not raw values), so it is
// safe to dump.
var projectFields = []string{
	"env", "repos", "prefix", "account", "git_url", "secrets", "env_json",
	"channel_roles", "slack_channel", "default_branch", "prewarmed_pool",
	"default_timezone", "rtk_enabled",
}

// accountFields are the ALLOWLISTED config fields captured/restored for
// account beads. This is intentionally a strict allowlist — all runtime and
// telemetry fields (total_*, *_cost_usd, utilization_*, throttle_*,
// probe_checked_at, rotation_count, session_count, last_rotated_at,
// exhausted_at, cooldown_until, soft_failover_state, overage_*, shadow_*,
// representative_claim, status) are excluded, as is litellm_key (a raw secret
// value, not a reference).
var accountFields = []string{
	"provider", "model", "credential", "fallback", "base_url", "probe_model",
}

// formulaFields are the fields captured/restored for formula beads.
var formulaFields = []string{
	"steps", "vars",
}

// dumpFieldKeyedBeads is the shared dump implementation for field-keyed bead
// sections. It mirrors dumpRoles: list open beads of beadType, capture only
// the allowlisted non-empty fields, and sort by title.
func dumpFieldKeyedBeads(ctx context.Context, client beadLister, beadType string, allowFields []string) ([]roleEntry, error) {
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{beadType},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s beads: %w", beadType, err)
	}

	entries := make([]roleEntry, 0, len(result.Beads))
	for _, b := range result.Beads {
		if b.Title == "" {
			continue
		}
		fields := make(map[string]string)
		for _, f := range allowFields {
			if v, ok := b.Fields[f]; ok && v != "" {
				fields[f] = v
			}
		}
		entries = append(entries, roleEntry{
			Name:   b.Title,
			Fields: fields,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return entries, nil
}

// loadFieldKeyedBeads is the shared load implementation for field-keyed bead
// sections. It mirrors loadRoles: dedup existing beads by title, update fields
// on a match, or create a new bead of beadType/beadKind otherwise.
func loadFieldKeyedBeads(ctx context.Context, client roleLoader, beadType, beadKind string, entries []roleEntry) (created, updated, errors int) {
	existing, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{beadType},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] warning: failed to list existing %s beads: %v\n", beadType, err)
	}

	existingMap := make(map[string]string) // title -> bead ID
	if existing != nil {
		for _, b := range existing.Beads {
			if b.Title != "" {
				existingMap[b.Title] = b.ID
			}
		}
	}

	for _, e := range entries {
		if id, ok := existingMap[e.Name]; ok {
			if err := client.UpdateBeadFields(ctx, id, e.Fields); err != nil {
				fmt.Fprintf(os.Stderr, "[config] warning: failed to update %s bead %q: %v\n", beadType, e.Name, err)
				errors++
				continue
			}
			fmt.Fprintf(os.Stderr, "[config] updated %s bead: %s\n", beadType, e.Name)
			updated++
		} else {
			fieldsJSON, _ := json.Marshal(e.Fields)
			_, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Title:  e.Name,
				Type:   beadType,
				Kind:   beadKind,
				Fields: fieldsJSON,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[config] warning: failed to create %s bead %q: %v\n", beadType, e.Name, err)
				errors++
				continue
			}
			fmt.Fprintf(os.Stderr, "[config] created %s bead: %s\n", beadType, e.Name)
			created++
		}
	}
	return created, updated, errors
}

// Per-type dump wrappers. trigger/schedule/project/account/formula beads each
// have their own allowlist; the dump shape is identical to roles.
func dumpTriggers(ctx context.Context, client beadLister) ([]roleEntry, error) {
	return dumpFieldKeyedBeads(ctx, client, "trigger", triggerFields)
}
func dumpSchedules(ctx context.Context, client beadLister) ([]roleEntry, error) {
	return dumpFieldKeyedBeads(ctx, client, "schedule", scheduleFields)
}
func dumpProjects(ctx context.Context, client beadLister) ([]roleEntry, error) {
	return dumpFieldKeyedBeads(ctx, client, "project", projectFields)
}
func dumpAccounts(ctx context.Context, client beadLister) ([]roleEntry, error) {
	return dumpFieldKeyedBeads(ctx, client, "account", accountFields)
}
func dumpFormulas(ctx context.Context, client beadLister) ([]roleEntry, error) {
	return dumpFieldKeyedBeads(ctx, client, "formula", formulaFields)
}

// Per-type load wrappers. The beadKind matches how each type is created
// elsewhere in the controller: trigger/formula beads are kind="data";
// schedule/project/account beads are kind="config".
func loadTriggers(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	return loadFieldKeyedBeads(ctx, client, "trigger", "data", entries)
}
func loadSchedules(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	return loadFieldKeyedBeads(ctx, client, "schedule", "config", entries)
}
func loadProjects(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	return loadFieldKeyedBeads(ctx, client, "project", "config", entries)
}
func loadAccounts(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	return loadFieldKeyedBeads(ctx, client, "account", "config", entries)
}
func loadFormulas(ctx context.Context, client roleLoader, entries []roleEntry) (created, updated, errors int) {
	return loadFieldKeyedBeads(ctx, client, "formula", "data", entries)
}

func loadConfigBeads(ctx context.Context, client configBeadCreator, entries []configBeadEntry) (created, skipped, errors int) {
	// Fetch existing config beads for dedup.
	existing, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] warning: failed to list existing config beads: %v\n", err)
	}

	existingSet := make(map[string]bool)
	if existing != nil {
		for _, b := range existing.Beads {
			sorted := make([]string, len(b.Labels))
			copy(sorted, b.Labels)
			sort.Strings(sorted)
			existingSet[b.Title+"|"+strings.Join(sorted, ",")] = true
		}
	}

	for _, e := range entries {
		sorted := make([]string, len(e.Labels))
		copy(sorted, e.Labels)
		sort.Strings(sorted)
		dedupKey := e.Title + "|" + strings.Join(sorted, ",")

		if existingSet[dedupKey] {
			fmt.Fprintf(os.Stderr, "[config] config bead exists, skipping: %s %v\n", e.Title, e.Labels)
			skipped++
			continue
		}

		_, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       e.Title,
			Type:        "config",
			Kind:        "config",
			Description: string(e.Value),
			Labels:      e.Labels,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to create config bead %s %v: %v\n", e.Title, e.Labels, err)
			errors++
			continue
		}
		fmt.Fprintf(os.Stderr, "[config] created config bead: %s %v\n", e.Title, e.Labels)
		created++
	}
	return created, skipped, errors
}

// --- Commands ---

var configDumpCmd = &cobra.Command{
	Use:   "dump",
	Short: "Dump config beads and advice as JSON (pipe to file for backup)",
	Long: `Fetches config beads and advice beads, then prints them as a JSON
object. The output is suitable for saving to a file and restoring
with 'gb config load'.

Sections:
  config_beads — label-based config beads
  roles        — role beads (pod spec config per agent role)
  triggers     — trigger beads (includes role=gardener triggers)
  schedules    — schedule beads
  projects     — project beads
  accounts     — account beads (config fields only; runtime/telemetry excluded)
  formulas     — formula beads
  advice       — advice beads (rules and guidance for agents)

Example:
  gb config dump > backup.json
  gb config dump | jq .config_beads`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		configBeads, err := dumpConfigBeads(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump config beads: %v\n", err)
		}

		roles, err := dumpRoles(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump role beads: %v\n", err)
		}

		triggers, err := dumpTriggers(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump trigger beads: %v\n", err)
		}

		schedules, err := dumpSchedules(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump schedule beads: %v\n", err)
		}

		projects, err := dumpProjects(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump project beads: %v\n", err)
		}

		accounts, err := dumpAccounts(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump account beads: %v\n", err)
		}

		formulas, err := dumpFormulas(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump formula beads: %v\n", err)
		}

		advice, err := dumpAdvice(ctx, daemon)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: failed to dump advice: %v\n", err)
		}

		dump := configDump{
			ConfigBeads: configBeads,
			Roles:       roles,
			Triggers:    triggers,
			Schedules:   schedules,
			Projects:    projects,
			Accounts:    accounts,
			Formulas:    formulas,
			Advice:      advice,
		}

		data, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			return fmt.Errorf("marshalling: %w", err)
		}

		fmt.Fprintln(os.Stdout, string(data))
		fmt.Fprintf(os.Stderr, "[config] dumped %d config beads, %d roles, %d triggers, %d schedules, %d projects, %d accounts, %d formulas, %d advice\n",
			len(configBeads), len(roles), len(triggers), len(schedules), len(projects), len(accounts), len(formulas), len(advice))
		return nil
	},
}

var configLoadCmd = &cobra.Command{
	Use:   "load <file>",
	Short: "Restore configs and advice from a dump file",
	Long: `Reads a JSON file produced by 'gb config dump' and restores
config entries, config beads, and advice beads to the daemon.

Config beads and advice are deduplicated by title+labels — existing
entries with matching identity are skipped.

Sections restored:
  config_beads — label-based config beads
  roles        — role beads (pod spec config per agent role)
  triggers     — trigger beads (includes role=gardener triggers)
  schedules    — schedule beads
  projects     — project beads
  accounts     — account beads (config fields only)
  formulas     — formula beads
  advice       — advice beads (rules and guidance for agents)

Field-keyed sections (roles, triggers, schedules, projects, accounts,
formulas) are deduplicated by title — fields on an existing bead are
updated; a new bead is created otherwise.

Supports three input formats:
  - Current: {"configs": [...], "config_beads": [...], "advice": [...]}
  - Legacy sectioned: {"configs": [...], "advice": [...]}
  - Legacy flat: [{key, value}, ...]

Example:
  gb config load backup.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		data, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("reading file: %w", err)
		}

		var dump configDump
		if err := json.Unmarshal(data, &dump); err != nil {
			// Try legacy format (flat array of config entries).
			var legacy []configEntry
			if err2 := json.Unmarshal(data, &legacy); err2 == nil {
				dump.Configs = legacy
			} else {
				return fmt.Errorf("parsing JSON: %w", err)
			}
		}

		// Skip legacy KV config entries with a warning.
		if len(dump.Configs) > 0 {
			fmt.Fprintf(os.Stderr, "[config] skipping %d legacy KV config entries (deprecated — use config beads instead)\n", len(dump.Configs))
		}

		// Restore config beads.
		if len(dump.ConfigBeads) > 0 {
			created, skipped, _ := loadConfigBeads(ctx, daemon, dump.ConfigBeads)
			fmt.Fprintf(os.Stderr, "[config] loaded %d config beads (%d skipped)\n", created, skipped)
		}

		// Restore role beads.
		if len(dump.Roles) > 0 {
			created, updated, _ := loadRoles(ctx, daemon, dump.Roles)
			fmt.Fprintf(os.Stderr, "[config] loaded %d role beads (%d updated)\n", created, updated)
		}

		// Restore trigger beads (includes role=gardener triggers).
		if len(dump.Triggers) > 0 {
			created, updated, _ := loadTriggers(ctx, daemon, dump.Triggers)
			fmt.Fprintf(os.Stderr, "[config] loaded %d trigger beads (%d updated)\n", created, updated)
		}

		// Restore schedule beads.
		if len(dump.Schedules) > 0 {
			created, updated, _ := loadSchedules(ctx, daemon, dump.Schedules)
			fmt.Fprintf(os.Stderr, "[config] loaded %d schedule beads (%d updated)\n", created, updated)
		}

		// Restore project beads.
		if len(dump.Projects) > 0 {
			created, updated, _ := loadProjects(ctx, daemon, dump.Projects)
			fmt.Fprintf(os.Stderr, "[config] loaded %d project beads (%d updated)\n", created, updated)
		}

		// Restore account beads.
		if len(dump.Accounts) > 0 {
			created, updated, _ := loadAccounts(ctx, daemon, dump.Accounts)
			fmt.Fprintf(os.Stderr, "[config] loaded %d account beads (%d updated)\n", created, updated)
		}

		// Restore formula beads.
		if len(dump.Formulas) > 0 {
			created, updated, _ := loadFormulas(ctx, daemon, dump.Formulas)
			fmt.Fprintf(os.Stderr, "[config] loaded %d formula beads (%d updated)\n", created, updated)
		}

		// Restore advice beads.
		if len(dump.Advice) > 0 {
			// Build set of existing advice titles for dedup.
			existing, _ := dumpAdvice(ctx, daemon)
			titleSet := make(map[string]bool, len(existing))
			for _, a := range existing {
				titleSet[a.Title] = true
			}

			created := 0
			for _, a := range dump.Advice {
				if titleSet[a.Title] {
					fmt.Fprintf(os.Stderr, "[config] advice exists, skipping: %s\n", a.Title)
					continue
				}
				_, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
					Title:       a.Title,
					Type:        "advice",
					Description: a.Description,
					Labels:      a.Labels,
					Priority:    a.Priority,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "[config] warning: failed to create advice %q: %v\n", a.Title, err)
					continue
				}
				fmt.Fprintf(os.Stderr, "[config] created advice: %s\n", a.Title)
				created++
			}
			fmt.Fprintf(os.Stderr, "[config] loaded %d advice beads (%d skipped)\n", created, len(dump.Advice)-created)
		}

		return nil
	},
}

// --- Migrate logic ---

// configMigrator is the interface for creating config beads during migration.
type configMigrator interface {
	ListConfigs(ctx context.Context, namespace string) ([]beadsapi.ConfigEntry, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// migrateAction describes one KV config entry to be migrated.
type migrateAction struct {
	Category string   // e.g. "claude-settings"
	Labels   []string // e.g. ["global"] or ["role:gardener"]
	Value    string   // JSON string (stored in bead Description)
	Skipped  bool     // true if a matching bead already exists
}

// roleKeyedNamespaces are KV namespaces where the suffix after ":"
// represents a role (or "global"). Other namespaces (type, view) use
// the suffix as a type/view identifier — not a role.
var roleKeyedNamespaces = map[string]bool{
	"claude-settings": true,
	"claude-hooks":    true,
	"claude-mcp":      true,
	"context":         true,
}

// parseKVKeyToLabels converts a KV config key into (title, labels) for
// the config bead representation.
//
// For role-keyed namespaces (claude-settings, claude-hooks, claude-mcp, context):
//
//	"claude-settings:global"  → title="claude-settings", labels=["global"]
//	"claude-settings:gardener" → title="claude-settings", labels=["role:gardener"]
//
// For definition namespaces (type, view):
//
//	"type:task"          → title="type:task",          labels=["global"]
//	"view:agents:active" → title="view:agents:active", labels=["global"]
func parseKVKeyToLabels(key string) (title string, labels []string) {
	idx := strings.Index(key, ":")
	if idx < 0 {
		return key, []string{"global"}
	}

	namespace := key[:idx]
	suffix := key[idx+1:]

	if !roleKeyedNamespaces[namespace] {
		// Definition namespace: the full key is the title, always global.
		return key, []string{"global"}
	}

	if suffix == "global" {
		return namespace, []string{"global"}
	}
	return namespace, []string{"role:" + suffix}
}

// planMigration scans all KV config entries and checks for existing
// config beads to produce a list of migration actions.
func planMigration(ctx context.Context, client configMigrator) ([]migrateAction, error) {
	// Fetch all existing config beads to check for duplicates.
	existing, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing existing config beads: %w", err)
	}

	// Build set of existing config beads keyed by "title|label1,label2".
	existingSet := make(map[string]bool)
	for _, b := range existing.Beads {
		sort.Strings(b.Labels)
		key := b.Title + "|" + strings.Join(b.Labels, ",")
		existingSet[key] = true
	}

	var actions []migrateAction
	for _, ns := range ConfigCategoryNames() {
		entries, err := client.ListConfigs(ctx, ns)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[migrate] warning: failed to list %s: %v\n", ns, err)
			continue
		}

		for _, entry := range entries {
			category, labels := parseKVKeyToLabels(entry.Key)
			sortedLabels := make([]string, len(labels))
			copy(sortedLabels, labels)
			sort.Strings(sortedLabels)

			dedupKey := category + "|" + strings.Join(sortedLabels, ",")

			actions = append(actions, migrateAction{
				Category: category,
				Labels:   labels,
				Value:    string(entry.Value),
				Skipped:  existingSet[dedupKey],
			})
		}
	}

	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Category != actions[j].Category {
			return actions[i].Category < actions[j].Category
		}
		return strings.Join(actions[i].Labels, ",") < strings.Join(actions[j].Labels, ",")
	})

	return actions, nil
}

// executeMigration creates config beads for non-skipped actions.
func executeMigration(ctx context.Context, client configMigrator, actions []migrateAction) (created, skipped, errors int) {
	for _, a := range actions {
		if a.Skipped {
			skipped++
			continue
		}
		_, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       a.Category,
			Type:        "config",
			Kind:        "config",
			Description: a.Value,
			Labels:      a.Labels,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[migrate] error creating %s %v: %v\n", a.Category, a.Labels, err)
			errors++
			continue
		}
		fmt.Fprintf(os.Stderr, "[migrate] created config bead: %s %v\n", a.Category, a.Labels)
		created++
	}
	return created, skipped, errors
}

var configMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate KV config entries to label-based config beads",
	Long: `Converts existing KV config entries (namespace:key pattern) into
proper beads with type="config", using labels for scoping.

By default runs in dry-run mode showing what would be migrated.
Use --execute to perform the actual migration.

The migration is idempotent: existing config beads with matching
title and labels are skipped.

Example:
  gb config migrate             # dry-run: show what would happen
  gb config migrate --execute   # actually create the config beads`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		actions, err := planMigration(ctx, daemon)
		if err != nil {
			return err
		}

		if len(actions) == 0 {
			fmt.Fprintf(os.Stderr, "[migrate] no KV config entries found to migrate\n")
			return nil
		}

		execute, _ := cmd.Flags().GetBool("execute")

		if !execute {
			// Dry-run: just print the plan.
			fmt.Fprintf(os.Stderr, "[migrate] dry-run — %d entries found:\n", len(actions))
			for _, a := range actions {
				status := "would create"
				if a.Skipped {
					status = "skip (exists)"
				}
				fmt.Fprintf(os.Stderr, "  %s: %s %v\n", status, a.Category, a.Labels)
			}
			fmt.Fprintf(os.Stderr, "\nRun with --execute to perform the migration.\n")
			return nil
		}

		created, skipped, errors := executeMigration(ctx, daemon, actions)
		fmt.Fprintf(os.Stderr, "[migrate] done: %d created, %d skipped, %d errors\n", created, skipped, errors)
		return nil
	},
}

func init() {
	configMigrateCmd.Flags().Bool("execute", false, "actually perform the migration (default is dry-run)")
	configCmd.AddCommand(configDumpCmd)
	configCmd.AddCommand(configLoadCmd)
	configCmd.AddCommand(configMigrateCmd)
}
