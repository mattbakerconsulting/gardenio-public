package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/config"
)

// claudeCodeModelAliases maps model names that Claude Code sends internally
// (for subagents and small-model operations) to the LiteLLM model groups
// that correctly route through the Anthropic API with OAuth token forwarding.
//
// Without these aliases, Claude Code's requests for models like "claude-sonnet-4-6"
// would either be rejected by the LiteLLM key ACL (if the name isn't in the
// allowed list) or routed to Bedrock-backed model groups (which don't accept
// OAuth tokens).
//
// The -oauth suffix model groups have forward_client_headers_to_llm_api enabled,
// which forwards the agent's OAuth token to Anthropic's direct API. The
// anthropic/ prefixed groups are pass-through entries for the same purpose.
var claudeCodeModelAliases = map[string]string{
	// Claude Code sends these when using model: "sonnet" in the Agent tool.
	// Since 2.1.257 the harness `sonnet` alias resolves to claude-sonnet-5 (and
	// the Fable consent dialog offers "Switch to Sonnet 5"), which 401'd at the
	// key ACL until this alias + the claude-sonnet-5-oauth group were added
	// (ka-0000004084, ka-0000001501). sonnet-4-6 stays routable for older pins.
	"claude-sonnet-5":            "claude-sonnet-5-oauth",
	"claude-sonnet-5-5":          "claude-sonnet-5-5-oauth",
	"claude-sonnet-4-6":          "claude-sonnet-4-6-oauth",
	"claude-sonnet-4-6-20250514": "claude-sonnet-4-6-oauth",

	// Claude Code sends these when using model: "haiku" (e.g. WebFetch)
	"claude-haiku-4-5":          "anthropic/claude-haiku-4-5",
	"claude-haiku-4-5-20251001": "anthropic/claude-haiku-4-5-20251001",

	// Claude Code sends these when using model: "opus"
	"claude-opus-5-5":          "claude-opus-5-5-oauth",
	"claude-opus-5":            "claude-opus-5-oauth",
	"claude-opus-4-8":          "claude-opus-4-8-oauth",
	"claude-opus-4-7":          "claude-opus-4-7-oauth",
	"claude-opus-4-6":          "claude-opus-4-6-oauth",
	"claude-opus-4-6-20260205": "claude-opus-4-6-oauth",

	// Claude Fable 5.1 (claude-fable-5-1, launched 2026-09-01) — the fleet's
	// HIGH-INTELLIGENCE TIER model, superseding fable-5 everywhere fable was
	// selected (ka-0000002744). Alias-only model ID (no dated snapshot): 1M ctx
	// / 128K out, adaptive thinking always-on, same $10/$50 per MTok as fable-5
	// with cache reads at a quarter of the price. This is what the `fable`
	// selector resolves to: the Slack short alias (bot_mentions_model.go), the
	// harness `fable` alias pin (agents.defaultFableModel), and fable-pinned
	// role/account beads.
	"claude-fable-5-1": "claude-fable-5-1-oauth",

	// Claude Fable 5 (claude-fable-5, launched 2026-06-09). Alias-only model
	// ID (no dated snapshot): 1M ctx / 128K out, thinking always-on. Retained as
	// a routable alias for rollback and for beads still pinning it, but it is no
	// longer any selector's target — fable-5-1 took that role (ka-0000002744).
	// It was never the standing fleet default either: the default account model
	// was flipped back to opus-4-8 (ka-0000004059) after fable was found to be
	// metered/billed, and then to opus-5 in the ka-0000000949 cutover (opus-4-8
	// now outdated). It was briefly the default (ka-0000000133).
	"claude-fable-5": "claude-fable-5-oauth",

	// GLM 5 on AWS Bedrock (zai.glm-5) — epic ka-0000002312; Matt decision
	// ka-0000003230 (company policy forbids OpenRouter/Z.ai managed APIs, so
	// Bedrock is the only policy-compliant GLM path). Selectable in Slack as
	// `--model=glm` (bot_mentions_model.go maps glm -> glm-5). Unlike every entry
	// above, the target is NOT a `-oauth` Anthropic model GROUP but the
	// `bedrock-glm-5` Bedrock model group (litellm.bedrock.models); LiteLLM's
	// /v1/messages unified endpoint translates the agent's Anthropic-format
	// request to the Bedrock InvokeModel call. The alias TARGET only exists when
	// litellm.bedrock.enabled=true (prod: on), which is why the chart's
	// model_group_alias default guards `glm-5: bedrock-glm-5` on that flag — this
	// runtime sync is likewise a no-op when the target group is absent (LiteLLM
	// keeps the alias but no deployment serves it until bedrock is enabled).
	"glm-5": "bedrock-glm-5",
}

