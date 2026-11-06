package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// litellm_key.go is the LiteLLM virtual-key WRITE path — the mint/update/delete
// counterpart to the read-only /key/info path in usage.go (queryLiteLLMKeyInfo,
// liteLLMKeyInfo). It closes the R4-COST-1 / R4-SEC-1 gap the claudemore
// de-risking flagged (gardenio/docs/research/claudemore-r4-derisking.md; epic
// ka-0000000278; task ka-0000000823): "MaxBudget informational-only, no
// /key/generate|/key/update write path exists anywhere in gardenio/kbeads/coop."
//
// The controller only ever READS keys (spend_sync.go GET /key/info) and never
// mints them, so an ephemeral CI run had no way to obtain a spend-capped,
// revocable virtual key. This command wires the three LiteLLM admin verbs that
// give a per-CI run a scoped, budget-capped, revocable key:
//
//   - mint   (POST /key/generate) — at namespace-create, mint a key scoped to a
//            per-run max_budget + budget_duration (+ optional model allowlist).
//            LiteLLM then hard-429s the key the moment cumulative spend crosses
//            max_budget, which is the structural guardrail (R4-COST-1): the worst
//            case is bounded regardless of agent behavior, without waiting on the
//            5-min spend reconcile.
//   - update (POST /key/update) — adjust the budget on an existing key.
//   - delete (POST /key/delete) — at reap, revoke the key so a leaked tracking
//            credential is bounded and short-lived (R4-SEC-1).
//
// It reuses the same URL / master-key resolution (LITELLM_URL /
// LITELLM_MASTER_KEY) and the liteLLMKeyInfo read-path struct as usage.go — the
// master key stays controller/operator-side and MUST never enter the CI agent
// path (R4-SEC: "keep the master key controller-side only").

// litellmKeyGenRequest is the POST /key/generate body. Only non-zero fields are
// serialized so a mint sends exactly the scoping the caller asked for and
// LiteLLM applies its own defaults for the rest.
//
// MaxBudget is a value (not a pointer) because a mint without a budget defeats
// the entire purpose of this command — runFn enforces max_budget > 0 before
// calling, so a zero here is never marshaled as an intentional "$0 budget".
type litellmKeyGenRequest struct {
	KeyAlias       string            `json:"key_alias,omitempty"`
	MaxBudget      float64           `json:"max_budget"`
	BudgetDuration string            `json:"budget_duration,omitempty"`
	Models         []string          `json:"models,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// litellmKeyMutateResponse is the flat object LiteLLM returns from
// /key/generate and /key/update. Unlike /key/info (which nests the object under
// `info`), these endpoints return the key fields at the top level, so this is a
// distinct shape from liteLLMKeyInfoResponse — it deliberately mirrors the
// read-path liteLLMKeyInfo field set so callers get a consistent view.
type litellmKeyMutateResponse struct {
	Key            string   `json:"key"`
	KeyAlias       string   `json:"key_alias"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
	BudgetResetAt  string   `json:"budget_reset_at"`
	Spend          float64  `json:"spend"`
	Models         []string `json:"models"`
}

// litellmKeyUpdateRequest is the POST /key/update body. Key is required;
// MaxBudget / Spend are pointers so "not provided" (leave unchanged) is
// distinguishable from an explicit 0 (block the key / reset spend to zero).
type litellmKeyUpdateRequest struct {
	Key            string   `json:"key"`
	MaxBudget      *float64 `json:"max_budget,omitempty"`
	BudgetDuration string   `json:"budget_duration,omitempty"`
	Spend          *float64 `json:"spend,omitempty"`
	Models         []string `json:"models,omitempty"`
}

var litellmKeyCmd = &cobra.Command{
	Use:   "litellm-key",
	Short: "Mint / update / delete LiteLLM virtual keys (spend-capped, revocable)",
	Long: `Manage LiteLLM virtual keys — the WRITE counterpart to the read-only
` + "`gb usage`" + ` / ` + "`gb account status`" + ` path (GET /key/info).

Minting a key with --max-budget makes LiteLLM hard-429 that key once its
cumulative spend crosses the budget, so a per-CI run is bounded regardless of
agent behavior (R4-COST-1). Deleting the key at teardown makes a leaked tracking
credential short-lived and revocable (R4-SEC-1). See
gardenio/docs/research/claudemore-r4-derisking.md and
gardenio/docs/claudemore-per-ci-budget-key.md.

Lifecycle for a per-CI scoped key:
  # at namespace-create — mint a $5/24h key scoped to Haiku:
  gb litellm-key mint --alias ci-<instance-id> --max-budget 5 \
      --budget-duration 24h --models claude-haiku-4-5

  # at reap/teardown — revoke it:
  gb litellm-key delete --key sk-...

Authenticates to LiteLLM with the master/admin key (--litellm-key /
LITELLM_MASTER_KEY). The master key is a controller/operator credential and MUST
NOT be injected into the CI agent path — mint with it out-of-band and inject only
the resulting scoped virtual key.`,
	GroupID: "orchestration",
}

