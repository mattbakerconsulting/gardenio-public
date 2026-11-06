// Package bridge provides the JIRA poller config watcher.
//
// JiraConfigWatcher reads the jira-poller config bead from the beads daemon
// on startup and subscribes to SSE update events for hot-reload. When a
// config bead with title "jira-poller" is updated, it parses the new fields
// and pushes the updated JiraPollerConfig to the poller.
package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// JiraConfigClient is the subset of beadsapi.Client used by the config watcher.
type JiraConfigClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadDescription(ctx context.Context, beadID, description string) error
}

// JiraConfigWatcher watches for config bead updates and pushes new
// configurations to a JiraPoller.
type JiraConfigWatcher struct {
	daemon JiraConfigClient
	poller *JiraPoller
	logger *slog.Logger
	envCfg JiraPollerConfig // original env-var config (fallback defaults)
	beadID string           // tracked config bead ID (empty until found/created)
}

// JiraConfigWatcherConfig holds configuration for the config watcher.
type JiraConfigWatcherConfig struct {
	Daemon JiraConfigClient
	Poller *JiraPoller
	Logger *slog.Logger
	EnvCfg JiraPollerConfig // initial config from env vars
}

// NewJiraConfigWatcher creates a new config watcher.
func NewJiraConfigWatcher(cfg JiraConfigWatcherConfig) *JiraConfigWatcher {
	return &JiraConfigWatcher{
		daemon: cfg.Daemon,
		poller: cfg.Poller,
		logger: cfg.Logger,
		envCfg: cfg.EnvCfg,
	}
}

// LoadOrSeed reads the jira-poller config bead from the daemon. If no config
// bead exists, it creates one seeded from the current env-var configuration.
// If a config bead exists, it applies its fields to the poller immediately.
func (w *JiraConfigWatcher) LoadOrSeed(ctx context.Context) {
	bead := w.findConfigBead(ctx)
	if bead == nil {
		w.seedConfigBead(ctx)
		return
	}

	w.beadID = bead.ID
	cfg := w.parseConfigBead(bead)
	w.poller.UpdateConfig(cfg)
	w.logger.Info("applied jira-poller config bead",
		"bead_id", bead.ID,
		"projects", cfg.Projects,
		"fix_versions", cfg.FixVersions,
		"required_labels", cfg.RequiredLabels)
}

// RegisterHandlers registers SSE event handlers for config bead updates.
func (w *JiraConfigWatcher) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.updated", w.handleUpdated)
	w.logger.Info("jira config watcher registered SSE handler",
		"topic", "beads.bead.updated")
}

// handleUpdated is called when any bead is updated. It filters for config
// beads with title "jira-poller" and pushes the new config to the poller.
func (w *JiraConfigWatcher) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Only handle jira-poller beads.
	if bead.Type != "jira-poller" {
		return
	}

	// Match by bead ID if known, or by title for initial discovery.
	if w.beadID != "" && bead.ID != w.beadID {
		return
	}
	if w.beadID == "" && bead.Title != "jira-poller" {
		return
	}

	w.beadID = bead.ID
	w.logger.Info("jira-poller config bead updated, reloading",
		"bead_id", bead.ID)

	// The SSE event may not carry all fields — fetch the full bead.
	detail := w.findConfigBeadByID(ctx, bead.ID)
	if detail == nil {
		// Fall back to parsing the SSE event fields directly.
		cfg := w.parseEventFields(bead.Fields)
		w.poller.UpdateConfig(cfg)
		return
	}

	cfg := w.parseConfigBead(detail)
	w.poller.UpdateConfig(cfg)
}

// findConfigBead searches for an open config bead with title "jira-poller".
func (w *JiraConfigWatcher) findConfigBead(ctx context.Context) *beadsapi.BeadDetail {
	result, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"jira-poller"},
		Statuses: []string{"open"},
		Search:   "jira-poller",
		Limit:    50,
	})
	if err != nil {
		w.logger.Warn("failed to search for jira-poller config bead", "error", err)
		return nil
	}

	for _, b := range result.Beads {
		if b.Title == "jira-poller" {
			return b
		}
	}
	return nil
}

// findConfigBeadByID fetches a specific bead by its ID using the list API
// (we use list+filter because GetBead isn't on the JiraConfigClient interface).
func (w *JiraConfigWatcher) findConfigBeadByID(ctx context.Context, beadID string) *beadsapi.BeadDetail {
	result, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"jira-poller"},
		Statuses: []string{"open"},
		Search:   "jira-poller",
		Limit:    50,
	})
	if err != nil {
		w.logger.Warn("failed to fetch config bead", "bead_id", beadID, "error", err)
		return nil
	}

	for _, b := range result.Beads {
		if b.ID == beadID {
			return b
		}
	}
	return nil
}

