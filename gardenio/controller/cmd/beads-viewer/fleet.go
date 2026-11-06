package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Sentinel byline indexes. Per design doc §7.6 (ka-0000003587 / M3): writes
// from `system` / `fleet` / bridge operators render as bylines linking to
// `/fleet/<bridge>` rather than appearing as authoritative authors. This
// surface lists known bridge sentinels and, for a chosen bridge, the recent
// beads it operated.

// fleetBridgeListLimit caps the operators-roster scan when building the
// /fleet index. The sentinel set is small in practice (single-digit bridges);
// the cap keeps the index bounded even if a future migration mints more.
const fleetBridgeListLimit = 200

// fleetBridgeBeadsLimit caps the per-bridge recent-beads list rendered on
// /fleet/<bridge>. Bridges write at relatively high volume (notification
// fan-out), so this is tighter than the homepage page-size.
const fleetBridgeBeadsLimit = 50

// fleetSentinelKinds is the set of operator kinds that surface as `/fleet`
// entries. Mirrors design doc §7.6 — `system` and `fleet` are the canonical
// kinds; bridge accounts carry kind=`fleet` per the operators schema.
var fleetSentinelKinds = map[string]struct{}{
	"system": {},
	"fleet":  {},
}

// fleetBridgeRow is one entry in the /fleet roster index.
type fleetBridgeRow struct {
	Slug    string
	Kind    string
	Display string
	Href    string
}

// fleetBeadRow is one bead row rendered on /fleet/<bridge>.
type fleetBeadRow struct {
	ID       string
	Title    string
	Type     string
	Status   string
	Priority int
	Project  string
	Ago      string
}

func (s *Server) handleFleetIndex(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"ViewerEmail": email,
		"Rows":        []fleetBridgeRow{},
	}

	if s.daemon == nil {
		s.render(w, r, "fleet_index.html", data)
		return
	}
	rows, err := s.listFleetBridges(r.Context())
	if err != nil {
		s.logger.Warn("fleet index: list bridges failed", "error", err)
		data["Error"] = "Failed to load fleet sentinels."
	}
	data["Rows"] = rows
	s.render(w, r, "fleet_index.html", data)
}

func (s *Server) handleFleetBridge(w http.ResponseWriter, r *http.Request) {
	bridge := strings.TrimSpace(r.PathValue("bridge"))
	if bridge == "" {
		http.NotFound(w, r)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"ViewerEmail": email,
		"Bridge":      bridge,
		"Kind":        "",
		"Display":     "",
		"Beads":       []fleetBeadRow{},
	}
	if s.daemon == nil {
		http.NotFound(w, r)
		return
	}

	// Resolve slug → (id, kind, display) via the operator directory in a
	// single shot. LookupOperatorBySlug returns only the id, so we lean on
	// ListOperators (already used by the index) to also enforce the
	// sentinel-kind gate.
	listResp, err := s.daemon.ListOperators(r.Context(), fleetBridgeListLimit)
	if err != nil {
		s.logger.Error("fleet bridge: list operators failed", "slug", bridge, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var entry *beadsapi.OperatorDirectoryEntry
	if listResp != nil {
		for i := range listResp.Operators {
			op := &listResp.Operators[i]
			if strings.EqualFold(strings.TrimSpace(op.Slug), bridge) {
				entry = op
				break
			}
		}
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(entry.Kind))
	if _, ok := fleetSentinelKinds[kind]; !ok {
		// Not a sentinel — surface a 404 rather than rendering a sentinel
		// index for a real person. The /u/{slug} route is the correct
		// surface for human operators.
		http.NotFound(w, r)
		return
	}
	data["Kind"] = kind
	data["Display"] = entry.Display

	beads, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		OperatedByID: entry.ID,
		Statuses:     []string{"open", "in_progress", "closed"},
		Limit:        fleetBridgeBeadsLimit,
		Sort:         "updated_at",
	})
	if err != nil {
		s.logger.Warn("fleet bridge: list beads failed", "slug", bridge, "error", err)
		data["Error"] = "Failed to load sentinel activity."
		s.render(w, r, "fleet_bridge.html", data)
		return
	}

	now := time.Now()
	rows := make([]fleetBeadRow, 0, len(beads.Beads))
	for _, b := range beads.Beads {
		if b == nil {
			continue
		}
		rows = append(rows, fleetBeadRow{
			ID:       b.ID,
			Title:    b.Title,
			Type:     b.Type,
			Status:   b.Status,
			Priority: b.Priority,
			Project:  projectFromLabels(b.Labels),
			Ago:      fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	data["Beads"] = rows
	s.render(w, r, "fleet_bridge.html", data)
}

// listFleetBridges walks the operator roster, filters down to sentinel kinds,
// and projects each into a fleetBridgeRow for the /fleet index. Sorting is
// kind-then-slug asc so `fleet` bridges (the higher-traffic sentinels) cluster
// together and the order is stable across requests.
func (s *Server) listFleetBridges(ctx context.Context) ([]fleetBridgeRow, error) {
	resp, err := s.daemon.ListOperators(ctx, fleetBridgeListLimit)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]fleetBridgeRow, 0, len(resp.Operators))
	for _, op := range resp.Operators {
		kind := strings.ToLower(strings.TrimSpace(op.Kind))
		if _, ok := fleetSentinelKinds[kind]; !ok {
			continue
		}
		slug := strings.TrimSpace(op.Slug)
		if slug == "" {
			continue
		}
		out = append(out, fleetBridgeRow{
			Slug:    slug,
			Kind:    kind,
			Display: op.Display,
			Href:    s.basePath + "/fleet/" + slug,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}
