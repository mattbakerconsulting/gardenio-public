package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// project_label_invariant.go: a bead that says which project it belongs to
// carries that project's label (gd-YdsEpnN3Qo).
//
// A per-agent token is scoped to one project and sees only beads labelled with
// it (beadMatchesTokenProject), while authz has always read a project-bearing
// bead's project from fields.project when the label is absent
// (labelsForAuthz). A bead created with fields.project but no project: label
// therefore passes authz and is then invisible to every agent of its own
// project: 403 "token not authorized for this bead's project". Escalation
// replay, the slack-bridge and owner-session spawns have each produced such
// beads, and each was found only by an agent hitting that 403. createBead
// closes the gap at the choke point every create path shares:
//
//   - a project-bearing type (its TypeConfig declares a `project` field) with
//     fields.project and no project: label gets project:<fields.project>;
//   - one whose project: labels all name a different project is refused, as a
//     release already is (releases.go);
//   - a message with no project: label inherits the one project label of the
//     thread head its fields.thread_id names (the slack-bridge writes thread
//     replies with thread_id and no project label).
//   - any other bead with no project: label that is created with a
//     parent_bead_id, or later linked parent-child, inherits the one project
//     label of its parent (an operator's laptop `kd create` + `kd dep add`
//     under a plan wrote steps no agent of the plan could read, gd-BYCG4IGJB2).
//
// gardenio_beads_project_label_missing{type} (project_label_lint.go) counts
// the beads that break the rule; the admin route
// POST /v1/admin/backfill-project-labels (kd admin backfill-project-labels)
// backfills the ones written before it.

// projectFieldNotHomeTypes declare a `project` field that names some other
// project than the bead's own, so the invariant does not apply: a
// plot_application's fields.project is the land it applies for, and its label
// is always project:lands-intake (plot_application.go).
var projectFieldNotHomeTypes = map[model.BeadType]bool{
	typePlotApplication: true,
}

// isProjectBearing reports whether beads of this type name their own project
// in fields.project.
func isProjectBearing(beadType model.BeadType, tc *model.TypeConfig) bool {
	if tc == nil || projectFieldNotHomeTypes[beadType] {
		return false
	}
	for _, f := range tc.Fields {
		if f.Name == "project" {
			return true
		}
	}
	return false
}

// knownProjectLabels returns the non-empty project names of labels, leaving
// out the "unknown" placeholder (projectLabelValues keeps it).
func knownProjectLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" && v != "unknown" {
			out = append(out, v)
		}
	}
	return out
}

// projectLabelMismatchWarnOnly reports whether BEADS_PROJECT_LABEL_MISMATCH
// asks for a contradicting project label to be logged rather than refused.
// The default (unset) refuses; "warn" is the escape hatch should a create
// path turn out to write one on purpose.
func projectLabelMismatchWarnOnly() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("BEADS_PROJECT_LABEL_MISMATCH")), "warn")
}

// applyProjectLabelInvariant returns labels with project:<fields.project>
// added to a project-bearing bead that has no project: label. When its
// project: labels all name another project it returns them unchanged with an
// inputError, which createBead returns after its authz checks so a denial
// keeps precedence.
func applyProjectLabelInvariant(ctx context.Context, beadType model.BeadType, tc *model.TypeConfig, labels []string, fields json.RawMessage) ([]string, error) {
	if !isProjectBearing(beadType, tc) {
		return labels, nil
	}
	project := strings.TrimSpace(projectFromFields(fields))
	if project == "" {
		return labels, nil
	}
	have := knownProjectLabels(labels)
	if len(have) == 0 {
		if hasProjectLabel(labels) {
			// Only project:unknown: leave it for the lint to count.
			return labels, nil
		}
		recordProjectLabelInvariant(string(beadType), projectLabelActionDerived)
		return append(labels, "project:"+project), nil
	}
	for _, p := range have {
		if p == project {
			return labels, nil
		}
	}
	if projectLabelMismatchWarnOnly() {
		recordProjectLabelInvariant(string(beadType), projectLabelActionMismatchWarn)
		slog.Warn("project label does not match fields.project", "type", beadType, "labels", have, "project", project, "actor", actorFromContext(ctx))
		return labels, nil
	}
	recordProjectLabelInvariant(string(beadType), projectLabelActionRefused)
	return labels, inputError(fmt.Sprintf("%s: label project:%s does not match fields.project %q", beadType, have[0], project))
}

