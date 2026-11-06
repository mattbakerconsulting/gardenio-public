package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// meTaintsScanLimit caps the owns/created scans backing /me/taints. This is a
// personal-scale page (one viewer's own beads), unlike the fleet-wide
// dashboards, so a generous cap is cheap and avoids silent truncation for any
// realistic operator.
const meTaintsScanLimit = 10000

// meTaintsTaintLookupLimit bounds how many of the viewer's own beads get a
// per-bead GetBeadTaints lookup. Deliberately far below meTaintsScanLimit:
// resolving taints one bead at a time made a full-width (up to 2 x
// meTaintsScanLimit, owns + created) view up to ~20,000 sequential daemon
// round trips per page render (operator review 2026-09-23). 500 mirrors
// batchGetMaxIDs, the daemon's own standard single-request/page bound.
// myTaintedBeads' two facets are each sorted -updated_at, so a viewer past
// this limit still sees taints for their most recently active beads first.
const meTaintsTaintLookupLimit = 500

// meTaintsTaintLookupConcurrency bounds how many GetBeadTaints calls run in
// flight at once, so the (already-bounded) lookup set does not still pay one
// round trip at a time.
const meTaintsTaintLookupConcurrency = 20

// meTaintTaintedBead is one bead row under a taint group on /me/taints.
type meTaintTaintedBead struct {
	ID     string
	Title  string
	Type   string
	Status string
	Href   string
}

// meTaintGroup is one taint's section on /me/taints: the taint name, its
// owner-team accent (mirrors taint_badges.go), and the viewer's own beads
// carrying it.
type meTaintGroup struct {
	Taint       string
	OwnerTeam   string
	AccentColor string
	Beads       []meTaintTaintedBead
}

// meTaintsScrubResult is one row of the "request detaint for all of mine"
// outcome banner: one scrub request filed per distinct taint shown.
type meTaintsScrubResult struct {
	Taint     string
	BeadCount int
	Error     string
}

// handleMeTaints renders /me/taints (docs/adr/taints.md §9.1 "My taints"):
// every taint on any bead the viewer owns or created, grouped by taint, using
// the same read-gate-respecting /v1/beads list path as every other surface —
// scopeListFilter applies the taint read gate server-side regardless of the
// caller-supplied filter, so a viewer never sees a taint grouping for a bead
// they can no longer read. Unauthenticated callers bounce to /login, mirroring
// /me's own redirectMeToViewer behaviour.
func (s *Server) handleMeTaints(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	if email == "" {
		s.redirectToLoginNext(w, r)
		return
	}
	alias := canonicalOIDC(email)

	data := map[string]any{
		"Groups":      []meTaintGroup{},
		"ViewerEmail": email,
	}

	beads, err := s.myTaintedBeads(ctx, alias)
	if err != nil {
		s.logger.Error("me/taints: loading owned/created beads", "alias", alias, "error", err)
		data["Error"] = "Failed to load your beads."
		s.render(w, r, "me_taints.html", data)
		return
	}

	groups := s.groupBeadsByTaint(ctx, beads)
	data["Groups"] = groups
	data["BeadCount"] = len(beads)
	data["TaintLookupTruncated"] = len(beads) > meTaintsTaintLookupLimit
	s.render(w, r, "me_taints.html", data)
}

// handleMeTaintsScrubAll handles POST /me/taints/scrub — "request detaint for
// all of mine" — filing exactly one scrub request per distinct taint
// currently shown, each scoped to the viewer's own beads carrying it
// (docs/adr/taints.md §9.1). Re-renders the same page with a per-taint result
// banner; a failure on one taint's request does not block the others.
func (s *Server) handleMeTaintsScrubAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	if email == "" {
		s.redirectToLoginNext(w, r)
		return
	}
	alias := canonicalOIDC(email)

	data := map[string]any{
		"Groups":      []meTaintGroup{},
		"ViewerEmail": email,
	}

	beads, err := s.myTaintedBeads(ctx, alias)
	if err != nil {
		s.logger.Error("me/taints: scrub-all loading owned/created beads", "alias", alias, "error", err)
		data["Error"] = "Failed to load your beads."
		s.render(w, r, "me_taints.html", data)
		return
	}

	groups := s.groupBeadsByTaint(ctx, beads)
	results := make([]meTaintsScrubResult, 0, len(groups))
	for _, g := range groups {
		ids := make([]string, len(g.Beads))
		for i, b := range g.Beads {
			ids[i] = b.ID
		}
		res := meTaintsScrubResult{Taint: g.Taint, BeadCount: len(ids)}
		if len(ids) == 0 {
			results = append(results, res)
			continue
		}
		if _, err := s.daemon.CreateScrubRequest(ctx, beadsapi.CreateScrubRequestInput{
			Taint:   g.Taint,
			Kind:    "detaint",
			BeadIDs: ids,
		}); err != nil {
			s.logger.Error("me/taints: filing scrub request", "taint", g.Taint, "error", err)
			res.Error = err.Error()
		}
		results = append(results, res)
	}

	data["Groups"] = groups
	data["BeadCount"] = len(beads)
	data["TaintLookupTruncated"] = len(beads) > meTaintsTaintLookupLimit
	data["ScrubResults"] = results
	s.render(w, r, "me_taints.html", data)
}

