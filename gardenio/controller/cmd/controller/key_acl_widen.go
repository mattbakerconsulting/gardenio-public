package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// key_acl_widen.go — widens an account's virtual-key model allow-list to cover
// every served Anthropic chat model group, so a newly-deployed model group
// (e.g. opus-5) does not 401 "Tried to access <group>" for keys minted before
// it existed. Restarting a pod cannot widen a key on its own — only a fresh
// mint or an explicit /key/update can — so this closes that gap without
// requiring a human to hand-edit key ACLs (see the comment in
// gardenio/scripts/litellm-stage2-migrate.sh:92-102 for the incident this
// followed).
//
// Widening only ever ADDS entries — it must never narrow a key, because an
// empty models list means "unrestricted" in LiteLLM, and writing a list back
// would silently take that away.

// keyACLWidener periodically scans account beads and widens each one's LiteLLM
// virtual-key model allow-list to the served Anthropic chat groups. It only
// talks to LiteLLM; it never writes an account bead. The dormant POC 30-day
// key rotation that used to share this loop was deleted (gd-OLYrS5E6F8); see
// gardenio/docs/litellm-key-rotation.md.
type keyACLWidener struct {
	daemon     *beadsapi.Client
	litellmURL string
	masterKey  string
	httpClient *http.Client
	logger     *slog.Logger
	interval   time.Duration
}

func newKeyACLWidener(daemon *beadsapi.Client, litellmURL, masterKey string, interval time.Duration, logger *slog.Logger) *keyACLWidener {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return &keyACLWidener{
		daemon:     daemon,
		litellmURL: strings.TrimRight(litellmURL, "/"),
		masterKey:  masterKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		logger:     logger.With("component", "key-acl"),
		interval:   interval,
	}
}

// RunLoop widens key ACLs on startup and then every interval until ctx is
// cancelled.
func (k *keyACLWidener) RunLoop(ctx context.Context) {
	k.logger.Info("key acl widen loop started", "interval", k.interval)
	k.reconcile(ctx)

	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			k.reconcile(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (k *keyACLWidener) reconcile(ctx context.Context) {
	resp, err := k.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    50,
	})
	if err != nil {
		k.logger.Warn("key acl: failed to list accounts", "error", err)
		return
	}

	// Fetch the served-model set once per pass (not once per account) so a
	// single LiteLLM read backs every key's widen check this cycle.
	served, err := fetchServedModelIDs(ctx, k.httpClient, k.litellmURL, k.masterKey)
	switch {
	case err != nil:
		k.logger.Warn("key acl: served-model fetch failed, skipping widen this pass", "error", err)
		return
	case len(served) == 0:
		k.logger.Warn("key acl: served-model fetch returned no models, skipping widen this pass")
		return
	}

	for _, bead := range resp.Beads {
		key := bead.Fields["litellm_key"]
		if key == "" {
			continue
		}
		if err := k.widenKeyACL(ctx, bead.Title, key, served); err != nil {
			k.logger.Warn("key acl: widen failed", "account", bead.Title, "bead", bead.ID, "error", err)
		}
	}
}

// isAnthropicChatModelGroup reports whether a served model id names an
// Anthropic chat model group safe to add to an OAuth key's ACL: the -oauth
// suffixed groups (forward_client_headers_to_llm_api) and the anthropic/
// prefixed pass-through groups. This excludes Bedrock and embedding groups
// (e.g. bedrock-glm-5) from leaking onto keys that should only ever reach
// Anthropic's direct API.
func isAnthropicChatModelGroup(name string) bool {
	return strings.HasSuffix(name, "-oauth") || strings.HasPrefix(name, "anthropic/")
}

// widenModels computes the widened model allow-list for a key. current is the
// key's existing models (from /key/info); served is the full served-model set
// (from /v1/models), filtered here to Anthropic chat groups only. merged is
// always a superset of current — entries are only ever added, never removed —
// and changed reports whether anything new was actually added.
//
// An empty current means the key is unrestricted (LiteLLM's "no models" =
// "all models" convention); that must never be narrowed by writing a list, so
// an empty current always returns unchanged.
func widenModels(current, served []string) (merged []string, changed bool) {
	if len(current) == 0 {
		return nil, false
	}

	set := make(map[string]bool, len(current))
	for _, m := range current {
		set[m] = true
	}

	added := false
	for _, s := range served {
		if !isAnthropicChatModelGroup(s) {
			continue
		}
		if !set[s] {
			set[s] = true
			added = true
		}
	}
	if !added {
		return current, false
	}

	merged = make([]string, 0, len(set))
	for m := range set {
		merged = append(merged, m)
	}
	sort.Strings(merged)
	return merged, true
}

// addedModelNames returns the entries present in merged but not in current,
// sorted — used only to make the "key acl: widened" log line legible.
func addedModelNames(current, merged []string) []string {
	set := make(map[string]bool, len(current))
	for _, m := range current {
		set[m] = true
	}
	var added []string
	for _, m := range merged {
		if !set[m] {
			added = append(added, m)
		}
	}
	sort.Strings(added)
	return added
}

// widenKeyACL reads key's current model allow-list, widens it against served,
// and — only when something was actually added — POSTs the merged list to
// LiteLLM's /key/update. No request is sent when nothing changed.
func (k *keyACLWidener) widenKeyACL(ctx context.Context, accountTitle, key string, served []string) error {
	current, err := k.queryKeyModels(ctx, key)
	if err != nil {
		return fmt.Errorf("querying key models: %w", err)
	}

	merged, changed := widenModels(current, served)
	if !changed {
		return nil
	}

	body, err := json.Marshal(map[string]any{"key": key, "models": merged})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.litellmURL+"/key/update", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.masterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /key/update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 120))
	}

	k.logger.Info("key acl: widened", "account", accountTitle, "added", addedModelNames(current, merged))
	return nil
}

// queryKeyModels reads the model allow-list of an existing virtual key.
func (k *keyACLWidener) queryKeyModels(ctx context.Context, virtualKey string) ([]string, error) {
	u := k.litellmURL + "/key/info?key=" + url.QueryEscape(virtualKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.masterKey)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		// A transport error's text embeds the request URL, which carries the
		// virtual key in its query string; keep only the underlying cause so
		// callers can log the error without leaking the key.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, fmt.Errorf("GET /key/info: %s", redactKey(uerr.Err.Error(), virtualKey))
		}
		return nil, fmt.Errorf("GET /key/info: %s", redactKey(err.Error(), virtualKey))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(redactKey(string(body), virtualKey), 120))
	}

	var result struct {
		Info struct {
			Models []string `json:"models"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Info.Models, nil
}

// redactKey replaces every occurrence of key in s so an error or response body
// that echoes a virtual key can be logged safely.
func redactKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}