// projectLabelUpdateError holds an update to the rule createBead enforces,
// for every caller: when a patch or a label route changes a project-bearing
// bead's project: labels or its fields.project, a non-empty fields.project
// must be one of its project: labels after the change. Without it a bead
// created in one project could be repointed at another by fields.project,
// which consumers such as the slack-bridge's reaction rules act on
// (gd-bRLiEzvMsy). A bead that had no known project label before and has none
// after is the legacy shape the lint counts and is left alone, so an
// unrelated patch never wedges it. BEADS_PROJECT_LABEL_MISMATCH=warn applies
// as at create.
func projectLabelUpdateError(ctx context.Context, beadType model.BeadType, tc *model.TypeConfig, priorLabels, labels []string, fields json.RawMessage) error {
	if !isProjectBearing(beadType, tc) {
		return nil
	}
	project := strings.TrimSpace(projectFromFields(fields))
	if project == "" {
		return nil
	}
	have := knownProjectLabels(labels)
	if slices.Contains(have, project) {
		return nil
	}
	if len(have) == 0 && len(knownProjectLabels(priorLabels)) == 0 {
		return nil
	}
	if projectLabelMismatchWarnOnly() {
		recordProjectLabelInvariant(string(beadType), projectLabelActionMismatchWarn)
		slog.Warn("project label does not match fields.project on update", "type", beadType, "labels", have, "project", project, "actor", actorFromContext(ctx))
		return nil
	}
	recordProjectLabelInvariant(string(beadType), projectLabelActionRefused)
	if len(have) == 0 {
		return inputError(fmt.Sprintf("%s: fields.project %q needs its label project:%s", beadType, project, project))
	}
	return inputError(fmt.Sprintf("%s: label project:%s does not match fields.project %q", beadType, have[0], project))
}

// projectLabelChangeError is projectLabelUpdateError for the label routes,
// which add or remove one label.
func (s *BeadsServer) projectLabelChangeError(ctx context.Context, target *model.Bead, label string, add bool) error {
	if !strings.HasPrefix(label, "project:") {
		return nil
	}
	tc, err := s.resolveTypeConfig(ctx, target.Type)
	if err != nil {
		return fmt.Errorf("failed to resolve type config: %w", err)
	}
	labels := slices.DeleteFunc(slices.Clone(target.Labels), func(l string) bool { return l == label })
	if add {
		labels = append(labels, label)
	}
	return projectLabelUpdateError(ctx, target.Type, tc, target.Labels, labels, target.Fields)
}

// projectFieldEndpointError refuses fields.project on a project-bearing bead
// through a single-field endpoint (the field routes, a close body). Those
// write the key straight to the store, past projectLabelUpdateError; a fields
// PATCH runs it.
func (s *BeadsServer) projectFieldEndpointError(ctx context.Context, bead *model.Bead, keys ...string) error {
	if bead == nil || !slices.Contains(keys, "project") {
		return nil
	}
	tc, err := s.resolveTypeConfig(ctx, bead.Type)
	if err != nil {
		return fmt.Errorf("failed to resolve type config: %w", err)
	}
	if !isProjectBearing(bead.Type, tc) {
		return nil
	}
	return fmt.Errorf("project on a %s bead is written with a fields update (kd update <id> -f project=...), which keeps it in step with its project: label", bead.Type)
}

// withThreadHeadProjectLabel returns a message's labels with the project
// label of its thread head (fields.thread_id) added when the message has no
// project: label and the head has exactly one. A head that cannot be read or
// carries no single project leaves the labels as they are: the create never
// fails on this lookup.
//
// The head is read through the gates a by-id fetch enforces (project scope,
// then taints). The authz checks after this name the inherited project in
// their denial, so inheriting from a head the caller cannot read would let
// any thread_id probe a bead's existence and project.
func (s *BeadsServer) withThreadHeadProjectLabel(ctx context.Context, labels []string, fields json.RawMessage) []string {
	if hasProjectLabel(labels) {
		return labels
	}
	headID, _, _ := exactFieldString(fields, "thread_id")
	project := s.readableSingleProject(ctx, strings.TrimSpace(headID), "message thread head")
	if project == "" {
		return labels
	}
	recordProjectLabelInvariant("message", projectLabelActionThreadInherited)
	return append(labels, "project:"+project)
}

// withParentProjectLabel returns a new bead's labels with its parent's one
// project label added when the bead has no project: label of its own. A
// label-less advice or config bead is global by design (labelLessReadableType)
// and stays so. It runs after the agent-token stamp, so only an unscoped
// caller's bead (an operator's OIDC kd, a machine) ever inherits.
func (s *BeadsServer) withParentProjectLabel(ctx context.Context, beadType model.BeadType, labels []string, parentID string) []string {
	if hasProjectLabel(labels) || labelLessReadableType(beadType) {
		return labels
	}
	project := s.readableSingleProject(ctx, strings.TrimSpace(parentID), "parent")
	if project == "" {
		return labels
	}
	recordProjectLabelInvariant(string(beadType), projectLabelActionParentInherited)
	return append(labels, "project:"+project)
}

