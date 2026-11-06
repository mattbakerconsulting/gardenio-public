package bridge

// plan_map_link.go gives a decision card that points at a plan one labelled
// button, "Open on the map", to the plan's plot on the map at /garden
// (gd-BJLVTSlAbO, part (a) of gd-kNV6Eos4ON). The owner could not find a plan
// from a beadLink to the beads-viewer, and agent prose that named "the
// fields" did not help; the map is where the plan is laid out.
//
// The link is the Lands deep link <viewer>/garden?at=<project>/<plan-id>
// (docs/homestead-world-design.md §7), on the same viewer base the bead links
// use. A card that does not resolve to a plan, or a plan with no project:
// label, or a bridge with no viewer URL, gets no button.

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// planMapActionID is the map button's action id. It is a URL button, so
// Slack opens the link itself; no handler takes the click (handleBlockActions
// skips an unknown action id).
const planMapActionID = "open_plan_map"

// dependencyLister is the optional daemon capability cardPlan needs to read a
// subject's parent. The production *beadsapi.Client satisfies it; a stub that
// only implements BeadClient resolves a plan only from requesting_plan or a
// subject that is itself the plan.
type dependencyLister interface {
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
}

// isPlanEpic reports whether bead is a plan: an epic labelled plan (the test
// gb uses for requesting_plan, resolveOwningPlan).
func isPlanEpic(bead *beadsapi.BeadDetail) bool {
	return bead != nil && bead.Type == "epic" && slices.Contains(bead.Labels, "plan")
}

// cardPlan is the plan a card points at: the decision's requesting_plan when
// it names a plan, else the subject (its requesting_task) when that is a
// plan, as on the steward's run-it card, else the subject's parent when that
// is a plan, as for a step's decision. nil when none of these is a plan.
func (b *Bot) cardPlan(ctx context.Context, requestingPlan string, subject *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if plan := b.cardSubjectBead(ctx, requestingPlan); isPlanEpic(plan) {
		return plan
	}
	if subject == nil {
		return nil
	}
	if isPlanEpic(subject) {
		return subject
	}
	parentID := subject.ParentID
	if parentID == "" {
		dl, ok := b.daemon.(dependencyLister)
		if !ok {
			return nil
		}
		deps, err := dl.GetDependencies(ctx, subject.ID)
		if err != nil {
			return nil
		}
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				parentID = d.DependsOnID
				break
			}
		}
	}
	if parent := b.cardSubjectBead(ctx, parentID); isPlanEpic(parent) {
		return parent
	}
	return nil
}

// planMapHref is the plan's place on the map, <viewerURL>/garden?at=<project>/
// <plan-id>, with the project read from the plan's project: label. "" when
// any part is missing.
func planMapHref(viewerURL string, plan *beadsapi.BeadDetail) string {
	if viewerURL == "" || plan == nil || !beadIDPattern.MatchString(plan.ID) {
		return ""
	}
	project := projectFromLabels(plan.Labels)
	if project == "" {
		return ""
	}
	return strings.TrimRight(viewerURL, "/") + "/garden?at=" + url.QueryEscape(project) + "/" + plan.ID
}

// planMapButton is the "Open on the map" URL button to plan's place on the
// map, nil when planMapHref has no link for it.
func planMapButton(viewerURL string, plan *beadsapi.BeadDetail) *slack.ButtonBlockElement {
	href := planMapHref(viewerURL, plan)
	if href == "" {
		return nil
	}
	btn := slack.NewButtonBlockElement(planMapActionID, plan.ID,
		slack.NewTextBlockObject("plain_text", "Open on the map", false, false))
	btn.URL = href
	return btn
}
