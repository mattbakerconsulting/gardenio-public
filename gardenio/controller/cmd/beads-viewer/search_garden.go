package main

import (
	"context"
	"errors"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// gardenSearchLimit is how many beads each search asks for: /search's own.
const gardenSearchLimit = 20

// gardenSearch is the search panel's reader (gardenhub.SearchReader, Lands
// workbench-glass spec WB-5, step 5.7): the Cmd-K palette's exact-id probe
// (cmdk.go looksLikeBeadID), then /search's semantic search, falling back to
// its lexical one on any semantic failure (search.go handleSearch), all on the
// request's bearer, so the daemon cuts the answer as it cuts the page.
type gardenSearch struct{ s *Server }

// Search implements gardenhub.SearchReader.
func (g gardenSearch) Search(ctx context.Context, q string) (*gardenhub.SearchDoc, error) {
	s := g.s
	doc := &gardenhub.SearchDoc{}
	if looksLikeBeadID(q) {
		if b, err := s.daemon.GetBead(ctx, q); err == nil && b != nil {
			h := gardenSearchHit(b)
			h.Exact = true
			doc.Hits = append(doc.Hits, h)
		} else if err != nil {
			var apiErr *beadsapi.APIError
			if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusNotFound && apiErr.StatusCode != http.StatusForbidden) {
				s.logger.Debug("garden search: id probe failed", "id", q, "error", err)
			}
		}
	}

	sem, err := s.daemon.SemanticSearch(ctx, beadsapi.SemanticSearchRequest{Query: q, Limit: gardenSearchLimit})
	if err == nil && sem != nil {
		doc.Mode = gardenhub.SearchSemantic
		for _, r := range sem.Results {
			if r != nil && r.Bead != nil {
				doc.Hits = append(doc.Hits, gardenSearchHit(r.Bead))
			}
		}
		return doc, nil
	}
	// Semantic search is an enhancement over lexical: any failure degrades to
	// the word search, and the panel says why (as /search's banner does).
	disabled := semanticDisabled(err)
	s.logger.Warn("garden search: semantic search failed; falling back to lexical", "query", q, "error", err, "embedder_disabled", disabled)
	doc.Mode = gardenhub.SearchLexical
	if disabled {
		doc.Note = "Semantic search is off on the beads daemon, so these are word matches only."
	} else {
		doc.Note = "Semantic search did not answer, so these are word matches only."
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Search: q, Limit: gardenSearchLimit})
	if err != nil {
		if len(doc.Hits) == 0 {
			return nil, err
		}
		// the id still answered: show it, and say the rest could not be read
		s.logger.Warn("garden search: lexical search failed", "query", q, "error", err)
		doc.Note = "Only the bead with this id is shown: the beads daemon did not answer the search."
		return doc, nil
	}
	for _, b := range res.Beads {
		if b != nil {
			doc.Hits = append(doc.Hits, gardenSearchHit(b))
		}
	}
	return doc, nil
}

func gardenSearchHit(b *beadsapi.BeadDetail) gardenhub.SearchHit {
	return gardenhub.SearchHit{
		ID: b.ID, Title: b.Title, ShortTitle: b.ShortTitle, Type: b.Type, Status: b.Status,
		Project: projectFromLabels(b.Labels), Priority: b.Priority, Plan: isPlanBead(b),
		Text: snippet(b.Description, 300), Updated: unixSec(b.UpdatedAt),
	}
}