// seedConfigBead creates a new jira-poller config bead seeded from env vars.
func (w *JiraConfigWatcher) seedConfigBead(ctx context.Context) {
	skipAssigned := "false"
	if w.envCfg.SkipAssigned {
		skipAssigned = "true"
	}
	fields := map[string]string{
		"projects":           strings.Join(w.envCfg.Projects, ","),
		"statuses":           strings.Join(w.envCfg.Statuses, ","),
		"issue_types":        strings.Join(w.envCfg.IssueTypes, ","),
		"fix_versions":       strings.Join(w.envCfg.FixVersions, ","),
		"required_labels":    strings.Join(w.envCfg.RequiredLabels, ","),
		"excluded_labels":    strings.Join(w.envCfg.ExcludedLabels, ","),
		"filter_projects":    strings.Join(w.envCfg.FilterProjects, ","),
		"custom_jql":         w.envCfg.CustomJQL,
		"skip_assigned":      skipAssigned,
		"poll_interval":      w.envCfg.PollInterval.String(),
		"custom_fields":      formatCustomFieldMap(w.envCfg.CustomFields),
		"capture_all_custom": strconv.FormatBool(w.envCfg.CaptureAllCustom),
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		w.logger.Error("failed to marshal jira-poller config fields", "error", err)
		return
	}

	id, err := w.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       "jira-poller",
		Type:        "jira-poller",
		Kind:        "config",
		Description: string(fieldsJSON),
		Labels:      []string{"project:gardenio"},
		CreatedBy:   "jira-bridge",
		Fields:      fieldsJSON,
	})
	if err != nil {
		w.logger.Warn("failed to seed jira-poller config bead", "error", err)
		return
	}

	w.beadID = id
	w.logger.Info("seeded jira-poller config bead from env vars", "bead_id", id)
}

// parseConfigBead extracts a JiraPollerConfig from a config bead's fields.
// Missing fields fall back to the original env-var config.
func (w *JiraConfigWatcher) parseConfigBead(bead *beadsapi.BeadDetail) JiraPollerConfig {
	return w.parseEventFields(bead.Fields)
}

// parseEventFields extracts a JiraPollerConfig from a field map.
// Missing fields fall back to the original env-var config.
func (w *JiraConfigWatcher) parseEventFields(fields map[string]string) JiraPollerConfig {
	cfg := w.envCfg // start with env defaults

	if v := fields["projects"]; v != "" {
		cfg.Projects = splitTrimmed(v)
	}
	if v := fields["statuses"]; v != "" {
		cfg.Statuses = splitTrimmed(v)
	}
	if v := fields["issue_types"]; v != "" {
		cfg.IssueTypes = splitTrimmed(v)
	}
	if v := fields["fix_versions"]; v != "" {
		cfg.FixVersions = splitTrimmed(v)
	}
	if v := fields["required_labels"]; v != "" {
		cfg.RequiredLabels = splitTrimmed(v)
	}
	if v := fields["excluded_labels"]; v != "" {
		cfg.ExcludedLabels = splitTrimmed(v)
	}
	if v := fields["filter_projects"]; v != "" {
		cfg.FilterProjects = splitTrimmed(v)
	}
	if v := fields["custom_jql"]; v != "" {
		cfg.CustomJQL = v
	}
	if v := fields["skip_assigned"]; v != "" {
		cfg.SkipAssigned = v == "true"
	}
	if v := fields["poll_interval"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.PollInterval = d
		}
	}
	if v := fields["custom_fields"]; v != "" {
		cfg.CustomFields = parseCustomFieldMap(v)
	}
	if v := fields["capture_all_custom"]; v != "" {
		cfg.CaptureAllCustom = v == "true"
	}

	return cfg
}

// parseCustomFieldMap parses a comma-separated "id:bead_field" list into a
// map of JIRA custom-field id → bead field name. Entries without a colon are
// skipped. Example: "customfield_11976:jira_issue_url,customfield_123:jira_env".
func parseCustomFieldMap(s string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, field, ok := strings.Cut(pair, ":")
		id = strings.TrimSpace(id)
		field = strings.TrimSpace(field)
		if !ok || id == "" || field == "" {
			continue
		}
		out[id] = field
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// formatCustomFieldMap renders a custom-field map back into the
// comma-separated "id:bead_field" form used in config beads.
func formatCustomFieldMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+":"+m[id])
	}
	return strings.Join(parts, ",")
}

// splitTrimmed splits a comma-separated string and trims whitespace.
func splitTrimmed(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
