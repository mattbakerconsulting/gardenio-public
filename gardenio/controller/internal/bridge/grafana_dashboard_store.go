// Package bridge — dashboard-bead model storage.
//
// A dashboard bead (type=dashboard) keeps only small metadata in its fields;
// the (large) JSON model is stored as a type=doc bundle artifact linked under
// the bead via a parent-child dependency, per the long-form-artifacts-in-a-
// bundle advice (ka-0000004517). These helpers read and write that model doc.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md (epic ka-0000006776).
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// dashboardModelDocLabel marks the type=doc bead that carries a dashboard's
// JSON model, so the store can find it among any other docs under the bead.
const dashboardModelDocLabel = "grafana:dashboard-model"

// dashboardConflictDocLabel marks audit artifacts written when a beads↔Grafana
// conflict is resolved — the losing side's model is preserved under this label
// so the store's primary-model lookup never confuses it for the live model.
const dashboardConflictDocLabel = "grafana:dashboard-conflict"

// DashboardStore reads/writes dashboard beads and their attached JSON-model
// docs against the beads daemon.
type DashboardStore struct {
	daemon DashboardDaemon
	actor  string
	// projects, when non-nil, fences uid lookups to dashboard beads whose
	// project: labels are all in it (see SetProjects).
	projects []string
}

// DashboardDaemon is the subset of beadsapi.Client the store needs. Defined as
// an interface so tests can supply a fake.
type DashboardDaemon interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	UpdateBeadDescription(ctx context.Context, beadID, description string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	AddLabel(ctx context.Context, beadID, label string) error
	AddComment(ctx context.Context, beadID, author, text string) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// NewDashboardStore constructs a store. actor stamps created_by on writes.
func NewDashboardStore(daemon DashboardDaemon, actor string) *DashboardStore {
	if actor == "" {
		actor = "grafana-bridge"
	}
	return &DashboardStore{daemon: daemon, actor: actor}
}

// SetProjects fences FindByUID and DashboardForUID to dashboard beads that
// carry a project: label and only allowed ones, the rule GrafanaSync applies
// (gd-eGC3fokT8T). The grafana-bridge sets it to the same list as
// GrafanaSyncConfig.Projects; an empty, non-nil list matches nothing. A store
// that never calls it is unfenced (the gb CLI).
func (s *DashboardStore) SetProjects(projects []string) {
	s.projects = append([]string{}, projects...)
}

// allowed reports whether a dashboard bead with these labels passes the
// project fence.
func (s *DashboardStore) allowed(labels []string) bool {
	return s.projects == nil || dashboardProjectError(labels, s.projects) == nil
}

// ModelDoc fetches the JSON model attached to a dashboard bead. It returns the
// doc bead ID and the model bytes. Returns ("", nil, nil) when the dashboard
// bead has no model doc yet.
func (s *DashboardStore) ModelDoc(ctx context.Context, dashboardID string) (docID string, model []byte, err error) {
	// Model docs are linked child→parent: AddDependency(docID, dashboardID,
	// "parent-child"), so the doc is a *reverse* dependency of the dashboard
	// (its row reads "docID depends on dashboardID").
	deps, err := s.daemon.GetReverseDependencies(ctx, dashboardID)
	if err != nil {
		return "", nil, fmt.Errorf("dashboard store: deps of %s: %w", dashboardID, err)
	}
	for _, d := range deps {
		if d.Type != "parent-child" {
			continue
		}
		child, gerr := s.daemon.GetBead(ctx, d.BeadID)
		if gerr != nil || child == nil || child.Type != "doc" {
			continue
		}
		if !hasLabel(child.Labels, dashboardModelDocLabel) {
			continue
		}
		return child.ID, []byte(child.Fields["content"]), nil
	}
	return "", nil, nil
}

// PutModel stores or replaces the JSON model for a dashboard bead. If the bead
// has no model doc yet, one is created and linked parent-child; otherwise the
// existing doc's content is replaced. Returns the model doc bead ID.
func (s *DashboardStore) PutModel(ctx context.Context, dashboardID string, model []byte) (string, error) {
	if !json.Valid(model) {
		return "", fmt.Errorf("dashboard store: model is not valid JSON")
	}
	existingDocID, _, err := s.ModelDoc(ctx, dashboardID)
	if err != nil {
		return "", err
	}
	if existingDocID != "" {
		fields := map[string]string{"content": string(model)}
		if err := s.daemon.UpdateBeadFields(ctx, existingDocID, fields); err != nil {
			return "", fmt.Errorf("dashboard store: update model doc %s: %w", existingDocID, err)
		}
		return existingDocID, nil
	}

	fields := map[string]string{
		"format":  "code",
		"content": string(model),
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("dashboard store: marshal doc fields: %w", err)
	}
	docID, err := s.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       "dashboard model: " + dashboardID,
		Type:        "doc",
		Kind:        "data",
		Priority:    2,
		CreatedBy:   s.actor,
		Description: "parent(" + dashboardID + ") — Grafana dashboard JSON model",
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", fmt.Errorf("dashboard store: create model doc: %w", err)
	}
	if err := s.daemon.AddDependency(ctx, docID, dashboardID, "parent-child", s.actor); err != nil {
		return "", fmt.Errorf("dashboard store: link model doc %s → %s: %w", docID, dashboardID, err)
	}
	if err := s.daemon.AddLabel(ctx, docID, dashboardModelDocLabel); err != nil {
		return docID, fmt.Errorf("dashboard store: label model doc %s: %w", docID, err)
	}
	return docID, nil
}