var litellmKeyMintCmd = &cobra.Command{
	Use:   "mint",
	Short: "Mint a scoped virtual key with a max_budget (POST /key/generate)",
	Long: `Mints a new LiteLLM virtual key scoped to a per-run budget.

--max-budget is required and must be > 0: a key without an enforced budget
defeats the purpose of this command (R4-COST-1). --budget-duration (e.g. 24h,
30d) sets the rolling reset window; without it the budget is lifetime-cumulative.
--models restricts the key to an allowlist (e.g. claude-haiku-4-5) so a runaway
run cannot reach a pricier model group than intended.

Prints the minted key to stdout (or the full object with --json). Capture it —
LiteLLM does not return the raw key again on /key/info.`,
	Example: `  gb litellm-key mint --alias ci-abc123 --max-budget 5 --budget-duration 24h \
      --models claude-haiku-4-5
  gb litellm-key mint --alias probe --max-budget 1 --json`,
	RunE: runLitellmKeyMint,
}

var litellmKeyUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update budget / spend on an existing virtual key (POST /key/update)",
	Long: `Updates an existing LiteLLM virtual key. --key is required.

--max-budget sets a new enforced budget; --budget-duration sets a new reset
window; --spend resets the recorded spend (e.g. --spend 0 to zero it). Fields
you do not pass are left unchanged.`,
	Example: `  gb litellm-key update --key sk-... --max-budget 10
  gb litellm-key update --key sk-... --spend 0`,
	RunE: runLitellmKeyUpdate,
}

var litellmKeyDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete (revoke) a virtual key (POST /key/delete)",
	Long: `Deletes a LiteLLM virtual key, immediately revoking it. --key is required.

Run this at namespace reap/teardown so a per-CI tracking key is short-lived and a
leak is bounded (R4-SEC-1). Idempotent from the caller's view: with
--ignore-missing a key LiteLLM no longer knows about is treated as already
deleted (exit 0), so a ret/re-run of a teardown never fails on an already-reaped
key.`,
	Example: `  gb litellm-key delete --key sk-...
  gb litellm-key delete --key sk-... --ignore-missing`,
	RunE: runLitellmKeyDelete,
}

var litellmKeyInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show a virtual key's spend + budget (GET /key/info)",
	Long: `Shows the current spend, max_budget, and reset instant for a virtual key —
the read-path convenience already used by ` + "`gb usage`" + `, exposed here so a
mint/verify loop stays in one command surface. --key is required.`,
	RunE: runLitellmKeyInfo,
}

// litellmKeyEndpoint resolves the LiteLLM base URL and master key from flags
// (falling back to LITELLM_URL / LITELLM_MASTER_KEY), erroring loudly when
// either is missing — a silent no-op here would leave a run un-capped.
func litellmKeyEndpoint(cmd *cobra.Command) (baseURL, masterKey string, err error) {
	baseURL, _ = cmd.Flags().GetString("litellm-url")
	masterKey, _ = cmd.Flags().GetString("litellm-key")
	baseURL = strings.TrimSpace(baseURL)
	masterKey = strings.TrimSpace(masterKey)
	if baseURL == "" {
		return "", "", fmt.Errorf("LiteLLM URL is required (--litellm-url or LITELLM_URL)")
	}
	if masterKey == "" {
		return "", "", fmt.Errorf("LiteLLM master key is required (--litellm-key or LITELLM_MASTER_KEY)")
	}
	return baseURL, masterKey, nil
}

