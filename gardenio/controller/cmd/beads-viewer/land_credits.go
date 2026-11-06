package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"gardenio/controller/internal/gardenhub"
)

// landCreditSource feeds a tenant land's credit charts (GET /garden/charts/costs-land, gardenhub/land_credits.go; plan
// gd-gh5J0vVbJP step 3.3) from the 3.2 recording rules in vmsingle (garden-grafana-panels.md, "Credit series (3.2)").
// It reads s.metricsUpstream directly, never the admin-only /api/v1/query_range proxy. The PromQL is fixed: the only
// value that reaches it is the land, which gardenhub has validated as a DNS label and checked against the viewer's
// scope, as an exact matcher (land="<name>", never =~), so nothing can widen the selector to another land. Every
// series it names is in credits.
type landCreditSource struct{ s *Server }

const (
	landSpendCreditsPrefix = "gardenio:land_spend_credits:"
	// landCreditsInstantNames are the instant series, read in one query: the three tiles, the burn rate and per agent.
	landCreditsInstantNames = "today|increase7d|increase30d|rate1d|by_agent"
	// landCreditsRemaining is the burn line's series, read over the window.
	landCreditsRemaining = "gardenio:land_credits:remaining"
)

// landCreditsMatcher is the land's exact matcher.
func landCreditsMatcher(land string) string { return `land="` + escapePromQLLabelValue(land) + `"` }

// buildLandCreditsInstantQuery selects the land's instant credit series by name; __name__ rides on each sample.
func buildLandCreditsInstantQuery(land string) string {
	return `{__name__=~"` + landSpendCreditsPrefix + `(` + landCreditsInstantNames + `)",` + landCreditsMatcher(land) + `}`
}

// buildLandCreditsRemainingQuery is the land's settled remaining, for the range query.
func buildLandCreditsRemainingQuery(land string) string {
	return landCreditsRemaining + `{` + landCreditsMatcher(land) + `}`
}

// vmMatrixResponse is the subset of vmsingle's /api/v1/query_range JSON for a "matrix" result.
type vmMatrixResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// rangeMatrixQuery is instantVectorQuery's range sibling: one query over [start, end] every step, through the same
// timeout, body cap and failure handling. The bool is false with no upstream, a failed read or a status other than
// success.
func (s *Server) rangeMatrixQuery(ctx context.Context, query string, w gardenhub.CreditWindow) (*vmMatrixResponse, bool) {
	if s.metricsUpstream == "" {
		return nil, false
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", strconv.FormatInt(w.Start.Unix(), 10))
	v.Set("end", strconv.FormatInt(w.End.Unix(), 10))
	v.Set("step", strconv.FormatInt(int64(w.Step.Seconds()), 10)+"s")
	var parsed vmMatrixResponse
	if !s.vmGetJSON(ctx, s.metricsUpstream+vmsingleQueryRangePath+"?"+v.Encode(), "land credits", &parsed) || parsed.Status != "success" {
		return nil, false
	}
	return &parsed, true
}

// LandCredits implements gardenhub.LandCreditSource. Recorded is true when any of the land's series answered a sample.
func (c landCreditSource) LandCredits(ctx context.Context, land string, w gardenhub.CreditWindow) (gardenhub.CreditHistory, error) {
	var h gardenhub.CreditHistory
	if c.s.metricsUpstream == "" {
		return h, gardenhub.ErrCreditsUnavailable
	}
	inst, ok := c.s.instantVectorQuery(ctx, buildLandCreditsInstantQuery(land))
	if !ok {
		return h, gardenhub.ErrCreditsUnavailable
	}
	rem, ok := c.s.rangeMatrixQuery(ctx, buildLandCreditsRemainingQuery(land), w)
	if !ok {
		return h, gardenhub.ErrCreditsUnavailable
	}
	for _, r := range inst.Data.Result {
		if r.Metric["land"] != land {
			continue
		}
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		h.Recorded = true
		switch strings.TrimPrefix(r.Metric["__name__"], landSpendCreditsPrefix) {
		case "today":
			h.Today = &v
		case "increase7d":
			h.Week = &v
		case "increase30d":
			h.Month = &v
		case "rate1d":
			h.RatePerHour = &v
		case "by_agent":
			if a := r.Metric["agent"]; a != "" {
				h.Agents = append(h.Agents, gardenhub.AgentCredits{Agent: a, Credits: v})
			}
		}
	}
	for _, r := range rem.Data.Result {
		if r.Metric["land"] != land {
			continue
		}
		for _, pt := range r.Values {
			var ts float64
			if json.Unmarshal(pt[0], &ts) != nil {
				continue
			}
			v, ok := parseVMSampleValue(pt[1])
			if !ok {
				continue
			}
			h.Recorded = true
			h.Remaining = append(h.Remaining, gardenhub.CreditPoint{ts, v})
		}
		break // one land, one series
	}
	return h, nil
}
