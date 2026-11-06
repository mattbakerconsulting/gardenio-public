package spendlogs

import (
	"context"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// accountLister is the slice of beadsapi.Client that proxy enumeration needs;
// narrowed to an interface so tests stub it without a daemon.
type accountLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// ProxyURLs enumerates the distinct LiteLLM base URLs a projection must sweep:
// every non-empty base_url on an open/in-progress account bead, plus the
// controller-global URL. Each request is metered by exactly one proxy, so the
// fleet-true spend for any tag is the sum across this set (mirrors
// distinctProxyURLs in cmd/controller/tag_spend_sync.go, ka-0000003221).
// Account proxy_url values (claudemore fronting proxies) are deliberately NOT
// included — they forward to the same LiteLLM and never meter spend
// themselves; sweeping them would double-count or 404.
//
// A daemon listing error degrades to just the global URL (matching the
// tag-spend-sync fallback) rather than failing: the primary proxy is always
// sweepable, and the caller sees which proxies were actually swept in
// Projection.Proxies.
func ProxyURLs(ctx context.Context, daemon accountLister, globalURL string) []string {
	seen := make(map[string]bool)
	var urls []string
	add := func(raw string) {
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}

	add(globalURL)

	resp, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    100,
	})
	if err == nil && resp != nil {
		for _, bead := range resp.Beads {
			add(bead.Fields["base_url"])
		}
	}
	sort.Strings(urls)
	return urls
}