func runLitellmKeyMint(cmd *cobra.Command, _ []string) error {
	baseURL, masterKey, err := litellmKeyEndpoint(cmd)
	if err != nil {
		return err
	}
	alias, _ := cmd.Flags().GetString("alias")
	maxBudget, _ := cmd.Flags().GetFloat64("max-budget")
	budgetDuration, _ := cmd.Flags().GetString("budget-duration")
	models, _ := cmd.Flags().GetStringSlice("models")
	metaPairs, _ := cmd.Flags().GetStringSlice("metadata")

	if maxBudget <= 0 {
		return fmt.Errorf("--max-budget must be > 0 (an unbudgeted key defeats the R4-COST-1 guardrail); got %v", maxBudget)
	}

	metadata, err := parseMetadataPairs(metaPairs)
	if err != nil {
		return err
	}

	req := litellmKeyGenRequest{
		KeyAlias:       strings.TrimSpace(alias),
		MaxBudget:      maxBudget,
		BudgetDuration: strings.TrimSpace(budgetDuration),
		Models:         models,
		Metadata:       metadata,
	}

	ctx := cmd.Context()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := litellmKeyGenerate(ctx, client, baseURL, masterKey, req)
	if err != nil {
		return fmt.Errorf("minting key: %w", err)
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	// Human path: print the raw key on its own line (so a script can capture it
	// with `$(gb litellm-key mint ...)`) plus a stderr summary that never
	// pollutes the captured stdout.
	fmt.Println(resp.Key)
	fmt.Fprintf(os.Stderr, "minted %s alias=%q max_budget=$%.2f duration=%s models=%v\n",
		keySuffix(resp.Key), resp.KeyAlias, maxBudget, orNone(budgetDuration), models)
	return nil
}

func runLitellmKeyUpdate(cmd *cobra.Command, _ []string) error {
	baseURL, masterKey, err := litellmKeyEndpoint(cmd)
	if err != nil {
		return err
	}
	key, _ := cmd.Flags().GetString("key")
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("--key is required")
	}

	req := litellmKeyUpdateRequest{Key: strings.TrimSpace(key)}
	if cmd.Flags().Changed("max-budget") {
		v, _ := cmd.Flags().GetFloat64("max-budget")
		req.MaxBudget = &v
	}
	if cmd.Flags().Changed("budget-duration") {
		v, _ := cmd.Flags().GetString("budget-duration")
		req.BudgetDuration = strings.TrimSpace(v)
	}
	if cmd.Flags().Changed("spend") {
		v, _ := cmd.Flags().GetFloat64("spend")
		req.Spend = &v
	}
	if cmd.Flags().Changed("models") {
		v, _ := cmd.Flags().GetStringSlice("models")
		req.Models = v
	}
	if req.MaxBudget == nil && req.BudgetDuration == "" && req.Spend == nil && req.Models == nil {
		return fmt.Errorf("nothing to update: pass at least one of --max-budget / --budget-duration / --spend / --models")
	}

	ctx := cmd.Context()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := litellmKeyUpdate(ctx, client, baseURL, masterKey, req)
	if err != nil {
		return fmt.Errorf("updating key: %w", err)
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	fmt.Fprintf(os.Stderr, "updated %s alias=%q max_budget=%s spend=$%.2f\n",
		keySuffix(req.Key), resp.KeyAlias, fmtMaxBudget(resp.MaxBudget), resp.Spend)
	return nil
}

func runLitellmKeyDelete(cmd *cobra.Command, _ []string) error {
	baseURL, masterKey, err := litellmKeyEndpoint(cmd)
	if err != nil {
		return err
	}
	key, _ := cmd.Flags().GetString("key")
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("--key is required")
	}
	ignoreMissing, _ := cmd.Flags().GetBool("ignore-missing")

	ctx := cmd.Context()
	client := &http.Client{Timeout: 15 * time.Second}
	err = litellmKeyDelete(ctx, client, baseURL, masterKey, strings.TrimSpace(key))
	if err != nil {
		if ignoreMissing && isKeyNotFound(err) {
			fmt.Fprintf(os.Stderr, "delete %s: already gone (ignored)\n", keySuffix(key))
			return nil
		}
		return fmt.Errorf("deleting key: %w", err)
	}
	fmt.Fprintf(os.Stderr, "deleted %s\n", keySuffix(key))
	return nil
}

func runLitellmKeyInfo(cmd *cobra.Command, _ []string) error {
	baseURL, masterKey, err := litellmKeyEndpoint(cmd)
	if err != nil {
		return err
	}
	key, _ := cmd.Flags().GetString("key")
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("--key is required")
	}

	ctx := cmd.Context()
	client := &http.Client{Timeout: 15 * time.Second}
	info, err := queryLiteLLMKeyInfo(ctx, client, baseURL, masterKey, strings.TrimSpace(key))
	if err != nil {
		return fmt.Errorf("reading key info: %w", err)
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Printf("%s alias=%q spend=$%.2f max_budget=%s reset=%s\n",
		keySuffix(info.Key), info.KeyAlias, info.Spend, fmtMaxBudget(info.MaxBudget), orNone(info.BudgetResetAt))
	return nil
}

// litellmKeyGenerate POSTs /key/generate and returns the minted key object.
func litellmKeyGenerate(ctx context.Context, client *http.Client, baseURL, masterKey string, req litellmKeyGenRequest) (*litellmKeyMutateResponse, error) {
	return postLitellmKeyMutation(ctx, client, baseURL, masterKey, "/key/generate", req)
}

// litellmKeyUpdate POSTs /key/update and returns the updated key object.
func litellmKeyUpdate(ctx context.Context, client *http.Client, baseURL, masterKey string, req litellmKeyUpdateRequest) (*litellmKeyMutateResponse, error) {
	return postLitellmKeyMutation(ctx, client, baseURL, masterKey, "/key/update", req)
}

