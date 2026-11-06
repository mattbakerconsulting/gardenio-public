package main

import (
	"context"
	"log/slog"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/subscriber"
)

// syncAccountsToCoopmux reads account beads, resolves their K8s secret
// credentials, and pushes them to coopmux. This replaces the manual
// `gb account sync-creds` command. Non-fatal: logs warnings on failure.
//
// litellmURL is the controller-level default base URL (cfg.LiteLLMURL); each
// account's pushed base_url is resolved the same way the pod-spawn path does
// (proxy_url > base_url > default) so a rotation onto the account uses the
// endpoint a fresh pod on it would get (ka-0000002623).
func syncAccountsToCoopmux(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, k8sClient kubernetes.Interface, namespace string, muxClient *coopmux.Client, credNotifier *credSyncNotifier, litellmURL, trigger string) bool {
	accounts, err := daemon.ListAccountBeads(ctx)
	if err != nil {
		logger.Warn("coopmux sync: failed to list account beads", "error", err)
		credNotifier.NotifyBulkFailed(err.Error(), trigger)
		return false
	}

	var toSync []coopmux.AccountSync
	// keepOnly names accounts coopmux must keep that this bulk sync does not push:
	// the refresh keeper owns them.
	keepOnly := map[string]bool{}
	for name, acct := range accounts {
		if acct.Credential == "" {
			continue
		}
		// A Codex subscription login (provider=openai) is not a bearer token: its
		// auth.json is owned by the refresh keeper and pushed to coopmux by it, never
		// bulk-synced from the "token" key. The cleanup below must not delete it.
		if strings.EqualFold(strings.TrimSpace(acct.Provider), providerOpenAI) {
			keepOnly[name] = true
			continue
		}

		credType, credValue, ok := strings.Cut(acct.Credential, ":")
		if !ok || credType != "secret" {
			continue // only sync secret-backed credentials
		}

		secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, credValue, metav1.GetOptions{})
		if err != nil {
			logger.Warn("coopmux sync: secret not found",
				"account", name, "secret", credValue, "error", err)
			continue
		}
		token := strings.TrimSpace(string(secret.Data["token"]))
		if token == "" {
			logger.Warn("coopmux sync: secret has empty token",
				"account", name, "secret", credValue)
			continue
		}

		provider := acct.Provider
		if provider == "" {
			provider = "claude"
		}

		toSync = append(toSync, coopmux.AccountSync{
			Name:       name,
			Provider:   provider,
			Token:      token,
			Model:      acct.Model,
			LiteLLMKey: acct.LiteLLMKey,
			BaseURL:    resolveAccountBaseURL(acct.ProxyURL, acct.BaseURL, litellmURL),
		})
	}

	if len(toSync) == 0 {
		logger.Debug("coopmux sync: no secret-backed accounts to sync")
		return true
	}

	synced, failed, err := muxClient.SyncAccounts(ctx, toSync)
	if err != nil {
		logger.Warn("coopmux sync: partial failure",
			"synced", synced, "failed", failed, "error", err)
		credNotifier.NotifyBulkFailed(err.Error(), trigger)
		return false
	}
	logger.Info("coopmux credential sync complete",
		"synced", synced, "failed", failed)
	credNotifier.NotifyBulkSynced(synced, failed, trigger)

	// Cleanup: remove accounts from coopmux that are no longer in the beads
	// daemon (closed or deleted). Without this, sessions can get stuck on
	// stale accounts that no longer have valid credentials.
	validAccounts := make(map[string]bool, len(toSync)+len(keepOnly))
	for _, a := range toSync {
		validAccounts[a.Name] = true
	}
	for name := range keepOnly {
		validAccounts[name] = true
	}
	muxAccounts, listErr := muxClient.ListAccounts(ctx)
	if listErr != nil {
		logger.Warn("coopmux sync: failed to list accounts for cleanup", "error", listErr)
		return true // sync succeeded, cleanup is best-effort
	}
	for _, name := range muxAccounts {
		if !validAccounts[name] {
			if rmErr := muxClient.RemoveAccount(ctx, name); rmErr != nil {
				logger.Warn("coopmux sync: failed to remove stale account",
					"account", name, "error", rmErr)
			} else {
				logger.Info("coopmux sync: removed stale account", "account", name)
			}
		}
	}

	return true
}

// syncSingleAccountToCoopmux resolves and pushes a single account's credential
// to coopmux. Called in response to an account bead update event for real-time
// credential propagation (instead of waiting for the next periodic bulk sync).
//
// litellmURL is the controller-level default base URL (cfg.LiteLLMURL); see
// syncAccountsToCoopmux for the per-account resolution order.
func syncSingleAccountToCoopmux(ctx context.Context, logger *slog.Logger, event subscriber.Event, k8sClient kubernetes.Interface, namespace string, muxClient *coopmux.Client, credNotifier *credSyncNotifier, litellmURL string) {
	accountName := event.Metadata["title"]
	credential := event.Metadata["credential"]
	provider := event.Metadata["provider"]
	if accountName == "" {
		logger.Warn("coopmux single sync: account event missing title", "bead", event.BeadID)
		return
	}
	if credential == "" {
		logger.Debug("coopmux single sync: account has no credential field", "account", accountName)
		return
	}
	// A Codex login's auth.json is pushed by the refresh keeper, not read as a token.
	if strings.EqualFold(strings.TrimSpace(provider), providerOpenAI) {
		return
	}

	credType, credValue, ok := strings.Cut(credential, ":")
	if !ok || credType != "secret" {
		logger.Debug("coopmux single sync: skipping non-secret credential",
			"account", accountName, "credential_type", credType)
		return
	}

	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, credValue, metav1.GetOptions{})
	if err != nil {
		logger.Warn("coopmux single sync: secret not found",
			"account", accountName, "secret", credValue, "error", err)
		return
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		logger.Warn("coopmux single sync: secret has empty token",
			"account", accountName, "secret", credValue)
		return
	}

	if provider == "" {
		provider = "claude"
	}

	model := event.Metadata["model"]
	litellmKey := event.Metadata["litellm_key"]
	baseURL := resolveAccountBaseURL(event.Metadata["proxy_url"], event.Metadata["base_url"], litellmURL)

	if err := muxClient.SyncSingleAccount(ctx, coopmux.AccountSync{
		Name:       accountName,
		Provider:   provider,
		Token:      token,
		Model:      model,
		LiteLLMKey: litellmKey,
		BaseURL:    baseURL,
	}); err != nil {
		logger.Warn("coopmux single sync: failed to push credential",
			"account", accountName, "error", err)
		credNotifier.NotifySingleFailed(accountName, err.Error())
		return
	}
	logger.Info("coopmux single sync: pushed updated credential",
		"account", accountName, "provider", provider)
	credNotifier.NotifySingleSynced(accountName, provider)
}
