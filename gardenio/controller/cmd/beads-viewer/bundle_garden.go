package main

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// gardenBundles is the bundle panel's reader (gardenhub.BundleReader, Lands workbench-glass spec WB-9, step 5.10): the
// documents /bundle/{id} shows (bundle.go buildBundleView: the type=doc children by the parent-child edges gb bundle add
// writes, in their position order; the producing pull request by its produces edge; the bead it was filed against by its
// first parent mention), raw, all on the request's bearer, so the daemon cuts the answer as it cuts the page. The panel
// renders each format itself. A report child (gb decision report under a bundle) reads as its markdown, and a screenshot
// in the sessions bucket is sent as its /image/ proxy path (bundle_renderers.go rewriteSessionImageURL), which the panel
// loads.
type gardenBundles struct{ s *Server }

// Bundle implements gardenhub.BundleReader.
func (g gardenBundles) Bundle(ctx context.Context, id string) (*gardenhub.BundleDoc, error) {
	s := g.s
	b, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil || b.Type != "bundle" {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not a bundle bead"}
	}
	doc := &gardenhub.BundleDoc{ID: b.ID, Title: b.Title, Kind: b.Fields["kind"], Status: b.Status, Description: b.Description}
	if mentions, err := s.daemon.GetMentions(ctx, id, 50); err != nil {
		s.logger.Debug("garden bundle: mentions", "id", id, "error", err)
	} else if back := firstBundleBackLink(mentions); back != nil {
		doc.Parent = &gardenhub.BundleRef{ID: back.ID, Title: back.Title}
	}
	deps, err := s.daemon.GetReverseDependencies(ctx, id)
	if err != nil {
		s.logger.Warn("garden bundle: reverse deps", "id", id, "error", err)
		doc.Partial = append(doc.Partial, "docs")
		return doc, nil
	}
	ids, mrIDs := dedupedParentChildIDs(deps), producesEdgeBeadIDs(deps)
	if len(ids)+len(mrIDs) == 0 {
		return doc, nil
	}
	got, err := s.daemon.BatchGetBeads(ctx, append(append([]string{}, ids...), mrIDs...))
	if err != nil {
		s.logger.Warn("garden bundle: docs", "id", id, "error", err)
		doc.Partial = append(doc.Partial, "docs")
		return doc, nil
	}
	for _, mid := range mrIDs {
		if m := got[mid]; m != nil && m.Type == "merge-request" {
			if f := mrFieldsFromBead(m); f.MRURL != "" {
				doc.MR = &gardenhub.BundleMR{URL: f.MRURL, Number: f.MRIID, State: f.MRState, Title: f.Title}
				break
			}
		}
	}
	rctx := s.newBundleRenderCtx()
	type part struct {
		gardenhub.BundlePart
		pos int
	}
	var parts []part
	for _, cid := range ids {
		c := got[cid]
		if c == nil || (c.Type != "doc" && c.Type != "report") { // a deleted doc's edge lingers: skip it, as the page does
			continue
		}
		pos, err := strconv.Atoi(strings.TrimSpace(c.Fields["position"]))
		if err != nil {
			pos = 1 << 30 // an unplaced document goes last
		}
		p := gardenhub.BundlePart{
			ID: c.ID, Title: firstNonBlank(c.Fields["headline"], c.Title), Format: c.Fields["format"],
			Language: c.Fields["language"], Content: c.Fields["content"], URL: c.Fields["content_url"],
		}
		if c.Type == "report" && (p.Format == "" || strings.EqualFold(p.Format, "markdown")) {
			p.Format = "md"
		}
		if proxied := rewriteSessionImageURL(rctx, p.URL); proxied != "" { // a session screenshot, through the image proxy
			p.URL = proxied
		}
		parts = append(parts, part{p, pos})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].pos < parts[j].pos })
	for _, p := range parts {
		doc.Docs = append(doc.Docs, p.BundlePart)
	}
	return doc, nil
}