// ArchiveConflict stores a losing-side dashboard model as an audit doc under
// the dashboard bead, so a conflict resolution that clobbers one side keeps the
// dropped model for forensics. The doc is labeled dashboardConflictDocLabel
// (not the live-model label) so ModelDoc never returns it. `which` is the
// losing side ("beads" or "grafana"); `version` tags the artifact. Returns the
// created doc bead ID.
func (s *DashboardStore) ArchiveConflict(ctx context.Context, dashboardID, which string, version int, model []byte) (string, error) {
	fields := map[string]string{
		"format":  "code",
		"content": string(model),
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("dashboard store: marshal conflict doc fields: %w", err)
	}
	title := fmt.Sprintf("dashboard conflict (%s v%d): %s", which, version, dashboardID)
	docID, err := s.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "doc",
		Kind:        "data",
		Priority:    2,
		CreatedBy:   s.actor,
		Description: "parent(" + dashboardID + ") — clobbered " + which + " dashboard model preserved for conflict audit",
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", fmt.Errorf("dashboard store: create conflict doc: %w", err)
	}
	if err := s.daemon.AddDependency(ctx, docID, dashboardID, "parent-child", s.actor); err != nil {
		return "", fmt.Errorf("dashboard store: link conflict doc %s → %s: %w", docID, dashboardID, err)
	}
	if err := s.daemon.AddLabel(ctx, docID, dashboardConflictDocLabel); err != nil {
		return docID, fmt.Errorf("dashboard store: label conflict doc %s: %w", docID, err)
	}
	return docID, nil
}

// dashboardActiveStatuses is the set of non-terminal statuses that count as a
// "live" dashboard bead. Closed dashboards are deliberately excluded so a
// deprecated/no-data dashboard that has been closed is not re-provisioned by the
// startup catch-up. FindByUID and DashboardForUID use the same set.
var dashboardActiveStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// ListActiveDashboardIDs returns the ids of every active (non-closed)
// type=dashboard bead, up to limit. Used by the grafana-bridge startup
// catch-up to sync dashboards that already existed before this pod started —
// the SSE stream only carries create/update events from "now", so pre-existing
// bead-authored dashboards would otherwise never be provisioned into Grafana
// after a fresh cluster bring-up or a bridge restart (ka-0000004760).
func (s *DashboardStore) ListActiveDashboardIDs(ctx context.Context, limit int) ([]string, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"dashboard"},
		Statuses: dashboardActiveStatuses,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("dashboard store: list active dashboards: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b != nil && b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids, nil
}

// FindByUID returns the active dashboard bead with the given dashboard_uid that
// passes the project fence, or "" if none exists. Used by backfill to stay
// idempotent and by read-back to map a Grafana uid to its bead; a bead outside
// the fence never shadows the fleet bead for its uid.
func (s *DashboardStore) FindByUID(ctx context.Context, uid string) (string, error) {
	q := beadsapi.ListBeadsQuery{
		Types:    []string{"dashboard"},
		Statuses: dashboardActiveStatuses,
		Fields:   map[string]string{"dashboard_uid": uid},
		Limit:    10,
	}
	if s.projects != nil {
		// Fenced rows are dropped after LIMIT, so ask only for allowed projects.
		q.LabelsAny = projectLabels(s.projects)
		if len(q.LabelsAny) == 0 {
			return "", nil
		}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		return "", fmt.Errorf("dashboard store: find by uid %q: %w", uid, err)
	}
	if res == nil {
		return "", nil
	}
	for _, b := range res.Beads {
		if b != nil && b.Fields["dashboard_uid"] == uid && s.allowed(b.Labels) {
			return b.ID, nil
		}
	}
	return "", nil
}

// DashboardForUID reports whether beadID is an active type=dashboard bead
// with dashboard_uid uid that passes the project fence. A nil error with false
// means the bead exists but does not qualify; the reason says why.
func (s *DashboardStore) DashboardForUID(ctx context.Context, beadID, uid string) (ok bool, reason string, err error) {
	b, err := s.daemon.GetBead(ctx, beadID)
	if err != nil {
		return false, "", fmt.Errorf("dashboard store: get %s: %w", beadID, err)
	}
	switch {
	case b == nil:
		return false, "bead not found", nil
	case b.Type != "dashboard":
		return false, "type is " + b.Type, nil
	case !slices.Contains(dashboardActiveStatuses, b.Status):
		return false, "status is " + b.Status, nil
	case b.Fields["dashboard_uid"] != uid:
		return false, "dashboard_uid is " + b.Fields["dashboard_uid"], nil
	case !s.allowed(b.Labels):
		return false, "project not allowed", nil
	}
	return true, "", nil
}

// projectLabels maps project names to their project: labels.
func projectLabels(projects []string) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		if p != "" {
			out = append(out, "project:"+p)
		}
	}
	return out
}