// syncLiteLLMModelAliases ensures the LiteLLM proxy has model_group_alias entries
// for Claude Code's internal model names. This prevents subagent/WebFetch requests
// from being rejected or misrouted.
//
// The function reads the LiteLLM admin key from a K8s secret, fetches the current
// router config, and patches in any missing aliases. It is non-fatal: failures are
// logged as warnings but do not prevent controller startup.
func syncLiteLLMModelAliases(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface) {
	if cfg.LiteLLMURL == "" {
		logger.Debug("litellm alias sync: skipped, no LITELLM_URL configured")
		return
	}

	// Resolve the LiteLLM admin key from K8s secret.
	adminKey, err := resolveLiteLLMAdminKey(ctx, k8sClient, cfg.Namespace, cfg.LiteLLMAdminSecret)
	if err != nil {
		logger.Warn("litellm alias sync: cannot resolve admin key",
			"secret", cfg.LiteLLMAdminSecret, "error", err)
		return
	}

	// Fetch current router settings.
	current, err := fetchModelGroupAliases(ctx, cfg.LiteLLMURL, adminKey)
	if err != nil {
		logger.Warn("litellm alias sync: cannot fetch current aliases", "error", err)
		return
	}

	// Compute missing aliases.
	missing := make(map[string]string)
	for name, target := range claudeCodeModelAliases {
		if _, ok := current[name]; !ok {
			missing[name] = target
		}
	}
	if len(missing) == 0 {
		logger.Info("litellm alias sync: all model aliases present",
			"count", len(claudeCodeModelAliases))
		return
	}

	// Merge missing into current.
	merged := make(map[string]string, len(current)+len(missing))
	maps.Copy(merged, current)
	maps.Copy(merged, missing)

	// Update router config.
	if err := updateModelGroupAliases(ctx, cfg.LiteLLMURL, adminKey, merged); err != nil {
		logger.Warn("litellm alias sync: failed to update aliases", "error", err)
		return
	}

	logger.Info("litellm alias sync: added missing model aliases",
		"added", len(missing), "total", len(merged))
	for name, target := range missing {
		logger.Info("litellm alias sync: added alias",
			"model", name, "target", target)
	}
}

func resolveLiteLLMAdminKey(ctx context.Context, k8sClient kubernetes.Interface, namespace, secretName string) (string, error) {
	if secretName == "" {
		return "", fmt.Errorf("no admin secret configured")
	}
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", secretName, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("secret %s has empty token", secretName)
	}
	return token, nil
}

type litellmCallbacksResp struct {
	RouterSettings struct {
		ModelGroupAlias map[string]string `json:"model_group_alias"`
	} `json:"router_settings"`
}

func fetchModelGroupAliases(ctx context.Context, baseURL, adminKey string) (map[string]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/get/config/callbacks"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}

	var result litellmCallbacksResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if result.RouterSettings.ModelGroupAlias == nil {
		return make(map[string]string), nil
	}
	return result.RouterSettings.ModelGroupAlias, nil
}

type litellmConfigUpdate struct {
	RouterSettings struct {
		ModelGroupAlias map[string]string `json:"model_group_alias"`
	} `json:"router_settings"`
}

func updateModelGroupAliases(ctx context.Context, baseURL, adminKey string, aliases map[string]string) error {
	url := strings.TrimRight(baseURL, "/") + "/config/update"

	payload := litellmConfigUpdate{}
	payload.RouterSettings.ModelGroupAlias = aliases

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, respBody)
	}
	return nil
}
