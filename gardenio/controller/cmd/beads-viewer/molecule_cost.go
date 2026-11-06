package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// fmtUSD pre-formats a dollar amount as "$12.34" for the template.
func fmtUSD(v float64) string {
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

// moleculeStepView is one row in the molecule cost card's per-step breakdown.
// Pre-formatted dollar strings keep the template dumb.
type moleculeStepView struct {
	BeadID        string
	Title         string
	SessionUSD    string
	WorkflowUSD   string
	TotalUSD      string
	HasWorkflow   bool
	AgentsSpawned int64
	WorkflowFinal bool
}

// moleculeCostView is the view-model rendered by templates/molecule_cost.html
// and (in raw form) returned by the JSON branch. It mirrors the CLI surface
// (design ka-0000007305 §3.3): the host-session vs workflow_spend split, the
// summed total, the 'partial' flag, and a per-step breakdown.
type moleculeCostView struct {
	MoleculeID string
	Partial    bool

	BeadCount        int
	SessionCount     int
	WorkflowRunCount int
	AgentsSpawned    int64

	TotalUSD    string
	SessionUSD  string
	WorkflowUSD string

	// Combined token pools (host session + workflow_spend), pre-summed.
	InputTokens      int64
	OutputTokens     int64
	ThinkingTokens   int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	Steps []moleculeStepView
}

// wantsJSON reports whether the request asked for a JSON response, via either
// ?format=json or an Accept header that prefers application/json.
func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("format") == "json" {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// handleMoleculeCost renders /molecule/{id}/cost. Default response is the HTML
// cost card; ?format=json (or an application/json Accept) returns the raw
// MoleculeUsage from the rollup walker.
func (s *Server) handleMoleculeCost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "molecule id required", http.StatusBadRequest)
		return
	}

	usage, err := s.daemon.RollupMoleculeUsage(r.Context(), id)
	if err != nil {
		s.logger.Error("molecule cost rollup", "molecule", id, "error", err)
		if wantsJSON(r) {
			http.Error(w, "rollup failed", http.StatusBadGateway)
			return
		}
		s.render(w, r, "molecule_cost.html", map[string]any{
			"MoleculeID": id,
			"Error":      "Failed to roll up molecule cost.",
		})
		return
	}
	if usage == nil {
		usage = &beadsapi.MoleculeUsage{MoleculeID: id}
	}

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(usage); err != nil {
			s.logger.Error("encoding molecule cost json", "molecule", id, "error", err)
		}
		return
	}

	s.render(w, r, "molecule_cost.html", map[string]any{
		"MoleculeID": id,
		"Cost":       buildMoleculeCostView(usage),
	})
}

// buildMoleculeCostView converts a raw MoleculeUsage into the pre-formatted
// view-model. The two token pools are non-overlapping so combined token
// counts are a straight sum (design §3.2).
func buildMoleculeCostView(u *beadsapi.MoleculeUsage) moleculeCostView {
	v := moleculeCostView{
		MoleculeID:       u.MoleculeID,
		Partial:          u.Partial,
		BeadCount:        u.BeadCount,
		SessionCount:     u.SessionCount,
		WorkflowRunCount: u.WorkflowRunCount,
		AgentsSpawned:    u.WorkflowSpend.AgentsSpawned,
		TotalUSD:         fmtUSD(u.TotalCostUSD),
		SessionUSD:       fmtUSD(u.SessionUsage.TotalCostUSD),
		WorkflowUSD:      fmtUSD(u.WorkflowSpend.TotalCostUSD),
		InputTokens:      u.SessionUsage.InputTokens + u.WorkflowSpend.InputTokens,
		OutputTokens:     u.SessionUsage.OutputTokens + u.WorkflowSpend.OutputTokens,
		ThinkingTokens:   u.SessionUsage.ThinkingTokens + u.WorkflowSpend.ThinkingTokens,
		CacheReadTokens:  u.SessionUsage.CacheReadTokens + u.WorkflowSpend.CacheReadTokens,
		CacheWriteTokens: u.SessionUsage.CacheWriteTokens + u.WorkflowSpend.CacheWriteTokens,
	}
	for _, s := range u.Steps {
		// Drop inert beads (molecule root / containers) with no contribution.
		if s.TotalCostUSD == 0 && s.WorkflowSpend == nil && !moleculeStepHasUsage(s) {
			continue
		}
		row := moleculeStepView{
			BeadID:     s.BeadID,
			Title:      s.Title,
			SessionUSD: fmtUSD(s.SessionUsage.TotalCostUSD),
			TotalUSD:   fmtUSD(s.TotalCostUSD),
		}
		if s.WorkflowSpend != nil {
			row.HasWorkflow = true
			row.WorkflowUSD = fmtUSD(s.WorkflowSpend.TotalCostUSD)
			row.AgentsSpawned = s.WorkflowSpend.AgentsSpawned
			row.WorkflowFinal = s.WorkflowSpend.Final
		}
		v.Steps = append(v.Steps, row)
	}
	return v
}

// moleculeStepHasUsage reports whether a step's own session usage carries any
// non-zero signal (mirrors the unexported beadsapi.SessionUsage.hasAny).
func moleculeStepHasUsage(s beadsapi.MoleculeStepUsage) bool {
	u := s.SessionUsage
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.ThinkingTokens != 0 ||
		u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.TotalCostUSD != 0 ||
		u.RequestCount != 0 || u.ToolCallCount != 0 || u.ErrorCount != 0 ||
		u.CompactionCount != 0
}
