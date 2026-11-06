package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/config"
)

// served_model_sync.go — periodically fetches the set of models LiteLLM actually
// serves and publishes it to cfg.ServedModels, so the spawn-time spread selector
// can refuse to bake an account whose model is NOT served as agent cognition
// (ka-0000002790).
//
// Why this exists: an account bead can carry a valid LiteLLM key and a
// project:<name> label yet name a model that LiteLLM does not (yet) serve — a
// TARGET model staged for a future smoke-test (the glm-bedrock account:
// model=bedrock-glm-5, keyed + labeled project:glm-lab, but the bedrock model
// group was not deployed). selectAccount treated it as a healthy cognition
// candidate, picked it as lowest-load, and podspec baked ANTHROPIC_MODEL/
// CLAUDE_MODEL=bedrock-glm-5 as the agent's OWN brain. Every first inference turn
// then failed model-not-found → 0 tokens → SIGTERM crash-loop → synthesized empty
// wrap-up. The served-model set is the authoritative "what can an agent actually
// call" signal the labels/keys alone cannot express.

// servedModelRefreshInterval is how often the served-model set is refreshed from
// LiteLLM. Model deployments ride release tags (infrequent), so a coarse interval
// is fine; it only needs to converge well within a release cycle. Kept in step
// with the spend-sync cadence (spend_sync.go).
const servedModelRefreshInterval = 5 * time.Minute

// servedModelSync fetches LiteLLM's served-model set (the union of /v1/models
// group ids and model_group_alias keys) and republishes it to cfg on an
// interval. It is non-fatal by construction: any fetch error leaves the previous
// published set in place (or the UNKNOWN nil state if none was ever published),
// so the spread gate stays fail-open and a transient LiteLLM blip never strips
// accounts from the pool.
type servedModelSync struct {
	cfg        *config.Config
	litellmURL string
	masterKey  string
	httpClient *http.Client
	logger     *slog.Logger
}

func newServedModelSync(cfg *config.Config, litellmURL, masterKey string, logger *slog.Logger) *servedModelSync {
	return &servedModelSync{
		cfg:        cfg,
		litellmURL: strings.TrimRight(litellmURL, "/"),
		masterKey:  masterKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger.With("component", "served-model-sync"),
	}
}

// RunLoop refreshes the served-model set immediately, then every
// servedModelRefreshInterval until ctx is cancelled.
func (s *servedModelSync) RunLoop(ctx context.Context) {
	s.refresh(ctx)

	ticker := time.NewTicker(servedModelRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.refresh(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// refresh fetches the current served-model set and, if non-empty, publishes it
// to cfg. An empty or errored fetch is deliberately NOT published: collapsing
// the set to known-empty would make the spread gate exclude every account, the
// exact fleet-wide failure the fail-open contract exists to prevent. On a
// successful publish it also warns about any active, project-labeled account
// whose baked model is unserved — the loud signal the glm-bedrock class lacked.
func (s *servedModelSync) refresh(ctx context.Context) {
	served, err := s.fetchServedModels(ctx)
	if err != nil {
		s.logger.Warn("served-model sync: fetch failed, keeping previous set", "error", err)
		return
	}
	if len(served) == 0 {
		// A reachable LiteLLM that serves zero models is almost certainly a
		// misread (auth/shape change), not ground truth. Do not publish an empty
		// set — that would gate every account. Leave the previous set in place.
		s.logger.Warn("served-model sync: LiteLLM returned no served models, keeping previous set")
		return
	}
	s.cfg.SetServedModels(served)
	s.logger.Info("served-model sync: refreshed served-model set", "count", len(served))
	s.warnUnservedProjectAccounts(served)
}

// warnUnservedProjectAccounts logs a WARN for every active, project-labeled,
// keyed account whose baked model is NOT in the served set and which routes
// through our LiteLLM — i.e. exactly the accounts the spread gate now excludes.
// This surfaces the misconfiguration loudly (the bug's third ask) instead of it
// manifesting only as blank crash-loop wrap-ups. Observability only; resolution
// is unchanged.
func (s *servedModelSync) warnUnservedProjectAccounts(served map[string]bool) {
	s.cfg.ProjectCacheMu.RLock()
	accounts := s.cfg.AccountCache
	s.cfg.ProjectCacheMu.RUnlock()

	var flagged []string
	for name, acct := range accounts {
		if acct.Status != "" && acct.Status != "active" {
			continue
		}
		if !accountHasProjectLabel(acct.Labels) {
			continue
		}
		if accountModelUnserved(acct, served, s.litellmURL) {
			flagged = append(flagged, fmt.Sprintf("%s(model=%s)", name, acct.Model))
		}
	}
	if len(flagged) == 0 {
		return
	}
	sort.Strings(flagged)
	s.logger.Warn("served-model sync: active project account(s) name a model LiteLLM does not serve — excluded from the spread cognition pool so they cannot crash-loop agents (set exclusive_to_role or drop the project label until the model is deployed)",
		"accounts", strings.Join(flagged, ", "))
}

// litellmModelsResponse is the /v1/models response shape (OpenAI-compatible):
// {"data":[{"id":"claude-opus-4-8-oauth",...},...]}.
type litellmModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// fetchServedModels returns the set of model names LiteLLM currently serves: the
// union of the /v1/models group ids and the model_group_alias keys. Both are
// names an agent can put in ANTHROPIC_MODEL and have LiteLLM route successfully,
// which is exactly what an account's baked model must be. The alias keys are a
// best-effort union — a failure to read them is non-fatal as long as /v1/models
// succeeded (the group ids are the primary signal; the fleet bakes literal
// group names, not aliases).
func (s *servedModelSync) fetchServedModels(ctx context.Context) (map[string]bool, error) {
	models, err := s.fetchModelIDs(ctx)
	if err != nil {
		return nil, err
	}
	served := make(map[string]bool, len(models))
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			served[m] = true
		}
	}
	// Best-effort union with model_group_alias keys (reuses the alias fetch from
	// litellm_aliases.go with the master key, which carries admin privileges).
	if aliases, err := fetchModelGroupAliases(ctx, s.litellmURL, s.masterKey); err != nil {
		s.logger.Debug("served-model sync: alias union skipped", "error", err)
	} else {
		for name := range aliases {
			if name = strings.TrimSpace(name); name != "" {
				served[name] = true
			}
		}
	}
	return served, nil
}

// fetchModelIDs GETs /v1/models and returns the served model group ids.
func (s *servedModelSync) fetchModelIDs(ctx context.Context) ([]string, error) {
	return fetchServedModelIDs(ctx, s.httpClient, s.litellmURL, s.masterKey)
}

// fetchServedModelIDs GETs /v1/models with the master key and returns the
// served model group ids. Shared by servedModelSync (fleet-wide cognition
// gating) and keyACLWidener (per-key ACL widening, key_acl_widen.go) so both read
// LiteLLM's served-model set the same way.
func fetchServedModelIDs(ctx context.Context, httpClient *http.Client, baseURL, masterKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result litellmModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding /v1/models response: %w", err)
	}
	ids := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// accountHasProjectLabel reports whether any of the account's labels is a
// "project:<name>" label — the marker that makes it a spread-pool candidate.
func accountHasProjectLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, "project:") {
			return true
		}
	}
	return false
}
