package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// ConfigEntry represents a config key/value from the daemon's legacy KV store.
type ConfigEntry struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// ListConfigs returns all config entries in the given namespace from the
// legacy KV store.
//
// Deprecated: KV config is superseded by label-based config beads.
// Retained temporarily for gb config migrate.
func (c *Client) ListConfigs(ctx context.Context, namespace string) ([]ConfigEntry, error) {
	q := url.Values{}
	if namespace != "" {
		q.Set("namespace", namespace)
	}
	path := "/v1/configs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		Configs []ConfigEntry `json:"configs"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing configs: %w", err)
	}
	return resp.Configs, nil
}