// myTaintedBeads returns the union of beads the alias OWNS (Owner column
// equals the viewer's canonical alias — a plain string column that defaults
// to the creating operator's alias at create time but is independently
// editable, kbeads auto_owner.go/owner_edit.go) and beads the alias CREATED
// (OperatedByAliases, resolved through the daemon's operator_aliases table so
// every alias the human has ever created under is covered), deduped by id.
//
// Owns is authoritative (its error fails the page, mirroring
// plans_involvement.go's creator-facet precedent); created is best-effort — a
// daemon hiccup on the secondary facet narrows the view rather than erroring
// the whole page.
func (s *Server) myTaintedBeads(ctx context.Context, alias string) ([]*beadsapi.BeadDetail, error) {
	ownRes, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Owner: alias,
		Sort:  "-updated_at",
		Limit: meTaintsScanLimit,
	})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	out := make([]*beadsapi.BeadDetail, 0)
	addAll := func(res *beadsapi.ListBeadsResult) {
		if res == nil {
			return
		}
		for _, b := range res.Beads {
			if b == nil || b.ID == "" {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
		}
	}

	// Defense against rollout skew (operator review 2026-09-23): ?owner= is a
	// query param this daemon just learned. A daemon pod still running the
	// prior build ignores an unknown param rather than rejecting it, so
	// ownRes could silently come back as "every bead visible to the caller"
	// instead of just the viewer's own — which "request detaint for all of
	// mine" would then happily scrub. Re-check Owner client-side so a
	// mixed-version rollout window fails closed (drops the mismatched row)
	// rather than open (files a scrub request against someone else's bead).
	if ownRes != nil {
		ownedOnly := make([]*beadsapi.BeadDetail, 0, len(ownRes.Beads))
		for _, b := range ownRes.Beads {
			if b == nil || b.Owner != alias {
				continue
			}
			ownedOnly = append(ownedOnly, b)
		}
		addAll(&beadsapi.ListBeadsResult{Beads: ownedOnly})
	}

	createdRes, cerr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		OperatedByAliases: []string{alias},
		Sort:              "-updated_at",
		Limit:             meTaintsScanLimit,
	})
	if cerr != nil {
		s.logger.Debug("me/taints: created-by scan", "alias", alias, "error", cerr)
	} else {
		addAll(createdRes)
	}

	return out, nil
}

// groupBeadsByTaint resolves each bead's taints (best-effort, per bead —
// mirrors taint_badges.go's buildTaintBadges) and buckets the viewer's own
// beads by taint name. The owner-team accent lookup (ListTaints) runs once
// for the whole page rather than once per bead.
//
// The per-bead GetBeadTaints lookup is bounded to meTaintsTaintLookupLimit
// beads and fanned out over meTaintsTaintLookupConcurrency workers rather
// than issued one at a time (operator review 2026-09-23: an unbounded
// sequential loop over up to meTaintsScanLimit*2 beads was up to ~20,000
// daemon round trips per page render). Each worker writes only its own
// result-slice index, so no lock is needed; the final grouping pass below
// already re-sorts groups and their bead lists, so it is independent of
// worker completion order.
func (s *Server) groupBeadsByTaint(ctx context.Context, beads []*beadsapi.BeadDetail) []meTaintGroup {
	ownerByName := make(map[string]string)
	registry, err := s.daemon.ListTaints(ctx)
	if err != nil {
		s.logger.Debug("me/taints: listing taint registry", "error", err)
	} else {
		for _, t := range registry {
			ownerByName[t.Name] = t.OwnerTeam
		}
	}

	scanned := beads
	if len(scanned) > meTaintsTaintLookupLimit {
		scanned = scanned[:meTaintsTaintLookupLimit]
	}

	type beadTaints struct {
		bead   *beadsapi.BeadDetail
		taints []beadsapi.BeadTaint
	}
	results := make([]beadTaints, len(scanned))
	sem := make(chan struct{}, meTaintsTaintLookupConcurrency)
	var wg sync.WaitGroup
	for i, b := range scanned {
		if b == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, b *beadsapi.BeadDetail) {
			defer wg.Done()
			defer func() { <-sem }()
			taints, err := s.daemon.GetBeadTaints(ctx, b.ID)
			if err != nil {
				s.logger.Debug("me/taints: getting bead taints", "id", b.ID, "error", err)
				return
			}
			results[i] = beadTaints{bead: b, taints: taints}
		}(i, b)
	}
	wg.Wait()

	byName := make(map[string]*meTaintGroup)
	order := make([]string, 0)
	for _, res := range results {
		b := res.bead
		if b == nil {
			continue
		}
		for _, bt := range res.taints {
			g, ok := byName[bt.TaintName]
			if !ok {
				owner := ownerByName[bt.TaintName]
				g = &meTaintGroup{
					Taint:       bt.TaintName,
					OwnerTeam:   owner,
					AccentColor: ownerInitialsHue(owner),
				}
				byName[bt.TaintName] = g
				order = append(order, bt.TaintName)
			}
			g.Beads = append(g.Beads, meTaintTaintedBead{
				ID:     b.ID,
				Title:  b.Title,
				Type:   b.Type,
				Status: b.Status,
				Href:   s.basePath + "/" + b.ID,
			})
		}
	}

	sort.Strings(order)
	out := make([]meTaintGroup, 0, len(order))
	for _, name := range order {
		g := byName[name]
		sort.Slice(g.Beads, func(i, j int) bool { return g.Beads[i].ID < g.Beads[j].ID })
		out = append(out, *g)
	}
	return out
}

// redirectToLoginNext bounces an unauthenticated caller to /login, preserving
// the original path + query so the post-login hop returns them here. Mirrors
// redirectMeToViewer's unauthenticated branch (user_home.go).
func (s *Server) redirectToLoginNext(w http.ResponseWriter, r *http.Request) {
	next := s.basePath + r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, s.basePath+"/login?next="+url.QueryEscape(next), http.StatusFound)
}
