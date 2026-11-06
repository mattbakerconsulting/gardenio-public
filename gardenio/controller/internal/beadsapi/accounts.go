package beadsapi

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

// AccountInfo represents a credential profile from a type:account config bead.
// Projects reference an account by name to inherit provider, base URL, model,
// and LiteLLM virtual key settings for LLM API routing.
type AccountInfo struct {
	Name     string // Account name (from bead title)
	Provider string // Provider name (e.g. "anthropic", "openai")
	BaseURL  string // API endpoint override (e.g. LiteLLM proxy URL)
	// ProxyURL, when set, is the URL agents are pointed at via ANTHROPIC_BASE_URL
	// INSTEAD of BaseURL — e.g. the claudemore record/replay proxy that fronts
	// litellm. Empty (the default) keeps the legacy behavior of routing straight
	// to BaseURL/litellm. This single field is the claudemore instant-rollback
	// lever (ka-0000007035): clearing it reverts the next pod spawn to litellm
	// within ~60s, no code change or redeploy.
	ProxyURL   string
	Credential string            // Token source: "coopmux:<account>", "secret:<k8s-secret>", "env:<var>"
	Model      string            // Default model for agents using this account
	MaxBudget  string            // Per-session budget hint (informational)
	Fallback   string            // Fallback account name when budget is exhausted
	LiteLLMKey string            // LiteLLM virtual key for budget tracking
	Status     string            // Rotation lifecycle: "active" (default), "exhausted", "cooldown"
	Labels     []string          // Raw bead labels (e.g. "project:<name>" authorizes this account for the project)
	Fields     map[string]string // All raw bead fields (for per-account config like soft_failover_threshold)
}

// ListAccountBeads queries the daemon for account beads (type=account) and returns
// a map of account name -> AccountInfo. Used by the controller to resolve project
// account references at pod creation time.
func (c *Client) ListAccountBeads(ctx context.Context) (map[string]AccountInfo, error) {
	resp, err := c.listBeads(ctx, []string{"account"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing account beads: %w", err)
	}

	accounts := make(map[string]AccountInfo)
	for _, b := range resp.Beads {
		fields := b.fieldsMap()
		name := b.Title
		if name == "" {
			continue
		}
		accounts[name] = AccountInfo{
			Name:       name,
			Provider:   fields["provider"],
			BaseURL:    fields["base_url"],
			ProxyURL:   fields["proxy_url"],
			Credential: fields["credential"],
			Model:      fields["model"],
			MaxBudget:  fields["max_budget"],
			Fallback:   fields["fallback"],
			LiteLLMKey: fields["litellm_key"],
			Status:     fields["status"],
			Labels:     b.Labels,
			Fields:     fields,
		}
	}
	return accounts, nil
}

// ListAccountBeadsByProject returns account beads authorized for the given
// project — i.e. type=account beads labeled with project:<name> and whose
// Status field is "active" (or empty, which means active by default).
//
// Results are sorted by (primary marker desc, name asc): accounts whose
// "primary" field equals "true" appear first, then the rest alphabetically.
// This keeps the alphabetically-first account stable as the fallback ordering
// when no operator has set a primary marker, while letting an operator pin a
// specific account as the project default.
//
// An account with multiple project:<name> labels appears for each of those
// projects. An account with no project:* labels is a global/fallback account
// and is NOT returned by this function — callers that want the global pool
// should use ListAccountBeads and filter as needed.
func (c *Client) ListAccountBeadsByProject(ctx context.Context, project string) ([]AccountInfo, error) {
	if project == "" {
		return nil, fmt.Errorf("project must be non-empty")
	}
	resp, err := c.listBeads(ctx, []string{"account"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing account beads: %w", err)
	}

	wantLabel := "project:" + project
	var out []AccountInfo
	for _, b := range resp.Beads {
		name := b.Title
		if name == "" {
			continue
		}
		if !slices.Contains(b.Labels, wantLabel) {
			continue
		}
		fields := b.fieldsMap()
		status := fields["status"]
		if status != "" && status != "active" {
			continue
		}
		out = append(out, AccountInfo{
			Name:       name,
			Provider:   fields["provider"],
			BaseURL:    fields["base_url"],
			ProxyURL:   fields["proxy_url"],
			Credential: fields["credential"],
			Model:      fields["model"],
			MaxBudget:  fields["max_budget"],
			Fallback:   fields["fallback"],
			LiteLLMKey: fields["litellm_key"],
			Status:     status,
			Labels:     b.Labels,
			Fields:     fields,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ip := out[i].Fields["primary"] == "true"
		jp := out[j].Fields["primary"] == "true"
		if ip != jp {
			return ip
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
