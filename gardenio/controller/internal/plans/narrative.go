package plans

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EditNarrative writes the plan's narrative summary (the plan epic's
// description). Empty text is allowed: an operator may clear the summary, since
// the bundle docs carry the long-form narrative.
func (s *Service) EditNarrative(ctx context.Context, planID, text string, a Actor) error {
	ctx = operatorCtx(ctx, a)
	if _, err := s.planEpic(ctx, planID); err != nil {
		return err
	}
	if len(text) > MaxNarrativeLen {
		return fail(KindTooLarge, "Narrative is too long — trim it or edit the full text in the plan bundle.", nil)
	}
	if err := s.Daemon.UpdateBead(ctx, planID, beadsapi.UpdateBeadRequest{Description: &text}); err != nil {
		s.Logger.Error("plan narrative edit", "plan", planID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to save the narrative.", err)
	}
	return nil
}

// EditDoc writes one bundle-doc body (fields.content). The doc must be a
// type=doc child of the plan's narrative bundle, so a stray id cannot rewrite
// an unrelated bead.
func (s *Service) EditDoc(ctx context.Context, planID, docID, content string, a Actor) error {
	ctx = operatorCtx(ctx, a)
	if _, err := s.planEpic(ctx, planID); err != nil {
		return err
	}
	if docID = strings.TrimSpace(docID); docID == "" {
		return fail(KindBadRequest, "Missing doc id", nil)
	}
	if len(content) > MaxDocLen {
		return fail(KindTooLarge, "Doc is too long — split it across sections or edit via kd.", nil)
	}
	doc, ok := s.docInBundle(ctx, planID, docID)
	if !ok {
		return fail(KindNotFound, "Doc is not part of this plan's narrative", nil)
	}
	// Image and link docs carry no inline body (their payload is content_url);
	// refuse rather than stamp a content field the renderer ignores.
	if f := doc.Fields["format"]; f == "image" || f == "link" {
		return fail(KindInvalid, "This doc has no editable text body.", nil)
	}
	if err := s.Daemon.UpdateBeadFieldsTyped(ctx, docID, map[string]any{"content": content}); err != nil {
		s.Logger.Error("plan doc edit", "plan", planID, "doc", docID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to save the doc.", err)
	}
	return nil
}

// docInBundle resolves docID to a type=doc bead iff it is a doc child of the
// plan's narrative bundle, walking the plan → bundle → doc chain the cockpit
// renders from. An unrelated bead, a non-doc, or a plan with no bundle all
// fail closed.
func (s *Service) docInBundle(ctx context.Context, planID, docID string) (*beadsapi.BeadDetail, bool) {
	revDeps, err := s.Daemon.GetReverseDependencies(ctx, planID)
	if err != nil {
		s.Logger.Debug("plan doc lookup: plan reverse deps", "plan", planID, "error", err)
		return nil, false
	}
	childIDs := parentChildIDs(revDeps)
	children, err := s.Daemon.BatchGetBeads(ctx, childIDs)
	if err != nil {
		s.Logger.Debug("plan doc lookup: batch get children", "plan", planID, "error", err)
		return nil, false
	}
	bundleID := BundleID(childIDs, children)
	if bundleID == "" {
		return nil, false
	}
	bRevDeps, err := s.Daemon.GetReverseDependencies(ctx, bundleID)
	if err != nil {
		s.Logger.Debug("plan doc lookup: bundle reverse deps", "bundle", bundleID, "error", err)
		return nil, false
	}
	for _, id := range parentChildIDs(bRevDeps) {
		if id != docID {
			continue
		}
		doc, err := s.Daemon.GetBead(ctx, docID)
		if err != nil || doc == nil || doc.Type != "doc" {
			return nil, false
		}
		return doc, true
	}
	return nil, false
}