// postLitellmKeyMutation is the shared POST-JSON-in / decode-key-object-out
// wrapper for /key/generate and /key/update. Both return a flat key object, so
// one helper serves both.
func postLitellmKeyMutation(ctx context.Context, client *http.Client, baseURL, masterKey, path string, payload any) (*litellmKeyMutateResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	u := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var out litellmKeyMutateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &out, nil
}

// litellmKeyDeleteRequest is the POST /key/delete body — LiteLLM deletes by a
// list of keys.
type litellmKeyDeleteRequest struct {
	Keys []string `json:"keys"`
}

// litellmKeyDelete POSTs /key/delete to revoke a single key. A non-200 surfaces
// as an error carrying the status so isKeyNotFound can classify a 404.
func litellmKeyDelete(ctx context.Context, client *http.Client, baseURL, masterKey, key string) error {
	body, err := json.Marshal(litellmKeyDeleteRequest{Keys: []string{key}})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	u := strings.TrimRight(baseURL, "/") + "/key/delete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}

// isKeyNotFound reports whether an error from litellmKeyDelete is a
// key-not-found (LiteLLM answers 404, or 400 with a not-found message) so a
// teardown can treat an already-reaped key as success under --ignore-missing.
func isKeyNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist")
}

// parseMetadataPairs turns repeated k=v flag values into a metadata map. An
// entry without '=' is an error rather than a silently-dropped pair.
func parseMetadataPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--metadata %q is not in key=value form", p)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// fmtMaxBudget renders a *max_budget for display: "$X.XX" or "unset".
func fmtMaxBudget(b *float64) string {
	if b == nil {
		return "unset"
	}
	return fmt.Sprintf("$%.2f", *b)
}

// The flag registrars below are standalone so tests can build a fresh,
// isolated cobra.Command with exactly a subcommand's flags — sharing the global
// subcommand's flagset across test cases would leak `Changed`/values between
// them.

// addEndpointFlags registers the shared --litellm-url / --litellm-key flags,
// defaulting to the same env vars usage.go/account.go read.
func addEndpointFlags(c *cobra.Command) {
	c.Flags().String("litellm-url", os.Getenv("LITELLM_URL"), "LiteLLM proxy URL (env: LITELLM_URL)")
	c.Flags().String("litellm-key", os.Getenv("LITELLM_MASTER_KEY"), "LiteLLM master/admin key (env: LITELLM_MASTER_KEY)")
}

func addMintFlags(c *cobra.Command) {
	addEndpointFlags(c)
	c.Flags().String("alias", "", "human-readable key_alias for the minted key")
	c.Flags().Float64("max-budget", 0, "enforced spend cap in API-equivalent dollars (required, > 0)")
	c.Flags().String("budget-duration", "", "rolling budget reset window (e.g. 24h, 30d); empty = lifetime-cumulative")
	c.Flags().StringSlice("models", nil, "restrict the key to this model allowlist (repeatable / comma-separated)")
	c.Flags().StringSlice("metadata", nil, "attach key=value metadata (repeatable)")
}

func addUpdateFlags(c *cobra.Command) {
	addEndpointFlags(c)
	c.Flags().String("key", "", "the sk-... virtual key to update (required)")
	c.Flags().Float64("max-budget", 0, "new enforced spend cap in dollars")
	c.Flags().String("budget-duration", "", "new rolling budget reset window (e.g. 24h, 30d)")
	c.Flags().Float64("spend", 0, "set the recorded spend (e.g. 0 to reset)")
	c.Flags().StringSlice("models", nil, "replace the key's model allowlist")
}

func addDeleteFlags(c *cobra.Command) {
	addEndpointFlags(c)
	c.Flags().String("key", "", "the sk-... virtual key to delete (required)")
	c.Flags().Bool("ignore-missing", false, "treat an already-deleted key as success (idempotent teardown)")
}

func addInfoFlags(c *cobra.Command) {
	addEndpointFlags(c)
	c.Flags().String("key", "", "the sk-... virtual key to inspect (required)")
}

func init() {
	addMintFlags(litellmKeyMintCmd)
	addUpdateFlags(litellmKeyUpdateCmd)
	addDeleteFlags(litellmKeyDeleteCmd)
	addInfoFlags(litellmKeyInfoCmd)

	litellmKeyCmd.AddCommand(litellmKeyMintCmd)
	litellmKeyCmd.AddCommand(litellmKeyUpdateCmd)
	litellmKeyCmd.AddCommand(litellmKeyDeleteCmd)
	litellmKeyCmd.AddCommand(litellmKeyInfoCmd)
}