// withMailRecipientProjectLabel returns a mail's labels with its recipient's
// one project label added when the mail has none. The recipient reads its
// mail with a project-scoped credential (its per-agent token, or the runner
// bearer for runner:<id> mail), and both the agent-scoped event stream and
// its list reads drop a bead without that label; mail from an unscoped sender
// (an operator, a bridge, the daemon) is otherwise born without one
// (gd-D8jkm7eyEO). The recipient is the agent bead whose fields.agent is the
// assignee, or the runner bead for runner:<id>. It is read through the
// caller's gates, as for a parent.
func (s *BeadsServer) withMailRecipientProjectLabel(ctx context.Context, labels []string, assignee string) []string {
	assignee = strings.TrimSpace(assignee)
	if hasProjectLabel(labels) || assignee == "" {
		return labels
	}
	id, isRunner := strings.CutPrefix(assignee, "runner:")
	if !isRunner {
		agent, err := s.store.FindAgentByName(ctx, assignee, nil)
		if err != nil || agent == nil {
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				slog.Warn("mail recipient lookup failed; writing without its project label", "assignee", assignee, "error", err)
			}
			return labels
		}
		id = agent.ID
	}
	project := s.readableSingleProject(ctx, id, "mail recipient")
	if project == "" {
		return labels
	}
	recordProjectLabelInvariant("mail", projectLabelActionRecipientInherited)
	return append(labels, "project:"+project)
}

// inheritParentProjectLabel gives the child of a new parent-child edge its
// parent's one project label when the child has none: the dep-add half of
// withParentProjectLabel, for a bead created first and linked after (a step
// `kd create` + `kd dep add`ed under a plan). Best-effort: the edge is already
// written, and a failure here only logs.
func (s *BeadsServer) inheritParentProjectLabel(ctx context.Context, dep *model.Dependency) {
	if dep.Type != model.DepParentChild {
		return
	}
	child, err := s.store.GetBead(ctx, dep.BeadID)
	if err != nil || child == nil {
		return
	}
	labels := s.withParentProjectLabel(ctx, child.Type, child.Labels, dep.DependsOnID)
	if len(labels) == len(child.Labels) {
		return
	}
	label := labels[len(labels)-1]
	if err := s.store.AddLabel(ctx, child.ID, label); err != nil {
		slog.Warn("parent-child dep: failed to inherit the parent's project label",
			"bead_id", child.ID, "parent", dep.DependsOnID, "label", label, "error", err)
		return
	}
	s.recordAndPublish(ctx, events.TopicLabelAdded, child.ID, "", events.LabelAdded{
		BeadID: child.ID,
		Label:  label,
		Fleet:  FleetFromContext(ctx),
	})
}

// readableSingleProject returns the one project of bead id, or "" when id is
// blank, the bead carries no single project, or the caller cannot read it.
// The bead is read through the gates a by-id fetch enforces (project scope,
// then taints): the authz checks after an inherit name the inherited project
// in their denial, so inheriting from a bead the caller cannot read would let
// any thread_id or parent probe a bead's existence and project.
func (s *BeadsServer) readableSingleProject(ctx context.Context, id, what string) string {
	if id == "" {
		return ""
	}
	b, err := s.getBeadFiltered(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errReadTokenDenied) && !errors.Is(err, errReadDenied) && err.Error() != "bead not found" {
			slog.Warn(what+" lookup failed; writing without its project label", "id", id, "error", err)
		}
		return ""
	}
	if denied, err := s.subresourceTaintDenied(ctx, b.ID); err != nil || denied {
		if err != nil {
			slog.Warn(what+" taint check failed; writing without its project label", "id", id, "error", err)
		}
		return ""
	}
	projects := knownProjectLabels(b.Labels)
	if len(projects) != 1 {
		return ""
	}
	return projects[0]
}

// projectLabelInvariantTotal counts what the invariant did at create and, for
// refused and mismatch_warn, at update.
// action=derived, thread_inherited, parent_inherited and recipient_inherited
// name the paths that still omit the label (by type); action=refused counts
// contradicting labels.
// Cardinality: the project-bearing types (about ten) plus message × 4 actions.
var projectLabelInvariantTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_project_label_invariant_total",
		Help: "Project-label invariant outcomes at create and update (gd-YdsEpnN3Qo, gd-bRLiEzvMsy). action=derived: project:<fields.project> added to a project-bearing bead; thread_inherited: a message took its thread head's project label; parent_inherited: a label-less bead took its parent-child parent's; recipient_inherited: label-less mail took its recipient agent's or runner's; refused: the project label contradicted fields.project; mismatch_warn: the same, allowed under BEADS_PROJECT_LABEL_MISMATCH=warn.",
	},
	[]string{"bead_type", "action"},
)

const (
	projectLabelActionDerived            = "derived"
	projectLabelActionThreadInherited    = "thread_inherited"
	projectLabelActionParentInherited    = "parent_inherited"
	projectLabelActionRecipientInherited = "recipient_inherited"
	projectLabelActionRefused            = "refused"
	projectLabelActionMismatchWarn       = "mismatch_warn"
)

func recordProjectLabelInvariant(beadType, action string) {
	projectLabelInvariantTotal.WithLabelValues(beadType, action).Inc()
}
